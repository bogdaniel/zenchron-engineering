package runtime

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The continuation budget is about ONE question: may this run start another
// distinct continuation binding? These tests are written against that question
// rather than against checkpoints, commits, provider invocations or attempts,
// because conflating any of those with continuation depth is the defect.
//
// The shape being defended is the one run-1b876b78f20d83195e6b503831fcc9c7
// produced under the old law: 4 checkpoints, 3 distinct continuation bindings,
// terminated while every continuation was still doing productive work.

// continuationFixture builds a replayed runState directly from durable
// operations and a projection. It deliberately does not drive a provider: what
// is under test is how the runtime READS its own durable state, and a test that
// has to produce four real checkpoints to ask that question would be measuring
// the provider instead.
type continuationFixture struct {
	limit      int
	attempts   int
	legacy     bool
	head       string
	complete   bool
	operations []RunOperation
	// checkpoints is only ever used to prove it does NOT drive the decision.
	checkpoints int
}

func (f continuationFixture) state(t *testing.T) *runState {
	t.Helper()
	budgets := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: f.attempts, MaxAssuranceAttempts: 2, MaxRemediationAttempts: 2}
	persisted := budgets
	if !f.legacy {
		persisted.MaxExecutionContinuations = f.limit
	}
	operations := map[string]RunOperation{}
	for _, op := range f.operations {
		operations[op.ID] = op
	}
	runtime := &EngineeringRuntime{deps: Dependencies{Budgets: budgets, Clock: newSteppingClock()}}
	return &runState{
		rt: runtime,
		run: EngineeringRun{
			ID: "run-continuation", SchemaVersion: SchemaVersion,
			Base:      Ref{ID: "main", Revision: "base-revision"},
			Contract:  Ref{ID: "contract-continuation", Revision: "1"},
			Budgets:   persisted,
			CreatedAt: time.Unix(1_800_000_000, 0).UTC(),
		},
		snapshot: RunSnapshot{EngineeringRun: EngineeringRun{Disposition: Active}, Operations: operations},
		// bindExecutionInvoke pins the base through the observed source, so a
		// replayed state needs one for the planner to bind anything at all.
		sources: []sourceRecord{{Repository: "acme/repo", Issue: 54, BaseRevision: "base-revision", State: "open"}},
		projection: RunProjection{
			Contract:          Ref{ID: "contract-continuation", Revision: "1"},
			BaseRevision:      "base-revision",
			CandidateRevision: f.head,
			CandidateComplete: f.complete,
			Checkpoints:       f.checkpoints,
		},
	}
}

// succeededCandidateCreate is the dependency bindExecutionInvoke checks before
// it will bind anything at all.
func succeededCandidateCreate() RunOperation {
	return RunOperation{
		ID: "op-candidate-create", Kind: OpCandidateCreate,
		IdempotencyKey: operationKey(OpCandidateCreate, "base-revision"),
		State:          Succeeded,
	}
}

func continuationOperation(revision string, attempt, maxAttempts int, state OperationState) RunOperation {
	binding := invocationContinuationPrefix + revision
	return RunOperation{
		ID: "op-execution-" + revision, Kind: OpExecutionInvoke,
		IdempotencyKey: operationKey(OpExecutionInvoke, binding),
		State:          state, Attempt: attempt, MaxAttempts: maxAttempts,
	}
}

func initialOperation(attempt int, state OperationState) RunOperation {
	binding := "initial|1|base-revision"
	return RunOperation{
		ID: "op-execution-initial", Kind: OpExecutionInvoke,
		IdempotencyKey: operationKey(OpExecutionInvoke, binding),
		State:          state, Attempt: attempt, MaxAttempts: 3,
	}
}

// TestInitialExecutionConsumesNoContinuationUnit is acceptance A and B: the
// initial subject is not a continuation however many times it is retried.
func TestInitialExecutionConsumesNoContinuationUnit(t *testing.T) {
	for attempt := 1; attempt <= 3; attempt++ {
		fixture := continuationFixture{
			limit: 8, attempts: 3,
			operations: []RunOperation{succeededCandidateCreate(), initialOperation(attempt, OperationFailed)},
		}
		state := fixture.state(t)
		if got := len(state.startedContinuationBindings()); got != 0 {
			t.Fatalf("attempt %d: continuation units = %d, want 0", attempt, got)
		}
		if state.continuationCeilingReached() {
			t.Fatalf("attempt %d: the ceiling refused an initial execution", attempt)
		}
	}
}

