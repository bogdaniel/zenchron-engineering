package runtime

// Regression proofs for Defect N and the fulfillability audit (Defect O), from
// the real dogfood run run-0943e257539346f8763db04505cbf322.
//
// That run did everything right and then concluded nothing was left to do:
// OpenAI execution succeeded and mutated 3 paths, execution.completed, the
// runtime-owned commit landed, #8 reassessed it (material_scope_change,
// verification_surface_changed, requested_privilege_count 0), baseline-go
// exact-tree assurance PASSED, base.integrate passed on attempt 1 with no
// drift, authority.evaluate ran - and returned INCOMPLETE. The reconciler knew
// only AWAITING_AUTHORITY and BLOCKED, so INCOMPLETE matched nothing, no
// operation was wanted, and the run settled waiting/goal_state_reached over an
// unauthorized protected action.
//
// Underneath it: the claim gating publication asks for evidence class
// "test_result" and the only configured producer declares "automated_test", so
// the contract was never fulfillable. That is proven here from the same
// vocabulary the runtime uses, not asserted.
//
// Nothing here makes a network or provider call.

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// N: one authoritative status mapping
// ---------------------------------------------------------------------------

// TestAuthorityStatusMatrixIsTotalAndOutstandingBlocksGoalState pins the whole
// matrix in one place, including the entries that previously matched nothing.
func TestAuthorityStatusMatrixIsTotalAndOutstandingBlocksGoalState(t *testing.T) {
	for name, tc := range map[string]struct {
		status      domain.AuthorityStatus
		disposition Disposition
		reason      string
		outstanding bool
	}{
		"authorized names no outstanding condition": {
			status: domain.AuthorityAuthorized, disposition: Active, reason: "", outstanding: false,
		},
		"awaiting authority waits for a human": {
			status: domain.AuthorityAwaitingAuthority, disposition: Waiting, reason: "awaiting_authority", outstanding: true,
		},
		"blocked stops precisely": {
			status: domain.AuthorityBlocked, disposition: Waiting, reason: "authority_blocked", outstanding: true,
		},
		"incomplete requires evidence, never a goal": {
			status: domain.AuthorityIncomplete, disposition: Active, reason: "evidence_required", outstanding: true,
		},
		"stale requires fresh applicable evidence": {
			status: domain.AuthorityStale, disposition: Active, reason: "fresh_evidence_required", outstanding: true,
		},
		"an unrecognized status fails closed": {
			status: domain.AuthorityStatus("not-a-status"), disposition: Waiting, reason: "authority_unknown", outstanding: true,
		},
		"an empty status fails closed": {
			status: domain.AuthorityStatus(""), disposition: Waiting, reason: "authority_unknown", outstanding: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			disposition, reason, outstanding := AuthorityDisposition(tc.status)
			if disposition != tc.disposition || reason != tc.reason || outstanding != tc.outstanding {
				t.Fatalf("mapping = %q/%q/%t, want %q/%q/%t", disposition, reason, outstanding, tc.disposition, tc.reason, tc.outstanding)
			}
			// The invariant, stated directly: a non-authorized status always
			// names an outstanding condition, and a named condition is what the
			// settle path prefers over goal_state_reached.
			if tc.status != domain.AuthorityAuthorized {
				if !outstanding || reason == "" {
					t.Fatalf("%q leaves goal_state_reached reachable", tc.status)
				}
				if waitingReason(reason, "goal_state_reached") == "goal_state_reached" {
					t.Fatalf("%q settles as goal_state_reached", tc.status)
				}
			}
		})
	}
}

// TestKernelAndReconcilerShareOneStatusMapping proves the duplication is gone:
// the kernel's NextDisposition is the same mapping, not a second opinion.
func TestKernelAndReconcilerShareOneStatusMapping(t *testing.T) {
	for _, status := range []domain.AuthorityStatus{
		domain.AuthorityAuthorized, domain.AuthorityAwaitingAuthority, domain.AuthorityBlocked,
		domain.AuthorityIncomplete, domain.AuthorityStale, domain.AuthorityStatus("unknown"),
	} {
		disposition, reason := NextDisposition(KernelState{Decision: domain.AuthorityDecision{Status: status}})
		mapped, mappedReason, outstanding := AuthorityDisposition(status)
		if !outstanding {
			// Authorized: the kernel names it, the reconciler stays silent so a
			// finished run keeps its own terminal reason.
			if disposition != Active || reason != "authorized" || mappedReason != "" {
				t.Fatalf("authorized disagrees: kernel %q/%q, mapping %q/%q", disposition, reason, mapped, mappedReason)
			}
			continue
		}
		if disposition != mapped || reason != mappedReason {
			t.Fatalf("%q disagrees: kernel %q/%q, mapping %q/%q", status, disposition, reason, mapped, mappedReason)
		}
	}
}

