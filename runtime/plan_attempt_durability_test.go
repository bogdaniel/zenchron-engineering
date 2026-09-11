package runtime

// Refused reasoning proposals as durable, inspectable, NON-EXECUTABLE product
// state.
//
// Before #120 a first proposal that failed to compile wrote nothing at all:
// `plan list` said "no plans" and `plan show` said "no such plan" about work an
// operator had just spent a provider invocation on. These tests fix the
// opposite behaviour, and fix its limits: an attempt is evidence, it is never a
// plan, and it means the same thing after a restart because it IS the journal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// attemptFixture is a PlanService over a real store, with no plan in it.
type attemptFixture struct {
	store    *SQLiteOperationStore
	service  PlanService
	stateDir string
	clock    *fakeClock
}

func newAttemptFixture(t *testing.T) *attemptFixture {
	t.Helper()
	stateDir := t.TempDir()
	store, err := OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	clock := &fakeClock{now: time.Unix(1700000000, 0).UTC()}
	return &attemptFixture{
		store: store, stateDir: stateDir, clock: clock,
		service: PlanService{
			Store: store, Clock: clock, Agents: planAgents(), DefaultAgent: "codex",
			Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 3, MaxProviderInvocations: 12},
		},
	}
}

// refusedProposal is the exact #119 dogfood proposal: an acyclic decomposition
// whose two implementers each carry an independence requirement over nothing.
func refusedProposal(f *attemptFixture) ProposeInput {
	contract := planFixtureContract(&phase8Fixture{base: "b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0b0"})
	empty := func() *domain.IndependenceRequirement {
		return &domain.IndependenceRequirement{Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{}}
	}
	return ProposeInput{
		PlanID: "plan-attempt", Objective: "Resolve the M2 hardening cohort.",
		Subject:    domain.Subject{Repository: "acme/repo", Revision: contract.Subject.Revision},
		Repository: "acme/repo", Contract: contract,
		Model: domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1",
			Subject: domain.Subject{Repository: "acme/repo", Revision: contract.Subject.Revision}},
		Issue: 119,
		Reasoned: []domain.PlanStage{
			{ID: "harden-runtime-transitions", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "harden", Independence: empty()},
			{ID: "correct-operator-views", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "correct", DependsOn: []string{"harden-runtime-transitions"}, Independence: empty()},
			{ID: "verify-combined-candidate", Kind: domain.StageAgent, Role: domain.RoleReviewer,
				Objective: "review", DependsOn: []string{"correct-operator-views"}},
		},
		Reasoning: &domain.PlanReasoningProvenance{
			AgentID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
			TrustMode: domain.TrustRequirementOperatorTrusted, Model: "gpt-5",
			InvocationMode:        domain.InvocationModeNonMutatingPlanning,
			WorkspaceDigestBefore: strings.Repeat("a", 64), WorkspaceDigestAfter: strings.Repeat("a", 64),
			WorkspaceUnchanged: true,
		},
		References: []PlanSourceReferencePayload{
			{Repository: "acme/repo", Issue: 110, Digest: strings.Repeat("b", 64), Available: true},
			{Repository: "acme/repo", Issue: 112, Available: false, Detail: "issue #112 could not be read"},
		},
	}
}

