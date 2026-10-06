package runtime

// #328: a physical provider-attempt wall deadline is NOT the run's cumulative
// active-work budget. The #324 dogfood run spent its whole 1800s run budget in
// one attempt, was classified execution_incomplete / route retry, and then
// failed run_wall_budget_exhausted - advertising a successor the run had no
// authority to execute.
//
// Each test names the invariant it proves. The fixture drives the real
// reconciler against an injected stepping clock; the provider below stands in
// for the CLI adapter by ending exactly the way it does at its deadline
// (FailureExecutionIncomplete with a plain process error), so nothing here
// needs a live provider or real time.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

type wallStep struct {
	mutate   bool          // write a new file before stopping
	complete bool          // finish normally
	spend    time.Duration // active time spent; 0 means "run to the deadline"
	class    FailureClass  // the stop's class; default execution_incomplete
	inactive bool          // the runtime ended it for inactivity
}

// wallProvider spends time on the fixture clock and stops the way the CLI
// adapter stops at a runtime bound.
type wallProvider struct {
	clock    *steppingClock
	steps    []wallStep
	requests []ExecutionRequest
	panicAt  int // 1-based invocation that "kills the controller"; 0 = never
}

func (p *wallProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *wallProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	index := len(p.requests)
	p.requests = append(p.requests, request)
	step := wallStep{}
	if index < len(p.steps) {
		step = p.steps[index]
	}
	if step.mutate {
		name := filepath.Join(request.CandidateDir, "work"+strconv.Itoa(index)+".go")
		if err := os.WriteFile(name, []byte("package candidate\n"), 0600); err != nil {
			return ExecutionResult{}, err
		}
	}
	// Termination is recorded in the attempt provenance exactly as the CLI
	// adapter records it, so the bound is read through the #327 path.
	cause := "provider_returned"
	switch {
	case step.spend > 0:
		p.clock.advance(step.spend)
	case request.Deadline != nil:
		p.clock.advance(request.Deadline.Sub(p.clock.Now()))
		cause = TerminationDeadlineReached
	}
	if step.inactive {
		cause = TerminationProviderInactivity
	}
	if p.panicAt == index+1 {
		panic("controller died mid-attempt")
	}
	invocation := &InvocationProvenance{AgentID: "test-provider", InvocationObservation: domain.InvocationObservation{TerminationCause: cause}}
	if step.complete {
		return ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded, Invocation: invocation}, nil
	}
	class := step.class
	if class == "" {
		class = FailureExecutionIncomplete
	}
	return ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: class}, Invocation: invocation},
		errors.New("signal: terminated")
}

// provenanceBounds are the deadline_bound each execution attempt's durable
// provenance event recorded, in journal order.
func provenanceBounds(t *testing.T, events []EngineeringEvent) []AttemptBound {
	t.Helper()
	var out []AttemptBound
	for _, e := range events {
		if e.Type != EventExecutionAttemptProvenance {
			continue
		}
		p, err := decodePayload[ExecutionAttemptProvenance](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p.Invocation.DeadlineBound)
	}
	return out
}

func wallFixture(t *testing.T, budgets RunBudgets, steps ...wallStep) (*phase8Fixture, *wallProvider) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	provider := &wallProvider{clock: fixture.clock, steps: steps}
	fixture.deps.Provider = provider
	fixture.deps.Budgets = budgets
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture, provider
}

// drive re-reconciles the way the watch loop does, until the run terminates or
// the provider has been asked for everything scripted.
func drive(f *phase8Fixture, p *wallProvider, runID string) {
	for pass := 0; pass < 12; pass++ {
		f.reconcile(runID)
		if terminalDisposition(f.state(runID).snapshot.Disposition) || len(p.requests) > len(p.steps) {
			return
		}
	}
}

// executions are the run's execution.invoke operations in durable order, with
// the diagnostic each recorded.
func executions(t *testing.T, state *runState) ([]RunOperation, []*ExecutionDiagnostic) {
	t.Helper()
	var ops []RunOperation
	for _, op := range sortOperations(mapValues(state.snapshot.Operations)) {
		if op.Kind == OpExecutionInvoke {
			ops = append(ops, op)
		}
	}
	diagnostics := make([]*ExecutionDiagnostic, len(ops))
	for i, op := range ops {
		var record executionRecord
		if len(op.Result) > 0 {
			if err := json.Unmarshal(op.Result, &record); err != nil {
				t.Fatal(err)
			}
		}
		diagnostics[i] = record.Diagnostic
	}
	return ops, diagnostics
}

