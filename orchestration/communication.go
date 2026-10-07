package orchestration

// The typed communication protocol (#473).
//
// Workers coordinate through a deliberately small vocabulary instead of an
// operator relaying transcript prose between them:
//
//	EngineeringHandoff    completed responsibility -> downstream context (#470, handoff.go)
//	CollaborationRequest  a bounded request to another unit, or the response to one
//	Finding               a defect, risk or inconsistency bound to an exact admitted subject
//	DecisionRequest       a question only a human or designated authority may answer
//	StateUpdate           a compact, non-authoritative progress observation
//
// The handoff is consumed, not redesigned: a message names an admitted handoff
// as its subject, and the subject it is bound to is that handoff's
// runtime-observed one.
//
// The same split as the handoff applies. A worker writes a MessageReport - only
// the parts that are its to say - through a runtime-owned slot. The runtime
// admits each draft into an immutable EngineeringMessage whose provenance,
// route and subject come from the runtime's own records, never from the
// worker. No message grants a permission, settles a lifecycle, or answers a
// DecisionRequest: there is no member that could say so.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// MessageSchemaVersion versions both the worker document and the admitted
// record. An unrecognized version is refused rather than interpreted.
const MessageSchemaVersion = "0.1"

// MessageKind is one of the four message primitives beside the handoff.
type MessageKind string

const (
	KindCollaborationRequest MessageKind = "collaboration_request"
	KindFinding              MessageKind = "finding"
	KindDecisionRequest      MessageKind = "decision_request"
	KindStateUpdate          MessageKind = "state_update"
)

// The categories a Finding may carry.
const (
	FindingDefect        = "defect"
	FindingRisk          = "risk"
	FindingInconsistency = "inconsistency"
)

// Audience is who an admitted message is routed to. The runtime derives it
// from the kind and the subject; a worker never chooses it.
type Audience string

const (
	// AudienceUnit routes to exactly one unit (Route.Unit).
	AudienceUnit Audience = "unit"
	// AudienceAuthority routes to the human or designated authority. Nothing
	// a worker writes can answer it.
	AudienceAuthority Audience = "authority"
	// AudienceScope is a non-authoritative observation visible across the
	// scope. It is never copied per unit.
	AudienceScope Audience = "scope"
)

// Bounds. They are refusals, never truncations.
const (
	MaxMessageReportBytes = 32 << 10
	// MaxMessagesPerInvocation is also the routing fan-out of one invocation:
	// every admitted message has at most one recipient.
	MaxMessagesPerInvocation = 8
	// MaxScopeMessages bounds one scope's durable history, and so every list
	// projected from it.
	MaxScopeMessages = 256
	// MaxScopeUnits bounds the roster a scope is admitted against.
	MaxScopeUnits     = MaxBatchItems
	maxMessageBody    = 4 << 10
	maxMessagePurpose = 256
	maxMessageRef     = 128
)

// MessageReport is the document a worker writes through the runtime-owned
// message slot. Like HandoffReport it carries no run, unit, candidate or
// agent identity: the strict decoder refuses such members as unknown.
type MessageReport struct {
	SchemaVersion string         `json:"schema_version"`
	Messages      []MessageDraft `json:"messages"`
}

// MessageDraft is one worker-stated message. Which members a kind may carry
// is fixed by validateDraft; a member a kind does not use is refused, not
// ignored, so a worker cannot route a Finding or answer a decision by
// writing a member the runtime would otherwise drop.
type MessageDraft struct {
	Kind MessageKind `json:"kind"`
	// Target is the unit a CollaborationRequest is addressed to. It is the
	// only member through which a worker names a recipient.
	Target string `json:"target,omitempty"`
	// InReplyTo makes a CollaborationRequest the response to an admitted
	// request addressed to the writer's unit.
	InReplyTo string `json:"in_reply_to,omitempty"`
	// Supersedes corrects an admitted message of the same kind from the same
	// unit. The superseded record stays; history is appended, never rewritten.
	Supersedes string `json:"supersedes,omitempty"`
	// SubjectHandoff names an admitted EngineeringHandoff. The subject bound
	// is that handoff's runtime-observed candidate, never a restatement.
	SubjectHandoff string `json:"subject_handoff,omitempty"`
	Purpose        string `json:"purpose,omitempty"`
	Category       string `json:"category,omitempty"`
	Body           string `json:"body"`
}

