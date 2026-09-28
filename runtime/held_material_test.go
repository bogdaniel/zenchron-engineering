package runtime

// #203: completed material work must not be silently made useless by a later
// budget boundary. Each test names the invariant it proves. Clocks are injected
// and providers scripted; nothing here reaches a live provider or real time.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// burningAssurance charges the fixture clock once for a verifier's time, so the
// envelope is gone at the exact moment the candidate becomes publishable - the
// run-9d2a446bc6071568ce3030503b01bd57 shape. (Salvaged from PR #208's test.)
type burningAssurance struct {
	inner AssuranceProvider
	clock *steppingClock
	burn  time.Duration
	spent bool
}

func (b *burningAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	if producer, ok := b.inner.(EvidenceProducer); ok {
		return producer.ProducedEvidenceClasses()
	}
	return nil
}

func (b *burningAssurance) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	result, err := b.inner.Assure(ctx, request)
	if !b.spent {
		b.spent = true
		b.clock.advance(b.burn)
	}
	return result, err
}

// terminalRecord is the run.failed payload the journal holds, decoded.
func terminalRecord(t *testing.T, events []EngineeringEvent) (dispositionRecord, int) {
	t.Helper()
	var record dispositionRecord
	count := 0
	for _, e := range events {
		if e.Type != EventRunFailed {
			continue
		}
		count++
		decoded, err := decodePayload[dispositionRecord](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		record = decoded
	}
	return record, count
}

func notPublished(t *testing.T, f *phase8Fixture, runID string) {
	t.Helper()
	if n := countMethod(f.forge.Calls, "CreatePullRequest"); n != 0 {
		t.Fatalf("held material was published: %d pull request(s) created", n)
	}
	if _, err := runGit(f.origin, "rev-parse", "--verify", "--quiet", "refs/heads/"+candidateBranch(runID)); err == nil {
		t.Fatal("held material was pushed to the remote")
	}
}

// INVARIANT (shape 1): a verified candidate cannot disappear into an ordinary
// budget failure. The run still ends - no budget is extended - but the terminal
// event names the exact verified commit, the step it could not take, and holds
// it; nothing is published.
func TestAVerifiedCandidateIsHeldWhenTheBudgetEndsTheRun(t *testing.T) {
	fixture, runID, outcome := verifiedHeldRun(t)
	if outcome.Disposition != Failed || outcome.Reason != "run_wall_budget_exhausted" {
		t.Fatalf("outcome %s/%s, want the budget to still end the run", outcome.Disposition, outcome.Reason)
	}
	state := fixture.state(runID)
	held := state.snapshot.HeldMaterial
	if held == nil {
		t.Fatalf("verified candidate %s disappeared into a bare %s", state.projection.CandidateRevision, outcome.Reason)
	}
	want := HeldMaterial{Kind: HeldVerifiedUnpublished, Revision: state.projection.CandidateRevision, Tree: state.projection.CandidateTree,
		NextStep: OpBaseIntegrate, BlockedBy: "run_wall_budget_exhausted", Disposition: HeldDisposition}
	if *held != want {
		t.Fatalf("held %+v, want %+v", *held, want)
	}
	notPublished(t, fixture, runID)
}

// verifiedHeldRun reproduces run-9d2a446: the envelope is gone the moment the
// last verifier answers.
func verifiedHeldRun(t *testing.T) (*phase8Fixture, string, Outcome) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	fixture.deps.Budgets = RunBudgets{WallLimit: 30 * time.Minute, MaxExecutionAttempts: 2, MaxRemediationAttempts: 2, MaxAssuranceAttempts: 2}
	fixture.deps.SemanticAssurance = &burningAssurance{inner: fixture.deps.SemanticAssurance, clock: fixture.clock, burn: 31 * time.Minute}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	return fixture, runID, fixture.reconcile(runID)
}