// attemptDiagnostics reads every execution attempt's diagnostic from the
// JOURNAL (operation.after), because a retried operation's row only keeps its
// latest attempt and the invariant is about each attempt.
func attemptDiagnostics(t *testing.T, events []EngineeringEvent) []*ExecutionDiagnostic {
	t.Helper()
	var out []*ExecutionDiagnostic
	for _, e := range events {
		if e.Type != EventOperationAfter {
			continue
		}
		op, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind != OpExecutionInvoke {
			continue
		}
		d, err := executionDiagnosticOf(op.Result)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

// authority is the wall bound a provider was dispatched with. The stepping
// clock ticks a second per read, so it trails the attempt's start by seconds.
func attemptAuthority(r ExecutionRequest) time.Duration {
	return r.Budgets.WallLimit
}

func nearly(got, want time.Duration) bool {
	return got <= want && want-got < time.Minute
}

// INVARIANT (#497): an absent attempt limit is derived at run creation to the
// shipped hard fuse, capped by the run budget (and by any narrower stage
// budget). A stated one is kept, and the configuration layer never resolves
// the absent member - so it stays absent from Config.RunBudgets and from the
// existing configuration digest.
func TestAttemptWallLimitDefaultsAreDerivedAtRunCreation(t *testing.T) {
	var config Config
	config.Budgets = BudgetConfig{WallLimitSeconds: 5400, MaxExecutionAttempts: 2, MaxRemediationAttempts: 2, MaxAssuranceAttempts: 2}
	if got := config.RunBudgets().AttemptWallLimit; got != 0 {
		t.Fatalf("configuration resolved an absent attempt limit to %s; it must stay absent until run creation", got)
	}
	if got := config.RunBudgets().defaults().AttemptWallLimit; got != 5400*time.Second {
		t.Fatalf("a run wall below the shipped fuse derived attempt limit %s, want the run budget 1h30m", got)
	}
	config.Budgets.WallLimitSeconds = 6 * 60 * 60
	if got := config.RunBudgets().defaults().AttemptWallLimit; got != DefaultAttemptWallLimit {
		t.Fatalf("a wide run derived attempt limit %s, want the shipped hard fuse %s", got, DefaultAttemptWallLimit)
	}
	config.Budgets.AttemptWallLimitSeconds = 1800
	if got := config.RunBudgets().defaults().AttemptWallLimit; got != 1800*time.Second {
		t.Fatalf("stated attempt limit became %s, want 30m", got)
	}
	config.Budgets.AttemptWallLimitSeconds = 4 * 60 * 60
	if got := config.RunBudgets().defaults().AttemptWallLimit; got != 4*time.Hour {
		t.Fatalf("explicit operator fuse became %s, want the stated 4h even though the shipped default is 3h", got)
	}
	config.Budgets.AttemptWallLimitSeconds = 1800
	// A plan stage that narrows the run budget below the attempt limit narrows
	// the attempt with it.
	narrowed := config.RunBudgets().defaults().tightenedBy(domain.StageBudget{MaxWallSeconds: 600})
	if narrowed.AttemptWallLimit != 600*time.Second {
		t.Fatalf("a 10m stage run kept a %s attempt limit", narrowed.AttemptWallLimit)
	}
	// An embedder with no budgets at all gets the documented 1h run and 1h attempt.
	if got := (RunBudgets{}).defaults(); got.WallLimit != time.Hour || got.AttemptWallLimit != time.Hour {
		t.Fatalf("zero budgets defaulted to wall %s attempt %s", got.WallLimit, got.AttemptWallLimit)
	}

	fixture, _ := wallFixture(t, RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 2})
	runID := fixture.start()
	if frozen := fixture.state(runID).run.Budgets.AttemptWallLimit; frozen != time.Hour {
		t.Fatalf("run froze attempt limit %s, want the run-capped 1h", frozen)
	}
	wide, _ := wallFixture(t, RunBudgets{WallLimit: 6 * time.Hour, MaxExecutionAttempts: 2})
	wideRun := wide.start()
	if frozen := wide.state(wideRun).run.Budgets.AttemptWallLimit; frozen != DefaultAttemptWallLimit {
		t.Fatalf("wide run froze attempt limit %s, want the shipped hard fuse %s", frozen, DefaultAttemptWallLimit)
	}
}

// INVARIANT (#497): healthy long work is not ended merely because it exceeds
// the old 25-minute operational value. With enough run authority, the default
// physical fuse is three hours; a 90-minute provider return is therefore an
// ordinary provider-owned ending. The provider-specific progress/inactivity
// oracle is tested separately by #238/#322 and is intentionally not duplicated
// here.
func TestAHealthyNinetyMinuteAttemptFitsInsideTheDefaultHardFuse(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: 6 * time.Hour, MaxExecutionAttempts: 2},
		wallStep{spend: 90 * time.Minute, complete: true})
	runID := fixture.start()
	fixture.reconcile(runID)

	if len(provider.requests) != 1 {
		t.Fatalf("provider invocations = %d, want one", len(provider.requests))
	}
	if got := attemptAuthority(provider.requests[0]); !nearly(got, DefaultAttemptWallLimit) {
		t.Fatalf("default physical attempt authority = %s, want %s", got, DefaultAttemptWallLimit)
	}
	var provenance *InvocationProvenance
	for _, event := range journalOf(t, fixture.runtime, runID) {
		if event.Type != EventExecutionAttemptProvenance {
			continue
		}
		p, err := decodePayload[ExecutionAttemptProvenance](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		provenance = &p.Invocation
	}
	if provenance == nil || provenance.TerminationCause != "provider_returned" || provenance.DeadlineBound != BoundAttemptWall {
		t.Fatalf("90m provider ending provenance = %+v, want provider_returned under the 3h attempt fuse", provenance)
	}
}

