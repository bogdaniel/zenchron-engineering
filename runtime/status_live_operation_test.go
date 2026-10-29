package runtime

import (
	"testing"
	"time"
)

// #326: status reports a RUNNING operation's progress and lease from the live
// row of that exact attempt, and nothing else from it. The journal is the only
// place an operation.before document exists, so each check runs while the
// fixture's provider is mid-invocation.
func duringInvocation(t *testing.T, check func(f *phase8Fixture, runID string, journal RunOperation)) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	ran := false
	fixture.provider.mutate = func(string) error {
		journal, ok := fixture.state(runID).currentOperation()
		if !ok || journal.State != Running {
			t.Fatalf("no running operation in the journal: %#v", journal)
		}
		ran = true
		check(fixture, runID, journal)
		return nil
	}
	fixture.reconcile(runID)
	if !ran {
		t.Fatal("the provider was never invoked")
	}
}

func liveStatus(t *testing.T, f *phase8Fixture, runID string) OperationStatus {
	t.Helper()
	report, err := f.runtime.Status(runID)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if report.Operation == nil {
		t.Fatal("status reported no operation")
	}
	return *report.Operation
}

// rewriteRow applies edit to the durable row and returns a restore func, so the
// run can settle normally afterwards.
func rewriteRow(t *testing.T, f *phase8Fixture, id string, edit func(*RunOperation)) func() {
	t.Helper()
	original, revision, found, err := f.store.Operation(id)
	if err != nil || !found {
		t.Fatalf("row %s: found=%t err=%v", id, found, err)
	}
	changed := copyOperation(original)
	edit(&changed)
	next, ok, err := f.store.PutOperation(changed, revision)
	if err != nil || !ok {
		t.Fatalf("PutOperation: ok=%t err=%v", ok, err)
	}
	return func() {
		if _, ok, err := f.store.PutOperation(original, next); err != nil || !ok {
			t.Fatalf("restore row: ok=%t err=%v", ok, err)
		}
	}
}

// Live progress is shown: progress recorded on the row after the attempt
// started is what status reports, not the start instant the journal holds.
// This is the restored-defect test for reading progress from the journal.
func TestStatusReportsLiveProgressFromTheAttemptRow(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		if journal.LastProgressAt == nil {
			t.Fatal("precondition: the journal's start document carries no progress instant")
		}
		start := *journal.LastProgressAt
		f.clock.advance(20 * time.Minute)
		recorded, err := f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity, ProviderProgress{Key: "1:42"})
		if err != nil {
			t.Fatal(err)
		}
		if recorded.LastProgressAt == nil || !recorded.LastProgressAt.After(start) {
			t.Fatalf("progress was not recorded on the row: %v", recorded.LastProgressAt)
		}
		// Precondition: the journal still says the attempt start.
		if again, _ := f.state(runID).currentOperation(); !again.LastProgressAt.Equal(start) {
			t.Fatalf("precondition: the journal moved to %v", again.LastProgressAt)
		}
		f.clock.advance(time.Minute)
		status := liveStatus(t, f, runID)
		if status.ProgressSource != "row" {
			t.Fatalf("progress source = %q, want row", status.ProgressSource)
		}
		if status.LastProgressAt == nil || !status.LastProgressAt.Equal(*recorded.LastProgressAt) {
			t.Fatalf("last progress = %v, want the row's %v (journal start %v)", status.LastProgressAt, recorded.LastProgressAt, start)
		}
		if status.SilentFor <= 0 || status.SilentFor > 2*time.Minute {
			t.Fatalf("silent for %s, want it measured from the recorded progress, not the attempt start", status.SilentFor)
		}
	})
}

// The lease comes from the row: a heartbeat moved on the row (as #180 will do)
// is the heartbeat status reports.
func TestStatusReportsTheLiveRowLease(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		if journal.Lease == nil {
			t.Fatal("precondition: the journal's running document has no lease")
		}
		beat := journal.Lease.HeartbeatAt.Add(7 * time.Minute)
		restore := rewriteRow(t, f, journal.ID, func(op *RunOperation) {
			op.Lease.HeartbeatAt = beat
			op.Lease.Owner = "row-owner"
		})
		defer restore()
		status := liveStatus(t, f, runID)
		if status.HeartbeatAt == nil || !status.HeartbeatAt.Equal(beat) {
			t.Fatalf("heartbeat = %v, want the row's %v", status.HeartbeatAt, beat)
		}
		if status.Lease == nil || status.Lease.Owner != "row-owner" || !status.Lease.HeartbeatAt.Equal(beat) {
			t.Fatalf("lease = %#v, want the row's", status.Lease)
		}
	})
}

