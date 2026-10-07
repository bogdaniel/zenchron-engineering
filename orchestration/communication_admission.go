package orchestration

import (
	"fmt"
	"time"
)

// BatchItemUnit is the unit name of one #470 batch item within its batch's
// message scope.
func BatchItemUnit(issue int) string { return fmt.Sprintf("issue-%d", issue) }

// MessageScope is everything a report is admitted against. The runtime
// supplies all of it from its own durable records: the units of one scope
// (a batch today, a WorkGraph when #472 provides one), the subjects of their
// admitted handoffs, and the messages already admitted there.
type MessageScope struct {
	ID    string
	Units []string
	// Subjects maps an admitted handoff's identity to the subject it binds.
	Subjects map[string]MessageSubject
	// Admitted is the scope's message history in admission order.
	Admitted []EngineeringMessage
}

// AdmitMessages turns one invocation's report into admitted records, or
// refuses the whole report. source is the runtime's record of the invocation;
// its Index is assigned per draft. Nothing the worker wrote is used as
// identity, provenance, route or subject.
func AdmitMessages(report MessageReport, digest string, scope MessageScope, source MessageSource, now time.Time) ([]EngineeringMessage, error) {
	if err := report.Validate(); err != nil {
		return nil, err
	}
	if len(scope.Units) > MaxScopeUnits {
		return nil, fmt.Errorf("scope %s names %d units, above the %d unit bound", scope.ID, len(scope.Units), MaxScopeUnits)
	}
	units := make(map[string]bool, len(scope.Units))
	for _, unit := range scope.Units {
		units[unit] = true
	}
	if !units[source.Unit] {
		return nil, fmt.Errorf("unit %q is not a unit of scope %s", source.Unit, scope.ID)
	}
	if total := len(scope.Admitted) + len(report.Messages); total > MaxScopeMessages {
		return nil, fmt.Errorf("scope %s would hold %d messages, above the %d message bound", scope.ID, total, MaxScopeMessages)
	}
	prior := make(map[string]EngineeringMessage, len(scope.Admitted))
	superseded := map[string]bool{}
	for _, message := range scope.Admitted {
		prior[message.ID] = message
		superseded[message.Supersedes] = true
	}
	admitted := make([]EngineeringMessage, 0, len(report.Messages))
	for i, draft := range report.Messages {
		message, err := admitDraft(draft, i, digest, scope, source, units, prior, superseded, now)
		if err != nil {
			return nil, fmt.Errorf("message %d: %w", i, err)
		}
		if message.Supersedes != "" && superseded[message.Supersedes] {
			return nil, fmt.Errorf("message %d: %s is already superseded; correct the latest record instead", i, message.Supersedes)
		}
		superseded[message.Supersedes] = true
		admitted = append(admitted, message)
	}
	return admitted, nil
}

func admitDraft(draft MessageDraft, index int, digest string, scope MessageScope, source MessageSource, units map[string]bool, prior map[string]EngineeringMessage, superseded map[string]bool, now time.Time) (EngineeringMessage, error) {
	id, err := MessageID(source.RunID, source.OperationID, source.Attempt, index)
	if err != nil {
		return EngineeringMessage{}, err
	}
	source.Index = index
	message := EngineeringMessage{
		SchemaVersion: MessageSchemaVersion, ID: id, Scope: scope.ID, Kind: draft.Kind, Source: source,
		InReplyTo: draft.InReplyTo, Supersedes: draft.Supersedes, Purpose: draft.Purpose,
		Category: draft.Category, Body: draft.Body, DocumentSHA256: digest, AdmittedAt: now,
	}
	if draft.SubjectHandoff != "" {
		subject, ok := scope.Subjects[draft.SubjectHandoff]
		if !ok {
			return EngineeringMessage{}, fmt.Errorf("subject %s is not an admitted handoff of scope %s", draft.SubjectHandoff, scope.ID)
		}
		message.Subject = &subject
	}
	if draft.Target != "" && (!units[draft.Target] || draft.Target == source.Unit) {
		return EngineeringMessage{}, fmt.Errorf("target %q is not another unit of scope %s", draft.Target, scope.ID)
	}
	if draft.InReplyTo != "" {
		request, ok := prior[draft.InReplyTo]
		// Only a collaboration request addressed to this unit can be answered
		// by it. A DecisionRequest is answered by authority, never by a reply.
		if !ok || request.Kind != KindCollaborationRequest || request.Route.Unit != source.Unit {
			return EngineeringMessage{}, fmt.Errorf("%s is not an admitted collaboration request addressed to %s", draft.InReplyTo, source.Unit)
		}
		// A superseded request is history: its author replaced it, so a
		// response to it would answer a question nobody is asking any more.
		if superseded[draft.InReplyTo] {
			return EngineeringMessage{}, fmt.Errorf("%s was superseded; answer the request that replaced it", draft.InReplyTo)
		}
		if draft.Target != request.Source.Unit {
			return EngineeringMessage{}, fmt.Errorf("a response to %s goes to its requester %s, not %s", draft.InReplyTo, request.Source.Unit, draft.Target)
		}
	}
	if draft.Supersedes != "" {
		corrected, ok := prior[draft.Supersedes]
		if !ok || corrected.Kind != draft.Kind || corrected.Source.Unit != source.Unit {
			return EngineeringMessage{}, fmt.Errorf("%s is not an admitted %s from %s", draft.Supersedes, draft.Kind, source.Unit)
		}
	}
	message.Route = routeOf(draft.Kind, draft.Target, message.Subject)
	return message, message.Validate()
}

