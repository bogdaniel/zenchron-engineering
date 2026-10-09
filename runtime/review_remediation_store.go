package runtime

import (
	"database/sql"
	"errors"
	"fmt"
)

// CreateReviewRemediationAdmission writes admission ONCE, globally, keyed by
// its DecisionID (#474). A decision already admitted - by this caller, by a
// concurrent one, or by an earlier crashed attempt that committed before
// crashing - is never admitted a second time: the stored document is
// returned instead, with created=false, exactly as
// review.CreateReviewDecision already does for the decision itself.
func (s *SQLiteOperationStore) CreateReviewRemediationAdmission(admission ReviewRemediationAdmission) (ReviewRemediationAdmission, bool, error) {
	if err := admission.validate(); err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	document, err := CanonicalJSON(admission)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO review_remediation_admissions (decision_id, run_id, admitted_unix_nano, document)
		VALUES (?, ?, ?, ?) ON CONFLICT(decision_id) DO NOTHING`,
		admission.DecisionID, admission.RunID, admission.AdmittedAt.UnixNano(), string(document))
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if inserted == 1 {
		return admission, true, nil
	}
	stored, found, err := s.ReviewRemediationAdmission(admission.DecisionID)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if !found {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("review remediation admission %s was neither inserted nor found", admission.DecisionID)
	}
	return stored, false, nil
}

// ReviewRemediationAdmission reads one admission by the exact DecisionID it
// admits. found=false means that decision has never been admitted for
// remediation.
func (s *SQLiteOperationStore) ReviewRemediationAdmission(decisionID string) (ReviewRemediationAdmission, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM review_remediation_admissions WHERE decision_id = ?`, decisionID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRemediationAdmission{}, false, nil
	}
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	var admission ReviewRemediationAdmission
	if err := strictJSON([]byte(document), &admission); err != nil {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("stored review remediation admission is unreadable: %w", err)
	}
	if err := admission.validate(); err != nil {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("stored review remediation admission is corrupt: %w", err)
	}
	return admission, true, nil
}

// ReviewRemediationAdmissionsForRun reads every admission ever recorded for
// runID, across every exact head it has ever been admitted against. This -
// not a journal event - is what pendingReviewRemediationKeys folds from: the
// admission row is the complete, single-write durable authorization.
func (s *SQLiteOperationStore) ReviewRemediationAdmissionsForRun(runID string) ([]ReviewRemediationAdmission, error) {
	rows, err := s.db.Query(`SELECT document FROM review_remediation_admissions WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var admissions []ReviewRemediationAdmission
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var admission ReviewRemediationAdmission
		if err := strictJSON([]byte(document), &admission); err != nil {
			return nil, fmt.Errorf("stored review remediation admission is unreadable: %w", err)
		}
		admissions = append(admissions, admission)
	}
	return admissions, rows.Err()
}
