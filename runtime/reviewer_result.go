package runtime

// The reviewer result protocol.
//
// A reviewer stage's verdict is authority-bearing lifecycle state: it settles
// the stage, it gates the assurance below it, and a BLOCK sends the producer
// back to work with findings bound to it. So it may not be INFERRED. A
// transcript that says "looks good" is a model reasoning out loud, and a
// transcript that says "blocking issue" is the same; neither is a machine
// authorizing anything. LLMs reason, machines authorize, and this file is where
// that boundary is drawn for reviews.
//
// The protocol is deliberately the smallest thing that can carry a verdict:
//
//	ReviewerResult { schema_version, verdict, findings, reason }
//
// It does NOT carry the candidate. The runtime already knows which exact
// CandidateRef it materialized for this invocation, and asking the reviewer to
// restate it would create a second answer to a question that already has one -
// a claim to be checked rather than a fact to be used. The optional
// candidate/tree members exist only so a reviewer that DOES state them can be
// caught contradicting the runtime; they are never the source of the binding.
//
// The channel is a runtime-owned file OUTSIDE the candidate workspace, named by
// the exact attempt identity. That placement is the security property:
//
//   - repository content cannot pre-seed it, because it is not in the
//     repository and its path is not predictable from repository content;
//   - a previous attempt's result cannot be reused, because the path carries
//     this attempt's identity and the runtime removes any leftover before the
//     invocation starts;
//   - transcript prose cannot become a verdict, because the transcript is never
//     read for one - stdout is evidence, this file is the protocol.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ReviewerResultSchemaVersion is the protocol version a reviewer must state.
// An unrecognized version is refused rather than interpreted: a result this
// build does not understand is not a verdict it may act on.
const ReviewerResultSchemaVersion = "0.1"

// reviewerResultFile is the basename of the runtime-owned result artifact.
const reviewerResultFile = "reviewer-result.json"

// maxReviewerResultBytes bounds the file the runtime will read. A reviewer that
// writes more than this is not producing a verdict, and an unbounded read of a
// worker-written file is a denial-of-service surface.
//
// ponytail: a flat ceiling. Per-finding bounds already apply below it.
const maxReviewerResultBytes = 64 << 10

// maxReviewerFindings bounds how many findings one verdict may carry.
//
// It IS the durable payload list bound, not a second number beside it. Two
// independently chosen limits drift, and this pair drifted immediately: an
// admission ceiling of 32 accepted verdicts the durable payload validator then
// refused at 16, so a reviewer naming 17 real defects had its result admitted,
// its append rejected, its invocation failed, and its whole remediation budget
// spent re-producing the same refusal before the stage died with an opaque
// reason. A result admission accepts must be a result the journal can hold.
const maxReviewerFindings = maxPayloadListItems

// ReviewerResult is what a reviewer stage emits through the structured channel.
type ReviewerResult struct {
	SchemaVersion string `json:"schema_version"`
	// Verdict is StageReviewAccepted or StageReviewBlocked. There is no third
	// value and no default: an absent verdict is an absent result.
	Verdict string `json:"verdict"`
	// Candidate and Tree are OPTIONAL restatements of what was reviewed. The
	// runtime binds the result to the candidate it materialized; these exist so
	// a reviewer that believes it reviewed something else is refused rather
	// than silently recorded against the runtime's answer.
	Candidate string `json:"candidate,omitempty"`
	Tree      string `json:"tree,omitempty"`
	// Findings are what a BLOCK blocks on. They become typed Findings on the
	// producer's next invocation, so they are bounded signatures rather than
	// prose: the runtime's own record of why an invocation is happening must
	// not be writable by the party being reviewed.
	Findings []ReviewerFinding `json:"findings,omitempty"`
	// Reason is a bounded human-facing summary. It is recorded and shown; it is
	// never parsed and never decides anything.
	Reason string `json:"reason,omitempty"`
}

// ReviewerFinding is one bounded defect a blocking verdict names.
type ReviewerFinding struct {
	// Signature identifies the finding stably enough that the same defect twice
	// is the same finding. It is bounded and never becomes an instruction.
	Signature string `json:"signature"`
	// Detail is optional bounded context for an operator.
	Detail string `json:"detail,omitempty"`
}

