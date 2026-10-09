package runtime

// #474 end-to-end acceptance: H1 -> independent BLOCK -> same logical
// producer remediation on its existing EngineeringRun -> H2 -> fresh
// independent review -> APPROVE, driven entirely through the real
// coordinator (phase8Fixture's Reconcile loop), the real ReviewPort
// (RunIndependentReview), and the real admission gate
// (ReconcileReviewRemediation -> AdmitReviewRemediation) - never by
// manually injecting journal or admission rows between stages.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// sequencedReviewProvider returns a different scripted ReviewerResult
// document on each successive call - the one shape a single fixed-document
// reviewStubProvider cannot express, needed here because H1 and H2 must be
// judged differently by the SAME reviewer identity across two separate,
// independent review operations.
type sequencedReviewProvider struct {
	documents []string
	calls     int
}

func (p *sequencedReviewProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	i := p.calls
	if i >= len(p.documents) {
		i = len(p.documents) - 1
	}
	p.calls++
	if err := os.WriteFile(request.ReviewerResultPath, []byte(p.documents[i]), 0600); err != nil {
		return ExecutionResult{}, err
	}
	return ExecutionResult{ProviderID: "test-reviewer", Outcome: execution.Succeeded}, nil
}

// e2eResolveAgent resolves the two independent identities this acceptance
// test's whole independence chain depends on: a producer and a reviewer of
// different vendor families (CheckReviewIndependence requires both).
func e2eResolveAgent(id string) (ResolvedAgent, error) {
	switch id {
	case "codex":
		return ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}, nil
	case "claude":
		return ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted}, nil
	}
	return ResolvedAgent{}, &UnknownAgentError{ID: id}
}

