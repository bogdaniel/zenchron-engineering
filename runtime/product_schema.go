package runtime

// productSchemaMigrations is #476's own migration, appended to sqliteMigrations
// (sqlite_store.go) rather than inlined there, to keep that file under the
// repository's file-size cap. It is still one ordered migration list and one
// schema version counter: this file contributes exactly one more entry to it.
var productSchemaMigrations = []string{`
-- The ProductEngineeringEnvironment (#476). product_revisions and
-- product_configuration_revisions are append-only, exactly like
-- work_graph_revisions: a revision is written once and kept, and "current" is
-- the highest one adopted. Neither table has a foreign key to the other -
-- product_id alone is not a unique column on either side of that
-- relationship, and the application layer (product_store.go) already
-- refuses to adopt a configuration or knowledge entry for an unknown product.
CREATE TABLE product_revisions (
	product_id        TEXT NOT NULL,
	revision          INTEGER NOT NULL,
	name              TEXT NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL,
	PRIMARY KEY (product_id, revision)
);
CREATE TABLE product_configuration_revisions (
	product_id        TEXT NOT NULL,
	revision          INTEGER NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL,
	PRIMARY KEY (product_id, revision)
);
-- A WorkGraph belongs to at most one product. graph_id IS the primary key, so
-- the first association for a graph stands and a conflicting second product
-- claiming it is refused rather than silently taking over its units' context.
CREATE TABLE product_graph_associations (
	graph_id             TEXT PRIMARY KEY,
	product_id           TEXT NOT NULL,
	associated_unix_nano INTEGER NOT NULL
);
CREATE INDEX product_graph_associations_by_product ON product_graph_associations(product_id);
-- Knowledge entries are append-only and never updated. scope_rank mirrors the
-- #476 scope ladder (product.ScopeRank) so an "at or below this boundary"
-- read is one indexed range scan rather than a decode-every-row filter.
CREATE TABLE product_knowledge_entries (
	id                TEXT PRIMARY KEY,
	product_id        TEXT NOT NULL,
	scope             TEXT NOT NULL,
	scope_rank        INTEGER NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
CREATE INDEX product_knowledge_by_product_scope ON product_knowledge_entries(product_id, scope_rank, created_unix_nano);
-- The durable audit trail of every authorized promotion. promoted_entry_id is
-- UNIQUE and a real foreign key: unlike product_id, entry id IS the sole
-- primary key of product_knowledge_entries, so this one reference is valid.
CREATE TABLE product_knowledge_promotions (
	id                 TEXT PRIMARY KEY,
	source_entry_id    TEXT NOT NULL,
	promoted_entry_id  TEXT NOT NULL UNIQUE REFERENCES product_knowledge_entries(id),
	promoted_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);
`}