// A refused FIRST proposal becomes durable, typed, inspectable state, and the
// refusal tells the operator where to read it.
func TestARefusedFirstProposalBecomesInspectablePlanState(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	transcript := filepath.Join(f.stateDir, "attempt-1.log")
	if err := os.WriteFile(transcript, []byte("planner said things"), 0o600); err != nil {
		t.Fatal(err)
	}
	input.Evidence = []Artifact{{
		Path: transcript, SHA256: textDigest("planner said things"),
		MediaType: "text/plain", LocalOnly: true,
	}}

	_, err := f.service.Propose(context.Background(), input)
	if err == nil {
		t.Fatal("the dogfood proposal compiled: the producer-independence refusal is not reachable")
	}
	var refused *PlanAttemptRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the refusal is not the typed attempt refusal: %T %v", err, err)
	}
	var invalid *planning.ValidationError
	if !errors.As(err, &invalid) {
		t.Fatalf("the underlying deterministic refusal was lost: %T %v", err, err)
	}
	if !strings.Contains(err.Error(), "autonomy plan show plan-attempt") {
		t.Fatalf("the refusal does not tell the operator how to read the attempt: %v", err)
	}

	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatalf("the refused attempt is not readable: %v", err)
	}
	if view.Executable {
		t.Fatal("a refused attempt reports an executable plan")
	}
	if len(view.Attempts) != 1 {
		t.Fatalf("attempts = %#v", view.Attempts)
	}
	attempt := view.Attempts[0]
	switch {
	case attempt.AttemptID != refused.AttemptID:
		t.Fatalf("attempt id = %q, refusal named %q", attempt.AttemptID, refused.AttemptID)
	case attempt.Origin != domain.ProposalOriginInitial:
		t.Fatalf("origin = %q", attempt.Origin)
	case attempt.Issue != 119:
		t.Fatalf("issue = %d", attempt.Issue)
	case attempt.Reasoning == nil || attempt.Reasoning.AgentID != "codex":
		t.Fatalf("reasoning provenance = %#v", attempt.Reasoning)
	case len(attempt.Errors) == 0:
		t.Fatal("the attempt records no typed validation error")
	case len(attempt.Stages) != 3:
		t.Fatalf("the proposed decomposition was not preserved: %#v", attempt.Stages)
	case len(attempt.Evidence) != 1 || attempt.Evidence[0].Path != transcript:
		t.Fatalf("the transcript reference was not preserved: %#v", attempt.Evidence)
	}
	// The ambiguous shorthand is preserved AS ITSELF. It is the fact that
	// explains the refusal, and normalizing it away would leave an operator
	// reading a record of a proposal nobody made.
	first := attempt.Stages[0]
	if first.Independence == nil {
		t.Fatal("the proposed independence requirement was recorded as absent")
	}
	if len(first.Independence.DifferentFrom) != 0 {
		t.Fatalf("different_from = %#v, want the empty list the model proposed", first.Independence.DifferentFrom)
	}
	// Hydration provenance, including what could NOT be read.
	if len(attempt.References) != 2 || attempt.References[1].Available {
		t.Fatalf("referenced-source provenance = %#v", attempt.References)
	}
}

// The attempt is not a plan: nothing lists it as executable work, nothing reads
// a document for it, and there is no revision to approve.
func TestARefusedAttemptIsNotExecutableState(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), refusedProposal(f)); err == nil {
		t.Fatal("the proposal compiled")
	}

	plans, err := f.store.Plans()
	if err != nil {
		t.Fatal(err)
	}
	if len(plans) != 0 {
		t.Fatalf("a refused attempt appeared as an executable plan: %#v", plans)
	}
	if _, found, err := f.store.Plan("plan-attempt"); err != nil || found {
		t.Fatalf("a plan document was stored for a proposal that never compiled (found=%v, err=%v)", found, err)
	}
	if _, found, err := f.store.PlanRevision("plan-attempt", 1); err != nil || found {
		t.Fatalf("revision 1 exists for a proposal that never compiled (found=%v, err=%v)", found, err)
	}
	// Approval cannot reach it, because there is no revision to name.
	if _, err := f.service.Approve("plan-attempt", 1, strings.Repeat("c", 64), "x", "operator", ""); err == nil {
		t.Fatal("a refused attempt was approvable")
	}
	// But the identity IS listed, which is what `plan list` reads.
	identities, err := f.store.PlanIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].PlanID != "plan-attempt" || identities[0].Revision != 0 {
		t.Fatalf("plan identities = %#v", identities)
	}
}