// INVARIANT: verified_unpublished is claimed only for EXACT-head evidence.
// Automated assurance that passed at an older head, or a failing independent
// semantic finding at the head, is committed_unverified.
func TestVerifiedUnpublishedRequiresExactHeadAndPassingSemantic(t *testing.T) {
	fixture, runID, _ := verifiedHeldRun(t)
	kind := func(mutate func(p *RunProjection)) string {
		state := fixture.state(runID)
		mutate(&state.projection)
		held := state.heldMaterial(ReasonRunWallBudgetExhausted)
		if held == nil {
			t.Fatal("no material derived")
		}
		return held.Kind
	}
	if got := kind(func(*RunProjection) {}); got != HeldVerifiedUnpublished {
		t.Fatalf("baseline kind %q, want verified_unpublished", got)
	}
	if got := kind(func(p *RunProjection) {
		older := *p.Assurance
		older.Commit = fixture.base
		p.Assurance = &older
	}); got != HeldCommittedUnverified {
		t.Fatalf("assurance at an older head gave %q, want committed_unverified", got)
	}
	if got := kind(func(p *RunProjection) {
		if p.SemanticAssurance == nil || p.SemanticAssurance.Commit != p.CandidateRevision {
			t.Fatal("scenario has no head semantic finding")
		}
		failing := *p.SemanticAssurance
		failing.Passed = false
		p.SemanticAssurance = &failing
	}); got != HeldCommittedUnverified {
		t.Fatalf("a failing semantic finding at the head gave %q, want committed_unverified", got)
	}
}

// INVARIANT (shapes 2 and 3): a productive attempt truncated by run active-work
// exhaustion leaves UNCOMMITTED work (#328 fails the run before the commit).
// It is named by its producing operation and exact content, bound to the
// commit it sits on, its next step is the commit, and its selected
// continuation is reported unavailable with the budget reason.
func TestUncommittedMaterialIsHeldWithItsUnadmittableSuccessor(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 50 * time.Minute, MaxExecutionAttempts: 5},
		wallStep{}, wallStep{mutate: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	held := state.snapshot.HeldMaterial
	if state.snapshot.Reason != "run_wall_budget_exhausted" || held == nil {
		t.Fatalf("run %s held %+v, want run_wall_budget_exhausted holding the productive attempt's work", state.snapshot.Reason, held)
	}
	ops, _ := executions(t, state)
	workspace := candidateDir(fixture.deps.StateDir, runID)
	paths, err := candidateChangedPaths(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if held.Kind != HeldUncommitted || held.Operation != ops[0].ID || held.PathCount != 1 ||
		held.ContentDigest == "" || held.ContentDigest != workspaceContentDigest(workspace, paths) {
		t.Fatalf("held %+v, want the uncommitted change of %s identified by its content", *held, ops[0].ID)
	}
	if held.Revision != fixture.base || held.NextStep != OpCandidateCommit {
		t.Fatalf("held revision=%s next=%s, want the base %s and candidate.commit", held.Revision, held.NextStep, fixture.base)
	}
	if held.Successor != SuccessorContinuation || held.SuccessorUnavailable != "run_active_work_exhausted" {
		t.Fatalf("held successor %q unavailable %q, want continuation unavailable: run_active_work_exhausted", held.Successor, held.SuccessorUnavailable)
	}
	notPublished(t, fixture, runID)
}

// INVARIANT (shape 3): a committed checkpoint whose selected continuation is
// refused by the continuation ceiling is held as a checkpoint, and the refusal
// is the continuation authority, not a generic failure.
func TestACheckpointWhoseContinuationIsNotAdmissibleIsHeld(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 5 * time.Minute, MaxExecutionAttempts: 3, MaxExecutionContinuations: 1},
		wallStep{mutate: true}, wallStep{mutate: true}, wallStep{mutate: true})
	runID := fixture.start()
	drive(fixture, provider, runID)

	state := fixture.state(runID)
	held := state.snapshot.HeldMaterial
	if held == nil {
		t.Fatalf("run %s/%s held nothing after %d checkpoint(s)", state.snapshot.Disposition, state.snapshot.Reason, state.projection.Checkpoints)
	}
	if held.BlockedBy != "execution_continuations_exhausted" || state.snapshot.Reason != held.BlockedBy {
		t.Fatalf("held blocked by %q, run reason %q", held.BlockedBy, state.snapshot.Reason)
	}
	if held.Kind != HeldCheckpoint || held.Revision != state.projection.CandidateRevision ||
		held.Tree != state.projection.CandidateTree || held.NextStep != OpExecutionInvoke {
		t.Fatalf("held %+v, want the checkpoint %s whose next step is its continuation", *held, state.projection.CandidateRevision)
	}
	if held.Successor != SuccessorContinuation || held.SuccessorUnavailable != "execution_continuations_exhausted" {
		t.Fatalf("held %+v, want continuation unavailable: execution_continuations_exhausted", *held)
	}
}

