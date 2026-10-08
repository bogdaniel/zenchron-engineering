// Package review holds the durable, provider-independent vocabulary for an
// independent PR review (#233): the exact subject a review was performed
// against, the verdict it reached, and the idempotent record of publishing
// that verdict to GitHub.
//
// It owns no execution, no scheduling and no GitHub transport - those stay in
// runtime, which is the only package allowed to know a provider's name or
// speak to a forge. This package is domain vocabulary, the same relationship
// orchestration.Batch has to the runtime code that drives it.
package review

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion versions the durable Decision document.
const SchemaVersion = "0.1"

// Verdict is the one decision a completed independent review reaches. There
// is no fourth value: a review that found nothing worth blocking but is not
// authorized to state GitHub's APPROVE on the configured publication identity
// states CommentOnly instead of pretending GitHub approved (#233 requirement
// 13), and nothing here lets more than one of these be true at once.
type Verdict string

const (
	VerdictApprove        Verdict = "approve"
	VerdictRequestChanges Verdict = "request_changes"
	VerdictCommentOnly    Verdict = "comment_only"
)

func (v Verdict) valid() bool {
	switch v {
	case VerdictApprove, VerdictRequestChanges, VerdictCommentOnly:
		return true
	default:
		return false
	}
}

// Severity distinguishes a finding that must block the exact reviewed head
// from engineering debt that does not invalidate the claimed change.
type Severity string

const (
	SeverityBlocking    Severity = "blocking"
	SeverityNonBlocking Severity = "non_blocking"
)

func (s Severity) valid() bool {
	switch s {
	case SeverityBlocking, SeverityNonBlocking:
		return true
	default:
		return false
	}
}

// Finding is one bounded defect or debt item a review names. Signature is
// what remediation and deduplication key on; Detail, Path and Line are
// optional context for an operator and for GitHub inline-comment placement.
type Finding struct {
	Severity  Severity `json:"severity"`
	Signature string   `json:"signature"`
	Detail    string   `json:"detail,omitempty"`
	Path      string   `json:"path,omitempty"`
	Line      int      `json:"line,omitempty"`
}

func (f Finding) validate() error {
	if !f.Severity.valid() {
		return fmt.Errorf("finding severity %q must be %q or %q", f.Severity, SeverityBlocking, SeverityNonBlocking)
	}
	if strings.TrimSpace(f.Signature) == "" {
		return errors.New("a finding states no signature, so it identifies nothing")
	}
	if f.Line < 0 {
		return fmt.Errorf("finding line %d cannot be negative", f.Line)
	}
	return nil
}

// Subject is the exact thing one review was performed against. A review
// applies only to this exact head: see Decision.StaleAgainst.
type Subject struct {
	Repository string `json:"repository"`
	PRNumber   int    `json:"pr_number"`
	HeadSHA    string `json:"head_sha"`
}

func (s Subject) Validate() error {
	if strings.TrimSpace(s.Repository) == "" {
		return errors.New("a review subject requires a repository")
	}
	if s.PRNumber <= 0 {
		return errors.New("a review subject requires a positive pull request number")
	}
	if strings.TrimSpace(s.HeadSHA) == "" {
		return errors.New("a review subject requires the exact head commit it was performed against")
	}
	return nil
}

