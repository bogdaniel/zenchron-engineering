package runtime

// Regression proofs for the fourth #32 dogfood, run-c56c766a136f0b15d57776c36b731ed4.
//
// The observed run, in order:
//
//	gpt-5.6-terra answered 16 HTTP 200 exchanges and the broker executed tools;
//	8 candidate.apply_patch requests were made and 1 succeeded;
//	the surviving change was one blank line in README.md;
//	the provider stopped with iteration_budget_exhausted;
//	execution.invoke was recorded Succeeded because mutation existed;
//	the runtime committed that partial mutation and ran exact-tree assurance;
//	assurance returned passed=false failure_class=transient_infrastructure;
//	assurance.go was recorded Succeeded anyway;
//	the planner then produced waiting/goal_state_reached.
//
// Three coupled defects. G: a retryable assurance failure satisfied its own
// operation and dead-ended the planner. H: partial producer work was promoted
// as completed execution. I: the patch contract wasted the whole iteration
// budget on a dialect Git never accepts and on hunk-count arithmetic.
//
// Everything here is a fake provider, a fake verifier and a real temporary Git
// workspace. No OpenAI call is made and no container is started.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// Defect G: a retryable assurance failure must not satisfy its operation
// ---------------------------------------------------------------------------

// alwaysFailingVerifier reports the same non-verdict forever. AssuranceRerun
// calls the provider twice per attempt and requires the two to agree, so a
// scripted slice would run dry; this cannot.
type alwaysFailingVerifier struct {
	class FailureClass
	calls int
}

func (v *alwaysFailingVerifier) Assure(context.Context, AssuranceRequest) (AssuranceResult, error) {
	v.calls++
	return AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: false, FailureClass: v.class}, nil
}

// TestTransientAssuranceFailureRetriesInsteadOfDeadEnding is the defect-G
// proof. It reproduces the exact planner state the fourth run reached.
func TestTransientAssuranceFailureRetriesInsteadOfDeadEnding(t *testing.T) {
	// The routing this defect is about is unchanged, and this test depends on
	// it: transient infrastructure is a retry, not a verdict.
	if RouteFailure(FailureTransientInfrastructure) != RouteRetry {
		t.Fatalf("transient infrastructure routes %q, want %q", RouteFailure(FailureTransientInfrastructure), RouteRetry)
	}
	fixture := newPhase8Fixture(t)
	fixture.useAssurance(&alwaysFailingVerifier{class: FailureTransientInfrastructure})
	runID := fixture.start()

	var lastOutcome Outcome
	seen := map[string]bool{}
	budget := 0
	for pass := 0; pass < 12; pass++ {
		// The invariant, checked at every point it can be checked: while the
		// current head carries a failed retry-routed assurance observation and
		// budget remains, an assurance operation is always wanted. A planner
		// that wants nothing here is exactly the goal_state_reached dead end.
		if state := fixture.state(runID); retryableAssuranceOutstanding(t, state, fixture.store, runID) {
			desired, wanted := state.plan()
			if !wanted || desired.kind != OpAssuranceGo {
				t.Fatalf("pass %d: planner wanted %+v (%t) while a retryable assurance failure was outstanding", pass, desired, wanted)
			}
			if state.satisfied(OpAssuranceGo, desired.key) {
				t.Fatal("a failed retryable assurance observation satisfied its own operation")
			}
		}
		lastOutcome = fixture.reconcile(runID)
		if lastOutcome.Reason == "goal_state_reached" {
			t.Fatalf("pass %d reached goal_state_reached with an unjudged candidate: %+v", pass, lastOutcome)
		}
		op, ok := assuranceOperation(t, fixture.store, runID)
		if !ok {
			continue
		}
		budget = op.MaxAttempts
		// Every retry is against the SAME exact binding.
		seen[op.IdempotencyKey] = true
		if lastOutcome.Disposition == Failed {
			break
		}
	}
	if len(seen) != 1 {
		t.Fatalf("assurance retried against %d different bindings; the exact commit/tree/contract must not move: %v", len(seen), seen)
	}
	op, _ := assuranceOperation(t, fixture.store, runID)
	if op.Attempt < 2 {
		t.Fatalf("assurance ran %d attempt(s); a retryable infrastructure failure must be retried", op.Attempt)
	}
	if op.Attempt > budget {
		t.Fatalf("assurance ran %d attempts against a ceiling of %d", op.Attempt, budget)
	}
	if lastOutcome.Disposition != Failed || lastOutcome.Reason != OpAssuranceGo+"_attempts_exhausted" {
		t.Fatalf("exhausted assurance settled %q (%q); want the bounded attempts_exhausted failure", lastOutcome.Disposition, lastOutcome.Reason)
	}

	events := journalOf(t, fixture.runtime, runID)
	// No producer ran between identical infrastructure retries: the candidate
	// the verifier could not judge is the candidate it is retried against.
	if commits := countType(events, EventCandidateCommitted); commits != 1 {
		t.Fatalf("%d candidate commits; an infrastructure retry must not re-run a producer", commits)
	}
	if attempts := countKindAttempts(events, OpExecutionInvoke); attempts != 1 {
		t.Fatalf("the producer ran %d times across identical assurance retries", attempts)
	}
	// And the observation itself is durable every time it happened.
	if observed := countType(events, EventAssuranceObserved); observed < 2 {
		t.Fatalf("%d assurance observations journalled for %d attempts", observed, op.Attempt)
	}
}