// A row on a different attempt is not this attempt's live state: status keeps
// the journal's values and says so, rather than mixing attempts.
func TestStatusDoesNotMixAnotherAttemptsRowIntoTheReport(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		other := journal.LastProgressAt.Add(9 * time.Minute)
		restore := rewriteRow(t, f, journal.ID, func(op *RunOperation) {
			op.AttemptIdentity++
			op.LastProgressAt = &other
			op.Lease.HeartbeatAt = other
		})
		defer restore()
		status := liveStatus(t, f, runID)
		if status.ProgressSource != "journal" {
			t.Fatalf("progress source = %q, want journal", status.ProgressSource)
		}
		if !status.LastProgressAt.Equal(*journal.LastProgressAt) || !status.HeartbeatAt.Equal(journal.Lease.HeartbeatAt) || status.Lease != nil {
			t.Fatalf("another attempt's row leaked into the report: %#v", status)
		}
	})
}

// A row of the same attempt that has already settled or been abandoned is no
// longer live state: status keeps the journal's values and says so.
func TestStatusIgnoresASettledRowOfTheSameAttempt(t *testing.T) {
	for _, settled := range []OperationState{Succeeded, Unknown} {
		t.Run(string(settled), func(t *testing.T) {
			duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
				other := journal.LastProgressAt.Add(9 * time.Minute)
				restore := rewriteRow(t, f, journal.ID, func(op *RunOperation) {
					op.State = settled
					op.LastProgressAt = &other
				})
				defer restore()
				status := liveStatus(t, f, runID)
				if status.ProgressSource != "journal" || !status.LastProgressAt.Equal(*journal.LastProgressAt) {
					t.Fatalf("a %s row leaked into the report: %#v", settled, status)
				}
			})
		})
	}
}

// A failed row read degrades to the journal instead of failing status.
func TestStatusDegradesToTheJournalWhenTheRowCannotBeRead(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		var document string
		if err := f.store.db.QueryRow(`SELECT document FROM run_operations WHERE id = ?`, journal.ID).Scan(&document); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`UPDATE run_operations SET document = '[]' WHERE id = ?`, journal.ID); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if _, err := f.store.db.Exec(`UPDATE run_operations SET document = ? WHERE id = ?`, document, journal.ID); err != nil {
				t.Fatal(err)
			}
		}()
		if _, _, _, err := f.store.Operation(journal.ID); err == nil {
			t.Fatal("precondition: the row read did not fail")
		}
		status := liveStatus(t, f, runID)
		if status.ProgressSource != "journal" || !status.LastProgressAt.Equal(*journal.LastProgressAt) {
			t.Fatalf("status did not degrade to the journal: %#v", status)
		}
	})
}

// Nothing else moves: only progress and lease may come from the row. State and
// attempt accounting stay the journal's even when the row disagrees.
func TestStatusTakesOnlyProgressAndLeaseFromTheRow(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		restore := rewriteRow(t, f, journal.ID, func(op *RunOperation) {
			op.State = Leased
			op.Attempt += 3
			op.MaxAttempts += 5
			op.Kind = "row-kind"
		})
		defer restore()
		status := liveStatus(t, f, runID)
		if status.ProgressSource != "row" {
			t.Fatalf("progress source = %q, want row", status.ProgressSource)
		}
		if status.State != journal.State || status.Attempt != journal.Attempt || status.MaxAttempts != journal.MaxAttempts ||
			status.Kind != journal.Kind || status.AttemptIdentity != journal.AttemptIdentity {
			t.Fatalf("a non-observation field came from the row: %#v (journal %#v)", status, journal)
		}
	})
}

// A settled operation is reported from the journal exactly as before.
func TestStatusOfASettledOperationIsJournalOnly(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)
	journal, ok := fixture.state(runID).currentOperation()
	if !ok || journal.State == Running || journal.State == Leased {
		t.Fatalf("precondition: current operation is not settled: %#v", journal)
	}
	status := liveStatus(t, fixture, runID)
	if status.ProgressSource != "" || status.Lease != nil {
		t.Fatalf("a settled operation carried a live source: %#v", status)
	}
	if (status.LastProgressAt == nil) != (journal.LastProgressAt == nil) ||
		(journal.LastProgressAt != nil && !status.LastProgressAt.Equal(*journal.LastProgressAt)) {
		t.Fatalf("last progress = %v, want the journal's %v", status.LastProgressAt, journal.LastProgressAt)
	}
}
