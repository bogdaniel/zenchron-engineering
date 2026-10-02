package runtime

// Regression proofs for #390: a refused invocation's mutated material must not
// cross candidate.committed, assurance, an authority decision or publication
// merely because the workspace changed. Mutation is evidence that material
// exists, not evidence that the invocation which produced it succeeded.
//
// The live defect (#388, PR #389): a Claude Code invocation reported a valid
// final result while a main-thread Bash call it started with
// run_in_background was never polled or killed before that result -
// cli_agent.go's own #384 detector correctly classified this as
// provider_background_work_unresolved - and the SAME invocation had already
// written 5 changed paths. Because record.Mutated was true, the operation's
// own admission gate left it Succeeded rather than Failed, so the refused
// material went on to candidate.committed, assurance, authority.evaluated,
// a push and a published pull request. #384's detector worked; the
// enforcement boundary downstream of it did not.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// refusedMutatingProducer reports a provider failure classification while
// having already mutated the candidate workspace in the SAME invocation -
// the #384/#388 shape, kept general: class is whatever failure a test wants
// to prove the gate against, and execErr is always nil, because the provider
// reached its own result rather than merely erroring out before one.
type refusedMutatingProducer struct {
	requests []ExecutionRequest
	class    FailureClass
	paths    int
}

func (p *refusedMutatingProducer) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *refusedMutatingProducer) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	for i := 0; i < p.paths; i++ {
		name := filepath.Join(request.CandidateDir, "refused-"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("refused material\n"), 0600); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{
		ProviderID: "test-provider", Model: "gpt-fixture", Attempt: 1,
		Outcome: OperationFailed,
		Failure: &ProviderFailure{Classification: p.class, RawDiagnosticRef: "artifacts/transcript.log"},
	}, nil
}

// assertRefusedMutationIsNotAdmitted is the shared shape of tests 1 and 4: an
// invocation that refuses with a failure classification, after having
// mutated the workspace, must fail its own operation, must not reach
// candidate.committed/assurance/authority/push/PR, and the material it left
// behind must be held, named by its producing operation and exact content -
// never destroyed, never promoted.
func assertRefusedMutationIsNotAdmitted(t *testing.T, class FailureClass, paths int) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	producer := &refusedMutatingProducer{class: class, paths: paths}
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	outcome := fixture.reconcile(runID)
	if outcome.Reason == "goal_state_reached" {
		t.Fatalf("a refused invocation reached goal_state_reached: %+v", outcome)
	}

	events := journalOf(t, fixture.runtime, runID)
	if n := countType(events, EventCandidateCommitted); n != 0 {
		t.Fatalf("refused material was committed: %d candidate.committed event(s)", n)
	}
	if n := countType(events, EventAssuranceObserved); n != 0 {
		t.Fatalf("refused material was sent to assurance: %d observation(s)", n)
	}
	if n := countType(events, EventAuthorityEvaluated); n != 0 {
		t.Fatalf("refused material reached an authority decision: %d decision(s)", n)
	}
	if n := countType(events, EventCandidateBaseIntegrated); n != 0 {
		t.Fatalf("refused material was base-integrated: %d integration(s)", n)
	}

	state := fixture.state(runID)
	if state.authorizedForPublication() {
		t.Fatal("a refused invocation's material authorized publication")
	}
	key, wanted := bindExecutionInvoke(state)
	if !wanted {
		t.Fatal("no execution.invoke binding for the refused invocation")
	}
	op, ok := state.operationByKey(OpExecutionInvoke, key)
	if !ok {
		t.Fatal("no execution.invoke operation was recorded")
	}
	if op.State != OperationFailed {
		t.Fatalf("the refused invocation's operation is %q, want %q", op.State, OperationFailed)
	}
	var record mutationResult
	if err := json.Unmarshal(op.Result, &record); err != nil {
		t.Fatal(err)
	}
	if record.FailureClass != class {
		t.Fatalf("recorded failure class %q, want %q", record.FailureClass, class)
	}
	if !record.Mutated || record.PathCount != paths {
		t.Fatalf("test setup error: record.Mutated=%v PathCount=%d, want a mutated %d-path invocation", record.Mutated, record.PathCount, paths)
	}

	// The material is held, not destroyed: named by the exact operation, path
	// count and content, exactly as #203 already holds a succeeded producer's
	// own not-yet-committed change.
	held := state.heldMaterial(OpExecutionInvoke + attemptsExhaustedSuffix)
	if held == nil {
		t.Fatal("the refused invocation's material is not held")
	}
	if held.Kind != HeldUncommitted || held.Operation != op.ID || held.PathCount != paths || held.ContentDigest == "" {
		t.Fatalf("held %+v, want the uncommitted change of %s identified by its content", *held, op.ID)
	}

	notPublished(t, fixture, runID)
}

