package runtime

import (
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

// CreateReviewDecisionFenced is CreateReviewDecision, but the insert only
// lands if claimKey is STILL held by token at the moment the write executes
// (#233 B4-A). A renewal loop checked just before this call cannot close the
// window between "we last confirmed we still own the claim" and "we write
// the decision" - a steal can land in between. Gating the INSERT on the
// current claim row, in the SAME statement, closes that window: this
// statement and a concurrent ClaimReview steal are both single writes
// against this database, and SQLite serializes them, so there is no
// interleaving in which both observe the claim as still held.
//
// found=false with a *ReviewClaimLostError means neither case above applies:
// the row was not already present AND the WHERE EXISTS guard refused the
// write, which only happens when claimKey no longer names token as its
// holder. Admitting the decision anyway would let a superseded caller's
// verdict become durable truth after another caller has already taken over
// the same review.
func (s *SQLiteOperationStore) CreateReviewDecisionFenced(decision review.Decision, claimKey, token string) (review.Decision, bool, error) {
	if err := decision.Validate(); err != nil {
		return review.Decision{}, false, err
	}
	document, err := CanonicalJSON(decision)
	if err != nil {
		return review.Decision{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO review_decisions (id, repository, pr_number, head_sha, run_id, created_unix_nano, document)
		SELECT ?, ?, ?, ?, ?, ?, ?
		WHERE EXISTS (SELECT 1 FROM review_claims WHERE claim_key = ? AND token = ?)
		ON CONFLICT(id) DO NOTHING`,
		decision.ID, decision.Subject.Repository, decision.Subject.PRNumber, decision.Subject.HeadSHA,
		decision.RunID, decision.CreatedAt.UnixNano(), string(document), claimKey, token)
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
	if found {
		return stored, false, nil
	}
	return review.Decision{}, false, &ReviewClaimLostError{ClaimKey: claimKey}
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

// RecordReviewPublicationAttempt durably marks decisionID's external
// submission as started, BEFORE PublishReview calls GitHub.SubmitReview
// (#233 P1). A row surviving here with no corresponding ReviewPublication is
// the uncertain-outcome marker: proof some caller began a POST whose result
// was never confirmed. An upsert, not insert-once: a caller legitimately
// retrying its OWN unresolved attempt (the only case that reaches this call
// at all - see PublishReview's own prior-attempt check) refreshes the
// timestamp rather than being refused by its own earlier row.
func (s *SQLiteOperationStore) RecordReviewPublicationAttempt(decisionID string, now time.Time) error {
	if decisionID == "" {
		return errors.New("a review publication attempt requires its decision identity")
	}
	_, err := s.db.Exec(`INSERT INTO review_publication_attempts (decision_id, started_unix_nano) VALUES (?, ?)
		ON CONFLICT(decision_id) DO UPDATE SET started_unix_nano = excluded.started_unix_nano`,
		decisionID, now.UnixNano())
	return err
}

// ReviewPublicationAttempt reads whether decisionID has an unresolved
// external-submission attempt outstanding. found=false means no attempt is
// outstanding - either none was ever made, or the one that was has already
// been resolved (ClearReviewPublicationAttempt) one way or another.
func (s *SQLiteOperationStore) ReviewPublicationAttempt(decisionID string) (startedAt time.Time, found bool, err error) {
	var startedNano int64
	err = s.db.QueryRow(`SELECT started_unix_nano FROM review_publication_attempts WHERE decision_id = ?`, decisionID).Scan(&startedNano)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	return time.Unix(0, startedNano).UTC(), true, nil
}

// ClearReviewPublicationAttempt resolves decisionID's outstanding attempt
// marker, once its outcome is known - either this invocation's own
// SubmitReview succeeded, or a later reconciliation found GitHub's matching
// review. It is always safe to call on a decision with no outstanding
// attempt (a no-op), which is what a direct reconciliation path - one that
// never itself called RecordReviewPublicationAttempt - needs.
func (s *SQLiteOperationStore) ClearReviewPublicationAttempt(decisionID string) error {
	_, err := s.db.Exec(`DELETE FROM review_publication_attempts WHERE decision_id = ?`, decisionID)
	return err
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
	// The row's own key and the document's own claimed identity must agree,
	// the same check queryStreamEvents already makes for a journal row: a
	// disagreement is not decidable by a caller that only sees the decoded
	// value, so it is refused here rather than silently handed back as if it
	// named decisionID.
	if pub.DecisionID != decisionID {
		return review.Publication{}, false, fmt.Errorf("stored review publication row %q disagrees with its own document (names %q)", decisionID, pub.DecisionID)
	}
	return pub, true, nil
}

// ClaimReview atomically claims claimKey for owner (#233 B4), stealing a
// claim older than staleAfter. It reports true iff the caller now owns the
// claim - via a fresh insert, or by reclaiming an abandoned one - and false
// when another, still-active claimant holds it. On success it also returns a
// fresh, unique FENCING TOKEN that must accompany every later
// Renew/ReleaseReviewClaim call for this claim.
//
// The token is what makes a steal safe. Without one, a claimant whose own
// claim was already stolen for running past staleAfter would still delete
// BY KEY ALONE on its eventual (stale) completion - deleting the NEW owner's
// live claim out from under it and letting a third caller start while the
// second is still active. A release or renewal naming the wrong (superseded)
// token is a correctly-ignored no-op instead.
//
// The insert and the steal are ONE statement: SQLite serializes it against
// every other writer on this database, so two concurrent callers can never
// both observe "unclaimed" and both proceed, the way a separate read-then-write
// would allow.
func (s *SQLiteOperationStore) ClaimReview(claimKey, owner string, now time.Time, staleAfter time.Duration) (claimed bool, token string, err error) {
	if claimKey == "" || owner == "" {
		return false, "", fmt.Errorf("a review claim requires its key and owner")
	}
	token = rand.Text()
	threshold := now.Add(-staleAfter).UnixNano()
	result, err := s.db.Exec(`INSERT INTO review_claims (claim_key, claimed_unix_nano, owner, token) VALUES (?, ?, ?, ?)
		ON CONFLICT(claim_key) DO UPDATE SET claimed_unix_nano = excluded.claimed_unix_nano, owner = excluded.owner, token = excluded.token
		WHERE review_claims.claimed_unix_nano < ?`,
		claimKey, now.UnixNano(), owner, token, threshold)
	if err != nil {
		return false, "", err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 1 {
		return false, "", err
	}
	return true, token, nil
}

// RenewReviewClaim extends claimKey's staleness clock to now, PROVIDED the
// caller still holds it (its token still matches). A genuinely long-running
// but still-alive holder renews periodically so a generous but finite
// staleAfter cannot steal an active claim out from under it; a holder whose
// claim was already stolen gets false back rather than silently believing it
// still owns an operation someone else now owns.
func (s *SQLiteOperationStore) RenewReviewClaim(claimKey, token string, now time.Time) (bool, error) {
	if claimKey == "" || token == "" {
		return false, fmt.Errorf("renewing a review claim requires its key and token")
	}
	result, err := s.db.Exec(`UPDATE review_claims SET claimed_unix_nano = ? WHERE claim_key = ? AND token = ?`, now.UnixNano(), claimKey, token)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ReleaseReviewClaim drops claimKey, PROVIDED token still names its current
// holder, so an ordinary completion (success or a clean, non-crash failure)
// lets the NEXT caller proceed immediately rather than waiting out the
// staleness bound. It is best-effort cleanup, never itself the sole
// correctness boundary: a release that never runs (the process crashed) is
// exactly the case ClaimReview's staleness reclaim covers, and a release
// naming a superseded token is correctly a no-op rather than deleting
// whichever caller holds the claim now.
func (s *SQLiteOperationStore) ReleaseReviewClaim(claimKey, token string) error {
	_, err := s.db.Exec(`DELETE FROM review_claims WHERE claim_key = ? AND token = ?`, claimKey, token)
	return err
}
