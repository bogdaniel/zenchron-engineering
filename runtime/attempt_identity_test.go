package runtime

// #236: a provider retry reused an immutable transcript identity.
//
// A run met an explicit usage limit, the runtime correctly kept it as an
// external wait rather than consuming it, and the operator resumed once the
// allowance returned. The second physical invocation of the same logical
// operation then addressed the FIRST one's transcript slot, the evidence store
// refused to overwrite it - correctly - and a run that was supposed to be
// resumable could never resume.
//
// The cause was an identity taken from the wrong counter. The operation's
// attempt is a BUDGET, and a wait-routed refusal gives it back, because
// observing an account limit is not work the attempt ceiling should pay for.
// That makes it deliberately non-monotonic, and a create-once transcript needs
// an identity that only ever moves forward.
//
// These tests pin the distinction from both ends: the same logical binding and
// the same cumulative budget across two attempts, and two different immutable
// transcripts.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// transcriptProvider writes its transcript through the real evidence store, so
// the create-once rule is exercised rather than described. A provider that only
// reported an attempt number could not reproduce this defect at all: the
// refusal came from the store, not from the classification.
type transcriptProvider struct {
	artifacts ArtifactStore
	mutate    func(dir string) error
	attempts  []int
	stored    []string
	failures  int
}

func (p *transcriptProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *transcriptProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.attempts = append(p.attempts, request.Attempt)
	quota := len(p.attempts) <= p.failures
	stdout := []byte("candidate updated\n")
	if quota {
		stdout = []byte("You've hit your usage limit.\n")
	}
	// The identity the runtime handed over, filed exactly as a real CLI adapter
	// files it. If the runtime reused a slot, this is where it is refused.
	artifacts, err := p.artifacts.StoreExecutionAttemptTranscript("codex", request.AttemptRef(), stdout, nil)
	if err != nil {
		return ExecutionResult{}, err
	}
	p.stored = append(p.stored, artifacts[0].Path)
	if quota {
		return ExecutionResult{
			ProviderID: "codex", Attempt: request.Attempt, Outcome: OperationFailed,
			Artifacts: artifacts,
			Failure:   &ProviderFailure{Classification: FailureProviderQuota, RawDiagnosticRef: artifacts[0].Path},
		}, nil
	}
	if p.mutate != nil {
		if err := p.mutate(request.CandidateDir); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{
		ProviderID: "codex", Attempt: request.Attempt, Outcome: Succeeded, Artifacts: artifacts,
	}, nil
}

func quotaThenSuccessFixture(t *testing.T) (*phase8Fixture, *transcriptProvider, Dependencies) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	provider := &transcriptProvider{
		artifacts: fixture.deps.Artifacts,
		failures:  1,
		mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "README.md"), []byte("resumed after the allowance returned\n"), 0o600)
		},
	}
	deps := fixture.deps
	deps.Provider = provider
	deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	return fixture, provider, deps
}