// TestDistinctContinuationBindingsAreTheUnit is acceptance C, D and E.
func TestDistinctContinuationBindingsAreTheUnit(t *testing.T) {
	base := []RunOperation{succeededCandidateCreate(), initialOperation(1, OperationFailed)}

	first := continuationFixture{
		limit: 8, attempts: 3, head: "checkpoint-a",
		operations: append(base, continuationOperation("checkpoint-a", 1, 3, OperationFailed)),
	}.state(t)
	if got := len(first.startedContinuationBindings()); got != 1 {
		t.Fatalf("one started continuation counted as %d units, want 1", got)
	}

	// Acceptance D: the SAME binding retried is still one unit. Three attempts
	// of continuation|checkpoint-a spend three attempts and one continuation.
	retried := continuationFixture{
		limit: 8, attempts: 3, head: "checkpoint-a",
		operations: append(base, continuationOperation("checkpoint-a", 3, 3, OperationFailed)),
	}.state(t)
	if got := len(retried.startedContinuationBindings()); got != 1 {
		t.Fatalf("three retries of one continuation counted as %d units, want 1", got)
	}

	// Acceptance E: a binding for a different candidate revision is the next unit.
	second := continuationFixture{
		limit: 8, attempts: 3, head: "checkpoint-b",
		operations: append(append([]RunOperation(nil), base...),
			continuationOperation("checkpoint-a", 3, 3, OperationFailed),
			continuationOperation("checkpoint-b", 1, 3, OperationFailed)),
	}.state(t)
	if got := len(second.startedContinuationBindings()); got != 2 {
		t.Fatalf("two distinct continuations counted as %d units, want 2", got)
	}
}

// TestObservedRunShapeSpendsThreeContinuationsNotFourCheckpoints is the
// regression for the run that made this repair necessary. It is the whole
// point: checkpoints and continuation bindings are different numbers, and the
// budget is about the second one.
func TestObservedRunShapeSpendsThreeContinuationsNotFourCheckpoints(t *testing.T) {
	// run-1b876b78f20d83195e6b503831fcc9c7 exactly: one initial invocation and
	// three continuation bindings, the last of which was retried once, leaving
	// four checkpoint commits behind.
	fixture := continuationFixture{
		limit: 8, attempts: 3, head: "checkpoint-d", checkpoints: 4,
		operations: []RunOperation{
			succeededCandidateCreate(),
			initialOperation(1, OperationFailed),
			continuationOperation("checkpoint-a", 1, 3, OperationFailed),
			continuationOperation("checkpoint-b", 1, 3, OperationFailed),
			continuationOperation("checkpoint-c", 2, 3, OperationFailed),
		},
	}
	state := fixture.state(t)
	if got := len(state.startedContinuationBindings()); got != 3 {
		t.Fatalf("continuation units = %d, want 3 (four checkpoints, three bindings)", got)
	}
	if state.projection.Checkpoints != 4 {
		t.Fatalf("fixture is not the observed shape: checkpoints = %d", state.projection.Checkpoints)
	}
	// Acceptance F and the whole defect: three distinct continuations exceed
	// max_execution_attempts and are still nowhere near the continuation bound.
	if state.projection.Checkpoints <= state.rt.deps.Budgets.MaxExecutionAttempts {
		t.Fatal("the fixture no longer reproduces the shape that tripped the old ceiling")
	}
	if state.continuationCeilingReached() {
		t.Fatal("a run three continuations into a budget of eight was refused")
	}
	disposition, reason := state.conditions()
	if disposition == Failed && reason == "execution_continuations_exhausted" {
		t.Fatal("the observed run shape still terminates as continuations-exhausted")
	}
}

// TestContinuationCeilingRefusesOnlyANewBinding is acceptance J, K, L and M.
func TestContinuationCeilingRefusesOnlyANewBinding(t *testing.T) {
	limit := 3
	base := []RunOperation{succeededCandidateCreate(), initialOperation(1, OperationFailed)}
	atCeiling := append(append([]RunOperation(nil), base...),
		continuationOperation("checkpoint-a", 1, 3, OperationFailed),
		continuationOperation("checkpoint-b", 1, 3, OperationFailed),
		continuationOperation("checkpoint-c", 1, 3, OperationFailed))

	// Acceptance J and K: the ceiling is reached, and the LAST allowed binding
	// may still be retried, because a retry starts nothing new.
	retrying := continuationFixture{limit: limit, attempts: 3, head: "checkpoint-c", operations: atCeiling}.state(t)
	if got := len(retrying.startedContinuationBindings()); got != limit {
		t.Fatalf("continuation units = %d, want %d", got, limit)
	}
	if retrying.continuationCeilingReached() {
		t.Fatal("a retry of the final permitted continuation was refused as a new one")
	}
	if disposition, reason := retrying.conditions(); disposition == Failed && reason == "execution_continuations_exhausted" {
		t.Fatal("a retry at the ceiling terminated the run")
	}

	// Acceptance L: that continuation completing is never retroactively failed.
	completed := continuationFixture{limit: limit, attempts: 3, head: "checkpoint-c", complete: true, operations: atCeiling}.state(t)
	if completed.continuationCeilingReached() {
		t.Fatal("a completed candidate at the ceiling was refused")
	}
	if disposition, reason := completed.conditions(); disposition == Failed && reason == "execution_continuations_exhausted" {
		t.Fatal("a completed candidate at the ceiling was retroactively failed")
	}

	// Acceptance M: a genuinely NEW binding past the ceiling is refused, and
	// the checkpoint that asked for it is left exactly where it is.
	needsAnother := continuationFixture{limit: limit, attempts: 3, head: "checkpoint-d", operations: atCeiling}.state(t)
	if !needsAnother.continuationCeilingReached() {
		t.Fatal("a fourth distinct continuation was permitted under a limit of three")
	}
	disposition, reason := needsAnother.conditions()
	if disposition != Failed || reason != "execution_continuations_exhausted" {
		t.Fatalf("terminal decision = %q/%q, want failed/execution_continuations_exhausted", disposition, reason)
	}
	if needsAnother.projection.CandidateRevision != "checkpoint-d" {
		t.Fatal("the refusal disturbed the preserved checkpoint")
	}
}

