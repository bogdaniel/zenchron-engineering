package runtime

// Plan persistence, in the store and the journal that already exist.
//
// The properties under test are the ones #64 freezes: a revision is immutable,
// consumed budget can never be reset, unknown cost stays unknown, gates own no
// runs, and a restart reconstructs everything from durable state alone. Each is
// asserted by writing, closing the store, reopening it, and replaying - because
// "survives restart" is not a property you can check without one.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func openPlanStore(t *testing.T) (string, *SQLiteOperationStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return dir, store
}

func reopenStore(t *testing.T, dir string) *SQLiteOperationStore {
	t.Helper()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// planFixture is a schema-valid plan with two agent stages and one assurance
// gate: enough graph for "gates create no runs" to be a real assertion.
func planFixture(t *testing.T, id string, revision int) domain.EngineeringPlan {
	t.Helper()
	plan := domain.EngineeringPlan{
		SchemaVersion:  domain.SchemaVersion,
		ID:             id,
		Revision:       revision,
		Objective:      "Add enterprise SSO to the payments service.",
		Subject:        domain.Subject{Repository: "acme/payments", Revision: "rev-b"},
		BudgetEnvelope: domain.PlanBudgetEnvelope{MaxChildRuns: 8, MaxConcurrency: 3, MaxProviderInvocations: 24},
		Stages: []domain.PlanStage{
			{ID: "implementation", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				InvocationMode: domain.InvocationModeMutating},
			{ID: "security-review", Kind: domain.StageAgent, Role: domain.RoleSecurityReviewer,
				DependsOn: []string{"implementation"}, InvocationMode: domain.InvocationModeMutating},
			{ID: "assurance", Kind: domain.StageAssuranceGate, DependsOn: []string{"security-review"},
				RequiredClaims: []string{"claim-tests"}},
		},
		Provenance: domain.PlanProvenance{
			CompilerVersion: "plan-compiler-v0.1",
			ProjectModel:    domain.ObjectRevision{ID: "project-acme-payments", Revision: "1"},
			Policy:          domain.ObjectRevision{ID: "policy-baseline", Revision: "1"},
			Contract:        domain.ObjectRevision{ID: "contract-sso", Revision: "3"},
		},
	}
	digest, err := plan.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	return plan
}

func planEvent(t *testing.T, planID, id, eventType string, payload any) EngineeringEvent {
	t.Helper()
	event := EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: id, PlanID: planID,
		Type: eventType, OccurredAt: time.Unix(100, 0).UTC(),
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		event.Payload = encoded
	}
	return event
}

func proposedPayload(plan domain.EngineeringPlan) PlanProposedPayload {
	return PlanProposedPayload{
		Revision: plan.Revision, Digest: plan.Digest, ObjectiveDigest: objectiveDigest(plan.Objective),
		StageCount: len(plan.Stages), AgentStageCount: 2, Origin: domain.ProposalOriginInitial,
		Budget: PlanBudgetPayload{
			MaxChildRuns:           plan.BudgetEnvelope.MaxChildRuns,
			MaxConcurrency:         plan.BudgetEnvelope.MaxConcurrency,
			MaxProviderInvocations: plan.BudgetEnvelope.MaxProviderInvocations,
		},
	}
}

func appendPlan(t *testing.T, store *SQLiteOperationStore, event EngineeringEvent) EngineeringEvent {
	t.Helper()
	written, err := store.AppendPlanEvent(event)
	if err != nil {
		t.Fatalf("append %s: %v", event.Type, err)
	}
	return written
}

// A plan revision is IMMUTABLE. An assignment, an approval and an event digest
// all point at exact content, so rewriting a stored revision would change what
// an operator approved after they approved it.
// objectiveDigest is how the journal names an objective: an identity rather
// than the text, because the objective is derived from untrusted issue content.
func objectiveDigest(objective string) string {
	digest, err := Digest(objective)
	if err != nil {
		panic(err)
	}
	return digest
}

