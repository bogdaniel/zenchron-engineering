package runtime

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// CreateReviewDecision writes a decision ONCE. A decision whose identity
// (review.DecisionID: exact subject plus exact reviewer) is already stored is
// not rewritten: the stored document is returned instead, with created=false.
// This is the whole crash/retry story for the decision itself - a repeated
// `review pr` for a head and reviewer already reviewed finds what it already
// decided rather than performing a second independent verdict.
func (s *SQLiteOperationStore) CreateReviewDecision(decision review.Decision) (review.Decision, bool, error) {
	if err := decision.Validate(); err != nil {
		return review.Decision{}, false, err
	}
	document, err := CanonicalJSON(decision)
	if err != nil {
		return review.Decision{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO review_decisions (id, repository, pr_number, head_sha, run_id, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		decision.ID, decision.Subject.Repository, decision.Subject.PRNumber, decision.Subject.HeadSHA,
		decision.RunID, decision.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return review.Decision{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return review.Decision{}, false, err
	}
	if inserted == 1 {
		return decision, true, nil
	}
	stored, found, err := s.ReviewDecision(decision.ID)
	if err != nil {
		return review.Decision{}, false, err
	}
	if !found {
		return review.Decision{}, false, fmt.Errorf("review decision %s was neither inserted nor found", decision.ID)
	}
	return stored, false, nil
}

// ReviewDecision reads one decision by its exact identity.
func (s *SQLiteOperationStore) ReviewDecision(id string) (review.Decision, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM review_decisions WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return review.Decision{}, false, nil
	}
	if err != nil {
		return review.Decision{}, false, err
	}
	decision, err := decodeReviewDecision(document)
	return decision, err == nil, err
}

// ReviewDecisionsForPullRequest reads every decision ever reached for one
// pull request, oldest first. A superseded decision is never deleted: it
// stays durable historical evidence for the head it was bound to (#233
// acceptance C), and this is how an operator reads that history back.
func (s *SQLiteOperationStore) ReviewDecisionsForPullRequest(repository string, prNumber int) ([]review.Decision, error) {
	rows, err := s.db.Query(`SELECT document FROM review_decisions WHERE repository = ? AND pr_number = ? ORDER BY created_unix_nano ASC, id ASC`,
		repository, prNumber)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var decisions []review.Decision
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		decision, err := decodeReviewDecision(document)
		if err != nil {
			return nil, err
		}
		decisions = append(decisions, decision)
	}
	return decisions, rows.Err()
}

// LatestReviewDecision reads the most recently created decision for one pull
// request, which is what review status and staleness read against: the
// current question is always "what did the LAST independent review of this PR
// conclude", never an arbitrary earlier one.
func (s *SQLiteOperationStore) LatestReviewDecision(repository string, prNumber int) (review.Decision, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM review_decisions WHERE repository = ? AND pr_number = ?
		ORDER BY created_unix_nano DESC, id DESC LIMIT 1`, repository, prNumber).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return review.Decision{}, false, nil
	}
	if err != nil {
		return review.Decision{}, false, err
	}
	decision, err := decodeReviewDecision(document)
	return decision, err == nil, err
}

func decodeReviewDecision(document string) (review.Decision, error) {
	var decision review.Decision
	if err := strictJSON([]byte(document), &decision); err != nil {
		return review.Decision{}, fmt.Errorf("stored review decision is unreadable: %w", err)
	}
	if err := decision.Validate(); err != nil {
		return review.Decision{}, fmt.Errorf("stored review decision is corrupt: %w", err)
	}
	return decision, nil
}

// RecordReviewPublication writes a decision's publication state ONCE. Like
// CreateReviewDecision, it is insert-only: a decision already recorded as
// published is left exactly as it was, which is the other half of the
// crash/retry guarantee - a controller that crashed after GitHub accepted the
// review but before this write landed must not be able to overwrite the
// GitHubReviewID a later reconciliation pass observed.
func (s *SQLiteOperationStore) RecordReviewPublication(pub review.Publication) (bool, error) {
	if pub.DecisionID == "" {
		return false, errors.New("a review publication requires its decision identity")
	}
	document, err := CanonicalJSON(pub)
	if err != nil {
		return false, err
	}
	result, err := s.db.Exec(`INSERT INTO review_publications (decision_id, document) VALUES (?, ?) ON CONFLICT(decision_id) DO NOTHING`,
		pub.DecisionID, string(document))
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// ReviewPublication reads one decision's publication state. found=false means
// no publication has ever been recorded for this decision - never attempted,
// or attempted and not yet durable - which is exactly the state in which a
// publication driver must ask GitHub itself whether an earlier attempt landed
// before submitting a second one.
func (s *SQLiteOperationStore) ReviewPublication(decisionID string) (review.Publication, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM review_publications WHERE decision_id = ?`, decisionID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return review.Publication{}, false, nil
	}
	if err != nil {
		return review.Publication{}, false, err
	}
	var pub review.Publication
	if err := strictJSON([]byte(document), &pub); err != nil {
		return review.Publication{}, false, fmt.Errorf("stored review publication is unreadable: %w", err)
	}
	return pub, true, nil
}
