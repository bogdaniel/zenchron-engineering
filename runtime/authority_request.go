package runtime

// authority_request.go is the operator-facing human-authority boundary.
//
// It contains exactly two things: a PROJECTION of the human-authority
// requirement the run's own authority evaluation currently reports
// (AuthorityRequest), and the operation that records a human's answer to it
// (Authorize). Neither is a source of governance.
//
// The one invariant the whole file turns on:
//
//	Authorize records EVIDENCE. It never assigns authority.
//
// There is no assignment to AuthorityDecision.Status anywhere below, no write
// of an authority.evaluated event, and no change to the run's phase or
// disposition. The only durable effect of Authorize is ONE
// human.authority_recorded event. What that evidence means is decided by the
// #7 evaluator in package authority, through the same KernelFlow.Decide bridge
// the runtime's own authority.evaluate operation uses. Every one of these is a
// correct outcome of the same call:
//
//	evidence recorded -> authorized
//	evidence recorded -> still blocked   (something else is failing or denied)
//	evidence recorded -> still incomplete (a non-human claim is unsatisfied)
//	evidence refused  -> the subject moved underneath the request
//
// Recording human authority is NOT an engineering side effect, so it does not
// take the run's operation lease: the event is appended straight through the
// journal with its own durable identity. Phase 10 §0 made event identity safe
// for exactly this.
//
// Authorization is contextual evidence, never a standing permission. A request
// names one exact state - controller, pinned source, base, candidate commit AND
// tree, contract revision, and the exact missing requirement - and its id is a
// digest of all of it. An approval is therefore inert the moment any of that
// moves, and it is never silently retargeted at the new state. The action
// string, the pull request number and the issue number are deliberately NOT
// sufficient to carry an approval forward; none of them identifies a subject.

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// HumanApprovalEvidenceClass is the evidence class a required claim uses to say
// "a human has to answer this". It is the same string package authority tests
// producer type against, named here so the runtime side is not a loose literal.
const HumanApprovalEvidenceClass domain.EvidenceClass = "human_approval"

// authorizableActions is the self-adoption boundary (§7), stated as an
// allowlist rather than a deny-list.
//
// Human authority over publication is legitimate: opening a pull request,
// updating one, and pushing the run-owned candidate branch are all actions a
// policy may legitimately place behind a human. ADOPTING the candidate is not
// on this list and is not made governable here. A candidate Zenchron cannot
// authorize its own adoption, so a merge or adoption action is refused as
// unsupported rather than given a shape that looks governed. There is
// deliberately no `autonomy merge` and no automatic pull request merge anywhere
// in this file.
var authorizableActions = map[string]bool{
	"git.pull_request.create": true,
	"git.pull_request.update": true,
	"candidate.push":          true,
}

// ---------------------------------------------------------------------------
// Refusals
// ---------------------------------------------------------------------------

// AuthorityRefusedError is the single typed refusal this boundary produces.
// Callers branch on Code, never on message text.
type AuthorityRefusedError struct{ Code, Detail string }

func (e *AuthorityRefusedError) Error() string {
	return "authority_refused: " + e.Code + ": " + e.Detail
}

const (
	// RefusedNoRequest: nothing about this run currently requires human
	// authority, so there is nothing to authorize.
	RefusedNoRequest = "no_authority_request"
	// RefusedStaleRequest: the named request is not the request the run's
	// current state produces. This is §5 in one code.
	RefusedStaleRequest = "stale_authority_request"
	// RefusedUnsupportedAction: §7. Merge/adoption is not governable here.
	RefusedUnsupportedAction = "unsupported_action"
	// RefusedControllerChanged: a different controller/configuration created
	// this run. A human cannot approve past a controller mismatch.
	RefusedControllerChanged = "controller_changed"
	// RefusedSourceMoved: the pinned source moved. Nothing may be authorized
	// against intent the run has not recompiled.
	RefusedSourceMoved = "source_intent_changed"
	// RefusedExternalHead: the candidate head observed externally is not the
	// head the runtime recorded. A human cannot approve past that.
	RefusedExternalHead = "candidate_external_changed"
	// RefusedRunTerminal: the run is finished; evidence about it is inert.
	RefusedRunTerminal = "run_terminal"
	// RefusedInvalidDecision: the recorded human answer is not approve/reject.
	RefusedInvalidDecision = "invalid_decision"
)