func TestPlanRevisionsAreImmutable(t *testing.T) {
	_, store := openPlanStore(t)
	plan := planFixture(t, "plan-sso", 1)
	claimed, err := store.ClaimPlan(plan)
	if err != nil || !claimed {
		t.Fatalf("claim: %v claimed=%v", err, claimed)
	}
	// The same document again is a no-op, so a retried write is safe.
	if _, err := store.PutPlanRevision(plan); err != nil {
		t.Fatalf("re-storing an identical revision was refused: %v", err)
	}

	rewritten := plan
	rewritten.Objective = "Quietly do something else."
	digest, err := rewritten.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	rewritten.Digest = digest
	_, err = store.PutPlanRevision(rewritten)
	var conflict *PlanRevisionConflictError
	if err == nil || !asError(err, &conflict) {
		t.Fatalf("expected an immutability refusal, got %v", err)
	}

	stored, found, err := store.PlanRevision("plan-sso", 1)
	if err != nil || !found {
		t.Fatalf("revision 1: %v found=%v", err, found)
	}
	if stored.Objective != plan.Objective {
		t.Fatalf("stored revision was rewritten: %q", stored.Objective)
	}

	// A revision whose stated digest is not its own content is refused before
	// it can be stored under a name it does not have.
	lying := planFixture(t, "plan-sso", 2)
	lying.Digest = strings.Repeat("0", 64)
	if _, err := store.PutPlanRevision(lying); err == nil || !strings.Contains(err.Error(), "digests to") {
		t.Fatalf("expected a digest refusal, got %v", err)
	}
}

// Restart is the whole point of durable plan state: the supervisor is a
// long-lived process, and everything it knows about a plan has to come back
// from the store rather than from anything it held in memory.
func TestRestartReconstructsPlanStateFromTheStoreAlone(t *testing.T) {
	dir, store := openPlanStore(t)
	plan := planFixture(t, "plan-sso", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-1", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-2", EventPlanValidated, PlanValidatedPayload{
		Revision: 1, Digest: plan.Digest, Status: string(domain.ProposalValid),
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-3", EventPlanApproved, PlanDecisionPayload{
		Revision: 1, Digest: plan.Digest, Operator: "operator-1", Note: "reviewed the decomposition",
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-4", EventPlanStageAssigned, PlanStageAssignedPayload{
		StageID: "implementation", AssignmentID: "assignment-1", Role: string(domain.RoleImplementer),
		ProfileID: "zenchron-builder", ProfileVersion: 2, ProfileDigest: strings.Repeat("a", 64),
		AgentID: "codex", ProviderKind: AgentKindCodexCLI, VendorFamily: "openai",
		TrustMode: string(TrustOperatorTrusted), InvocationMode: string(domain.InvocationModeMutating),
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-5", EventPlanRunStarted, PlanRunStartedPayload{
		StageID: "implementation", RunID: "run-implementation",
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-6", EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		StageID: "implementation", RunID: "run-implementation", ChildRuns: 1, ProviderInvocations: 2, WallSeconds: 900,
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "plan-sso-7", EventPlanGateSatisfied, PlanGateSatisfiedPayload{
		StageID: "assurance", Kind: string(domain.StageAssuranceGate), Claims: []string{"claim-tests"},
		Evidence: Ref{ID: "bundle-1", Revision: "1"},
	}))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopenStore(t, dir)
	snapshot, err := reopened.ReplayPlan("plan-sso")
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if snapshot.Revision != 1 || snapshot.Digest != plan.Digest {
		t.Fatalf("replayed revision %d/%s", snapshot.Revision, snapshot.Digest)
	}
	approved, ok := snapshot.ApprovedRevision()
	if !ok || approved != 1 || snapshot.Approval.Operator != "operator-1" {
		t.Fatalf("approval = %#v", snapshot.Approval)
	}
	if snapshot.Validation.Status != domain.ProposalValid {
		t.Fatalf("validation = %#v", snapshot.Validation)
	}
	implementation := snapshot.Stages["implementation"]
	if implementation.State != PlanStageRunning || implementation.RunID != "run-implementation" {
		t.Fatalf("implementation stage = %#v", implementation)
	}
	// The exact configuration that performed the work is frozen in the replay.
	if implementation.ProfileID != "zenchron-builder" || implementation.ProfileVersion != 2 ||
		implementation.ProfileDigest != strings.Repeat("a", 64) || implementation.AgentID != "codex" ||
		implementation.VendorFamily != "openai" {
		t.Fatalf("assignment freeze lost: %#v", implementation)
	}
	gate := snapshot.Stages["assurance"]
	if gate.State != PlanStageSatisfied || gate.Gate == nil || gate.Gate.Evidence.ID != "bundle-1" {
		t.Fatalf("gate = %#v", gate)
	}
	// A gate never owns a run. That is answerable from replayed state, not only
	// from the code that wrote it.
	if gate.RunID != "" {
		t.Fatalf("a satisfied gate owns EngineeringRun %q", gate.RunID)
	}
	if got := snapshot.ChildRuns(); len(got) != 1 || got[0] != "run-implementation" {
		t.Fatalf("child runs = %v", got)
	}
	if snapshot.Consumed.ChildRuns != 1 || snapshot.Consumed.ProviderInvocations != 2 || snapshot.Consumed.WallSeconds != 900 {
		t.Fatalf("consumed = %#v", snapshot.Consumed)
	}
	if plan, found, err := reopened.Plan("plan-sso"); err != nil || !found || plan.Objective != "Add enterprise SSO to the payments service." {
		t.Fatalf("plan row after restart: %v found=%v %#v", err, found, plan)
	}
}

