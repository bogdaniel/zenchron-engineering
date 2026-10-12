package runtime

// Batch 1 (#581, ADR-0008 "additive foundation"): schema + pure read paths
// only. These tests prove the new tables are structurally correct and that
// a graph/unit with zero remediation activity - every graph that exists on
// main today - sees exactly nothing from them. No admission/write path
// exists yet; rows are seeded directly for round-trip verification, exactly
// as the eventual Batch 2 writer will shape them.

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// seedBatch creates the one durable batch a remediation epoch's batch_id FK
// requires, mirroring workgraph_store_test.go's own setup.
func seedBatch(t *testing.T, store *SQLiteOperationStore, issue int, runID string, now time.Time) string {
	t.Helper()
	id, err := orchestration.BatchID("acme/repo", "claude", []int{issue})
	if err != nil {
		t.Fatal(err)
	}
	batch := orchestration.Batch{SchemaVersion: orchestration.BatchSchemaVersion,
		Repository: "acme/repo", AgentID: "claude", CreatedAt: now,
		Items: []orchestration.BatchItem{{Issue: issue, RunID: runID}}}
	batch.ID = id
	if _, _, err := store.CreateOrchestrationBatch(batch); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestUntouchedGraphHasNoRemediationActivity(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	epochs, err := store.WorkUnitRemediationEpochs("graph-1", "a")
	if err != nil || len(epochs) != 0 {
		t.Fatalf("epochs = %+v (%v), want empty/no error for a graph with no remediation activity", epochs, err)
	}
	authorizations, err := store.RemediationAuthorizationsFor("graph-1", "a")
	if err != nil || len(authorizations) != 0 {
		t.Fatalf("authorizations = %+v (%v), want empty/no error", authorizations, err)
	}
	if got := latestAttemptEpoch(epochs); got != 1 {
		t.Fatalf("latestAttemptEpoch with no remediation rows = %d, want 1 (epoch 1 is always implicit)", got)
	}
}

func TestExistingIntegrationFixtureIsUnaffectedByTheNewTables(t *testing.T) {
	// The exact fixture #475's own proven tests use (orchestration_fleet_test.go),
	// driven only far enough to prove the new migration coexists cleanly with
	// a real WorkGraph adoption + activation + handoff admission sequence -
	// not a re-run of #475's own suite (already proven separately), just
	// evidence the new tables are inert alongside it.
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1700000000, 0).UTC()
	graph, err := orchestration.WorkGraphProposal{Name: "m2-o4", Revision: 1,
		Units: []orchestration.WorkUnit{{ID: "a", Purpose: "land a", Role: "implementer", Issue: 101}},
	}.Compose("acme/repo", "claude", "operator@example", now)
	if err != nil {
		t.Fatal(err)
	}
	adopted, created, err := store.AdoptWorkGraphRevision(graph)
	if err != nil || !created {
		t.Fatalf("adopt: created=%t err=%v", created, err)
	}
	batchID := seedBatch(t, store, 101, "run-a", now)
	if _, err := store.ActivateWorkUnit(WorkUnitActivation{
		GraphID: adopted.ID, UnitID: "a", BatchID: batchID, RunID: "run-a",
		InputsDigest: "digest-a", ActivatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// The new tables see nothing from this entirely ordinary, non-remediated
	// activation.
	epochs, err := store.WorkUnitRemediationEpochs(adopted.ID, "a")
	if err != nil || len(epochs) != 0 {
		t.Fatalf("an ordinary activation produced remediation epoch rows: %+v (%v)", epochs, err)
	}
}

func TestRemediationAuthorizationRoundTripsBothProvenancePaths(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1700000000, 0).UTC()

	// A plan-preauthorized grant: no decision_resolution_id, no raises_ceiling_to.
	if _, err := store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, policy_clause_ref, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth-policy-1", "graph-1", "a", 2, "finding-1", string(RemediationAuthorizedByPolicy), "clause-7", now.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	authorizations, err := store.RemediationAuthorizationsFor("graph-1", "a")
	if err != nil || len(authorizations) != 1 {
		t.Fatalf("authorizations = %+v (%v)", authorizations, err)
	}
	got := authorizations[0]
	if got.AuthorizedBy != RemediationAuthorizedByPolicy || got.PolicyClauseRef != "clause-7" ||
		got.DecisionResolutionID != "" || got.RaisesCeilingTo != nil || got.ProposedEpoch != 2 || got.FindingID != "finding-1" {
		t.Fatalf("round-tripped policy authorization = %+v", got)
	}

	// A decision-authorized grant that raises the ceiling - needs a real
	// decision_resolutions row to satisfy the FK.
	if _, err := store.db.Exec(`INSERT INTO decision_resolutions (id, request_id, scope, resolved_unix_nano, document)
		VALUES (?, ?, ?, ?, ?)`, "resolution-1", "request-1", "scope-1", now.UnixNano(), "{}",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, decision_resolution_id, raises_ceiling_to, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth-decision-1", "graph-1", "b", 2, "finding-2", string(RemediationAuthorizedByDecision), "resolution-1", 2, now.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	authorizations, err = store.RemediationAuthorizationsFor("graph-1", "b")
	if err != nil || len(authorizations) != 1 {
		t.Fatalf("authorizations = %+v (%v)", authorizations, err)
	}
	got = authorizations[0]
	if got.AuthorizedBy != RemediationAuthorizedByDecision || got.DecisionResolutionID != "resolution-1" ||
		got.RaisesCeilingTo == nil || *got.RaisesCeilingTo != 2 || got.PolicyClauseRef != "" {
		t.Fatalf("round-tripped decision authorization = %+v", got)
	}
}

// TestRemediationAuthorizationCheckConstraintRefusesAPolicyRowWithACeilingRaise
// is the direct regression for ADR-0008 §11.1/§5: the plan-preauthorized
// path must be structurally unable to raise the ceiling, enforced by the
// table's own CHECK constraint, not merely by convention.
func TestRemediationAuthorizationCheckConstraintRefusesAPolicyRowWithACeilingRaise(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, raises_ceiling_to, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth-bad", "graph-1", "a", 2, "finding-1", string(RemediationAuthorizedByPolicy), 3, time.Now().UnixNano(),
	)
	if err == nil {
		t.Fatal("a policy-authorized row with raises_ceiling_to set was accepted; the CHECK constraint did not refuse it")
	}
}

func TestRemediationEpochRoundTripsAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0).UTC()
	batchID := seedBatch(t, store, 202, "run-a-epoch2", now)
	if _, err := store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, policy_clause_ref, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth-1", "graph-1", "a", 2, "finding-1", string(RemediationAuthorizedByPolicy), "clause-1", now.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO work_graph_remediation_epochs
		(graph_id, unit_id, epoch, batch_id, run_id, inputs_digest, authorization_id, activated_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"graph-1", "a", 2, batchID, "run-a-epoch2", "digest-epoch2", "auth-1", now.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Genuinely close and reopen: nothing survives out of a connection pool
	// or page cache (authority_matrix_test.go's own reopen rationale).
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	epochs, err := reopened.WorkUnitRemediationEpochs("graph-1", "a")
	if err != nil || len(epochs) != 1 {
		t.Fatalf("epochs after restart = %+v (%v)", epochs, err)
	}
	got := epochs[0]
	if got.Epoch != 2 || got.BatchID != batchID || got.RunID != "run-a-epoch2" ||
		got.InputsDigest != "digest-epoch2" || got.AuthorizationID != "auth-1" {
		t.Fatalf("epoch after restart = %+v", got)
	}
	if got := latestAttemptEpoch(epochs); got != 2 {
		t.Fatalf("latestAttemptEpoch = %d, want 2", got)
	}
}

func TestRemediationEpochPrimaryKeyRefusesADuplicate(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1700000000, 0).UTC()
	batchID := seedBatch(t, store, 303, "run-a-epoch2", now)
	if _, err := store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, policy_clause_ref, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		"auth-1", "graph-1", "a", 2, "finding-1", string(RemediationAuthorizedByPolicy), "clause-1", now.UnixNano(),
	); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO work_graph_remediation_epochs
		(graph_id, unit_id, epoch, batch_id, run_id, inputs_digest, authorization_id, activated_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`
	if _, err := store.db.Exec(insert, "graph-1", "a", 2, batchID, "run-a-epoch2", "digest-epoch2", "auth-1", now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	// A second row at the SAME (graph_id, unit_id, epoch) is a primary-key
	// violation, not a silent second row - the exact property #581 Batch 2's
	// atomic admission transaction (ADR-0008 §8) depends on.
	if _, err := store.db.Exec(insert, "graph-1", "a", 2, batchID, "run-a-epoch2-second", "digest-epoch2-second", "auth-1", now.UnixNano()); err == nil {
		t.Fatal("a second epoch-2 row for the same unit was accepted; the primary key did not refuse it")
	}
}
