package runtime

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestPublicationHumanGateRequiresTheFinalCandidateDecision(t *testing.T) {
	action := domain.Action{Type: PublicationActionType, Target: "main"}
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "producer-a", Kind: domain.StageAgent, Role: domain.RoleImplementer, InvocationMode: domain.InvocationModeMutating},
		{ID: "producer-b", Kind: domain.StageAgent, Role: domain.RoleImplementer, InvocationMode: domain.InvocationModeMutating},
		{ID: "integrate", Kind: domain.StageAgent, Role: domain.RoleIntegrator, InvocationMode: domain.InvocationModeMutating,
			ExecutionKind: "integration_compose", DependsOn: []string{"producer-a", "producer-b"}},
		{ID: "review", Kind: domain.StageAgent, Role: domain.RoleReviewer, InvocationMode: domain.InvocationModeMutating, DependsOn: []string{"integrate"}},
		{ID: "verify", Kind: domain.StageAssuranceGate, DependsOn: []string{"review"}},
		{ID: "approve", Kind: domain.StageHumanDecisionGate, Action: &action,
			RequiredClaims: []string{"claim-publication-approval"}, DependsOn: []string{"verify"}},
	})
	plan := fixture.plan
	producerA := appendPublicationCandidate(t, fixture, "producer-a-run", strings.Repeat("a", 40), true)
	producerB := appendPublicationCandidate(t, fixture, "producer-b-run", strings.Repeat("b", 40), true)
	integrated := appendPublicationCandidate(t, fixture, "integrated-run", strings.Repeat("c", 40), false)
	snapshot := PlanSnapshot{Stages: map[string]PlanStageProjection{
		"producer-a": {RunID: producerA}, "producer-b": {RunID: producerB}, "integrate": {RunID: integrated},
	}}
	stage, _ := plan.Stage("approve")

	if _, satisfied, err := fixture.reconciler.gateSatisfaction(stage, plan, snapshot); err != nil || satisfied {
		t.Fatalf("producer approvals satisfied the final-candidate gate: satisfied=%v err=%v", satisfied, err)
	}

	appendPublicationDecision(t, fixture, integrated, "wrong-contract-approval", strings.Repeat("c", 40), "approve", Ref{ID: "other-contract", Revision: "1"}, action)
	appendPublicationDecision(t, fixture, integrated, "wrong-action-approval", strings.Repeat("c", 40), "approve", Ref{ID: "contract", Revision: "1"},
		domain.Action{Type: PublicationActionType, Target: "other"})
	if _, satisfied, err := fixture.reconciler.gateSatisfaction(stage, plan, snapshot); err != nil || satisfied {
		t.Fatalf("approval for another contract or publication target satisfied the gate: satisfied=%v err=%v", satisfied, err)
	}

	appendPublicationDecision(t, fixture, integrated, "integrated-approval", strings.Repeat("c", 40), "approve", Ref{ID: "contract", Revision: "1"}, action)
	payload, satisfied, err := fixture.reconciler.gateSatisfaction(stage, plan, snapshot)
	if err != nil || !satisfied {
		t.Fatalf("approval of the exact integrated candidate did not satisfy the gate: satisfied=%v err=%v", satisfied, err)
	}
	if len(payload.ProvingRuns) != 1 || payload.ProvingRuns[0] != integrated {
		t.Fatalf("publication gate cites non-final candidate runs: %+v", payload.ProvingRuns)
	}

	// A newer integrated commit invalidates the earlier approval even when the
	// producers still carry matching approvals.
	recordCandidate(t, fixture, integrated, strings.Repeat("d", 40))
	if _, satisfied, err := fixture.reconciler.gateSatisfaction(stage, plan, snapshot); err != nil || satisfied {
		t.Fatalf("approval of a superseded integrated head satisfied the gate: satisfied=%v err=%v", satisfied, err)
	}
}