// INVARIANT (ADR-0003 §4 (a)): upgrading the binary does not move the Global
// digest of an existing configuration. The pinned digest was computed from this
// document by the build BEFORE the attempt limit existed.
func TestAnAbsentAttemptLimitLeavesAnExistingConfigDigestByteIdentical(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// Fixed paths, so the digest is a constant of the document.
	document := operatorConfigJSON("/fixed")
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	operator, digest, err := LoadOperatorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest != preAttemptLimitDigest {
		t.Fatalf("an existing configuration's digest moved to %s (was %s): every live run would park as controller_changed", digest, preAttemptLimitDigest)
	}
	canonical, err := json.Marshal(operator)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), "attempt_wall_limit") {
		t.Fatalf("the digested form carries an attempt limit nobody stated: %s", canonical)
	}
	// A STATED value is digested like any other member.
	stated := strings.Replace(document, `"wall_limit_seconds": 3600`, `"wall_limit_seconds": 3600, "attempt_wall_limit_seconds": 900`, 1)
	if err := os.WriteFile(path, []byte(stated), 0600); err != nil {
		t.Fatal(err)
	}
	_, statedDigest, err := LoadOperatorConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if statedDigest == digest {
		t.Fatal("a stated attempt limit is not part of the controller-effective digest")
	}
}

// preAttemptLimitDigest is LoadOperatorConfig's digest of the document above,
// captured on origin/main (eca0601) before #328.
const preAttemptLimitDigest = "f27735e735903084e3ed587d74fb95f75ba8728a36da7c4853d55496424a4be3"

