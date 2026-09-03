// Package runtime implements the local, deterministic operational layer above
// the authorization kernel. It intentionally contains no provider authority.
package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gowebpki/jcs"
)

const SchemaVersion = "0.1"

// Run-mode exits are stable before the live GitHub runner is introduced.
const (
	ExitCompleted = 0
	ExitWaiting   = 10
	ExitFailed    = 11
	ExitCancelled = 12
	ExitInvalid   = 64
)

type Disposition string

const (
	Active    Disposition = "active"
	Waiting   Disposition = "waiting"
	Completed Disposition = "completed"
	Failed    Disposition = "failed"
	Cancelled Disposition = "cancelled"
)

type Phase string

const (
	Contract  Phase = "contract"
	Execute   Phase = "execute"
	Observe   Phase = "observe"
	Assure    Phase = "assure"
	Authorize Phase = "authorize"
	Remediate Phase = "remediate"
	Publish   Phase = "publish"
)

type OperationState string

const (
	Pending            OperationState = "pending"
	Leased             OperationState = "leased"
	Running            OperationState = "running"
	Succeeded          OperationState = "succeeded"
	OperationFailed    OperationState = "failed"
	OperationCancelled OperationState = "cancelled"
	Unknown            OperationState = "unknown"
)

const (
	EventRunCreated          = "run.created"
	EventRunWaiting          = "run.waiting"
	EventRunCompleted        = "run.completed"
	EventRunFailed           = "run.failed"
	EventRunCancelled        = "run.cancelled"
	EventSourceIntentChanged = "source.intent_changed"
	EventSourceOptInRemoved  = "source.opt_in_removed"
	EventSourceOptInRestored = "source.opt_in_restored"
	EventOperationPlanned    = "operation.planned"
	EventOperationBefore     = "operation.before"
	EventOperationAfter      = "operation.after"
	EventCandidateChanged    = "candidate.changed"
	EventCandidateCommitted  = "candidate.committed"
	// EventCandidateCheckpointed is a runtime-owned commit of work an
	// interrupted producer left behind. It is deliberately NOT
	// candidate.committed: every reader of that event treats it as an
	// execution-complete, assurance-eligible candidate, and a checkpoint is
	// neither. See RunProjection.CandidateComplete.
	EventCandidateCheckpointed = "candidate.checkpointed"
	// EventExecutionCompleted records that a producer invocation ended by
	// finishing, rather than by running out of a bound. It is a producer
	// COMPLETION OBSERVATION and nothing more: it asserts no acceptance, no
	// evidence and no authority.
	EventExecutionCompleted       = "execution.completed"
	EventCandidateBaseIntegrated  = "candidate.base_integrated"
	EventCandidateExternalChanged = "candidate.external_changed"
	EventContractCompiled         = "contract.compiled"
	EventReassessmentCompleted    = "reassessment.completed"
	EventAssuranceObserved        = "assurance.observed"
	// EventSemanticAssuranceObserved is the INDEPENDENT semantic verifier's own
	// observation. It is a distinct event because it is a distinct producer
	// answering a distinct question: every reader that means "the automated
	// verifier judged this tree" keeps meaning exactly that.
	EventSemanticAssuranceObserved = "assurance.semantic_observed"
	EventAuthorityEvaluated        = "authority.evaluated"
	EventGitHubCIObserved          = "github.ci_observed"
	EventGitHubReviewObserved      = "github.review_observed"
	EventGitHubPRObserved          = "github.pr_observed"
	EventHumanAuthorityRecorded    = "human.authority_recorded"
)

var eventTypes = map[string]bool{EventRunCreated: true, EventRunWaiting: true, EventRunCompleted: true, EventRunFailed: true, EventRunCancelled: true, EventSourceIntentChanged: true, EventSourceOptInRemoved: true, EventSourceOptInRestored: true, EventOperationPlanned: true, EventOperationBefore: true, EventOperationAfter: true, EventCandidateChanged: true, EventCandidateCommitted: true, EventCandidateCheckpointed: true, EventExecutionCompleted: true, EventCandidateBaseIntegrated: true, EventCandidateExternalChanged: true, EventContractCompiled: true, EventReassessmentCompleted: true, EventAssuranceObserved: true, EventSemanticAssuranceObserved: true, EventAuthorityEvaluated: true, EventGitHubCIObserved: true, EventGitHubReviewObserved: true, EventGitHubPRObserved: true, EventHumanAuthorityRecorded: true}