// TestContinuationDepthMayExceedTheAttemptBudget is acceptance F stated on its
// own: the two resources are independent, so productive depth is not capped by
// how many times one invocation may be retried.
func TestContinuationDepthMayExceedTheAttemptBudget(t *testing.T) {
	operations := []RunOperation{succeededCandidateCreate(), initialOperation(1, OperationFailed)}
	for _, revision := range []string{"cp-1", "cp-2", "cp-3", "cp-4", "cp-5", "cp-6"} {
		operations = append(operations, continuationOperation(revision, 1, 2, OperationFailed))
	}
	state := continuationFixture{limit: 8, attempts: 2, head: "cp-7", operations: operations}.state(t)
	if got := len(state.startedContinuationBindings()); got != 6 {
		t.Fatalf("continuation units = %d, want 6", got)
	}
	if state.continuationCeilingReached() {
		t.Fatal("six continuations were refused under an attempt budget of two and a continuation budget of eight")
	}
}

// TestNonExecutionOperationsSpendNoContinuation is acceptance G, H and I: only
// a continuation execution binding is a continuation.
func TestNonExecutionOperationsSpendNoContinuation(t *testing.T) {
	state := continuationFixture{
		limit: 8, attempts: 3, head: "checkpoint-a", complete: true, checkpoints: 3,
		operations: []RunOperation{
			succeededCandidateCreate(),
			initialOperation(1, OperationFailed),
			{ID: "op-commit", Kind: OpCandidateCommit, IdempotencyKey: operationKey(OpCandidateCommit, "op-execution-initial"), State: Succeeded},
			{ID: "op-assure", Kind: OpAssuranceGo, IdempotencyKey: operationKey(OpAssuranceGo, "checkpoint-a"), State: Succeeded},
			{ID: "op-gofmt", Kind: OpRemediationGofmt, IdempotencyKey: operationKey(OpRemediationGofmt, "checkpoint-a"), State: Succeeded},
		},
	}.state(t)
	if got := len(state.startedContinuationBindings()); got != 0 {
		t.Fatalf("commits, assurance and remediation counted %d continuation units, want 0", got)
	}
	if state.continuationCeilingReached() {
		t.Fatal("a complete candidate was refused a continuation it never asked for")
	}
}

// TestHistoricalRunsReplayUnderLegacySemantics is acceptance R. A run created
// before the field existed persisted nothing, and reinterpreting that absence
// as today's default would silently change what an old run's terminal decision
// replays to.
func TestHistoricalRunsReplayUnderLegacySemantics(t *testing.T) {
	// Legacy record with budgets persisted but no continuation field: the
	// effective limit is that record's own attempt budget.
	legacy := continuationFixture{limit: 0, attempts: 3, legacy: true, head: "checkpoint-d",
		operations: []RunOperation{
			succeededCandidateCreate(), initialOperation(1, OperationFailed),
			continuationOperation("checkpoint-a", 1, 3, OperationFailed),
			continuationOperation("checkpoint-b", 1, 3, OperationFailed),
			continuationOperation("checkpoint-c", 1, 3, OperationFailed),
		}}.state(t)
	if got := legacy.continuationLimit(); got != 3 {
		t.Fatalf("legacy continuation limit = %d, want the record's MaxExecutionAttempts 3", got)
	}
	if got := legacy.continuationLimit(); got == DefaultMaxExecutionContinuations {
		t.Fatal("a historical record was reinterpreted as the new default")
	}
	if !legacy.continuationCeilingReached() {
		t.Fatal("a historical run did not reproduce its bounded terminal decision")
	}

	// The oldest records persisted no budgets at all. Those replay against the
	// configured attempt budget, exactly as they did when they ran.
	oldest := continuationFixture{limit: 0, attempts: 0, legacy: true, head: "checkpoint-b",
		operations: []RunOperation{
			succeededCandidateCreate(), initialOperation(1, OperationFailed),
			continuationOperation("checkpoint-a", 1, 3, OperationFailed),
		}}.state(t)
	oldest.rt.deps.Budgets.MaxExecutionAttempts = 1
	if got := oldest.continuationLimit(); got != 1 {
		t.Fatalf("oldest-record continuation limit = %d, want the configured attempt budget 1", got)
	}
	if !oldest.continuationCeilingReached() {
		t.Fatal("an oldest-shape record did not reproduce its bounded decision")
	}
}

