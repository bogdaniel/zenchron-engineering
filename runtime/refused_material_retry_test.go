package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// attemptIsolationProvider is deliberately adversarial: attempt one writes A
// and is refused; attempt two fails the test immediately if A is still visible.
// That makes the filesystem boundary itself the assertion rather than inferring
// isolation later from which commit happened to be produced.
type attemptIsolationProvider struct {
	t              *testing.T
	writeOnSuccess bool
	calls          int
	requests       []ExecutionRequest
}

func (p *attemptIsolationProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *attemptIsolationProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.calls++
	p.requests = append(p.requests, request)
	switch p.calls {
	case 1:
		if err := os.WriteFile(filepath.Join(request.CandidateDir, "a.txt"), []byte("refused-A\n"), 0600); err != nil {
			p.t.Fatal(err)
		}
		return ExecutionResult{
			ProviderID: "test-provider",
			Outcome:    OperationFailed,
			Failure: &ProviderFailure{
				Classification:   FailureProviderBackgroundWorkUnresolved,
				RawDiagnosticRef: "attempt-1-background-work",
			},
		}, nil
	case 2:
		if _, err := os.Stat(filepath.Join(request.CandidateDir, "a.txt")); !os.IsNotExist(err) {
			p.t.Fatalf("retry inherited refused A from attempt 1: stat err=%v", err)
		}
		if p.writeOnSuccess {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, "b.txt"), []byte("admitted-B\n"), 0600); err != nil {
				p.t.Fatal(err)
			}
		}
		return ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}, nil
	default:
		p.t.Fatalf("unexpected provider attempt %d", p.calls)
		return ExecutionResult{}, nil
	}
}

func attemptIsolationFixture(t *testing.T, writeOnSuccess bool) (*phase8Fixture, *attemptIsolationProvider) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	provider := &attemptIsolationProvider{t: t, writeOnSuccess: writeOnSuccess}
	fixture.deps.Provider = provider
	fixture.deps.Budgets.MaxExecutionAttempts = 2
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture, provider
}

func firstRefusedMaterial(t *testing.T, events []EngineeringEvent) *RefusedMaterialSnapshot {
	t.Helper()
	for _, event := range events {
		if event.Type != EventOperationAfter {
			continue
		}
		var operation RunOperation
		if err := json.Unmarshal(event.Payload, &operation); err != nil || operation.Kind != OpExecutionInvoke || operation.State != OperationFailed {
			continue
		}
		var record executionRecord
		if err := json.Unmarshal(operation.Result, &record); err != nil {
			t.Fatal(err)
		}
		if record.RefusedMaterial != nil {
			copy := *record.RefusedMaterial
			return &copy
		}
	}
	t.Fatal("no failed execution attempt recorded refused material")
	return nil
}

func assertSameInitialSubject(t *testing.T, provider *attemptIsolationProvider, base string) {
	t.Helper()
	if len(provider.requests) != 2 {
		t.Fatalf("provider requests = %d, want 2", len(provider.requests))
	}
	for i, request := range provider.requests {
		if request.Purpose != InvocationInitial {
			t.Fatalf("attempt %d purpose = %q, want initial retry rather than continuation", i+1, request.Purpose)
		}
		if request.Candidate.Revision != base {
			t.Fatalf("attempt %d subject = %s, want governed base %s", i+1, request.Candidate.Revision, base)
		}
	}
	if provider.requests[0].Candidate.Tree != provider.requests[1].Candidate.Tree {
		t.Fatalf("retry subject tree changed: %s -> %s", provider.requests[0].Candidate.Tree, provider.requests[1].Candidate.Tree)
	}
}

// Binding regression 1 from #390: attempt one creates A and is refused;
// attempt two succeeds without changing anything. A must not become the
// successful attempt's material merely because it was still on disk.
func TestRefusedAttemptAThenCleanSuccessDoesNotPromoteA(t *testing.T) {
	fixture, provider := attemptIsolationFixture(t, false)
	runID := fixture.start()
	fixture.reconcile(runID)

	assertSameInitialSubject(t, provider, fixture.base)
	state := fixture.state(runID)
	if countType(state.events, EventCandidateCommitted) != 0 || countType(state.events, EventCandidateCheckpointed) != 0 {
		t.Fatalf("refused A became candidate material after a clean retry: %v", journalTypes(state.events))
	}
	if countType(state.events, EventAssuranceObserved) != 0 {
		t.Fatalf("assurance ran without an admitted candidate: %v", journalTypes(state.events))
	}
	if _, err := os.Stat(filepath.Join(candidateDir(fixture.stateDir, runID), "a.txt")); !os.IsNotExist(err) {
		t.Fatalf("retry workspace still contains refused A: %v", err)
	}
	refused := firstRefusedMaterial(t, state.events)
	if refused.SubjectCommit != fixture.base || refused.PathCount != 1 || refused.ContentDigest == "" {
		t.Fatalf("refused material identity = %+v", refused)
	}
	if _, err := os.Stat(refusedMaterialBundlePath(fixture.stateDir, refused.ID)); err != nil {
		t.Fatalf("refused A was not preserved outside the retry workspace: %v", err)
	}
}

