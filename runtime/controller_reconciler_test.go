package runtime

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// scriptedService records what the executor asked for and answers with whatever
// the case needs, including a role that disappears between the two calls.
type scriptedService struct {
	recoverErr func() error
	enableErr  func() error
	calls      []string
}

func (s *scriptedService) RecoverActivated(handoffID string) (ControllerHandoff, error) {
	s.calls = append(s.calls, "recover:"+handoffID)
	if s.recoverErr == nil {
		return ControllerHandoff{ID: handoffID, Phase: HandoffActivated}, nil
	}
	return ControllerHandoff{ID: handoffID, Phase: HandoffActivated}, s.recoverErr()
}

func (s *scriptedService) EnableWorkAdmission(handoffID string) error {
	s.calls = append(s.calls, "enable:"+handoffID)
	if s.enableErr == nil {
		return nil
	}
	return s.enableErr()
}

func resumeIntent() ReconciliationIntent {
	generation := activeGeneration()
	return ReconciliationIntent{
		Action: ReconcileResumeActivatedService, HandoffID: "handoff-1",
		Generation: &generation, Reason: "this controller is the activated generation and is not admitting work",
	}
}

func executedAt() time.Time { return time.Unix(1700000011, 0).UTC() }

// THE CONVERGENCE, and the reason the intent is named after it: one call does
// not reach it.
func TestResumingServiceComposesBothOperations(t *testing.T) {
	service := &scriptedService{}
	result := ExecuteReconciliation(service, resumeIntent(), executedAt())

	if got := strings.Join(service.calls, " "); got != "recover:handoff-1 enable:handoff-1" {
		t.Fatalf("calls = %q, want recovery then admission", got)
	}
	if !result.Converged {
		t.Fatalf("the executor did not converge: %s", result.Summary())
	}
	for name, step := range map[string]ReconciliationStep{
		"recovery": result.Recovery, "projection": result.Projection, "admission": result.WorkAdmission,
	} {
		if step.Outcome != StepSucceeded {
			t.Fatalf("%s = %q, want succeeded", name, step.Outcome)
		}
	}
}

// A PROJECTION THAT WOULD NOT REPAIR DOES NOT STOP SERVICE. The naive
// early-return here would quietly make the stable entrypoint a condition of
// serving, undoing #276 in the least visible place available.
func TestProjectionFailureStillOpensService(t *testing.T) {
	service := &scriptedService{recoverErr: func() error {
		return &ProjectionRepairFailedError{HandoffID: "handoff-1", Cause: fmt.Errorf("a directory occupies the entrypoint")}
	}}
	result := ExecuteReconciliation(service, resumeIntent(), executedAt())

	if got := strings.Join(service.calls, " "); got != "recover:handoff-1 enable:handoff-1" {
		t.Fatalf("calls = %q: admission must still be attempted after a failed repair", got)
	}
	if result.Recovery.Outcome != StepSucceeded {
		t.Fatalf("recovery = %q: a failed projection is not a failed recovery", result.Recovery.Outcome)
	}
	if result.Projection.Outcome != StepDrifted {
		t.Fatalf("projection = %q, want drifted", result.Projection.Outcome)
	}
	if result.WorkAdmission.Outcome != StepSucceeded || !result.Converged {
		t.Fatalf("service did not open despite an active controller: %s", result.Summary())
	}
	if !strings.Contains(result.Projection.Detail, "directory occupies") {
		t.Fatalf("the drift lost its cause: %q", result.Projection.Detail)
	}
}

// ANY OTHER REFUSAL STOPS. The recovery that would have established the right
// to serve did not happen, and asking anyway would be hoping a second check
// disagrees with the first.
func TestAuthorityRefusalNeverReachesAdmission(t *testing.T) {
	for _, refusal := range []struct {
		name string
		err  error
	}{
		{"this process is not the activated generation", fmt.Errorf("this process is not the activated generation: measured x, record y")},
		{"the role is not held", &ControllerRoleUnheldError{Detail: "the controller role lease has been released"}},
		{"the transition never activated", fmt.Errorf("transition handoff-1 is at phase %q", HandoffSuccessorAcquired)},
	} {
		t.Run(refusal.name, func(t *testing.T) {
			service := &scriptedService{recoverErr: func() error { return refusal.err }}
			result := ExecuteReconciliation(service, resumeIntent(), executedAt())

			if got := strings.Join(service.calls, " "); got != "recover:handoff-1" {
				t.Fatalf("calls = %q: admission must not be attempted after an authority refusal", got)
			}
			if result.Recovery.Outcome != StepRefused {
				t.Fatalf("recovery = %q, want refused", result.Recovery.Outcome)
			}
			if result.WorkAdmission.Outcome != StepNotAttempted {
				t.Fatalf("admission = %q, want not attempted", result.WorkAdmission.Outcome)
			}
			if result.Converged {
				t.Fatal("a refused recovery reported convergence")
			}
		})
	}
}

