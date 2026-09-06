package runtime

// Provider capacity is not an engineering failure.
//
// Several concurrent workers sharing one subscription is the ordinary shape of
// this product, so hitting that subscription's allowance is ordinary too. What
// must never happen is for it to spend a run's remediation budget, reset a
// candidate, or change anything about the evidence and authority the run had
// already established: nothing about the work went wrong, the allowance simply
// ran out and will come back.

import (
	"context"
	"testing"
)

// quotaProvider refuses at its own capacity boundary before doing any work,
// which is exactly what a subscription CLI does when its window is spent.
type quotaProvider struct {
	class    FailureClass
	requests int
}

func (p *quotaProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *quotaProvider) Execute(context.Context, ExecutionRequest) (ExecutionResult, error) {
	p.requests++
	return ExecutionResult{
		ProviderID: "codex", Outcome: OperationFailed,
		Failure: &ProviderFailure{Classification: p.class},
	}, nil
}

// TestProviderCapacityWaitsWithoutSpendingAnEngineeringAttempt is the law for
// both capacity classes, asserted through a real run.
func TestProviderCapacityWaitsWithoutSpendingAnEngineeringAttempt(t *testing.T) {
	for _, tc := range []struct {
		class  FailureClass
		reason string
	}{
		{FailureProviderQuota, "execution_provider_quota"},
		{FailureProviderRateLimited, "execution_provider_rate_limited"},
	} {
		t.Run(string(tc.class), func(t *testing.T) {
			// The two are kept apart on purpose: a quota returns on the
			// provider's own schedule, while repeated rate limiting means the
			// configured concurrency is above what that account tolerates, and
			// an operator cannot see that difference through one merged class.
			if RouteFailure(tc.class) != RouteWait {
				t.Fatalf("%s routes to %q, want a wait", tc.class, RouteFailure(tc.class))
			}
			if waitReason(tc.class) != tc.reason {
				t.Fatalf("%s wait reason = %q, want %q", tc.class, waitReason(tc.class), tc.reason)
			}

			fixture := newPhase8Fixture(t)
			provider := &quotaProvider{class: tc.class}
			deps := fixture.deps
			deps.Provider = provider
			deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
			engine := fixture.newRuntime(deps)

			runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := engine.Reconcile(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Disposition != Waiting || outcome.Reason != tc.reason {
				t.Fatalf("outcome = %#v, want a %s wait", outcome, tc.reason)
			}
			if provider.requests != 1 {
				t.Fatalf("the provider was called %d times; a capacity wait performs no immediate retry", provider.requests)
			}

			state, err := engine.load(runID)
			if err != nil {
				t.Fatal(err)
			}
			// The attempt is GIVEN BACK. Without that, repeated passes over an
			// external condition the runtime cannot fix would exhaust the
			// budget meant for failures of the work itself.
			operation := executionOperation(t, fixture.store, runID)
			if operation.Attempt > 1 {
				t.Fatalf("a capacity wait consumed engineering attempt %d", operation.Attempt)
			}
			if countType(state.events, EventRunFailed) != 0 {
				t.Fatalf("provider capacity failed the run: %v", journalTypes(state.events))
			}
			// Nothing about the candidate, the evidence or the authority moved.
			if state.projection.CandidateRevision != "" {
				t.Fatalf("a capacity wait produced a candidate: %q", state.projection.CandidateRevision)
			}
			if len(state.projection.EvidenceBundles) != 0 {
				t.Fatalf("a capacity wait changed the evidence: %#v", state.projection.EvidenceBundles)
			}
			if len(state.projection.AuthorityDecisions) != 0 {
				t.Fatalf("a capacity wait changed an authority decision: %#v", state.projection.AuthorityDecisions)
			}
			// And it is visible: an operator can see WHICH shared worker is the
			// constrained resource rather than a generic stall.
			if diagnostic := state.projection.ExecutionDiagnostic; diagnostic == nil || diagnostic.FailureClass != tc.class {
				t.Fatalf("the constrained provider is not visible in status: %#v", diagnostic)
			}
		})
	}
}

// TestUnrecognizedProviderDiagnosticsAreNotGuessedIntoCapacity keeps the
// classification narrow. Inventing a wait out of an unknown error is how a
// terminal fault becomes an endless one.
func TestUnrecognizedProviderDiagnosticsAreNotGuessedIntoCapacity(t *testing.T) {
	for _, diagnostic := range []string{
		"permission denied",
		"unexpected end of JSON input",
		"the model produced an invalid patch",
		"quota", // a bare word is not one of the recognized signals
	} {
		if got := classifyAgentFailure(codexSpec, []byte(diagnostic), nil); got == FailureProviderQuota || got == FailureProviderRateLimited {
			t.Fatalf("%q was guessed into %q", diagnostic, got)
		}
	}
}