// TestGoalStateIsUnreachableWhileRetryableAssuranceIsOutstanding states the
// invariant directly, over the planner rather than over a scenario: while the
// current head carries a failed, retry-routed assurance observation and budget
// remains, an assurance operation is always wanted, so nothing can conclude the
// goal was reached.
func TestGoalStateIsUnreachableWhileRetryableAssuranceIsOutstanding(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.deps.Budgets.MaxAssuranceAttempts = 4
	fixture.useAssurance(&alwaysFailingVerifier{class: FailureTransientInfrastructure})
	runID := fixture.start()
	fixture.reconcile(runID)

	state := fixture.state(runID)
	assurance := state.projection.Assurance
	if assurance == nil || assurance.Stale || assurance.Passed {
		t.Fatalf("the fixture did not produce a current failed assurance observation: %#v", assurance)
	}
	if RouteFailure(assurance.FailureClass) != RouteRetry {
		t.Fatalf("observation class %q does not route to a retry", assurance.FailureClass)
	}
	op, ok := assuranceOperation(t, fixture.store, runID)
	if !ok {
		t.Fatal("no assurance operation exists")
	}
	// The core of the repair: the observation did not satisfy its operation, so
	// the exact binding never looks complete while the candidate is unjudged.
	key, wanted := bindAssuranceGo(state)
	if !wanted {
		t.Fatal("assurance is not even bound for a committed, execution-complete candidate")
	}
	if state.satisfied(OpAssuranceGo, key) {
		t.Fatal("a failed retryable assurance observation satisfied its own operation")
	}
	// And the run reached a bounded failure rather than concluding its goal.
	if state.snapshot.Reason == "goal_state_reached" {
		t.Fatalf("the run concluded the goal was reached with an unjudged candidate: %+v", state.snapshot)
	}
	if op.Attempt != op.MaxAttempts {
		t.Fatalf("assurance stopped at attempt %d of %d without exhausting its budget", op.Attempt, op.MaxAttempts)
	}
}

// retryableAssuranceOutstanding reports the exact precondition of the
// invariant: a current failed assurance observation whose class routes to a
// retry, with budget still remaining.
func retryableAssuranceOutstanding(t *testing.T, state *runState, store OperationStore, runID string) bool {
	t.Helper()
	assurance := state.projection.Assurance
	if assurance == nil || assurance.Stale || assurance.Passed {
		return false
	}
	if RouteFailure(assurance.FailureClass) != RouteRetry {
		return false
	}
	op, ok := assuranceOperation(t, store, runID)
	return ok && op.Attempt < op.MaxAttempts
}

