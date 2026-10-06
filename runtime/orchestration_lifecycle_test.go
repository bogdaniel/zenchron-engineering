package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func lifecycleEvent(t *testing.T, store *SQLiteOperationStore, runID, kind string, payload any) {
	t.Helper()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event := EngineeringEvent{SchemaVersion: SchemaVersion, ID: fmt.Sprintf("%s-event-%d", runID, len(events)+1), RunID: runID, Type: kind, OccurredAt: time.Unix(100+int64(len(events)), 0).UTC(), Payload: raw}
	if op, ok := payload.(RunOperation); ok {
		event.OperationID = op.ID
	}
	if _, err := store.AppendEvent(event); err != nil {
		t.Fatal(err)
	}
}

func lifecycleBatch(t *testing.T, store *SQLiteOperationStore, runID string) string {
	t.Helper()
	id, err := orchestration.BatchID("acme/repo", "worker", []int{41})
	if err != nil {
		t.Fatal(err)
	}
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion, ID: id, Repository: "acme/repo", AgentID: "worker", CreatedAt: time.Unix(100, 0).UTC(), Items: []orchestration.BatchItem{{Issue: 41, RunID: runID}}}
	if _, _, err := store.CreateOrchestrationBatch(batch); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestOrchestrationStatusUsesCurrentDurableLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name            string
		kind            string
		wait            string
		acquire         bool
		start           bool
		journalFinished bool
		rowFinished     bool
		rowDisposition  Disposition
		paused          bool
		want            orchestration.ItemState
		transitional    bool
	}{
		{name: "queued source observation", kind: OpSourceObserve, wait: ReasonObservationCapacity, want: orchestration.ItemQueued},
		{name: "queued execution", kind: OpExecutionInvoke, wait: ReasonWorkCapacity, want: orchestration.ItemQueued},
		{name: "operator pause overrides capacity queue", wait: ReasonObservationCapacity, paused: true, want: orchestration.ItemWaiting},
		{name: "operator pause over active disposition", paused: true, want: orchestration.ItemWaiting, transitional: true},
		{name: "acquired operation", kind: OpExecutionInvoke, acquire: true, want: orchestration.ItemRunning},
		{name: "operation running", kind: OpExecutionInvoke, acquire: true, start: true, want: orchestration.ItemRunning},
		{name: "waiting disposition while operation resumes", kind: OpExecutionInvoke, wait: ReasonWorkCapacity, acquire: true, start: true, want: orchestration.ItemRunning, transitional: true},
		{name: "provider done, runtime committing", kind: OpCandidateCommit, acquire: true, start: true, want: orchestration.ItemRunning},
		{name: "assurance remediation", kind: OpRemediationGofmt, acquire: true, start: true, want: orchestration.ItemRunning},
		{name: "work complete, handoff absent", wait: ReasonGoalStateReached, want: orchestration.ItemHandoffPending},
		{name: "old goal after newer completed work", kind: OpRemediationGofmt, wait: ReasonGoalStateReached, acquire: true, start: true, journalFinished: true, rowFinished: true, want: orchestration.ItemQueued, transitional: true},
		{name: "journal finished before row, crash", kind: OpExecutionInvoke, acquire: true, start: true, journalFinished: true, want: "", transitional: true},
		{name: "row finished before journal", kind: OpExecutionInvoke, acquire: true, start: true, rowFinished: true, want: "", transitional: true},
		{name: "run row and journal disagree", wait: ReasonGoalStateReached, rowDisposition: Active, want: "", transitional: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, store := openJournal(t)
			run, _, err := store.Run("r")
			if err != nil {
				t.Fatal(err)
			}
			if tc.wait != "" {
				var capacity *CapacityWait
				if tc.wait == ReasonWorkCapacity {
					capacity = &CapacityWait{CapacityWork, 10}
				}
				if tc.wait == ReasonObservationCapacity {
					capacity = &CapacityWait{CapacityObservation, 2}
				}
				lifecycleEvent(t, store, run.ID, EventRunWaiting, dispositionRecord{Reason: tc.wait, Capacity: capacity})
				run.Disposition, run.Reason = Waiting, tc.wait
				if tc.rowDisposition != "" {
					run.Disposition, run.Reason = tc.rowDisposition, ""
				}
				if err := store.PutRun(run); err != nil {
					t.Fatal(err)
				}
			}
			scheduler := capacityScheduler(store, "owner", 10, 2)
			if tc.kind != "" {
				op := planKind(t, scheduler, run.ID, tc.kind)
				lifecycleEvent(t, store, run.ID, EventOperationPlanned, op)
				if tc.acquire {
					if mustNext(t, scheduler, run.ID) == nil {
						t.Fatal("operation not acquired")
					}
					if tc.start {
						var err error
						op, err = scheduler.Start(op.ID)
						if err != nil {
							t.Fatal(err)
						}
						lifecycleEvent(t, store, run.ID, EventOperationBefore, op)
						if tc.journalFinished {
							ended := op
							ended.State = Succeeded
							ended.Lease = nil
							lifecycleEvent(t, store, run.ID, EventOperationAfter, ended)
						}
						if tc.rowFinished {
							if _, err := scheduler.Finish(op.ID, Succeeded); err != nil {
								t.Fatal(err)
							}
						}
					}
				}
			}
			if tc.paused {
				if _, err := PauseRun(store, time.Unix(150, 0).UTC(), run.ID, "operator maintenance", "operator"); err != nil {
					t.Fatal(err)
				}
			}
			batchID := lifecycleBatch(t, store, run.ID)
			check := func(read *SQLiteOperationStore) {
				t.Helper()
				view, err := OrchestrationStatus(read, dir, batchID, time.Unix(200, 0).UTC())
				if err != nil {
					t.Fatal(err)
				}
				item := view.Items[0]
				if item.State != tc.want || (item.Observation == "transitioning") != tc.transitional {
					t.Fatalf("item=%+v, want state %q transitioning=%v", item, tc.want, tc.transitional)
				}
				if tc.paused && item.Paused == nil {
					t.Fatal("operator pause disappeared from status")
				}
				if tc.acquire && !tc.rowFinished && !item.Executing {
					t.Fatal("owned operation vanished from status")
				}
				if tc.wait == ReasonObservationCapacity && !tc.acquire && !tc.paused && (item.Capacity == nil || item.Capacity.Class != CapacityObservation || item.Capacity.Ceiling != 2) {
					t.Fatalf("capacity=%+v", item.Capacity)
				}
				if item.Executing && (item.State == orchestration.ItemCompleted || item.State == orchestration.ItemPartial || item.State == orchestration.ItemHandoffPending) {
					t.Fatalf("owned operation reported settled: %+v", item)
				}
			}
			check(store)
			reopened, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			check(reopened)
		})
	}
}