func TestReviewRemediationEndToEndH1BlockH2Approve(t *testing.T) {
	fixture := newPhase8Fixture(t)
	// A real producer identity is required for independence to be
	// evaluable at all (CheckReviewIndependence fails closed on an unknown
	// vendor family); the fixture's own default Dependencies.Agent is the
	// zero value, so it is set explicitly here and the runtime rebuilt
	// against the same store/forge/provider the fixture already wired.
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	var producerAttempts int
	fixture.provider = newIsolatedProvider(func(dir string) error {
		producerAttempts++
		content := []byte("package candidate\n// v1: incomplete\n")
		if producerAttempts > 1 {
			content = []byte("package candidate\n// v2: addresses independent review finding\n")
		}
		return os.WriteFile(filepath.Join(dir, "candidate.go"), content, 0600)
	})
	fixture.deps.Provider = fixture.provider
	fixture.runtime = fixture.newRuntime(fixture.deps)

	runID := fixture.start()
	// Drive the real coordinator to H1: candidate committed, assured,
	// published. The fixed pass count mirrors the probe this test's fixture
	// shape was validated against; the loop below tolerates fewer passes
	// being needed and simply stops advancing once nothing is wanted.
	for i := 0; i < 20; i++ {
		fixture.reconcile(runID)
	}
	state := fixture.state(runID)
	if state.projection.CandidateRevision == "" || state.projection.PullRequest == nil {
		t.Fatalf("producer did not reach a published candidate: projection=%+v", state.projection)
	}
	h1 := state.projection.CandidateRevision
	prNumber := state.projection.PullRequest.Number
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	blockDocument := `{"schema_version":"0.1","verdict":"blocked","findings":[{"signature":"candidate is marked incomplete","severity":"blocking"}]}`
	approveDocument := `{"schema_version":"0.1","verdict":"accepted"}`
	reviewerProvider := &sequencedReviewProvider{documents: []string{blockDocument, approveDocument}}

	port := &SupervisorReviewPort{
		Store: fixture.store, GitHub: fixture.forge, ResolveAgent: e2eResolveAgent,
		ProviderFor: func(ResolvedAgent) (ExecutionProvider, error) { return reviewerProvider, nil },
		StateDir:    filepath.Join(fixture.root, "review-state"), Source: fixture.origin,
		ControllerID: "controller-a", Clock: fixture.clock,
	}
	ctx := context.Background()

	// H1: independent review reaches REQUEST_CHANGES, and #474's automatic
	// trigger durably admits remediation from it - no operator relay.
	outcome1, admission1, err := fixture.runtime.ReconcileReviewRemediation(ctx, port, repo, prNumber, "claude")
	if err != nil {
		t.Fatalf("ReconcileReviewRemediation (H1): %v", err)
	}
	if outcome1.Decision.Verdict != "request_changes" {
		t.Fatalf("expected H1 to be reviewed REQUEST_CHANGES, got %+v", outcome1.Decision)
	}
	if admission1 == nil {
		t.Fatal("expected H1's BLOCK to be durably admitted for remediation")
	}
	if admission1.HeadSHA != h1 {
		t.Fatalf("admission bound to %s, want H1 %s", admission1.HeadSHA, h1)
	}

	// The SAME logical producer, same RunID, is given a FRESH bounded
	// invocation - no inherited provider transcript (the original
	// isolatedProvider call is a brand-new Execute call; nothing here
	// resumes anything) - and produces a DIFFERENT exact candidate, H2.
	// A post-commit base-integrate can still move the candidate again
	// before the push settles, so this polls to actual convergence (the
	// published head matching the current candidate) rather than assuming
	// any fixed pass count is enough - the same thing a real supervisor's
	// own repeated ticks do.
	for i := 0; i < 30; i++ {
		fixture.reconcile(runID)
		state = fixture.state(runID)
		if state.projection.PullRequest != nil && !state.projection.PullRequest.Stale {
			break
		}
	}
	h2 := state.projection.CandidateRevision
	if h2 == "" || h2 == h1 {
		t.Fatalf("expected a new exact candidate after remediation, got h1=%s h2=%s", h1, h2)
	}
	if producerAttempts < 2 {
		t.Fatalf("expected at least 2 producer invocations (initial + remediation), got %d", producerAttempts)
	}
	if state.projection.PullRequest == nil || state.projection.PullRequest.Stale {
		t.Fatalf("expected H2 %s to converge to a freshly published, non-stale PR within the bounded poll, got %+v", h2, state.projection.PullRequest)
	}

	// H1's old review is stale against the new head.
	stale, err := port.IsStale(ctx, repo, prNumber)
	if err != nil {
		t.Fatalf("IsStale: %v", err)
	}
	if !stale {
		t.Fatal("expected H1's review to be stale once H2 is the current head")
	}

	// A fresh independent review of the exact new head: a DIFFERENT
	// DecisionID (bound to H2, not H1), and this time ACCEPT.
	outcome2, admission2, err := fixture.runtime.ReconcileReviewRemediation(ctx, port, repo, prNumber, "claude")
	if err != nil {
		t.Fatalf("ReconcileReviewRemediation (H2): %v", err)
	}
	if outcome2.Decision.Verdict != "approve" {
		t.Fatalf("expected H2 to be independently APPROVEd, got %+v", outcome2.Decision)
	}
	if outcome2.Decision.ID == outcome1.Decision.ID {
		t.Fatal("expected H2's decision to have a distinct identity from H1's")
	}
	if outcome2.Decision.Subject.HeadSHA != h2 {
		t.Fatalf("expected the fresh decision to be bound to H2 %s, got %s", h2, outcome2.Decision.Subject.HeadSHA)
	}
	if admission2 != nil {
		t.Fatalf("an APPROVE must never be routed through remediation admission, got %+v", admission2)
	}

	// H1's stale decision can never satisfy H2: admitting against H1 again
	// is refused (the decision is no longer current), proving old-head
	// review cannot approve the new head.
	if _, _, err := fixture.runtime.AdmitReviewRemediation(ctx, port, repo, prNumber); err == nil {
		t.Fatal("expected AdmitReviewRemediation to refuse once the latest decision is APPROVE, not REQUEST_CHANGES")
	}
}

