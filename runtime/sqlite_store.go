package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

// sqliteMigrations are applied in order; each one advances PRAGMA user_version
// by one, so the applied count is the schema version. Never edit an applied
// migration: append a new one.
var sqliteMigrations = []string{`
CREATE TABLE run_operations (
	id                TEXT PRIMARY KEY,
	run_id            TEXT NOT NULL,
	kind              TEXT NOT NULL,
	idempotency_key   TEXT NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	revision          INTEGER NOT NULL,
	document          TEXT NOT NULL,
	UNIQUE(run_id, idempotency_key)
);
CREATE INDEX run_operations_queue ON run_operations(run_id, created_unix_nano, id);
`, `
CREATE TABLE runs (
	id                 TEXT PRIMARY KEY,
	repository         TEXT NOT NULL,
	base_id            TEXT NOT NULL,
	base_revision      TEXT NOT NULL,
	contract_id        TEXT NOT NULL,
	contract_revision  TEXT NOT NULL,
	candidate_branch   TEXT NOT NULL,
	candidate_revision TEXT NOT NULL,
	candidate_tree     TEXT NOT NULL,
	controller_sha256  TEXT NOT NULL,
	created_unix_nano  INTEGER NOT NULL,
	document           TEXT NOT NULL
);
-- The journal is append-only: rows are only ever inserted. UNIQUE(run_id,
-- sequence) is both the duplicate-sequence refusal and, as SQLite implements it
-- with a b-tree index, the ordered-replay index; no second index is needed.
CREATE TABLE events (
	id                  TEXT PRIMARY KEY,
	run_id              TEXT NOT NULL REFERENCES runs(id),
	sequence            INTEGER NOT NULL,
	type                TEXT NOT NULL,
	operation_id        TEXT NOT NULL,
	previous_event_id   TEXT NOT NULL,
	previous_event_hash TEXT NOT NULL,
	state_before        TEXT NOT NULL,
	state_after         TEXT NOT NULL,
	event_hash          TEXT NOT NULL,
	document            TEXT NOT NULL,
	UNIQUE(run_id, sequence)
);
`, `
-- Watch observation state, one row per repository. Global configuration remains
-- the sole authority for WHICH repositories are watched: nothing enumerates
-- this table, so a repository the operator dropped cannot resurrect itself from
-- a leftover row - it is simply never looked up again. The row records what a
-- poll observed and never a credential. revision is the CAS guard, so two
-- watchers updating one repository cannot lose each other's writes.
CREATE TABLE watch_state (
	repository TEXT PRIMARY KEY,
	revision   INTEGER NOT NULL,
	document   TEXT NOT NULL
);
`, `
-- EngineeringPlans, in the SAME database as runs, operations and the journal.
-- #64 forbids a second plan database, and a second one would be a second answer
-- to "what work exists".
--
-- The split between the two tables is the immutability boundary. plans holds
-- the identity and the highest revision written; plan_revisions holds each
-- revision's canonical document forever. A revision row is never updated, so a
-- historical approved plan stays reconstructable byte for byte - which is what
-- an assignment, an event digest and an operator's approval all point at.
CREATE TABLE plans (
	id                TEXT PRIMARY KEY,
	repository        TEXT NOT NULL,
	current_revision  INTEGER NOT NULL,
	created_unix_nano INTEGER NOT NULL
);
CREATE TABLE plan_revisions (
	plan_id  TEXT NOT NULL REFERENCES plans(id),
	revision INTEGER NOT NULL,
	digest   TEXT NOT NULL,
	document TEXT NOT NULL,
	PRIMARY KEY (plan_id, revision)
);
`, `
-- The journal becomes STREAM-scoped so plan lifecycle events live in the same
-- append-only table, under the same hash chain, as run events. #64 forbids a
-- second event journal; generalizing this one is the alternative to keeping two.
--
-- SQLite cannot alter a table constraint in place, so the supported rebuild is
-- used: create, copy, drop, rename. Every historical row copies across with its
-- document and every hash UNCHANGED - run events keep exactly the canonical
-- document they were hashed over, and stream_kind defaults to 'run' so an
-- eleven-column insert still lands as a run event.
--
-- The foreign key from run_id to runs(id) is not carried over, because a plan
-- event has no run and NOT NULL DEFAULT '' cannot reference one. What that
-- constraint enforced is enforced where it always mattered: AppendEvent reads
-- the run row inside the append transaction and refuses an unknown run.
ALTER TABLE events RENAME TO events_run_scoped;
CREATE TABLE events (
	id                  TEXT PRIMARY KEY,
	run_id              TEXT NOT NULL,
	sequence            INTEGER NOT NULL,
	type                TEXT NOT NULL,
	operation_id        TEXT NOT NULL,
	previous_event_id   TEXT NOT NULL,
	previous_event_hash TEXT NOT NULL,
	state_before        TEXT NOT NULL,
	state_after         TEXT NOT NULL,
	event_hash          TEXT NOT NULL,
	document            TEXT NOT NULL,
	stream_kind         TEXT NOT NULL DEFAULT 'run',
	plan_id             TEXT NOT NULL DEFAULT '',
	UNIQUE(stream_kind, run_id, plan_id, sequence)
);
INSERT INTO events (id, run_id, sequence, type, operation_id, previous_event_id,
	previous_event_hash, state_before, state_after, event_hash, document, stream_kind, plan_id)
SELECT id, run_id, sequence, type, operation_id, previous_event_id,
	previous_event_hash, state_before, state_after, event_hash, document, 'run', ''
FROM events_run_scoped;
DROP TABLE events_run_scoped;
-- The plan stream needs its own ordered index: the UNIQUE index above is
-- prefixed by run_id, which is empty for every plan event, so it cannot serve
-- an ordered read keyed on plan_id alone.
CREATE INDEX events_plan_stream ON events(plan_id, sequence);
`, `
-- The frozen AgentAssignment for one stage of one plan revision.
--
-- It is a row rather than a field on the run, because it is the artifact an
-- operator approved the plan against: which profile, which underlying worker,
-- which instruction packs by digest, which context the stage receives, and why
-- that worker was eligible. The run row carries the IDENTITIES that point here,
-- so the two cannot drift.
--
-- Immutable per (plan, revision, stage). A resolution that would now choose a
-- different worker has to be a new revision going through the approval
-- boundary, never a rewrite of what a live run is executing under.
CREATE TABLE plan_assignments (
	plan_id       TEXT NOT NULL REFERENCES plans(id),
	revision      INTEGER NOT NULL,
	stage_id      TEXT NOT NULL,
	assignment_id TEXT NOT NULL,
	document      TEXT NOT NULL,
	PRIMARY KEY (plan_id, revision, stage_id)
);
`, `
-- The SOURCE a plan answers.
--
-- A plan exists because an operator asked for work on a specific issue, and
-- every stage run it creates answers that same source with a different stage
-- objective. Recording it beside the plan is what lets a restarted supervisor
-- create the next stage's run without reconstructing intent from a plan id.
--
-- It is a column with a default rather than a new table: there is exactly one
-- source per plan, and a table would only create a way for the two to disagree.
ALTER TABLE plans ADD COLUMN source_issue INTEGER NOT NULL DEFAULT 0;
`, `
-- The compiled EngineeringWorkContract one plan revision was planned against.
--
-- The plan references it by id and revision, and this is where that reference
-- resolves. It is stored because resolution needs the OBLIGATIONS: which
-- acceptance criteria a stage is judged against, which prohibitions it carries,
-- which claims a gate references. Recompiling it at resolve time would mean a
-- plan could be resolved against a contract nobody approved it under.
--
-- Immutable per (plan, revision), like the revision document itself.
CREATE TABLE plan_contracts (
	plan_id  TEXT NOT NULL REFERENCES plans(id),
	revision INTEGER NOT NULL,
	document TEXT NOT NULL,
	PRIMARY KEY (plan_id, revision)
);
`, `
-- PlanRevisionProposals: the durable artifact a decomposition emits.
--
-- A planner-role stage proposes a replacement revision; it does not create a
-- nested plan and it does not apply anything. The proposal is stored so the
-- deterministic verdict on it, the budget effect it would have and the operator
-- decision about it all reference the same document.
CREATE TABLE plan_proposals (
	id           TEXT PRIMARY KEY,
	plan_id      TEXT NOT NULL REFERENCES plans(id),
	from_revision INTEGER NOT NULL,
	to_revision   INTEGER NOT NULL,
	document      TEXT NOT NULL
);
CREATE INDEX plan_proposals_by_plan ON plan_proposals(plan_id, to_revision);
`, `
-- An assignment is frozen per (plan, revision, stage, EXECUTION GENERATION).
--
-- A generation is what re-performs an already-approved stage whose upstream
-- input moved: the producer's candidate changed, which is an execution fact,
-- and the approved obligation - this role, this profile, this worker, this
-- trust ceiling - is unchanged. The old generation's row stays exactly as it
-- was, because it records a performance that happened.
--
-- Existing rows are generation 0, which is every assignment written before
-- this existed and every first performance since.
ALTER TABLE plan_assignments RENAME TO plan_assignments_v1;
CREATE TABLE plan_assignments (
	plan_id       TEXT NOT NULL REFERENCES plans(id),
	revision      INTEGER NOT NULL,
	stage_id      TEXT NOT NULL,
	generation    INTEGER NOT NULL DEFAULT 0,
	assignment_id TEXT NOT NULL,
	document      TEXT NOT NULL,
	PRIMARY KEY (plan_id, revision, stage_id, generation)
);
INSERT INTO plan_assignments (plan_id, revision, stage_id, generation, assignment_id, document)
	SELECT plan_id, revision, stage_id, 0, assignment_id, document FROM plan_assignments_v1;
DROP TABLE plan_assignments_v1;
`, `
-- The assignments an operator SAW when they approved one plan revision.
--
-- Approval is a decision about a document AND about who will perform it under
-- what configuration. Resolution is otherwise recomputed from the live
-- registry on every look, so an unstarted stage could be re-resolved after the
-- approval - onto an edited profile, an edited instruction pack, or a
-- different worker after the operator's default changed - and the first
-- execution would freeze something nobody had approved.
--
-- These rows are what the approval bound. They are immutable per (plan,
-- revision, stage) for the same reason the revision document is: an approval
-- that could be rewritten afterwards is not an approval. The approval event
-- carries their canonical digest, so the binding is in the hash-chained
-- journal and not only in a table beside it.
--
-- A revision approved before this existed has no rows and no digest, and
-- resolves live exactly as it did then.
CREATE TABLE plan_approved_assignments (
	plan_id  TEXT NOT NULL REFERENCES plans(id),
	revision INTEGER NOT NULL,
	stage_id TEXT NOT NULL,
	document TEXT NOT NULL,
	PRIMARY KEY (plan_id, revision, stage_id)
);
`, `
-- One row per controller transition (#234). It is durable state rather than
-- process state because the question it answers - which controller may recover
-- this handoff - has to survive the crash of both parties, and a phase held in
-- memory is a phase that disappears with the process that was mid-transition.
--
-- phase is a column of its own, beside the document that also carries it, so a
-- transition can be a conditional UPDATE: the compare-and-set is what stops two
-- processes from advancing the same handoff along two different paths.
CREATE TABLE controller_handoffs (
	id                TEXT PRIMARY KEY,
	phase             TEXT NOT NULL,
	updated_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
`, `
-- WHICH ACTIVATION GOVERNS NOW, as one durable subject.
--
-- A handoff row records that a transition activated, and that fact is
-- historical and permanent: H1 activated, and it will have activated forever.
-- It does not answer whether H1 still governs, and the runtime had no place
-- that did - so every authority-bearing path asked "is this transition
-- activated", which a superseded one still truthfully answers yes to.
--
-- This table is that missing subject. Exactly one row, written in the SAME
-- TRANSACTION as the activation it records, so there is no instant where a
-- handoff is activated and the current pointer disagrees.
CREATE TABLE controller_current_activation (
	id                TEXT PRIMARY KEY CHECK (id = 'current'),
	handoff_id        TEXT NOT NULL,
	updated_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);

-- A state directory written before this table existed carries activated
-- handoffs and no pointer. The newest activated row is the only information
-- the old schema holds about which of them governs, so it seeds the pointer
-- once, here. Wall-clock order is not good enough to BE the authority rule -
-- which is why it is not used after this - and it is the best evidence
-- available about a past this schema did not record.
INSERT INTO controller_current_activation (id, handoff_id, updated_unix_nano, document)
SELECT 'current', id, updated_unix_nano, document
  FROM controller_handoffs
 WHERE phase = 'activated'
 ORDER BY updated_unix_nano DESC, id ASC
 LIMIT 1;
`, `
-- WHAT GOVERNS NOW, WITHOUT ASSUMING A HANDOFF PUT IT THERE.
--
-- controller_current_activation can only name an activated transition, and for
-- as long as that was the only way authority could be established it was the
-- whole truth. It is not: a controller-effective configuration change cannot
-- cross an ordinary succession - the successor binding differs in exactly the
-- member evaluateConfiguration requires to be unchanged - so a state directory
-- whose configuration moved had no way to establish a new governing root. The
-- old activation stayed current, every upgrade was correctly refused, and the
-- directory sat in INVARIANT_VIOLATION with no in-protocol way out.
--
-- So present authority becomes its own subject, and it records HOW it was
-- established. An operator re-adoption is not an activation and is never
-- written as one: no handoff is fabricated, and controller_handoffs remains
-- exactly the history it was.
CREATE TABLE controller_readoptions (
	id                 TEXT PRIMARY KEY,
	recorded_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);

CREATE TABLE controller_current_authority (
	id                TEXT PRIMARY KEY CHECK (id = 'current'),
	kind              TEXT NOT NULL,
	ref               TEXT NOT NULL,
	updated_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);

-- The existing pointer is RESTATED, not reinterpreted. Whatever activation
-- governed a moment before this migration governs a moment after it, under the
-- name the new subject uses for it, carrying the binding that activation
-- already named as its successor.
INSERT INTO controller_current_authority (id, kind, ref, updated_unix_nano, document)
SELECT 'current', 'handoff_activation', handoff_id, updated_unix_nano,
       json_object('kind', 'handoff_activation', 'ref', handoff_id,
                   'binding', json(json_extract(document, '$.successor.binding')),
                   'artifact', json_extract(document, '$.successor.artifact_path'))
  FROM controller_current_activation
 WHERE id = 'current';
`, `
CREATE INDEX run_operations_active ON run_operations(run_id, created_unix_nano, id)
WHERE json_extract(document, '$.state') IN ('leased', 'running');
CREATE INDEX runs_active ON runs(created_unix_nano, id)
WHERE COALESCE(json_extract(document, '$.disposition'), '') NOT IN ('completed', 'failed', 'cancelled');
`, `
-- The controller-effective configuration a plan was first claimed under (#307).
-- Empty for plans that predate it; see planConfigurationHold.
ALTER TABLE plans ADD COLUMN config_digest TEXT NOT NULL DEFAULT '';
`, `
-- A durable, monotonically increasing CROSS-STREAM cursor (#399).
--
-- Every stream's own sequence column already orders that one run's (or
-- plan's) history, but nothing orders events ACROSS runs: a fleet-wide
-- observer asking "which runs changed since X" had no answer but polling
-- every run or replaying the whole table (#96). global_sequence is that total
-- order - one counter shared by every stream, allocated inside the same
-- write-locked append transaction that allocates the stream-local sequence
-- (appendToStream), so the two can never disagree about which event came
-- first.
--
-- It is never part of the hash-chained document: a run's chain and digests
-- are defined entirely by that run's own stream, and a fleet-wide numbering
-- scheme is not something a run's history should need to know about, or be
-- able to invalidate, to stay verifiable.
--
-- Existing rows are backfilled in ROWID order. That order already agrees with
-- every stream's sequence order - appendToStream commits each row strictly
-- after the rows before it in that stream, so ROWID order and per-stream
-- sequence order are the same order seen through two different columns - so
-- backfilling by it gives historical rows a global order consistent with the
-- history they already have, not a renumbering of it.
ALTER TABLE events ADD COLUMN global_sequence INTEGER NOT NULL DEFAULT 0;
UPDATE events SET global_sequence = ranked.ord
FROM (SELECT rowid AS rid, ROW_NUMBER() OVER (ORDER BY rowid) AS ord FROM events) AS ranked
WHERE events.rowid = ranked.rid;
CREATE UNIQUE INDEX events_global_sequence ON events(global_sequence);
-- ChangesSince filters by stream_kind and orders by global_sequence. Without
-- this index SQLite's planner prefers the existing (stream_kind, run_id,
-- plan_id, sequence) index for the equality filter and then sorts the result
-- in a temp B-tree before LIMIT applies - a scan of every matching row, which
-- is exactly the unbounded cost #399 exists to avoid. This composite index
-- answers the filter AND the ordering from one range scan, so LIMIT bounds
-- the rows actually touched.
CREATE INDEX events_stream_global_sequence ON events(stream_kind, global_sequence);
`, `
-- The operator pause (#86) is read inside every lease acquisition: the run's
-- latest run.paused/run.unpaused event. This partial index makes the common
-- never-paused case an empty probe rather than a scan of the run's journal.
CREATE INDEX events_run_pause ON events(run_id, sequence) WHERE type IN ('run.paused', 'run.unpaused');
`, `
-- Basic explicit orchestration (#470), in the SAME database as runs and the
-- journal. Both tables are insert-only: a batch is written once with every
-- item's child run already decided, and an admitted handoff is immutable. Item
-- state is never stored; it is projected from the child runs on every read.
CREATE TABLE orchestration_batches (
	id                TEXT PRIMARY KEY,
	repository        TEXT NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
CREATE TABLE orchestration_handoffs (
	id                 TEXT PRIMARY KEY,
	batch_id           TEXT NOT NULL REFERENCES orchestration_batches(id),
	run_id             TEXT NOT NULL REFERENCES runs(id),
	admitted_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);
CREATE INDEX orchestration_handoffs_by_run ON orchestration_handoffs(run_id, admitted_unix_nano, id);
-- A reported handoff the runtime decided it can never admit (its report is
-- gone or no longer the journalled document). Insert-only, keyed by the same
-- identity an admission would have had, so one invocation is settled once.
CREATE TABLE orchestration_handoff_refusals (
	id                TEXT PRIMARY KEY,
	batch_id          TEXT NOT NULL REFERENCES orchestration_batches(id),
	run_id            TEXT NOT NULL REFERENCES runs(id),
	refused_unix_nano INTEGER NOT NULL,
	reason            TEXT NOT NULL
);
CREATE INDEX orchestration_handoff_refusals_by_run ON orchestration_handoff_refusals(run_id);
`, `
-- One row per supervisor start (ADR-0003 B4): the configuration identity it
-- served under, that identity's controller-effective (C-only) content, and the
-- supervisor operating policy (S) it applied. Insert-only. A row is also the
-- evidence that a configuration identity token names exactly that C content,
-- which is what lets an S-only edit keep the identity.
CREATE TABLE supervisor_starts (
	id                   TEXT PRIMARY KEY,
	started_unix_nano    INTEGER NOT NULL,
	config_global        TEXT NOT NULL,
	config_repository    TEXT NOT NULL,
	effective_global     TEXT NOT NULL,
	effective_repository TEXT NOT NULL,
	policy_digest        TEXT NOT NULL,
	document             TEXT NOT NULL
);
CREATE INDEX supervisor_starts_projection ON supervisor_starts(config_global, config_repository, effective_global, effective_repository);
`, `
-- Nested grants share the operation database and its scheduler authority.
-- They consume verification only; their parent already holds the work slot.
CREATE TABLE verification_permits (
	id TEXT PRIMARY KEY,
	revision INTEGER NOT NULL,
	document TEXT NOT NULL
);
`, `
-- Typed inter-worker communication (#473). Insert-only, like the handoffs: a
-- message is immutable once admitted, and a correction is a NEW row naming
-- the one it supersedes. The partial unique index is the "superseded at most
-- once" rule, so history stays a chain rather than a fork. seq is admission
-- order, which is the order every projection reads.
CREATE TABLE orchestration_messages (
	seq                INTEGER PRIMARY KEY,
	id                 TEXT NOT NULL UNIQUE,
	scope              TEXT NOT NULL,
	run_id             TEXT NOT NULL REFERENCES runs(id),
	operation_id       TEXT NOT NULL,
	attempt            INTEGER NOT NULL,
	supersedes         TEXT,
	admitted_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);
CREATE INDEX orchestration_messages_by_scope ON orchestration_messages(scope, seq);
CREATE UNIQUE INDEX orchestration_messages_supersedes ON orchestration_messages(supersedes) WHERE supersedes IS NOT NULL;
-- An invocation's message report the runtime decided it can never admit,
-- keyed by the invocation, so it is settled once.
CREATE TABLE orchestration_message_refusals (
	id                TEXT PRIMARY KEY,
	scope             TEXT NOT NULL,
	run_id            TEXT NOT NULL REFERENCES runs(id),
	refused_unix_nano INTEGER NOT NULL,
	reason            TEXT NOT NULL
);
CREATE INDEX orchestration_message_refusals_by_scope ON orchestration_message_refusals(scope);
`, `
-- The WorkGraph (#472). Both tables are insert-only, and neither stores a unit
-- state: a revision document is immutable, an activation is written once, and
-- every unit state is projected from these two plus the child runs on each read.
--
-- A revision is an APPEND. Adopting revision N+1 leaves N and every activation
-- exactly as they were, which is what keeps a mutation from resetting a budget
-- a child run has already consumed.
CREATE TABLE work_graph_revisions (
	graph_id          TEXT NOT NULL,
	revision          INTEGER NOT NULL,
	repository        TEXT NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL,
	PRIMARY KEY (graph_id, revision)
);
-- One row per unit whose child run this graph has claimed, with the exact
-- upstream outputs it was activated against. The primary key is what makes
-- child association idempotent across replay and recovery: a unit is activated
-- once, so a lost reply or a crashed pass cannot produce a second child run.
--
-- batch_id references the #470 batch that owns the child run - the one whose
-- identity binds this graph, this unit and this exact input set - so the batch
-- is durable before any activation can name it. run_id carries NO foreign key:
-- the batch decides the child identity, and the existing orchestration pass
-- creates that row afterwards.
CREATE TABLE work_graph_activations (
	graph_id            TEXT NOT NULL,
	unit_id             TEXT NOT NULL,
	batch_id            TEXT NOT NULL REFERENCES orchestration_batches(id),
	run_id              TEXT NOT NULL,
	inputs_digest       TEXT NOT NULL,
	activated_unix_nano INTEGER NOT NULL,
	PRIMARY KEY (graph_id, unit_id)
);
`, `
-- Durable, authorized decision resolution (#508). Both tables are insert-only.
--
-- work_unit_holds is #472's readiness-owner seam, supplied: a hold an operator
-- placed on one WorkGraph unit before its first activation. Its primary key IS
-- the identity rule - a unit can be held at most once, ever - so a lost reply
-- finds the hold already placed instead of a conflict.
CREATE TABLE work_unit_holds (
	id                TEXT PRIMARY KEY,
	graph_id          TEXT NOT NULL,
	unit_id           TEXT NOT NULL,
	requested_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL,
	UNIQUE(graph_id, unit_id)
);
CREATE INDEX work_unit_holds_by_graph ON work_unit_holds(graph_id);
-- decision_resolutions answers exactly one live request - a worker-authored
-- #473 decision_request message, or a work_unit_holds row above - with its id
-- deterministic from the request alone (orchestration.DecisionResolutionID).
-- One request can therefore have at most one row here, ever: an identical
-- retry finds it, and a conflicting answer is refused before a second row
-- could exist to disagree with the first.
CREATE TABLE decision_resolutions (
	id                TEXT PRIMARY KEY,
	request_id        TEXT NOT NULL UNIQUE,
	scope             TEXT NOT NULL,
	resolved_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
`, `
-- decision_resumption_admissions pins, at most once per execution.invoke
-- operation, the exact decision set and subject a #508 P4b decision-resumed
-- binding is authorized to resume for (review B1/B2): admitted inside one
-- BEGIN IMMEDIATE transaction that re-validates liveness and subject fresh
-- (AdmitDecisionResumption, decision_store.go), so a supersession or subject
-- drift either lands BEFORE this row exists - refusing admission - or
-- belongs to a later epoch, never retroactively rewriting an operation
-- already admitted to run.
CREATE TABLE decision_resumption_admissions (
	operation_id       TEXT PRIMARY KEY,
	binding            TEXT NOT NULL,
	admitted_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);
`, `
-- Independent PR review (#233). review_decisions is insert-only and never
-- rewritten: a moved head or a different reviewer always identifies a
-- different decision (review.DecisionID), so there is no "latest" to update
-- in place, only more rows. repository and pr_number are indexed columns
-- (not only fields inside document) so status and staleness can be answered
-- with one query instead of a decode-every-row scan.
--
-- review_publications is a SEPARATE table on purpose: a publication attempt
-- that fails after the decision already committed must never be able to
-- corrupt or erase that decision by sharing its row.
CREATE TABLE review_decisions (
	id                TEXT PRIMARY KEY,
	repository        TEXT NOT NULL,
	pr_number         INTEGER NOT NULL,
	head_sha          TEXT NOT NULL,
	run_id            TEXT NOT NULL,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
CREATE INDEX review_decisions_by_pr ON review_decisions(repository, pr_number, created_unix_nano);
CREATE TABLE review_publications (
	decision_id TEXT PRIMARY KEY REFERENCES review_decisions(id),
	document    TEXT NOT NULL
);
`, `
-- A durable, atomic claim over one bounded review operation (#233 B4):
-- performing an independent review, or publishing one decision, each claim
-- exactly one key before doing any expensive or externally-visible work, so
-- two concurrent callers for the same exact subject/reviewer cannot both
-- invoke a provider or both submit a GitHub review. A claim is released by
-- its owner on ordinary completion (success or a clean failure); one left
-- behind by a crashed process is reclaimed once it is older than the
-- caller's staleness bound - see ClaimReview - so a dead claimant can never
-- strand the operation forever.
CREATE TABLE review_claims (
	claim_key         TEXT PRIMARY KEY,
	claimed_unix_nano INTEGER NOT NULL,
	owner             TEXT NOT NULL,
	-- token is the fencing token ClaimReview mints on every successful claim
	-- or steal. A release or renewal naming any other token is a no-op: it
	-- names a claim that has already been superseded, so acting on it would
	-- let a stale holder delete or extend a claim it no longer owns.
	token             TEXT NOT NULL
);
-- review_publication_attempts is the durable "an external POST may be in
-- flight or its outcome is unknown" marker (#233 P1). It is written BEFORE
-- PublishReview calls GitHub.SubmitReview and removed only once that
-- decision's outcome is confirmed - either a success this invocation itself
-- observed, or a later reconciliation that found GitHub's own matching
-- review. A row surviving past that point is proof no caller has yet
-- confirmed what happened to some earlier POST, which is exactly the
-- condition under which a NEW POST must be refused: GitHub's matching
-- review not being visible yet is never proof it was never accepted.
CREATE TABLE review_publication_attempts (
	decision_id       TEXT PRIMARY KEY REFERENCES review_decisions(id),
	started_unix_nano INTEGER NOT NULL
);
`, `
-- Durable, idempotent authorization to route one independent review's
-- REQUEST_CHANGES decision into producer remediation (#474). One row per
-- decision_id, ever: the primary key is what makes a second admission
-- attempt for the same already-admitted decision a read, never a second
-- remediation budget envelope or a second provider invocation.
CREATE TABLE review_remediation_admissions (
	decision_id        TEXT PRIMARY KEY REFERENCES review_decisions(id),
	run_id             TEXT NOT NULL,
	admitted_unix_nano INTEGER NOT NULL,
	document           TEXT NOT NULL
);
CREATE INDEX review_remediation_admissions_by_run ON review_remediation_admissions(run_id);
`, `
-- A durable, restart-visible claim that one independent-review trigger
-- invocation occupies a verification-capacity slot (#474 R5). An
-- independent review invokes a full reviewer provider - the same
-- capacity-class weight as an ordinary verification operation - but is
-- bound to no run's own operation row, so it has no other way to
-- participate in the SAME MaxConcurrentVerifications ceiling
-- verificationCountSQL already enforces for both durable verification
-- operations and nested VerificationPermits. Counted additively into that
-- SAME formula; never a second ceiling, never a second scheduler.
CREATE TABLE review_verification_claims (
	id       TEXT PRIMARY KEY,
	revision INTEGER NOT NULL,
	document TEXT NOT NULL
);
`}

