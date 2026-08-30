package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// maxCanonicalPayloadBytes is the M0 ceiling on one event payload after RFC 8785
// canonicalization. It is what makes "no unbounded raw data in canonical event
// rows" an invariant rather than a naming convention: the deny-list below is
// bypassed by renaming a field, a byte ceiling is not.
//
// 8 KiB is deliberately loose. The largest payload the M0 catalogue produces is
// a RunOperation carrying a lease and a handful of depends_on ids, which
// canonicalizes to a few hundred bytes; the disposition payload is one short
// reason string. That is roughly 10x headroom for every legitimate event, while
// still being far below anything that could hold a provider transcript, a diff,
// or captured command output. Material that does not fit is persisted by the
// artifact store and recorded as an Artifact reference (path + sha256, no body),
// which lives outside the payload and so is never charged against this ceiling.
//
// ponytail: bounds the payload only. The Artifacts slice is bounded per element
// by ValidateArtifact, not in aggregate; a ceiling on the whole canonical event
// document is the upgrade if an event ever accumulates artifact references in a
// loop.
const maxCanonicalPayloadBytes = 8 << 10

// payloadValidator checks one event type's payload. A type absent from the
// registry is reserved: it is in the eventTypes catalogue but has no implemented
// payload schema, and appending one fails closed rather than accepting arbitrary
// JSON that a later reducer would have to guess at.
type payloadValidator func(json.RawMessage) error

var eventPayloads = map[string]payloadValidator{
	// run.created carries the creating controller's provenance (ControllerBuild)
	// when the build is attested. It is optional because an unattested build
	// records no claim, and strict because a recorded claim must be complete.
	EventRunCreated: optionalPayload(payloadSchema(ControllerBuild.validateAttested)),

	EventRunWaiting:   dispositionPayload,
	EventRunCompleted: dispositionPayload,
	EventRunFailed:    dispositionPayload,
	EventRunCancelled: dispositionPayload,

	EventOperationPlanned: operationPayload,
	EventOperationBefore:  operationPayload,
	EventOperationAfter:   operationPayload,

	EventSourceIntentChanged: payloadSchema(func(p SourceIntentChangedPayload) error {
		return errors.Join(
			required("previous_digest", p.PreviousDigest),
			required("current_digest", p.CurrentDigest),
			bounded("reason", p.Reason))
	}),
	EventSourceOptInRemoved:  sourceOptInPayload,
	EventSourceOptInRestored: sourceOptInPayload,
	EventContractCompiled: payloadSchema(func(p ContractCompiledPayload) error {
		return errors.Join(
			requiredRef("contract", p.Contract),
			required("subject.repository", p.Subject.Repository),
			required("subject.revision", p.Subject.Revision))
	}),
	// candidate.changed and assurance.observed predate Phase 8 as artifact-only
	// events, so an absent payload stays legal; a present one is fully checked.
	EventCandidateChanged: optionalPayload(payloadSchema(func(p CandidateChangedPayload) error {
		return errors.Join(
			required("producer_id", p.ProducerID),
			required("purpose", string(p.Purpose)),
			required("outcome", string(p.Outcome)))
	})),
	EventCandidateCommitted: payloadSchema(func(p CandidateCommittedPayload) error {
		return errors.Join(
			required("commit", p.Commit),
			required("tree", p.Tree),
			nonNegative("path_count", p.PathCount),
			required("paths_digest", p.PathsDigest))
	}),
	// A checkpoint carries the same identity a commit does: it IS a real
	// runtime-owned commit, and the difference is what it means, not what it
	// records.
	EventCandidateCheckpointed: payloadSchema(func(p CandidateCommittedPayload) error {
		return errors.Join(
			required("commit", p.Commit),
			required("tree", p.Tree),
			nonNegative("path_count", p.PathCount),
			required("paths_digest", p.PathsDigest))
	}),
	EventExecutionCompleted: payloadSchema(func(p ExecutionCompletedPayload) error {
		return errors.Join(
			required("producer_id", p.ProducerID),
			required("purpose", string(p.Purpose)),
			required("subject_commit", p.SubjectCommit),
			required("subject_tree", p.SubjectTree))
	}),
	EventCandidateBaseIntegrated: payloadSchema(func(p CandidateBaseIntegratedPayload) error {
		if p.Strategy != "rebase" && p.Strategy != "merge" {
			return fmt.Errorf("base integration strategy %q must be rebase or merge", p.Strategy)
		}
		return errors.Join(
			required("base_revision", p.BaseRevision),
			required("commit", p.Commit),
			required("tree", p.Tree))
	}),
	EventCandidateExternalChanged: payloadSchema(func(p CandidateExternalChangedPayload) error {
		return errors.Join(
			required("expected_revision", p.ExpectedRevision),
			required("observed_revision", p.ObservedRevision))
	}),
	EventReassessmentCompleted: payloadSchema(func(p ReassessmentCompletedPayload) error {
		return errors.Join(
			requiredRef("contract", p.Contract),
			boundedList("deviation_kinds", p.DeviationKinds),
			nonNegative("requested_privilege_count", p.RequestedPrivilegeCount))
	}),
	EventAssuranceObserved: optionalPayload(payloadSchema(func(p AssuranceObservedPayload) error {
		return errors.Join(
			required("provider_id", p.ProviderID),
			required("verifier_definition", p.VerifierDefinition),
			bounded("failure_class", string(p.FailureClass)),
			required("commit", p.Commit),
			required("tree", p.Tree),
			optionalRef("bundle", p.Bundle))
	})),
	EventAuthorityEvaluated: payloadSchema(func(p AuthorityEvaluatedPayload) error {
		return errors.Join(
			requiredRef("decision", p.Decision),
			required("action.type", p.Action.Type),
			required("action.target", p.Action.Target),
			required("status", string(p.Status)))
	}),
	EventGitHubPRObserved: payloadSchema(func(p GitHubPRObservedPayload) error {
		if p.Number <= 0 {
			return fmt.Errorf("pull request number %d must be positive", p.Number)
		}
		return errors.Join(
			required("head_revision", p.HeadRevision),
			required("base_revision", p.BaseRevision),
			required("state", p.State))
	}),
	EventGitHubCIObserved: payloadSchema(func(p GitHubCIObservedPayload) error {
		return errors.Join(
			required("head_revision", p.HeadRevision),
			required("conclusion", p.Conclusion),
			nonNegative("check_count", p.CheckCount),
			boundedList("failing_checks", p.FailingChecks))
	}),
	EventGitHubReviewObserved: payloadSchema(func(p GitHubReviewObservedPayload) error {
		return errors.Join(
			required("head_revision", p.HeadRevision),
			required("state", p.State),
			nonNegative("finding_count", p.FindingCount))
	}),
	EventHumanAuthorityRecorded: humanAuthorityPayload,
}