type Ref struct {
	ID       string `json:"id"`
	Revision string `json:"revision"`
}
type Candidate struct {
	Branch   string `json:"branch"`
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}
type Cursor struct {
	LastSequence  int64  `json:"last_sequence"`
	LastEventID   string `json:"last_event_id"`
	LastEventHash string `json:"last_event_hash,omitempty"`
}
type Artifact struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	MediaType   string `json:"media_type"`
	LocalOnly   bool   `json:"local_only"`
	Sanitized   bool   `json:"sanitized"`
	Publishable bool   `json:"publishable"`
}
type EngineeringRun struct {
	SchemaVersion    string      `json:"schema_version"`
	ID               string      `json:"id"`
	Repository       string      `json:"repository"`
	Goal             string      `json:"goal"`
	Phase            Phase       `json:"phase"`
	Disposition      Disposition `json:"disposition"`
	Reason           string      `json:"reason,omitempty"`
	Base             Ref         `json:"base"`
	Candidate        Candidate   `json:"candidate"`
	Contract         Ref         `json:"contract"`
	ControllerSHA256 string      `json:"controller_sha256"`
	// Budgets are the bounds this run is judged under, captured when it was
	// created. A run created before this field existed has the zero value, and
	// runState.continuationLimit reads that absence as the legacy rule rather
	// than as today's configuration.
	Budgets   RunBudgets `json:"budgets,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	Cursor    Cursor     `json:"journal_cursor"`
}
type RunOperation struct {
	SchemaVersion    string          `json:"schema_version"`
	ID               string          `json:"id"`
	RunID            string          `json:"run_id"`
	Kind             string          `json:"kind"`
	IdempotencyKey   string          `json:"idempotency_key"`
	State            OperationState  `json:"state"`
	Attempt          int             `json:"attempt"`
	MaxAttempts      int             `json:"max_attempts"`
	DependsOn        []string        `json:"depends_on,omitempty"`
	InputStateSHA256 string          `json:"input_state_sha256"`
	Lease            *Lease          `json:"lease,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	CreatedAt        time.Time       `json:"created_at,omitempty"`
	StartedAt        *time.Time      `json:"started_at,omitempty"`
	LastProgressAt   *time.Time      `json:"last_progress_at,omitempty"`
	WallBudget       time.Duration   `json:"wall_budget,omitempty"`
	NoProgressBudget time.Duration   `json:"no_progress_budget,omitempty"`
	NoProgressKey    string          `json:"no_progress_key,omitempty"`
	CancelRequested  bool            `json:"cancel_requested,omitempty"`
}
type Lease struct {
	Owner       string    `json:"owner"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}
type EngineeringEvent struct {
	SchemaVersion     string          `json:"schema_version"`
	ID                string          `json:"id"`
	RunID             string          `json:"run_id"`
	Sequence          int64           `json:"sequence"`
	Type              string          `json:"type"`
	OccurredAt        time.Time       `json:"occurred_at"`
	OperationID       string          `json:"operation_id,omitempty"`
	PreviousEventID   string          `json:"previous_event_id,omitempty"`
	PreviousEventHash string          `json:"previous_event_hash,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
	Artifacts         []Artifact      `json:"artifacts,omitempty"`
	StateBefore       string          `json:"state_before,omitempty"`
	StateAfter        string          `json:"state_after,omitempty"`
	EventHash         string          `json:"event_hash,omitempty"`
}
type RunSnapshot struct {
	EngineeringRun
	Operations  map[string]RunOperation `json:"operations"`
	Artifacts   []Artifact              `json:"artifacts,omitempty"`
	StateSHA256 string                  `json:"state_sha256"`
}