// ReviewerResultPath is the runtime-owned location for ONE invocation's result.
//
// It lives beside the attempt's transcripts, under the runtime's state
// directory, and never inside the candidate workspace. A reviewer is told this
// path; nothing else can derive it from repository content.
func ReviewerResultPath(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	if err := attempt.Validate(); err != nil {
		return "", err
	}
	prefix, err := attemptTranscriptPrefix("review", attempt)
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "artifacts", prefix+"."+reviewerResultFile), nil
}

// PrepareReviewerResult clears any leftover result and returns the path this
// invocation must write to.
//
// Clearing is not housekeeping. A result file surviving from an earlier attempt
// of the same operation would be admitted as THIS attempt's verdict, which
// would let a reviewer that produced nothing inherit the answer of one that
// did. The runtime owns the slot, so the runtime empties it.
func PrepareReviewerResult(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	path, err := ReviewerResultPath(stateDir, attempt)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", err
	}
	return path, nil
}

// ReadReviewerResult reads and decodes the result a reviewer wrote.
//
// An absent file is not an error and not a verdict: it is (nil, nil), and the
// reviewer stage stays unsettled. That is the case a reviewer producing only
// prose falls into, and leaving it unsettled rather than guessing is the whole
// point of the protocol.
func ReadReviewerResult(path string) (*ReviewerResult, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("reviewer result at %s is a directory", path)
	}
	if info.Size() > maxReviewerResultBytes {
		return nil, fmt.Errorf("reviewer result is %d bytes, above the %d byte bound", info.Size(), maxReviewerResultBytes)
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// STRICT decoding, the way every other governed document in this system is
	// decoded: exactly one JSON value and no unknown member. A result carrying
	// a member this build does not know is a result from a protocol this build
	// does not implement.
	decoder := json.NewDecoder(strings.NewReader(string(document)))
	decoder.DisallowUnknownFields()
	var result ReviewerResult
	if err := decoder.Decode(&result); err != nil {
		return nil, fmt.Errorf("reviewer result is not a valid %s document: %w", reviewerResultFile, err)
	}
	if decoder.More() {
		return nil, fmt.Errorf("reviewer result carries more than one JSON value")
	}
	return &result, nil
}

// ReviewerResultRefusedError is the typed refusal for a result that exists and
// may not become lifecycle state. It is distinct from an absent result: nothing
// was claimed there, and something invalid was claimed here.
type ReviewerResultRefusedError struct {
	StageID string
	Detail  string
}

func (e *ReviewerResultRefusedError) Error() string {
	if e.StageID == "" {
		return "reviewer result refused: " + e.Detail
	}
	return "reviewer result for stage " + e.StageID + " refused: " + e.Detail
}

