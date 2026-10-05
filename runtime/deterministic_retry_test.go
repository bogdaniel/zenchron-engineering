package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

func failedOperation(t *testing.T, f *phase8Fixture, id, kind string) RunOperation {
	t.Helper()
	var found []RunOperation
	for _, op := range f.state(id).snapshot.Operations {
		if op.Kind == kind && op.State == OperationFailed {
			found = append(found, op)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d failed %s operations, want one", len(found), kind)
	}
	return found[0]
}

func requireOneDeterministicFailure(t *testing.T, f *phase8Fixture, id, kind, code string) RunOperation {
	t.Helper()
	op := failedOperation(t, f, id, kind)
	if op.MaxAttempts != 3 || op.Attempt != 1 || op.AttemptIdentity != 1 {
		t.Fatalf("%s spent %d budget / %d physical attempts of %d; want one of three", kind, op.Attempt, op.AttemptIdentity, op.MaxAttempts)
	}
	if op.Failure == nil || op.Failure.Classification != deterministicLocal || op.Failure.Code != code || op.Failure.BindingScope != deterministicBindingScope ||
		!isSHA256Hex(op.Failure.Signature) || !isSHA256Hex(op.Failure.BindingSHA256) {
		t.Fatalf("failure identity %+v", op.Failure)
	}
	outcomes := 0
	for _, event := range f.state(id).events {
		if event.Type == EventOperationAfter && event.OperationID == op.ID {
			outcomes++
		}
	}
	if outcomes != 1 {
		t.Fatalf("%d journalled outcomes, want one", outcomes)
	}
	return op
}

// The original shape: a real runtime commit exists but a deterministic local
// cleanliness check fails. Three attempts are available. No clock or prose
// comparison decides whether to spend them, and both the commit and residue
// remain preserved.
func TestIdenticalDeterministicCommitFailureSpendsOneAttempt(t *testing.T) {
	f := newPhase8Fixture(t)
	withAfterCommitUpdateRef(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "residue.go"), []byte("package residue\n"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	id := f.start()
	out := f.reconcile(id)
	if out.Disposition != Waiting || out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("outcome %+v", out)
	}
	op := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.residue")
	var failure commitFailure
	if err := decodeJSON(op.Result, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.RuntimeCommit == nil {
		t.Fatal("the runtime commit was lost")
	}
	head, tree, count := runtimeCommitsPastBase(t, f, id)
	if count != "1" || head != failure.RuntimeCommit.Commit || tree != failure.RuntimeCommit.Tree {
		t.Fatalf("commit %s/%s (%s), record %+v", head, tree, count, failure.RuntimeCommit)
	}
	if data, err := os.ReadFile(filepath.Join(candidateDir(f.stateDir, id), "residue.go")); err != nil || string(data) != "package residue\n" {
		t.Fatalf("residue was not preserved: %q / %v", data, err)
	}
	for range 3 {
		f.clock.at = f.clock.at.Add(time.Hour)
		out = f.reconcile(id)
		if out.Reason != ReasonDeterministicFailureUnchanged {
			t.Fatalf("clock changed the retry decision: %+v", out)
		}
	}
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.residue")
	notPublished(t, f, id)
}

// #337's shape uses different rule and requirement ids: conflicting observed
// definitions of one requirement. It is reached only after the worker produced
// a real checkpoint; no special case for a commit, rule, id or error string.
func conflictingObservedPolicy(f *phase8Fixture) {
	for id, rule := range f.deps.Policy.Rules {
		requirements := map[string]domain.PolicyRequirement{
			"regression-evidence": {Statement: "protect the original boundary", RequiredClaims: &[]string{"verification"}},
		}
		rule.Effect.Obligations = &requirements
		f.deps.Policy.Rules[id] = rule
	}
	requirements := map[string]domain.PolicyRequirement{
		"regression-evidence": {Statement: "a contradictory definition", RequiredClaims: &[]string{"verification"}},
	}
	f.deps.Policy.Rules["observed-conflict"] = domain.PolicyRule{
		When:   domain.PolicyCondition{Fact: "service.boundary_modified", Equals: domain.FactFalse},
		Effect: domain.PolicyEffect{Obligations: &requirements},
	}
	f.runtime = f.newRuntime(f.deps)
}

func checkpointConflictFixture(t *testing.T) (*phase8Fixture, string) {
	t.Helper()
	f, _ := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed,
			Failure: &ProviderFailure{Classification: FailureExecutionIncomplete}},
		mutate: writesCandidate,
	})
	conflictingObservedPolicy(f)
	return f, f.start()
}