func TestCapacityRefusalNamesDurableClassAndCeiling(t *testing.T) {
	for _, tc := range []struct {
		kind    string
		class   CapacityClass
		ceiling int
	}{
		{OpSourceObserve, CapacityObservation, 2}, {OpExecutionInvoke, CapacityWork, 3},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			_, store := openJournal(t)
			s := capacityScheduler(store, "owner", 3, 2)
			for i := range tc.ceiling {
				id := fmt.Sprintf("holder-%d", i)
				if err := store.PutRun(newJournalRun(id)); err != nil {
					t.Fatal(err)
				}
				planKind(t, s, id, tc.kind)
				if mustNext(t, s, id) == nil {
					t.Fatal("holder not acquired")
				}
			}
			queued := planKind(t, s, "r", tc.kind)
			if mustNext(t, s, "r") != nil {
				t.Fatal("capacity exceeded")
			}
			wait, err := s.CapacityBlocked(queued)
			if err != nil || wait == nil || wait.Class != tc.class || wait.Ceiling != tc.ceiling {
				t.Fatalf("wait=%+v err=%v", wait, err)
			}
			durable, _, found, err := store.Operation(queued.ID)
			if err != nil || !found || durable.Attempt != 0 {
				t.Fatalf("capacity refusal consumed an attempt: %+v %v", durable, err)
			}
		})
	}
}

