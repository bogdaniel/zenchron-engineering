package runtime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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

	EventFeedbackPublicationIdentity: payloadSchema(FeedbackPublicationIdentityPayload.validate),

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
	EventSemanticAssuranceObserved: payloadSchema(func(p AssuranceObservedPayload) error {
		return errors.Join(
			required("provider_id", p.ProviderID),
			required("verifier_definition", p.VerifierDefinition),
			required("commit", p.Commit),
			required("tree", p.Tree))
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

	EventRunAgentAssigned: payloadSchema(func(p AgentAssignedPayload) error {
		if _, known := agentKinds[p.ProviderKind]; !known {
			return fmt.Errorf("agent provider kind %q is not a configurable kind", p.ProviderKind)
		}
		if p.TrustMode != agentKinds[p.ProviderKind] {
			return fmt.Errorf("agent trust mode %q is not the trust mode of kind %q", p.TrustMode, p.ProviderKind)
		}
		return errors.Join(required("agent_id", p.AgentID), bounded("model", p.Model))
	}),
	// A handoff record is identity, counts and digests. The schema checks the
	// members a reader would have to trust: which run, which two agents, and a
	// stated reason - an unexplained provider transition is the thing this
	// event exists to make impossible.
	EventRunAgentHandoffRefused: payloadSchema(func(p AgentHandoffRecord) error {
		return errors.Join(
			required("run_id", p.RunID),
			required("to.agent_id", p.To.AgentID),
			bounded("reason", p.Reason),
			required("refusal_code", string(p.RefusalCode)),
			nonNegative("evidence_bundles", p.EvidenceBundles),
			nonNegative("feedback_consumed", p.FeedbackConsumed))
	}),
	EventFeedbackObserved: payloadSchema(func(p FeedbackObservedPayload) error {
		return errors.Join(
			required("key", p.Key),
			required("class", string(p.Class)),
			required("reason", p.Reason),
			required("text_digest", p.TextDigest),
			bounded("actor", p.Actor))
	}),
	EventFeedbackConsumed: payloadSchema(func(p FeedbackConsumedPayload) error {
		// A record may carry ONLY unavailable keys: when every pending item's
		// text artifact has been reclaimed, nothing was delivered and the
		// record exists to drain the pending set. Requiring Keys made that
		// case unwritable - AppendEvent failed after the provider had already
		// been invoked, and every later pass re-derived the same set, re-ran
		// the provider, and died at the same append.
		if len(p.Keys) == 0 && len(p.Unavailable) == 0 {
			return fmt.Errorf("a consumption record with no keys accounts for nothing")
		}
		if len(p.Unavailable) > maxFeedbackKeysPerEvent {
			return fmt.Errorf("a consumption record carries %d unavailable keys, more than the bound of %d", len(p.Unavailable), maxFeedbackKeysPerEvent)
		}
		if len(p.Keys) > maxFeedbackKeysPerEvent {
			return fmt.Errorf("a consumption record carries %d keys, more than the bound of %d", len(p.Keys), maxFeedbackKeysPerEvent)
		}
		if p.Attempt < 1 {
			return fmt.Errorf("feedback delivery must name the attempt that received it, got %d", p.Attempt)
		}
		return errors.Join(required("operation_id", p.OperationID),
			boundedList("keys", p.Keys), boundedList("unavailable", p.Unavailable))
	}),
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
	// Semantic marks an observation produced by the independent semantic
	// acceptance verifier rather than the automated one, and ClaimResults is its
	// per-claim answer. A semantic verdict is claim-specific: one acceptance
	// obligation may be discharged while another is not.
	Semantic     bool              `json:"semantic,omitempty"`
	ClaimResults map[string]string `json:"claim_results,omitempty"`
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
	// Requires is what the request the person answered was ABOUT: the exact
	// outstanding human_approval claim ids. It is recorded because a decision
	// is evidence of answering a QUESTION, and a plan gate that states claims
	// has to be able to tell whether those are the claims that were answered -
	// authorizing a publication is not the same act as attesting that a change
	// was independently reviewed.
	Requires []string `json:"requires,omitempty"`
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
		boundedList("requires", p.Requires),
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

// ---------------------------------------------------------------------------
// Plan lifecycle payloads
// ---------------------------------------------------------------------------

// The plan payloads are identities, counts, digests and short enum-ish status
// strings, exactly like the run payloads above. An objective is the one piece
// of human-authored text among them, and it is bounded like every other field:
// a plan's full content lives in the immutable plan_revisions row, which the
// digest here references, so the journal never becomes a second copy of it.

// PlanProposedPayload records a plan revision offered for approval.
type PlanProposedPayload struct {
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
	// ObjectiveDigest is the SHA-256 of the plan's objective, not the objective
	// itself. The objective is derived from an issue title and body, which are
	// UNTRUSTED third-party text: the journal carries no such text anywhere, so
	// it carries an identity here and the readable objective lives in the plan
	// revision document the operator reads.
	ObjectiveDigest string `json:"objective_digest"`
	// StageCount and AgentStageCount are what an approval view needs before it
	// loads the revision document, and they are also the two numbers that make
	// "gates create no runs" checkable from the journal alone.
	StageCount      int `json:"stage_count"`
	AgentStageCount int `json:"agent_stage_count"`
	// Template is the exact template revision this plan was compiled from, or
	// absent when the operator supplied none. A later template edit cannot
	// change what this says.
	Template *PlanTemplateRef `json:"template,omitempty"`
	// Budget is the aggregate envelope the revision proposes.
	Budget PlanBudgetPayload `json:"budget"`
	// Reasoning is present when a registered execution agent contributed
	// semantic reasoning, and carries the runtime's OWN verification that the
	// planning workspace did not change.
	Reasoning *PlanReasoningPayload `json:"reasoning,omitempty"`
	// ProposalID is the PlanRevisionProposal this revision came from, absent
	// for an initial plan.
	ProposalID string `json:"proposal_id,omitempty"`
	Origin     string `json:"origin"`
}

// PlanTemplateRef pins a template revision by digest.
type PlanTemplateRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
	Digest  string `json:"digest"`
}

// PlanBudgetPayload is the aggregate envelope. MaxCostMicros is a POINTER
// because most configurations cannot report cost at all: absent means unknown,
// and unknown is never rendered as zero.
type PlanBudgetPayload struct {
	MaxChildRuns           int    `json:"max_child_runs"`
	MaxConcurrency         int    `json:"max_concurrency"`
	MaxProviderInvocations int    `json:"max_provider_invocations"`
	MaxWallSeconds         int    `json:"max_wall_seconds,omitempty"`
	MaxCostMicros          *int64 `json:"max_cost_micros,omitempty"`
}

// PlanReasoningPayload is the planner invocation's provenance, including the
// proof that it could not write. The two workspace digests are the runtime's
// own measurement, taken before and after the invocation; WorkspaceUnchanged is
// the conclusion, recorded so a reader never has to compare two hashes to learn
// the answer.
type PlanReasoningPayload struct {
	AgentID               string `json:"agent_id"`
	ProviderKind          string `json:"provider_kind"`
	VendorFamily          string `json:"vendor_family,omitempty"`
	TrustMode             string `json:"trust_mode"`
	Model                 string `json:"model,omitempty"`
	ProfileID             string `json:"profile_id,omitempty"`
	InvocationMode        string `json:"invocation_mode"`
	ProviderMode          string `json:"provider_mode,omitempty"`
	WorkspaceDigestBefore string `json:"workspace_digest_before"`
	WorkspaceDigestAfter  string `json:"workspace_digest_after"`
	WorkspaceUnchanged    bool   `json:"workspace_unchanged"`
}

// PlanValidatedPayload records the deterministic validator's verdict. Errors
// are bounded runtime-authored statements, never provider text.
type PlanValidatedPayload struct {
	Revision int `json:"revision"`
	// Digest is absent for a REFUSED proposal, because a proposal that failed
	// to compile produced no document to digest. Requiring it here refused the
	// refusal record itself, so the one thing this event exists to preserve -
	// why a proposal was turned down - was replaced by a payload error.
	Digest string   `json:"digest,omitempty"`
	Status string   `json:"status"`
	Errors []string `json:"errors,omitempty"`
}

// PlanDecisionPayload is the operator's approval or rejection of one exact
// revision. Both the revision number and its digest are required: approving a
// revision number whose content could since have changed would be approving
// something nobody looked at.
type PlanDecisionPayload struct {
	Revision int    `json:"revision"`
	Digest   string `json:"digest"`
	Operator string `json:"operator"`
	Note     string `json:"note,omitempty"`
}

// PlanStageAssignedPayload freezes which configuration performed a stage. The
// profile digest is what makes a later profile edit unable to rewrite work
// already assigned.
type PlanStageAssignedPayload struct {
	StageID        string `json:"stage_id"`
	AssignmentID   string `json:"assignment_id"`
	Role           string `json:"role"`
	ProfileID      string `json:"profile_id"`
	ProfileVersion int    `json:"profile_version"`
	ProfileDigest  string `json:"profile_digest"`
	AgentID        string `json:"agent_id"`
	ProviderKind   string `json:"provider_kind"`
	VendorFamily   string `json:"vendor_family"`
	TrustMode      string `json:"trust_mode"`
	InvocationMode string `json:"invocation_mode"`
	RunID          string `json:"run_id,omitempty"`
}

// PlanRunStartedPayload associates one agent stage with one ordinary
// EngineeringRun. It is the ONLY link between the two streams, and it exists
// only for agent stages: a gate that produced one of these would be a fake
// worker run.
type PlanRunStartedPayload struct {
	StageID string `json:"stage_id"`
	RunID   string `json:"run_id"`
}

// PlanStageSettledPayload records how a stage ended.
type PlanStageSettledPayload struct {
	StageID string `json:"stage_id"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason,omitempty"`
	// Revision is set on an INVALIDATION that happens under the revision the
	// stage was performed under, rather than because a new revision replaced
	// it. The two are different situations and the machinery around them is
	// revision-scoped: a stage's run identity carries the revision, so a
	// supersession-invalidated stage genuinely starts again as a new run, and
	// a same-revision one would adopt the run whose work was just discarded.
	//
	// omitempty: an event written before this existed carries none, and none
	// means the supersession case, which is what every such event was.
	Revision int `json:"revision,omitempty"`
}

// PlanGateSatisfiedPayload records that a typed gate's EXISTING durable
// references now prove it. The references are the evidence, authority and human
// decision records the kernel already owns; the plan stores no second copy of
// any of them, which is what keeps a gate from becoming a parallel evidence
// model.
type PlanGateSatisfiedPayload struct {
	StageID string   `json:"stage_id"`
	Kind    string   `json:"kind"`
	Claims  []string `json:"claims,omitempty"`
	// Evidence, Decision and HumanEvidenceID are references. At least one is
	// required: a gate satisfied by nothing is a gate that proved nothing.
	Evidence        Ref    `json:"evidence,omitzero"`
	Decision        Ref    `json:"decision,omitzero"`
	HumanEvidenceID string `json:"human_evidence_id,omitempty"`
	// ProvingRuns is every upstream run that carried a verdict this gate was
	// judged on. Evidence above names one bundle, and a gate over two producers
	// is proved by both - recording only the last one left the record thinner
	// than the check that produced it.
	ProvingRuns []string `json:"proving_runs,omitempty"`
	// ProvenHeads is the exact candidate each proving run carried when it
	// proved this gate, as "<run>@<head>". A gate is a statement about a
	// CHANGE, and a goal-state run is not finished: it can be re-activated by
	// reviewer feedback and produce a different candidate. Without the head,
	// the gate's verdict silently transfers to work nobody judged.
	ProvenHeads []string `json:"proven_heads,omitempty"`
}

// PlanBudgetConsumedPayload is a DELTA, never a total. Totals are projected by
// summing these, so nothing a revision or a restart does can lower one.
//
// CostMicros is a pointer and CostKnown is separate from it because "not
// reported" and "zero" are opposite facts: a subscription CLI reports no cost,
// and rendering that as zero would fabricate a currency figure.
type PlanBudgetConsumedPayload struct {
	// Key identifies WHAT was consumed, so the same consumption recorded twice
	// counts once. The reconciler is idempotent by design - it re-derives the
	// same run every tick - and a crash between two appends made it re-record
	// a delta it had already recorded, permanently inflating the plan's
	// consumption against its ceiling.
	//
	// It is omitempty because an event written before it existed has none, and
	// those are counted exactly as they were: an absent key means "count this",
	// which is what the projection did for every event until now.
	Key                 string `json:"key,omitempty"`
	StageID             string `json:"stage_id,omitempty"`
	RunID               string `json:"run_id,omitempty"`
	ChildRuns           int    `json:"child_runs,omitempty"`
	ProviderInvocations int    `json:"provider_invocations,omitempty"`
	WallSeconds         int    `json:"wall_seconds,omitempty"`
	CostMicros          *int64 `json:"cost_micros,omitempty"`
	CostKnown           bool   `json:"cost_known,omitempty"`
}

// PlanRevisionSupersededPayload records one revision replacing another and
// exactly which downstream stages that invalidated. Only AFFECTED stages
// appear: invalidating an unrelated stage to be safe would discard valid work.
type PlanRevisionSupersededPayload struct {
	FromRevision int `json:"from_revision"`
	ToRevision   int `json:"to_revision"`
	// ProposalID is absent for a revision an OPERATOR made directly. Only a
	// revision that came from a PlanRevisionProposal has one, and requiring it
	// would refuse to record the supersession of an ordinary operator edit -
	// losing the fact that one approved revision replaced another.
	ProposalID        string   `json:"proposal_id,omitempty"`
	InvalidatedStages []string `json:"invalidated_stages,omitempty"`
}

// planPayloads registers the plan schemas. It is a separate map merged into
// eventPayloads by init so the run catalogue above stays readable; the
// validation path is unchanged and there is still exactly one registry.
var planPayloads = map[string]payloadValidator{
	EventPlanProposed: payloadSchema(func(p PlanProposedPayload) error {
		if p.Revision < 1 {
			return fmt.Errorf("plan revision %d must be positive", p.Revision)
		}
		if p.StageCount < 1 {
			return fmt.Errorf("a proposed plan carries %d stages", p.StageCount)
		}
		if p.AgentStageCount > p.StageCount {
			return fmt.Errorf("plan reports %d agent stages of %d stages", p.AgentStageCount, p.StageCount)
		}
		if err := errors.Join(
			required("digest", p.Digest),
			required("objective_digest", p.ObjectiveDigest),
			knownOrigin(p.Origin),
			bounded("proposal_id", p.ProposalID),
			validatePlanBudget(p.Budget),
		); err != nil {
			return err
		}
		if p.Template != nil {
			if p.Template.Version < 1 {
				return fmt.Errorf("template version %d must be positive", p.Template.Version)
			}
			if err := errors.Join(required("template.id", p.Template.ID), required("template.digest", p.Template.Digest)); err != nil {
				return err
			}
		}
		return validatePlanReasoning(p.Reasoning)
	}),
	EventPlanValidated: payloadSchema(func(p PlanValidatedPayload) error {
		if p.Status != string(domain.ProposalValid) && p.Status != string(domain.ProposalRefused) {
			return fmt.Errorf("plan validation status %q must be %q or %q", p.Status, domain.ProposalValid, domain.ProposalRefused)
		}
		if p.Status == string(domain.ProposalRefused) && len(p.Errors) == 0 {
			return errors.New("a refused validation records no reason")
		}
		if p.Revision < 1 {
			return fmt.Errorf("plan revision %d must be positive", p.Revision)
		}
		if p.Status == string(domain.ProposalValid) {
			return errors.Join(required("digest", p.Digest), boundedList("errors", p.Errors))
		}
		return errors.Join(bounded("digest", p.Digest), boundedList("errors", p.Errors))
	}),
	EventPlanApproved: planDecisionPayload,
	EventPlanRejected: planDecisionPayload,
	EventPlanStageAssigned: payloadSchema(func(p PlanStageAssignedPayload) error {
		if p.ProfileVersion < 1 {
			return fmt.Errorf("profile version %d must be positive", p.ProfileVersion)
		}
		if p.InvocationMode != string(domain.InvocationModeMutating) && p.InvocationMode != string(domain.InvocationModeNonMutatingPlanning) {
			return fmt.Errorf("invocation mode %q is not a mode", p.InvocationMode)
		}
		if !domain.KnownRole(domain.EngineeringRole(p.Role)) {
			return fmt.Errorf("role %q is not in the role catalogue", p.Role)
		}
		return errors.Join(
			required("stage_id", p.StageID),
			required("assignment_id", p.AssignmentID),
			required("profile_id", p.ProfileID),
			required("profile_digest", p.ProfileDigest),
			required("agent_id", p.AgentID),
			required("provider_kind", p.ProviderKind),
			required("vendor_family", p.VendorFamily),
			required("trust_mode", p.TrustMode),
			bounded("run_id", p.RunID))
	}),
	EventPlanRunStarted: payloadSchema(func(p PlanRunStartedPayload) error {
		return errors.Join(required("stage_id", p.StageID), required("run_id", p.RunID))
	}),
	EventPlanStageSettled: payloadSchema(func(p PlanStageSettledPayload) error {
		switch p.Outcome {
		case string(Completed), string(Failed), planStageInvalidated:
		default:
			return fmt.Errorf("stage outcome %q must be %q, %q or %q", p.Outcome, Completed, Failed, planStageInvalidated)
		}
		return errors.Join(required("stage_id", p.StageID), bounded("reason", p.Reason))
	}),
	EventPlanGateSatisfied: payloadSchema(func(p PlanGateSatisfiedPayload) error {
		if p.Kind != string(domain.StageAssuranceGate) && p.Kind != string(domain.StageHumanDecisionGate) {
			return fmt.Errorf("gate kind %q must be %q or %q", p.Kind, domain.StageAssuranceGate, domain.StageHumanDecisionGate)
		}
		// A gate is satisfied by an existing durable record, so at least one
		// reference is required. Without this a gate could be journalled as
		// satisfied by nothing at all, which is the fabricated-evidence path
		// the typed gates exist to close.
		if p.Evidence == (Ref{}) && p.Decision == (Ref{}) && p.HumanEvidenceID == "" {
			return errors.New("a satisfied gate must reference the evidence, authority decision or human decision that proves it")
		}
		return errors.Join(
			required("stage_id", p.StageID),
			boundedList("claims", p.Claims),
			optionalRef("evidence", p.Evidence),
			optionalRef("decision", p.Decision),
			bounded("human_evidence_id", p.HumanEvidenceID),
			boundedList("proving_runs", p.ProvingRuns),
			boundedList("proven_heads", p.ProvenHeads))
	}),
	EventPlanBudgetConsumed: payloadSchema(func(p PlanBudgetConsumedPayload) error {
		if p.ChildRuns == 0 && p.ProviderInvocations == 0 && p.WallSeconds == 0 && p.CostMicros == nil {
			return errors.New("a consumption record with no delta accounts for nothing")
		}
		if p.CostMicros != nil && !p.CostKnown {
			return errors.New("a cost figure was recorded without stating that cost is known")
		}
		if p.CostKnown && p.CostMicros == nil {
			return errors.New("cost was stated as known with no figure: unknown and zero are different facts")
		}
		if p.CostMicros != nil && *p.CostMicros < 0 {
			return fmt.Errorf("cost delta %d must not be negative", *p.CostMicros)
		}
		return errors.Join(
			nonNegative("child_runs", p.ChildRuns),
			nonNegative("provider_invocations", p.ProviderInvocations),
			nonNegative("wall_seconds", p.WallSeconds),
			bounded("key", p.Key),
			bounded("stage_id", p.StageID),
			bounded("run_id", p.RunID))
	}),
	EventPlanRevisionSuperseded: payloadSchema(func(p PlanRevisionSupersededPayload) error {
		if p.FromRevision < 1 || p.ToRevision < 1 {
			return fmt.Errorf("supersession from revision %d to %d must name positive revisions", p.FromRevision, p.ToRevision)
		}
		if p.ToRevision <= p.FromRevision {
			return fmt.Errorf("revision %d cannot supersede %d: a revision replaces an EARLIER one", p.ToRevision, p.FromRevision)
		}
		return errors.Join(bounded("proposal_id", p.ProposalID), boundedList("invalidated_stages", p.InvalidatedStages))
	}),
}

// planStageInvalidated is the third stage outcome: not completed, not failed,
// but no longer valid because an upstream change invalidated its assumptions.
const planStageInvalidated = "invalidated"

// knownOrigin holds a proposal's origin to the catalogue rather than merely to
// non-emptiness: the origin is copied from the caller into a durable event, and
// a value nothing defines describes a provenance no reader can interpret.
func knownOrigin(origin string) error {
	if err := required("origin", origin); err != nil {
		return err
	}
	if !domain.KnownProposalOrigin(origin) {
		return fmt.Errorf("proposal origin %q is not one of %s", origin, strings.Join(domain.ProposalOrigins(), ", "))
	}
	return nil
}

var planDecisionPayload = payloadSchema(func(p PlanDecisionPayload) error {
	if p.Revision < 1 {
		return fmt.Errorf("plan revision %d must be positive", p.Revision)
	}
	return errors.Join(
		required("digest", p.Digest),
		required("operator", p.Operator),
		bounded("note", p.Note))
})

func validatePlanBudget(budget PlanBudgetPayload) error {
	if budget.MaxChildRuns < 1 || budget.MaxConcurrency < 1 || budget.MaxProviderInvocations < 1 {
		return fmt.Errorf("plan budget envelope must bound child runs, concurrency and provider invocations, got %d/%d/%d",
			budget.MaxChildRuns, budget.MaxConcurrency, budget.MaxProviderInvocations)
	}
	if budget.MaxCostMicros != nil && *budget.MaxCostMicros < 0 {
		return fmt.Errorf("plan cost ceiling %d must not be negative", *budget.MaxCostMicros)
	}
	return nonNegative("max_wall_seconds", budget.MaxWallSeconds)
}

// validatePlanReasoning refuses a reasoning record that does not carry its own
// proof. A planner invocation whose workspace was not verified afterwards, or
// which ran in a mutating mode, is exactly what the non-mutating boundary
// exists to prevent, so it cannot be recorded as if it had happened safely.
func validatePlanReasoning(reasoning *PlanReasoningPayload) error {
	if reasoning == nil {
		return nil
	}
	if reasoning.InvocationMode != string(domain.InvocationModeNonMutatingPlanning) {
		return fmt.Errorf("planner reasoning ran in mode %q: planning is %q", reasoning.InvocationMode, domain.InvocationModeNonMutatingPlanning)
	}
	if !reasoning.WorkspaceUnchanged {
		return errors.New("planner reasoning recorded a changed planning workspace: a plan may not be built on an invocation that wrote")
	}
	if reasoning.WorkspaceDigestBefore != reasoning.WorkspaceDigestAfter {
		return errors.New("planner reasoning claims an unchanged workspace while its own digests differ")
	}
	return errors.Join(
		required("agent_id", reasoning.AgentID),
		required("provider_kind", reasoning.ProviderKind),
		required("trust_mode", reasoning.TrustMode),
		required("workspace_digest_before", reasoning.WorkspaceDigestBefore),
		required("workspace_digest_after", reasoning.WorkspaceDigestAfter),
		bounded("vendor_family", reasoning.VendorFamily),
		bounded("model", reasoning.Model),
		bounded("profile_id", reasoning.ProfileID),
		bounded("provider_mode", reasoning.ProviderMode))
}

func init() {
	for eventType, validator := range planPayloads {
		eventPayloads[eventType] = validator
	}
}