func TestDeterministicPolicyConflictPreservesCheckpointWithoutRetryStorm(t *testing.T) {
	f, id := checkpointConflictFixture(t)
	out := f.reconcile(id)
	if out.Disposition != Waiting || out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("outcome %+v", out)
	}
	op := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	var failure commitFailure
	if err := decodeJSON(op.Result, &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Stage != commitStageObservation || failure.RuntimeCommit == nil || !failure.RuntimeCommit.Checkpoint {
		t.Fatalf("lost checkpoint or conflict stage: %+v", failure)
	}
	head, tree, count := runtimeCommitsPastBase(t, f, id)
	if count != "1" || head != failure.RuntimeCommit.Commit || tree != failure.RuntimeCommit.Tree {
		t.Fatalf("checkpoint not preserved: %s/%s (%s)", head, tree, count)
	}
	// Uncommitted scratch and index refreshes cannot repair a contradiction
	// compiled against this exact committed subject.
	dir := candidateDir(f.stateDir, id)
	if err := os.WriteFile(filepath.Join(dir, "irrelevant-scratch"), []byte("scratch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "update-index", "--refresh")
	if out := f.reconcile(id); out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("scratch changed the policy retry decision: %+v", out)
	}
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	notPublished(t, f, id)
}

// The same law applies before a candidate exists, through contract.compile.
func TestIdenticalDeterministicContractFailureSpendsOneAttempt(t *testing.T) {
	f := newPhase8Fixture(t)
	rule := f.deps.Policy.Rules["service-unknown"]
	deny := []domain.Action{{Type: PublicationActionType, Target: f.branch}}
	rule.Effect.Prohibitions = &deny
	f.deps.Policy.Rules["service-unknown"] = rule
	f.runtime = f.newRuntime(f.deps)
	id := f.start()
	out := f.reconcile(id)
	if out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("outcome %+v", out)
	}
	requireOneDeterministicFailure(t, f, id, OpContractCompile, "policy.compile")
	if _, err := os.Stat(candidateDir(f.stateDir, id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate created: %v", err)
	}
}

func TestRelevantCandidateChangeEnablesAnotherDeterministicAttempt(t *testing.T) {
	f, _ := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
		mutate: func(dir string) error { return os.WriteFile(filepath.Join(dir, ".env"), []byte("fixture\n"), 0600) },
	})
	id := f.start()
	if out := f.reconcile(id); out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("outcome %+v", out)
	}
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.sensitive_path")
	dir := candidateDir(f.stateDir, id)
	if err := os.Remove(filepath.Join(dir, ".env")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	after := f.state(id).snapshot.Operations[before.ID]
	if after.Attempt != 2 || after.State != Succeeded || after.Failure != nil {
		t.Fatalf("repaired candidate did not enable recovery: %+v", after)
	}
}