func refuse(code, detail string) error { return &AuthorityRefusedError{Code: code, Detail: detail} }

// ---------------------------------------------------------------------------
// AuthorityRequest
// ---------------------------------------------------------------------------

// AuthorityRequest is a projection of run state plus the exact missing human
// authority requirement. It is never persisted and never consulted as a
// permission: it is regenerated from the journal on every read, so it cannot
// drift from the state it describes.
//
// ID and Digest are DERIVED. Digest is a SHA-256 over the canonical (RFC 8785)
// form of this struct with the four observation members below cleared, and ID
// is a short prefix of it. Every other member is therefore part of the id: the
// run, the action, the repository, the controller identity, the pinned source
// snapshot, the exact base revision, the exact candidate revision AND tree, the
// contract revision, the current decision and evidence context, and the exact
// outstanding requirement. That is what lets an operator name a request id and
// be naming exact state - and why they never have to type a candidate SHA or a
// contract revision by hand.
//
// The digest is computed by zeroing rather than by listing members, following
// EventDigest and StateDigest. A member added later is in the digest by
// default, which is the safe direction: a new binding tightens staleness rather
// than being silently ignored by it.
type AuthorityRequest struct {
	SchemaVersion string `json:"schema_version"`

	ID     string `json:"id"`
	Digest string `json:"digest"`

	RunID      string        `json:"run_id"`
	Repository string        `json:"repository"`
	Action     domain.Action `json:"action"`
	// Controller is the controller identity and its configuration digest.
	Controller Ref `json:"controller"`
	// Source is the pinned source snapshot identity and its content digest.
	Source Ref `json:"source"`
	// Base is the exact base revision the candidate is measured against.
	Base Ref `json:"base"`
	// Candidate is the exact candidate revision AND tree. Both, because a
	// rewritten commit with an identical tree and an identical commit with a
	// different tree are both changes of subject.
	Candidate Candidate `json:"candidate"`
	Contract  Ref       `json:"contract"`

	// Decision, Status and the requirement lists are the current authority and
	// evidence context. Requires is the subset of Missing whose required claims
	// are human_approval claims: it is the REASON human authority is required,
	// stated as the exact claim ids that are outstanding.
	Decision Ref                    `json:"decision"`
	Status   domain.AuthorityStatus `json:"status"`
	Requires []string               `json:"requires"`
	Missing  []string               `json:"missing"`
	Stale    []string               `json:"stale,omitempty"`
	Blocking []string               `json:"blocking,omitempty"`
	Evidence []Ref                  `json:"evidence,omitempty"`

	// Observation members. These are what the operator was SHOWN, not what the
	// request is bound to, and they are excluded from the digest.
	//
	// StateSHA256 in particular is the whole run's state digest, which changes
	// on every appended event including ones with no bearing on this subject -
	// a controller recording a GitHub observation, for instance. Binding the
	// request id to it would expire an operator's request for reasons that have
	// nothing to do with what they are approving, while adding nothing: every
	// governance-material component of the state is already in the digest
	// above.
	StateSHA256 string      `json:"state_sha256"`
	ObservedAt  time.Time   `json:"observed_at"`
	Disposition Disposition `json:"disposition"`
	Reason      string      `json:"reason,omitempty"`
}

// identify computes and stamps ID and Digest. See the type comment for why the
// preimage is "this struct minus the observation members".
func (a AuthorityRequest) identify() (AuthorityRequest, error) {
	preimage := a
	preimage.ID, preimage.Digest = "", ""
	preimage.StateSHA256, preimage.Reason = "", ""
	preimage.ObservedAt, preimage.Disposition = time.Time{}, ""
	digest, err := Digest(preimage)
	if err != nil {
		return AuthorityRequest{}, err
	}
	a.ID, a.Digest = "authreq-"+digest[:32], digest
	return a, nil
}

// binding is the frozen HumanAuthorityBinding this request would produce. The
// rule that an approval must not be carried onto a moved candidate or contract
// lives in adapters.go and is not restated here.
func (a AuthorityRequest) binding(decision, humanID string) HumanAuthorityBinding {
	return HumanAuthorityBinding{
		RunID:             a.RunID,
		CandidateRevision: a.Candidate.Revision,
		CandidateTree:     a.Candidate.Tree,
		Contract:          a.Contract,
		Action:            a.Action.Type,
		Decision:          decision,
		HumanID:           humanID,
	}
}