// THE ROLE CAN VANISH BETWEEN THE TWO CALLS. The second operation refuses
// through its own authority check; the executor reports an incomplete
// convergence and reconstructs nothing.
//
// This is the executor's version of the races pinned in #271 and #274: the
// dangerous response is not the refusal, it is any attempt to recover from it.
func TestRoleLostBetweenRecoveryAndAdmissionIsReportedNotRepaired(t *testing.T) {
	service := &scriptedService{enableErr: func() error {
		return &ControllerRoleUnheldError{Detail: "the controller role lease has been released"}
	}}
	result := ExecuteReconciliation(service, resumeIntent(), executedAt())

	if got := strings.Join(service.calls, " "); got != "recover:handoff-1 enable:handoff-1" {
		t.Fatalf("calls = %q: the executor retried or reacquired something", got)
	}
	if result.Recovery.Outcome != StepSucceeded {
		t.Fatalf("recovery = %q: the step that did succeed must still say so", result.Recovery.Outcome)
	}
	if result.WorkAdmission.Outcome != StepRefused {
		t.Fatalf("admission = %q, want refused", result.WorkAdmission.Outcome)
	}
	if result.Converged {
		t.Fatal("a refused admission reported convergence")
	}
	// The four worlds stay distinguishable, which is the point of reporting
	// per step rather than returning one error.
	if !strings.Contains(result.Summary(), "recovery succeeded") ||
		!strings.Contains(result.Summary(), "admission refused") {
		t.Fatalf("the summary flattened the steps: %s", result.Summary())
	}
}

// Repairing a projection asks for the recorded activation to be resumed, which
// is the operation that repairs the entrypoint. It never opens service.
func TestRepairingAProjectionDoesNotOpenService(t *testing.T) {
	generation := activeGeneration()
	intent := ReconciliationIntent{
		Action: ReconcileRepairProjection, HandoffID: "handoff-1",
		Generation: &generation, Reason: "the stable entrypoint is drift while main-x is durably active",
	}
	service := &scriptedService{}
	result := ExecuteReconciliation(service, intent, executedAt())

	if got := strings.Join(service.calls, " "); got != "recover:handoff-1" {
		t.Fatalf("calls = %q: repairing a pointer must not open service", got)
	}
	if !result.Converged || result.Projection.Outcome != StepSucceeded {
		t.Fatalf("the repair did not converge: %s", result.Summary())
	}
	if result.WorkAdmission.Outcome != StepNotAttempted {
		t.Fatalf("admission = %q, want not attempted", result.WorkAdmission.Outcome)
	}

	// And a repair that cannot be performed is reported without escalation.
	failing := &scriptedService{recoverErr: func() error {
		return &ProjectionRepairFailedError{HandoffID: "handoff-1", Cause: fmt.Errorf("a directory occupies the entrypoint")}
	}}
	drifted := ExecuteReconciliation(failing, intent, executedAt())
	if drifted.Projection.Outcome != StepDrifted || drifted.Converged {
		t.Fatalf("a failed repair was not reported as unconverged drift: %s", drifted.Summary())
	}
}

// NON-MUTATING INTENTS MUTATE NOTHING, including an action this build does not
// recognise. The executor is as fail-closed about its own vocabulary as the
// classifier is about the runtime's.
func TestNonMutatingIntentsCallNothing(t *testing.T) {
	for _, action := range []ReconciliationAction{
		ReconcileNone, ReconcileUnknown, ReconcileRefuse, ReconciliationAction("rebalance_quorum"),
	} {
		t.Run(string(action), func(t *testing.T) {
			service := &scriptedService{}
			result := ExecuteReconciliation(service, ReconciliationIntent{
				Action: action, HandoffID: "handoff-1", Reason: "fixture",
			}, executedAt())
			if len(service.calls) != 0 {
				t.Fatalf("%q called %v", action, service.calls)
			}
			for name, step := range map[string]ReconciliationStep{
				"recovery": result.Recovery, "projection": result.Projection, "admission": result.WorkAdmission,
			} {
				if step.Outcome != StepNotAttempted {
					t.Fatalf("%q attempted %s", action, name)
				}
			}
			if result.Converged != (action == ReconcileNone) {
				t.Fatalf("%q reported converged=%v", action, result.Converged)
			}
		})
	}
}

// END TO END against the real service: classify what the runtime reports, then
// execute it. This is the only test here that uses no fake, and it proves the
// halves fit.
func TestClassifyThenExecuteAgainstTheRealService(t *testing.T) {
	fixture := activatedFixture(t)
	self := fixture.self

	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return fixture.service.DescribeLiveController(fixture.record.ID, executedAt()), nil
	}, executedAt())
	if err != nil {
		t.Fatal(err)
	}
	intent := ClassifyReconciliation(WatcherObservation{Status: status, Self: self})
	if intent.Action != ReconcileResumeActivatedService {
		t.Fatalf("action = %q, want resume: the controller is activated and not serving (%s)", intent.Action, intent.Reason)
	}
	result := ExecuteReconciliation(fixture.service, intent, executedAt())
	if !result.Converged {
		t.Fatalf("the real service did not converge: %s", result.Summary())
	}
	if !fixture.service.AdmittingWork() {
		t.Fatal("the controller is not admitting work after a converged reconciliation")
	}

	// AND IT SETTLES. Classifying again yields no action, which is what stops
	// a watcher from re-firing the same intent forever - the defect the intent
	// name was changed to prevent.
	settled, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return fixture.service.DescribeLiveController(fixture.record.ID, executedAt()), nil
	}, executedAt())
	if err != nil {
		t.Fatal(err)
	}
	if again := ClassifyReconciliation(WatcherObservation{Status: settled, Self: self}); again.Action != ReconcileNone {
		t.Fatalf("a converged controller still asks for %q: %s", again.Action, again.Reason)
	}
}