// ---------------------------------------------------------------------------
// Defect H: partial producer work is a checkpoint, not a completed execution
// ---------------------------------------------------------------------------

// interruptedProducer reproduces the observed shape: it mutates the workspace
// and then reports that it ran out of reasoning iterations, exactly as the real
// provider did after eight patch attempts. Once switched, it completes.
type interruptedProducer struct {
	requests   []ExecutionRequest
	mutate     func(dir string, invocation int) error
	completeAt int
}

func (p *interruptedProducer) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *interruptedProducer) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	invocation := len(p.requests)
	if p.mutate != nil {
		if err := p.mutate(request.CandidateDir, invocation); err != nil {
			return ExecutionResult{}, err
		}
	}
	result := ExecutionResult{ProviderID: "test-provider", Model: "gpt-fixture", Attempt: 1, Outcome: Succeeded}
	if p.completeAt > 0 && invocation >= p.completeAt {
		return result, nil
	}
	// The observed stop: the provider reasoned, mutated, and was cut off by the
	// runtime's own iteration bound.
	result.Outcome = OperationFailed
	result.Failure = &ProviderFailure{Classification: FailureUnknown, RawDiagnosticRef: "artifacts/transcript.log"}
	return result, &ProviderStopError{Reason: StopIterationBudget, Detail: "reasoning iterations exceeded 16"}
}

func blankLineProducer() *interruptedProducer {
	return &interruptedProducer{mutate: func(dir string, invocation int) error {
		// One blank line appended to README.md - the entire surviving output of
		// the real run's sixteen exchanges.
		path := filepath.Join(dir, "README.md")
		existing, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.WriteFile(path, append(existing, []byte(strings.Repeat("\n", invocation))...), 0600)
	}}
}

// TestIterationBudgetStopCheckpointsInsteadOfCompleting is the defect-H proof
// for one pass: real work is preserved and exactly identified, #8 reassesses it,
// and nothing downstream may act on it.
func TestIterationBudgetStopCheckpointsInsteadOfCompleting(t *testing.T) {
	fixture := newPhase8Fixture(t)
	producer := blankLineProducer()
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	outcome := fixture.reconcile(runID)
	if outcome.Reason != "execution_checkpointed" {
		t.Fatalf("the pass settled %q (%q); a checkpoint must end the pass at a durable point", outcome.Disposition, outcome.Reason)
	}

	events := journalOf(t, fixture.runtime, runID)
	// The work is preserved as a runtime-owned commit, named for what it is.
	if countType(events, EventCandidateCheckpointed) != 1 {
		t.Fatalf("interrupted work was not checkpointed: %v", journalTypes(events))
	}
	if countType(events, EventCandidateCommitted) != 0 {
		t.Fatalf("interrupted work was promoted as a completed candidate: %v", journalTypes(events))
	}
	if countType(events, EventExecutionCompleted) != 0 {
		t.Fatalf("a provider that ran out of iterations was recorded as having completed: %v", journalTypes(events))
	}
	// The checkpoint has an exact commit and tree, and #8 reassessed it.
	state := fixture.state(runID)
	if state.projection.CandidateRevision == "" || state.projection.CandidateTree == "" {
		t.Fatalf("the checkpoint has no exact identity: %+v", state.projection)
	}
	if state.projection.CandidateComplete {
		t.Fatal("a checkpoint reported itself execution-complete")
	}
	if state.projection.Reassessment == nil {
		t.Fatalf("the checkpoint was not reassessed: %v", journalTypes(events))
	}
	// Nothing past execution is eligible, and no evidence or authority exists.
	if _, wanted := bindAssuranceGo(state); wanted {
		t.Fatal("assurance is eligible for an execution that never completed")
	}
	if countType(events, EventAssuranceObserved) != 0 {
		t.Fatalf("assurance ran on preserved partial work: %v", journalTypes(events))
	}
	if len(state.projection.EvidenceBundles) != 0 {
		t.Fatalf("a checkpoint produced evidence: %#v", state.projection.EvidenceBundles)
	}
	if len(state.projection.AuthorityDecisions) != 0 {
		t.Fatalf("a checkpoint produced an authority decision: %#v", state.projection.AuthorityDecisions)
	}
	if state.authorizedForPublication() {
		t.Fatal("a checkpoint authorized publication")
	}
	// A continuation is what execution binds to next, against the exact
	// checkpoint commit.
	key, wanted := bindExecutionInvoke(state)
	if !wanted {
		t.Fatal("a checkpointed run does not want to continue execution")
	}
	if key != "continuation|"+state.projection.CandidateRevision {
		t.Fatalf("the next execution is not bound to the exact checkpoint: %q", key)
	}
}