// The frozen budget law: a revision, a reassignment or a restart may not reset
// consumed budget. Consumption is a SUM over immutable events, so this is a
// property of the representation rather than a rule somebody has to remember.
func TestConsumedBudgetSurvivesRevisionAndRestart(t *testing.T) {
	dir, store := openPlanStore(t)
	plan := planFixture(t, "plan-budget", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "b-1", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "b-2", EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		ChildRuns: 2, ProviderInvocations: 5, WallSeconds: 1800,
	}))

	next := planFixture(t, "plan-budget", 2)
	if _, err := store.PutPlanRevision(next); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "b-3", EventPlanRevisionSuperseded, PlanRevisionSupersededPayload{
		FromRevision: 1, ToRevision: 2, ProposalID: "proposal-2", InvalidatedStages: []string{"security-review"},
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "b-4", EventPlanProposed, proposedPayload(next)))

	before, err := store.ReplayPlan("plan-budget")
	if err != nil {
		t.Fatal(err)
	}
	if before.Consumed.ChildRuns != 2 || before.Consumed.ProviderInvocations != 5 || before.Consumed.WallSeconds != 1800 {
		t.Fatalf("a new revision changed consumption: %#v", before.Consumed)
	}
	// A new revision is NOT approved by inheritance.
	if before.Approval.Status != domain.ApprovalPending || before.Approval.Revision != 2 {
		t.Fatalf("a new revision carried an approval forward: %#v", before.Approval)
	}
	// Only the stage the proposal named was invalidated.
	if before.Stages["security-review"].State != PlanStageInvalidated {
		t.Fatalf("named stage was not invalidated: %#v", before.Stages["security-review"])
	}
	if state := before.Stages["implementation"].State; state == PlanStageInvalidated {
		t.Fatal("an unaffected stage was invalidated by the supersession")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	after, err := reopenStore(t, dir).ReplayPlan("plan-budget")
	if err != nil {
		t.Fatal(err)
	}
	if after.Consumed != before.Consumed {
		t.Fatalf("restart changed consumption: %#v -> %#v", before.Consumed, after.Consumed)
	}
}

// Unknown cost is not zero. A subscription CLI reports no cost, and rendering
// that as zero would fabricate a currency figure an operator might act on.
func TestUnknownCostStaysUnknownAndKnownCostAccumulates(t *testing.T) {
	dir, store := openPlanStore(t)
	plan := planFixture(t, "plan-cost", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "c-1", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "c-2", EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		ProviderInvocations: 1,
	}))
	unknown, err := store.ReplayPlan("plan-cost")
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Consumed.CostKnown || unknown.Consumed.CostMicros != nil {
		t.Fatalf("an unreported cost was rendered as a figure: %#v", unknown.Consumed)
	}

	reported := int64(2500)
	appendPlan(t, store, planEvent(t, plan.ID, "c-3", EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		ProviderInvocations: 1, CostMicros: &reported, CostKnown: true,
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "c-4", EventPlanBudgetConsumed, PlanBudgetConsumedPayload{
		ProviderInvocations: 1, CostMicros: &reported, CostKnown: true,
	}))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	known, err := reopenStore(t, dir).ReplayPlan("plan-cost")
	if err != nil {
		t.Fatal(err)
	}
	if !known.Consumed.CostKnown || known.Consumed.CostMicros == nil || *known.Consumed.CostMicros != 5000 {
		t.Fatalf("reported cost did not accumulate: %#v", known.Consumed)
	}
	if known.Consumed.ProviderInvocations != 3 {
		t.Fatalf("invocations = %d, want 3", known.Consumed.ProviderInvocations)
	}
}

