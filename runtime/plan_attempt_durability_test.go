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
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	if !strings.Contains(strings.Join(attempt.Errors, " "), "named no id or no kind") {
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