// PendingAuthorityRequest is the operator read. It returns the run's current
// human-authority request, or (nil, nil) when the run does not currently
// require human authority. It performs no network call and appends nothing.
func (r *EngineeringRuntime) PendingAuthorityRequest(runID string) (*AuthorityRequest, error) {
	state, err := r.load(runID)
	if err != nil {
		return nil, err
	}
	return r.authorityRequest(state)
}

// authorityRequest derives the request from replayed state. It never invents a
// request: the action, the decision and the outstanding claims all come from
// the SAME kernel evaluation the runtime's authority.evaluate operation
// performs, and a run whose evaluation reports no outstanding human_approval
// claim has no request at all.
func (r *EngineeringRuntime) authorityRequest(state *runState) (*AuthorityRequest, error) {
	if err := authorizableState(state); err != nil {
		return nil, err
	}
	if state.source == nil || state.projection.CandidateRevision == "" || state.projection.Contract == (Ref{}) {
		return nil, nil
	}
	kernel, err := r.decide(state)
	if err != nil {
		return nil, err
	}
	requires := outstandingHumanClaims(kernel)
	if len(requires) == 0 {
		return nil, nil
	}
	if !authorizableActions[kernel.Decision.Action.Type] {
		return nil, refuse(RefusedUnsupportedAction, "action "+kernel.Decision.Action.Type+" is not a human-authorizable action")
	}
	disposition, reason := state.conditions()
	request := AuthorityRequest{
		SchemaVersion: SchemaVersion,
		RunID:         state.run.ID,
		Repository:    state.run.Repository,
		Action:        kernel.Decision.Action,
		Controller:    Ref{ID: r.deps.ControllerID, Revision: state.run.ControllerSHA256},
		Source:        Ref{ID: sourceSnapshotID(state), Revision: state.source.Digest},
		Base:          Ref{ID: r.deps.Repository.DefaultBranch, Revision: state.baseRevision()},
		Candidate: Candidate{
			Branch:   candidateBranch(state.run.ID),
			Revision: state.projection.CandidateRevision,
			Tree:     state.projection.CandidateTree,
		},
		Contract:    state.projection.Contract,
		Decision:    Ref{ID: kernel.Decision.ID, Revision: kernel.Decision.Revision},
		Status:      kernel.Decision.Status,
		Requires:    requires,
		Missing:     kernel.Decision.Missing,
		Stale:       kernel.Decision.Stale,
		Blocking:    kernel.Decision.Blocking,
		Evidence:    evidenceRefs(kernel.Evidence),
		StateSHA256: state.snapshot.StateSHA256,
		ObservedAt:  r.deps.Clock.Now(),
		Disposition: disposition,
		Reason:      reason,
	}
	identified, err := request.identify()
	if err != nil {
		return nil, err
	}
	return &identified, nil
}

// authorizableState is what a run must be before ANY human authority is read
// or recorded for it. Each condition is a live problem that a human answer is
// not the fix for, so each is a refusal rather than an outstanding request -
// and because it gates recording as well as reading, no approval can be used
// to override one.
func authorizableState(state *runState) error {
	// §7 first and unconditionally: if the journal already holds a decision for
	// an action this boundary does not govern, refuse the whole run rather than
	// answer about the part that happens to be supported.
	if err := requireAuthorizableDecisions(state); err != nil {
		return err
	}
	if state.controllerChanged {
		return refuse(RefusedControllerChanged, "the run was created by a different controller or configuration")
	}
	if state.projection.ObservedExternalHead != "" {
		return refuse(RefusedExternalHead, "the observed candidate head is not the recorded head")
	}
	if state.projection.SourceIntentChanged {
		return refuse(RefusedSourceMoved, "the pinned source moved and has not been recompiled")
	}
	if terminalDisposition(state.snapshot.Disposition) {
		return refuse(RefusedRunTerminal, "run is "+string(state.snapshot.Disposition))
	}
	return nil
}

// requireAuthorizableDecisions refuses a run whose journal holds an authority
// decision for an action outside the allowlist. Iteration is over sorted keys
// so the refusal is deterministic when more than one is present.
func requireAuthorizableDecisions(state *runState) error {
	keys := make([]string, 0, len(state.projection.AuthorityDecisions))
	for key := range state.projection.AuthorityDecisions {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		action := state.projection.AuthorityDecisions[key].Action
		if !authorizableActions[action.Type] {
			return refuse(RefusedUnsupportedAction, "action "+action.Type+" is not a human-authorizable action")
		}
	}
	return nil
}