// INVARIANT: a stated attempt limit above the run budget is refused at load,
// and a repository may only tighten it - against the operator's stated limit,
// or against min(the shipped hard fuse, the operator run budget) when the
// operator states none. Its own simultaneously tightened run wall narrows that
// ceiling again.
func TestAttemptWallLimitAboveTheRunBudgetIsRefused(t *testing.T) {
	var base OperatorConfig
	if err := json.Unmarshal([]byte(operatorConfigJSON("/fixed")), &base); err != nil {
		t.Fatal(err)
	}
	base.Budgets.WallLimitSeconds = 1800
	for _, tc := range []struct {
		name    string
		attempt int
		refused bool
	}{
		{"absent", 0, false},
		{"equal to the run budget", 1800, false},
		{"below", 900, false},
		{"above the run budget", 1801, true},
		{"negative", -1, true},
	} {
		config := base
		config.Budgets.AttemptWallLimitSeconds = tc.attempt
		err := config.validate("config.json")
		if refused := err != nil; refused != tc.refused {
			t.Errorf("%s: validate() = %v, want refused=%v", tc.name, err, tc.refused)
		}
	}
	seconds := func(n int) *int { return &n }
	for _, tc := range []struct {
		name     string
		operator int
		proposed int
		refused  bool
	}{
		{"repository narrows an absent operator limit", 0, 600, false},
		{"repository cannot exceed the run budget when the operator states none", 0, 1801, true},
		{"repository narrows a stated limit", 900, 600, false},
		{"repository cannot widen a stated limit", 900, 901, true},
		{"repository cannot state zero", 900, 0, true},
	} {
		config := base
		config.Budgets.AttemptWallLimitSeconds = tc.operator
		tightened, err := config.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{AttemptWallLimitSeconds: seconds(tc.proposed)}})
		if refused := err != nil; refused != tc.refused {
			t.Errorf("%s: Tighten() = %v, want refused=%v", tc.name, err, tc.refused)
			continue
		}
		if !tc.refused && tightened.Budgets.AttemptWallLimitSeconds != tc.proposed {
			t.Errorf("%s: tightened to %d, want %d", tc.name, tightened.Budgets.AttemptWallLimitSeconds, tc.proposed)
		}
	}
	// A repository that says nothing leaves an absent limit absent.
	tightened, err := base.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{WallLimitSeconds: seconds(900)}})
	if err != nil || tightened.Budgets.AttemptWallLimitSeconds != 0 {
		t.Fatalf("an unstated repository attempt limit became %d (%v)", tightened.Budgets.AttemptWallLimitSeconds, err)
	}

	// #497: with a run budget above the shipped fuse, an absent operator value
	// authorizes only that fuse. The repository may tighten it, never widen it
	// back toward the larger run wall. If it tightens both fields, its own run
	// wall is also an upper bound.
	wide := base
	wide.Budgets.WallLimitSeconds = 6 * 60 * 60
	wide.Budgets.AttemptWallLimitSeconds = 0
	for _, tc := range []struct {
		name        string
		runWall     *int
		attemptWall int
		refused     bool
	}{
		{"repository narrows the shipped 3h fuse", nil, 2 * 60 * 60, false},
		{"repository cannot widen the shipped 3h fuse", nil, 4 * 60 * 60, true},
		{"repository attempt cannot exceed its simultaneously tightened run wall", seconds(2 * 60 * 60), 150 * 60, true},
		{"repository may match its simultaneously tightened run wall", seconds(2 * 60 * 60), 2 * 60 * 60, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := wide.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{
				WallLimitSeconds: tc.runWall, AttemptWallLimitSeconds: seconds(tc.attemptWall),
			}})
			if refused := err != nil; refused != tc.refused {
				t.Fatalf("Tighten() = %v, want refused=%v", err, tc.refused)
			}
		})
	}
}

// INVARIANT (ADR-0003 §4 (b)): the attempt limit is frozen at run creation and
// read back exactly. A later configuration edit - wider or narrower - does not
// reach the live run, and a run that predates the field is never handed the
// live value.
func TestTheAttemptLimitIsFrozenAtRunCreation(t *testing.T) {
	fixture, _ := wallFixture(t, RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 10 * time.Minute, MaxExecutionAttempts: 2})
	runID := fixture.start()
	for _, edited := range []time.Duration{50 * time.Minute, 2 * time.Minute} {
		deps := fixture.deps
		deps.Budgets.AttemptWallLimit = edited
		fixture.runtime = fixture.newRuntime(deps)
		state := fixture.state(runID)
		if got := state.budgets().AttemptWallLimit; got != 10*time.Minute {
			t.Fatalf("after editing the configuration to %s the run reads attempt limit %s, want its frozen 10m", edited, got)
		}
		if limit := state.attemptLimit(fixture.clock.Now()); limit == nil || limit.Within != 10*time.Minute || limit.Bound != BoundAttemptWall {
			t.Fatalf("after editing the configuration to %s the next attempt gets %+v, want the frozen 10m attempt wall", edited, limit)
		}
	}
}

