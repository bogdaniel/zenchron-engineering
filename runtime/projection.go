package runtime

import (
	"encoding/json"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// RunProjection is a second read of the same journal Reduce folds, not a second
// state model. Reduce owns disposition, operations, artifacts, and
// state_sha256; this owns the Phase 8 domain view a reconciler needs to decide
// what to do next: which contract governs, which candidate exists, which
// evidence and decisions it produced, and what GitHub currently reports about
// it. Nothing here is derived from wall time, the filesystem, or the network.
//
// CandidateMetadata is the trusted digest of the runtime-owned .git metadata as
// of the last runtime-owned Git operation that SUCCEEDED. It is the only reason
// a restarted runtime can tell tampering apart from its own work: without it
// the baseline would be whatever the live repository happens to say, which is
// exactly what an attacker with the process stopped controls.
//
// A journalled candidate head move clears it, and the operation that produced
// that move re-establishes it from its own operation.after. That keeps the
// baseline bound to the head it was taken at, and keeps the pre-Phase-9 event
// shape - a candidate.committed with no baseline behind it - honest: it falls
// back to the in-process baseline rather than trusting a digest for a head the
// workspace has since left.
type RunProjection struct {
	Contract             Ref                            `json:"contract,omitzero"`
	Subject              domain.Subject                 `json:"subject,omitzero"`
	CandidateRevision    string                         `json:"candidate_revision,omitempty"`
	CandidateTree        string                         `json:"candidate_tree,omitempty"`
	CandidateMetadata    string                         `json:"candidate_metadata,omitempty"`
	BaseRevision         string                         `json:"base_revision,omitempty"`
	SourceIntentChanged  bool                           `json:"source_intent_changed"`
	ObservedExternalHead string                         `json:"observed_external_head,omitempty"`
	Reassessment         *ReassessmentCompletedPayload  `json:"reassessment,omitempty"`
	EvidenceBundles      []Ref                          `json:"evidence_bundles,omitempty"`
	AuthorityDecisions   map[string]AuthorityEvaluation `json:"authority_decisions,omitempty"`
	PullRequest          *PullRequestObservation        `json:"pull_request,omitempty"`
	Assurance            *AssuranceObservation          `json:"assurance,omitempty"`
	CI                   *CIObservation                 `json:"ci,omitempty"`
	Review               *ReviewObservation             `json:"review,omitempty"`
	Attempts             map[string]int                 `json:"attempts,omitempty"`
	// ExecutionDiagnostic is the LATEST sanitized execution failure the journal
	// holds. It is projected from operation.after, exactly like the metadata
	// baseline is, so a restarted process reports the same root cause without
	// any process-local error state and without an operator opening runtime.db
	// or knowing an artifact path.
	ExecutionDiagnostic *ExecutionDiagnostic `json:"execution_diagnostic,omitempty"`
	// CandidateComplete reports whether the CURRENT candidate head is
	// execution-complete: a producer finished against it, rather than being cut
	// off mid-invocation with its partial work preserved. It is what separates
	// a runtime-owned checkpoint from a candidate anything downstream may act
	// on, and it is why assurance cannot run on interrupted work.
	//
	// It is a fold, so the LAST event about the head decides: a commit or a
	// base integration makes the head complete, a checkpoint makes it
	// incomplete, and a producer completion observed against it completes it
	// without inventing a commit that no mutation produced.
	CandidateComplete bool `json:"candidate_complete"`
	// Checkpoints is how many times a producer left work unfinished on this
	// run. It only ever grows, which is what makes it a bound rather than a
	// counter a stalling provider could reset by producing something new.
	Checkpoints int `json:"checkpoints,omitempty"`
}

// Observation is the part of every latest-wins observation that is about the
// observation rather than its subject: where it came from in the journal, and
// whether the head it describes is still the run's head. A reconciler ignores a
// stale observation instead of acting on a finding for a superseded commit.
type Observation struct {
	Sequence int64 `json:"sequence"`
	Stale    bool  `json:"stale"`
}

type PullRequestObservation struct {
	GitHubPRObservedPayload
	Observation
}
type AssuranceObservation struct {
	AssuranceObservedPayload
	Observation
}
type CIObservation struct {
	GitHubCIObservedPayload
	Observation
}
type ReviewObservation struct {
	GitHubReviewObservedPayload
	Observation
}

// AuthorityEvaluation is the latest decision recorded for one action.
type AuthorityEvaluation struct {
	AuthorityEvaluatedPayload
	Sequence int64 `json:"sequence"`
}

// Head is the head every observation is judged against: the candidate commit
// the runtime recorded, or, before the first commit, the observed pull request
// head, which is then the only head that exists.
func (p RunProjection) Head() string {
	if p.CandidateRevision != "" {
		return p.CandidateRevision
	}
	if p.PullRequest != nil {
		return p.PullRequest.HeadRevision
	}
	return ""
}

// Project folds an ordered event slice into the Phase 8 view. It is pure: the
// same events always produce the same projection. It does not re-verify the
// hash chain or the sequence, which is Reduce's job; it does refuse a payload
// it cannot decode, because a durable row that no longer matches its schema is
// not something to guess at.
func Project(events []EngineeringEvent) (RunProjection, error) {
	var p RunProjection
	for _, e := range events {
		if err := p.apply(e); err != nil {
			return RunProjection{}, fmt.Errorf("event %q (%s): %w", e.ID, e.Type, err)
		}
	}
	// Staleness is settled once, at the end, against the head the run finished
	// on. Deciding it while folding would freeze an observation as current that
	// a later candidate commit superseded.
	head := p.Head()
	if p.PullRequest != nil {
		p.PullRequest.Stale = p.PullRequest.HeadRevision != head
	}
	if p.Assurance != nil {
		p.Assurance.Stale = p.Assurance.Commit != head
	}
	if p.CI != nil {
		p.CI.Stale = p.CI.HeadRevision != head
	}
	if p.Review != nil {
		p.Review.Stale = p.Review.HeadRevision != head
	}
	return p, nil
}

func (p *RunProjection) apply(e EngineeringEvent) error {
	switch e.Type {
	case EventSourceIntentChanged:
		if _, err := decodePayload[SourceIntentChangedPayload](e.Payload); err != nil {
			return err
		}
		p.SourceIntentChanged = true
	case EventContractCompiled:
		payload, err := decodePayload[ContractCompiledPayload](e.Payload)
		if err != nil {
			return err
		}
		p.Contract = payload.Contract
		p.Subject = payload.Subject
	case EventCandidateCommitted:
		payload, err := decodePayload[CandidateCommittedPayload](e.Payload)
		if err != nil {
			return err
		}
		p.CandidateRevision, p.CandidateTree = payload.Commit, payload.Tree
		p.CandidateMetadata = ""
		p.CandidateComplete = true
	case EventCandidateCheckpointed:
		payload, err := decodePayload[CandidateCommittedPayload](e.Payload)
		if err != nil {
			return err
		}
		// A checkpoint moves the head exactly as a commit does - it is a real
		// commit - but it leaves the head INCOMPLETE, which is what keeps
		// assurance and everything past it ineligible.
		p.CandidateRevision, p.CandidateTree = payload.Commit, payload.Tree
		p.CandidateMetadata = ""
		p.CandidateComplete = false
		p.Checkpoints++
	case EventExecutionCompleted:
		if _, err := decodePayload[ExecutionCompletedPayload](e.Payload); err != nil {
			return err
		}
		p.CandidateComplete = true
	case EventCandidateBaseIntegrated:
		payload, err := decodePayload[CandidateBaseIntegratedPayload](e.Payload)
		if err != nil {
			return err
		}
		p.BaseRevision = payload.BaseRevision
		p.CandidateRevision, p.CandidateTree = payload.Commit, payload.Tree
		p.CandidateMetadata = ""
		p.CandidateComplete = true
	case EventCandidateExternalChanged:
		payload, err := decodePayload[CandidateExternalChangedPayload](e.Payload)
		if err != nil {
			return err
		}
		p.ObservedExternalHead = payload.ObservedRevision
	case EventReassessmentCompleted:
		payload, err := decodePayload[ReassessmentCompletedPayload](e.Payload)
		if err != nil {
			return err
		}
		p.Reassessment = &payload
		p.Contract = payload.Contract
	case EventAuthorityEvaluated:
		payload, err := decodePayload[AuthorityEvaluatedPayload](e.Payload)
		if err != nil {
			return err
		}
		if p.AuthorityDecisions == nil {
			p.AuthorityDecisions = map[string]AuthorityEvaluation{}
		}
		key := payload.Action.Type + "\x00" + payload.Action.Target
		p.AuthorityDecisions[key] = AuthorityEvaluation{payload, e.Sequence}
	case EventAssuranceObserved:
		if len(e.Payload) == 0 {
			return nil // artifact-only form; it names no head to bind to
		}
		payload, err := decodePayload[AssuranceObservedPayload](e.Payload)
		if err != nil {
			return err
		}
		if payload.Bundle != (Ref{}) && !containsRef(p.EvidenceBundles, payload.Bundle) {
			p.EvidenceBundles = append(p.EvidenceBundles, payload.Bundle)
		}
		if p.Assurance == nil || supersedes(p.Assurance.Commit, payload.Commit, p.Head()) {
			p.Assurance = &AssuranceObservation{payload, Observation{Sequence: e.Sequence}}
		}
	case EventGitHubPRObserved:
		payload, err := decodePayload[GitHubPRObservedPayload](e.Payload)
		if err != nil {
			return err
		}
		// The pull request is anchored to the recorded candidate commit only:
		// before there is one, the observed pull request head is the head, so a
		// newly observed head must be adopted rather than judged against itself.
		if p.PullRequest == nil || supersedes(p.PullRequest.HeadRevision, payload.HeadRevision, p.CandidateRevision) {
			p.PullRequest = &PullRequestObservation{payload, Observation{Sequence: e.Sequence}}
		}
	case EventGitHubCIObserved:
		payload, err := decodePayload[GitHubCIObservedPayload](e.Payload)
		if err != nil {
			return err
		}
		if p.CI == nil || supersedes(p.CI.HeadRevision, payload.HeadRevision, p.Head()) {
			p.CI = &CIObservation{payload, Observation{Sequence: e.Sequence}}
		}
	case EventGitHubReviewObserved:
		payload, err := decodePayload[GitHubReviewObservedPayload](e.Payload)
		if err != nil {
			return err
		}
		if p.Review == nil || supersedes(p.Review.HeadRevision, payload.HeadRevision, p.Head()) {
			p.Review = &ReviewObservation{payload, Observation{Sequence: e.Sequence}}
		}
	case EventOperationBefore:
		// One operation.before is one attempt. Reduce keeps the latest operation
		// document per id; how many times a kind was attempted is not in it.
		operation, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			return err
		}
		if p.Attempts == nil {
			p.Attempts = map[string]int{}
		}
		p.Attempts[operation.Kind]++
	case EventOperationAfter:
		operation, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			return err
		}
		// A failed execution is the one operation whose RESULT an operator has
		// to be told about: the diagnostic is what names the root cause. It is
		// read before the succeeded-only baseline rule below, because that rule
		// exists to refuse an untrustworthy digest, not to hide a failure.
		if operation.Kind == OpExecutionInvoke {
			diagnostic, err := executionDiagnosticOf(operation.Result)
			if err != nil {
				return err
			}
			if diagnostic != nil {
				p.ExecutionDiagnostic = diagnostic
			}
		}
		// The ordering rule: a new baseline is adopted only from an operation
		// that SUCCEEDED. operation.after is the last event an operation
		// appends, so by the time this row exists the mutation and its own
		// events are already durable. A failed operation carries no digest and
		// would not be believed if it did.
		if operation.State != Succeeded {
			return nil
		}
		digest, err := metadataBaseline(operation.Result)
		if err != nil {
			return err
		}
		if digest != "" {
			p.CandidateMetadata = digest
		}
	}
	return nil
}