// AdmitReviewerResult decides whether one structured result may become a
// durable verdict, and returns the verdict it admits.
//
// Every check here is about AUTHORITY rather than about the review's opinion.
// The runtime is not judging whether the reviewer was right; it is establishing
// that this result came from the worker the operator approved, for the stage
// that was assigned, about the candidate that was actually materialized, and
// that it says one thing rather than two.
//
// The candidate binding is taken from the frozen assignment, never from the
// result. A reviewer that restates a different candidate is refused - not
// corrected - because two answers to "what did you review" is exactly the
// ambiguity a verdict must not carry.
func AdmitReviewerResult(
	stage domain.PlanStage,
	assignment domain.AgentAssignment,
	binding *RunPlanBinding,
	subject domain.UpstreamOutput,
	reviewerRunID string,
	providerID string,
	result *ReviewerResult,
) (PlanStageReviewedPayload, error) {
	refuse := func(format string, args ...any) (PlanStageReviewedPayload, error) {
		return PlanStageReviewedPayload{}, &ReviewerResultRefusedError{StageID: stage.ID, Detail: fmt.Sprintf(format, args...)}
	}
	if result == nil {
		return PlanStageReviewedPayload{}, nil
	}
	// 1. The run is bound to a plan revision at all.
	if binding == nil || binding.PlanID == "" || binding.StageID == "" {
		return refuse("the run is not bound to a plan stage, so nothing authorized a verdict")
	}
	// 2. The stage is a VERDICT-PRODUCING stage. An implementer that writes a
	// perfectly formed result gets nothing: the role is what carries the
	// authority, and printing matching JSON is not a way to acquire one.
	if stage.Kind != domain.StageAgent || stage.Role != domain.RoleReviewer {
		return refuse("stage role %q does not produce review verdicts", stage.Role)
	}
	// 3-4. The frozen assignment is THIS stage's, and the worker that produced
	// the result is the exact reviewer the operator approved.
	if assignment.StageID != stage.ID || assignment.ID != binding.AssignmentID {
		return refuse("the frozen assignment is %q for stage %q and the run was created under %q for stage %q",
			assignment.ID, assignment.StageID, binding.AssignmentID, stage.ID)
	}
	if providerID != "" && assignment.Agent.ID != providerID {
		return refuse("the approved reviewer is %q and the result was produced by %q", assignment.Agent.ID, providerID)
	}
	// 5. Independence still holds. A reviewer that has become the producer's
	// own worker is not an independent verdict, whatever it says.
	for _, independence := range assignment.Independence {
		for _, class := range independence.OtherClasses {
			if class != "" && class == independence.Class {
				return refuse("independence in dimension %q no longer holds: the reviewer and a producer share class %q",
					independence.Dimension, class)
			}
		}
	}
	// 6-9. The EXACT candidate. The subject comes from the assignment, so this
	// is the runtime's own answer; a restatement that contradicts it is
	// refused rather than reconciled.
	if subject.Candidate == "" || subject.Tree == "" {
		return refuse("the assignment froze no exact upstream candidate for this stage to have reviewed")
	}
	if result.Candidate != "" && result.Candidate != subject.Candidate {
		return refuse("the result claims candidate %s and the invocation materialized %s",
			short12(result.Candidate), short12(subject.Candidate))
	}
	if result.Tree != "" && result.Tree != subject.Tree {
		return refuse("the result claims tree %s and the invocation materialized %s",
			short12(result.Tree), short12(subject.Tree))
	}
	// 12. A recognized protocol version.
	if result.SchemaVersion != ReviewerResultSchemaVersion {
		return refuse("result schema version %q is not %q", result.SchemaVersion, ReviewerResultSchemaVersion)
	}
	// 10-11. One answer, and an answer that can be acted on.
	findings, err := boundedReviewerFindings(result.Findings)
	if err != nil {
		return refuse("%s", err.Error())
	}
	switch result.Verdict {
	case StageReviewAccepted:
		// ACCEPT AND BLOCKING FINDINGS ARE TWO ANSWERS. A downstream gate reads
		// the verdict, so accepting while naming defects would let the more
		// permissive half decide.
		if len(findings) > 0 {
			return refuse("an accepting verdict names %d finding(s): acceptance and outstanding findings are different answers", len(findings))
		}
	case StageReviewBlocked:
		// A BLOCK MUST BE ACTIONABLE. Remediation is bound to the finding set,
		// so a block naming nothing would plan the same invocation forever.
		if len(findings) == 0 {
			return refuse("a blocking verdict names no finding, so remediation would have nothing to be bound to")
		}
	default:
		return refuse("verdict %q must be %q or %q", result.Verdict, StageReviewAccepted, StageReviewBlocked)
	}
	return PlanStageReviewedPayload{
		StageID: stage.ID, RunID: reviewerRunID,
		UpstreamStageID: subject.StageID, UpstreamRunID: subject.RunID,
		Candidate: subject.Candidate, Tree: subject.Tree,
		Verdict: result.Verdict, Reason: boundedDetail(result.Reason), Findings: findings,
	}, nil
}

// boundedReviewerFindings turns reviewer-authored findings into the bounded
// signatures a durable payload may carry, refusing a set that cannot be one.
func boundedReviewerFindings(findings []ReviewerFinding) ([]string, error) {
	if len(findings) > maxReviewerFindings {
		return nil, fmt.Errorf("the result names %d findings, above the %d finding bound", len(findings), maxReviewerFindings)
	}
	out := make([]string, 0, len(findings))
	for _, finding := range findings {
		signature := strings.TrimSpace(finding.Signature)
		if signature == "" {
			return nil, fmt.Errorf("a finding states no signature, so it identifies nothing to remediate")
		}
		if detail := strings.TrimSpace(finding.Detail); detail != "" {
			signature += ": " + detail
		}
		out = append(out, boundedDetail(signature))
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