// INVARIANT (legacy): a run persisted before #328 carries no attempt limit and
// keeps today's rule exactly - its attempt is bounded by the operation's own
// WallBudget remainder - even when the live configuration now states one.
func TestALegacyRunKeepsTheOperationRemainderRule(t *testing.T) {
	clock := newSteppingClock()
	live := RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 5 * time.Minute, MaxExecutionAttempts: 2}
	legacy := RunBudgets{WallLimit: time.Hour, MaxExecutionAttempts: 2} // no attempt_wall_limit
	state := &runState{
		rt:  &EngineeringRuntime{deps: Dependencies{Budgets: live, Clock: clock}},
		run: EngineeringRun{ID: "run-legacy", Budgets: &legacy, CreatedAt: clock.Now()},
	}
	if limit := state.attemptLimit(clock.Now()); limit != nil {
		t.Fatalf("a legacy run was handed an attempt limit %+v it was never created with", limit)
	}
	scheduler := Scheduler{Store: NewMemoryOperationStore(), Clock: clock, Owner: "owner", LeaseDuration: time.Minute}
	op, _, err := scheduler.Plan(RunOperation{RunID: "run-legacy", Kind: OpExecutionInvoke, IdempotencyKey: "k", MaxAttempts: 2, WallBudget: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	started, err := scheduler.StartWithin(op.ID, state.attemptLimit(clock.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if got := started.Deadline.Sub(*started.ActiveSince); got != time.Hour || started.DeadlineBound != "" {
		t.Fatalf("legacy attempt authority %s bound %q, want the whole 1h operation remainder and no bound", got, started.DeadlineBound)
	}
}

// INVARIANT: an attempt-wall stop on ZERO-DELTA work selects a same-binding
// retry, which is actually executed while run budget remains; the retry spends
// retry authority and gets min(attempt limit, remaining run work).
func TestAZeroDeltaAttemptWallStopRetriesTheSameBinding(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 20 * time.Minute, MaxExecutionAttempts: 3},
		wallStep{}, wallStep{mutate: true, complete: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	if len(provider.requests) < 2 {
		t.Fatalf("the typed retry was never executed: %d invocation(s)", len(provider.requests))
	}
	first, second := provider.requests[0], provider.requests[1]
	if first.OperationID != second.OperationID || second.Purpose != InvocationInitial {
		t.Fatalf("the successor was not a retry of the same binding: %s/%s vs %s/%s", first.OperationID, first.Purpose, second.OperationID, second.Purpose)
	}
	if got := attemptAuthority(first); !nearly(got, 20*time.Minute) {
		t.Fatalf("first attempt authority %s, want the 20m attempt wall", got)
	}
	if got := attemptAuthority(second); got <= 0 || got > 20*time.Minute {
		t.Fatalf("retry authority %s, want at most the attempt wall", got)
	}
	d := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))[0]
	if d.Bound != BoundAttemptWall || d.Successor != SuccessorRetry || d.SuccessorUnavailable != "" || d.Route != RouteRetry {
		t.Fatalf("attempt-wall stop recorded %+v, want bound=attempt_wall successor=retry available", d)
	}
}

// INVARIANT (#54): an attempt-wall stop on PRODUCTIVE work is a checkpoint and
// its successor is continuation|<revision>. It is never forced into same-binding
// retry accounting merely because the attempt ended at its wall.
func TestAProductiveAttemptWallStopContinuesRatherThanRetries(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 20 * time.Minute, MaxExecutionAttempts: 3},
		wallStep{mutate: true}, wallStep{mutate: true, complete: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	if len(provider.requests) < 2 {
		t.Fatalf("no successor was executed: %d invocation(s)", len(provider.requests))
	}
	second := provider.requests[1]
	if second.Purpose != InvocationContinuation || second.OperationID == provider.requests[0].OperationID {
		t.Fatalf("successor purpose %s on operation %s, want a NEW continuation binding", second.Purpose, second.OperationID)
	}
	state := fixture.state(runID)
	if state.projection.Checkpoints != 1 {
		t.Fatalf("checkpoints = %d, want the productive attempt-wall stop preserved as one checkpoint", state.projection.Checkpoints)
	}
	ops, _ := executions(t, state)
	if ops[0].Attempt != 1 {
		t.Fatalf("the productive binding spent %d retry attempts, want 1", ops[0].Attempt)
	}
	d := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))[0]
	if d.Bound != BoundAttemptWall || d.Successor != SuccessorContinuation || d.SuccessorUnavailable != "" {
		t.Fatalf("productive attempt-wall stop recorded %+v, want bound=attempt_wall successor=continuation available", d)
	}
}