// outstandingHumanClaims is the exact reason human authority is required: the
// missing requirements that are human_approval claims of the governing
// contract. A missing requirement that is not such a claim (a permission, a
// capability, an automated test) is deliberately not included - no human
// answer can supply it.
func outstandingHumanClaims(kernel KernelState) []string {
	var out []string
	for _, requirement := range kernel.Decision.Missing {
		if claim, ok := kernel.Contract.RequiredClaims[requirement]; ok && claim.EvidenceClass == HumanApprovalEvidenceClass {
			out = append(out, requirement)
		}
	}
	sort.Strings(out)
	return out
}

func evidenceRefs(bundles map[string]domain.EvidenceBundle) []Ref {
	refs := make([]Ref, 0, len(bundles))
	for _, bundle := range bundles {
		refs = append(refs, Ref{ID: bundle.ID, Revision: bundle.Revision})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID != refs[j].ID {
			return refs[i].ID < refs[j].ID
		}
		return refs[i].Revision < refs[j].Revision
	})
	return refs
}

// ---------------------------------------------------------------------------
// Kernel evaluation including recorded human authority
// ---------------------------------------------------------------------------

// decide rebuilds the kernel from replayed state, folds in the human authority
// the run has actually recorded, and asks #7. It is the same rebuild and the
// same KernelFlow.Decide bridge that operations.go's authority.evaluate uses;
// the only addition is the human evidence bundle, which is built the same way
// the assurance bundle is - bound to the exact subject, contract and policy
// revisions, so it cannot apply to anything else.
func (r *EngineeringRuntime) decide(state *runState) (KernelState, error) {
	kernel, err := r.buildKernel(state)
	if err != nil {
		return KernelState{}, err
	}
	bundle, ok, err := humanAuthorityEvidence(state, kernel.Contract)
	if err != nil {
		return KernelState{}, err
	}
	if ok {
		kernel.Evidence[bundle.ID] = bundle
	}
	action := domain.Action{Type: PublicationActionType, Target: r.deps.Repository.DefaultBranch}
	producer := domain.EvidenceProducer{ID: providerIdentity(state), Type: domain.ProducerExecutionProvider}
	return r.flow.Decide(kernel, action, producer)
}

// humanAuthorityEvidence rebuilds the human-approval evidence the run can
// prove for exactly the contract handed in.
//
// Two bindings decide whether a recorded authority is evidence here at all: the
// contract revision it was given against, and the candidate revision it was
// given against. Neither is ever rebound. That is why an approval survives no
// candidate move, no base integration, and no reassessment - the record stays
// in the journal as a permanent fact about a subject that no longer exists, and
// it simply stops being evidence for the subject that does.
//
// A "reject" is recorded as FAILED evidence, not as an absence. That is what
// makes a refusal blocking rather than merely unsatisfied.
//
// ponytail: one item per claim, latest recorded answer wins, so an operator can
// correct an answer against the same subject. Per-operator items - so that any
// standing rejection blocks regardless of a later approval by someone else - is
// the upgrade if a milestone ever records more than one operator identity.
func humanAuthorityEvidence(state *runState, contract domain.EngineeringWorkContract) (domain.EvidenceBundle, bool, error) {
	contractRef := Ref{ID: contract.ID, Revision: contract.Revision}
	items := map[string]domain.EvidenceItem{}
	for _, event := range state.events {
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		payload, err := decodePayload[HumanAuthorityRecordedPayload](event.Payload)
		if err != nil {
			return domain.EvidenceBundle{}, false, fmt.Errorf("event %q (%s): %w", event.ID, event.Type, err)
		}
		if payload.Contract != contractRef || payload.Candidate.Revision != contract.Subject.Revision {
			continue
		}
		result := domain.EvidenceResult{Status: domain.EvidenceFailed}
		if payload.Decision == "approve" {
			result = domain.EvidenceResult{Status: domain.EvidencePassed}
		}
		for claimID, claim := range contract.RequiredClaims {
			if claim.EvidenceClass != HumanApprovalEvidenceClass {
				continue
			}
			items["human-"+claimID] = domain.EvidenceItem{
				ClaimID:       claimID,
				EvidenceClass: HumanApprovalEvidenceClass,
				Producer:      domain.EvidenceProducer{ID: payload.Operator.ID, Type: domain.ProducerHuman},
				// The environment is the local operator boundary and it records
				// the PROVENANCE of the identity, which in M0 is unverified.
				// Nothing here is a credential; see operator.go.
				Environment: domain.EvidenceEnvironment{Type: "operator_boundary", Identifier: string(payload.Operator.Provenance)},
				Result:      result,
				Lifecycle:   domain.EvidenceLifecycle{Status: domain.EvidenceValid},
				Provenance: domain.EvidenceProvenance{
					Source:     "zenchron-operator",
					RecordedAt: event.OccurredAt.UTC().Format(time.RFC3339),
					Integrity:  &domain.EvidenceIntegrity{Method: "git-tree-sha", Value: payload.Candidate.Tree},
				},
			}
		}
	}
	if len(items) == 0 {
		return domain.EvidenceBundle{}, false, nil
	}
	bundle := domain.EvidenceBundle{
		SchemaVersion: domain.SchemaVersion,
		ID:            "human-evidence-" + state.run.ID,
		Revision:      contract.Subject.Revision + "@" + contract.Revision,
		Subject:       contract.Subject,
		Contract:      domain.ObjectRevision{ID: contract.ID, Revision: contract.Revision},
		Policy:        contract.Provenance.Policy,
		Evidence:      items,
	}
	if _, err := domain.Encode(bundle); err != nil {
		return domain.EvidenceBundle{}, false, fmt.Errorf("rebuilt human authority evidence bundle is invalid: %w", err)
	}
	return bundle, true, nil
}