// validateEventPayload enforces the byte ceiling and the per-type schema. It
// runs before the append transaction, so a refused event writes no row.
func validateEventPayload(e EngineeringEvent) error {
	validate, implemented := eventPayloads[e.Type]
	if !implemented {
		if eventTypes[e.Type] {
			return fmt.Errorf("event type %q is reserved: it has no implemented payload schema", e.Type)
		}
		return fmt.Errorf("unknown event type %q", e.Type)
	}
	if len(e.Payload) > 0 {
		// CanonicalJSON is the same canonicalizer the durable row uses, so the
		// measured size is the size that would have been stored. It also refuses
		// duplicate object members and invalid JSON on the way through.
		canonical, err := CanonicalJSON(e.Payload)
		if err != nil {
			return fmt.Errorf("invalid event payload: %w", err)
		}
		if len(canonical) > maxCanonicalPayloadBytes {
			return fmt.Errorf("canonical event payload is %d bytes, above the %d byte ceiling; persist the material as an artifact and record its reference instead",
				len(canonical), maxCanonicalPayloadBytes)
		}
	}
	return validate(e.Payload)
}

// dispositionPayload is what Reduce reads from the run disposition events.
func dispositionPayload(raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	var payload struct {
		Reason string `json:"reason"`
	}
	return strictJSON(raw, &payload)
}

// operationPayload is the RunOperation lifecycle payload Reduce folds into the
// snapshot. Reduce separately checks that the operation's run id matches the run
// and that the event's operation id agrees; this only checks the shape.
func operationPayload(raw json.RawMessage) error {
	if len(raw) == 0 {
		return errors.New("operation lifecycle event requires an operation payload")
	}
	var operation RunOperation
	if err := strictJSON(raw, &operation); err != nil {
		return err
	}
	if operation.ID == "" || operation.RunID == "" {
		return errors.New("operation lifecycle payload requires an operation id and run id")
	}
	return nil
}

// strictJSON decodes exactly one JSON value into target, rejecting unknown
// members and trailing content. This is the domain/json.go strict-decoding
// posture; duplicate members are refused one layer up by the canonical (JCS)
// pass every payload already goes through, so they are not re-checked here.
func strictJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid event payload: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("event payload must contain exactly one JSON value")
	}
	return nil
}

