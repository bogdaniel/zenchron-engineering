package orchestration

// Durable, authorized DecisionResolution (#508): the external-authority half
// of the DecisionRequest lifecycle #473 introduced.
//
// Two different durable facts can need a human decision, and this file keeps
// them distinct rather than collapsing them:
//
//   - a worker-authored decision_request MESSAGE (#473, communication.go) is
//     a worker ASKING a question from inside its own run. Resolving one
//     supplies the requesting unit's own next invocation a fact to read; it
//     never gates anything by itself.
//   - a WorkUnitHold (workgraph_hold.go) is placed by an operator - never a
//     worker, because there is no run yet to write one from - directly on a
//     not-yet-activated WorkGraph unit: #472's readiness-owner seam. Resolving
//     one lifts the hold; #472's existing frontier recomputation, not this
//     package, decides whether the unit is then runnable.
//
// Both resolve through the SAME DecisionResolution record and the SAME
// authority and idempotency rules: "who may answer, how a retry behaves, how
// a conflict is refused" is one fact, not two. DecisionRequestRef is the
// common normalization that lets ResolveDecision validate either source
// identically without this package needing to know which one it was given.

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// DecisionResolutionSchemaVersion versions the durable resolution record.
const DecisionResolutionSchemaVersion = "0.1"

// Outcome kinds a resolution may carry. The request's own contract prescribes
// which kind answers it; nothing here lets an actor invent a fifth kind or
// smuggle an unbounded document through one of these three.
const (
	DecisionAllowDeny      = "allow_deny"
	DecisionSelectedOption = "selected_option"
	DecisionText           = "text"
)

// The two legal values of an allow_deny outcome.
const (
	DecisionAllow = "allow"
	DecisionDeny  = "deny"
)

const (
	maxDecisionOptionBytes = 64
	maxDecisionTextBytes   = 2000
	maxDecisionReasonBytes = 2000
)

// DecisionOutcome is the bounded, typed answer. Kind fixes which shape Value
// must take, so validation - not an actor's intent - decides what counts as a
// legal answer.
type DecisionOutcome struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Validate refuses an outcome this build does not recognize, or whose value
// does not fit the shape its kind prescribes.
func (o DecisionOutcome) Validate() error {
	switch o.Kind {
	case DecisionAllowDeny:
		if o.Value != DecisionAllow && o.Value != DecisionDeny {
			return fmt.Errorf("an %s outcome must be %q or %q, not %q", DecisionAllowDeny, DecisionAllow, DecisionDeny, o.Value)
		}
		return nil
	case DecisionSelectedOption:
		return boundedDecisionText("selected_option value", o.Value, maxDecisionOptionBytes)
	case DecisionText:
		return boundedDecisionText("text value", o.Value, maxDecisionTextBytes)
	default:
		return fmt.Errorf("decision outcome kind %q is not %q, %q or %q", o.Kind, DecisionAllowDeny, DecisionSelectedOption, DecisionText)
	}
}

// AuthorityKindOperator is the only authority kind this build recognizes: the
// existing operator/control-plane identity the control endpoint already
// establishes for a plan decision (runtime.RecordedOperator), reused rather
// than restated. Identity alone - an actor name, or a socket connection by
// itself - is never authority; a resolution is accepted only through this one
// recognized kind, and anything else - including an empty one a worker's
// report could never produce in the first place - is refused, closed, rather
// than treated as probably fine.
//
// What this constant and its Validate() prove, precisely: a resolution is
// REFUSED unless something upstream already stamped AuthorityKindOperator,
// and the one caller that does so (runtime.Supervisor.ResolveDecision)
// hardcodes it - there is no field through which any caller, worker or
// otherwise, can request a different kind. What they do NOT prove: that the
// caller reaching that Supervisor method really is the operator. THAT
// guarantee is #398's control endpoint - a Unix socket, mode 0600, inside a
// mode-0700 state directory - and it is an OS file-permission fact, not
// something this package re-implements or re-authorizes. A worker's provider
// process has no path to that socket in its own sandboxed invocation; see
// runtime/control_endpoint_ownership_test.go for the first direct tests of
// that boundary's own refusal behavior, and provider_isolation.go for the
// separate, pre-existing claim about what a sandboxed provider can reach.
const AuthorityKindOperator = "operator"