func TestCandidateOnlyFailureDoesNotRetryAfterPolicyOrModelChanges(t *testing.T) {
	f, _ := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
		mutate: func(dir string) error { return os.WriteFile(filepath.Join(dir, ".env"), []byte("fixture\n"), 0600) },
	})
	id := f.start()
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.sensitive_path")
	f.deps.Policy.Revision = "irrelevant-revision"
	rule := f.deps.Policy.Rules["service-unknown"]
	deny := []domain.Action{{Type: "unrelated", Target: "unrelated"}}
	rule.Effect.Prohibitions = &deny
	f.deps.Policy.Rules["service-unknown"] = rule
	f.deps.ProjectModel.Revision = "irrelevant-model"
	reopen(t, f)
	if out := f.reconcile(id); out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("unrelated policy/model renewed candidate retry: %+v", out)
	}
	after := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.sensitive_path")
	if !reflect.DeepEqual(before.Failure, after.Failure) {
		t.Fatalf("unrelated policy/model changed failure identity: %+v / %+v", before.Failure, after.Failure)
	}
	if err := os.WriteFile(filepath.Join(candidateDir(f.stateDir, id), ".env"), []byte("different opaque fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.sensitive_path")
	// The actual name repair, under the same changed policy, remains eligible.
	dir := candidateDir(f.stateDir, id)
	if err := os.Rename(filepath.Join(dir, ".env"), filepath.Join(dir, "candidate.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	op := f.state(id).snapshot.Operations[before.ID]
	if op.Attempt != 2 || op.State != Succeeded || op.Failure != nil {
		t.Fatalf("candidate repair did not renew retry: %+v", op)
	}
}

func TestIndexFlagFailureRequiresFlagChange(t *testing.T) {
	f, _ := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
		mutate: func(dir string) error {
			if err := writesCandidate(dir); err != nil {
				return err
			}
			_, err := runGit(dir, "update-index", "--assume-unchanged", "README.md")
			return err
		},
	})
	id := f.start()
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.index_flags")
	f.deps.Policy.Revision = "unrelated"
	reopen(t, f)
	dir := candidateDir(f.stateDir, id)
	if err := os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate // changed bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.index_flags")
	mustGit(t, dir, "update-index", "--no-assume-unchanged", "README.md")
	f.reconcile(id)
	if op := f.state(id).snapshot.Operations[before.ID]; op.Attempt != 2 || op.State != Succeeded {
		t.Fatalf("flag repair did not enable retry: %+v", op)
	}
}