// CanonicalJSON serializes a typed runtime value to JSON, then applies RFC 8785
// JSON Canonicalization Scheme (JCS). encoding/json only creates the input JSON;
// it is not itself a canonical serializer.
func CanonicalJSON(v any) ([]byte, error) {
	if err := validateIJSONValue(reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jcs.Transform(raw)
}

const maxSafeInteger = int64(1<<53 - 1)

// validateIJSONValue rejects Go values whose JSON representation would silently
// replace invalid string data or lose integer precision before JCS sees it.
// RawMessage remains JSON input: JCS then rejects duplicate names and invalid
// JSON-number representations rather than falling back to encoding/json output.
func validateIJSONValue(v reflect.Value) error {
	if !v.IsValid() {
		return nil
	}
	if v.Type() == reflect.TypeFor[json.RawMessage]() {
		raw := v.Bytes()
		if raw == nil {
			return nil
		}
		if !utf8.Valid(raw) || !json.Valid(raw) {
			return fmt.Errorf("invalid JSON raw message")
		}
		return nil
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return nil
		}
		return validateIJSONValue(v.Elem())
	case reflect.String:
		if !utf8.ValidString(v.String()) {
			return fmt.Errorf("invalid UTF-8 string")
		}
	case reflect.Float32, reflect.Float64:
		if f := v.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("non-finite JSON number")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if n := v.Int(); n < -maxSafeInteger || n > maxSafeInteger {
			return fmt.Errorf("integer %d exceeds the I-JSON interoperable range", n)
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		if n := v.Uint(); n > uint64(maxSafeInteger) {
			return fmt.Errorf("integer %d exceeds the I-JSON interoperable range", n)
		}
	case reflect.Array, reflect.Slice:
		for i := 0; i < v.Len(); i++ {
			if err := validateIJSONValue(v.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			if err := validateIJSONValue(iter.Key()); err != nil {
				return err
			}
			if err := validateIJSONValue(iter.Value()); err != nil {
				return err
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath != "" {
				continue
			}
			if err := validateIJSONValue(v.Field(i)); err != nil {
				return err
			}
		}
	}
	return nil
}
func Digest(v any) (string, error) {
	b, e := CanonicalJSON(v)
	if e != nil {
		return "", e
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}
func StateDigest(s RunSnapshot) (string, error) {
	s.StateSHA256 = ""
	s.Cursor = Cursor{}
	return Digest(s)
}
func EventDigest(e EngineeringEvent) (string, error) { e.EventHash = ""; return Digest(e) }
func ValidateArtifact(a Artifact) error {
	if !a.Sanitized && !a.LocalOnly {
		return fmt.Errorf("raw artifact %q must be local-only", a.Path)
	}
	if a.Publishable && !a.Sanitized {
		return fmt.Errorf("raw artifact %q is not publishable", a.Path)
	}
	return nil
}
func Reduce(run EngineeringRun, events []EngineeringEvent) (RunSnapshot, error) {
	s := RunSnapshot{EngineeringRun: run, Operations: map[string]RunOperation{}}
	var prev EngineeringEvent
	for i, e := range events {
		if !eventTypes[e.Type] {
			return s, fmt.Errorf("unknown event type %q", e.Type)
		}
		if e.RunID != run.ID || e.Sequence != int64(i+1) {
			return s, fmt.Errorf("invalid event sequence")
		}
		if i > 0 && (e.PreviousEventID != prev.ID || e.PreviousEventHash != prev.EventHash) {
			return s, fmt.Errorf("broken event chain")
		}
		h, err := EventDigest(e)
		if err != nil || (e.EventHash != "" && e.EventHash != h) {
			return s, fmt.Errorf("invalid event hash")
		}
		e.EventHash = h
		for _, a := range e.Artifacts {
			if err := ValidateArtifact(a); err != nil {
				return s, err
			}
			s.Artifacts = append(s.Artifacts, a)
		}
		if e.Type == EventOperationPlanned || e.Type == EventOperationBefore || e.Type == EventOperationAfter {
			var operation RunOperation
			if err := json.Unmarshal(e.Payload, &operation); err != nil || operation.ID == "" || operation.RunID != run.ID {
				return s, fmt.Errorf("invalid operation lifecycle payload")
			}
			if e.OperationID != "" && e.OperationID != operation.ID {
				return s, fmt.Errorf("operation event identity mismatch")
			}
			s.Operations[operation.ID] = operation
		}
		if e.Type == EventRunWaiting {
			s.Disposition = Waiting
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunCompleted {
			s.Disposition = Completed
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunFailed {
			s.Disposition = Failed
			s.Reason = payloadReason(e.Payload)
		}
		if e.Type == EventRunCancelled {
			s.Disposition = Cancelled
			s.Reason = payloadReason(e.Payload)
		}
		s.Cursor = Cursor{e.Sequence, e.ID, e.EventHash}
		prev = e
	}
	d, err := StateDigest(s)
	s.StateSHA256 = d
	return s, err
}
func payloadReason(raw json.RawMessage) string {
	var v struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &v)
	return v.Reason
}
func StableOperationKey(runID, kind string, bindings ...string) string {
	sort.Strings(bindings)
	return strings.Join(append([]string{runID, kind}, bindings...), ":")
}
func CanAcquire(op RunOperation, now time.Time, ownerAlive bool) bool {
	if op.State == Succeeded || op.State == OperationCancelled {
		return false
	}
	return op.Lease == nil || (!ownerAlive && !now.Before(op.Lease.ExpiresAt))
}

// OperationElapsed reports elapsed time only for an actively started operation.
func OperationElapsed(op RunOperation, now time.Time) time.Duration {
	if op.StartedAt == nil || now.Before(*op.StartedAt) {
		return 0
	}
	return now.Sub(*op.StartedAt)
}

// NoProgressExceeded is deliberately separate from wall time: a heartbeat can
// prove liveness while its unchanged progress fingerprint still exhausts the
// operation's forward-progress budget.
func NoProgressExceeded(op RunOperation, now time.Time) bool {
	return op.NoProgressBudget > 0 && op.LastProgressAt != nil && !now.Before(*op.LastProgressAt) && now.Sub(*op.LastProgressAt) > op.NoProgressBudget
}