// DecisionResolutionAuthority is who authorized a resolution. It is built
// from the existing operator identity mechanism and never re-derived here.
type DecisionResolutionAuthority struct {
	Actor         string `json:"actor"`
	AuthorityKind string `json:"authority_kind"`
	Provenance    string `json:"provenance"`
}

// Validate enforces law #2: identity is not authority. An actor is named, but
// accepted only under the one authority kind this build can validate.
func (a DecisionResolutionAuthority) Validate() error {
	if strings.TrimSpace(a.Actor) == "" {
		return errors.New("a decision resolution authority needs its actor")
	}
	if a.AuthorityKind != AuthorityKindOperator {
		return fmt.Errorf("decision resolution authority kind %q is not recognized; identity alone is never authority", a.AuthorityKind)
	}
	if strings.TrimSpace(a.Provenance) == "" {
		return errors.New("a decision resolution authority needs its provenance")
	}
	return nil
}

// DecisionResolution is the immutable, append-only answer to exactly one live
// request. Its identity is deterministic from RequestID alone
// (DecisionResolutionID), so one request can ever have exactly one durable
// resolution: an identical retry finds it, and a different answer finds one
// to conflict with rather than a free slot to write into.
type DecisionResolution struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	RequestID     string `json:"request_id"`
	// Scope is the request's own scope, copied rather than re-derived: the
	// batch (or work-unit hold) the request belongs to.
	Scope string `json:"scope"`
	// Subject is the request's own exact-subject binding, copied from it.
	// nil when the request carries none.
	Subject    *MessageSubject             `json:"subject,omitempty"`
	Outcome    DecisionOutcome             `json:"outcome"`
	Reason     string                      `json:"reason,omitempty"`
	Authority  DecisionResolutionAuthority `json:"authority"`
	ResolvedAt time.Time                   `json:"resolved_at"`
}

// Validate refuses a record whose identity, outcome or authority is not
// internally coherent.
func (r DecisionResolution) Validate() error {
	if r.SchemaVersion != DecisionResolutionSchemaVersion {
		return fmt.Errorf("decision resolution schema version %q is not %q", r.SchemaVersion, DecisionResolutionSchemaVersion)
	}
	id, err := DecisionResolutionID(r.RequestID)
	if err != nil {
		return err
	}
	if id != r.ID {
		return fmt.Errorf("decision resolution %s does not match the identity %s of its own request", r.ID, id)
	}
	if strings.TrimSpace(r.Scope) == "" {
		return errors.New("a decision resolution needs its scope")
	}
	if err := r.Outcome.Validate(); err != nil {
		return err
	}
	if r.Reason != "" {
		if err := boundedDecisionText("decision resolution reason", r.Reason, maxDecisionReasonBytes); err != nil {
			return err
		}
	}
	if err := r.Authority.Validate(); err != nil {
		return err
	}
	if r.ResolvedAt.IsZero() {
		return errors.New("a decision resolution needs its resolution time")
	}
	return nil
}

// DecisionResolutionID is deterministic from the request alone.
func DecisionResolutionID(requestID string) (string, error) {
	if strings.TrimSpace(requestID) == "" {
		return "", errors.New("a decision resolution needs the request it resolves")
	}
	digest, err := domain.Digest(struct {
		Request string `json:"request"`
	}{requestID})
	if err != nil {
		return "", err
	}
	return "decision-resolution-" + digest[:32], nil
}

func boundedDecisionText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes, above the %d byte bound", name, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	return nil
}