func TestStagedCredentialFailureIgnoresChangesOutsideItsSubject(t *testing.T) {
	f, _ := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
		mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "leaked.txt"), []byte(githubClassicTokenValue()), 0600)
		},
	})
	id := f.start()
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.staged_credential_value")
	if string(before.Failure.CheckPath) != "leaked.txt" {
		t.Fatalf("typed check subject %+v", before.Failure)
	}
	f.deps.Policy.Revision = "unrelated"
	reopen(t, f)
	dir := candidateDir(f.stateDir, id)
	if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("unrelated bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	mustGit(t, dir, "update-index", "--chmod=+x", "leaked.txt")
	f.reconcile(id)
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.staged_credential_value")
	if err := os.WriteFile(filepath.Join(dir, "leaked.txt"), []byte("ordinary safe fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	if op := f.state(id).snapshot.Operations[before.ID]; op.Attempt != 2 || op.State != Succeeded {
		t.Fatalf("credential subject repair did not enable retry: %+v", op)
	}
}

func TestResidueFailureRequiresResidueRepair(t *testing.T) {
	f := newPhase8Fixture(t)
	withAfterCommitUpdateRef(t, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "residue.go"), []byte("package residue\n"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	id := f.start()
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.residue")
	f.deps.Policy.Revision = "unrelated"
	reopen(t, f)
	dir := candidateDir(f.stateDir, id)
	if err := os.WriteFile(filepath.Join(dir, "residue.go"), []byte("package residue // changed bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "candidate.residue")
	if err := os.Remove(filepath.Join(dir, "residue.go")); err != nil {
		t.Fatal(err)
	}
	f.reconcile(id)
	if op := f.state(id).snapshot.Operations[before.ID]; op.Attempt != 2 || op.State != Succeeded {
		t.Fatalf("residue repair did not enable retry: %+v", op)
	}
	if _, _, count := runtimeCommitsPastBase(t, f, id); count != "1" {
		t.Fatalf("recovery produced %s commits, want one", count)
	}
}

func TestRelevantPolicyChangeEnablesCheckpointRecovery(t *testing.T) {
	f, id := checkpointConflictFixture(t)
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	delete(f.deps.Policy.Rules, "observed-conflict")
	f.deps.Policy.Revision = "2"
	f.runtime = f.newRuntime(f.deps)
	f.reconcile(id)
	state := f.state(id)
	op := state.snapshot.Operations[before.ID]
	if op.Attempt != 2 || op.State != Succeeded || op.Failure != nil {
		t.Fatalf("recovered operation %+v", op)
	}
	if len(journalPayloads[CandidateCommittedPayload](t, state.events, EventCandidateCheckpointed)) != 1 {
		t.Fatal("the preserved commit was not recovered as a checkpoint")
	}
	if _, _, count := runtimeCommitsPastBase(t, f, id); count != "1" {
		t.Fatalf("recovery made %s commits", count)
	}
}

func TestRelevantModelChangeEnablesCheckpointRecovery(t *testing.T) {
	f, id := checkpointConflictFixture(t)
	f.reconcile(id)
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	boundaries := map[string]domain.CriticalBoundary{
		"service": {Type: "service", Paths: []string{"candidate.go"}},
	}
	f.deps.ProjectModel.CriticalBoundaries = &boundaries
	f.deps.ProjectModel.Revision = "2"
	reopen(t, f)
	f.reconcile(id)
	if op := f.state(id).snapshot.Operations[before.ID]; op.Attempt != 2 || op.State != Succeeded || op.Failure != nil {
		t.Fatalf("model inputs did not enable checkpoint recovery: %+v", op)
	}
	if _, _, count := runtimeCommitsPastBase(t, f, id); count != "1" {
		t.Fatalf("model recovery made %s commits", count)
	}
}

type cancelObservedDetector struct{ cancel context.CancelFunc }

func (d cancelObservedDetector) Detect(model domain.ProjectModel, in analysis.Input) ([]domain.EngineeringFact, error) {
	facts, err := (analysis.CriticalBoundaryDetector{}).Detect(model, in)
	if in.Stage == domain.StageObserved {
		d.cancel()
	}
	return facts, err
}

func TestRestartBetweenDeterministicFailureAndReconciliationDoesNotRepeat(t *testing.T) {
	f, id := checkpointConflictFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.runtime.flow = KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(cancelObservedDetector{cancel})}
	if _, err := f.runtime.Reconcile(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("stop between passes: %v", err)
	}
	before := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	if f.state(id).run.Reason == ReasonDeterministicFailureUnchanged {
		t.Fatal("the no-repeat decision already ran before restart")
	}
	// Downtime between the durable failure and its truthful wait is not work.
	f.clock.at = f.clock.at.Add(time.Hour)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	f.store, f.deps.Store = store, store
	f.runtime = f.newRuntime(f.deps)
	out := f.reconcile(id)
	if out.Reason != ReasonDeterministicFailureUnchanged {
		t.Fatalf("restart outcome %+v", out)
	}
	after := requireOneDeterministicFailure(t, f, id, OpCandidateCommit, "policy.compile")
	if !reflect.DeepEqual(before.Failure, after.Failure) {
		t.Fatalf("restart changed failure: %+v / %+v", before.Failure, after.Failure)
	}
}

type repeatObservationFailure struct{}

func (repeatObservationFailure) Detect(model domain.ProjectModel, in analysis.Input) ([]domain.EngineeringFact, error) {
	if in.Stage == domain.StageObserved {
		return nil, errors.New("conflicting local-looking observation")
	}
	return (analysis.CriticalBoundaryDetector{}).Detect(model, in)
}

func TestUnclassifiedFailureIsNotGuessedDeterministic(t *testing.T) {
	f := newPhase8Fixture(t)
	// This detector can change independently of its inputs. Its ordinary error
	// is not the pure compiler's typed rejection, even if a diagnostic repeats.
	f.runtime.flow = KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(repeatObservationFailure{})}
	id := f.start()
	out := f.reconcile(id)
	op := failedOperation(t, f, id, OpCandidateCommit)
	if out.Reason != OpCandidateCommit+attemptsExhaustedSuffix || op.Attempt != 3 || op.Failure != nil {
		t.Fatalf("unknown analyzer failure was guessed deterministic: %+v / %+v", out, op)
	}
}

func TestUnclassifiedRecoveryInfrastructureFailureIsNotGuessedDeterministic(t *testing.T) {
	f := newPhase8Fixture(t)
	withAfterCommitUpdateRef(t, func(dir string) {
		store := subjectStoreDir(dir)
		if err := os.RemoveAll(store); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store, []byte("unavailable object-store directory\n"), 0600); err != nil {
			t.Fatal(err)
		}
	})
	id := f.start()
	out := f.reconcile(id)
	op := failedOperation(t, f, id, OpCandidateCommit)
	if out.Reason != OpCandidateCommit+attemptsExhaustedSuffix || op.Attempt != 3 || op.Failure != nil {
		t.Fatalf("recovery I/O failure was guessed deterministic: %+v / %+v", out, op)
	}
	if _, _, count := runtimeCommitsPastBase(t, f, id); count != "1" {
		t.Fatalf("I/O failure lost or repeated the runtime commit: %s", count)
	}
	notPublished(t, f, id)
}