// Phase 8 payload bounds. Every string field and every list in the schemas
// below is bounded here, so no combination of legal fields can approach
// maxCanonicalPayloadBytes: the widest payload is one bounded list plus a
// handful of bounded scalars, roughly 4 KiB worst case against an 8 KiB
// ceiling. The bounds are enforced by the validators, not documented by them.
const (
	maxPayloadFieldBytes    = 200
	maxPayloadListItems     = 16
	maxPayloadListItemBytes = 200
)

// The Phase 8 schemas below carry references and identities only: revisions,
// ids, counts, and short enum-ish status strings. Bulk material (diffs, logs,
// transcripts, review bodies, issue bodies) is an Artifact reference on the
// event, which lives outside the payload.

// SourceIntentChangedPayload records that the pinned source snapshot moved.
// The digests are references to snapshots, never the snapshots themselves.
type SourceIntentChangedPayload struct {
	PreviousDigest string `json:"previous_digest"`
	CurrentDigest  string `json:"current_digest"`
	Reason         string `json:"reason"`
}

// SourceOptInChangedPayload records an opt-in transition on the run's source
// issue: the operator's consent label was removed, or put back. It is identity
// only - the issue number and the operator-configured label - so no untrusted
// issue text ever reaches a durable row, and there is no list to bound.
//
// Both transitions share one schema because they record the same fact about
// the same subject; what happened is the event type, not a payload field.
type SourceOptInChangedPayload struct {
	Issue int    `json:"issue"`
	Label string `json:"label"`
}

var sourceOptInPayload = payloadSchema(func(p SourceOptInChangedPayload) error {
	if p.Issue <= 0 {
		return fmt.Errorf("source issue number %d must be positive", p.Issue)
	}
	return required("label", p.Label)
})

// ContractCompiledPayload records which contract revision now governs which
// exact subject revision.
type ContractCompiledPayload struct {
	Contract Ref            `json:"contract"`
	Subject  domain.Subject `json:"subject"`
}

// CandidateChangedPayload records one producer invocation's outcome. The
// change itself is observed from the candidate repository, not from here.
type CandidateChangedPayload struct {
	ProducerID string            `json:"producer_id"`
	Purpose    InvocationPurpose `json:"purpose"`
	Outcome    OperationState    `json:"outcome"`
}

// CandidateCommittedPayload records the commit/tree identity the runtime
// created. The changed path set is a count plus a digest over it, so a wide
// change cannot grow the payload.
type CandidateCommittedPayload struct {
	Commit      string `json:"commit"`
	Tree        string `json:"tree"`
	PathCount   int    `json:"path_count"`
	PathsDigest string `json:"paths_digest"`
}

// ExecutionCompletedPayload records that the producer finished its invocation
// against an exact subject, rather than being cut off by an iteration, tool
// call, token or time bound. It is the observation that lets the runtime tell a
// finished candidate from preserved partial work; it is not evidence, not a
// verdict, and it authorizes nothing.
type ExecutionCompletedPayload struct {
	ProducerID    string            `json:"producer_id"`
	Purpose       InvocationPurpose `json:"purpose"`
	SubjectCommit string            `json:"subject_commit"`
	SubjectTree   string            `json:"subject_tree"`
}

// CandidateBaseIntegratedPayload records a rebase or merge-from-base and the
// commit/tree it produced. A runtime force-push is not a strategy.
type CandidateBaseIntegratedPayload struct {
	Strategy     string `json:"strategy"`
	BaseRevision string `json:"base_revision"`
	Commit       string `json:"commit"`
	Tree         string `json:"tree"`
}

// CandidateExternalChangedPayload records that the candidate head observed
// externally is not the head the runtime recorded.
type CandidateExternalChangedPayload struct {
	ExpectedRevision string `json:"expected_revision"`
	ObservedRevision string `json:"observed_revision"`
}

// ReassessmentCompletedPayload records a #8 reassessment outcome. Deviation
// details are deliberately dropped: only the bounded kinds are journalled.
type ReassessmentCompletedPayload struct {
	Material                bool     `json:"material"`
	Contract                Ref      `json:"contract"`
	DeviationKinds          []string `json:"deviation_kinds,omitempty"`
	RequestedPrivilegeCount int      `json:"requested_privilege_count"`
}

