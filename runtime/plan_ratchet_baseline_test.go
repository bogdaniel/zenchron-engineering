package runtime

// A rejected plan revision must not become the monotonicity floor for later
// proposals (#370, dogfood evidence from #64 plan dogfood / #343).
//
// The privilege ratchet in planning.revisionPrivilegeViolations exists to stop
// a revision from silently WEAKENING an obligation it already carried. Before
// this file, `Propose` fed it the latest STORED revision - approved, rejected
// or still pending, whichever was newest - so an LLM-proposed independence
// requirement the operator never approved, and then rejected, became a floor
// every later proposal for the same plan was held to. Rejecting a proposal
// must never be how an obligation quietly disappears, and it must equally
// never be how one is quietly MANUFACTURED: a rejected revision is immutable
// history, not authority in either direction.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ratchetTestContract is a minimal compiled contract for these tests. When
// requirements is nil, it states no plan-shaped obligation at all, so the
// only thing that could refuse a dropped independence requirement is the
// ratchet under test. When requirements is set, it is the contract's OWN
// obligation, which the compiler enforces on every compilation regardless of
// revision history.
func ratchetTestContract(base *phase8Fixture, requirements *domain.PlanRequirements) domain.EngineeringWorkContract {
	return domain.EngineeringWorkContract{
		SchemaVersion:    domain.SchemaVersion,
		ID:               "contract",
		Revision:         "1",
		Objective:        "Make the widget idempotent.",
		AcceptanceIntent: []string{"The widget is idempotent."},
		Subject:          domain.Subject{Repository: "acme/repo", Revision: base.base},
		Scope: domain.ContractScope{
			Stage: domain.StageObserved, AllowedPaths: []string{"."}, ProhibitedPaths: []string{},
		},
		Facts:               []string{},
		Invariants:          map[string]domain.Requirement{},
		Obligations:         map[string]domain.Requirement{},
		RequiredClaims:      map[string]domain.RequiredClaim{},
		PlanRequirements:    requirements,
		Permissions:         []domain.Action{},
		Prohibitions:        []domain.Action{},
		AuthorityConditions: []domain.AuthorityCondition{},
		Provenance: domain.ContractProvenance{
			ProjectModel:    domain.ObjectRevision{ID: "project", Revision: "1"},
			Policy:          domain.ObjectRevision{ID: "policy", Revision: "1"},
			CompilerVersion: "compiler-v0.1",
		},
	}
}

func ratchetProposeInput(base *phase8Fixture, planID string, contract domain.EngineeringWorkContract, reasoned []domain.PlanStage) ProposeInput {
	return ProposeInput{
		PlanID: planID, Objective: "Make the widget idempotent.",
		Subject:  domain.Subject{Repository: "acme/repo", Revision: base.base},
		Contract: contract, Issue: base.issue, Reasoned: reasoned,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1",
			Subject: domain.Subject{Repository: "acme/repo", Revision: base.base},
		},
	}
}

func withReviewer() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating},
		{ID: "code-review", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			DependsOn: []string{"implementation"}, Objective: "Review the change.",
			InvocationMode: domain.InvocationModeMutating,
			Independence: &domain.IndependenceRequirement{
				Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"implementation"},
			}},
	}
}

func withoutReviewer() []domain.PlanStage {
	return []domain.PlanStage{
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "Do the work.", InvocationMode: domain.InvocationModeMutating},
	}
}

// The restored-defect shape: revision 1 proposes an independent reviewer that
// nothing but the LLM's own proposal asked for, the operator REJECTS it for a
// reason unrelated to the reviewer, and the next proposal omits it. That must
// not be refused: the rejected revision granted no privilege, so there is
// nothing for the next proposal to have weakened.
func TestARejectedProposalGrantsNoRatchetFloor(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	contract := ratchetTestContract(fixture.phase8Fixture, nil)
	planID := "plan-rejected-floor"

	first, err := fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withReviewer()))
	if err != nil {
		t.Fatalf("the first proposal was refused: %v", err)
	}
	if _, err := fixture.service.Reject(planID, first.Revision, first.Digest, "", "operator", "not enough child-run budget for two stages"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	second, err := fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withoutReviewer()))
	if err != nil {
		t.Fatalf("a proposal following a REJECTED revision was held to that revision's independence obligation: %v", err)
	}
	if len(second.Stages) != 1 || second.Stages[0].ID != "implementation" {
		t.Fatalf("unexpected stages on the accepted revision: %#v", second.Stages)
	}
}

// The approved control: the same removal, but revision 1 was APPROVED rather
// than rejected. An approved revision IS the privilege floor, and dropping
// its independence obligation is refused exactly as before.
func TestAnApprovedRevisionStillRatchets(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	contract := ratchetTestContract(fixture.phase8Fixture, nil)
	planID := "plan-approved-floor"

	first, err := fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withReviewer()))
	if err != nil {
		t.Fatalf("the first proposal was refused: %v", err)
	}
	if _, err := fixture.service.Approve(planID, first.Revision, first.Digest,
		shownAssignments(t, fixture.service, planID, first.Revision), "operator", "looks right"); err != nil {
		t.Fatalf("approve: %v", err)
	}

	_, err = fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withoutReviewer()))
	var refused *PlanAttemptRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("dropping an independence obligation carried by the APPROVED revision was accepted: %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be dropped by renaming") {
		t.Fatalf("the refusal does not name the dropped obligation: %v", err)
	}
}

// The policy control: an independence requirement that comes from the work
// contract is enforced on every compilation, whether or not any revision has
// ever been approved or rejected. This is the compiler's own obligation
// completion (planning.applyPolicyObligations), not the revision ratchet, and
// rejecting a proposal must not be a way to route around it.
func TestAPolicyIndependenceObligationSurvivesARejection(t *testing.T) {
	fixture := newPlanRunFixture(t, parallelStages())
	requirements := domain.PlanRequirements{Roles: []domain.RoleRequirement{{
		Role:      domain.RoleReviewer,
		Statement: "A reviewer independent of the execution agent is required.",
		Independence: &domain.IndependenceRequirement{
			Dimension: domain.IndependenceExecutionAgent,
		},
	}}}
	contract := ratchetTestContract(fixture.phase8Fixture, &requirements)
	planID := "plan-policy-floor"

	requireReviewerIndependence := func(plan domain.EngineeringPlan) {
		t.Helper()
		reviewer, found := plan.Stage("reviewer")
		if !found || reviewer.Independence == nil || reviewer.Independence.Dimension != domain.IndependenceExecutionAgent {
			t.Fatalf("the compiled plan does not carry the policy-required independent reviewer: %#v", plan.Stages)
		}
	}

	// The proposal omits the reviewer entirely; the compiler completes it from
	// the contract regardless.
	first, err := fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withoutReviewer()))
	if err != nil {
		t.Fatalf("the first proposal was refused: %v", err)
	}
	requireReviewerIndependence(first)

	if _, err := fixture.service.Reject(planID, first.Revision, first.Digest, "", "operator", "not this one"); err != nil {
		t.Fatalf("reject: %v", err)
	}

	// Rejected, then proposed again with the SAME omission: the obligation is
	// completed again, from the contract, exactly as before. Nothing about the
	// rejection weakened it and nothing about it needed to.
	second, err := fixture.service.Propose(context.Background(), ratchetProposeInput(fixture.phase8Fixture, planID, contract, withoutReviewer()))
	if err != nil {
		t.Fatalf("the second proposal was refused: %v", err)
	}
	requireReviewerIndependence(second)
}