// TestCheckpointContinuesAcrossRestartAndOnlyThenBecomesAssurable is the full
// mandatory restart proof, in the ten steps the repair specifies.
func TestCheckpointContinuesAcrossRestartAndOnlyThenBecomesAssurable(t *testing.T) {
	fixture := newPhase8Fixture(t)
	producer := blankLineProducer()
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()

	// 1-3: the provider mutates, ends on the iteration budget, and the runtime
	// checkpoints an exact commit and tree.
	fixture.reconcile(runID)
	before := fixture.state(runID)
	checkpointCommit, checkpointTree := before.projection.CandidateRevision, before.projection.CandidateTree
	workspaceBefore := candidateDir(fixture.stateDir, runID)
	if checkpointCommit == "" || before.projection.CandidateComplete {
		t.Fatalf("no checkpoint was produced: %+v", before.projection)
	}

	// 4-5: close the store and reopen from the journal alone.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fixture.store = reopened
	fixture.deps.Store = reopened
	replayed, err := reopened.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if projection.CandidateComplete || projection.CandidateRevision != checkpointCommit {
		t.Fatalf("the checkpoint did not survive the restart: %+v", projection)
	}

	// 6-8: the producer now completes. No manual cleanup or reset happens.
	producer.completeAt = len(producer.requests) + 1
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.reconcile(runID)

	continuation := producer.requests[len(producer.requests)-1]
	if continuation.Purpose != InvocationContinuation {
		t.Fatalf("the resumed invocation had purpose %q, want %q", continuation.Purpose, InvocationContinuation)
	}
	if continuation.Candidate.Revision != checkpointCommit || continuation.Candidate.Tree != checkpointTree {
		t.Fatalf("the continuation was not bound to the exact checkpoint: %+v, want %s/%s",
			continuation.Candidate, checkpointCommit, checkpointTree)
	}
	if continuation.RunID != runID {
		t.Fatalf("the continuation belonged to run %s", continuation.RunID)
	}
	if got := candidateDir(fixture.stateDir, runID); got != workspaceBefore {
		t.Fatalf("the continuation used a second candidate workspace: %s vs %s", got, workspaceBefore)
	}

	// 9-10: the candidate is execution-complete, and only now is assurance
	// eligible - and it actually ran.
	after := fixture.state(runID)
	if !after.projection.CandidateComplete {
		t.Fatalf("a completed continuation left the candidate incomplete: %+v", after.projection)
	}
	resumed := journalOf(t, fixture.runtime, runID)
	if countType(resumed, EventExecutionCompleted) == 0 {
		t.Fatalf("no producer completion was observed: %v", journalTypes(resumed))
	}
	if countType(resumed, EventAssuranceObserved) == 0 {
		t.Fatalf("assurance never became eligible after execution completed: %v", journalTypes(resumed))
	}
	// Assurance saw the exact head, not the checkpoint that preceded it.
	verifier := fixture.verifier()
	if len(verifier.Requests) == 0 {
		t.Fatal("the verifier was never asked")
	}
	last := verifier.Requests[len(verifier.Requests)-1]
	if last.Commit != after.projection.CandidateRevision || last.Tree != after.projection.CandidateTree {
		t.Fatalf("assurance ran on %s/%s, not the exact completed head %s/%s",
			last.Commit, last.Tree, after.projection.CandidateRevision, after.projection.CandidateTree)
	}
}