// supersedes decides whether a newly observed finding may replace the one
// already held for its kind. Latest wins, except that a finding for a head that
// is not the run's current head never displaces one that is for the current
// head: that would erase the only current-head answer the reconciler has and
// leave a superseded finding looking authoritative. The displaced-from case is
// still representable, because the surviving observation carries Stale.
func supersedes(existingHead, head, current string) bool {
	return head == current || existingHead != current
}

func containsRef(refs []Ref, ref Ref) bool {
	for _, existing := range refs {
		if existing == ref {
			return true
		}
	}
	return false
}

// metadataBaseline reads the trusted .git metadata digest an operation result
// recorded, if it recorded one. Operation results are per-handler shapes, so
// this reads the one shared field rather than decoding the whole document; it
// is still pure and total over the bytes the journal holds.
func metadataBaseline(result json.RawMessage) (string, error) {
	if len(result) == 0 {
		return "", nil
	}
	var baseline struct {
		MetadataDigest string `json:"metadata_digest"`
	}
	if err := json.Unmarshal(result, &baseline); err != nil {
		return "", fmt.Errorf("invalid operation result: %w", err)
	}
	return baseline.MetadataDigest, nil
}

// decodePayload reuses the registry's strict decode, so the projection reads a
// payload exactly as strictly as the journal accepted it.
func decodePayload[T any](raw json.RawMessage) (T, error) {
	var payload T
	err := strictJSON(raw, &payload)
	return payload, err
}

// executionDiagnosticOf reads the sanitized diagnostic an execution.invoke
// result recorded, if it recorded one. Like metadataBaseline it decodes the
// fields it needs rather than the whole per-handler document, so an older row
// written before a field existed still projects. failure_class is read from the
// embedded mutationResult when the diagnostic itself does not carry one, which
// is what keeps rows written by the previous controller readable.
func executionDiagnosticOf(result json.RawMessage) (*ExecutionDiagnostic, error) {
	if len(result) == 0 {
		return nil, nil
	}
	var record struct {
		FailureClass FailureClass         `json:"failure_class"`
		Diagnostic   *ExecutionDiagnostic `json:"diagnostic"`
	}
	if err := json.Unmarshal(result, &record); err != nil {
		return nil, fmt.Errorf("decode execution result: %w", err)
	}
	if record.Diagnostic == nil {
		return nil, nil
	}
	if record.Diagnostic.FailureClass == "" {
		record.Diagnostic.FailureClass = record.FailureClass
	}
	return record.Diagnostic, nil
}