// TestUnresolvedReviewRemediationReturnFailsTheOperationAndStaysActionable is
// #474 B1's regression, the review-remediation sibling of
// TestUnresolvedFeedbackReturnFailsTheOperationAndStaysActionable (#376): an
// admitted independent-review BLOCK must never be satisfied by a producer
// invocation that merely returns success without mutating the candidate and
// without stating an admitted no-change resolution naming it. Before B1,
// invokeExecution's completion gate only consulted len(feedback); a provider
// that ignored delivered review-remediation findings left
// EventExecutionCompleted/Succeeded for the exact review-remediation|H1|...
// binding, which bindExecutionInvoke would then treat as satisfied forever
// even though the reviewer's REQUEST_CHANGES was never addressed.
func TestUnresolvedReviewRemediationReturnFailsTheOperationAndStaysActionable(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	var attempts int
	fixture.provider = newIsolatedProvider(func(dir string) error {
		attempts++
		if attempts == 1 {
			return os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate\n// v1\n"), 0600)
		}
		// Every remediation attempt returns success having neither mutated
		// the candidate nor (resolveFeedback stays false) stated a
		// no-change resolution - the exact #376/B1 shape under test.
		return nil
	})
	fixture.deps.Provider = fixture.provider
	fixture.runtime = fixture.newRuntime(fixture.deps)

	runID := fixture.start()
	for i := 0; i < 20; i++ {
		fixture.reconcile(runID)
	}
	state := fixture.state(runID)
	if state.projection.CandidateRevision == "" || state.projection.PullRequest == nil {
		t.Fatalf("producer did not reach a published candidate: projection=%+v", state.projection)
	}
	h1 := state.projection.CandidateRevision
	prNumber := state.projection.PullRequest.Number
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	blockDocument := `{"schema_version":"0.1","verdict":"blocked","findings":[{"signature":"candidate is marked incomplete","severity":"blocking"}]}`
	reviewerProvider := &sequencedReviewProvider{documents: []string{blockDocument}}
	port := &SupervisorReviewPort{
		Store: fixture.store, GitHub: fixture.forge, ResolveAgent: e2eResolveAgent,
		ProviderFor: func(ResolvedAgent) (ExecutionProvider, error) { return reviewerProvider, nil },
		StateDir:    filepath.Join(fixture.root, "review-state-b1"), Source: fixture.origin,
		ControllerID: "controller-a", Clock: fixture.clock,
	}
	ctx := context.Background()
	outcome1, admission1, err := fixture.runtime.ReconcileReviewRemediation(ctx, port, repo, prNumber, "claude")
	if err != nil {
		t.Fatalf("ReconcileReviewRemediation: %v", err)
	}
	if outcome1.Decision.Verdict != "request_changes" || admission1 == nil {
		t.Fatalf("expected H1 to be admitted for remediation, got outcome=%+v admission=%v", outcome1, admission1)
	}

	// MaxExecutionAttempts is 2 (phase8Governance's fixture budget): one
	// Reconcile call runs the binding to attempt exhaustion, exactly as
	// TestUnresolvedFeedbackReturnFailsTheOperationAndStaysActionable does.
	outcome := fixture.reconcile(runID)
	state = fixture.state(runID)
	if state.projection.CandidateRevision != h1 {
		t.Fatalf("a provider that neither mutated nor resolved the review-remediation finding advanced the candidate past H1: %s -> %s", h1, state.projection.CandidateRevision)
	}
	var invoke RunOperation
	found := 0
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke && strings.HasPrefix(bindingOf(op), reviewRemediationBindingPrefix) {
			invoke, found = op, found+1
		}
	}
	if found != 1 {
		t.Fatalf("expected exactly one review-remediation execution.invoke operation, found %d", found)
	}
	if invoke.State != OperationFailed {
		t.Fatalf("an invocation that neither changed the candidate nor stated an admitted resolution was recorded as %s, not a failed operation", invoke.State)
	}
	var result mutationResult
	if err := decodeJSON(invoke.Result, &result); err != nil || result.FailureClass != FailureReviewRemediationUnresolved {
		t.Fatalf("unresolved review remediation did not record failure_class=review_remediation_unresolved: %+v (decode err %v)", result, err)
	}
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("an admitted BLOCK that was never discharged is not outstanding: %v", pending)
	}
	if outcome.Disposition == Waiting && outcome.Reason == ReasonGoalStateReached {
		t.Fatalf("an unaddressed independent-review BLOCK reached goal_state_reached: %+v", outcome)
	}
	if outcome.Disposition != Failed || !BudgetBoundary(outcome.Disposition, outcome.Reason) {
		t.Fatalf("exhausted attempts did not stop the run truthfully under its existing finite authority: %+v", outcome)
	}
}