// The record survives a restart with the same meaning, because it is a
// projection of the durable journal rather than anything a process held.
func TestARefusedAttemptSurvivesRestartUnchanged(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), refusedProposal(f)); err == nil {
		t.Fatal("the proposal compiled")
	}
	before, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	beforeState, err := f.store.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	service := PlanService{Store: reopened, Clock: f.clock, Agents: planAgents(), DefaultAgent: "codex"}

	after, err := service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatalf("the attempt disappeared across a restart: %v", err)
	}
	afterState, err := reopened.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if afterState.StateSHA256 != beforeState.StateSHA256 {
		t.Fatalf("the replayed plan state changed across a restart: %s -> %s",
			short12(beforeState.StateSHA256), short12(afterState.StateSHA256))
	}
	if len(after.Attempts) != len(before.Attempts) || after.Executable != before.Executable {
		t.Fatalf("the attempt changed meaning across a restart: %#v -> %#v", before, after)
	}
	if after.Attempts[0].AttemptID != before.Attempts[0].AttemptID ||
		strings.Join(after.Attempts[0].Errors, "|") != strings.Join(before.Attempts[0].Errors, "|") {
		t.Fatalf("the attempt's identity or reasons changed across a restart: %#v", after.Attempts[0])
	}
}

// Two refusals are two attempts. A recurring defect must not look like one
// event that happened once.
func TestRepeatedRefusalsAccumulateAsSeparateAttempts(t *testing.T) {
	f := newAttemptFixture(t)
	for i := 0; i < 2; i++ {
		if _, err := f.service.Propose(context.Background(), refusedProposal(f)); err == nil {
			t.Fatalf("attempt %d compiled", i+1)
		}
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Attempts) != 2 {
		t.Fatalf("attempts = %d, want 2", len(view.Attempts))
	}
	if view.Attempts[0].AttemptID == view.Attempts[1].AttemptID {
		t.Fatalf("both attempts share the identity %q", view.Attempts[0].AttemptID)
	}
}

// A plan whose first attempt was refused can still be planned. The attempt
// identity is not a claim on the plan: promoting it to a real revision is the
// ordinary path, and refusing it as "created concurrently" would have made one
// bad proposal permanent.
func TestAPlanWhoseFirstAttemptFailedCanStillBeProposed(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), refusedProposal(f)); err == nil {
		t.Fatal("the first proposal compiled")
	}

	good := refusedProposal(f)
	good.Reasoned[0].Independence = nil
	good.Reasoned[1].Independence = nil
	plan, err := f.service.Propose(context.Background(), good)
	if err != nil {
		t.Fatalf("the corrected proposal was refused after a failed attempt: %v", err)
	}
	if plan.Revision != 1 {
		t.Fatalf("revision = %d, want the first revision", plan.Revision)
	}
	stored, found, err := f.store.Plan("plan-attempt")
	if err != nil || !found {
		t.Fatalf("the plan was not stored (found=%v, err=%v)", found, err)
	}
	if stored.Digest != plan.Digest {
		t.Fatalf("the stored plan is not the one returned: %s vs %s", short12(stored.Digest), short12(plan.Digest))
	}
	// The earlier refusal is still readable beside the plan that replaced it.
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if !view.Executable || len(view.Attempts) != 1 {
		t.Fatalf("attempts view after a successful proposal = %#v", view)
	}
}

