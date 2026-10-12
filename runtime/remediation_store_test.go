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

// TestRemediationAuthorizationCheckConstraintRefusesAPolicyRowWithNoClauseRef
// proves the other half of the same CHECK: a policy-authorized row needs its
// own clause reference - "non-null iff authorized_by = policy" (ADR-0008
// §5) is bidirectional, not merely a refusal of the decision-only fields.
func TestRemediationAuthorizationCheckConstraintRefusesAPolicyRowWithNoClauseRef(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	_, err = store.db.Exec(`INSERT INTO remediation_authorizations
		(id, graph_id, unit_id, proposed_epoch, finding_id, authorized_by, authorized_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"auth-bad", "graph-1", "a", 2, "finding-1", string(RemediationAuthorizedByPolicy), time.Now().UnixNano(),
	)
	if err == nil {
		t.Fatal("a policy-authorized row with no policy_clause_ref was accepted; the CHECK constraint did not refuse it")
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

// Batch 2: the two insert-only write paths.

func TestAdmitRemediationAuthorizationIsIdempotentAndValidates(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1700000000, 0).UTC()

	policy := RemediationAuthorization{ID: "auth-policy", GraphID: "graph-1", UnitID: "a", ProposedEpoch: 2,
		FindingID: "finding-1", AuthorizedBy: RemediationAuthorizedByPolicy, PolicyClauseRef: "clause-1", AuthorizedAt: now}
	inserted, err := store.AdmitRemediationAuthorization(policy)
	if err != nil || !inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}
	// A repeated proposal of the SAME authorization is a no-op, not a
	// conflict - the exact idempotency a reconcile pass depends on.
	inserted, err = store.AdmitRemediationAuthorization(policy)
	if err != nil || inserted {
		t.Fatalf("a repeated identical authorization re-inserted: inserted=%t err=%v", inserted, err)
	}
	got, found, err := store.RemediationAuthorizationByID("auth-policy")
	if err != nil || !found || got.PolicyClauseRef != "clause-1" || got.AuthorizedBy != RemediationAuthorizedByPolicy {
		t.Fatalf("got=%+v found=%t err=%v", got, found, err)
	}

	// A decision-authorized grant needs its resolution id and may carry a
	// ceiling raise.
	raise := 2
	decision := RemediationAuthorization{ID: "auth-decision", GraphID: "graph-1", UnitID: "b", ProposedEpoch: 2,
		FindingID: "finding-2", AuthorizedBy: RemediationAuthorizedByDecision, DecisionResolutionID: "resolution-1",
		RaisesCeilingTo: &raise, AuthorizedAt: now}
	if _, err := store.db.Exec(`INSERT INTO decision_resolutions (id, request_id, scope, resolved_unix_nano, document)
		VALUES (?, ?, ?, ?, ?)`, "resolution-1", "request-1", "scope-1", now.UnixNano(), "{}"); err != nil {
		t.Fatal(err)
	}
	inserted, err = store.AdmitRemediationAuthorization(decision)
	if err != nil || !inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}

	// Mismatched shapes are refused before they ever reach the CHECK
	// constraint - a clearer error than a bare SQL constraint violation.
	for name, bad := range map[string]RemediationAuthorization{
		"policy with a decision resolution": {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 2, FindingID: "f", AuthorizedBy: RemediationAuthorizedByPolicy, PolicyClauseRef: "c", DecisionResolutionID: "r", AuthorizedAt: now},
		"decision with a policy clause":     {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 2, FindingID: "f", AuthorizedBy: RemediationAuthorizedByDecision, DecisionResolutionID: "r", PolicyClauseRef: "c", AuthorizedAt: now},
		"policy with no clause reference":   {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 2, FindingID: "f", AuthorizedBy: RemediationAuthorizedByPolicy, AuthorizedAt: now},
		"decision with no resolution":       {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 2, FindingID: "f", AuthorizedBy: RemediationAuthorizedByDecision, AuthorizedAt: now},
		"epoch below 2":                     {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 1, FindingID: "f", AuthorizedBy: RemediationAuthorizedByPolicy, PolicyClauseRef: "c", AuthorizedAt: now},
		"unrecognized authorized_by":        {ID: "x", GraphID: "g", UnitID: "u", ProposedEpoch: 2, FindingID: "f", AuthorizedBy: "bogus", AuthorizedAt: now},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.AdmitRemediationAuthorization(bad); err == nil {
				t.Fatalf("%s: accepted", name)
			}
		})
	}
}

func TestActivateRemediationEpochIsIdempotentAndValidates(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Unix(1700000000, 0).UTC()
	batchID := seedBatch(t, store, 404, "run-a-epoch2", now)
	if _, err := store.AdmitRemediationAuthorization(RemediationAuthorization{
		ID: "auth-1", GraphID: "graph-1", UnitID: "a", ProposedEpoch: 2, FindingID: "finding-1",
		AuthorizedBy: RemediationAuthorizedByPolicy, PolicyClauseRef: "clause-1", AuthorizedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	epoch := RemediationEpoch{GraphID: "graph-1", UnitID: "a", Epoch: 2, BatchID: batchID,
		RunID: "run-a-epoch2", InputsDigest: "digest-2", AuthorizationID: "auth-1", ActivatedAt: now}
	inserted, err := store.ActivateRemediationEpoch(epoch)
	if err != nil || !inserted {
		t.Fatalf("inserted=%t err=%v", inserted, err)
	}
	// Replaying the SAME activation (e.g. after a crash before this call's
	// caller observed success) claims nothing a second time.
	inserted, err = store.ActivateRemediationEpoch(epoch)
	if err != nil || inserted {
		t.Fatalf("a replayed activation claimed the unit again: inserted=%t err=%v", inserted, err)
	}
	if _, err := store.ActivateRemediationEpoch(RemediationEpoch{GraphID: "graph-1", UnitID: "a", Epoch: 1,
		BatchID: batchID, RunID: "run-x", InputsDigest: "d", AuthorizationID: "auth-1", ActivatedAt: now}); err == nil {
		t.Fatal("an epoch below 2 was accepted")
	}
}