// Inbox is what one unit is shown of its scope: the live messages routed to
// it, and the latest observation of every other unit. Superseded records are
// history, not inbox entries.
type Inbox struct {
	// Requests are collaboration requests and responses addressed to the unit.
	Requests []EngineeringMessage `json:"requests,omitempty"`
	// Findings are bound to the unit's CURRENT subject.
	Findings []EngineeringMessage `json:"findings,omitempty"`
	// StaleFindings are bound to a subject the unit has since replaced. They
	// are shown as history and do not apply to the current subject.
	StaleFindings []EngineeringMessage `json:"stale_findings,omitempty"`
	Updates       []EngineeringMessage `json:"updates,omitempty"`
	// Omitted counts live entries left out by the per-list bound, oldest first.
	Omitted int `json:"omitted,omitempty"`
}

// maxInboxEntries bounds each inbox list a worker is shown.
const maxInboxEntries = 16

// InboxFor projects one unit's inbox. current is the unit's latest admitted
// handoff subject, or nil when it has none; a Finding applies only to that
// exact subject.
func InboxFor(unit string, messages []EngineeringMessage, current *HandoffSubject) Inbox {
	var inbox Inbox
	latestUpdate := map[string]int{}
	for _, message := range Live(messages) {
		switch {
		case message.Kind == KindStateUpdate && message.Source.Unit != unit:
			if at, seen := latestUpdate[message.Source.Unit]; seen {
				inbox.Updates[at] = message
				continue
			}
			latestUpdate[message.Source.Unit] = len(inbox.Updates)
			inbox.Updates = append(inbox.Updates, message)
		case message.Route.Audience != AudienceUnit || message.Route.Unit != unit:
		case message.Kind == KindFinding && (current == nil || !message.AppliesTo(*current)):
			inbox.StaleFindings = append(inbox.StaleFindings, message)
		case message.Kind == KindFinding:
			inbox.Findings = append(inbox.Findings, message)
		default:
			inbox.Requests = append(inbox.Requests, message)
		}
	}
	inbox.Requests = inbox.latest(inbox.Requests)
	inbox.Findings = inbox.latest(inbox.Findings)
	inbox.StaleFindings = inbox.latest(inbox.StaleFindings)
	return inbox
}

func (i *Inbox) latest(messages []EngineeringMessage) []EngineeringMessage {
	if len(messages) <= maxInboxEntries {
		return messages
	}
	i.Omitted += len(messages) - maxInboxEntries
	return messages[len(messages)-maxInboxEntries:]
}

// Live drops every superseded record, keeping admission order.
func Live(messages []EngineeringMessage) []EngineeringMessage {
	superseded := make(map[string]bool, len(messages))
	for _, message := range messages {
		superseded[message.Supersedes] = true
	}
	live := make([]EngineeringMessage, 0, len(messages))
	for _, message := range messages {
		if !superseded[message.ID] {
			live = append(live, message)
		}
	}
	return live
}

// OpenDecisions are the live DecisionRequests of a scope: each is a typed
// wait on human or designated authority. Nothing a worker can write closes
// one - no member answers it, a reply to it is refused, and a StateUpdate
// claiming it was decided changes nothing.
func OpenDecisions(messages []EngineeringMessage) []EngineeringMessage {
	var open []EngineeringMessage
	for _, message := range Live(messages) {
		if message.Kind == KindDecisionRequest {
			open = append(open, message)
		}
	}
	return open
}