// TestContinuationWithoutFurtherMutationPromotesTheCheckpoint is case C: the
// continuation finds nothing left to do and finishes. The checkpoint it
// inherited becomes the execution-complete candidate, with no invented commit.
func TestContinuationWithoutFurtherMutationPromotesTheCheckpoint(t *testing.T) {
	fixture := newPhase8Fixture(t)
	producer := &interruptedProducer{mutate: func(dir string, invocation int) error {
		if invocation > 1 {
			return nil // the continuation changes nothing
		}
		return os.WriteFile(filepath.Join(dir, "README.md"), []byte("partial\n"), 0600)
	}}
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)

	before := fixture.state(runID)
	checkpoint := before.projection.CandidateRevision
	if checkpoint == "" || before.projection.CandidateComplete {
		t.Fatalf("no checkpoint: %+v", before.projection)
	}

	producer.completeAt = len(producer.requests) + 1
	fixture.reconcile(runID)

	after := fixture.state(runID)
	if !after.projection.CandidateComplete {
		t.Fatal("a completed continuation did not promote the checkpoint")
	}
	if after.projection.CandidateRevision != checkpoint {
		t.Fatalf("promotion invented a new commit: %s, want the checkpoint %s", after.projection.CandidateRevision, checkpoint)
	}
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventCandidateCheckpointed) != 1 {
		t.Fatalf("promotion produced another checkpoint: %v", journalTypes(events))
	}
}

// TestContinuationsAreBoundedByTheExecutionBudget is case D. A provider that
// never finishes must not produce checkpoints forever: every continuation
// shares one operation identity, so they spend one attempt budget between them
// and then settle with the bounded failure.
func TestContinuationsAreBoundedByTheExecutionBudget(t *testing.T) {
	fixture := newPhase8Fixture(t)
	producer := blankLineProducer()
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()

	var outcome Outcome
	for pass := 0; pass < 16; pass++ {
		outcome = fixture.reconcile(runID)
		if outcome.Disposition == Failed {
			break
		}
		if outcome.Reason == "goal_state_reached" {
			t.Fatalf("pass %d concluded the goal was reached with an incomplete candidate", pass)
		}
	}
	if outcome.Disposition != Failed || outcome.Reason != "execution_continuations_exhausted" {
		t.Fatalf("continuations did not terminate on the execution bound: %+v", outcome)
	}
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventAssuranceObserved) != 0 {
		t.Fatalf("assurance ran while execution never completed: %v", journalTypes(events))
	}
}

// TestUnrelatedProviderStopsKeepTheirExistingSemantics keeps the repair narrow:
// only the observed iteration-budget stop becomes a checkpoint.
func TestUnrelatedProviderStopsKeepTheirExistingSemantics(t *testing.T) {
	for name, reason := range map[string]ProviderStop{
		"no progress":      StopNoProgress,
		"deadline":         StopDeadlineExceeded,
		"cancelled":        StopCancelled,
		"provider error":   StopProviderError,
		"tool call budget": StopToolCallBudget,
		"token budget":     StopTokenBudget,
	} {
		t.Run(name, func(t *testing.T) {
			if continuationEligible(&ProviderStopError{Reason: reason}) {
				t.Fatalf("%q was treated as continuable work", reason)
			}
		})
	}
	if !continuationEligible(&ProviderStopError{Reason: StopIterationBudget}) {
		t.Fatal("the observed iteration-budget stop is not continuable")
	}
	// A plain error is not a bounded stop and is not continuable either.
	if continuationEligible(context.Canceled) || continuationEligible(nil) {
		t.Fatal("a non-stop error was treated as continuable work")
	}
}