// A payload that states a cost without stating that cost is known - or the
// reverse - is refused, because the two are opposite facts and a reader would
// have no way to tell which was meant.
func TestCostPayloadsMustStateWhetherCostIsKnown(t *testing.T) {
	_, store := openPlanStore(t)
	plan := planFixture(t, "plan-cost-schema", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "s-1", EventPlanProposed, proposedPayload(plan)))

	figure := int64(10)
	for _, tc := range []struct {
		name    string
		payload PlanBudgetConsumedPayload
		detail  string
	}{
		{"figure without known", PlanBudgetConsumedPayload{ProviderInvocations: 1, CostMicros: &figure}, "without stating that cost is known"},
		{"known without figure", PlanBudgetConsumedPayload{ProviderInvocations: 1, CostKnown: true}, "unknown and zero are different facts"},
		{"no delta at all", PlanBudgetConsumedPayload{}, "accounts for nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := store.AppendPlanEvent(planEvent(t, plan.ID, "s-"+tc.name, EventPlanBudgetConsumed, tc.payload))
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("expected %q, got %v", tc.detail, err)
			}
		})
	}
}

// Two streams, one table, one hash chain implementation - and neither stream's
// sequence or chain is affected by the other.
func TestRunAndPlanStreamsCoexistWithoutInterference(t *testing.T) {
	dir, store := openPlanStore(t)
	run := EngineeringRun{
		SchemaVersion: SchemaVersion, ID: "run-1", Repository: "acme/payments",
		Goal: "github-issue:acme/payments#1", Phase: Contract, Disposition: Active,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	plan := planFixture(t, "plan-coexist", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}

	runEvents := []EngineeringEvent{}
	for i, eventType := range []string{EventRunCreated, EventCandidateChanged} {
		written, err := store.AppendEvent(EngineeringEvent{
			SchemaVersion: SchemaVersion, ID: "r-" + string(rune('a'+i)), RunID: "run-1",
			Type: eventType, OccurredAt: time.Unix(int64(10+i), 0).UTC(),
		})
		if err != nil {
			t.Fatalf("append run event: %v", err)
		}
		runEvents = append(runEvents, written)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "p-a", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "p-b", EventPlanValidated, PlanValidatedPayload{
		Revision: 1, Digest: plan.Digest, Status: string(domain.ProposalValid),
	}))

	// Each stream numbers itself from 1, independently.
	if runEvents[0].Sequence != 1 || runEvents[1].Sequence != 2 {
		t.Fatalf("run sequences = %d, %d", runEvents[0].Sequence, runEvents[1].Sequence)
	}
	planEvents, err := store.PlanEvents(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(planEvents) != 2 || planEvents[0].Sequence != 1 || planEvents[1].Sequence != 2 {
		t.Fatalf("plan sequences = %#v", planEvents)
	}
	// Neither stream's read returns the other's events.
	replayedRun, err := store.Events("run-1")
	if err != nil || len(replayedRun) != 2 {
		t.Fatalf("run events = %d (%v)", len(replayedRun), err)
	}
	for _, e := range replayedRun {
		if e.PlanID != "" {
			t.Fatalf("a plan event leaked into the run stream: %#v", e)
		}
	}
	for _, e := range planEvents {
		if e.RunID != "" {
			t.Fatalf("a run event leaked into the plan stream: %#v", e)
		}
	}

	// An event may not be appended to the wrong stream.
	if _, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "wrong-1", RunID: "run-1", Type: EventPlanApproved,
	}); err == nil || !strings.Contains(err.Error(), "belongs to the plan stream") {
		t.Fatalf("a plan event was accepted into a run: %v", err)
	}
	if _, err := store.AppendPlanEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "wrong-2", PlanID: plan.ID, Type: EventRunCreated,
	}); err == nil || !strings.Contains(err.Error(), "does not belong to the plan stream") {
		t.Fatalf("a run event was accepted into a plan: %v", err)
	}
	if _, err := store.AppendPlanEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "wrong-3", PlanID: plan.ID, RunID: "run-1", Type: EventPlanApproved,
	}); err == nil || !strings.Contains(err.Error(), "exactly one stream") {
		t.Fatalf("an event in two streams was accepted: %v", err)
	}

	// Restart, and both streams still replay to the same digests.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := reopenStore(t, dir)
	after, err := reopened.Events("run-1")
	if err != nil {
		t.Fatal(err)
	}
	for i := range after {
		if after[i].EventHash != runEvents[i].EventHash || after[i].StateAfter != runEvents[i].StateAfter {
			t.Fatalf("run event %d changed across restart: %#v vs %#v", i, after[i], runEvents[i])
		}
	}
}

