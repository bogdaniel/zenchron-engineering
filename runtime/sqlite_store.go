package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

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
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)&_txlock=immediate"
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
// table. A run holds a slot exactly while one of its operations is leased or
// running, so a run parked on CI, authority, auth, or opt-in removal holds
// nothing - there is no slot to forget to release, and a durable run that
// nobody is driving never occupies one. Reclaiming a crashed driver's slot is
// therefore the existing lease takeover, which CanAcquire already gates on
// owner death AND expiry, so an expired heartbeat alone still steals nothing.
func (s *SQLiteOperationStore) AcquireOperation(op RunOperation, expected int64, maxRuns int) (int64, bool, error) {
	if op.ID == "" || expected <= 0 {
		return 0, false, fmt.Errorf("acquiring an operation needs its id and the revision it was read at")
	}
	document, err := CanonicalJSON(op)
	if err != nil {
		return 0, false, err
	}
	result, err := s.db.Exec(`UPDATE run_operations SET revision = revision + 1, document = ?
		WHERE id = ? AND revision = ?
		  AND (SELECT COUNT(DISTINCT run_id) FROM run_operations
		       WHERE run_id <> ? AND json_extract(document, '$.state') IN ('leased', 'running')) < ?`,
		string(document), op.ID, expected, op.RunID, maxRuns)
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