func TestObservationCapacityWaitSurvivesRestartAndConfigurationChange(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	fixture.deps.MaxConcurrentObservations = 2
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	s := capacityScheduler(fixture.store, "owner-1", 10, 2)
	for i := range 2 {
		id := fmt.Sprintf("observation-holder-%d", i)
		if err := fixture.store.PutRun(newJournalRun(id)); err != nil {
			t.Fatal(err)
		}
		planKind(t, s, id, OpSourceObserve)
		if mustNext(t, s, id) == nil {
			t.Fatal("holder not acquired")
		}
	}
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := engine.Reconcile(context.Background(), view.Items[0].RunID)
	if err != nil || outcome.Reason != ReasonObservationCapacity {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	item := fixture.status(view.BatchID).Items[0]
	if item.State != orchestration.ItemQueued || item.Capacity == nil || item.Capacity.Ceiling != 2 {
		t.Fatalf("item=%+v", item)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fixture.deps.MaxConcurrentObservations = 17
	recovered, err := OrchestrationStatus(reopened, fixture.stateDir, view.BatchID, fixture.clock.Now())
	if err != nil || recovered.Items[0].Capacity == nil || recovered.Items[0].Capacity.Ceiling != 2 {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
	if strings.Contains(recovered.Items[0].Reason, "operation_unavailable") {
		t.Fatal("known capacity was reported unknown")
	}
}

func TestAdmittedOrRefusedHandoffCannotSettleOwnedWork(t *testing.T) {
	for _, tc := range []struct {
		name      string
		behaviour fleetBehaviour
		handoff   orchestration.HandoffObservation
	}{
		{"admitted handoff, assurance remediation", fleetValidHandoff, orchestration.HandoffAdmitted},
		{"refused handoff, bounded repair executing", fleetNoHandoff, orchestration.HandoffRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newFleetFixture(t, 10)
			view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
			runID := view.Items[0].RunID
			fixture.worker.set(runID, tc.behaviour)
			settled := fixture.drive(fixture.supervisor(), view.BatchID)
			if settled.Items[0].Handoff != tc.handoff {
				t.Fatalf("handoff=%s", settled.Items[0].Handoff)
			}
			s := capacityScheduler(fixture.store, "owner-1", 10, 2)
			op := planKind(t, s, runID, OpRemediationGofmt)
			lifecycleEvent(t, fixture.store, runID, EventOperationPlanned, op)
			if mustNext(t, s, runID) == nil {
				t.Fatal("remediation not acquired")
			}
			op, err := s.Start(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			lifecycleEvent(t, fixture.store, runID, EventOperationBefore, op)
			item := fixture.status(view.BatchID).Items[0]
			if item.State != orchestration.ItemRunning || !item.Executing || item.Handoff != tc.handoff || item.Operation != OpRemediationGofmt || item.Observation != "transitioning" {
				t.Fatalf("owned remediation was settled: %+v", item)
			}
			if tc.handoff == orchestration.HandoffRefused && (item.HandoffReason == "" || item.Reason == item.HandoffReason) {
				t.Fatalf("handoff refusal replaced lifecycle reason: %+v", item)
			}
			ended, err := s.Finish(op.ID, Succeeded)
			if err != nil {
				t.Fatal(err)
			}
			lifecycleEvent(t, fixture.store, runID, EventOperationAfter, ended)
			lifecycleEvent(t, fixture.store, runID, EventRunFailed, dispositionRecord{Reason: "run_wall_budget_exhausted"})
			run, _, err := fixture.store.Run(runID)
			if err != nil {
				t.Fatal(err)
			}
			run.Disposition, run.Reason = Failed, "run_wall_budget_exhausted"
			if err := fixture.store.PutRun(run); err != nil {
				t.Fatal(err)
			}
			item = fixture.status(view.BatchID).Items[0]
			if item.State != orchestration.ItemFailed || item.Handoff != tc.handoff || item.Reason != run.Reason {
				t.Fatalf("stale handoff hid terminal failure: %+v", item)
			}
		})
	}
}

func TestCapacityWaitPayloadRejectsUnsupportedFacts(t *testing.T) {
	for _, tc := range []struct {
		name, kind, reason string
		wait               CapacityWait
	}{
		{"unknown class", EventRunWaiting, ReasonWorkCapacity, CapacityWait{"unknown", 2}},
		{"zero ceiling", EventRunWaiting, ReasonWorkCapacity, CapacityWait{CapacityWork, 0}},
		{"reason mismatch", EventRunWaiting, ReasonWorkCapacity, CapacityWait{CapacityObservation, 2}},
		{"terminal metadata", EventRunCompleted, ReasonWorkCapacity, CapacityWait{CapacityWork, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, store := openJournal(t)
			raw, err := json.Marshal(dispositionRecord{Reason: tc.reason, Capacity: &tc.wait})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "invalid", RunID: "r", Type: tc.kind, OccurredAt: time.Unix(101, 0).UTC(), Payload: raw})
			if err == nil {
				t.Fatal("unsupported capacity fact admitted")
			}
		})
	}
}

// This adapter holds the real provider boundary with synchronization, so status
// is read during an actual scheduler-owned invocation rather than after it.
type lifecycleHeldProvider struct {
	*fleetProvider
	started chan struct{}
	resume  chan struct{}
}

func (p *lifecycleHeldProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	close(p.started)
	select {
	case <-ctx.Done():
		return ExecutionResult{}, ctx.Err()
	case <-p.resume:
		return p.fleetProvider.Execute(ctx, request)
	}
}

func TestOrchestrationCannotSettleDuringPhysicalProviderExecution(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	provider := &lifecycleHeldProvider{fleetProvider: fixture.worker, started: make(chan struct{}), resume: make(chan struct{})}
	fixture.deps.Provider = provider
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	resume := func() { once.Do(func() { close(provider.resume) }) }
	finished := make(chan struct{})
	done := make(chan error, 1)
	t.Cleanup(func() {
		resume()
		cancel()
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			t.Error("provider driver did not stop")
		}
	})
	go func() { defer close(finished); _, err := engine.Reconcile(ctx, view.Items[0].RunID); done <- err }()
	select {
	case <-provider.started:
	case err := <-done:
		t.Fatalf("driver ended before invocation: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("provider never started")
	}
	status := fixture.status(view.BatchID)
	item := status.Items[0]
	if item.State != orchestration.ItemRunning || !item.Executing || status.Counts.Completed != 0 || status.Counts.Partial != 0 || status.Counts.HandoffPending != 0 {
		t.Fatalf("physical invocation reported settled: %+v", status)
	}
	resume()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("driver did not finish")
	}
	admitOnce(t, fixture, view.BatchID)
	item = fixture.status(view.BatchID).Items[0]
	if item.State != orchestration.ItemCompleted || item.Executing {
		t.Fatalf("finished producer never settled after admission: %+v", item)
	}
}