// ---------------------------------------------------------------------------
// Defect I: one patch engine, a stated grammar, recoverable diagnostics
// ---------------------------------------------------------------------------

// TestBeginPatchDialectIsRefusedByNameWithoutTranslation is the fixture for the
// dialect the real provider emitted repeatedly.
func TestBeginPatchDialectIsRefusedByNameWithoutTranslation(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	dialect := "*** Begin Patch\n*** Update File: hello.txt\n@@\n-candidate-content-9c3\n+rewritten-9c3\n*** End Patch\n"
	err := broker.ApplyPatch([]byte(dialect))
	if err == nil {
		t.Fatal("the *** Begin Patch dialect was accepted")
	}
	for _, want := range []string{"git-compatible unified diff", "*** Begin Patch", "*** Update File"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal does not name the problem (%q): %v", want, err)
		}
	}
	// Nothing was translated and nothing was written.
	unchanged, readErr := os.ReadFile(filepath.Join(broker.CandidateDir, "hello.txt"))
	if readErr != nil || string(unchanged) != "candidate-content-9c3\n" {
		t.Fatalf("a refused dialect mutated the workspace: %v %q", readErr, unchanged)
	}
	// The refusal is deterministic: the same input gives the same text.
	if second := broker.ApplyPatch([]byte(dialect)); second == nil || second.Error() != err.Error() {
		t.Fatalf("the refusal is not deterministic: %v vs %v", err, second)
	}
}

// TestMalformedHunkCountsStillApplyWithRecount is the other half of the wasted
// iterations: a coherent unified diff whose @@ header arithmetic is wrong. Git
// stays the parser; --recount makes the hunk body authoritative.
func TestMalformedHunkCountsStillApplyWithRecount(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	path := filepath.Join(broker.CandidateDir, "counted.txt")
	if err := os.WriteFile(path, []byte("l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := commitFixtureWorkspace(broker.CandidateDir); err != nil {
		t.Fatal(err)
	}
	// The header claims 7 old and 6 new lines; the body carries 5 and 4. This
	// is the shape the real run's "@@ -157,7 +157,6 @@" had.
	patch := "--- a/counted.txt\n+++ b/counted.txt\n@@ -2,7 +2,6 @@\n l2\n l3\n-l4\n l5\n l6\n"
	if err := broker.ApplyPatch([]byte(patch)); err != nil {
		t.Fatalf("a coherent diff with wrong header counts was refused: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != "l1\nl2\nl3\nl5\nl6\nl7\nl8\n" {
		t.Fatalf("recount applied the wrong change: %v %q", err, after)
	}
}

// TestWrongContextStillFailsClosedWithAUsefulDiagnostic proves --recount did not
// weaken verification: the hunk body is authoritative, the file still is not.
func TestWrongContextStillFailsClosedWithAUsefulDiagnostic(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	patch := "--- a/hello.txt\n+++ b/hello.txt\n@@ -1,2 +1,2 @@\n THIS-LINE-IS-NOT-IN-THE-FILE\n-candidate-content-9c3\n+rewritten-9c3\n"
	err := broker.ApplyPatch([]byte(patch))
	if err == nil {
		t.Fatal("a patch whose context does not match the workspace was applied")
	}
	if !strings.Contains(err.Error(), "hello.txt") {
		t.Fatalf("the diagnostic does not name the path: %v", err)
	}
	unchanged, readErr := os.ReadFile(filepath.Join(broker.CandidateDir, "hello.txt"))
	if readErr != nil || string(unchanged) != "candidate-content-9c3\n" {
		t.Fatalf("a failing patch mutated the workspace: %v %q", readErr, unchanged)
	}
	assertSafePatchDiagnostic(t, err, broker.CandidateDir)
}

// TestPatchOutsideTheBrokerRulesRemainsRefused proves no capability was widened
// by any of the above: the resolve gate still owns which paths exist.
func TestPatchOutsideTheBrokerRulesRemainsRefused(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	for name, patch := range map[string]string{
		"escapes the workspace":  "--- a/../outside.txt\n+++ b/../outside.txt\n@@ -0,0 +1 @@\n+escaped\n",
		"runtime-owned git data": "--- /dev/null\n+++ b/.git/config\n@@ -0,0 +1 @@\n+[core]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := broker.ApplyPatch([]byte(patch)); err == nil {
				t.Fatal("a patch outside the broker rules was accepted")
			}
			if _, err := os.Stat(filepath.Join(broker.CandidateDir, "..", "outside.txt")); err == nil {
				t.Fatal("a refused patch wrote outside the candidate workspace")
			}
		})
	}
}

