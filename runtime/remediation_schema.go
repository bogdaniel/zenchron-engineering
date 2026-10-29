package runtime

// remediationSchemaMigrations is #581's own migration (ADR-0008 Batch 1:
// additive foundation only), appended to sqliteMigrations (sqlite_store.go)
// rather than inlined there, to keep that file under the repository's
// file-size cap - the same reason product_schema.go exists. It is still one
// ordered migration list and one schema version counter: this file
// contributes exactly two more entries to it.
//
// Both tables are brand new and start empty for every graph that exists
// today. Nothing here alters work_graph_activations (#472) or any other
// existing table - this migration is purely additive, and no existing query
// is pointed at either new table by this change (ADR-0008 §3). Compatibility
// for every untouched graph is therefore structural, not asserted: an empty
// table cannot change the answer to a query that does not read it.
var remediationSchemaMigrations = []string{`
-- remediation_authorizations is the one durable record (ADR-0008 §5) that a
-- Finding never is: it is written only by an explicit, bounded policy
-- clause compiled into the unit's EngineeringContract, or by resolving a
-- #508 decision_request(epoch_authorization) - never by a Finding's mere
-- presence. The CHECK constraint is what makes "the plan-preauthorized path
-- can never raise the ceiling" a schema fact rather than a convention
-- (ADR-0008 §11.1): only a decision-authorized row may ever carry
-- raises_ceiling_to, and only a decision-authorized row may cite a
-- decision_resolutions row.
CREATE TABLE remediation_authorizations (
	id                     TEXT PRIMARY KEY,
	graph_id               TEXT NOT NULL,
	unit_id                TEXT NOT NULL,
	proposed_epoch         INTEGER NOT NULL,
	finding_id             TEXT NOT NULL,
	authorized_by          TEXT NOT NULL,
	policy_clause_ref      TEXT,
	decision_resolution_id TEXT REFERENCES decision_resolutions(id),
	raises_ceiling_to      INTEGER,
	authorized_unix_nano   INTEGER NOT NULL,
	CHECK (
		(authorized_by = 'policy' AND policy_clause_ref IS NOT NULL AND raises_ceiling_to IS NULL AND decision_resolution_id IS NULL)
		OR
		(authorized_by = 'decision' AND policy_clause_ref IS NULL AND decision_resolution_id IS NOT NULL)
	)
);
CREATE INDEX remediation_authorizations_by_unit ON remediation_authorizations(graph_id, unit_id, proposed_epoch);
`, `
-- work_graph_remediation_epochs holds every WorkUnit remediation attempt
-- beyond the first (ADR-0008 §3). work_graph_activations (#472) is never
-- altered or re-keyed: it stays epoch 1's unconditional system of record,
-- forever. Every row here is written once, in the same transaction as the
-- run and operations it names (ADR-0008 §8, #581 Batch 2) - no field is
-- ever updated after insertion. "How this epoch is going" is read off
-- run_id's own existing lifecycle (orchestration.DeriveEpochOutcome), never
-- off a second, separately-mutated status here - there is deliberately no
-- status column.
CREATE TABLE work_graph_remediation_epochs (
	graph_id            TEXT NOT NULL,
	unit_id             TEXT NOT NULL,
	epoch               INTEGER NOT NULL,
	batch_id            TEXT NOT NULL REFERENCES orchestration_batches(id),
	run_id              TEXT NOT NULL,
	inputs_digest       TEXT NOT NULL,
	authorization_id    TEXT NOT NULL REFERENCES remediation_authorizations(id),
	activated_unix_nano INTEGER NOT NULL,
	PRIMARY KEY (graph_id, unit_id, epoch)
);
`}
