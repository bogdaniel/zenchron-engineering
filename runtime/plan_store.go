package runtime

// Durable EngineeringPlans, in the runtime store that already exists.
//
// There is no second database, no second migration mechanism and no second
// journal here: plans live in runtime.db beside runs and operations, and their
// lifecycle events go through the same append-only, hash-chained table under
// the same sequence allocation. What is new is the immutability of a revision.
//
// A run row is an upsert because a run's projection moves. A plan REVISION is
// not: an assignment, an approval and an event digest all point at exact plan
// content, so rewriting a stored revision would silently change what an
// operator approved and what a worker was assigned. Writing a different
// document under a revision that already exists is therefore a typed refusal.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const sqlitePlanColumns = `id, repository, current_revision, created_unix_nano`

// PlanRevisionConflictError is the refusal to overwrite a stored plan revision.
// It names both digests so an operator can see that the revision number was
// reused rather than that a write was merely rejected.
type PlanRevisionConflictError struct {
	PlanID   string
	Revision int
	Stored   string
	Offered  string
}

func (e *PlanRevisionConflictError) Error() string {
	return fmt.Sprintf("plan %s revision %d already exists with digest %s and cannot be rewritten as %s: a plan revision is immutable, and a change is a NEW revision",
		e.PlanID, e.Revision, short12(e.Stored), short12(e.Offered))
}

// ClaimPlan durably claims a plan identity and stores its first revision.
//
// Like ClaimRun it is a conditional insert, so the database decides which
// process created the plan and a caller that loses the race adopts the winner's
// plan rather than overwriting it.
//
// createdAt comes from the CALLER's clock, exactly as a run's does. The store
// reads no clock of its own: a plan document carries no timestamp - it would
// make the digest depend on when it was written - so this row is the only
// record of when the plan appeared, and it must be on the same time axis as
// every other row the runtime writes.
func (s *SQLiteOperationStore) ClaimPlan(plan domain.EngineeringPlan, createdAt time.Time) (bool, error) {
	if err := validatePlanIdentity(plan); err != nil {
		return false, err
	}
	document, err := CanonicalJSON(plan)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`INSERT INTO plans (`+sqlitePlanColumns+`) VALUES (?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		plan.ID, plan.Subject.Repository, plan.Revision, createdAt.UnixNano())
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if claimed != 1 {
		// The row exists. That is a competing proposal ONLY if it carries a
		// revision; a row at current_revision 0 is an ATTEMPT identity - this
		// plan's own earlier reasoning proposal, refused before it compiled -
		// and refusing here would mean a plan whose first attempt failed could
		// never be planned again. The conditional UPDATE is what makes the
		// promotion a race the database decides: exactly one writer moves the
		// row off zero.
		// The repository moves with the revision. The attempt row recorded the
		// repository the PROPOSER named; the revision document carries the one
		// its subject is bound to, and that is the one every later read should
		// see. Leaving the old value would have PlanIdentities describing a
		// plan by a repository its own document does not name.
		promoted, err := tx.Exec(`UPDATE plans SET current_revision = ?, repository = ? WHERE id = ? AND current_revision = 0`,
			plan.Revision, plan.Subject.Repository, plan.ID)
		if err != nil {
			return false, err
		}
		moved, err := promoted.RowsAffected()
		if err != nil || moved != 1 {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO plan_revisions (plan_id, revision, digest, document) VALUES (?, ?, ?, ?)`,
		plan.ID, plan.Revision, plan.Digest, string(document)); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ClaimPlanAttempt claims a plan IDENTITY with no revision.
//
// It is what a refused first proposal needs and nothing more. A plan identity
// is derived - repository, issue, operator configuration - so it exists as a
// fact the moment an operator asks to plan that issue; what does not exist,
// when compilation refuses the proposal, is a revision. This row carries
// current_revision 0 and no plan_revisions row, which means:
//
//   - Plans() joins on current_revision and therefore never returns it, so
//     nothing that lists executable work can see it;
//   - Plan() resolves through PlanRevision() and reports not-found, so nothing
//     that reads a plan document can mistake it for one;
//   - the plan's journal stream exists, so the refused attempt recorded in it
//     is durable, replayable and readable after a restart.
//
// In other words the identity is the hook the evidence hangs on, and it grants
// nothing. It reports whether THIS call created the row; an existing row - of
// either kind - is not an error, because a second refused attempt at the same
// plan is an ordinary thing to happen.
func (s *SQLiteOperationStore) ClaimPlanAttempt(planID, repository string, createdAt time.Time) (bool, error) {
	if planID == "" || repository == "" {
		return false, fmt.Errorf("a plan attempt identity needs a plan id and a repository")
	}
	result, err := s.db.Exec(`INSERT INTO plans (`+sqlitePlanColumns+`) VALUES (?, ?, 0, ?) ON CONFLICT(id) DO NOTHING`,
		planID, repository, createdAt.UnixNano())
	if err != nil {
		return false, err
	}
	claimed, err := result.RowsAffected()
	return claimed == 1, err
}