func TestGrantedVerificationToolPreventsProducerSettlement(t *testing.T) {
	dir, store := openJournal(t)
	s := verificationScheduler(store, "controller", 10, 2)
	op := planKind(t, s, "r", OpExecutionInvoke)
	lifecycleEvent(t, store, "r", EventOperationPlanned, op)
	if mustNext(t, s, "r") == nil {
		t.Fatal("parent not acquired")
	}
	op, err := s.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEvent(t, store, "r", EventOperationBefore, op)
	permit, err := s.RequestVerification(ExecutionAttemptRef{op.RunID, op.ID, op.AttemptIdentity}, "tool", "tool-owner")
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.AcquireVerification(permit); err != nil || !ok {
		t.Fatalf("grant=%v err=%v", ok, err)
	}
	ended, err := s.Finish(op.ID, Succeeded)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleEvent(t, store, "r", EventOperationAfter, ended)
	lifecycleEvent(t, store, "r", EventRunWaiting, dispositionRecord{Reason: ReasonGoalStateReached})
	run, _, err := store.Run("r")
	if err != nil {
		t.Fatal(err)
	}
	run.Disposition, run.Reason = Waiting, ReasonGoalStateReached
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	batchID := lifecycleBatch(t, store, "r")
	view, err := OrchestrationStatus(store, dir, batchID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	item := view.Items[0]
	if item.State != orchestration.ItemRunning || !item.Executing || len(item.VerificationTools) != 1 {
		t.Fatalf("held nested tool reported settled: %+v", item)
	}
	lifecycleEvent(t, store, "r", EventRunFailed, dispositionRecord{Reason: "run_wall_budget_exhausted"})
	run.Disposition, run.Reason = Failed, "run_wall_budget_exhausted"
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	view, err = OrchestrationStatus(store, dir, batchID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if item := view.Items[0]; item.State != orchestration.ItemFailed || !item.Executing || item.Observation != "transitioning" {
		t.Fatalf("terminal run hid retained ownership: %+v", item)
	}
	s.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner != "tool-owner" })
	if err := s.ReleaseVerification(permit); err != nil {
		t.Fatal(err)
	}
	view, err = OrchestrationStatus(store, dir, batchID, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if view.Items[0].Executing || view.Items[0].State != orchestration.ItemFailed {
		t.Fatalf("released tool prevents settlement: %+v", view.Items[0])
	}
}

func TestSchedulerCapacityWaitExcludesQueueTimeButCountsResumedWork(t *testing.T) {
	for _, reason := range []string{ReasonWorkCapacity, ReasonObservationCapacity} {
		t.Run(reason, func(t *testing.T) {
			start := time.Unix(100, 0).UTC()
			run := EngineeringRun{CreatedAt: start}
			events := []EngineeringEvent{waitEvent(start.Add(time.Minute), reason)}
			resumed := start.Add(time.Hour)
			if got := ActiveElapsed(run, events, resumed); got != time.Minute {
				t.Fatalf("queue time consumed active budget: %s", got)
			}
			before, err := json.Marshal(RunOperation{ID: "op", RunID: "r", Kind: OpExecutionInvoke, State: Running})
			if err != nil {
				t.Fatal(err)
			}
			events = append(events, EngineeringEvent{Type: EventOperationBefore, OperationID: "op", OccurredAt: resumed, Payload: before})
			if got := ActiveElapsed(run, events, resumed.Add(2*time.Minute)); got != 3*time.Minute {
				t.Fatalf("resumed work was excluded: %s", got)
			}
		})
	}
}