// INVARIANT: when the run has less active work left than one attempt limit, the
// attempt is TRUNCATED to what remains, the stop is recorded as run active-work
// exhaustion (not attempt wall), no successor is advertised, and the run fails
// run_wall_budget_exhausted. The retry spends the remainder, never a fresh
// envelope. Covers both successor kinds.
func TestATruncatedAttemptIsRunExhaustionAndAdvertisesNoSuccessor(t *testing.T) {
	for _, tc := range []struct {
		name      string
		second    wallStep
		successor string
	}{
		{"zero-delta retry", wallStep{}, SuccessorRetry},
		{"productive continuation", wallStep{mutate: true}, SuccessorContinuation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// 50m attempt wall inside a 1h run: the first zero-delta attempt
			// stops at its wall, the second is truncated to what is left.
			fixture, provider := wallFixture(t,
				RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 50 * time.Minute, MaxExecutionAttempts: 5},
				wallStep{}, tc.second)
			runID := fixture.start()
			drive(fixture, provider, runID)

			if len(provider.requests) != 2 {
				t.Fatalf("invocations = %d, want exactly 2", len(provider.requests))
			}
			if got := attemptAuthority(provider.requests[1]); got <= 0 || got >= 10*time.Minute {
				t.Fatalf("second attempt authority %s, want the <10m the run had left rather than a fresh envelope", got)
			}
			diagnostics := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))
			if first := diagnostics[0]; first.Bound != BoundAttemptWall || first.SuccessorUnavailable != "" {
				t.Fatalf("first stop %+v, want an attempt-wall stop with its retry available", first)
			}
			last := diagnostics[len(diagnostics)-1]
			if last.Bound != BoundRunActiveWork || last.Successor != tc.successor ||
				last.SuccessorUnavailable != "run_active_work_exhausted" || last.Route != RouteStop {
				t.Fatalf("truncated stop recorded %+v, want bound=run_active_work successor=%s unavailable, route=stop", last, tc.successor)
			}
			// The same fact is in each attempt's durable provenance (#327):
			// one source, which the diagnostic above was read from.
			if bounds := provenanceBounds(t, journalOf(t, fixture.runtime, runID)); len(bounds) != 2 ||
				bounds[0] != BoundAttemptWall || bounds[1] != BoundRunActiveWork {
				t.Fatalf("attempt provenance deadline bounds %v, want [attempt_wall run_active_work]", bounds)
			}
			state := fixture.state(runID)
			if state.snapshot.Disposition != Failed || state.snapshot.Reason != "run_wall_budget_exhausted" {
				t.Fatalf("run %s/%s, want failed/run_wall_budget_exhausted", state.snapshot.Disposition, state.snapshot.Reason)
			}
		})
	}
}

// INVARIANT: an attempt whose own retry authority is spent does not advertise a
// retry either, even with run budget left.
func TestAnExhaustedRetryBudgetIsNotAdvertised(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 5 * time.Minute, MaxExecutionAttempts: 2},
		wallStep{}, wallStep{})
	runID := fixture.start()
	drive(fixture, provider, runID)
	diagnostics := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))
	last := diagnostics[len(diagnostics)-1]
	if last.Successor != SuccessorRetry || last.SuccessorUnavailable != "execution_attempts_exhausted" || last.Route != RouteStop {
		t.Fatalf("last attempt recorded %+v, want its retry named unavailable", last)
	}
}

// INVARIANT: status and the journal distinguish the three bounds - provider
// inactivity, the physical-attempt wall, and run active-work exhaustion.
func TestStatusDistinguishesInactivityAttemptWallAndRunExhaustion(t *testing.T) {
	for _, tc := range []struct {
		name  string
		steps []wallStep
		want  AttemptBound
	}{
		{"inactivity", []wallStep{{spend: time.Minute, class: FailureProviderNoProgress}}, BoundProviderInactivity},
		// A provider that named a condition and then went quiet keeps that
		// class (#238); the bound is still inactivity.
		{"inactivity with a named condition", []wallStep{{spend: time.Minute, class: FailureProviderQuota, inactive: true}}, BoundProviderInactivity},
		{"attempt wall", []wallStep{{}}, BoundAttemptWall},
		// 30m spent without reaching any bound, then an attempt truncated to
		// the <30m the run has left.
		{"run active work", []wallStep{{spend: 30 * time.Minute, class: FailureTransientProvider}, {}}, BoundRunActiveWork},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, provider := wallFixture(t,
				RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 40 * time.Minute, MaxExecutionAttempts: 1 + len(tc.steps)}, tc.steps...)
			provider.steps = append(provider.steps, wallStep{mutate: true, complete: true})
			runID := fixture.start()
			for pass := 0; pass < 12 && len(provider.requests) < len(tc.steps); pass++ {
				fixture.reconcile(runID)
			}
			journal := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))
			if got := journal[len(tc.steps)-1].Bound; got != tc.want {
				t.Fatalf("journalled bound %q, want %q", got, tc.want)
			}
			report, err := fixture.runtime.Status(runID)
			if err != nil {
				t.Fatal(err)
			}
			if report.ExecutionDiagnostic == nil || report.ExecutionDiagnostic.Bound != tc.want {
				t.Fatalf("status diagnostic %+v, want bound %q", report.ExecutionDiagnostic, tc.want)
			}
		})
	}
}