// A reasoning invocation whose ANSWER could not be read is durable too.
//
// The #120 dogfood failed here rather than at compilation: the model answered
// correctly and the runtime read the wrong object out of the transcript. That
// refusal happens before any proposal exists to compile, and it used to leave
// nothing behind at all - the same product gap, one layer further up.
func TestAPlannerRefusalBeforeCompilationIsDurableToo(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	// No stages: the answer never decoded, so there is no proposal.
	input.Reasoned = nil
	transcript := filepath.Join(f.stateDir, "unreadable.log")
	if err := os.WriteFile(transcript, []byte("the provider said something else"), 0o600); err != nil {
		t.Fatal(err)
	}
	input.Evidence = []Artifact{{
		Path: transcript, SHA256: textDigest("the provider said something else"),
		MediaType: "text/plain", LocalOnly: true,
	}}
	cause := &PlannerRefusedError{AgentID: "codex", Detail: `proposed stage "kebab-case-id" has kind "agent|assurance_gate|human_decision_gate", which is not a stage kind`}

	attemptID, err := f.service.RecordPlanningRefusal(input, cause)
	if err != nil {
		t.Fatalf("a planner refusal could not be recorded: %v", err)
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if view.Executable || len(view.Attempts) != 1 {
		t.Fatalf("attempts view = %#v", view)
	}
	attempt := view.Attempts[0]
	switch {
	case attempt.AttemptID != attemptID:
		t.Fatalf("attempt id = %q, want %q", attempt.AttemptID, attemptID)
	case attempt.Reasoning == nil || attempt.Reasoning.AgentID != "codex":
		t.Fatal("the reasoning provenance of an unreadable answer was lost")
	case len(attempt.Evidence) != 1:
		t.Fatal("the transcript of an unreadable answer was lost")
	case len(attempt.Errors) == 0 || !strings.Contains(strings.Join(attempt.Errors, " "), "not a stage kind"):
		t.Fatalf("the typed refusal was not preserved: %#v", attempt.Errors)
	}
	// It is still not a plan, and still not approvable.
	if _, found, err := f.store.Plan("plan-attempt"); err != nil || found {
		t.Fatalf("an unreadable answer produced a plan document (found=%v, err=%v)", found, err)
	}
}

// A proposal whose stages named no id or no kind is still recorded.
//
// The payload requires both, so copying an empty value through would make the
// journal refuse the record - losing it for exactly the proposal that was most
// broken. The substitution is stated as a reason rather than silently invented.
func TestAnAttemptRecordsStagesThatNamedNoIdentity(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	input.Reasoned = append(input.Reasoned, domain.PlanStage{Objective: "nameless"})

	if _, err := f.service.Propose(context.Background(), input); err == nil {
		t.Fatal("a stage with no id compiled")
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatalf("the attempt was not recorded at all: %v", err)
	}
	if len(view.Attempts) != 1 {
		t.Fatalf("attempts = %#v", view.Attempts)
	}
	attempt := view.Attempts[0]
	found := false
	for _, stage := range attempt.Stages {
		if strings.Contains(stage.ID, "unnamed stage") && stage.Kind == "(unstated)" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the nameless stage is not in the record: %#v", attempt.Stages)
	}
	if !strings.Contains(strings.Join(attempt.Errors, " "), "shortened or filled in") {
		t.Fatalf("the substitution was not stated as a reason: %#v", attempt.Errors)
	}
}

// Promoting an attempt to a real plan carries the revision's own repository.
func TestPromotingAnAttemptCarriesTheRevisionRepository(t *testing.T) {
	f := newAttemptFixture(t)
	attempt := refusedProposal(f)
	attempt.Repository = "acme/named-by-the-proposer"
	if _, err := f.service.Propose(context.Background(), attempt); err == nil {
		t.Fatal("the proposal compiled")
	}

	good := refusedProposal(f)
	good.Reasoned[0].Independence = nil
	good.Reasoned[1].Independence = nil
	if _, err := f.service.Propose(context.Background(), good); err != nil {
		t.Fatalf("the corrected proposal was refused: %v", err)
	}
	identities, err := f.store.PlanIdentities()
	if err != nil {
		t.Fatal(err)
	}
	if len(identities) != 1 || identities[0].Repository != good.Subject.Repository {
		t.Fatalf("the promoted plan reports repository %#v, want %q", identities, good.Subject.Repository)
	}
}

// Two refusals of the SAME plan revision, racing, get two identities.
//
// This is the reachable production race, not a hypothetical one. The supervisor
// answers control connections concurrently - ControlListener.Serve spawns a
// goroutine per connection - and proposeSerialized deliberately runs the
// planning invocation OUTSIDE the plan lock, because holding it across a
// provider call would stall every run in the fleet. So two operators asking to
// plan the same issue at the same time reach this record concurrently, inside
// one process, where no advisory file lock separates them.
//
// The test drives the seam the race actually runs through rather than the
// control endpoint above it: the causal claim is about read-then-append, and
// exercising it directly means the proof does not depend on whether the layers
// above happen to serialize today. Nothing here takes a lock; the journal's own
// append transaction is what makes the allocation atomic.
func TestConcurrentRefusalsOfOneRevisionGetDistinctIdentities(t *testing.T) {
	f := newAttemptFixture(t)
	// The plan identity exists first, so both racers are recording against one
	// stream rather than also racing to create it - which is a different,
	// already-settled question (ClaimPlanAttempt is a conditional insert).
	if _, err := f.store.ClaimPlanAttempt("plan-attempt", "acme/repo", f.clock.Now()); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	start := make(chan struct{})
	identities := make([]string, racers)
	failures := make([]error, racers)
	var waiting, done sync.WaitGroup
	waiting.Add(racers)
	done.Add(racers)
	for i := 0; i < racers; i++ {
		go func(i int) {
			defer done.Done()
			input := refusedProposal(f)
			cause := &PlannerRefusedError{AgentID: "codex", Detail: fmt.Sprintf("racer %d could not read the answer", i)}
			waiting.Done()
			<-start
			identities[i], failures[i] = f.service.RecordPlanningRefusal(input, cause)
		}(i)
	}
	waiting.Wait()
	close(start)
	done.Wait()

	for i, err := range failures {
		if err != nil {
			t.Fatalf("racer %d lost its attempt entirely: %v", i, err)
		}
	}
	distinct := map[string]int{}
	for i, id := range identities {
		if first, clash := distinct[id]; clash {
			t.Fatalf("racers %d and %d were filed under one identity %q", first, i, id)
		}
		distinct[id] = i
	}

	// NO LOST EVENT. Two invocations were refused, so two attempts are durable.
	snapshot, err := f.store.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Attempts) != racers {
		t.Fatalf("attempts = %d, want %d: %#v", len(snapshot.Attempts), racers, snapshot.Attempts)
	}
	seen := map[string]bool{}
	for _, attempt := range snapshot.Attempts {
		if seen[attempt.AttemptID] {
			t.Fatalf("duplicate attempt identity %q in the durable record", attempt.AttemptID)
		}
		seen[attempt.AttemptID] = true
		if attempt.Revision != 1 {
			t.Fatalf("attempt %q names revision %d", attempt.AttemptID, attempt.Revision)
		}
	}
	// The identities the callers were TOLD are the identities on disk. A caller
	// handed an id that is not the one recorded would send an operator to read
	// an attempt that does not exist.
	for i, id := range identities {
		if !seen[id] {
			t.Fatalf("racer %d was told identity %q, which is not in the durable record", i, id)
		}
	}
	// And they are the contiguous ordinals the allocator promises, in either
	// order the race resolved.
	for ordinal := 1; ordinal <= racers; ordinal++ {
		if want := PlanAttemptID("plan-attempt", 1, ordinal); !seen[want] {
			t.Fatalf("the allocated ordinals are not contiguous: %q is missing from %v", want, seen)
		}
	}

	// RESTART preserves both, because the identities are in the journal rather
	// than in anything the process held.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	after, err := reopened.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Attempts) != racers {
		t.Fatalf("attempts after restart = %d, want %d", len(after.Attempts), racers)
	}
	for i, attempt := range after.Attempts {
		if attempt.AttemptID != snapshot.Attempts[i].AttemptID {
			t.Fatalf("attempt %d changed identity across a restart: %q -> %q",
				i, snapshot.Attempts[i].AttemptID, attempt.AttemptID)
		}
	}
	if after.StateSHA256 != snapshot.StateSHA256 {
		t.Fatalf("the replayed state changed across a restart: %s -> %s",
			short12(snapshot.StateSHA256), short12(after.StateSHA256))
	}
}