// ---------------------------------------------------------------------------
// Authorize
// ---------------------------------------------------------------------------

// AuthorizeInput is one operator's answer to one exact request.
type AuthorizeInput struct {
	RunID string
	// RequestID is the request the operator is answering. It is required: a
	// human answer that does not name a subject is not evidence of anything.
	RequestID string
	// Digest optionally pins the request's full binding digest as well. The
	// id already contains it, so this is a redundancy check, not a second key.
	Digest string
	// Action optionally states which action is being authorized. When set it
	// must be both a human-authorizable action and the request's own action,
	// so a caller asking to authorize a merge or an adoption is refused as
	// unsupported instead of quietly authorizing publication.
	Action domain.Action
	// Decision is "approve" or "reject". Both are evidence.
	Decision string
	// Note is an OPTIONAL, UNTRUSTED operator annotation. It is an input to
	// nothing: it is excluded from the idempotency key and from the binding.
	Note string
	// Operator is the already-resolved operator provenance, from
	// OperatorConfig.ResolveOperator. It is resolved by the caller because the
	// operator layer's configuration is deliberately not a runtime dependency.
	Operator RecordedOperator
}

// AuthorizeResult is what the operator is told. Status is what the #7
// evaluator concluded AFTER the evidence was recorded - it is a report, not a
// grant, and "still blocked" and "still incomplete" are ordinary results.
type AuthorizeResult struct {
	RunID      string `json:"run_id"`
	EvidenceID string `json:"evidence_id"`
	// Recorded is false when this call adopted evidence that already existed
	// for the same request, operator and decision, which is what makes a retry
	// after a crash safe rather than duplicating the record.
	Recorded bool `json:"recorded"`
	// Request references the exact request this evidence answers: its id and
	// its full binding digest. It is a reference rather than a copy because a
	// satisfied request no longer exists to project, and an adopted retry must
	// still be able to say which request it answered.
	Request Ref `json:"request"`

	Status      domain.AuthorityStatus `json:"status"`
	Missing     []string               `json:"missing,omitempty"`
	Stale       []string               `json:"stale,omitempty"`
	Blocking    []string               `json:"blocking,omitempty"`
	Disposition Disposition            `json:"disposition"`
	Reason      string                 `json:"reason,omitempty"`
}

