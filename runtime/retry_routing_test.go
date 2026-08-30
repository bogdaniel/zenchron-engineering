package runtime

// retry_routing_test.go pins the retry rule the #29 dogfood run broke: an
// operation is re-attempted only when the class of the failure it recorded
// routes to RouteRetry AND its attempt budget still has room. In the failed run
// a deterministic provider binding error was classified `unknown` and re-run
// three times in under a hundred milliseconds, because `attempt < max_attempts`
// was the only question the runtime asked.
//
// Everything here is driven through the real EngineeringRuntime with the phase 8
// fixture: no network, no real GitHub, no real provider, an injected clock. Every
// assertion is against the PERSISTED JOURNAL - the folded operation document and
// the operation.after events - because the durable record is the contract.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// providerAnswer is one scripted provider outcome: what it reports, what it
// leaves in the candidate workspace, and whether it errors outright.
type providerAnswer struct {
	result ExecutionResult
	err    error
	mutate func(dir string) error
}

// routedProvider replays scripted answers in order and repeats the last one
// forever, so "how many times was the provider actually invoked" is an exact,
// deterministic number rather than a timing observation.
type routedProvider struct {
	answers []providerAnswer
	calls   int
}

func (p *routedProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *routedProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	answer := p.answers[len(p.answers)-1]
	if p.calls < len(p.answers) {
		answer = p.answers[p.calls]
	}
	p.calls++
	if answer.mutate != nil {
		if err := answer.mutate(request.CandidateDir); err != nil {
			return ExecutionResult{}, err
		}
	}
	return answer.result, answer.err
}

func writesCandidate(dir string) error {
	return os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate\n"), 0600)
}

// classifiedFailure is a provider that reports a failure of a known class and
// changes nothing, which is the shape the failed run produced.
func classifiedFailure(class FailureClass) providerAnswer {
	return providerAnswer{result: ExecutionResult{
		ProviderID: "test-provider",
		Outcome:    OperationFailed,
		Failure:    &ProviderFailure{Classification: class, RawDiagnosticRef: "diagnostic-" + string(class)},
	}}
}

// newRoutingFixture builds a phase 8 run whose provider is scripted and whose
// execution attempt budget is set explicitly, so "budget remained" is never the
// reason a test observes one attempt.
func newRoutingFixture(t *testing.T, maxAttempts int, answers ...providerAnswer) (*phase8Fixture, *routedProvider) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	provider := &routedProvider{answers: answers}
	fixture.deps.Provider = provider
	fixture.deps.Budgets.MaxExecutionAttempts = maxAttempts
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture, provider
}

// durableInvoke returns the journal's folded record for the run's single
// execution.invoke operation together with the number of terminal outcomes the
// journal holds for it. Both come from the persisted events, not from a counter
// the test kept.
func durableInvoke(t *testing.T, fixture *phase8Fixture, runID string) (RunOperation, int) {
	t.Helper()
	state := fixture.state(runID)
	var found RunOperation
	seen := 0
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpExecutionInvoke {
			found, seen = op, seen+1
		}
	}
	if seen != 1 {
		t.Fatalf("journal holds %d execution.invoke operations, want exactly one: %v", seen, journalTypes(state.events))
	}
	outcomes := 0
	for _, e := range state.events {
		if e.Type == EventOperationAfter && e.OperationID == found.ID {
			outcomes++
		}
	}
	return found, outcomes
}

func durableFailureClass(t *testing.T, op RunOperation) FailureClass {
	t.Helper()
	var result mutationResult
	if err := decodeJSON(op.Result, &result); err != nil {
		t.Fatalf("durable result of %s is not readable: %v (%s)", op.ID, err, op.Result)
	}
	return result.FailureClass
}