// INVARIANT: a restart does not renew either budget. The abandoned attempt is
// charged in full, the successor is a new physical attempt counted against the
// binding's retry authority, and its deadline is min(frozen attempt limit,
// remaining run active work) - the orphaned interval included. Here 12m of work
// plus 40m of a dead controller leave the 1h run under 8m: the successor is
// truncated to that, and its stop is run exhaustion, not a fresh 15m attempt.
func TestARestartRenewsNeitherTheAttemptNorTheRunBudget(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 15 * time.Minute, MaxExecutionAttempts: 3},
		wallStep{spend: 12 * time.Minute}, wallStep{})
	provider.panicAt = 1
	runID := fixture.start()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the scripted controller death did not happen")
			}
		}()
		fixture.reconcile(runID)
	}()
	// The controller is gone 40m; its lease expires and a new one takes over.
	fixture.clock.advance(40 * time.Minute)
	fixture.runtime = fixture.newRuntime(fixture.deps)
	drive(fixture, provider, runID)

	if len(provider.requests) < 2 {
		t.Fatalf("the successor attempt never ran: %d invocation(s)", len(provider.requests))
	}
	successor := provider.requests[1]
	if successor.OperationID != provider.requests[0].OperationID || successor.Attempt <= provider.requests[0].Attempt {
		t.Fatalf("the successor is not a new physical attempt of the same binding: %+v", successor)
	}
	if got := attemptAuthority(successor); got <= 0 || got >= 8*time.Minute {
		t.Fatalf("successor authority %s, want the <8m the run had left", got)
	}
	ops, _ := executions(t, fixture.state(runID))
	if ops[0].Attempt < 2 || ops[0].ConsumedExecution < 52*time.Minute {
		t.Fatalf("operation attempts=%d consumed=%s, want the abandoned attempt counted and charged in full", ops[0].Attempt, ops[0].ConsumedExecution)
	}
	diagnostics := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))
	if last := diagnostics[len(diagnostics)-1]; last.Bound != BoundRunActiveWork || last.SuccessorUnavailable != "run_active_work_exhausted" {
		t.Fatalf("successor stop recorded %+v, want run active-work exhaustion with no successor", last)
	}
	if state := fixture.state(runID); state.snapshot.Reason != "run_wall_budget_exhausted" {
		t.Fatalf("run reason %q, want run_wall_budget_exhausted", state.snapshot.Reason)
	}
}

// INVARIANT (#54): a productive attempt-wall stop whose continuation would be a
// binding beyond the continuation ceiling does not advertise it, and the run
// ends execution_continuations_exhausted with run budget left.
func TestAnExhaustedContinuationBudgetIsNotAdvertised(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 10 * time.Minute, MaxExecutionAttempts: 3, MaxExecutionContinuations: 1},
		wallStep{mutate: true}, wallStep{mutate: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	if len(provider.requests) != 2 || provider.requests[1].Purpose != InvocationContinuation {
		t.Fatalf("want an initial attempt and one continuation, got %d invocation(s)", len(provider.requests))
	}
	diagnostics := attemptDiagnostics(t, journalOf(t, fixture.runtime, runID))
	if first := diagnostics[0]; first.Successor != SuccessorContinuation || first.SuccessorUnavailable != "" {
		t.Fatalf("first stop %+v, want its one permitted continuation available", first)
	}
	last := diagnostics[len(diagnostics)-1]
	if last.Bound != BoundAttemptWall || last.Successor != SuccessorContinuation ||
		last.SuccessorUnavailable != "execution_continuations_exhausted" || last.Route != RouteStop {
		t.Fatalf("second stop %+v, want continuation unavailable: execution_continuations_exhausted, route stop", last)
	}
	if state := fixture.state(runID); state.snapshot.Disposition != Failed || state.snapshot.Reason != "execution_continuations_exhausted" {
		t.Fatalf("run %s/%s, want failed/execution_continuations_exhausted", state.snapshot.Disposition, state.snapshot.Reason)
	}
}