// Authorize records one human authority decision against one exact request.
//
// It replays the run, recomputes the current request, refuses anything that is
// not the exact current binding, appends ONE human.authority_recorded event
// without taking the run's operation lease, replays again, and re-evaluates
// through the normal kernel path. It assigns no status and changes no phase.
func (r *EngineeringRuntime) Authorize(ctx context.Context, in AuthorizeInput) (AuthorizeResult, error) {
	if err := ctx.Err(); err != nil {
		return AuthorizeResult{}, err
	}
	// §7 before anything is loaded: being ASKED to authorize an adoption is
	// refused as unsupported whatever the run's state happens to be.
	if in.Action != (domain.Action{}) && !authorizableActions[in.Action.Type] {
		return AuthorizeResult{}, refuse(RefusedUnsupportedAction, "action "+in.Action.Type+" is not a human-authorizable action")
	}
	if in.Decision != "approve" && in.Decision != "reject" {
		return AuthorizeResult{}, refuse(RefusedInvalidDecision, "decision must be approve or reject")
	}
	if in.Operator.ID == "" {
		return AuthorizeResult{}, &OperatorIdentityError{Detail: "recording human authority requires a resolved operator identity"}
	}
	if in.Operator.Provenance != ProvenanceLocalUnverified {
		return AuthorizeResult{}, &OperatorIdentityError{Detail: "operator provenance " + string(in.Operator.Provenance) + " is not a provenance this milestone can record"}
	}
	state, err := r.load(in.RunID)
	if err != nil {
		return AuthorizeResult{}, err
	}
	if err := authorizableState(state); err != nil {
		return AuthorizeResult{}, err
	}
	// Idempotency is settled before the request is projected, because a
	// SATISFIED request no longer exists: once an approval is recorded the
	// human requirement is met, so the run stops producing a request for it.
	// A retry after a crash that lost the CLI's output would otherwise be told
	// there is nothing to authorize instead of finding its own record.
	existing, adopted, err := recordedAuthority(state, in)
	if err != nil {
		return AuthorizeResult{}, err
	}
	reference, evidenceID := existing.Request, existing.EvidenceID
	recorded := false
	if !adopted {
		request, err := r.authorityRequest(state)
		if err != nil {
			return AuthorizeResult{}, err
		}
		if request == nil {
			return AuthorizeResult{}, refuse(RefusedNoRequest, "the run does not currently require human authority")
		}
		// The staleness check. The request id is a digest of the exact
		// bindings, so naming an id that is not the current one is naming state
		// the run has left - a superseded candidate, a re-integrated base, a
		// new contract revision, a re-pinned source, or a changed requirement.
		if in.RequestID != request.ID {
			return AuthorizeResult{}, refuse(RefusedStaleRequest,
				"request "+in.RequestID+" is not the run's current request "+request.ID)
		}
		if in.Digest != "" && in.Digest != request.Digest {
			return AuthorizeResult{}, refuse(RefusedStaleRequest, "request digest does not match the current binding")
		}
		if in.Action != (domain.Action{}) && in.Action != request.Action {
			return AuthorizeResult{}, refuse(RefusedStaleRequest, "the named action is not the request's action")
		}
		// The frozen rule, applied to the durable run document as well. The
		// request above was derived from the journal projection; this is the
		// independent check that the run's own identity bindings still agree.
		if err := request.binding(in.Decision, in.Operator.ID).Validate(state.snapshot); err != nil {
			return AuthorizeResult{}, refuse(RefusedStaleRequest, err.Error())
		}
		reference = Ref{ID: request.ID, Revision: request.Digest}
		if evidenceID, err = humanEvidenceID(reference, in.Operator.ID, in.Decision); err != nil {
			return AuthorizeResult{}, err
		}
		if recorded, err = r.recordHumanAuthority(state, *request, in, evidenceID); err != nil {
			return AuthorizeResult{}, err
		}
	}

	// Replay and re-evaluate. Nothing below decides anything: it reports what
	// the evaluator concluded and what the run's own conditions now say.
	state, err = r.load(state.run.ID)
	if err != nil {
		return AuthorizeResult{}, err
	}
	kernel, err := r.decide(state)
	if err != nil {
		return AuthorizeResult{}, err
	}
	disposition, reason := state.conditions()
	return AuthorizeResult{
		RunID:       state.run.ID,
		EvidenceID:  evidenceID,
		Recorded:    recorded,
		Request:     reference,
		Status:      kernel.Decision.Status,
		Missing:     kernel.Decision.Missing,
		Stale:       kernel.Decision.Stale,
		Blocking:    kernel.Decision.Blocking,
		Disposition: disposition,
		Reason:      reason,
	}, nil
}