// DecodeMessageReport strictly decodes and validates one document: exactly
// one JSON object, no unknown member, no trailing data, within every bound.
func DecodeMessageReport(document []byte) (MessageReport, error) {
	if len(document) > MaxMessageReportBytes {
		return MessageReport{}, fmt.Errorf("message report is %d bytes, above the %d byte bound", len(document), MaxMessageReportBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(document))
	decoder.DisallowUnknownFields()
	var report MessageReport
	if err := decoder.Decode(&report); err != nil {
		return MessageReport{}, fmt.Errorf("message report is not a valid document: %w", err)
	}
	if len(bytes.TrimSpace(document[decoder.InputOffset():])) != 0 {
		return MessageReport{}, errors.New("message report carries trailing data after its JSON value")
	}
	return report, report.Validate()
}

// Validate refuses a report this build cannot act on. An absent slot is how
// a worker says nothing, so an empty report is refused rather than admitted
// as a second way to say it.
func (r MessageReport) Validate() error {
	if r.SchemaVersion != MessageSchemaVersion {
		return fmt.Errorf("message schema version %q is not %q", r.SchemaVersion, MessageSchemaVersion)
	}
	if len(r.Messages) == 0 || len(r.Messages) > MaxMessagesPerInvocation {
		return fmt.Errorf("a message report carries %d messages, not 1 to %d", len(r.Messages), MaxMessagesPerInvocation)
	}
	for i, draft := range r.Messages {
		if err := validateDraft(draft); err != nil {
			return fmt.Errorf("message %d: %w", i, err)
		}
	}
	return nil
}

// draftShape is which optional members a kind requires (true) or permits
// (false); a member absent from the map is forbidden.
var draftShape = map[MessageKind]map[string]bool{
	KindCollaborationRequest: {"target": true, "purpose": true, "in_reply_to": false, "subject_handoff": false},
	KindFinding:              {"subject_handoff": true, "category": true},
	KindDecisionRequest:      {"purpose": true, "subject_handoff": false},
	KindStateUpdate:          {},
}

func validateDraft(d MessageDraft) error {
	shape, known := draftShape[d.Kind]
	if !known {
		return fmt.Errorf("message kind %q is not a recognized kind", d.Kind)
	}
	if err := messageText("body", d.Body, maxMessageBody); err != nil {
		return err
	}
	members := map[string]string{
		"target": d.Target, "purpose": d.Purpose, "in_reply_to": d.InReplyTo,
		"subject_handoff": d.SubjectHandoff, "category": d.Category,
	}
	for name, value := range members {
		required, allowed := shape[name]
		switch {
		case value == "" && required:
			return fmt.Errorf("a %s requires %s", d.Kind, name)
		case value != "" && !allowed:
			return fmt.Errorf("a %s may not carry %s", d.Kind, name)
		}
	}
	for name, value := range map[string]string{"target": d.Target, "in_reply_to": d.InReplyTo, "supersedes": d.Supersedes, "subject_handoff": d.SubjectHandoff} {
		if value != "" {
			if err := messageText(name, value, maxMessageRef); err != nil {
				return err
			}
		}
	}
	if d.Purpose != "" {
		if err := messageText("purpose", d.Purpose, maxMessagePurpose); err != nil {
			return err
		}
	}
	if d.Category != "" && d.Category != FindingDefect && d.Category != FindingRisk && d.Category != FindingInconsistency {
		return fmt.Errorf("finding category %q must be %q, %q or %q", d.Category, FindingDefect, FindingRisk, FindingInconsistency)
	}
	return nil
}

func messageText(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("message %s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("message %s is %d bytes, above the %d byte bound", name, len(value), limit)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("message %s is not valid UTF-8", name)
	}
	return nil
}

// EngineeringMessage is the durable, immutable record of one admitted
// message. Everything except Purpose, Category and Body is runtime-owned.
type EngineeringMessage struct {
	SchemaVersion string          `json:"schema_version"`
	ID            string          `json:"id"`
	Scope         string          `json:"scope"`
	Kind          MessageKind     `json:"kind"`
	Source        MessageSource   `json:"source"`
	Route         MessageRoute    `json:"route"`
	Subject       *MessageSubject `json:"subject,omitempty"`
	InReplyTo     string          `json:"in_reply_to,omitempty"`
	Supersedes    string          `json:"supersedes,omitempty"`
	Purpose       string          `json:"purpose,omitempty"`
	Category      string          `json:"category,omitempty"`
	Body          string          `json:"body"`
	// DocumentSHA256 is the digest of the exact slot document journalled when
	// the producing invocation completed.
	DocumentSHA256 string    `json:"document_sha256"`
	AdmittedAt     time.Time `json:"admitted_at"`
}

// MessageSource is the exact invocation that wrote a message, as the runtime
// recorded it. Index is the draft's position in that invocation's report.
type MessageSource struct {
	Unit        string `json:"unit"`
	RunID       string `json:"run_id"`
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	Index       int    `json:"index"`
}

// MessageRoute is the runtime-derived recipient.
type MessageRoute struct {
	Audience Audience `json:"audience"`
	Unit     string   `json:"unit,omitempty"`
}

// MessageSubject is the exact subject a message is bound to: an admitted
// handoff, the unit that owns it, and the candidate the runtime observed.
type MessageSubject struct {
	Handoff  string         `json:"handoff"`
	Owner    string         `json:"owner"`
	Revision HandoffSubject `json:"revision"`
}

// Validate refuses a record missing any runtime-owned binding, or one whose
// route contradicts its kind.
func (m EngineeringMessage) Validate() error {
	if m.SchemaVersion != MessageSchemaVersion {
		return fmt.Errorf("engineering message schema version %q is not %q", m.SchemaVersion, MessageSchemaVersion)
	}
	for name, value := range map[string]string{
		"id": m.ID, "scope": m.Scope, "source.unit": m.Source.Unit, "source.run_id": m.Source.RunID,
		"source.agent_id": m.Source.AgentID, "source.operation_id": m.Source.OperationID, "document_sha256": m.DocumentSHA256,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("engineering message %s is required", name)
		}
	}
	if m.Source.Attempt <= 0 || m.Source.Index < 0 || m.AdmittedAt.IsZero() {
		return errors.New("engineering message attempt, index and admission time must be valid")
	}
	if route := routeOf(m.Kind, m.Target(), m.Subject); route != m.Route {
		return fmt.Errorf("engineering message route %+v is not the route of a %s", m.Route, m.Kind)
	}
	return validateDraft(m.draft())
}