// successorState is the smallest runState admitSuccessor reads: a run whose
// 30m budget is exactly spent, with the given journal.
func successorState(events []EngineeringEvent, budgets RunBudgets) *runState {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	state := conditionsFixture(start, &steppingClock{at: start.Add(30 * time.Minute)}, budgets, events)
	state.run.Budgets = &budgets
	return state
}

// truncatedStop is a zero-delta or productive stop at a deadline whose bound
// was the given one, as the attempt's #327 provenance records it.
func truncatedStop(bound AttemptBound) (*ExecutionDiagnostic, RunOperation, *InvocationProvenance) {
	return &ExecutionDiagnostic{FailureClass: FailureExecutionIncomplete, Route: RouteRetry},
		RunOperation{ID: "op", Kind: OpExecutionInvoke, IdempotencyKey: OpExecutionInvoke + "#initial|c|b", Attempt: 1, MaxAttempts: 3},
		&InvocationProvenance{DeadlineBound: bound, InvocationObservation: domain.InvocationObservation{TerminationCause: TerminationDeadlineReached}}
}

// INVARIANT (#210 composition): run exhaustion does not refuse the successor
// while outstanding review feedback has no grant yet - grantReviewContinuation
// grants one and the successor runs under it. Once a grant exists and is spent,
// it is refused.
func TestRunExhaustionWithPendingReviewDoesNotRefuseTheSuccessor(t *testing.T) {
	budgets := RunBudgets{WallLimit: 30 * time.Minute, MaxExecutionContinuations: 8}
	observed, _ := json.Marshal(FeedbackObservedPayload{FeedbackDecision: FeedbackDecision{Key: "review:1", Admitted: true}})
	events := []EngineeringEvent{{Type: EventFeedbackObserved, Payload: observed}}

	state := successorState(events, budgets)
	d, op, invocation := truncatedStop(BoundRunActiveWork)
	state.admitSuccessor(d, op, invocation, true, false, state.rt.deps.Clock.Now())
	if d.Bound != BoundRunActiveWork || d.Successor != SuccessorContinuation || d.SuccessorUnavailable != "" || d.Route == RouteStop {
		t.Fatalf("pending review recorded %+v, want the continuation left available", d)
	}

	grant, _ := json.Marshal(ReviewContinuationGrant{Allowance: time.Minute})
	state = successorState(append(events, EngineeringEvent{Type: EventReviewContinuationGranted, Payload: grant}), budgets)
	d, op, invocation = truncatedStop(BoundRunActiveWork)
	state.admitSuccessor(d, op, invocation, true, false, state.rt.deps.Clock.Now())
	if d.SuccessorUnavailable != "run_active_work_exhausted" || d.Route != RouteStop {
		t.Fatalf("spent review grant recorded %+v, want run_active_work_exhausted", d)
	}
}

// INVARIANT: a run whose provider-invocation total (a plan stage's headroom) is
// spent by this attempt advertises no successor, with run budget left.
func TestAnExhaustedInvocationTotalIsNotAdvertised(t *testing.T) {
	budgets := RunBudgets{WallLimit: time.Hour, MaxExecutionContinuations: 8, MaxProviderInvocations: 2}
	state := successorState(nil, budgets)
	state.projection.Attempts = map[string]int{OpExecutionInvoke: 1} // this attempt is the 2nd
	d, op, invocation := truncatedStop(BoundAttemptWall)
	state.admitSuccessor(d, op, invocation, false, true, state.rt.deps.Clock.Now())
	if d.Successor != SuccessorRetry || d.SuccessorUnavailable != "run_provider_invocations_exhausted" || d.Route != RouteStop {
		t.Fatalf("spent invocation total recorded %+v, want retry unavailable: run_provider_invocations_exhausted", d)
	}
}

// INVARIANT: zero remaining active work is exhausted. At EXACT equality the run
// fails rather than dispatching an attempt with no authority; the review grant
// shares the same boundary through runBudgetSpent.
func TestExactlySpentActiveWorkIsExhausted(t *testing.T) {
	start := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	state := conditionsFixture(start, &steppingClock{at: start.Add(30 * time.Minute)}, RunBudgets{WallLimit: 30 * time.Minute}, nil)
	if d, reason := state.conditions(); d != Failed || reason != "run_wall_budget_exhausted" {
		t.Fatalf("exactly spent run budget read as %s/%s", d, reason)
	}
	if !runBudgetSpent(30*time.Minute, 30*time.Minute) {
		t.Fatal("the review grant would not be due at exactly the spent budget")
	}
}