// INVARIANT: exhaustion with no material result is distinguishable from
// exhaustion holding material - a zero-delta run holds nothing.
func TestExhaustionWithNoMaterialHoldsNothing(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 5 * time.Minute, MaxExecutionAttempts: 2},
		wallStep{}, wallStep{})
	runID := fixture.start()
	drive(fixture, provider, runID)
	state := fixture.state(runID)
	if !BudgetBoundary(state.snapshot.Disposition, state.snapshot.Reason) {
		t.Fatalf("run %s/%s, want a budget boundary", state.snapshot.Disposition, state.snapshot.Reason)
	}
	if state.snapshot.HeldMaterial != nil {
		t.Fatalf("a run that produced nothing claims to hold %+v", *state.snapshot.HeldMaterial)
	}
	record, _ := terminalRecord(t, journalOf(t, fixture.runtime, runID))
	if record.HeldMaterial != nil {
		t.Fatal("the journal records held material for a run that produced none")
	}
}

// INVARIANT: restart and replay reproduce the same held identity and
// disposition from the journal - not from mutable configuration - and holding
// renews nothing: another reconcile under a WIDER configuration appends no
// terminal event, starts no attempt, spends no budget and publishes nothing.
func TestHeldMaterialSurvivesRestartAndRenewsNoAuthority(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 50 * time.Minute, MaxExecutionAttempts: 5},
		wallStep{}, wallStep{mutate: true})
	provider.steps = append(provider.steps, wallStep{mutate: true, complete: true})
	runID := fixture.start()
	drive(fixture, provider, runID)
	before := fixture.state(runID)
	if before.snapshot.HeldMaterial == nil {
		t.Fatal("scenario held nothing")
	}
	events := journalOf(t, fixture.runtime, runID)
	invocations := len(provider.requests)

	// A restarted controller with a much wider configuration.
	deps := fixture.deps
	deps.Budgets = RunBudgets{WallLimit: 10 * time.Hour, AttemptWallLimit: 5 * time.Hour, MaxExecutionAttempts: 9, MaxExecutionContinuations: 9}
	fixture.clock.advance(time.Hour)
	fixture.runtime = fixture.newRuntime(deps)
	for pass := 0; pass < 3; pass++ {
		fixture.reconcile(runID)
	}
	after := fixture.state(runID)
	if !reflect.DeepEqual(after.snapshot.HeldMaterial, before.snapshot.HeldMaterial) {
		t.Fatalf("held material changed across restart: %+v -> %+v", *before.snapshot.HeldMaterial, after.snapshot.HeldMaterial)
	}
	if after.snapshot.Disposition != Failed || after.snapshot.Reason != before.snapshot.Reason {
		t.Fatalf("disposition moved to %s/%s", after.snapshot.Disposition, after.snapshot.Reason)
	}
	if len(provider.requests) != invocations {
		t.Fatalf("holding manufactured %d new provider attempt(s)", len(provider.requests)-invocations)
	}
	if !reflect.DeepEqual(after.run.Budgets, before.run.Budgets) {
		t.Fatalf("the run's frozen budgets moved: %+v -> %+v", before.run.Budgets, after.run.Budgets)
	}
	if !reflect.DeepEqual(after.snapshot.Operations, before.snapshot.Operations) {
		t.Fatal("holding changed operation state: an attempt or budget was spent or renewed")
	}
	if got := journalOf(t, fixture.runtime, runID); len(got) != len(events) {
		t.Fatalf("journal grew from %d to %d events after the run was held", len(events), len(got))
	}
	// Pure replay of the stored journal reads the same record back.
	replayed, err := Reduce(after.run, events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.HeldMaterial, before.snapshot.HeldMaterial) {
		t.Fatalf("replay reconstructed %+v, want the recorded %+v", replayed.HeldMaterial, *before.snapshot.HeldMaterial)
	}
	report, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report.HeldMaterial, before.snapshot.HeldMaterial) {
		t.Fatalf("status reports %+v", report.HeldMaterial)
	}
	notPublished(t, fixture, runID)
}