// The stream-scoped journal must not have changed what a RUN event is. A run
// event's canonical document is what its hash chain, its state digests and its
// run identity are computed over, so a member that appeared - even an empty one
// - would re-identify every event ever written.
func TestRunEventDocumentsAreUnchangedByThePlanStream(t *testing.T) {
	dir, store := openPlanStore(t)
	run := EngineeringRun{
		SchemaVersion: SchemaVersion, ID: "run-doc", Repository: "acme/payments",
		Goal: "github-issue:acme/payments#7", Phase: Contract, Disposition: Active,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	written, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "doc-1", RunID: "run-doc",
		Type: EventRunCreated, OccurredAt: time.Unix(5, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	var document string
	db := rawJournalDB(t, dir)
	if err := db.QueryRow(`SELECT document FROM events WHERE id = ?`, "doc-1").Scan(&document); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(document, "plan_id") {
		t.Fatalf("a run event's canonical document carries a plan member: %s", document)
	}
	// The stream columns exist and default to the run stream, so the eleven
	// column insert every existing caller uses still lands correctly.
	var kind, planID string
	if err := db.QueryRow(`SELECT stream_kind, plan_id FROM events WHERE id = ?`, "doc-1").Scan(&kind, &planID); err != nil {
		t.Fatal(err)
	}
	if kind != streamRun || planID != "" {
		t.Fatalf("run event stored as stream %q plan %q", kind, planID)
	}
	// And the digest is the digest of exactly that document.
	digest, err := EventDigest(written)
	if err != nil {
		t.Fatal(err)
	}
	if digest != written.EventHash {
		t.Fatalf("event hash %s does not match its document digest %s", written.EventHash, digest)
	}
	if _, err := filepath.Abs(dir); err != nil {
		t.Fatal(err)
	}
}

// A plan's hash chain is verified on replay exactly as a run's is.
func TestPlanJournalRefusesATamperedChain(t *testing.T) {
	dir, store := openPlanStore(t)
	plan := planFixture(t, "plan-tamper", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "t-1", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "t-2", EventPlanValidated, PlanValidatedPayload{
		Revision: 1, Digest: plan.Digest, Status: string(domain.ProposalValid),
	}))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db := rawJournalDB(t, dir)
	if _, err := db.Exec(`UPDATE events SET previous_event_hash = ? WHERE id = 't-2'`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	if _, err := reopenStore(t, dir).ReplayPlan("plan-tamper"); err == nil {
		t.Fatal("a tampered plan chain replayed as valid")
	}
}

// The migration that made the journal stream-scoped had to carry every
// historical row across UNCHANGED. This builds a database at the PRE-migration
// schema, writes a run journal into it by hand, then opens it with this binary
// and asserts that the document, the hash chain and the replayed state digest
// are all byte-identical afterwards.
//
// Without this the rebuild is only asserted against journals this binary wrote,
// which is the one case that cannot detect a migration that silently rewrote
// history.
func TestMigrationPreservesAPreExistingRunJournal(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db")+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatal(err)
	}
	// The schema as it stood before plans existed: the first three migrations,
	// and PRAGMA user_version left at three so this binary migrates it forward.
	preMigration := sqliteMigrations[:3]
	for _, migration := range preMigration {
		if _, err := db.Exec(migration); err != nil {
			t.Fatalf("apply pre-migration schema: %v", err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version = 3`); err != nil {
		t.Fatal(err)
	}

	run := EngineeringRun{
		SchemaVersion: SchemaVersion, ID: "run-legacy", Repository: "acme/payments",
		Goal: "github-issue:acme/payments#3", Phase: Contract, Disposition: Active,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}
	runDocument, err := CanonicalJSON(run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO runs (`+sqliteRunColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		run.ID, run.Repository, run.Base.ID, run.Base.Revision, run.Contract.ID, run.Contract.Revision,
		run.Candidate.Branch, run.Candidate.Revision, run.Candidate.Tree, run.ControllerSHA256,
		run.CreatedAt.UnixNano(), string(runDocument)); err != nil {
		t.Fatal(err)
	}

	// One event, chained exactly as the journal would have chained it.
	event := EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: "legacy-1", RunID: run.ID,
		Type: EventRunCreated, OccurredAt: time.Unix(5, 0).UTC(), Sequence: 1,
	}
	before, err := Reduce(run, nil)
	if err != nil {
		t.Fatal(err)
	}
	event.StateBefore = before.StateSHA256
	after, err := Reduce(run, []EngineeringEvent{event})
	if err != nil {
		t.Fatal(err)
	}
	event.StateAfter = after.StateSHA256
	if event.EventHash, err = EventDigest(event); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalJSON(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.ID, event.RunID, event.Sequence, event.Type, event.OperationID, event.PreviousEventID,
		event.PreviousEventHash, event.StateBefore, event.StateAfter, event.EventHash, string(canonical)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Opening migrates. Nothing about the historical event may have moved.
	migrated := reopenStore(t, dir)
	replayed, err := migrated.Events(run.ID)
	if err != nil {
		t.Fatalf("replay after migration: %v", err)
	}
	if len(replayed) != 1 {
		t.Fatalf("migrated journal holds %d events", len(replayed))
	}
	if replayed[0].EventHash != event.EventHash || replayed[0].StateAfter != event.StateAfter {
		t.Fatalf("migration changed a historical event: %#v vs %#v", replayed[0], event)
	}
	var storedDocument string
	if err := rawJournalDB(t, dir).QueryRow(`SELECT document FROM events WHERE id = ?`, event.ID).Scan(&storedDocument); err != nil {
		t.Fatal(err)
	}
	if storedDocument != string(canonical) {
		t.Fatalf("migration rewrote a canonical document:\n%s\n%s", storedDocument, canonical)
	}
	snapshot, err := migrated.Replay(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.StateSHA256 != event.StateAfter {
		t.Fatalf("replayed state digest %s, want %s", snapshot.StateSHA256, event.StateAfter)
	}
	// And the migrated database is usable for plan work immediately.
	plan := planFixture(t, "plan-after-migration", 1)
	if claimed, err := migrated.ClaimPlan(plan); err != nil || !claimed {
		t.Fatalf("claim after migration: %v claimed=%v", err, claimed)
	}
	appendPlan(t, migrated, planEvent(t, plan.ID, "am-1", EventPlanProposed, proposedPayload(plan)))
}

// asError is errors.As without importing errors into a file that otherwise
// needs nothing from it.
func asError[T error](err error, target *T) bool {
	for err != nil {
		if typed, ok := err.(T); ok {
			*target = typed
			return true
		}
		unwrapped, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapped.Unwrap()
	}
	return false
}

// Consumption is idempotent per keyed fact. The reconciler re-derives the same
// work every tick, so a crash between two appends replays a delta that was
// already recorded - and consumption that can only go up must not go up twice
// for one thing.
func TestConsumptionCountsOneFactOnce(t *testing.T) {
	_, store := openPlanStore(t)
	plan := planFixture(t, "plan-consumption", 1)
	if _, err := store.ClaimPlan(plan); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := appendPlanBudget(t, store, plan.ID, PlanBudgetConsumedPayload{
			Key: "child_run:run-1", StageID: "implementation", RunID: "run-1", ChildRuns: 1,
		}, i); err != nil {
			t.Fatal(err)
		}
	}
	// A DIFFERENT run is a different fact.
	if err := appendPlanBudget(t, store, plan.ID, PlanBudgetConsumedPayload{
		Key: "child_run:run-2", StageID: "review", RunID: "run-2", ChildRuns: 1,
	}, 3); err != nil {
		t.Fatal(err)
	}
	// An event written before keys existed carries none and is counted, exactly
	// as it always was.
	if err := appendPlanBudget(t, store, plan.ID, PlanBudgetConsumedPayload{
		StageID: "decomposition", ProviderInvocations: 1,
	}, 4); err != nil {
		t.Fatal(err)
	}

	snapshot, err := store.ReplayPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Consumed.ChildRuns != 2 {
		t.Fatalf("consumed %d child runs, want 2: a replayed delta was counted twice", snapshot.Consumed.ChildRuns)
	}
	if snapshot.Consumed.ProviderInvocations != 1 {
		t.Fatalf("consumed %d provider invocations, want 1", snapshot.Consumed.ProviderInvocations)
	}
}

func appendPlanBudget(t *testing.T, store *SQLiteOperationStore, planID string, payload PlanBudgetConsumedPayload, seq int) error {
	t.Helper()
	body, err := marshalPayloadJSON(payload)
	if err != nil {
		return err
	}
	_, err = store.AppendPlanEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: fmt.Sprintf("budget-%d", seq), PlanID: planID,
		Type: EventPlanBudgetConsumed, OccurredAt: time.Unix(int64(100+seq), 0).UTC(), Payload: body,
	})
	return err
}