// The identity is the JOURNAL's to allocate, and a caller that chooses one is
// refused rather than silently overwritten.
func TestACallerMayNotChooseAnAttemptIdentity(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.store.ClaimPlanAttempt("plan-attempt", "acme/repo", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	_, err := f.store.AppendPlanEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "e-1", PlanID: "plan-attempt",
		Type: EventPlanAttemptRefused, OccurredAt: f.clock.Now(),
		Payload: marshalPayload(t, PlanAttemptRefusedPayload{
			AttemptID: "attempt-i-picked-this", Revision: 1,
			Origin: domain.ProposalOriginInitial, Errors: []string{"refused"},
		}),
	})
	if err == nil || !strings.Contains(err.Error(), "allocated by the journal") {
		t.Fatalf("a caller-chosen attempt identity was accepted: %v", err)
	}
}

// The identity is a function of the STREAM, not of the payload handed in.
//
// This is the causal core of the race, proven without concurrency: two appends
// carrying the BYTE-IDENTICAL payload - same placeholder, same revision, the
// view a caller would have built from a stream it read once - come out as
// different attempts. A caller's view of what was already there cannot
// influence the identity, so a stale view cannot mint a duplicate.
func TestAttemptIdentityIsAFunctionOfTheStreamNotThePayload(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.store.ClaimPlanAttempt("plan-attempt", "acme/repo", f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	payload := PlanAttemptRefusedPayload{
		AttemptID: PendingAttemptID, Revision: 1,
		Origin: domain.ProposalOriginInitial, Errors: []string{"refused"},
	}
	var allocated []string
	for i := 0; i < 2; i++ {
		appended, err := appendPlanEventWithArtifacts(f.store, f.clock.Now(), "plan-attempt",
			EventPlanAttemptRefused, payload, nil)
		if err != nil {
			t.Fatalf("append %d: %v", i+1, err)
		}
		var recorded PlanAttemptRefusedPayload
		if err := json.Unmarshal(appended.Payload, &recorded); err != nil {
			t.Fatal(err)
		}
		allocated = append(allocated, recorded.AttemptID)
	}
	for ordinal, id := range allocated {
		if want := PlanAttemptID("plan-attempt", 1, ordinal+1); id != want {
			t.Fatalf("append %d allocated %q, want %q", ordinal+1, id, want)
		}
	}
	// The placeholder the caller passed is never what gets stored.
	snapshot, err := f.store.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	for _, attempt := range snapshot.Attempts {
		if attempt.AttemptID == PendingAttemptID {
			t.Fatal("the placeholder reached the durable record")
		}
	}
}

// Model-supplied strings that exceed the journal's field bounds are truncated,
// not allowed to lose the whole record.
//
// The payload validator refuses any field or list element over its bound, so an
// over-long stage id, role or dependency made the append fail and Propose
// reported "the refused attempt could not be recorded" - the exact
// invocation-spent-nothing-durable gap this record exists to close, reachable by
// ordinary bad model output.
func TestAnAttemptRecordsStagesWhoseStringsExceedTheFieldBounds(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	huge := strings.Repeat("x", maxPayloadFieldBytes*3)
	input.Reasoned = append(input.Reasoned, domain.PlanStage{
		ID: huge, Kind: domain.StageAgent, Role: domain.RoleImplementer,
		DependsOn: []string{huge, huge},
		Independence: &domain.IndependenceRequirement{
			Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{huge},
		},
	})

	if _, err := f.service.Propose(context.Background(), input); err == nil {
		t.Fatal("the proposal compiled")
	} else if strings.Contains(err.Error(), "could not be recorded") {
		t.Fatalf("an over-long stage id lost the whole attempt: %v", err)
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatalf("the attempt was not recorded: %v", err)
	}
	attempt := view.Attempts[0]
	for _, stage := range attempt.Stages {
		if len(stage.ID) > maxPayloadFieldBytes || len(stage.Role) > maxPayloadFieldBytes {
			t.Fatalf("a recorded stage exceeds the field bound: %#v", stage)
		}
		for _, dependency := range stage.DependsOn {
			if len(dependency) > maxPayloadListItemBytes {
				t.Fatalf("a recorded dependency exceeds the element bound: %d bytes", len(dependency))
			}
		}
		if stage.Independence != nil {
			for _, peer := range stage.Independence.DifferentFrom {
				if len(peer) > maxPayloadListItemBytes {
					t.Fatalf("a recorded independence peer exceeds the element bound: %d bytes", len(peer))
				}
			}
		}
	}
	if !strings.Contains(strings.Join(attempt.Errors, " "), "shortened or filled in") {
		t.Fatalf("the truncation was not stated as a reason: %#v", attempt.Errors)
	}
}

// The truncation notices keep their slots when the validator produced more
// reasons than the record holds.
//
// Appending the notices to a full list and then cutting the result dropped
// exactly them, so the record understated itself and said nothing about doing
// so - which is the one thing the notices exist to report. The cause is built
// directly here because the compiler stops at the first offending stage, so the
// only way to reach a reason flood is to state one.
func TestTruncationNoticesSurviveAFloodOfReasons(t *testing.T) {
	f := newAttemptFixture(t)
	input := refusedProposal(f)
	for i := 0; i < maxPayloadListItems+4; i++ {
		input.Reasoned = append(input.Reasoned, domain.PlanStage{
			ID: fmt.Sprintf("producer-%d", i), Kind: domain.StageAgent,
			Role: domain.RoleImplementer, Objective: "work",
		})
	}
	reasons := make([]string, 0, 40)
	for i := 0; i < 40; i++ {
		reasons = append(reasons, fmt.Sprintf("deterministic reason %d", i))
	}
	cause := &planning.ValidationError{PlanID: input.PlanID, Reasons: reasons}

	if _, err := f.service.RecordPlanningRefusal(input, cause); err != nil {
		t.Fatalf("the attempt was lost: %v", err)
	}
	view, err := f.service.AttemptsView("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	recorded := view.Attempts[0].Errors
	if len(recorded) > maxPayloadListItems {
		t.Fatalf("the record holds %d reasons, above the %d bound", len(recorded), maxPayloadListItems)
	}
	joined := strings.Join(recorded, " | ")
	for _, want := range []string{
		"further proposed stages are not recorded",
		"further deterministic reasons are not recorded",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("the record understates itself with no notice of %q:\n%s", want, joined)
		}
	}
	// And the reasons themselves did not all vanish to make room.
	if !strings.Contains(joined, "deterministic reason 0") {
		t.Fatalf("the notices displaced every reason:\n%s", joined)
	}
}

// An independence requirement over NOTHING is recorded as the empty list it
// was, in the durable bytes and after replay.
//
// different_from is deliberately not omitempty, so a nil slice encodes as
// `null` - and `null` is the one shape this record must not produce. The exact
// proposal was "independent of nothing in execution_agent", which is the state
// #120 was opened about; a record that renders it as an absent member has lost
// the evidence it exists to hold.
func TestAnEmptyDifferentFromIsRecordedAsAnEmptyList(t *testing.T) {
	f := newAttemptFixture(t)
	if _, err := f.service.Propose(context.Background(), refusedProposal(f)); err == nil {
		t.Fatal("the dogfood proposal compiled")
	}

	// The DURABLE BYTES, not just the decoded value.
	events, err := f.store.PlanEvents("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Type != EventPlanAttemptRefused {
			continue
		}
		found = true
		canonical, err := CanonicalJSON(event)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(canonical), `"different_from":null`) {
			t.Fatalf("an explicitly empty different_from was stored as null:\n%s", canonical)
		}
		if !strings.Contains(string(canonical), `"different_from":[]`) {
			t.Fatalf("the stored payload does not record an empty different_from:\n%s", canonical)
		}
	}
	if !found {
		t.Fatal("no refused-attempt event was recorded")
	}

	// And REPLAY returns a non-nil zero-length slice, so a reader sees the same
	// thing the bytes say.
	snapshot, err := f.store.ReplayPlan("plan-attempt")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, stage := range snapshot.Attempts[0].Stages {
		if stage.Independence == nil {
			continue
		}
		checked++
		if stage.Independence.DifferentFrom == nil {
			t.Fatalf("stage %q replayed different_from as nil rather than as the empty list it stated", stage.ID)
		}
		if len(stage.Independence.DifferentFrom) != 0 {
			t.Fatalf("stage %q replayed different_from = %#v", stage.ID, stage.Independence.DifferentFrom)
		}
	}
	if checked == 0 {
		t.Fatal("no proposed stage carried an independence requirement")
	}
}