// TestARefusedInvocationsMutatedMaterialIsNotAdmitted is the restored defect:
// a failure classification on an invocation result is authoritative for that
// invocation whatever Mutated is. This must fail against a gate that excuses
// provider_background_work_unresolved from failing its operation merely
// because the workspace changed.
func TestARefusedInvocationsMutatedMaterialIsNotAdmitted(t *testing.T) {
	assertRefusedMutationIsNotAdmitted(t, FailureProviderBackgroundWorkUnresolved, 5)
}

// TestADifferentFailureClassWithMutationIsAlsoNotAdmitted proves the fix is
// general across failure classes, not a special case for
// provider_background_work_unresolved: any class outside the two reasoned
// checkpoint shapes (FailureExecutionIncomplete, and FailureProviderNoProgress
// with outstanding review feedback) must fail its operation the same way.
func TestADifferentFailureClassWithMutationIsAlsoNotAdmitted(t *testing.T) {
	assertRefusedMutationIsNotAdmitted(t, FailureTransientProvider, 2)
}

// TestANonMutatingRefusalStaysRefusedWithNothingHeld proves the fix did not
// invent a new disposition: a refusal that changed nothing is refused exactly
// as before, and holds nothing, because there is no material to hold.
func TestANonMutatingRefusalStaysRefusedWithNothingHeld(t *testing.T) {
	fixture := newPhase8Fixture(t)
	producer := &refusedMutatingProducer{class: FailureProviderBackgroundWorkUnresolved, paths: 0}
	fixture.deps.Provider = producer
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)

	state := fixture.state(runID)
	key, wanted := bindExecutionInvoke(state)
	if !wanted {
		t.Fatal("no execution.invoke binding")
	}
	op, ok := state.operationByKey(OpExecutionInvoke, key)
	if !ok || op.State != OperationFailed {
		t.Fatalf("operation found=%v state=%q, want a Failed operation", ok, op.State)
	}
	var record mutationResult
	if err := json.Unmarshal(op.Result, &record); err != nil {
		t.Fatal(err)
	}
	if record.Mutated {
		t.Fatal("test setup error: a zero-path producer reported a mutation")
	}
	if held := state.heldMaterial(OpExecutionInvoke + attemptsExhaustedSuffix); held != nil {
		t.Fatalf("a non-mutating refusal claims to hold %+v", *held)
	}
	events := journalOf(t, fixture.runtime, runID)
	if n := countType(events, EventCandidateCommitted); n != 0 {
		t.Fatalf("a non-mutating refusal produced %d commit(s)", n)
	}
}

// TestANormalMutatedSuccessIsStillCommittedAndAssured proves the fix is
// scoped to refusals: an invocation that mutates and reports no failure is
// still committed and still reaches assurance, exactly as before.
func TestANormalMutatedSuccessIsStillCommittedAndAssured(t *testing.T) {
	fixture := newPhase8Fixture(t)
	verifier := fixture.verifier()
	runID := fixture.start()
	fixture.reconcile(runID)

	events := journalOf(t, fixture.runtime, runID)
	if n := countType(events, EventCandidateCommitted); n != 1 {
		t.Fatalf("%d candidate.committed event(s), want exactly 1 for a normal mutated success", n)
	}
	if len(verifier.Requests) == 0 {
		t.Fatal("a normal mutated success never reached assurance")
	}
	state := fixture.state(runID)
	if !state.projection.CandidateComplete {
		t.Fatal("a normal mutated success left the candidate incomplete")
	}
}