// AssuranceObservedPayload records one verifier result against the exact
// commit/tree it verified, so a finding belonging to a superseded head can be
// discarded rather than believed. Bundle is the evidence bundle the result was
// recorded into; it is optional because a result can precede a bundle, but an
// id without a revision is not a reference and is refused.
type AssuranceObservedPayload struct {
	ProviderID         string       `json:"provider_id"`
	VerifierDefinition string       `json:"verifier_definition"`
	Passed             bool         `json:"passed"`
	FailureClass       FailureClass `json:"failure_class,omitempty"`
	Commit             string       `json:"commit"`
	Tree               string       `json:"tree"`
	Bundle             Ref          `json:"bundle,omitzero"`
}

// AuthorityEvaluatedPayload records a #7 decision reference for one action.
// The decision's evidence basis stays in the decision, not in the journal.
type AuthorityEvaluatedPayload struct {
	Decision Ref                    `json:"decision"`
	Action   domain.Action          `json:"action"`
	Status   domain.AuthorityStatus `json:"status"`
}

// GitHubPRObservedPayload records the bound pull request's identity as
// observed, including the exact head it currently carries.
type GitHubPRObservedPayload struct {
	Number       int    `json:"number"`
	HeadRevision string `json:"head_revision"`
	BaseRevision string `json:"base_revision"`
	State        string `json:"state"`
	Merged       bool   `json:"merged"`
}

// GitHubCIObservedPayload records a check-suite conclusion for one exact head.
// Failing check names are bounded; check logs are artifacts, never payload.
type GitHubCIObservedPayload struct {
	HeadRevision  string   `json:"head_revision"`
	Conclusion    string   `json:"conclusion"`
	CheckCount    int      `json:"check_count"`
	FailingChecks []string `json:"failing_checks,omitempty"`
}

// GitHubReviewObservedPayload records review state for one exact head. Review
// comment bodies are untrusted text and are never journalled.
type GitHubReviewObservedPayload struct {
	HeadRevision string `json:"head_revision"`
	State        string `json:"state"`
	FindingCount int    `json:"finding_count"`
}

// HumanAuthorityRecordedPayload is the durable evidence that a human
// authorized ONE action against ONE exact binding. There is deliberately no
// free-form approved flag: an approval that is not pinned to a request, a
// candidate revision and tree, a contract revision, and a state digest is not
// evidence of anything, so every one of those is required and none of them can
// be supplied later. Binding returns exactly the frozen HumanAuthorityBinding
// from adapters.go, which is what refuses an approval carried onto a moved
// candidate or contract; this payload is that binding's durable form and does
// not restate its rule.
//
// It carries identities and references only. Nothing here is a credential, and
// nothing here is proof of who a person is - see RecordedOperator in
// operator.go.
type HumanAuthorityRecordedPayload struct {
	SchemaVersion string `json:"schema_version"`
	// EvidenceID is this authority evidence record's id. The journal writes it
	// as the event id, so the evidence can be cited without citing a sequence
	// number that only exists inside one run.
	EvidenceID string `json:"evidence_id"`
	// Request binds the evidence to the exact AuthorityRequest that was shown
	// to the human: ID is that request's opaque identifier and Revision is its
	// binding digest. An id without the digest would name a request whose
	// content could since have changed, so both are required. The request type
	// itself is deliberately not imported: this is a bounded reference, not a
	// copy of another component's structure.
	Request  Ref              `json:"request"`
	Operator RecordedOperator `json:"operator"`
	// Decision is the recorded outcome and is closed to the same two values
	// HumanAuthorityBinding accepts. It is not a standalone permission flag: it
	// is inert without the request, candidate, contract, and state bindings
	// above and below it, all of which are required and all of which are
	// revalidated against live run state before anything acts on it.
	Decision   string        `json:"decision"`
	Action     domain.Action `json:"action"`
	Repository string        `json:"repository"`
	Controller Ref           `json:"controller"`
	Candidate  Candidate     `json:"candidate"`
	Contract   Ref           `json:"contract"`
	// StateSHA256 is the run state digest the human was shown. It identifies the
	// subject the authorization was given against.
	StateSHA256 string `json:"state_sha256"`
	// OccurredAt is the operator boundary clock reading at the moment authority
	// was recorded. It is not the journal's append time, which the event header
	// records separately.
	OccurredAt time.Time `json:"occurred_at"`
	// Note is an OPTIONAL, UNTRUSTED operator annotation. It is bounded by the
	// same field bound as every other string here, and it is an input to
	// nothing: Binding ignores it, so no permission decision can be made to
	// depend on its text.
	Note string `json:"note,omitempty"`
}