// PlanIdentity is a plan row, whether or not it has a revision. Revision 0
// means no revision has ever been stored: the identity exists because planning
// was attempted, and every attempt so far was refused.
type PlanIdentity struct {
	PlanID     string `json:"plan_id"`
	Repository string `json:"repository"`
	Revision   int    `json:"current_revision"`
	Issue      int    `json:"source_issue,omitempty"`
}

// PlanIdentities lists every plan row in creation order, including the ones
// that have no revision.
//
// Plans() deliberately cannot answer this: it joins each plan to its governing
// revision document, which is exactly what an attempt-only plan does not have.
// A list that showed only plans with documents is what told an operator "no
// plans" about work they had just asked for and paid a provider invocation on.
func (s *SQLiteOperationStore) PlanIdentities() ([]PlanIdentity, error) {
	rows, err := s.db.Query(`SELECT id, repository, current_revision, source_issue FROM plans
		ORDER BY created_unix_nano ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var identities []PlanIdentity
	for rows.Next() {
		var identity PlanIdentity
		if err := rows.Scan(&identity.PlanID, &identity.Repository, &identity.Revision, &identity.Issue); err != nil {
			return nil, err
		}
		identities = append(identities, identity)
	}
	return identities, rows.Err()
}

// PutPlanRevision stores one revision of an already-claimed plan and reports
// whether THIS call created it.
//
// Re-storing the SAME document is a no-op, so a retried write is safe. Storing
// a DIFFERENT document under an existing revision is refused: that is not an
// update, it is a rewrite of history somebody has already approved or been
// assigned against.
//
// The boolean matters because the no-op is indistinguishable from a first
// write at the caller otherwise, and two proposers computing the same next
// revision from the same stored state both "succeeded" - and both announced a
// proposal for one revision. The insert decides, inside this transaction, which
// one of them actually made it.
func (s *SQLiteOperationStore) PutPlanRevision(plan domain.EngineeringPlan) (bool, error) {
	if err := validatePlanIdentity(plan); err != nil {
		return false, err
	}
	document, err := CanonicalJSON(plan)
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var stored string
	switch err := tx.QueryRow(`SELECT digest FROM plan_revisions WHERE plan_id = ? AND revision = ?`, plan.ID, plan.Revision).Scan(&stored); err {
	case nil:
		if stored != plan.Digest {
			return false, &PlanRevisionConflictError{PlanID: plan.ID, Revision: plan.Revision, Stored: stored, Offered: plan.Digest}
		}
		return false, tx.Commit()
	case sql.ErrNoRows:
	default:
		return false, err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(1) FROM plans WHERE id = ?`, plan.ID).Scan(&exists); err != nil {
		return false, err
	}
	if exists == 0 {
		return false, fmt.Errorf("unknown plan %q", plan.ID)
	}
	if _, err := tx.Exec(`INSERT INTO plan_revisions (plan_id, revision, digest, document) VALUES (?, ?, ?, ?)`,
		plan.ID, plan.Revision, plan.Digest, string(document)); err != nil {
		return false, err
	}
	// current_revision tracks the HIGHEST revision written, which is not the
	// same question as which revision is approved. Approval lives in the
	// journal, so a proposed-but-unapproved revision can be stored - it has to
	// be, an operator has to read it before deciding - without that storage
	// implying any authority to execute it.
	if _, err := tx.Exec(`UPDATE plans SET current_revision = ? WHERE id = ? AND current_revision < ?`,
		plan.Revision, plan.ID, plan.Revision); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Plan returns the highest stored revision of one plan.
func (s *SQLiteOperationStore) Plan(id string) (domain.EngineeringPlan, bool, error) {
	var revision int
	err := s.db.QueryRow(`SELECT current_revision FROM plans WHERE id = ?`, id).Scan(&revision)
	if err == sql.ErrNoRows {
		return domain.EngineeringPlan{}, false, nil
	}
	if err != nil {
		return domain.EngineeringPlan{}, false, err
	}
	return s.PlanRevision(id, revision)
}

// PlanRevision returns one exact stored revision. It is what an assignment, an
// approval and a superseded-revision reference all resolve through, so a
// historical plan stays readable after any number of later revisions.
func (s *SQLiteOperationStore) PlanRevision(id string, revision int) (domain.EngineeringPlan, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM plan_revisions WHERE plan_id = ? AND revision = ?`, id, revision).Scan(&document)
	if err == sql.ErrNoRows {
		return domain.EngineeringPlan{}, false, nil
	}
	if err != nil {
		return domain.EngineeringPlan{}, false, err
	}
	plan, err := decodePlan(document)
	return plan, err == nil, err
}

// Plans lists every plan at its highest stored revision, oldest first.
func (s *SQLiteOperationStore) Plans() ([]domain.EngineeringPlan, error) {
	rows, err := s.db.Query(`SELECT r.document FROM plans p
		JOIN plan_revisions r ON r.plan_id = p.id AND r.revision = p.current_revision
		ORDER BY p.created_unix_nano ASC, p.id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var plans []domain.EngineeringPlan
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		plan, err := decodePlan(document)
		if err != nil {
			return nil, err
		}
		plans = append(plans, plan)
	}
	return plans, rows.Err()
}