func TestPublicationGateWaitsWhenTheFinalCandidateIsAmbiguous(t *testing.T) {
	action := domain.Action{Type: PublicationActionType, Target: "main"}
	fixture := newPlanRunFixture(t, []domain.PlanStage{
		{ID: "producer-a", Kind: domain.StageAgent, Role: domain.RoleImplementer, InvocationMode: domain.InvocationModeMutating},
		{ID: "producer-b", Kind: domain.StageAgent, Role: domain.RoleImplementer, InvocationMode: domain.InvocationModeMutating},
		{ID: "approve", Kind: domain.StageHumanDecisionGate, Action: &action,
			RequiredClaims: []string{"claim-publication-approval"}, DependsOn: []string{"producer-a", "producer-b"}},
	})
	plan := fixture.plan
	producerA := appendPublicationCandidate(t, fixture, "ambiguous-producer-a", strings.Repeat("a", 40), true)
	producerB := appendPublicationCandidate(t, fixture, "ambiguous-producer-b", strings.Repeat("b", 40), true)
	snapshot := PlanSnapshot{Stages: map[string]PlanStageProjection{
		"producer-a": {RunID: producerA}, "producer-b": {RunID: producerB},
	}}
	stage, _ := plan.Stage("approve")
	if _, satisfied, err := fixture.reconciler.gateSatisfaction(stage, plan, snapshot); err != nil || satisfied {
		t.Fatalf("ambiguous producer candidates satisfied publication: satisfied=%v err=%v", satisfied, err)
	}
}

func appendPublicationCandidate(t *testing.T, fixture *planRunFixture, runID, head string, approve bool) string {
	t.Helper()
	run := newJournalRun(runID)
	run.Repository = fixture.plan.Subject.Repository
	run.Contract = Ref{ID: "contract", Revision: "1"}
	run.Candidate = Candidate{Branch: candidateBranch(runID), Revision: head, Tree: strings.Repeat("e", 40)}
	if err := fixture.store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	appendRunEventFor(t, fixture, runID, head, EventCandidateCommitted, CandidateCommittedPayload{
		Commit: head, Tree: run.Candidate.Tree, PathCount: 1, PathsDigest: strings.Repeat("f", 64),
	})
	if approve {
		appendPublicationDecision(t, fixture, runID, "producer-approval-"+runID, head, "approve", run.Contract,
			domain.Action{Type: PublicationActionType, Target: "main"})
	}
	return runID
}

func appendPublicationDecision(t *testing.T, fixture *planRunFixture, runID, eventID, head, decision string, contract Ref, action domain.Action) {
	t.Helper()
	state, found, err := fixture.store.Run(runID)
	if err != nil || !found || state.Candidate.Revision != head {
		t.Fatalf("load publication candidate run %s: %+v, found=%v err=%v", runID, state, found, err)
	}
	appendRunEventFor(t, fixture, runID, head, EventHumanAuthorityRecorded, humanAuthorityFixture(map[string]any{
		"evidence_id": eventID, "decision": decision, "action": map[string]any{"type": action.Type, "target": action.Target},
		"candidate": map[string]any{"branch": state.Candidate.Branch, "revision": state.Candidate.Revision, "tree": state.Candidate.Tree},
		"contract":  map[string]any{"id": contract.ID, "revision": contract.Revision},
		"requires":  []any{"claim-publication-approval"},
	}))
}

func appendCurrentHumanDecision(t *testing.T, fixture *planRunFixture, runID, eventID, decision string, claims []string) {
	t.Helper()
	run, found, err := fixture.store.Run(runID)
	if err != nil || !found {
		t.Fatalf("load run %s for human decision: found=%v err=%v", runID, found, err)
	}
	if run.Contract == (Ref{}) {
		run.Contract = Ref{ID: "contract", Revision: "1"}
		if err := fixture.store.PutRun(run); err != nil {
			t.Fatal(err)
		}
	}
	projection, err := fixture.reconciler.runProjection(runID)
	if err != nil {
		t.Fatal(err)
	}
	contract := projection.Contract
	if contract == (Ref{}) {
		contract = run.Contract
	}
	requires := make([]any, len(claims))
	for i, claim := range claims {
		requires[i] = claim
	}
	appendRunEventFor(t, fixture, runID, projection.Head(), EventHumanAuthorityRecorded, humanAuthorityFixture(map[string]any{
		"evidence_id": eventID, "decision": decision,
		"candidate": map[string]any{"branch": candidateBranch(runID), "revision": projection.CandidateRevision, "tree": projection.CandidateTree},
		"contract":  map[string]any{"id": contract.ID, "revision": contract.Revision}, "requires": requires,
	}))
}