// DecisionRequestRef is one live request's facts, normalized from whichever
// durable owner holds it, so ResolveDecision validates every source the same
// way instead of each caller re-implementing liveness and subject checks.
type DecisionRequestRef struct {
	ID    string
	Scope string
	// Subject is the exact subject this request is bound to, or nil when it
	// carries none.
	Subject *MessageSubject
	// Live is false for a request that is known but no longer answerable -
	// superseded by its own author, or otherwise withdrawn. ResolveDecision
	// refuses such a request deterministically rather than answering it.
	Live bool
	// ExpectedOutcomeKind, when non-empty, is the ONLY outcome kind this
	// request's own contract permits - prescribed by the request itself, never
	// chosen by whoever answers it. A WorkUnitHold always names one: a gate
	// is inherently allow_deny, so nothing can resolve it with a bounded-text
	// or selected-option answer that was never its question.
	//
	// Empty means the request's source does not yet prescribe one. #473's
	// decision_request message carries no outcome-kind or option-set member
	// today, so a message-sourced request cannot be constrained here without
	// #473 adding one - which this package does not do on #473's behalf
	// (docs/orchestration.md names the exact boundary). Until it does,
	// ResolveDecision accepts any of the three bounded kinds for those.
	ExpectedOutcomeKind string
}

// ResolveDecision validates a proposed answer against the live request and
// the scope's CURRENT subject, and returns the resolution to persist.
//
// current is the CURRENT subject of the owner ref.Subject names, or nil when
// ref carries no subject. A subject-bound request whose owner has since moved
// to a different candidate is refused as stale even though the request itself
// is otherwise live: exact subject/revision/tree binding is checked before a
// decision is accepted, never assumed to still hold.
//
// existing is the resolution already durably stored for this request, or nil.
// An identical retry - the same outcome and reason - returns existing
// unchanged: idempotent, not a second write. A different answer to an
// already-resolved request is refused: conflicting second answers are refused,
// never silently preferred over the first.
func ResolveDecision(ref DecisionRequestRef, current *HandoffSubject, outcome DecisionOutcome, reason string,
	authority DecisionResolutionAuthority, existing *DecisionResolution, now time.Time) (DecisionResolution, error) {
	if strings.TrimSpace(ref.ID) == "" {
		return DecisionResolution{}, errors.New("unknown decision request")
	}
	if !ref.Live {
		return DecisionResolution{}, fmt.Errorf("decision request %s is no longer live (superseded or withdrawn)", ref.ID)
	}
	if ref.Subject != nil && (current == nil || *current != ref.Subject.Revision) {
		return DecisionResolution{}, fmt.Errorf(
			"decision request %s is bound to a subject that is no longer current; it cannot be resolved against a stale subject", ref.ID)
	}
	if err := outcome.Validate(); err != nil {
		return DecisionResolution{}, err
	}
	if ref.ExpectedOutcomeKind != "" && outcome.Kind != ref.ExpectedOutcomeKind {
		return DecisionResolution{}, fmt.Errorf(
			"decision request %s requires a %s outcome, not %s", ref.ID, ref.ExpectedOutcomeKind, outcome.Kind)
	}
	if reason != "" {
		if err := boundedDecisionText("decision resolution reason", reason, maxDecisionReasonBytes); err != nil {
			return DecisionResolution{}, err
		}
	}
	if err := authority.Validate(); err != nil {
		return DecisionResolution{}, err
	}
	id, err := DecisionResolutionID(ref.ID)
	if err != nil {
		return DecisionResolution{}, err
	}
	if existing != nil {
		if existing.ID != id {
			return DecisionResolution{}, fmt.Errorf("decision resolution %s does not resolve request %s", existing.ID, ref.ID)
		}
		if existing.Outcome == outcome && existing.Reason == reason {
			return *existing, nil
		}
		return DecisionResolution{}, fmt.Errorf(
			"decision request %s is already resolved as %s %q; a conflicting answer is refused", ref.ID, existing.Outcome.Kind, existing.Outcome.Value)
	}
	resolution := DecisionResolution{
		SchemaVersion: DecisionResolutionSchemaVersion, ID: id, RequestID: ref.ID, Scope: ref.Scope,
		Subject: ref.Subject, Outcome: outcome, Reason: reason, Authority: authority, ResolvedAt: now,
	}
	return resolution, resolution.Validate()
}
