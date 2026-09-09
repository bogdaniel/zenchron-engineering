package runtime

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A decision that lands while the planner runs ends the pass.
//
// The plan lock is released across a planning invocation - deliberately, so one
// plan's provider call does not stall the fleet - and that invocation takes
// minutes. The pass holds a decoded plan document from before the release. If
// an operator approves a different revision in that window, every remaining
// step of the pass is about a document nobody is executing: the proposal would
// name the superseded revision as its source, the stage would be settled
// "completed" under it, and a run started from it would be created AFTER the
// supersession computed its invalidations - so it would never be retired, never
// stopped, and would do work the approved plan no longer contains.
func TestADecisionDuringAPlanningInvocationEndsThePass(t *testing.T) {
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "decomposition", Kind: domain.StageAgent, Role: domain.RolePlanner,
			Objective:            "Decide how this should be split.",
			InvocationMode:       domain.InvocationModeNonMutatingPlanning,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis, domain.CapabilityRequirementsAnalysis}},
		{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			DependsOn: []string{"decomposition"}, Objective: "Do the work.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
	})
	fixture.approve(t)

	fixture.reconciler.Planner = func(context.Context, PlanDecompositionRequest) (PlannerOutput, error) {
		// The operator approves a second revision while the provider is
		// running. This is the whole window.
		next := fixture.plan
		next.Revision = 2
		next.Objective = "Make the widget idempotent, in two halves."
		digest, err := next.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		next.Digest = digest
		if _, err := fixture.store.PutPlanRevision(next); err != nil {
			t.Fatal(err)
		}
		// The contract this revision was planned against. Approval binds the
		// assignments it resolves, and resolution reads the obligations from
		// here - so a revision stored without one is a state the propose path
		// never produces.
		if err := fixture.store.PutPlanContract(next.ID, next.Revision, planFixtureContract(fixture.phase8Fixture)); err != nil {
			t.Fatal(err)
		}
		if _, err := fixture.service.Approve(next.ID, next.Revision, next.Digest, "operator", "the two-half split"); err != nil {
			t.Fatal(err)
		}
		return PlannerOutput{
			Stages: []domain.PlanStage{
				{ID: "decomposition", Kind: domain.StageAgent, Role: domain.RolePlanner,
					InvocationMode: domain.InvocationModeNonMutatingPlanning},
				{ID: "backend", Kind: domain.StageAgent, Role: domain.RoleImplementer, DependsOn: []string{"decomposition"}},
			},
			Notes: "A proposal compiled against revision 1, produced after revision 2 was approved.",
			Reasoning: domain.PlanReasoningProvenance{
				AgentID: "claude", ProviderKind: "claude_code", VendorFamily: "anthropic",
				TrustMode:      domain.TrustRequirementOperatorTrusted,
				InvocationMode: domain.InvocationModeNonMutatingPlanning, ProviderMode: "plan",
				WorkspaceDigestBefore: strings.Repeat("a", 64), WorkspaceDigestAfter: strings.Repeat("a", 64),
				WorkspaceUnchanged: true,
			},
		}, nil
	}

	report := fixture.reconcile(t)
	if !strings.Contains(report.Waiting, "approved during this pass") {
		t.Fatalf("the pass did not stop: %#v", report)
	}
	if len(report.Started) != 0 {
		t.Fatalf("a stage was started from the superseded document: %#v", report.Started)
	}

	// The proposal compiled against the superseded revision was not recorded.
	proposals, err := fixture.store.PlanProposals(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 0 {
		t.Fatalf("a proposal against revision %d was stored after revision 2 was approved: %#v", fixture.plan.Revision, proposals)
	}

	// And the stage was not settled "completed" under a revision that is no
	// longer the plan.
	snapshot, err := fixture.store.ReplayPlan(fixture.plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Stages["decomposition"].State; state == "completed" {
		t.Fatalf("the decomposition stage was settled completed under the superseded revision")
	}
	runs, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("%d runs were created from the superseded document", len(runs))
	}
	// The invocation itself stays spent: it happened.
	if snapshot.Consumed.ProviderInvocations != 1 {
		t.Fatalf("the invocation that ran was recorded as %d", snapshot.Consumed.ProviderInvocations)
	}

}
