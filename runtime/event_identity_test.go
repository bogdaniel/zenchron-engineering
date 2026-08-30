package runtime

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// TestEventIdentityIsSafeUnderIndependentJournalWriters is the Phase 10
// precondition for a watch process and an operator CLI writing one run at the
// same time. Three writers on three independent SQLiteOperationStore handles
// over ONE database file are released from a barrier together:
//
//   - "watch" holds the run's durable operation lease and records observations;
//   - "observer" is a second process recording the SAME event type at the SAME
//     clock instant, which is precisely the case an id derived from an
//     in-memory sequence plus the clock cannot survive;
//   - "operator" holds NO lease and records the human-authority event, because
//     recording human authority is not an engineering side effect and must not
//     have to take over the run's operation lease.
//
// Correctness has to come from the database, not from a Go lock: nothing here
// serializes the writers, and the assertions are made against a fresh handle
// that replays the durable journal.
//
// human.authority_recorded is still a reserved event type with no payload
// schema, so the operator writes authority.evaluated, which is the same shape
// of fact (an operator-recorded authority outcome carrying no side effect) and
// is registered today. Swap the type once the schema lands; nothing else here
// depends on which type it is.
func TestEventIdentityIsSafeUnderIndependentJournalWriters(t *testing.T) {
	const appendsPerWriter = 8
	at := time.Unix(100, 0).UTC()

	dir, watch, operator := openPair(t)
	observer, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { observer.Close() })
	run := newJournalRun("r")
	if err := watch.PutRun(run); err != nil {
		t.Fatal(err)
	}

	// The lease is durable state, so "operator holds no lease" is a fact about
	// the database rather than about this test's variables.
	lease := RunOperation{SchemaVersion: SchemaVersion, ID: "op-1", RunID: run.ID, Kind: "provider", IdempotencyKey: "a", State: Pending, MaxAttempts: 2, CreatedAt: at}
	if _, ok, err := watch.PutOperation(leasedAt(lease, "watch", at), 0); err != nil || !ok {
		t.Fatal(err, ok)
	}
	operations, err := operator.Operations(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(operations) != 1 || operations[0].Lease == nil || operations[0].Lease.Owner != "watch" {
		t.Fatalf("expected the run's only operation to be leased by watch, got %+v", operations)
	}

	prObserved, err := json.Marshal(GitHubPRObservedPayload{Number: 7, HeadRevision: run.Candidate.Revision, BaseRevision: run.Base.Revision, State: "open"})
	if err != nil {
		t.Fatal(err)
	}
	authority, err := json.Marshal(AuthorityEvaluatedPayload{
		Decision: Ref{ID: "decision", Revision: "1"},
		Action:   domain.Action{Type: "merge", Target: "github.com/example/repo#7"},
		Status:   domain.AuthorityAuthorized,
	})
	if err != nil {
		t.Fatal(err)
	}
	writers := []struct {
		name      string
		store     *SQLiteOperationStore
		eventType string
		payload   json.RawMessage
	}{
		{"watch", watch, EventGitHubPRObserved, prObserved},
		{"observer", observer, EventGitHubPRObserved, prObserved},
		{"operator", operator, EventAuthorityEvaluated, authority},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < appendsPerWriter; i++ {
				_, err := w.store.AppendEvent(EngineeringEvent{
					SchemaVersion: SchemaVersion,
					ID:            newEventID(run.ID),
					RunID:         run.ID,
					Type:          w.eventType,
					OccurredAt:    at,
					Payload:       w.payload,
				})
				if err != nil {
					t.Errorf("%s append %d: %v", w.name, i, err)
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()

	replayed, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { replayed.Close() })
	events, err := replayed.Events(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(writers) * appendsPerWriter; len(events) != want {
		t.Fatalf("journal holds %d events, expected %d: an append was lost", len(events), want)
	}

	ids, sequences, operatorEvents := map[string]bool{}, map[int64]bool{}, 0
	for i, e := range events {
		if ids[e.ID] {
			t.Fatalf("event id %q is not unique", e.ID)
		}
		ids[e.ID] = true
		if sequences[e.Sequence] {
			t.Fatalf("sequence %d is used twice", e.Sequence)
		}
		sequences[e.Sequence] = true
		if e.Sequence != int64(i+1) {
			t.Fatalf("sequence %d at position %d: the journal left a gap", e.Sequence, i)
		}
		if i > 0 && (e.PreviousEventID != events[i-1].ID || e.PreviousEventHash != events[i-1].EventHash) {
			t.Fatalf("event %q is not chained to its predecessor", e.ID)
		}
		hash, err := EventDigest(e)
		if err != nil || hash != e.EventHash {
			t.Fatalf("event %q hash %q does not recompute to %q (%v)", e.ID, e.EventHash, hash, err)
		}
		if e.Type == EventAuthorityEvaluated {
			operatorEvents++
		}
	}
	if operatorEvents != appendsPerWriter {
		t.Fatalf("the lease-less operator landed %d events, expected %d", operatorEvents, appendsPerWriter)
	}

	snapshot, err := replayed.Replay(run.ID)
	if err != nil {
		t.Fatalf("replay of the concurrently written journal failed: %v", err)
	}
	last := events[len(events)-1]
	if snapshot.Cursor != (Cursor{LastSequence: last.Sequence, LastEventID: last.ID, LastEventHash: last.EventHash}) {
		t.Fatalf("replayed cursor %+v does not name the last event %q", snapshot.Cursor, last.ID)
	}
	if snapshot.StateSHA256 != last.StateAfter {
		t.Fatalf("replayed state %q is not the last event's state_after %q", snapshot.StateSHA256, last.StateAfter)
	}
}
