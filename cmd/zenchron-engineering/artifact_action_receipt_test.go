package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/product"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func newArtifactReceiptComposition(t *testing.T) *composition {
	t.Helper()
	dir := t.TempDir()
	store, err := runtime.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	agents, err := (runtime.OperatorConfig{
		Agents:       map[string]runtime.AgentConfig{"claude": {Kind: runtime.AgentKindClaudeCode, TrustMode: string(runtime.TrustOperatorTrusted)}},
		DefaultAgent: "claude",
	}).AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return &composition{
		store: store, agents: agents,
		config: runtime.Config{OperatorConfig: runtime.OperatorConfig{PlanningDir: t.TempDir()}},
	}
}

func newTestSupervisor(t *testing.T, c *composition) *runtime.Supervisor {
	t.Helper()
	repo, err := runtime.ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := runtime.NewSupervisor(runtime.SupervisorDependencies{
		Store: c.store, Clock: runtime.RealClock{}, Owner: "owner-1",
		Repositories: []runtime.GitHubRepo{repo}, Agents: c.agents,
		Runtime: func(runtime.GitHubRepo, runtime.ResolvedAgent) (*runtime.EngineeringRuntime, error) {
			t.Fatal("no run should be driven by this test")
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

// TestDispatchArtifactWithReceiptRecoversAPlanningArtifactWrite proves the
// unknown-outcome contract for planning-artifact-propose: a resend with the
// SAME operation id and request digest - simulating a lost response - is
// refused rather than re-executed, and the durable receipt it left behind
// identifies exactly the artifact the FIRST attempt wrote.
func TestDispatchArtifactWithReceiptRecoversAPlanningArtifactWrite(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	supervisor := newTestSupervisor(t, c)
	ctx := context.Background()
	noShutdown := func() {}
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "1111111111111111111111111111111111111111111111111111111111111111"

	request := runtime.ControlRequest{
		Command: runtime.ControlPlanningArtifactPropose, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding",
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
			"execution_agent": "claude",
			"capabilities": ["repository_analysis"],
			"trust_requirement": "operator_trusted"
		}`)},
	}
	first := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, request)
	if !first.OK {
		t.Fatalf("the first attempt must succeed: %+v", first)
	}

	// The response is lost; the client resends the SAME operation reference,
	// but with a DIFFERENT document (a plausible retry after re-reading
	// stale form state) - a write this idempotent-by-content would silently
	// succeed and overwrite with the new content if the guard were skipped,
	// so this is the resend shape that actually exercises it.
	escalatedRequest := request
	escalatedRequest.PlanningArtifact = &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
		"execution_agent": "claude",
		"capabilities": ["repository_analysis", "security_review"],
		"trust_requirement": "operator_trusted"
	}`)}
	resend := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, escalatedRequest)
	if resend.OK || resend.Code != runtime.ControlCodeOutcomeUnknown {
		t.Fatalf("a resent operation reference must never re-execute: %+v", resend)
	}

	receipt, err := c.store.ArtifactActionReceipt(opID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil {
		t.Fatal("the first attempt must have left a durable receipt")
	}
	if receipt.ArtifactKind != "agent_profile" || receipt.ArtifactID != "reviewer" || receipt.ArtifactDigest == "" {
		t.Fatalf("the receipt does not identify the exact artifact written: %+v", receipt)
	}
	registry, err := planning.LoadRegistry(c.config.PlanningDir)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := registry.Profile("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if installed.Digest != receipt.ArtifactDigest {
		t.Fatalf("the second, differently-worded request must never have reached the registry: installed digest %s, receipt digest %s", installed.Digest, receipt.ArtifactDigest)
	}

	// A profile naming an id that was never attempted at all reports
	// genuinely unknown (nil), never success.
	if never, err := c.store.ArtifactActionReceipt("00000000-0000-4000-8000-000000000000"); err != nil || never != nil {
		t.Fatalf("an operation never attempted must report no receipt: %+v, %v", never, err)
	}
}

// TestDispatchArtifactWithReceiptRecoversAProductWrite is the product-propose
// case: product adoption is NOT idempotent-by-content (CAS on revision), so
// a resend reaching the handler a second time would otherwise be refused for
// the wrong reason (stale revision) rather than recognized as the same
// already-delivered operation.
func TestDispatchArtifactWithReceiptRecoversAProductWrite(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	supervisor := newTestSupervisor(t, c)
	ctx := context.Background()
	noShutdown := func() {}
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "2222222222222222222222222222222222222222222222222222222222222222"

	request := runtime.ControlRequest{
		Command: runtime.ControlProductPropose, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding",
		Product: &product.ProductProposal{Name: "widget", Revision: 1, Repositories: []string{"acme/repo"}},
	}
	first := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, request)
	if !first.OK {
		t.Fatalf("the first attempt must succeed: %+v", first)
	}

	// The response is lost; the client resends the SAME operation reference
	// proposing the NEXT revision, exactly what an operator who believes the
	// first attempt failed would naturally do next.
	nextRevisionRequest := request
	nextRevisionRequest.Product = &product.ProductProposal{Name: "widget", Revision: 2, Repositories: []string{"acme/repo"}}
	resend := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, nextRevisionRequest)
	if resend.OK || resend.Code != runtime.ControlCodeOutcomeUnknown {
		t.Fatalf("a resent operation reference must never re-adopt a second revision: %+v", resend)
	}

	receipt, err := c.store.ArtifactActionReceipt(opID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt == nil || receipt.ProductID == "" || receipt.ProductRevision != 1 {
		t.Fatalf("the receipt does not identify the exact product revision adopted: %+v", receipt)
	}

	// Confirm only ONE revision was ever adopted - the resend truly did not
	// reach AdoptProduct a second time, not merely that it returned an error.
	current, _, err := c.store.CurrentProduct(receipt.ProductID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != 1 {
		t.Fatalf("exactly one revision must have been adopted, got revision %d", current.Revision)
	}
}

// TestDispatchArtifactWithReceiptReleasesAnUnfinalizedClaimOnRefusal is the
// claim/release half of the recovery contract: nothing durable happens when
// the effect is REFUSED (unlike a lost response, where it already did), so
// the operation id must not stay reserved forever - an operator who
// corrects the mistake and resubmits under the SAME reference must succeed.
func TestDispatchArtifactWithReceiptReleasesAnUnfinalizedClaimOnRefusal(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	supervisor := newTestSupervisor(t, c)
	ctx := context.Background()
	noShutdown := func() {}
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "3333333333333333333333333333333333333333333333333333333333333333"

	refused := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, runtime.ControlRequest{
		Command: runtime.ControlPlanningArtifactPropose, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding",
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
			"execution_agent": "does-not-exist",
			"capabilities": ["repository_analysis"],
			"trust_requirement": "operator_trusted"
		}`)},
	})
	if refused.OK {
		t.Fatalf("a profile naming an unconfigured agent must be refused: %+v", refused)
	}
	if receipt, err := c.store.ArtifactActionReceipt(opID); err != nil || receipt != nil {
		t.Fatalf("a refused attempt must leave no receipt and no stale claim: %+v, %v", receipt, err)
	}

	corrected := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, runtime.ControlRequest{
		Command: runtime.ControlPlanningArtifactPropose, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding",
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
			"execution_agent": "claude",
			"capabilities": ["repository_analysis"],
			"trust_requirement": "operator_trusted"
		}`)},
	})
	if !corrected.OK {
		t.Fatalf("a corrected retry under the SAME operation reference must succeed once the prior claim was released: %+v", corrected)
	}
}

