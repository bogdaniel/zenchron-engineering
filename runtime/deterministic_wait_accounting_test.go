package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

type timedRecoveryDetector struct {
	clock  *steppingClock
	cancel context.CancelFunc
	spent  bool
}

func TestDeterministicCheckSubjectSurvivesJournalEncoding(t *testing.T) {
	_, store := openJournal(t)
	path := []byte{'d', '/', 0xff, '.', 'e', 'n', 'v'}
	op := RunOperation{SchemaVersion: SchemaVersion, ID: "op", RunID: "r", State: OperationFailed,
		Failure: &OperationFailure{Classification: deterministicLocal, Code: "candidate.sensitive_path", CheckPath: path,
			Signature: strings.Repeat("a", 64), BindingSHA256: strings.Repeat("b", 64)}}
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "after", RunID: "r",
		OperationID: "op", Type: EventOperationAfter, OccurredAt: time.Unix(105, 0).UTC(), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	var decoded RunOperation
	if err := json.Unmarshal(events[0].Payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Failure == nil || !bytes.Equal(decoded.Failure.CheckPath, path) {
		t.Fatalf("journal replaced the check subject: %+v", decoded.Failure)
	}
}

func TestUnversionedDeterministicScopeCannotRenewAttemptOnRestart(t *testing.T) {
	f, id := checkpointConflictFixture(t)
	f.reconcile(id)
	op := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	// An earlier draft journal remains readable. Its broad digest is not
	// evidence of changed inputs under the new scoped comparison definition.
	op.Failure.BindingScope = ""
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "earlier-draft-after", RunID: id,
		OperationID: op.ID, Type: EventOperationAfter, OccurredAt: f.clock.Now(), Payload: payload}); err != nil {
		t.Fatal(err)
	}
	reopen(t, f)
	if _, err := f.runtime.Reconcile(context.Background(), id); err == nil || !strings.Contains(err.Error(), "unsupported input binding scope") {
		t.Fatalf("unversioned comparison did not report unavailable identity: %v", err)
	}
	if got := failedOperation(t, f, id, OpCandidateCommit); got.Attempt != 1 || got.AttemptIdentity != 1 {
		t.Fatalf("old binding format renewed an attempt: %+v", got)
	}
}

func TestDeterministicWaitIgnoresOtherOperationProbes(t *testing.T) {
	start := time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC)
	failure, err := json.Marshal(RunOperation{ID: "blocked", RunID: "run", State: OperationFailed,
		Failure: &OperationFailure{Classification: deterministicLocal, Code: "candidate.sensitive_path",
			Signature: strings.Repeat("a", 64), BindingSHA256: strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatal(err)
	}
	events := []EngineeringEvent{
		{Type: EventOperationAfter, OperationID: "blocked", OccurredAt: start.Add(time.Minute), Payload: failure},
		waitEvent(start.Add(time.Minute), ReasonDeterministicFailureUnchanged),
	}
	events = append(events, operationPair("probe", start.Add(30*time.Minute), time.Minute)...)
	run := EngineeringRun{CreatedAt: start}
	if got := ActiveElapsed(run, events, start.Add(time.Hour)); got != 2*time.Minute {
		t.Fatalf("unrelated probe closed the wait: active %s", got)
	}
	events = append(events, operationPair("blocked", start.Add(time.Hour), 2*time.Minute)...)
	if got := ActiveElapsed(run, events, start.Add(65*time.Minute)); got != 7*time.Minute {
		t.Fatalf("same-operation retry did not close the wait: active %s", got)
	}
	// A second deterministic failure opens a new interval, not the old one.
	events = append(events, EngineeringEvent{Type: EventOperationAfter, OperationID: "blocked",
		OccurredAt: start.Add(65 * time.Minute), Payload: failure})
	if got := ActiveElapsed(run, events, start.Add(2*time.Hour)); got != 7*time.Minute {
		t.Fatalf("second contradiction wait: active %s", got)
	}
}

func (d *timedRecoveryDetector) Detect(model domain.ProjectModel, in analysis.Input) ([]domain.EngineeringFact, error) {
	if in.Stage == domain.StageObserved && !d.spent {
		d.spent = true
		d.clock.advance(2 * time.Minute)
		d.cancel()
	}
	return (analysis.CriticalBoundaryDetector{}).Detect(model, in)
}

func TestDeterministicWaitClosesAtRetryAndStaysClosedAcrossRestart(t *testing.T) {
	f := newPhase8Fixture(t)
	conflictingObservedPolicy(f)
	id := f.start()
	f.clock.step = 0
	f.reconcile(id)
	op := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	status := func() StatusReport {
		t.Helper()
		s, err := f.runtime.Status(id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := status()
	failureAt := before.Now
	f.clock.advance(time.Hour)
	reopen(t, f)
	if during := status(); during.ActiveElapsed != before.ActiveElapsed || during.ExternalWaitElapsed-before.ExternalWaitElapsed != time.Hour {
		t.Fatalf("restart charged idle wait: before %+v, during %+v", before, during)
	}
	delete(f.deps.Policy.Rules, "observed-conflict")
	f.deps.Policy.Revision = "2"
	f.runtime = f.newRuntime(f.deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runtime.flow = KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(&timedRecoveryDetector{clock: f.clock, cancel: cancel})}
	if out, err := f.runtime.Reconcile(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop just after recovery: %+v / %v", out, err)
	}
	state := f.state(id)
	if recovered := state.snapshot.Operations[op.ID]; recovered.State != Succeeded || recovered.Attempt != 2 {
		t.Fatalf("recovery %+v", recovered)
	}
	var retryAt time.Time
	for _, event := range state.events {
		if event.Type == EventOperationBefore && event.OperationID == op.ID {
			retryAt = event.OccurredAt
		}
	}
	if retryAt != failureAt.Add(time.Hour) {
		t.Fatalf("retry at %s, want %s", retryAt, failureAt.Add(time.Hour))
	}
	after := status()
	if after.ActiveElapsed-before.ActiveElapsed != 2*time.Minute || after.ExternalWaitElapsed-before.ExternalWaitElapsed != time.Hour {
		t.Fatalf("recovery accounting: before %+v, after %+v", before, after)
	}
	f.clock.advance(3 * time.Minute)
	continued := status()
	if continued.ActiveElapsed-after.ActiveElapsed != 3*time.Minute || continued.ExternalWaitElapsed != after.ExternalWaitElapsed {
		t.Fatalf("wait remained open after recovery: after %+v, continued %+v", after, continued)
	}
	reopen(t, f)
	replayed := status()
	if replayed.ActiveElapsed != continued.ActiveElapsed || replayed.ExternalWaitElapsed != continued.ExternalWaitElapsed {
		t.Fatalf("restart reopened wait: continued %+v, replayed %+v", continued, replayed)
	}
	f.clock.advance(2 * time.Hour)
	if out := f.reconcile(id); out.Disposition != Failed || out.Reason != "run_wall_budget_exhausted" {
		t.Fatalf("recovered run escaped wall budget: %+v", out)
	}
}