// TestIncompleteAuthorityNeverReachesGoalState is the end-to-end proof against
// the reconciler, in the exact shape the run was in: a current, unanswered,
// non-authorized decision for the protected action.
func TestIncompleteAuthorityNeverReachesGoalState(t *testing.T) {
	for _, status := range []domain.AuthorityStatus{
		domain.AuthorityIncomplete, domain.AuthorityStale,
		domain.AuthorityAwaitingAuthority, domain.AuthorityBlocked,
		domain.AuthorityStatus("some-future-status"),
	} {
		t.Run(string(status), func(t *testing.T) {
			fixture := newPhase8Fixture(t)
			runID := fixture.start()
			fixture.reconcile(runID)
			state := fixture.state(runID)

			decision, ok := state.decidedPublication()
			if !ok {
				t.Skip("this fixture did not reach an authority decision")
			}
			decision.Status = status
			state.projection.AuthorityDecisions[decision.Action.Type+"\x00"+decision.Action.Target] = decision

			live, reason := state.conditions()
			if reason == "" {
				t.Fatalf("%q produced no condition, so nothing stops goal_state_reached", status)
			}
			if waitingReason(reason, "goal_state_reached") == "goal_state_reached" {
				t.Fatalf("%q settles as goal_state_reached", status)
			}
			expected, expectedReason, _ := AuthorityDisposition(status)
			if live != expected || reason != expectedReason {
				t.Fatalf("%q produced %q/%q, want %q/%q", status, live, reason, expected, expectedReason)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// O: producer capability and fulfillability
// ---------------------------------------------------------------------------

// TestProducibleEvidenceClassesAreDeclaredNeverAssumed proves capability is
// stated: a provider that declares nothing contributes nothing.
func TestProducibleEvidenceClassesAreDeclaredNeverAssumed(t *testing.T) {
	silent := ProducibleEvidenceClasses(silentAssuranceProvider{})
	if silent[AssuranceEvidenceClass] {
		t.Fatal("a provider that declares nothing was credited with producing automated_test")
	}
	if !silent[HumanEvidenceClass] {
		t.Fatal("human approval is obtained through the operator boundary and must always be producible")
	}
	declared := ProducibleEvidenceClasses(BaselineGoVerifier{})
	if !declared[AssuranceEvidenceClass] {
		t.Fatalf("the baseline verifier does not declare %q: %v", AssuranceEvidenceClass, declared)
	}
	// It declares ONLY that. Running a test suite is not a security review.
	for _, other := range []domain.EvidenceClass{"security_review", "test_result", "external_audit"} {
		if declared[other] {
			t.Fatalf("the baseline verifier claims to produce %q", other)
		}
	}
}

// silentAssuranceProvider declares no capability at all. It exists to prove the
// model fails closed: capability is stated, never inferred from being wired in.
type silentAssuranceProvider struct{}

func (silentAssuranceProvider) Assure(context.Context, AssuranceRequest) (AssuranceResult, error) {
	return AssuranceResult{}, nil
}

// TestUnfulfillableEvidenceIsExactAndOrdered is the vocabulary check itself.
func TestUnfulfillableEvidenceIsExactAndOrdered(t *testing.T) {
	action := domain.Action{Type: PublicationActionType, Target: "main"}
	contract := domain.EngineeringWorkContract{
		RequiredClaims: map[string]domain.RequiredClaim{
			"claim-machine": {EvidenceClass: AssuranceEvidenceClass},
			"claim-policy":  {EvidenceClass: "test_result"},
			"claim-human":   {EvidenceClass: HumanEvidenceClass},
			"claim-audit":   {EvidenceClass: "external_audit"},
		},
		AuthorityConditions: []domain.AuthorityCondition{
			{Action: action, RequiredClaims: []string{"claim-policy", "claim-machine", "claim-human", "claim-audit"}},
			{Action: domain.Action{Type: "git.merge", Target: "main"}, RequiredClaims: []string{"claim-audit"}},
		},
	}
	producible := ProducibleEvidenceClasses(BaselineGoVerifier{})
	unsupported := UnfulfillableEvidence(contract, action, producible)
	if len(unsupported) != 2 {
		t.Fatalf("unsupported = %+v, want exactly the two classes nothing produces", unsupported)
	}
	if unsupported[0].ClaimID != "claim-audit" || unsupported[1].ClaimID != "claim-policy" {
		t.Fatalf("unsupported is not deterministically ordered: %+v", unsupported)
	}
	if unsupported[1].EvidenceClass != "test_result" {
		t.Fatalf("the unsupported class is not named: %+v", unsupported[1])
	}
	// A contract whose gate is entirely producible reports nothing, and a
	// condition for a DIFFERENT action is not this action's problem.
	fulfillable := contract
	fulfillable.AuthorityConditions = []domain.AuthorityCondition{
		{Action: action, RequiredClaims: []string{"claim-machine", "claim-human"}},
		{Action: domain.Action{Type: "git.merge", Target: "main"}, RequiredClaims: []string{"claim-audit"}},
	}
	if got := UnfulfillableEvidence(fulfillable, action, producible); len(got) != 0 {
		t.Fatalf("a fulfillable gate reported %+v", got)
	}
}

// TestUnfulfillableContractIsRefusedBeforeAnyModelBudget is the rule the run
// broke: the refusal happens at contract.compile, before a candidate workspace
// exists and before any provider is asked to do anything.
func TestUnfulfillableContractIsRefusedBeforeAnyModelBudget(t *testing.T) {
	fixture := newPhase8Fixture(t)
	// The governance artifact asks for a class the configured verifier does not
	// declare - exactly the real dogfood policy's "test_result" against
	// baseline-go's "automated_test" - and it gates the run's actual
	// publication action.
	model, _ := phase8Governance("acme/repo", fixture.base)
	action := domain.Action{Type: PublicationActionType, Target: fixture.branch}
	claims := map[string]domain.RequiredClaim{
		"claim-policy-vocabulary": {EvidenceClass: "test_result", IndependentFromChangeProducer: true},
	}
	permissions := []domain.Action{action}
	conditions := []domain.AuthorityCondition{{Action: action, RequiredClaims: []string{"claim-policy-vocabulary"}}}
	effect := domain.PolicyEffect{RequiredClaims: &claims, Permissions: &permissions, AuthorityConditions: &conditions}
	rules := map[string]domain.PolicyRule{}
	for name, value := range map[string]domain.FactValue{
		"service-unknown": domain.FactUnknown,
		"service-clear":   domain.FactFalse,
		"service-touched": domain.FactTrue,
	} {
		rules[name] = domain.PolicyRule{
			When:   domain.PolicyCondition{Fact: "service.boundary_modified", Equals: value},
			Effect: effect,
		}
	}
	fixture.deps.ProjectModel = model
	fixture.deps.Policy = domain.EngineeringPolicy{
		SchemaVersion: domain.SchemaVersion, ID: "policy-unfulfillable", Revision: "1", Rules: rules,
	}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()

	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Failed {
		t.Fatalf("an unfulfillable contract did not stop the run: %+v", outcome)
	}
	events := journalOf(t, fixture.runtime, runID)
	// Nothing expensive happened: no candidate workspace, no provider call.
	if countKindAttempts(events, OpExecutionInvoke) != 0 {
		t.Fatalf("a model budget was spent on an unfulfillable contract: %v", journalTypes(events))
	}
	if countKindAttempts(events, OpCandidateCreate) != 0 {
		t.Fatalf("a candidate workspace was created for an unfulfillable contract: %v", journalTypes(events))
	}
	if len(fixture.provider.requests) != 0 {
		t.Fatalf("the execution provider was invoked %d times", len(fixture.provider.requests))
	}
	// And the refusal names the exact requirement.
	if RouteFailure(FailureRequiredEvidenceUnsupported) != RouteStop {
		t.Fatalf("an unfulfillable contract routes to %q", RouteFailure(FailureRequiredEvidenceUnsupported))
	}
	found := false
	for _, e := range events {
		if e.Type != EventOperationAfter {
			continue
		}
		var op RunOperation
		if json.Unmarshal(e.Payload, &op) != nil || op.Kind != OpContractCompile || len(op.Result) == 0 {
			continue
		}
		var result contractCompileResult
		if json.Unmarshal(op.Result, &result) == nil && result.FailureClass == FailureRequiredEvidenceUnsupported {
			found = true
			if len(result.Unsupported) == 0 || result.Unsupported[0].EvidenceClass != "test_result" {
				t.Fatalf("the refusal does not name the unsupported class: %+v", result.Unsupported)
			}
		}
	}
	if !found {
		t.Fatalf("no typed unfulfillable-contract result was journalled: %v", journalTypes(events))
	}
}

// TestFulfillableContractStillCompiles keeps the check narrow: a contract whose
// gate the configured producer can answer is untouched.
func TestFulfillableContractStillCompiles(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)
	state := fixture.state(runID)
	if state.projection.Contract == (Ref{}) {
		t.Fatalf("a fulfillable contract was refused: %+v", state.snapshot)
	}
	if state.snapshot.Reason == "contract.compile_failure_not_retryable" {
		t.Fatal("a fulfillable contract was refused as unfulfillable")
	}
}
