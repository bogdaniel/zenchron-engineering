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
	"github.com/bogdaniel/zenchron-engineering/review"
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
	// #474 R1: a failed review-remediation attempt must never ALSO journal
	// EventExecutionCompleted or an admissible handoff - Project reads the
	// former as "this candidate is execution-complete", which would
	// contradict the operation it was just recorded failed under. Only the
	// ORIGINAL, successful attempt that first reached H1 may have journalled
	// one; every subsequent failed remediation attempt must not add another,
	// and this run (no WorkGraph unit) must report no handoff at all.
	completions, handoffs := 0, 0
	for _, event := range state.events {
		switch event.Type {
		case EventExecutionCompleted:
			completions++
		case EventHandoffReported, EventHandoffRefused:
			handoffs++
		}
	}
	if completions != 1 {
		t.Fatalf("expected exactly 1 EventExecutionCompleted (the original H1 commit), got %d - a failed review-remediation attempt journalled completion evidence", completions)
	}
	if handoffs != 0 {
		t.Fatalf("expected no handoff observation for a run with no WorkGraph unit, got %d", handoffs)
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

// TestSameHeadReviewSupersessionBlocksProviderDispatch is #474 R2's required
// regression: D1 (REQUEST_CHANGES) is admitted for H1, then a SECOND
// independent reviewer reaches D2 (APPROVE) for the EXACT SAME head, with no
// intervening projection change - no new candidate, no new observation, the
// exact scenario where B2's live-head check alone would still pass, because
// the head never moved. D1's admission must never bind or deliver findings
// once a newer decision for the same PR exists, and the producer must never
// be invoked for it.
func TestSameHeadReviewSupersessionBlocksProviderDispatch(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
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
		StateDir:    filepath.Join(fixture.root, "review-state-r2"), Source: fixture.origin,
		ControllerID: "controller-a", Clock: fixture.clock,
	}
	ctx := context.Background()
	outcome1, admission1, err := fixture.runtime.ReconcileReviewRemediation(ctx, port, repo, prNumber, "claude")
	if err != nil {
		t.Fatalf("ReconcileReviewRemediation (D1): %v", err)
	}
	if outcome1.Decision.Verdict != "request_changes" || admission1 == nil {
		t.Fatalf("expected H1 to be admitted for remediation, got outcome=%+v admission=%v", outcome1, admission1)
	}

	// D2: a second, independent reviewer reaches APPROVE for the identical
	// subject (same repo, same PR, same H1 head) - directly stored, mirroring
	// exactly what a real second RunIndependentReview call would durably
	// produce, without re-running a full second provider invocation here.
	subject := review.Subject{Repository: repo.String(), PRNumber: prNumber, HeadSHA: h1}
	d2ID, err := review.DecisionID(subject, "claude2")
	if err != nil {
		t.Fatal(err)
	}
	d2 := review.Decision{
		SchemaVersion: review.SchemaVersion, ID: d2ID, Subject: subject, RunID: runID,
		ProducerAgentID: "codex", ReviewerAgentID: "claude2", Verdict: review.VerdictApprove,
		CreatedAt: fixture.clock.Now(),
	}
	if _, _, err := fixture.store.CreateReviewDecision(d2); err != nil {
		t.Fatal(err)
	}

	state = fixture.state(runID)
	pending, err := state.pendingReviewRemediationKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("D1, superseded by D2 at the identical head, must never still bind remediation: %v", pending)
	}
	findings, err := fixture.runtime.reviewRemediationFindings(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("D1's findings must never be delivered once superseded by D2: %+v", findings)
	}

	calls := len(fixture.provider.requests)
	outcome := fixture.reconcile(runID)
	if len(fixture.provider.requests) != calls {
		t.Fatalf("the producer was invoked for a superseded review-remediation admission: %d -> %d requests", calls, len(fixture.provider.requests))
	}
	state = fixture.state(runID)
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke && strings.HasPrefix(bindingOf(op), reviewRemediationBindingPrefix) {
			t.Fatalf("a review-remediation operation was dispatched for a superseded admission: %+v", op)
		}
	}
	_ = outcome
}

// TestReviewRemediationSupersessionFailsClosedOnAnUnreadableLatestDecision
// proves reviewRemediationSuperseded's other required half (#474 R2): a
// store read failure while checking for a newer decision must refuse the
// admission, never treat "could not prove this is still current" as
// "current".
func TestReviewRemediationSupersessionFailsClosedOnAnUnreadableLatestDecision(t *testing.T) {
	f := newAdmissionFixture(t)
	decision := f.seedDecision(f.headSHA, "claude", review.VerdictRequestChanges, blockingFinding("f1"))
	if _, created, err := f.runtime().AdmitReviewRemediation(context.Background(), f.port(), testRepo, admissionTestPRNumber); err != nil || !created {
		t.Fatalf("admitting the decision: created=%v err=%v", created, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := reviewRemediationSuperseded(f.store, decision); err == nil {
		t.Fatal("expected a read failure against a closed store to be returned as an error, never silently treated as current")
	}
}
