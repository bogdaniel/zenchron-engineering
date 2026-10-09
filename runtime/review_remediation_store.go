package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/review"
)

// reviewRemediationBindingInvariants re-establishes EVERY invariant an
// admission must satisfy to be treated as authorized remediation for run,
// independent of how or when the row was written: its referenced decision is
// REQUEST_CHANGES and agrees with the admission's own claimed subject; the
// RUN ITSELF - never merely the admission's claim - published the exact PR
// number and is worked by the exact producer agent the decision names; and
// the admission's FindingSignatures are EXACTLY the decision's own blocking
// findings, never an invented or substituted set.
//
// This is the one place "is this admission actually good" is answered,
// called both when a row is written (CreateReviewRemediationAdmission) and
// whenever one is read back as a candidate for execution
// (pendingReviewRemediationKeys). The second call site is what closes the
// gap the first alone cannot: a direct CreateReviewRemediationAdmission call
// for a DIFFERENT, otherwise-valid REQUEST_CHANGES decision - a different PR
// number, at the same repository and head, naming the SAME run and producer
// - would still pass a check that only compares the admission against its
// own referenced decision, because that decision and that admission agree
// with EACH OTHER; it is the run's actual published PR, re-read fresh at
// consumption time, that such a forged pair can never agree with.
func reviewRemediationBindingInvariants(decision review.Decision, admission ReviewRemediationAdmission, run EngineeringRun, projection RunProjection) error {
	if decision.Verdict != review.VerdictRequestChanges {
		return fmt.Errorf("decision %s verdict is %q, not %q", decision.ID, decision.Verdict, review.VerdictRequestChanges)
	}
	if decision.RunID != admission.RunID || decision.Subject.Repository != admission.Repository ||
		decision.Subject.PRNumber != admission.PRNumber || decision.Subject.HeadSHA != admission.HeadSHA {
		return fmt.Errorf("admission %s disagrees with its own referenced decision %s's subject", admission.DecisionID, decision.ID)
	}
	if run.Repository != admission.Repository {
		return fmt.Errorf("run %s's own repository %q disagrees with admission %s's claimed %q", run.ID, run.Repository, admission.DecisionID, admission.Repository)
	}
	if projection.PullRequest == nil || projection.PullRequest.Number != admission.PRNumber {
		return fmt.Errorf("run %s's own runtime-recorded pull request is not #%d", run.ID, admission.PRNumber)
	}
	if decision.ProducerAgentID == "" || run.AgentID != decision.ProducerAgentID {
		return fmt.Errorf("run %s's own agent %q disagrees with decision %s's claimed producer %q", run.ID, run.AgentID, decision.ID, decision.ProducerAgentID)
	}
	want := blockingFindingSignatures(decision.Findings)
	got := append([]string(nil), admission.FindingSignatures...)
	sort.Strings(got)
	if !slices.Equal(want, got) {
		return fmt.Errorf("admission %s's finding signatures disagree with decision %s's actual blocking findings", admission.DecisionID, decision.ID)
	}
	return nil
}

// blockingFindingSignatures is the sorted set of signatures a decision's
// blocking findings carry - the one authoritative value an admission's own
// FindingSignatures must equal exactly.
func blockingFindingSignatures(findings []review.Finding) []string {
	var signatures []string
	for _, finding := range findings {
		if finding.Severity == review.SeverityBlocking {
			signatures = append(signatures, finding.Signature)
		}
	}
	sort.Strings(signatures)
	return signatures
}

// CreateReviewRemediationAdmission writes admission ONCE, globally, keyed by
// its DecisionID (#474). A decision already admitted - by this caller, by a
// concurrent one, or by an earlier crashed attempt that committed before
// crashing - is never admitted a second time: the stored document is
// returned instead, with created=false, exactly as
// review.CreateReviewDecision already does for the decision itself.
//
// This table is now the sole authorization source bindExecutionInvoke's
// deferred wiring will trust, so it must never accept a row that is merely
// self-consistent with the decision it names: every insert re-reads that
// decision AND the named run's own durable state and runs
// reviewRemediationBindingInvariants before committing. review_decisions is
// insert-only and never rewritten (review/decision.go), so reading it here
// is safe without a transaction; the run's own state can change concurrently,
// but that only ever makes this check MORE conservative; it cannot be raced
// into wrongly approving. This closes the gap AdmitReviewRemediation's own
// checks cannot: AdmitReviewRemediation is the one INTENDED caller, but
// nothing in Go stops another caller in this package from calling this
// exported method directly with a hand-built, structurally valid but
// unauthorized document - the write itself must refuse that, not merely the
// one caller this package currently has.
func (s *SQLiteOperationStore) CreateReviewRemediationAdmission(admission ReviewRemediationAdmission) (ReviewRemediationAdmission, bool, error) {
	if err := admission.validate(); err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	decision, found, err := s.ReviewDecision(admission.DecisionID)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if !found {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("review remediation admission %s names a decision that does not exist", admission.DecisionID)
	}
	run, found, err := s.Run(admission.RunID)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if !found {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("review remediation admission %s names a run that does not exist", admission.DecisionID)
	}
	events, err := s.Events(admission.RunID)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	projection, err := Project(events)
	if err != nil {
		return ReviewRemediationAdmission{}, false, err
	}
	if err := reviewRemediationBindingInvariants(decision, admission, run, projection); err != nil {
		return ReviewRemediationAdmission{}, false, fmt.Errorf("review remediation admission %s refused: %w", admission.DecisionID, err)
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
//
// Like ReviewPublication's own row/document identity check, a row whose
// INDEXED run_id column disagrees with its document's own claimed RunID is
// refused outright rather than silently returned under whichever identity a
// caller queried by: CreateReviewRemediationAdmission never writes such a
// row, so one existing is evidence the table itself cannot be trusted
// without investigation, not something to paper over by picking a side.
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
		if admission.RunID != runID {
			return nil, fmt.Errorf("stored review remediation admission %s disagrees with its own indexed run %q (names %q)", admission.DecisionID, runID, admission.RunID)
		}
		admissions = append(admissions, admission)
	}
	return admissions, rows.Err()
}