// TestAProviderRetryGetsItsOwnTranscriptIdentity is the live failure, reproduced
// and then required not to happen.
func TestAProviderRetryGetsItsOwnTranscriptIdentity(t *testing.T) {
	fixture, provider, deps := quotaThenSuccessFixture(t)
	engine := fixture.newRuntime(deps)

	runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := engine.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Disposition != Waiting || outcome.Reason != "execution_provider_quota" {
		t.Fatalf("first pass = %#v, want a provider quota wait", outcome)
	}
	first := executionOperation(t, fixture.store, runID)
	firstConsumed := first.ConsumedExecution
	if len(provider.stored) != 1 {
		t.Fatalf("the first invocation stored %d transcripts, want exactly one", len(provider.stored))
	}
	firstPath := provider.stored[0]
	// What the run had EARNED at the moment the allowance ran out: nothing. A
	// retry must not be the thing that changes this.
	waiting := fixture.state(runID)
	if len(waiting.projection.AuthorityDecisions) != 0 || len(waiting.projection.EvidenceBundles) != 0 {
		t.Fatalf("the quota wait produced authority or evidence: %#v / %#v",
			waiting.projection.AuthorityDecisions, waiting.projection.EvidenceBundles)
	}
	firstBytes, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatal(err)
	}

	// THE RESUME. Before the repair this returned operation_refused, because
	// the second invocation addressed the first one's transcript.
	resumed, err := engine.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatalf("resume after a provider quota wait failed: %v", err)
	}
	if len(provider.attempts) < 2 {
		t.Fatalf("the provider was invoked %d time(s); a resumed wait must reach it again: %#v", len(provider.attempts), resumed)
	}

	// Two physical invocations, two identities, and the first one untouched.
	if provider.attempts[0] == provider.attempts[1] {
		t.Fatalf("both invocations claimed attempt %d; a retry must not reuse an attempt identity", provider.attempts[0])
	}
	if provider.attempts[1] <= provider.attempts[0] {
		t.Fatalf("attempt identity went backwards: %v", provider.attempts)
	}
	if len(provider.stored) != 2 || provider.stored[0] == provider.stored[1] {
		t.Fatalf("transcripts did not separate: %v", provider.stored)
	}
	again, err := os.ReadFile(firstPath)
	if err != nil {
		t.Fatalf("the first transcript is no longer readable: %v", err)
	}
	if string(again) != string(firstBytes) {
		t.Fatalf("the first transcript changed: %q -> %q", firstBytes, again)
	}
	if _, err := os.Stat(provider.stored[1]); err != nil {
		t.Fatalf("the second transcript was not stored: %v", err)
	}

	// Same run, same logical binding, and a budget that moved forward rather
	// than starting over.
	second := executionOperation(t, fixture.store, runID)
	if second.ID != first.ID || second.IdempotencyKey != first.IdempotencyKey {
		t.Fatalf("the retry became a different logical operation: %q/%q -> %q/%q",
			first.ID, first.IdempotencyKey, second.ID, second.IdempotencyKey)
	}
	if second.AttemptIdentity <= first.AttemptIdentity {
		t.Fatalf("the physical attempt identity did not advance: %d -> %d", first.AttemptIdentity, second.AttemptIdentity)
	}
	if second.ConsumedExecution < firstConsumed {
		t.Fatalf("the active budget reset: %s -> %s", firstConsumed, second.ConsumedExecution)
	}
	// The WAIT itself is still free. #232's law survives the repair: only the
	// attempt ceiling is refunded, and waiting adds nothing to the clock.
	if first.Attempt > 1 {
		t.Fatalf("the quota wait spent engineering attempt %d", first.Attempt)
	}
	state := fixture.state(runID)
	if countType(state.events, EventRunFailed) != 0 {
		t.Fatalf("a resumable provider wait failed the run: %v", journalTypes(state.events))
	}
	// Authority did not come from the retry. Whatever the resumed run went on
	// to earn, it earned against the candidate the second invocation actually
	// produced - the exact-subject law, which a retry may not shortcut.
	for action, decision := range state.projection.AuthorityDecisions {
		if !strings.Contains(decision.Decision.ID, state.projection.CandidateRevision) {
			t.Fatalf("authority for %q is not bound to the resumed candidate %q: %#v",
				action, state.projection.CandidateRevision, decision.Decision)
		}
	}
}

// TestARestartBetweenAttemptsComputesTheSameNextIdentity is crash boundary B and
// C together: the wait is durable, the process is gone, and the next invocation
// still has to land on a free slot deterministically.
func TestARestartBetweenAttemptsComputesTheSameNextIdentity(t *testing.T) {
	fixture, provider, deps := quotaThenSuccessFixture(t)
	engine := fixture.newRuntime(deps)

	runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if outcome, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	} else if outcome.Disposition != Waiting {
		t.Fatalf("first pass = %#v, want a wait", outcome)
	}
	firstAttempt := provider.attempts[0]

	// THE RESTART. A new runtime over the same durable state, exactly as a
	// supervisor coming back up reads it.
	restarted := fixture.newRuntime(deps)
	if _, err := restarted.Reconcile(context.Background(), runID); err != nil {
		t.Fatalf("resume after restart failed: %v", err)
	}
	if len(provider.attempts) < 2 {
		t.Fatalf("the restarted runtime never reached the provider: %v", provider.attempts)
	}
	if provider.attempts[1] <= firstAttempt {
		t.Fatalf("a restart reused or rewound the attempt identity: %v", provider.attempts)
	}
	if provider.stored[0] == provider.stored[1] {
		t.Fatalf("a restart reused the transcript slot: %v", provider.stored)
	}
}