// sqliteSchemaVersion is the newest schema this binary can operate.
var sqliteSchemaVersion = len(sqliteMigrations)

// UnsupportedSchemaError reports a database written by a newer binary. It is
// fatal by design: silently down-migrating or writing through an unknown schema
// would corrupt state another process still depends on.
type UnsupportedSchemaError struct{ Found, Supported int }

func (e UnsupportedSchemaError) Error() string {
	return fmt.Sprintf("runtime database schema version %d is newer than supported version %d", e.Found, e.Supported)
}

// SQLiteOperationStore is the durable OperationStore. Cross-process safety comes
// from SQLite itself: every write is one conditional statement, so lease
// acquisition, idempotent creation, and heartbeats are atomic without any
// process-local lock.
type SQLiteOperationStore struct{ db *sql.DB }

const sqliteOperationColumns = `id, run_id, kind, idempotency_key, created_unix_nano, revision, document`

// OpenSQLiteOperationStore opens <stateDir>/runtime.db, creating the
// owner-only (0700) directory and (0600) database when absent.
func OpenSQLiteOperationStore(stateDir string) (*SQLiteOperationStore, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("state directory is required")
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return nil, err
	}
	if err := os.Chmod(stateDir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(stateDir, "runtime.db")
	// Create the file before SQLite does so the journal never exists with
	// broader permissions than the operator's own. The 0700 directory keeps
	// the -wal/-shm sidecars private as well.
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0600)
	if err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	// _txlock=immediate takes the write lock at BEGIN, so a transaction that
	// reads state it is about to overwrite (journal sequence allocation) waits
	// on busy_timeout instead of failing an unretryable upgrade in WAL mode.
	//
	// busy_timeout is SQLite's own bounded wait for the write lock. Ten
	// drivers writing at once could wait past 5s, and a write that gives up
	// fails its pass - which since #485 costs that pass and, truthfully, an
	// attempt it had started, but should not happen for mere contention.
	// 15s stays under the 30s control-request deadline.
	dsn := sqliteFileURI(path) + "?_pragma=busy_timeout(15000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := migrateSQLite(db); err != nil {
		db.Close()
		return nil, err
	}
	return &SQLiteOperationStore{db: db}, nil
}

