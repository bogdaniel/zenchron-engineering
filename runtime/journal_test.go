package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newJournalRun(id string) EngineeringRun {
	at := time.Unix(100, 0).UTC()
	return EngineeringRun{
		SchemaVersion:    SchemaVersion,
		ID:               id,
		Repository:       "github.com/example/repo",
		Goal:             "close the journal gap",
		Phase:            Execute,
		Disposition:      Active,
		Base:             Ref{ID: "base", Revision: strings.Repeat("1", 40)},
		Candidate:        Candidate{Branch: "candidate", Revision: strings.Repeat("2", 40), Tree: strings.Repeat("3", 40)},
		Contract:         Ref{ID: "contract", Revision: "1"},
		ControllerSHA256: strings.Repeat("4", 64),
		CreatedAt:        at,
		UpdatedAt:        at,
	}
}

// journalFixture exercises every recorded field: an operation lifecycle payload,
// artifact references (raw local-only alongside a sanitized candidate), and a
// run disposition the reducer must fold.
func journalFixture(t *testing.T, runID string) []EngineeringEvent {
	t.Helper()
	operation, err := json.Marshal(RunOperation{SchemaVersion: SchemaVersion, ID: "op-1", RunID: runID, Kind: "provider", IdempotencyKey: "a", State: Pending, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := json.Marshal(map[string]string{"reason": "merged"})
	if err != nil {
		t.Fatal(err)
	}
	return []EngineeringEvent{
		{SchemaVersion: SchemaVersion, ID: "e-1", RunID: runID, Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()},
		{SchemaVersion: SchemaVersion, ID: "e-2", RunID: runID, Type: EventOperationPlanned, OperationID: "op-1", OccurredAt: time.Unix(101, 0).UTC(), Payload: operation},
		{SchemaVersion: SchemaVersion, ID: "e-3", RunID: runID, Type: EventAssuranceObserved, OccurredAt: time.Unix(102, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain", LocalOnly: true},
			{Path: "/state/verify.sanitized-candidate.log", SHA256: strings.Repeat("b", 64), MediaType: "text/plain", Sanitized: true},
		}},
		{SchemaVersion: SchemaVersion, ID: "e-4", RunID: runID, Type: EventRunCompleted, OccurredAt: time.Unix(103, 0).UTC(), Payload: completed},
	}
}

func openJournal(t *testing.T) (string, *SQLiteOperationStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

// rawJournalDB bypasses the store to model corruption or a foreign writer.
func rawJournalDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db")+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestJournalReplayIsIdenticalAfterReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := newJournalRun("r")
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	appended := []EngineeringEvent{}
	for _, e := range journalFixture(t, "r") {
		stored, err := store.AppendEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Sequence != int64(len(appended)+1) {
			t.Fatalf("expected sequence %d, got %d", len(appended)+1, stored.Sequence)
		}
		if n := len(appended); n > 0 && (stored.PreviousEventID != appended[n-1].ID || stored.PreviousEventHash != appended[n-1].EventHash) {
			t.Fatalf("event %q is not linked to its predecessor: %+v", stored.ID, stored)
		}
		if n := len(appended); n > 0 && stored.StateBefore != appended[n-1].StateAfter {
			t.Fatalf("state_before of %q does not continue the previous state_after", stored.ID)
		}
		appended = append(appended, stored)
	}
	live, err := store.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if live.StateSHA256 != restored.StateSHA256 {
		t.Fatalf("state digest changed across reopen: %q vs %q", live.StateSHA256, restored.StateSHA256)
	}
	before, err := Digest(live)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Digest(restored)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("replayed snapshot changed across reopen: %q vs %q", before, after)
	}
	last := appended[len(appended)-1]
	if last.StateAfter != restored.StateSHA256 {
		t.Fatalf("last event state_after %q is not the replayed state %q", last.StateAfter, restored.StateSHA256)
	}
	if restored.Cursor != (Cursor{LastSequence: int64(len(appended)), LastEventID: last.ID, LastEventHash: last.EventHash}) {
		t.Fatalf("journal cursor was not rebuilt: %+v", restored.Cursor)
	}
	if restored.Disposition != Completed || restored.Reason != "merged" {
		t.Fatalf("run disposition was not rebuilt: %q %q", restored.Disposition, restored.Reason)
	}
	if len(restored.Operations) != 1 || restored.Operations["op-1"].Kind != "provider" {
		t.Fatalf("operation state was not rebuilt: %+v", restored.Operations)
	}
	if len(restored.Artifacts) != 2 || restored.Artifacts[0].Path != "/state/verify.raw.log" {
		t.Fatalf("artifact references were not rebuilt: %+v", restored.Artifacts)
	}
	stored, ok, err := reopened.Run("r")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if stored.Repository != run.Repository || stored.Contract != run.Contract || stored.Base != run.Base || stored.Candidate != run.Candidate {
		t.Fatalf("run subject/contract binding did not survive reopen: %+v", stored)
	}
}

func TestJournalRefusesBrokenChain(t *testing.T) {
	for _, tamper := range []struct {
		name  string
		apply func(t *testing.T, db *sql.DB)
	}{
		{"chain column", func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE events SET previous_event_hash = ? WHERE run_id = 'r' AND sequence = 3`, strings.Repeat("0", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{"event hash column", func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE events SET event_hash = ? WHERE run_id = 'r' AND sequence = 2`, strings.Repeat("0", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{"document and column together", func(t *testing.T, db *sql.DB) {
			var document string
			if err := db.QueryRow(`SELECT document FROM events WHERE run_id = 'r' AND sequence = 3`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			var e EngineeringEvent
			if err := json.Unmarshal([]byte(document), &e); err != nil {
				t.Fatal(err)
			}
			e.PreviousEventHash = strings.Repeat("0", 64)
			tampered, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE events SET document = ?, previous_event_hash = ? WHERE run_id = 'r' AND sequence = 3`, string(tampered), e.PreviousEventHash); err != nil {
				t.Fatal(err)
			}
		}},
		{"payload body", func(t *testing.T, db *sql.DB) {
			var document string
			if err := db.QueryRow(`SELECT document FROM events WHERE run_id = 'r' AND sequence = 4`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(document, `"merged"`, `"forged"`, 1)
			if tampered == document {
				t.Fatal("fixture payload did not contain the expected reason")
			}
			if _, err := db.Exec(`UPDATE events SET document = ? WHERE run_id = 'r' AND sequence = 4`, tampered); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutRun(newJournalRun("r")); err != nil {
				t.Fatal(err)
			}
			for _, e := range journalFixture(t, "r") {
				if _, err := store.AppendEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			tamper.apply(t, rawJournalDB(t, dir))

			reopened, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			snapshot, err := reopened.Replay("r")
			if err == nil {
				t.Fatalf("tampered journal replayed successfully: %+v", snapshot)
			}
			// A further append must not build on an unverifiable chain either.
			if _, err := reopened.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-5", RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(104, 0).UTC()}); err == nil {
				t.Fatal("append accepted a tampered journal as its predecessor")
			}
		})
	}
}

func TestJournalRefusesDuplicateSequence(t *testing.T) {
	dir, store := openJournal(t)
	first, err := store.AppendEvent(journalFixture(t, "r")[0])
	if err != nil {
		t.Fatal(err)
	}
	// The database, not the caller, is what refuses a duplicate sequence.
	_, err = rawJournalDB(t, dir).Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, '', '', '', '', '', '', '{}')`,
		"forged", first.RunID, first.Sequence, first.Type)
	if err == nil {
		t.Fatal("a duplicate (run_id, sequence) was accepted")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("expected a UNIQUE constraint refusal, got %v", err)
	}
	// The same row id is refused too, so an event id is never reused.
	_, err = rawJournalDB(t, dir).Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, '', '', '', '', '', '', '{}')`,
		first.ID, first.RunID, int64(99), first.Type)
	if err == nil {
		t.Fatal("a duplicate event id was accepted")
	}
	// A caller may not choose its own sequence or chain link.
	chosen := journalFixture(t, "r")[1]
	chosen.Sequence = 1
	if _, err := store.AppendEvent(chosen); err == nil {
		t.Fatal("a caller-chosen sequence was accepted")
	}
	events, err := store.Events("r")
	if err != nil || len(events) != 1 {
		t.Fatalf("expected exactly the one appended event, got %d (%v)", len(events), err)
	}
}

func TestJournalSequenceAllocationIsMonotonicAcrossHandles(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	const perHandle = 8
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[int64]string{}
	for handle, store := range []*SQLiteOperationStore{first, second} {
		for i := 0; i < perHandle; i++ {
			wg.Add(1)
			go func(store *SQLiteOperationStore, id string) {
				defer wg.Done()
				stored, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id, RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(200, 0).UTC()})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if other, duplicate := seen[stored.Sequence]; duplicate {
					t.Errorf("sequence %d allocated to both %q and %q", stored.Sequence, other, id)
					return
				}
				seen[stored.Sequence] = id
			}(store, fmt.Sprintf("e-%d-%d", handle, i))
		}
	}
	wg.Wait()
	total := int64(2 * perHandle)
	if int64(len(seen)) != total {
		t.Fatalf("expected %d distinct sequences, got %d", total, len(seen))
	}
	for n := int64(1); n <= total; n++ {
		if seen[n] == "" {
			t.Fatalf("sequence %d was never allocated: gap in %v", n, seen)
		}
	}
	// Replaying through the reducer proves the chain survived the race.
	snapshot, err := second.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor.LastSequence != total {
		t.Fatalf("expected cursor at %d, got %+v", total, snapshot.Cursor)
	}
}

func TestJournalRefusesRawTranscriptBody(t *testing.T) {
	_, store := openJournal(t)
	body := "github_pat_fixturesecret\n+ go test ./...\n"
	inline, err := json.Marshal(map[string]any{"provider": "codex", "transcript": body})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := json.Marshal(map[string]any{"attempts": []any{map[string]any{"stderr": body}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		name  string
		event EngineeringEvent
	}{
		{"inline transcript", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-x", RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(105, 0).UTC(), Payload: inline}},
		{"nested stderr", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-y", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Payload: nested}},
		{"raw artifact that is not local-only", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-z", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain"},
		}}},
		{"raw artifact marked publishable", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-w", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain", LocalOnly: true, Publishable: true},
		}}},
	} {
		if _, err := store.AppendEvent(refused.event); err == nil {
			t.Fatalf("%s was appended to a canonical event row", refused.name)
		}
	}
	events, err := store.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused append still wrote %d rows", len(events))
	}
	// The reference form of the same transcript is what the journal accepts.
	if _, err := store.AppendEvent(journalFixture(t, "r")[2]); err != nil {
		t.Fatalf("an artifact reference must be appendable: %v", err)
	}
}

func TestJournalRefusesEventsForAnUnknownRun(t *testing.T) {
	_, store := openJournal(t)
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "other", Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()}); err == nil {
		t.Fatal("an event was appended to a run that does not exist")
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r", Type: "run.invented", OccurredAt: time.Unix(100, 0).UTC()}); err == nil {
		t.Fatal("an event outside the catalogue was appended")
	}
}