// TestDispatchArtifactWithReceiptRefusesAnOperationIDAlreadyUsedByAPlanOrRunReceipt
// and its sibling below close the gap the two dispatch paths would otherwise
// leave open: operation_id is one client-chosen namespace, but each path only
// ever checked its OWN receipt store before claiming it, so the same id
// reused for a resume/plan-revise/refresh AND a planning-artifact/product
// action could end up recorded in both - and the shared GET endpoint
// (controlplane/operator_action_receipt.go) would then silently return
// whichever store it tries first, permanently shadowing the other.
func TestDispatchArtifactWithReceiptRefusesAnOperationIDAlreadyUsedByAPlanOrRunReceipt(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	supervisor := newTestSupervisor(t, c)
	ctx := context.Background()
	noShutdown := func() {}
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "4444444444444444444444444444444444444444444444444444444444444444"

	at := time.Unix(100, 0).UTC()
	if err := c.store.PutRun(runtime.EngineeringRun{
		SchemaVersion: runtime.SchemaVersion, ID: "s", Repository: "example/repo",
		Phase: runtime.Execute, Disposition: runtime.Active, CreatedAt: at, UpdatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.AppendEvent(runtime.EngineeringEvent{
		SchemaVersion: runtime.SchemaVersion, ID: "s-created", RunID: "s", Type: runtime.EventRunCreated, OccurredAt: at,
	}); err != nil {
		t.Fatal(err)
	}

	planOrRunReceipt := runtime.OperatorActionReceipt{
		OperationID: opID, RequestDigest: digest, Action: runtime.ControlResume,
		ControllerBinding: "test-binding", SourceRunID: "s", RunID: "s",
	}
	payload, err := json.Marshal(planOrRunReceipt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.store.AppendEvent(runtime.EngineeringEvent{
		SchemaVersion: runtime.SchemaVersion, ID: "operator-action-" + opID, OperationID: opID, RunID: "s",
		Type: runtime.EventRunOperatorActionReceipt, OccurredAt: time.Unix(200, 0).UTC(), Payload: payload,
	}); err != nil {
		t.Fatal(err)
	}

	crossed := c.dispatchArtifactWithReceipt(ctx, supervisor, noShutdown, runtime.ControlRequest{
		Command: runtime.ControlPlanningArtifactPropose, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding",
		PlanningArtifact: &planning.ArtifactWrite{Kind: planning.KindAgentProfile, ID: "reviewer", Document: []byte(`{
			"execution_agent": "claude",
			"capabilities": ["repository_analysis"],
			"trust_requirement": "operator_trusted"
		}`)},
	})
	if crossed.OK || crossed.Code != runtime.ControlCodeOutcomeUnknown {
		t.Fatalf("an operation id already recorded for a plan/run receipt must never be claimed by an artifact action: %+v", crossed)
	}
	if receipt, err := c.store.ArtifactActionReceipt(opID); err != nil || receipt != nil {
		t.Fatalf("the crossed attempt must leave no artifact claim behind: %+v, %v", receipt, err)
	}
}

func TestDispatchPlanOrRunWithReceiptRefusesAnOperationIDAlreadyUsedByAnArtifactReceipt(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	ctx := context.Background()
	noShutdown := func() {}
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "5555555555555555555555555555555555555555555555555555555555555555"

	if claimed, err := c.store.ClaimArtifactActionOperation(opID, time.Unix(100, 0).UTC()); err != nil || !claimed {
		t.Fatalf("claimed=%v, err=%v", claimed, err)
	}
	if err := c.store.FinalizeArtifactActionReceipt(runtime.ArtifactActionReceipt{
		OperationID: opID, RequestDigest: digest, Action: runtime.ControlPlanningArtifactPropose,
		ControllerBinding: "test-binding", ArtifactKind: "agent_profile", ArtifactID: "reviewer",
		ArtifactDigest: digest,
	}, time.Unix(100, 0).UTC()); err != nil {
		t.Fatal(err)
	}

	crossed := c.dispatchPlanOrRunWithReceipt(ctx, nil, noShutdown, runtime.ControlRequest{
		Command: runtime.ControlResume, OperationID: opID, RequestDigest: digest, ExpectedController: "test-binding", RunID: "s",
	})
	if crossed.OK || crossed.Code != runtime.ControlCodeOutcomeUnknown {
		t.Fatalf("an operation id already recorded for an artifact receipt must never be claimed by a plan/run action: %+v", crossed)
	}
}