var humanAuthorityPayload = payloadSchema(func(p HumanAuthorityRecordedPayload) error {
	if p.SchemaVersion != SchemaVersion {
		return fmt.Errorf("human authority schema_version %q must be %q", p.SchemaVersion, SchemaVersion)
	}
	if p.Decision != "approve" && p.Decision != "reject" {
		return fmt.Errorf("human authority decision %q must be approve or reject", p.Decision)
	}
	if p.Operator.Provenance != ProvenanceLocalUnverified {
		return fmt.Errorf("recorded operator provenance %q is not a provenance this milestone can record", p.Operator.Provenance)
	}
	if p.OccurredAt.IsZero() {
		return errors.New(`payload field "occurred_at" is required`)
	}
	return errors.Join(
		required("evidence_id", p.EvidenceID),
		requiredRef("request", p.Request),
		required("operator.id", p.Operator.ID),
		bounded("operator.account_name", p.Operator.AccountName),
		bounded("operator.host", p.Operator.Host),
		required("action.type", p.Action.Type),
		required("action.target", p.Action.Target),
		required("repository", p.Repository),
		requiredRef("controller", p.Controller),
		required("candidate.revision", p.Candidate.Revision),
		required("candidate.tree", p.Candidate.Tree),
		bounded("candidate.branch", p.Candidate.Branch),
		requiredRef("contract", p.Contract),
		required("state_sha256", p.StateSHA256),
		bounded("note", p.Note))
})

// Binding is the frozen HumanAuthorityBinding this evidence records, so a
// recorded authority is revalidated against live run state by the one rule that
// already exists rather than by a second copy of it. The run id comes from the
// event that carries the payload, not from the payload, so evidence cannot
// claim to belong to a run it was not journalled into.
//
// The note is not an input here. That is the whole of "the annotation has no
// part in permission semantics", stated as code.
func (p HumanAuthorityRecordedPayload) Binding(runID string) HumanAuthorityBinding {
	return HumanAuthorityBinding{
		RunID:             runID,
		CandidateRevision: p.Candidate.Revision,
		CandidateTree:     p.Candidate.Tree,
		Contract:          p.Contract,
		Action:            p.Action.Type,
		Decision:          p.Decision,
		HumanID:           p.Operator.ID,
	}
}

// payloadSchema decodes a payload strictly into T and applies its field checks.
// It exists so twelve schemas do not repeat the same decode boilerplate.
func payloadSchema[T any](check func(T) error) payloadValidator {
	return func(raw json.RawMessage) error {
		var payload T
		if err := strictJSON(raw, &payload); err != nil {
			return err
		}
		return check(payload)
	}
}

// optionalPayload keeps a type's pre-Phase-8 artifact-only form appendable: an
// absent payload stays legal, a present one must satisfy the full schema.
func optionalPayload(v payloadValidator) payloadValidator {
	return func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		return v(raw)
	}
}

func required(name, value string) error {
	if value == "" {
		return fmt.Errorf("payload field %q is required", name)
	}
	return bounded(name, value)
}

func bounded(name, value string) error {
	if len(value) > maxPayloadFieldBytes {
		return fmt.Errorf("payload field %q is %d bytes, above the %d byte field bound", name, len(value), maxPayloadFieldBytes)
	}
	return nil
}

// boundedList is what keeps a list field from growing toward the ceiling: both
// the element count and each element's length are refused, not documented.
func boundedList(name string, values []string) error {
	if len(values) > maxPayloadListItems {
		return fmt.Errorf("payload list %q has %d elements, above the %d element bound", name, len(values), maxPayloadListItems)
	}
	for _, value := range values {
		if len(value) > maxPayloadListItemBytes {
			return fmt.Errorf("payload list %q has a %d byte element, above the %d byte element bound", name, len(value), maxPayloadListItemBytes)
		}
	}
	return nil
}

func nonNegative(name string, n int) error {
	if n < 0 {
		return fmt.Errorf("payload field %q must not be negative", name)
	}
	return nil
}

// requiredRef and optionalRef keep a reference whole: an id without a revision
// does not identify an exact object revision and is not accepted as one.
func requiredRef(name string, ref Ref) error {
	return errors.Join(required(name+".id", ref.ID), required(name+".revision", ref.Revision))
}

func optionalRef(name string, ref Ref) error {
	if ref == (Ref{}) {
		return nil
	}
	return requiredRef(name, ref)
}