// assertSafePatchDiagnostic holds the boundary the richer diagnostics must not
// cross: a model may learn which path and hunk failed, and nothing else.
func assertSafePatchDiagnostic(t *testing.T, err error, root string) {
	t.Helper()
	detail := err.Error()
	if strings.Contains(detail, root) {
		t.Fatalf("the diagnostic exposes the host workspace path: %v", err)
	}
	for _, field := range strings.Fields(detail) {
		if strings.HasPrefix(field, "/") {
			t.Fatalf("the diagnostic exposes an absolute host path %q: %v", field, err)
		}
	}
	for _, forbidden := range []string{".git/", "PATH=", "HOME=", "Authorization", "Bearer ", "sk-", "ghp_"} {
		if strings.Contains(detail, forbidden) {
			t.Fatalf("the diagnostic exposes %q: %v", forbidden, err)
		}
	}
	if len(detail) > maxPayloadFieldBytes+64 {
		t.Fatalf("the diagnostic is unbounded: %d bytes", len(detail))
	}
}

// TestApplyPatchToolContractStatesTheAcceptedGrammar keeps the model-facing
// contract honest: the description is what a model reads before its first
// attempt, and the real run proved an unstated grammar costs a whole budget.
func TestApplyPatchToolContractStatesTheAcceptedGrammar(t *testing.T) {
	var description string
	for _, definition := range toolDefinitions() {
		if definition.Name == ToolCandidateApplyPatch {
			description = definition.Description
		}
	}
	if description == "" {
		t.Fatalf("%s is not advertised", ToolCandidateApplyPatch)
	}
	for _, want := range []string{
		"--- a/path", "+++ b/path", "@@ ", "diff --git",
		"*** Begin Patch", "*** Update File:", "*** Add File:", "*** Delete File:", "*** End Patch",
		"git apply", "repo.read",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("the tool contract does not state %q:\n%s", want, description)
		}
	}
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

func assuranceOperation(t *testing.T, store OperationStore, runID string) (RunOperation, bool) {
	t.Helper()
	ops, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpAssuranceGo {
			return op, true
		}
	}
	return RunOperation{}, false
}

func countKindAttempts(events []EngineeringEvent, kind string) int {
	n := 0
	for _, e := range events {
		if e.Type != EventOperationBefore {
			continue
		}
		var op RunOperation
		if json.Unmarshal(e.Payload, &op) == nil && op.Kind == kind {
			n++
		}
	}
	return n
}

// commitFixtureWorkspace makes new fixture files part of the workspace history,
// so a patch against them is a patch against tracked content.
func commitFixtureWorkspace(dir string) error {
	git := GitRunner{Dir: dir}
	if _, err := git.run("add", "-A"); err != nil {
		return err
	}
	_, err := git.run("-c", "user.email=fixture@zenchron.invalid", "-c", "user.name=fixture", "commit", "-m", "fixture")
	return err
}