// Binding regression 2 from #390: attempt one creates A and is refused;
// attempt two creates B and succeeds. The candidate may contain B, but A must
// never be re-attributed to the successful physical attempt.
func TestRefusedAttemptAThenSuccessBCommitsOnlyB(t *testing.T) {
	fixture, provider := attemptIsolationFixture(t, true)
	runID := fixture.start()
	fixture.reconcile(runID)

	assertSameInitialSubject(t, provider, fixture.base)
	state := fixture.state(runID)
	if countType(state.events, EventCandidateCheckpointed) != 0 {
		t.Fatalf("a retry-routed refusal was converted into continuation semantics: %v", journalTypes(state.events))
	}
	if countType(state.events, EventCandidateCommitted) != 1 || state.projection.CandidateRevision == "" || !state.projection.CandidateComplete {
		t.Fatalf("successful B did not become the one complete candidate: projection=%+v events=%v", state.projection, journalTypes(state.events))
	}
	if out, err := gitOutput(candidateDir(fixture.stateDir, runID), "show", state.projection.CandidateRevision+":b.txt"); err != nil || string(out) != "admitted-B\n" {
		t.Fatalf("candidate does not contain admitted B: %q %v", out, err)
	}
	if _, err := gitOutput(candidateDir(fixture.stateDir, runID), "show", state.projection.CandidateRevision+":a.txt"); err == nil {
		t.Fatal("candidate silently attributed refused A to the successful retry")
	}
	refused := firstRefusedMaterial(t, state.events)
	if _, err := os.Stat(refusedMaterialBundlePath(fixture.stateDir, refused.ID)); err != nil {
		t.Fatalf("refused A was deleted instead of preserved: %v", err)
	}
}

// The quarantine is restart-safe independently of a process-local runtime
// object: the durable bundle survives, while rebuilding CandidateWorkspace from
// the original governed identity still observes the exact clean subject.
func TestRefusedMaterialQuarantineSurvivesWorkspaceReconstruction(t *testing.T) {
	fixture := newPhase8Fixture(t)
	workspace, err := CreateCandidateClone(fixture.stateDir, "quarantine-restart", fixture.origin, fixture.base, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject, err := workspace.head()
	if err != nil {
		t.Fatal(err)
	}
	baseline := workspace.TrustedMetadata
	if err := os.WriteFile(filepath.Join(workspace.Dir, "a.txt"), []byte("refused-A\n"), 0600); err != nil {
		t.Fatal(err)
	}
	attempt := ExecutionAttemptRef{RunID: "quarantine-restart", OperationID: "quarantine-restart:execution.invoke:initial", Attempt: 1}
	snapshot, err := workspace.QuarantineRefusedMaterial(fixture.stateDir, attempt, subject)
	if err != nil {
		t.Fatal(err)
	}

	bundlePath := refusedMaterialBundlePath(fixture.stateDir, snapshot.ID)
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bundle)
	if hex.EncodeToString(sum[:]) != snapshot.BundleSHA256 {
		t.Fatalf("bundle digest changed across reconstruction: got %x want %s", sum, snapshot.BundleSHA256)
	}

	// New value, same durable directory: nothing process-local is carried.
	rebuilt := CandidateWorkspace{
		Dir: workspace.Dir, BaseRevision: workspace.BaseRevision,
		TrustedMetadata: baseline, Remote: workspace.Remote,
	}
	if err := rebuilt.AssertIntegrity(); err != nil {
		t.Fatalf("reconstructed workspace rejected the post-quarantine baseline: %v", err)
	}
	head, err := rebuilt.head()
	if err != nil {
		t.Fatal(err)
	}
	if head.Commit != subject.Commit || head.Tree != subject.Tree {
		t.Fatalf("reconstructed workspace subject = %s/%s, want %s/%s", head.Commit, head.Tree, subject.Commit, subject.Tree)
	}
	paths, err := candidateChangedPaths(rebuilt.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 0 {
		t.Fatalf("reconstructed retry workspace is contaminated: %v", paths)
	}
	listed, err := gitOutput(rebuilt.Dir, "bundle", "list-heads", bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(listed), snapshot.Commit) {
		t.Fatalf("preserved bundle does not name refused snapshot commit %s: %s", snapshot.Commit, listed)
	}
}


// A later failed retry that produces nothing must not erase the refused
// material an earlier failed attempt preserved. The run still exhausts its
// ordinary attempt budget; its terminal held-material record simply names what
// remains valuable rather than pretending the earlier bytes disappeared.
func TestRetryExhaustionStillHoldsEarlierRefusedMaterial(t *testing.T) {
	first := providerAnswer{
		result: ExecutionResult{
			ProviderID: "test-provider", Outcome: OperationFailed,
			Failure: &ProviderFailure{Classification: FailureProviderBackgroundWorkUnresolved, RawDiagnosticRef: "first"},
		},
		mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "a.txt"), []byte("refused-A\n"), 0600)
		},
	}
	fixture, _ := newRoutingFixture(t, 2, first, classifiedFailure(FailureTransientProvider))
	runID := fixture.start()
	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Failed || outcome.Reason != OpExecutionInvoke+"_attempts_exhausted" {
		t.Fatalf("outcome = %#v, want ordinary retry-budget exhaustion", outcome)
	}
	state := fixture.state(runID)
	held := state.snapshot.HeldMaterial
	if held == nil || held.Kind != HeldRefused || held.PathCount != 1 || held.ContentDigest == "" {
		t.Fatalf("earlier refused material disappeared at retry exhaustion: %+v", held)
	}
	refused := firstRefusedMaterial(t, state.events)
	if held.Revision != refused.Commit || held.Tree != refused.Tree {
		t.Fatalf("held identity = %s/%s, want refused snapshot %s/%s", held.Revision, held.Tree, refused.Commit, refused.Tree)
	}
	if _, err := os.Stat(refusedMaterialBundlePath(fixture.stateDir, refused.ID)); err != nil {
		t.Fatalf("terminal held material has no preserved bundle: %v", err)
	}
}
