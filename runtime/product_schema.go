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
-- #476 scope ladder (product.ScopeRank). provenance_type, owner_ref and
-- audience_ref are indexed columns, not only fields inside document, because
-- visibility is never "scope_rank <= requested" alone:
--   - OwnedKnowledge matches one exact owner_ref and excludes
--     provenance_type = 'promotion';
--   - WidelyPromotedKnowledgeAtOrBelow matches provenance_type = 'promotion'
--     AND audience_ref = '' (product/organization scope, no narrower
--     audience);
--   - AudiencedPromotedKnowledge matches provenance_type = 'promotion' AND
--     one exact scope AND one exact audience_ref (work_unit/feature scope,
--     targeted at one unit or feature).
-- Splitting the index this way is what keeps a raw, unpromoted discovery out
-- of a wider-scope read, one owner's discovery out of another owner's read,
-- and a promotion targeted at one work unit or feature out of every OTHER
-- work unit's or feature's read - all within the same product.
CREATE TABLE product_knowledge_entries (
	id                TEXT PRIMARY KEY,
	product_id        TEXT NOT NULL,
	scope             TEXT NOT NULL,
	scope_rank        INTEGER NOT NULL,
	provenance_type   TEXT NOT NULL,
	owner_ref         TEXT NOT NULL DEFAULT '',
	audience_ref      TEXT NOT NULL DEFAULT '',
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
CREATE INDEX product_knowledge_owned ON product_knowledge_entries(product_id, owner_ref, provenance_type, scope_rank);
CREATE INDEX product_knowledge_widely_promoted ON product_knowledge_entries(product_id, provenance_type, audience_ref, scope_rank);
CREATE INDEX product_knowledge_audienced ON product_knowledge_entries(product_id, provenance_type, scope, audience_ref);
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