// Target is the unit a CollaborationRequest names, read back from its route.
func (m EngineeringMessage) Target() string {
	if m.Kind != KindCollaborationRequest {
		return ""
	}
	return m.Route.Unit
}

func (m EngineeringMessage) draft() MessageDraft {
	draft := MessageDraft{Kind: m.Kind, Target: m.Target(), InReplyTo: m.InReplyTo, Supersedes: m.Supersedes,
		Purpose: m.Purpose, Category: m.Category, Body: m.Body}
	if m.Subject != nil {
		draft.SubjectHandoff = m.Subject.Handoff
	}
	return draft
}

// routeOf is the one routing rule. A Finding goes to the owner of the subject
// it is bound to - deterministically, and never where its writer pointed it.
func routeOf(kind MessageKind, target string, subject *MessageSubject) MessageRoute {
	switch kind {
	case KindCollaborationRequest:
		return MessageRoute{Audience: AudienceUnit, Unit: target}
	case KindFinding:
		if subject == nil {
			return MessageRoute{}
		}
		return MessageRoute{Audience: AudienceUnit, Unit: subject.Owner}
	case KindDecisionRequest:
		return MessageRoute{Audience: AudienceAuthority}
	case KindStateUpdate:
		return MessageRoute{Audience: AudienceScope}
	}
	return MessageRoute{}
}

// AppliesTo reports whether a message bound to a subject applies to this
// exact one. A Finding about an older candidate never silently carries over
// to a newer one: different revision, different answer.
func (m EngineeringMessage) AppliesTo(current HandoffSubject) bool {
	return m.Subject != nil && m.Subject.Revision == current
}

// MessageID is the identity of the draft at index of one exact invocation's
// report. Replaying the same invocation yields the same identities.
func MessageID(runID, operationID string, attempt, index int) (string, error) {
	if runID == "" || operationID == "" || attempt <= 0 || index < 0 {
		return "", errors.New("a message identity needs the run, operation, physical attempt and index")
	}
	digest, err := domain.Digest(struct {
		Run       string `json:"run"`
		Operation string `json:"operation"`
		Attempt   int    `json:"attempt"`
		Index     int    `json:"index"`
	}{runID, operationID, attempt, index})
	if err != nil {
		return "", err
	}
	return "message-" + digest[:32], nil
}