// INVARIANT: GC does not destroy held material. A failed run past retention
// keeps its candidate workspace while it holds material; one that held none
// is reclaimed as before (TestGCRemovesEligibleTerminalRunMaterial).
func TestGCRetainsTheWorkspaceOfHeldMaterial(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-held")
	m := f.material("run-held")
	payload, err := marshalPayloadJSON(dispositionRecord{Reason: "run_wall_budget_exhausted", HeldMaterial: &HeldMaterial{
		Kind: HeldVerifiedUnpublished, Revision: "b6f2c09", BlockedBy: "run_wall_budget_exhausted", Disposition: HeldDisposition}})
	if err != nil {
		t.Fatal(err)
	}
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "run-held-failed", RunID: "run-held",
		Type: EventRunFailed, OccurredAt: gcBase, Payload: payload})
	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := retainedReason(plan, m.candidate); !ok || !strings.Contains(reason, "verified_unpublished") {
		t.Fatalf("held workspace retained for %q (%v), want the held-material refusal", reason, ok)
	}
	if plan.Held.Workspaces != 1 || plan.Held.Bytes <= 0 {
		t.Fatalf("gc held summary %+v, want one workspace with its measured size", plan.Held)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "the held candidate workspace", m.candidate)
}

// INVARIANT: the held record is bounded and fits the 8192-byte canonical event
// ceiling at every field's bound; an unbounded or incomplete one is refused.
func TestTheHeldMaterialEventIsBounded(t *testing.T) {
	long := strings.Repeat("x", maxPayloadFieldBytes)
	held := HeldMaterial{Kind: HeldCommittedUnverified, Revision: long, Tree: long, Operation: long, PathCount: 1 << 30, ContentDigest: long,
		NextStep: long, BlockedBy: long, Successor: long, SuccessorUnavailable: long, Disposition: HeldDisposition}
	event := func(h HeldMaterial) EngineeringEvent {
		payload, err := marshalPayloadJSON(dispositionRecord{Reason: long, HeldMaterial: &h})
		if err != nil {
			t.Fatal(err)
		}
		return EngineeringEvent{Type: EventRunFailed, Payload: payload}
	}
	e := event(held)
	canonical, err := CanonicalJSON(e.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(canonical) > maxCanonicalPayloadBytes {
		t.Fatalf("a maximal held record is %d bytes, above the %d ceiling", len(canonical), maxCanonicalPayloadBytes)
	}
	if err := validateEventPayload(e); err != nil {
		t.Fatalf("a maximal legal held record was refused: %v", err)
	}
	over := held
	over.ContentDigest += "x"
	if err := validateEventPayload(event(over)); err == nil {
		t.Fatal("an over-bound held field was accepted")
	}
	missing := held
	missing.Kind = "salvaged"
	if err := validateEventPayload(event(missing)); err == nil {
		t.Fatal("held material with a kind outside the closed set was accepted")
	}
	undisposed := held
	undisposed.Disposition = "published"
	if err := validateEventPayload(event(undisposed)); err == nil {
		t.Fatal("held material with a disposition other than held was accepted")
	}
	// Only run.failed may carry held material.
	for _, eventType := range []string{EventRunWaiting, EventRunCompleted, EventRunCancelled} {
		e := event(held)
		e.Type = eventType
		if err := validateEventPayload(e); err == nil {
			t.Fatalf("%s carrying held material was accepted", eventType)
		}
	}
}

// INVARIANT: a derived record never makes the terminal event unappendable. A
// descriptive field is bounded; an identity field past the bound is UNKNOWN,
// never truncated into a different identity.
func TestAnOverBoundHeldRecordStaysAppendable(t *testing.T) {
	long := strings.Repeat("y", maxPayloadFieldBytes+1)
	held := HeldMaterial{Kind: HeldUncommitted, Revision: long, Tree: long, Operation: long, ContentDigest: long,
		NextStep: long, BlockedBy: ReasonRunWallBudgetExhausted + long, Successor: long, SuccessorUnavailable: long,
		Disposition: HeldDisposition}.bounded()
	if held.Revision != "" || held.Tree != "" || held.Operation != "" || held.ContentDigest != "" {
		t.Fatalf("an over-bound identity was kept or truncated: %+v", *held)
	}
	if err := held.validate(); err != nil {
		t.Fatalf("a bounded record is still refused: %v", err)
	}
}

// INVARIANT: the budget boundary is a CLOSED set plus an operation's attempt
// budget - not any reason that happens to end in _exhausted.
func TestBudgetBoundaryIsAClosedSet(t *testing.T) {
	for reason, want := range map[string]bool{
		ReasonRunWallBudgetExhausted: true, ReasonLifecycleDeadlineExhausted: true,
		ReasonContinuationsExhausted: true, ReasonProviderInvocationsExhausted: true,
		OpExecutionInvoke + "_attempts_exhausted": true,
		ReasonReviewBudgetExhausted:               false,
		"patience_exhausted":                      false,
		"no_progress":                             false,
	} {
		if got := BudgetBoundary(Failed, reason); got != want {
			t.Errorf("BudgetBoundary(failed, %q) = %v, want %v", reason, got, want)
		}
	}
	if BudgetBoundary(Waiting, ReasonRunWallBudgetExhausted) {
		t.Error("a waiting disposition is a budget boundary")
	}
}

// INVARIANT: the record is STICKY. A later re-settle of the same run - a
// lifecycle deadline after restart, an invariant violation - keeps the first
// record, journals the same one, and replay equals the live state.
func TestAHeldRecordSurvivesALaterReSettle(t *testing.T) {
	fixture, provider := wallFixture(t,
		RunBudgets{WallLimit: time.Hour, AttemptWallLimit: 50 * time.Minute, MaxExecutionAttempts: 5},
		wallStep{}, wallStep{mutate: true})
	runID := fixture.start()
	drive(fixture, provider, runID)
	original := fixture.state(runID).snapshot.HeldMaterial
	if original == nil {
		t.Fatal("scenario held nothing")
	}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	for _, reason := range []string{ReasonLifecycleDeadlineExhausted, "invariant_violation"} {
		state := fixture.state(runID)
		if err := fixture.runtime.recordDisposition(state, Failed, reason); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(state.snapshot.HeldMaterial, original) {
			t.Fatalf("re-settle as %s replaced the record: %+v", reason, state.snapshot.HeldMaterial)
		}
	}
	events := journalOf(t, fixture.runtime, runID)
	record, count := terminalRecord(t, events)
	if count != 3 || !reflect.DeepEqual(record.HeldMaterial, original) {
		t.Fatalf("%d run.failed events, last carrying %+v; want 3, each with the original record", count, record.HeldMaterial)
	}
	live := fixture.state(runID)
	replayed, err := Reduce(live.run, events)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.HeldMaterial, original) || !reflect.DeepEqual(live.snapshot.HeldMaterial, original) {
		t.Fatalf("replay %+v / live %+v, want the original %+v", replayed.HeldMaterial, live.snapshot.HeldMaterial, *original)
	}
}