// Decision is the durable, one-shot verdict of one independent review
// operation. It is written once per (Subject, ReviewerAgentID): see
// DecisionID. Nothing here is ever rewritten in place - a moved head or a
// different reviewer always identifies a different Decision - which is what
// lets replay, restart and a second reconciliation pass find the exact
// decision a crash interrupted instead of risking a second one.
type Decision struct {
	SchemaVersion string  `json:"schema_version"`
	ID            string  `json:"id"`
	Subject       Subject `json:"subject"`
	// BaseSHA is the PR's target branch tip the review was shown as the
	// trusted base, recorded for an operator reading the decision later.
	BaseSHA string `json:"base_sha,omitempty"`
	// RunID is the producing EngineeringRun this PR was resolved to, when one
	// could be established. Required: #233 fails closed rather than inventing
	// provenance when a PR cannot be bound to a producing run.
	RunID string `json:"run_id"`
	// ProducerAgentID and ReviewerAgentID are the two identities independence
	// was evaluated between. Both are recorded even when they are equal,
	// because a Decision that could not establish independence is refused
	// before construction (see runtime's independence check) and a reader must
	// be able to see what was actually compared.
	ProducerAgentID string `json:"producer_agent_id"`
	ReviewerAgentID string `json:"reviewer_agent_id"`
	// ReviewerProviderKind and ReviewerVendorFamily are the reviewer's own
	// projected identity facts, recorded the same way a producer's are
	// elsewhere, so an operator can read independence directly off this
	// document without cross-referencing the agent registry.
	ReviewerProviderKind string    `json:"reviewer_provider_kind,omitempty"`
	ReviewerVendorFamily string    `json:"reviewer_vendor_family,omitempty"`
	Verdict              Verdict   `json:"verdict"`
	Findings             []Finding `json:"findings,omitempty"`
	Reason               string    `json:"reason,omitempty"`
	// ContextDigest is the digest of the ReviewPacket the reviewer was shown.
	// It is evidence a later reader can use to confirm what context produced
	// this verdict; it is never recomputed from repository state to revalidate
	// the verdict itself; that would require re-running the review.
	ContextDigest string    `json:"context_digest,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

func (d Decision) Validate() error {
	if d.SchemaVersion != SchemaVersion {
		return fmt.Errorf("review decision schema version %q is not %q", d.SchemaVersion, SchemaVersion)
	}
	if strings.TrimSpace(d.ID) == "" {
		return errors.New("a review decision requires its identity")
	}
	if err := d.Subject.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(d.RunID) == "" {
		return errors.New("a review decision requires the producing run it was bound to")
	}
	if strings.TrimSpace(d.ReviewerAgentID) == "" {
		return errors.New("a review decision requires the reviewing agent's identity")
	}
	if !d.Verdict.valid() {
		return fmt.Errorf("review verdict %q must be %q, %q or %q", d.Verdict, VerdictApprove, VerdictRequestChanges, VerdictCommentOnly)
	}
	hasBlocking := false
	for _, finding := range d.Findings {
		if err := finding.validate(); err != nil {
			return err
		}
		if finding.Severity == SeverityBlocking {
			hasBlocking = true
		}
	}
	// APPROVE and a blocking finding are two answers, the same way a plan-stage
	// accept and a finding are (runtime.AdmitReviewerResult). REQUEST_CHANGES
	// without a blocking finding would be unactionable: nothing would tell
	// remediation what to fix.
	switch d.Verdict {
	case VerdictApprove:
		if hasBlocking {
			return errors.New("an approving verdict names a blocking finding: acceptance and an outstanding blocker are different answers")
		}
	case VerdictRequestChanges:
		if !hasBlocking {
			return errors.New("a request-changes verdict names no blocking finding, so remediation would have nothing to be bound to")
		}
	}
	if d.CreatedAt.IsZero() {
		return errors.New("a review decision requires its creation time")
	}
	return nil
}

// StaleAgainst reports whether this decision no longer applies because the PR
// has moved past the exact head it was bound to (#233 acceptance C). It is the
// one place "is this review current" is answered, so a caller never compares
// Subject.HeadSHA against an observed head itself.
func (d Decision) StaleAgainst(currentHeadSHA string) bool {
	return d.Subject.HeadSHA != strings.TrimSpace(currentHeadSHA)
}

// DecisionID is the deterministic identity of one review: the same subject
// reviewed by the same agent always names the same Decision, and a different
// head, repository, PR or reviewer never does. That is what makes review
// creation idempotent - a retried `review pr` after a lost reply, or a second
// reconciliation pass after a crash, finds the decision it already wrote
// instead of risking a second independent verdict for the same exact head.
func DecisionID(subject Subject, reviewerAgentID string) (string, error) {
	if err := subject.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(reviewerAgentID) == "" {
		return "", errors.New("a review identity requires the reviewing agent")
	}
	sum := sha256.Sum256([]byte(subject.Repository + "\x00" + fmt.Sprint(subject.PRNumber) + "\x00" + subject.HeadSHA + "\x00" + reviewerAgentID))
	return "review-" + hex.EncodeToString(sum[:])[:24], nil
}

// Publication is the durable, idempotent record of publishing one Decision to
// GitHub. It is a separate document from Decision on purpose (#233
// requirement 14): a publication failure must never erase or alter the
// durable local decision it failed to publish, so the two can never share one
// row that a failed write could corrupt together.
type Publication struct {
	DecisionID string `json:"decision_id"`
	Published  bool   `json:"published"`
	// GitHubReviewID is the forge's own identity for the review it created,
	// recorded so a retry can distinguish "never attempted" from "succeeded but
	// the reply was lost" without submitting a second review to find out.
	GitHubReviewID int64 `json:"github_review_id,omitempty"`
	// PublishedVerdict is the Verdict that was actually sent to GitHub. It is
	// recorded separately from Decision.Verdict so a COMMENT_ONLY fallback
	// (#233 requirement 13: GitHub refused the configured identity's APPROVE)
	// is visible as what happened, not inferred from the decision alone.
	PublishedVerdict Verdict   `json:"published_verdict,omitempty"`
	PublishedAt      time.Time `json:"published_at,omitempty"`
}