// TestDeterministicBindingFailureExecutesExactlyOnce is the observed regression
// itself. The provider refuses the request before producing any result - the
// exact shape of `OpenAIProvider.Execute` returning
// (ExecutionResult{}, "incomplete execution request binding") - and the budget
// is deliberately three, the budget the failed run had. Retrying a request the
// provider rejected deterministically cannot change the answer, so it must not
// happen: one attempt, then terminal.
func TestDeterministicBindingFailureExecutesExactlyOnce(t *testing.T) {
	fixture, provider := newRoutingFixture(t, 3, providerAnswer{err: errors.New("incomplete execution request binding")})
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	op, outcomes := durableInvoke(t, fixture, runID)
	if op.Attempt != 1 || outcomes != 1 {
		t.Fatalf("durable record shows %d attempts and %d outcomes of %d permitted, want exactly one of each",
			op.Attempt, outcomes, op.MaxAttempts)
	}
	if op.MaxAttempts != 3 {
		t.Fatalf("the budget under test is %d, not the 3 the failed run had", op.MaxAttempts)
	}
	if provider.calls != 1 {
		t.Fatalf("the provider was invoked %d times for one deterministic refusal", provider.calls)
	}
	if class := durableFailureClass(t, op); RouteFailure(class) == RouteRetry {
		t.Fatalf("class %q routes to retry, so this test proves nothing about withholding one", class)
	}
	if outcome.Disposition != Failed || outcome.Reason != OpExecutionInvoke+"_failure_not_retryable" {
		t.Fatalf("outcome = %#v, want the run failed because the failure does not route to a retry", outcome)
	}
}

// TestUnknownFailureFailsClosed is the fails-closed case. `unknown` is what the
// failed run durably recorded, and RouteFailure sends it to RouteStop, so the
// run stops on the first attempt with its diagnostic intact rather than
// repeating an unexplained failure until a counter runs out.
func TestUnknownFailureFailsClosed(t *testing.T) {
	if RouteFailure(FailureUnknown) != RouteStop {
		t.Fatalf("this test depends on FailureUnknown routing to stop, not %q", RouteFailure(FailureUnknown))
	}
	fixture, provider := newRoutingFixture(t, 3, classifiedFailure(FailureUnknown))
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	op, outcomes := durableInvoke(t, fixture, runID)
	if op.Attempt != 1 || outcomes != 1 || provider.calls != 1 {
		t.Fatalf("an unknown failure ran %d attempts / %d outcomes / %d provider calls, want one of each",
			op.Attempt, outcomes, provider.calls)
	}
	if class := durableFailureClass(t, op); class != FailureUnknown {
		t.Fatalf("durable failure class = %q, want the diagnostic preserved as %q", class, FailureUnknown)
	}
	if outcome.Disposition != Failed {
		t.Fatalf("outcome = %#v, want the run to fail closed", outcome)
	}
}

// TestTransientProviderFailureIsRetried is the other half of the rule: a class
// that DOES route to retry still retries, so this repair withholds retries
// rather than removing them.
func TestTransientProviderFailureIsRetried(t *testing.T) {
	fixture, provider := newRoutingFixture(t, 3,
		classifiedFailure(FailureTransientProvider),
		providerAnswer{
			result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
			mutate: writesCandidate,
		},
	)
	runID := fixture.start()
	fixture.reconcile(runID)

	op, outcomes := durableInvoke(t, fixture, runID)
	if op.Attempt != 2 || outcomes != 2 || provider.calls != 2 {
		t.Fatalf("a transient failure ran %d attempts / %d outcomes / %d provider calls, want two of each",
			op.Attempt, outcomes, provider.calls)
	}
	state := fixture.state(runID)
	if countType(state.events, EventCandidateCommitted) != 1 {
		t.Fatalf("the retried invocation never produced a committed candidate: %v", journalTypes(state.events))
	}
}

// TestRetryBudgetStillCapsRetryableFailures proves the second condition is
// still load-bearing: a route that says retry does not retry forever.
func TestRetryBudgetStillCapsRetryableFailures(t *testing.T) {
	fixture, provider := newRoutingFixture(t, 2, classifiedFailure(FailureTransientInfrastructure))
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	op, outcomes := durableInvoke(t, fixture, runID)
	if op.Attempt != 2 || outcomes != 2 || provider.calls != 2 {
		t.Fatalf("a retryable failure ran %d attempts / %d outcomes / %d provider calls under a budget of 2",
			op.Attempt, outcomes, provider.calls)
	}
	if outcome.Disposition != Failed || outcome.Reason != OpExecutionInvoke+"_attempts_exhausted" {
		t.Fatalf("outcome = %#v, want the run failed with its retry budget exhausted", outcome)
	}
}
