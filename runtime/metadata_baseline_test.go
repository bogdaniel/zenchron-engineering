package runtime

// Phase 9 §0.4: the trusted Git-metadata baseline is durable, so a runtime that
// was stopped and started again still knows what its own .git looked like.
// Everything here is offline: the same fixture the Phase 8 suite uses.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// haltingProvider stops the reconcile loop the moment the producer is reached
// and records every invocation, so "the provider was never invoked" and "no
// provider output was accepted" are both directly observable.
type haltingProvider struct {
	*FakeExecutionProvider
	requests []ExecutionRequest
	cancel   context.CancelFunc
}

func newHaltingProvider() *haltingProvider {
	return &haltingProvider{FakeExecutionProvider: &FakeExecutionProvider{
		Result: ExecutionResult{ProviderID: "halting-provider", Outcome: Succeeded},
	}}
}

func (p *haltingProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *haltingProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	if p.cancel != nil {
		p.cancel()
	}
	return ExecutionResult{}, errors.New("halted: the controller process stopped")
}

// runtimeOn builds a runtime over an independently opened store on the same
// state directory. That is what makes a restart a restart rather than a second
// method call on the same in-memory objects.
func runtimeOn(t *testing.T, fixture *phase8Fixture, provider ExecutionProvider) *EngineeringRuntime {
	t.Helper()
	store, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	deps := fixture.deps
	deps.Store = store
	deps.Provider = provider
	rt, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

func liveMetadata(t *testing.T, dir string) string {
	t.Helper()
	digest, err := gitMetadataDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

// TestTrustedMetadataBaselineSurvivesARestart is the Phase 9 §0.4 scenario end
// to end: create the candidate, close the runtime, tamper with the runtime-owned
// .git while nothing is running, reopen, resume. The resumed runtime must refuse
// with a typed workspace_integrity_violation and must not accept any provider
// output - and it can only know that because the baseline came out of the
// journal, not out of the repository it is being asked to judge.
func TestTrustedMetadataBaselineSurvivesARestart(t *testing.T) {
	fixture := newPhase8Fixture(t)

	// --- first process -----------------------------------------------------
	first := newHaltingProvider()
	before := runtimeOn(t, fixture, first)
	runID, err := before.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first.cancel = cancel
	_, _ = before.Reconcile(ctx, runID)

	dir := candidateDir(fixture.stateDir, runID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the run never created its candidate: %v", err)
	}
	halted, err := before.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	baseline := halted.projection.CandidateMetadata
	if baseline == "" {
		t.Fatal("candidate creation recorded no durable metadata baseline")
	}
	changed := countType(halted.events, EventCandidateChanged)
	committed := countType(halted.events, EventCandidateCommitted)
	if live := liveMetadata(t, dir); baseline != live {
		t.Fatalf("the recorded baseline %q is not the created workspace's metadata %q", baseline, live)
	}

	// --- the process stops -------------------------------------------------
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := before.deps.Store.Close(); err != nil {
		t.Fatal(err)
	}

	// --- tampering, with no runtime running --------------------------------
	// A planted ref is runtime-owned Git metadata that no other guard covers:
	// the repository-config policy vets config, and the recorded-head check
	// only looks at HEAD. Nothing in the worktree changes. It is written
	// straight into .git, because that is what an attacker with the controller
	// stopped actually has - there is no runtime to go through.
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	planted := filepath.Join(dir, ".git", "refs", "remotes", "origin", "attacker")
	if err := os.MkdirAll(filepath.Dir(planted), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planted, []byte(head), 0600); err != nil {
		t.Fatal(err)
	}
	if liveMetadata(t, dir) == baseline {
		t.Fatal("the tamper did not change the runtime-owned Git metadata")
	}

	// --- second process ----------------------------------------------------
	second := newHaltingProvider()
	after := runtimeOn(t, fixture, second)
	resumed, err := after.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := resumed.projection.CandidateMetadata; got != baseline {
		t.Fatalf("the resumed runtime read baseline %q from the journal, want %q", got, baseline)
	}
	workspace, err := after.workspace(resumed)
	if err != nil {
		t.Fatal(err)
	}
	if workspace.TrustedMetadata != baseline {
		t.Fatalf("the rebuilt workspace trusts %q, not the journalled baseline %q", workspace.TrustedMetadata, baseline)
	}
	var violation *WorkspaceIntegrityError
	if err := workspace.AssertIntegrity(); !errors.As(err, &violation) {
		t.Fatalf("want a typed *WorkspaceIntegrityError, got %T: %v", err, err)
	}

	// Resuming must refuse rather than adopt, and must never reach the producer.
	if _, err := after.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	if len(second.requests) != 0 {
		t.Fatalf("the resumed runtime invoked the producer %d times over a tampered workspace", len(second.requests))
	}
	state, err := after.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if countType(state.events, EventCandidateChanged) != changed || countType(state.events, EventCandidateCommitted) != committed {
		t.Fatalf("provider output was accepted after a metadata violation: %v", journalTypes(state.events))
	}
	if !recordedIntegrityViolation(state) {
		t.Fatalf("no operation recorded a workspace_integrity_violation: %v", journalTypes(state.events))
	}
	// The failed operations left the baseline exactly where creation put it.
	if got := state.projection.CandidateMetadata; got != baseline {
		t.Fatalf("a refused operation moved the durable baseline to %q, want %q", got, baseline)
	}
}

// recordedIntegrityViolation reports whether some operation ended with the typed
// violation. The failure text is what operation.after durably carries.
func recordedIntegrityViolation(state *runState) bool {
	for _, op := range state.snapshot.Operations {
		if op.State == OperationFailed && strings.Contains(string(op.Result), "workspace_integrity_violation") {
			return true
		}
	}
	return false
}

// TestFailedGitOperationLeavesNoNewBaseline is the ordering rule. A base
// integration fetches first - which really does move remote-tracking refs and so
// really does change the metadata digest - and then conflicts. The operation
// failed, so no new baseline may be durable, even though the mutation happened.
func TestFailedGitOperationLeavesNoNewBaseline(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	// Stop just after the candidate is committed and assured.
	fixture.crash(runID, func(call GitHubCall) bool {
		return call.Method == "RefSHA" && strings.HasPrefix(call.Ref, "zenchron/")
	})
	state := fixture.state(runID)
	if state.projection.CandidateRevision == "" {
		t.Fatalf("the run never committed a candidate: %v", journalTypes(state.events))
	}
	baseline := state.projection.CandidateMetadata
	if baseline == "" {
		t.Fatal("the committed candidate recorded no durable metadata baseline")
	}

	// The base now adds the same path the candidate added, with other content,
	// so the rebase inside base.integrate conflicts.
	fixture.moveBase("candidate.go", "package other\n")
	driftKey, _ := bindBaseIntegrate(state)
	if !forgetOperation(t, fixture, runID, OpBaseIntegrate, driftKey) {
		t.Fatal("no base drift check to re-run")
	}
	fixture.reconcile(runID)

	after := fixture.state(runID)
	dir := candidateDir(fixture.stateDir, runID)
	if liveMetadata(t, dir) == baseline {
		t.Fatal("the fetch did not change the runtime-owned Git metadata, so this proves nothing")
	}
	if got := after.projection.CandidateMetadata; got != baseline {
		t.Fatalf("a failed base integration established baseline %q; the durable baseline must still be %q", got, baseline)
	}
	if countType(after.events, EventCandidateBaseIntegrated) != 0 {
		t.Fatalf("a conflicting integration journalled an integration event: %v", journalTypes(after.events))
	}
}

// TestProjectionIgnoresABaselineFromAnUnsuccessfulOperation is the same rule at
// the projection seam, where it is enforced: only an operation.after that
// reports success may move the baseline.
func TestProjectionIgnoresABaselineFromAnUnsuccessfulOperation(t *testing.T) {
	operation := func(state OperationState, digest string) RunOperation {
		return RunOperation{
			SchemaVersion: SchemaVersion, ID: "op-" + digest, RunID: "r", Kind: OpCandidateCreate,
			State: state, Result: []byte(`{"dir":"d","base_revision":"b","metadata_digest":"` + digest + `"}`),
		}
	}
	events := []EngineeringEvent{
		projectionEvent(t, 1, EventOperationAfter, operation(Succeeded, "digest-good")),
		projectionEvent(t, 2, EventOperationAfter, operation(OperationFailed, "digest-bad")),
		// An operation that records no digest at all leaves the baseline alone.
		projectionEvent(t, 3, EventOperationAfter, RunOperation{
			SchemaVersion: SchemaVersion, ID: "op-3", RunID: "r", Kind: OpGitHubObserve, State: Succeeded,
		}),
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	if projection.CandidateMetadata != "digest-good" {
		t.Fatalf("CandidateMetadata = %q, want digest-good", projection.CandidateMetadata)
	}
}
