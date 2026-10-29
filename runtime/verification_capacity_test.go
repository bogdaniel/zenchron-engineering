package runtime

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// verificationScheduler is a scheduler bound to all three ceilings.
func verificationScheduler(store OperationStore, owner string, runs, verifications int) Scheduler {
	s := capacityScheduler(store, owner, runs, 2)
	s.MaxConcurrentVerifications = verifications
	return s
}

// TestTheVerificationCeilingIsIndependentOfTheWorkCeiling is #490's fleet
// shape on both stores: work capacity 10, verification capacity 3. Ten runs
// ask for verification at once, concurrently, from ten drivers: exactly three
// are admitted. Providers keep reasoning in the work slots the verifications
// leave, the refused verifications spend no attempt, and a freed verification
// slot is handed on.
func TestTheVerificationCeilingIsIndependentOfTheWorkCeiling(t *testing.T) {
	sqlite, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close() })
	for name, store := range map[string]OperationStore{"sqlite": sqlite, "memory": NewMemoryOperationStore()} {
		planner := verificationScheduler(store, "planner", 10, 3)
		queued := map[string]RunOperation{}
		for i := range 10 {
			run := fmt.Sprintf("verify-%d", i)
			queued[run] = planKind(t, planner, run, OpAssuranceGo)
		}
		var mu sync.Mutex
		var wg sync.WaitGroup
		admitted := map[string]*RunOperation{}
		for run := range queued {
			wg.Add(1)
			go func() {
				defer wg.Done()
				leased, err := verificationScheduler(store, "driver-"+run, 10, 3).Next(run)
				if err != nil {
					t.Error(err)
					return
				}
				if leased != nil {
					mu.Lock()
					admitted[run] = leased
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if len(admitted) != 3 {
			t.Fatalf("%s: %d verifications ran at once under a verification ceiling of 3", name, len(admitted))
		}
		// Seven work slots remain, and providers take every one of them.
		for i := range 7 {
			run := fmt.Sprintf("provider-%d", i)
			planKind(t, planner, run, OpExecutionInvoke)
			if mustNext(t, planner, run) == nil {
				t.Fatalf("%s: provider %d was refused while only verification capacity was full", name, i)
			}
		}
		planKind(t, planner, "provider-extra", OpExecutionInvoke)
		if mustNext(t, planner, "provider-extra") != nil {
			t.Fatalf("%s: an eleventh run worked under a work ceiling of 10; a verification is work", name)
		}
		var freed string
		for run, op := range queued {
			if _, held := admitted[run]; held {
				freed = run
				continue
			}
			stored, _, _, err := store.Operation(op.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.State != Pending || stored.Attempt != 0 || stored.Lease != nil {
				t.Fatalf("%s: a refused verification spent budget or holds a lease: %+v", name, stored)
			}
		}
		if _, err := verificationScheduler(store, "driver-"+freed, 10, 3).Finish(admitted[freed].ID, Succeeded); err != nil {
			t.Fatal(err)
		}
		handedOn := 0
		for run := range queued {
			if _, held := admitted[run]; held {
				continue
			}
			if leased := mustNext(t, verificationScheduler(store, "late-"+run, 10, 3), run); leased != nil {
				handedOn++
			}
		}
		if handedOn != 1 {
			t.Fatalf("%s: one freed verification slot admitted %d queued verifications", name, handedOn)
		}
	}
}

// TestVerificationOccupancySurvivesARestart: the slots are durable leases, so a
// reopened store still refuses a queued verification, and no held one is
// leased a second time.
func TestVerificationOccupancySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := verificationScheduler(store, "before", 10, 1)
	held := planKind(t, s, "held", OpAssuranceGo)
	queued := planKind(t, s, "queued", OpAssuranceGo)
	if mustNext(t, s, "held") == nil {
		t.Fatal("the only verification slot was not granted")
	}
	store.Close()
	reopened := reopenStore(t, dir)
	after := verificationScheduler(reopened, "after", 10, 1)
	if mustNext(t, after, "queued") != nil {
		t.Fatal("a reopened store forgot the verification slot is held")
	}
	if mustNext(t, after, "held") != nil {
		t.Fatal("a held verification was leased a second time after a restart")
	}
	if saturated, err := after.VerificationSaturated("queued"); err != nil || !saturated {
		t.Fatalf("saturation after a restart = %v (%v), want true", saturated, err)
	}
	for _, op := range []RunOperation{held, queued} {
		stored, _, _, err := reopened.Operation(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		wantState, wantOwner := Pending, ""
		if op.ID == held.ID {
			wantState, wantOwner = Leased, "before"
		}
		owner := ""
		if stored.Lease != nil {
			owner = stored.Lease.Owner
		}
		if stored.State != wantState || owner != wantOwner || stored.Attempt != 0 {
			t.Fatalf("%s after a restart = %s owned by %q attempt %d, want %s owned by %q attempt 0",
				op.RunID, stored.State, owner, stored.Attempt, wantState, wantOwner)
		}
	}
}

// TestAVerificationCapacityWaitIsNotAFailure drives a run through the engine
// while another controller holds the only verification slot. The run waits
// with the capacity reason: the verifier is never called, nothing is
// journalled as an assurance result, no remediation is invoked and the
// assurance attempt is untouched. Once the slot frees, the same operation
// runs under its ordinary semantics.
func TestAVerificationCapacityWaitIsNotAFailure(t *testing.T) {
	f := newPhase8Fixture(t)
	f.deps.Liveness = alwaysAlive()
	// A verification is work too, so the occupant holds a work slot: the run
	// needs a second one to reach its own verification.
	f.deps.MaxConcurrentRuns, f.deps.OperatorMaxConcurrentRuns = 10, 10
	f.deps.MaxConcurrentVerifications = 1
	assurance := passingAssurance()
	f.deps.Assurance = assurance
	f.runtime = f.newRuntime(f.deps)

	occupant := verificationScheduler(f.store, "other-controller", 10, 1)
	occupied := planKind(t, occupant, "occupant", OpAssuranceGo)
	if mustNext(t, occupant, "occupant") == nil {
		t.Fatal("the occupant was not granted the verification slot")
	}

	runID := f.start()
	var outcome Outcome
	for pass := 0; pass < 20 && outcome.Reason != ReasonVerificationCapacity; pass++ {
		outcome = f.reconcile(runID)
	}
	if outcome.Disposition != Waiting || outcome.Reason != ReasonVerificationCapacity {
		t.Fatalf("outcome = %s/%s, want waiting on verification capacity", outcome.Disposition, outcome.Reason)
	}
	state := f.state(runID)
	if len(assurance.Requests) != 0 || len(automatedAssurancePayloads(t, state.events)) != 0 {
		t.Fatal("a verification ran, or an assurance result was journalled, without a verification slot")
	}
	if len(f.provider.requests) != 1 {
		t.Fatalf("a capacity wait invoked the provider again: %d invocations", len(f.provider.requests))
	}
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpAssuranceGo && (op.State != Pending || op.Attempt != 0) {
			t.Fatalf("the waiting verification spent its budget: %+v", op)
		}
	}
	fleet, err := FleetStatus(f.store, f.stateDir, 10, 2, 1, f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fleet.AwaitingVerification != 1 || fleet.Runnable != 1 || fleet.VerificationCapacity != 1 {
		t.Fatalf("fleet = awaiting %d runnable %d capacity %d, want the run shown awaiting verification",
			fleet.AwaitingVerification, fleet.Runnable, fleet.VerificationCapacity)
	}

	if _, err := occupant.Finish(occupied.ID, Succeeded); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 20 && len(assurance.Requests) == 0; pass++ {
		f.reconcile(runID)
	}
	observed := automatedAssurancePayloads(t, f.state(runID).events)
	if len(assurance.Requests) != 1 || len(observed) != 1 || !observed[0].Passed {
		t.Fatalf("the freed slot did not run the verification once under its ordinary semantics: %d requests, %+v",
			len(assurance.Requests), observed)
	}
}

// TestAVerificationCapacityWaitDoesNotSpendTheRunBudget: the run executes
// nothing while it waits for a slot, so the wait is excluded from active work.
func TestAVerificationCapacityWaitDoesNotSpendTheRunBudget(t *testing.T) {
	start := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	state := &runState{
		run:    EngineeringRun{CreatedAt: start},
		events: []EngineeringEvent{waitEvent(start.Add(time.Minute), ReasonVerificationCapacity)},
	}
	if active := state.activeElapsed(start.Add(2 * time.Hour)); active > 2*time.Minute {
		t.Fatalf("waiting on verification capacity spent %s of the run's active-work budget", active)
	}
}

func TestTheDefaultVerificationCeilingIsTwo(t *testing.T) {
	if DefaultMaxConcurrentVerifications != 2 || resolveMaxConcurrentVerifications(0) != 2 {
		t.Fatalf("the default verification ceiling is 2, got %d", resolveMaxConcurrentVerifications(0))
	}
	if !consumesVerification(OpAssuranceGo) || consumesVerification(OpExecutionInvoke) || consumesVerification(OpAssuranceSemantic) {
		t.Fatal("only the host verifier consumes verification capacity")
	}
}

// TestTheStricterVerificationBoundWins: supervisor and watch both state the
// verification ceiling, the stricter wins, unstated is the default, and a
// negative operator value is refused rather than read as unstated.
func TestTheStricterVerificationBoundWins(t *testing.T) {
	for _, c := range []struct{ supervisor, watch, want int }{{1, 3, 1}, {3, 1, 1}, {0, 0, DefaultMaxConcurrentVerifications}} {
		settings, err := OperatorConfig{
			Supervisor: SupervisorConfig{MaxConcurrentVerifications: c.supervisor},
			Watch:      WatchConfig{MaxConcurrentVerifications: c.watch},
		}.WatchSettings()
		if err != nil || settings.MaxConcurrentVerifications != c.want {
			t.Fatalf("supervisor %d, watch %d: got %d (%v), want %d", c.supervisor, c.watch, settings.MaxConcurrentVerifications, err, c.want)
		}
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "config.json"), strings.Replace(operatorConfigJSON(dir), "{\n",
		"{\n\t\"supervisor\": {\"max_concurrent_verifications\": -1},\n", 1))
	requireConfigError(t, second(LoadOperatorConfig(filepath.Join(dir, "config.json"))))
}

// TestAnEnvironmentalVerifierFailureIsNotACandidateFinding is the other half
// of #490's boundary. An admitted verifier that fails on a recognized
// infrastructure condition - the baseline verifier's non-verdict exits, such
// as a signal death under host pressure (TestBaselineVerifierReturnsAJudged
// FailureWithoutAnError pins that classification) - retries the same
// verification and never reaches the worker as remediation. A judged failure
// still does (TestBaselineVerifierFailuresReachTheConfirmationRerun).
func TestAnEnvironmentalVerifierFailureIsNotACandidateFinding(t *testing.T) {
	f := newPhase8Fixture(t)
	verifier := &alwaysFailingVerifier{class: FailureTransientInfrastructure}
	f.useAssurance(verifier)
	runID := f.start()
	for pass := 0; pass < 12 && verifier.calls < f.deps.Budgets.MaxAssuranceAttempts; pass++ {
		f.reconcile(runID)
	}
	if verifier.calls < 2 {
		t.Fatalf("the environmental failure was not retried: %d verifications", verifier.calls)
	}
	if len(f.provider.requests) != 1 {
		t.Fatalf("an environmental verifier failure reached the worker: %d invocations", len(f.provider.requests))
	}
	for _, observed := range automatedAssurancePayloads(t, f.state(runID).events) {
		if CandidateVerdict(AssuranceResult{Passed: observed.Passed, FailureClass: observed.FailureClass}) {
			t.Fatalf("an environmental verifier failure was journalled as a verdict about the candidate: %+v", observed)
		}
	}
}