// recordedAuthority finds evidence this run already holds for exactly this
// request, operator and decision. It is the read half of the idempotency key:
// the same three inputs that mint the key are the ones matched here, so a retry
// recognises its own record without recomputing a request that may since have
// been satisfied. The note is not matched, for the same reason it is not in the
// key. The latest matching record wins, so a corrected answer is the one found.
func recordedAuthority(state *runState, in AuthorizeInput) (HumanAuthorityRecordedPayload, bool, error) {
	for i := len(state.events) - 1; i >= 0; i-- {
		event := state.events[i]
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		payload, err := decodePayload[HumanAuthorityRecordedPayload](event.Payload)
		if err != nil {
			return HumanAuthorityRecordedPayload{}, false, fmt.Errorf("event %q (%s): %w", event.ID, event.Type, err)
		}
		if payload.Request.ID != in.RequestID || payload.Operator.ID != in.Operator.ID || payload.Decision != in.Decision {
			continue
		}
		if in.Digest != "" && payload.Request.Revision != in.Digest {
			continue
		}
		if in.Action != (domain.Action{}) && payload.Action != in.Action {
			continue
		}
		return payload, true, nil
	}
	return HumanAuthorityRecordedPayload{}, false, nil
}

// humanEvidenceID is the idempotency key (§6). It is a digest of the exact
// request binding, the operator identity, and the decision - and of nothing
// else. The note is excluded on purpose: re-running the same authorization
// with different wording is the same authorization, and an annotation must
// never be able to mint a second evidence record.
//
// The journal writes it as the EVENT id, which the events table holds as its
// primary key. Deduplication is therefore the database's, not a read-modify-
// write in this process: two operator processes racing the same authorization
// produce the same id, and exactly one insert can win.
func humanEvidenceID(request Ref, operatorID, decision string) (string, error) {
	digest, err := Digest(struct {
		Request  Ref    `json:"request"`
		Operator string `json:"operator"`
		Decision string `json:"decision"`
	}{request, operatorID, decision})
	if err != nil {
		return "", err
	}
	return "authev-" + digest, nil
}

// recordHumanAuthority appends the evidence, or adopts the evidence that is
// already there. It reports whether THIS call wrote the row.
//
// The event carries no operation id and goes straight through the journal: it
// does not plan, lease, or finish an operation, because recording that a human
// answered is not an engineering side effect and must not have to take a lease
// a controller may be holding.
//
// The crash matrix this satisfies:
//
//	validated, crash before append -> nothing durable; the retry revalidates
//	    against live state and appends once.
//	appended, crash before the CLI printed -> the retry finds the id already
//	    present and adopts it. Recorded is false.
//	two processes at once -> both compute the same id; the events primary key
//	    admits one; the loser re-reads, finds it, and adopts it.
func (r *EngineeringRuntime) recordHumanAuthority(state *runState, request AuthorityRequest, in AuthorizeInput, evidenceID string) (bool, error) {
	if hasEvent(state.events, evidenceID) {
		return false, nil
	}
	payload := HumanAuthorityRecordedPayload{
		SchemaVersion: SchemaVersion,
		EvidenceID:    evidenceID,
		Request:       Ref{ID: request.ID, Revision: request.Digest},
		Operator:      in.Operator,
		Decision:      in.Decision,
		Action:        request.Action,
		Repository:    request.Repository,
		Controller:    request.Controller,
		Candidate:     request.Candidate,
		Contract:      request.Contract,
		StateSHA256:   request.StateSHA256,
		OccurredAt:    r.deps.Clock.Now(),
		Note:          in.Note,
	}
	raw, err := marshalPayloadJSON(payload)
	if err != nil {
		return false, err
	}
	_, err = r.deps.Store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            evidenceID,
		RunID:         state.run.ID,
		Type:          EventHumanAuthorityRecorded,
		OccurredAt:    payload.OccurredAt,
		Payload:       raw,
	})
	if err == nil {
		return true, nil
	}
	// The append failed. If the evidence is nonetheless present, another
	// process wrote exactly this record and this call adopts it; the error is
	// only reported when it is not. Presence is checked rather than the error
	// text, so this does not depend on a driver's message.
	events, readErr := r.deps.Store.Events(state.run.ID)
	if readErr != nil {
		return false, err
	}
	if hasEvent(events, evidenceID) {
		return false, nil
	}
	return false, err
}

func hasEvent(events []EngineeringEvent, id string) bool {
	for _, event := range events {
		if event.ID == id {
			return true
		}
	}
	return false
}