// TestNewRunsPersistAnExplicitContinuationBudget is acceptance S.
func TestNewRunsPersistAnExplicitContinuationBudget(t *testing.T) {
	budgets := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 3}.defaults()
	if budgets.MaxExecutionContinuations != DefaultMaxExecutionContinuations {
		t.Fatalf("default continuation budget = %d, want %d", budgets.MaxExecutionContinuations, DefaultMaxExecutionContinuations)
	}
	encoded, err := json.Marshal(EngineeringRun{ID: "run-new", Budgets: budgets})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"max_execution_continuations":8`) {
		t.Fatalf("a new run did not persist an explicit continuation budget: %s", encoded)
	}
	var restored EngineeringRun
	if err := json.Unmarshal(encoded, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Budgets.MaxExecutionContinuations != DefaultMaxExecutionContinuations {
		t.Fatal("the persisted continuation budget did not survive a round trip")
	}
	// An explicit operator value is persisted verbatim rather than defaulted.
	explicit := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 3, MaxExecutionContinuations: 4}.defaults()
	if explicit.MaxExecutionContinuations != 4 {
		t.Fatalf("an explicit continuation budget was overwritten: %d", explicit.MaxExecutionContinuations)
	}
}

// TestContinuationBudgetIsConfigurableAndTightenOnly is acceptance P and Q.
func TestContinuationBudgetIsConfigurableAndTightenOnly(t *testing.T) {
	// Absent resolves to the M1 default, so a pre-#54 operator configuration -
	// which cannot contain this field - stays loadable instead of being refused.
	absentDir := t.TempDir()
	absent, absentDigest, err := LoadOperatorConfig(writeOperatorConfig(t, absentDir))
	if err != nil {
		t.Fatal(err)
	}
	if absent.Budgets.MaxExecutionContinuations != DefaultMaxExecutionContinuations {
		t.Fatalf("absent continuation budget resolved to %d, want %d", absent.Budgets.MaxExecutionContinuations, DefaultMaxExecutionContinuations)
	}
	if budgets := (Config{OperatorConfig: absent}).RunBudgets(); budgets.MaxExecutionContinuations <= 0 {
		t.Fatal("an absent continuation budget produced an unbounded run budget")
	}

	// An explicit operator value is carried through unchanged.
	statedDir := t.TempDir()
	stated := strings.Replace(operatorConfigJSON(statedDir), `"max_execution_attempts": 3`, `"max_execution_attempts": 3, "max_execution_continuations": 6`, 1)
	operator, statedDigest, err := LoadOperatorConfig(writeFile(t, filepath.Join(statedDir, "config.json"), stated))
	if err != nil {
		t.Fatal(err)
	}
	if operator.Budgets.MaxExecutionContinuations != 6 {
		t.Fatalf("operator continuation budget = %d, want 6", operator.Budgets.MaxExecutionContinuations)
	}
	// Acceptance Q: the configuration digest is about the effective bounds.
	if statedDigest == absentDigest {
		t.Fatal("changing the continuation budget did not change the configuration digest")
	}

	// Acceptance P: a repository may tighten the operator bound, never widen it.
	tightened, err := operator.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{MaxExecutionContinuations: intPointer(4)}})
	if err != nil {
		t.Fatal(err)
	}
	if tightened.Budgets.MaxExecutionContinuations != 4 {
		t.Fatalf("repository tightening produced %d, want 4", tightened.Budgets.MaxExecutionContinuations)
	}
	if _, err := operator.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{MaxExecutionContinuations: intPointer(9)}}); err == nil {
		t.Fatal("a repository widened the operator continuation bound")
	}
}

func intPointer(v int) *int { return &v }

// TestNegativeContinuationBudgetIsRefused proves there is no spelling that
// means unbounded.
func TestNegativeContinuationBudgetIsRefused(t *testing.T) {
	dir := t.TempDir()
	body := strings.Replace(operatorConfigJSON(dir), `"max_execution_attempts": 3`, `"max_execution_attempts": 3, "max_execution_continuations": -1`, 1)
	if _, _, err := LoadOperatorConfig(writeFile(t, filepath.Join(dir, "config.json"), body)); err == nil {
		t.Fatal("a negative continuation budget was accepted")
	}
}