// AppendPlanEvent appends one event to a plan's stream. It shares the sequence
// allocation, chain linking, state digests and transaction with AppendEvent:
// there is one hash chain implementation in this runtime, and a plan's history
// is as tamper-evident as a run's because it IS the same mechanism.
func (s *SQLiteOperationStore) AppendPlanEvent(e EngineeringEvent) (EngineeringEvent, error) {
	if e.ID == "" || e.PlanID == "" {
		return EngineeringEvent{}, fmt.Errorf("event id and plan id are required")
	}
	if e.RunID != "" {
		return EngineeringEvent{}, fmt.Errorf("plan event %q names run %q: an event belongs to exactly one stream, and the plan/run association is stated by %s", e.ID, e.RunID, EventPlanRunStarted)
	}
	if !planEventTypes[e.Type] {
		return EngineeringEvent{}, fmt.Errorf("event type %q does not belong to the plan stream", e.Type)
	}
	return s.appendToStream(e, journalStream{
		kind: streamPlan, id: e.PlanID,
		// The plan row is read inside the transaction, for the same reason the
		// run row is: an event may not be journalled against a plan that does
		// not exist.
		bind: func(tx *sql.Tx) error {
			var exists int
			if err := tx.QueryRow(`SELECT COUNT(1) FROM plans WHERE id = ?`, e.PlanID).Scan(&exists); err != nil {
				return err
			}
			if exists == 0 {
				return fmt.Errorf("unknown plan %q", e.PlanID)
			}
			return nil
		},
		events: func(tx *sql.Tx) ([]EngineeringEvent, error) { return queryPlanEvents(tx, e.PlanID) },
		digest: func(events []EngineeringEvent) (string, error) {
			snapshot, err := ReducePlan(e.PlanID, events)
			return snapshot.StateSHA256, err
		},
		insert: func(tx *sql.Tx, event EngineeringEvent, canonical string) error {
			_, err := tx.Exec(`INSERT INTO events (`+sqlitePlanEventColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				event.ID, "", event.Sequence, event.Type, event.OperationID, event.PreviousEventID,
				event.PreviousEventHash, event.StateBefore, event.StateAfter, event.EventHash, canonical,
				streamPlan, event.PlanID)
			return err
		},
	})
}

// PlanEvents returns one plan's events in sequence order.
func (s *SQLiteOperationStore) PlanEvents(planID string) ([]EngineeringEvent, error) {
	return queryPlanEvents(s.db, planID)
}

// ReplayPlan rebuilds plan state from the durable journal alone.
//
// "From the journal alone" is the point: no provider session, no in-memory
// cache and no stored counter contributes, so a supervisor restarted mid-plan
// reconstructs the same approval state, the same assignments, the same gate
// satisfaction and the same consumed budget it had before.
func (s *SQLiteOperationStore) ReplayPlan(planID string) (PlanSnapshot, error) {
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM plans WHERE id = ?`, planID).Scan(&exists); err != nil {
		return PlanSnapshot{}, err
	}
	if exists == 0 {
		return PlanSnapshot{}, fmt.Errorf("unknown plan %q", planID)
	}
	events, err := s.PlanEvents(planID)
	if err != nil {
		return PlanSnapshot{}, err
	}
	return ReducePlan(planID, events)
}

func validatePlanIdentity(plan domain.EngineeringPlan) error {
	if plan.ID == "" {
		return fmt.Errorf("plan id is required")
	}
	if plan.Revision < 1 {
		return fmt.Errorf("plan %q revision %d must be positive", plan.ID, plan.Revision)
	}
	if plan.Digest == "" {
		return fmt.Errorf("plan %q revision %d has no content digest", plan.ID, plan.Revision)
	}
	// The digest is what every reference to this revision resolves through, so
	// a document whose stated digest is not its own content is refused rather
	// than stored under a name it does not have.
	computed, err := plan.ContentDigest()
	if err != nil {
		return err
	}
	if computed != plan.Digest {
		return fmt.Errorf("plan %q revision %d states digest %s but its content digests to %s",
			plan.ID, plan.Revision, short12(plan.Digest), short12(computed))
	}
	if plan.Subject.Repository == "" {
		return fmt.Errorf("plan %q names no repository", plan.ID)
	}
	return nil
}

func decodePlan(document string) (domain.EngineeringPlan, error) {
	var plan domain.EngineeringPlan
	if err := json.Unmarshal([]byte(document), &plan); err != nil {
		return domain.EngineeringPlan{}, fmt.Errorf("decode durable plan: %w", err)
	}
	return plan, nil
}