// sqliteFileURI is the one way a filesystem path becomes a SQLite "file:" URI.
// SQLite percent-decodes the URI, so a raw "file:"+path opens a DIFFERENT file
// whenever the path holds a '%', '?' or '#' (#331); url.URL escapes them.
// path must be absolute: a relative one is not a valid "file:" URI path.
func sqliteFileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

func (s *SQLiteOperationStore) Close() error { return s.db.Close() }

func migrateSQLite(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version > sqliteSchemaVersion {
		return UnsupportedSchemaError{Found: version, Supported: sqliteSchemaVersion}
	}
	if version == sqliteSchemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i := version; i < len(sqliteMigrations); i++ {
		if _, err := tx.Exec(sqliteMigrations[i]); err != nil {
			return fmt.Errorf("apply runtime schema migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept bound parameters; i+1 is an internal counter.
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, i+1)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *SQLiteOperationStore) Operations(runID string) ([]RunOperation, error) {
	rows, err := s.db.Query(`SELECT document FROM run_operations WHERE run_id = ? ORDER BY created_unix_nano ASC, id ASC`, runID)
	if err != nil {
		return nil, err
	}
	return scanOperations(rows)
}
func (s *SQLiteOperationStore) AllOperations() ([]RunOperation, error) {
	rows, err := s.db.Query(`SELECT document FROM run_operations ORDER BY created_unix_nano ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	return scanOperations(rows)
}

// ActiveOperations returns only leased/running rows, optionally scoped to a run.
func (s *SQLiteOperationStore) ActiveOperations(runID string) ([]RunOperation, error) {
	query := `SELECT document FROM run_operations WHERE json_extract(document, '$.state') IN ('leased', 'running')`
	var args []any
	if runID != "" {
		query += ` AND run_id = ?`
		args = append(args, runID)
	}
	rows, err := s.db.Query(query+` ORDER BY created_unix_nano ASC, id ASC`, args...)
	if err != nil {
		return nil, err
	}
	return scanOperations(rows)
}

func (s *SQLiteOperationStore) Operation(id string) (RunOperation, int64, bool, error) {
	var document string
	var revision int64
	err := s.db.QueryRow(`SELECT document, revision FROM run_operations WHERE id = ?`, id).Scan(&document, &revision)
	if err == sql.ErrNoRows {
		return RunOperation{}, 0, false, nil
	}
	if err != nil {
		return RunOperation{}, 0, false, err
	}
	op, err := decodeOperation(document)
	return op, revision, err == nil, err
}
func (s *SQLiteOperationStore) OperationByIdempotencyKey(runID, key string) (RunOperation, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM run_operations WHERE run_id = ? AND idempotency_key = ?`, runID, key).Scan(&document)
	if err == sql.ErrNoRows {
		return RunOperation{}, false, nil
	}
	if err != nil {
		return RunOperation{}, false, err
	}
	op, err := decodeOperation(document)
	return op, err == nil, err
}

// PutOperation is a single conditional statement, which SQLite executes as one
// transaction. A create that loses the primary-key or idempotency-key race and
// an update whose expected revision is stale both report false and leave the
// stored row untouched.
func (s *SQLiteOperationStore) PutOperation(op RunOperation, expected int64) (int64, bool, error) {
	if op.ID == "" {
		return 0, false, fmt.Errorf("operation id is required")
	}
	// The durable document is canonical (RFC 8785) so equal state is byte-equal.
	document, err := CanonicalJSON(op)
	if err != nil {
		return 0, false, err
	}
	if expected == 0 {
		result, err := s.db.Exec(`INSERT INTO run_operations (`+sqliteOperationColumns+`)
			SELECT ?, ?, ?, ?, ?, 1, ?
			WHERE NOT EXISTS (SELECT 1 FROM run_operations WHERE id = ? OR (run_id = ? AND idempotency_key = ?))`,
			op.ID, op.RunID, op.Kind, op.IdempotencyKey, op.CreatedAt.UnixNano(), string(document),
			op.ID, op.RunID, op.IdempotencyKey)
		if err != nil {
			return 0, false, err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected == 0 {
			return 0, false, err
		}
		return 1, true, nil
	}
	result, err := s.db.Exec(`UPDATE run_operations SET revision = revision + 1, document = ? WHERE id = ? AND revision = ?`,
		string(document), op.ID, expected)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return 0, false, err
	}
	return expected + 1, true, nil
}

// AcquireOperation is PutOperation's update path guarded by the durable global
// run ceiling. Counting the other actively driven runs and writing the lease
// are ONE statement, so two watcher processes cannot each observe a free slot
// and each take one: SQLite serializes the two updates and the loser's guard
// already sees the winner's row.
//
// The run-driving slot is the durable operation lease itself, not a second
// table. A run holds a slot exactly while one of its operations CARRIES A
// LEASE, so a run parked on CI, authority, auth, or opt-in removal holds
// nothing - there is no slot to forget to release, and a durable run that
// nobody is driving never occupies one.
//
// The lease, and not the leased/running state, is what the count is taken over.
// The state records what the last attempt was doing and belongs to the journal;
// the lease records who is doing it now. Separating them is what lets an
// abandoned attempt give its slot back while the row still says what the
// journal says about it.
//
// A TERMINAL RUN's operation is refused outright, and that condition belongs in
// this statement rather than anywhere cheaper. A stop writes the run document
// and then finishes the run's active operations, so this check is what decides
// which side of the stop a concurrent acquisition fell on; a driver that read
// the run before the stop and checked it in Go would be asking a question whose
// answer had already changed. The count is deliberately left alone: it still
// counts leases, and a terminal run holds none once its stop has finished them.
//
// Reclaiming a crashed driver's slot is the existing lease takeover, which
// CanAcquire gates on owner death AND expiry, so an expired heartbeat alone
// still steals nothing. This statement does not perform that reclamation and
// must not: owner death is a probe of the operating system, not a fact in the
// database. Scheduler.reclaimAbandoned retires an abandoned operation before
// this count is taken, so what is counted here is always durable state - and
// never a durable row plus a live opinion about it.
//
// The count is PER CAPACITY CLASS (#85): only other runs holding an active
// operation of the class being acquired are counted, against that class's
// ceiling. The class is read from the canonical document's kind - the one the
// runtime decodes - and never from the denormalized kind column, which nothing
// verifies; the observation kinds are bound parameters generated from
// OperationCapacityClass, so there is no second list. A row whose document
// carries no kind is counted as work, which is the closed direction. A
// verification operation (#490) is additionally counted against the
// verification ceiling the same way, from the same document kind.
//
// The same statement also refuses an operation while ANOTHER operation of the
// same run holds a lease. That is what makes "a run holds at most one active
// operation" a durable fact rather than a property of whoever drives it: a
// second process can never observe a run beside the work another process is
// doing on it.
func (s *SQLiteOperationStore) AcquireOperation(op RunOperation, expected int64, maxRuns, maxObservations, maxVerifications int) (int64, bool, error) {
	if op.ID == "" || expected <= 0 {
		return 0, false, fmt.Errorf("acquiring an operation needs its id and the revision it was read at")
	}
	document, err := CanonicalJSON(op)
	if err != nil {
		return 0, false, err
	}
	args := []any{string(document), op.ID, expected, op.RunID, op.ID, op.RunID}
	for _, disposition := range terminalDispositions {
		args = append(args, string(disposition))
	}
	observation := observationKindList()
	ceiling, acquiringObservation := maxRuns, 0
	if OperationCapacityClass(op.Kind) == CapacityObservation {
		ceiling, acquiringObservation = maxObservations, 1
	}
	args = append(args, op.RunID)
	for _, kind := range observation {
		args = append(args, kind)
	}
	args = append(args, acquiringObservation, ceiling)
	// The verification ceiling (#490) is asked only of a verification
	// operation, and counted exactly as the class ceiling is: other runs
	// holding a leased verification operation, read from the document's kind.
	acquiringVerification := 0
	if consumesVerification(op.Kind) {
		acquiringVerification = 1
	}
	count, verificationArgs := verificationCountSQL()
	args = append(args, acquiringVerification)
	args = append(args, verificationArgs...)
	args = append(args, maxVerifications)
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(terminalDispositions)), ",")
	kinds := strings.TrimSuffix(strings.Repeat("?,", len(observation)), ",")
	result, err := s.db.Exec(`UPDATE run_operations SET revision = revision + 1, document = ?
		WHERE id = ? AND revision = ?
		  AND NOT EXISTS (SELECT 1 FROM run_operations AS other
		       WHERE other.run_id = ? AND other.id <> ?
		         AND json_extract(other.document, '$.state') IN ('leased', 'running')
		         AND json_extract(other.document, '$.lease') IS NOT NULL)
		  AND NOT EXISTS (SELECT 1 FROM runs WHERE runs.id = ?
		       AND json_extract(runs.document, '$.disposition') IN (`+placeholders+`))
		  AND NOT (`+runPausedSQL("run_operations.run_id")+`)
		  AND NOT EXISTS (SELECT 1 FROM verification_permits AS held
		       WHERE json_extract(held.document, '$.state') = 'granted'
		         AND json_extract(held.document, '$.parent.RunID') = run_operations.run_id)
		  AND (SELECT COUNT(DISTINCT run_id) FROM run_operations
		       WHERE run_id <> ? AND json_extract(document, '$.state') IN ('leased', 'running')
		         AND json_extract(document, '$.lease') IS NOT NULL
		         AND COALESCE(json_extract(document, '$.kind') IN (`+kinds+`), 0) = ?) < ?
		  AND (? = 0 OR (`+count+`) < ?)`,
		args...)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return 0, false, err
	}
	return expected + 1, true, nil
}

func scanOperations(rows *sql.Rows) ([]RunOperation, error) {
	defer rows.Close()
	out := []RunOperation{}
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		op, err := decodeOperation(document)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, rows.Err()
}
func decodeOperation(document string) (RunOperation, error) {
	var op RunOperation
	if err := json.Unmarshal([]byte(document), &op); err != nil {
		return RunOperation{}, fmt.Errorf("decode durable operation: %w", err)
	}
	return op, nil
}