// TestTheEvidenceStoreStillRefusesTheSameIdentityTwice keeps the invariant the
// repair is NOT allowed to relax. The lifecycle must avoid reaching this
// refusal; the refusal itself stays exactly as strict as it was.
func TestTheEvidenceStoreStillRefusesTheSameIdentityTwice(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	ref := ExecutionAttemptRef{RunID: "run-236", OperationID: "execution.invoke#initial|1|base", Attempt: 1}
	if _, err := store.StoreExecutionAttemptTranscript("codex", ref, []byte("first\n"), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreExecutionAttemptTranscript("codex", ref, []byte("second\n"), nil); err == nil {
		t.Fatal("the evidence store overwrote an existing attempt transcript")
	}
}

// crashingProvider begins an invocation and produces NO transcript, which is
// the crash this test is about: the dispatch happened, so the identity is
// spent, and the evidence that would prove it is exactly what is missing.
type crashingProvider struct {
	attempts []int
	crashes  int
}

func (p *crashingProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *crashingProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.attempts = append(p.attempts, request.Attempt)
	if len(p.attempts) <= p.crashes {
		// Dispatched, and then nothing. No transcript is stored, so the slot
		// this invocation claimed still LOOKS free to the evidence store.
		return ExecutionResult{
			ProviderID: "codex", Attempt: request.Attempt, Outcome: OperationFailed,
			Failure: &ProviderFailure{Classification: FailureProviderQuota},
		}, nil
	}
	return ExecutionResult{ProviderID: "codex", Attempt: request.Attempt, Outcome: Succeeded}, nil
}

// TestAnIdentityIsReservedBeforeDispatchSoACrashCannotReuseIt is the crash hole
// the first repair left open.
//
// The identity is decided by two durable facts and the later one wins: what the
// scheduler allocated, and what the evidence says is free. When the evidence is
// AHEAD - a record written before the identity was tracked, or one already
// stranded on an occupied slot - the selected identity is higher than the one on
// the record. Dispatching under it without writing it down first is one crash
// away from handing the same identity out twice: the counter is still behind,
// the slot still looks free, and the next invocation claims it.
//
// So the selection is reserved durably BEFORE the provider is dispatched, and
// the next invocation after a crash is strictly later even though the crashed
// one left no evidence behind.
func TestAnIdentityIsReservedBeforeDispatchSoACrashCannotReuseIt(t *testing.T) {
	fixture := newPhase8Fixture(t)
	provider := &crashingProvider{crashes: 1}
	deps := fixture.deps
	deps.Provider = provider
	deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	engine := fixture.newRuntime(deps)

	runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	// The operation has to exist before its evidence can be ahead of it, so the
	// first pass is what creates it. It crashes without a transcript.
	if _, err := engine.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	operation := executionOperation(t, fixture.store, runID)
	if len(provider.attempts) != 1 {
		t.Fatalf("the provider was invoked %d time(s), want one", len(provider.attempts))
	}

	// EVIDENCE AHEAD OF THE RECORD, written the way a controller that predates
	// the identity would have written it: a transcript exists at the identity
	// the record is sitting on.
	seeded := ExecutionAttemptRef{RunID: runID, OperationID: operation.ID, Attempt: operation.AttemptIdentity}
	if _, err := deps.Artifacts.StoreExecutionAttemptTranscript("codex", seeded, []byte("written by an earlier controller\n"), nil); err != nil {
		t.Fatal(err)
	}

	// THE CRASHING DISPATCH. The identity it receives must be past the seeded
	// evidence, and must be durable before the provider is reached.
	restarted := fixture.newRuntime(deps)
	provider.crashes = 2
	if _, err := restarted.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(provider.attempts) < 2 {
		t.Fatalf("the provider was not reached again: %v", provider.attempts)
	}
	crashed := provider.attempts[1]
	if crashed <= seeded.Attempt {
		t.Fatalf("the invocation landed on or behind seeded evidence: got %d, seeded %d", crashed, seeded.Attempt)
	}
	reserved := executionOperation(t, fixture.store, runID)
	if reserved.AttemptIdentity < crashed {
		t.Fatalf("identity %d was dispatched but only %d was reserved durably; a crash here reuses it",
			crashed, reserved.AttemptIdentity)
	}

	// THE RESTART. The crashed invocation left no transcript, so the evidence
	// store still reports its slot free. Only the reservation stops it being
	// handed out a second time.
	afterCrash := fixture.newRuntime(deps)
	if _, err := afterCrash.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(provider.attempts) < 3 {
		t.Fatalf("the restarted runtime never reached the provider: %v", provider.attempts)
	}
	if next := provider.attempts[2]; next <= crashed {
		t.Fatalf("a crash without a transcript let identity %d be reused: %v", crashed, provider.attempts)
	}

	// Same logical operation, and nothing about the budget started over.
	final := executionOperation(t, fixture.store, runID)
	if final.ID != operation.ID || final.IdempotencyKey != operation.IdempotencyKey {
		t.Fatalf("the crash changed the logical operation: %q -> %q", operation.ID, final.ID)
	}
	if final.ConsumedExecution < operation.ConsumedExecution {
		t.Fatalf("the execution budget reset across the crash: %s -> %s",
			operation.ConsumedExecution, final.ConsumedExecution)
	}
	if final.AttemptIdentity <= reserved.AttemptIdentity-1 {
		t.Fatalf("the identity did not advance across the crash: %d", final.AttemptIdentity)
	}
}
