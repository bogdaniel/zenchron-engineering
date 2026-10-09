package orchestration

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

var commNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func subjectAt(owner, handoff, revision string) MessageSubject {
	return MessageSubject{Handoff: handoff, Owner: owner, Revision: HandoffSubject{BaseRevision: "base", CandidateRevision: revision, CandidateTree: "tree-" + revision}}
}

func commScope(admitted ...EngineeringMessage) MessageScope {
	return MessageScope{
		ID: "batch-1", Units: []string{"issue-1", "issue-2", "issue-3"},
		Subjects: map[string]MessageSubject{
			"handoff-b1": subjectAt("issue-2", "handoff-b1", "c1"),
			"handoff-b2": subjectAt("issue-2", "handoff-b2", "c2"),
		},
		Admitted: admitted,
	}
}

func source(unit, run string, attempt int) MessageSource {
	return MessageSource{Unit: unit, RunID: run, AgentID: "claude", OperationID: "op-" + run, Attempt: attempt}
}

func admit(t *testing.T, scope MessageScope, from MessageSource, drafts ...MessageDraft) []EngineeringMessage {
	t.Helper()
	messages, err := AdmitMessages(MessageReport{SchemaVersion: MessageSchemaVersion, Messages: drafts}, "sha", scope, from, commNow)
	if err != nil {
		t.Fatal(err)
	}
	return messages
}

func refused(t *testing.T, scope MessageScope, from MessageSource, want string, drafts ...MessageDraft) {
	t.Helper()
	_, err := AdmitMessages(MessageReport{SchemaVersion: MessageSchemaVersion, Messages: drafts}, "sha", scope, from, commNow)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("admission error = %v, want one containing %q", err, want)
	}
}

func request(target, body string) MessageDraft {
	return MessageDraft{Kind: KindCollaborationRequest, Target: target, Purpose: "interface", Body: body}
}

// Malformed, unknown, oversized and over-bound documents are refused at the
// decoder, before anything is admitted.
func TestAMessageDocumentIsStrict(t *testing.T) {
	valid := `{"schema_version":"0.1","messages":[{"kind":"state_update","body":"halfway"}]}`
	if _, err := DecodeMessageReport([]byte(valid)); err != nil {
		t.Fatalf("a valid document was refused: %v", err)
	}
	many := make([]string, MaxMessagesPerInvocation+1)
	for i := range many {
		many[i] = `{"kind":"state_update","body":"x"}`
	}
	for name, document := range map[string]string{
		"oversized":            `{"schema_version":"0.1","messages":[{"kind":"state_update","body":"` + strings.Repeat("x", MaxMessageReportBytes) + `"}]}`,
		"unknown version":      `{"schema_version":"0.2","messages":[{"kind":"state_update","body":"x"}]}`,
		"unknown kind":         `{"schema_version":"0.1","messages":[{"kind":"chat","body":"x"}]}`,
		"unknown member":       `{"schema_version":"0.1","messages":[{"kind":"state_update","body":"x","candidate":"abc123"}]}`,
		"unknown top member":   `{"schema_version":"0.1","run_id":"run-1","messages":[{"kind":"state_update","body":"x"}]}`,
		"trailing data":        valid + `{}`,
		"empty":                `{"schema_version":"0.1","messages":[]}`,
		"fan-out above bound":  `{"schema_version":"0.1","messages":[` + strings.Join(many, ",") + `]}`,
		"body above bound":     `{"schema_version":"0.1","messages":[{"kind":"state_update","body":"` + strings.Repeat("x", maxMessageBody+1) + `"}]}`,
		"routed finding":       `{"schema_version":"0.1","messages":[{"kind":"finding","target":"issue-1","subject_handoff":"h","category":"defect","body":"x"}]}`,
		"finding sans subject": `{"schema_version":"0.1","messages":[{"kind":"finding","category":"defect","body":"x"}]}`,
		"bad category":         `{"schema_version":"0.1","messages":[{"kind":"finding","subject_handoff":"h","category":"nit","body":"x"}]}`,
		"lifecycle update":     `{"schema_version":"0.1","messages":[{"kind":"state_update","purpose":"complete","body":"90% complete"}]}`,
		"answered decision":    `{"schema_version":"0.1","messages":[{"kind":"decision_request","purpose":"q","in_reply_to":"message-1","body":"x"}]}`,
		"untargeted request":   `{"schema_version":"0.1","messages":[{"kind":"collaboration_request","purpose":"p","body":"x"}]}`,
	} {
		if _, err := DecodeMessageReport([]byte(document)); err == nil {
			t.Errorf("%s: document was accepted", name)
		}
	}
}

// Provenance, route and subject come from the runtime, and a Finding goes to
// the owner of its subject whatever its writer intended.
func TestAdmissionBindsRuntimeFactsAndRoutesFindingsToTheirOwner(t *testing.T) {
	from := source("issue-1", "run-a", 2)
	messages := admit(t, commScope(), from,
		request("issue-3", "need the schema"),
		MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-b2", Category: FindingDefect, Body: "nil map write"})
	for i, message := range messages {
		want, _ := MessageID("run-a", "op-run-a", 2, i)
		if message.ID != want || message.Source.RunID != "run-a" || message.Source.Attempt != 2 || message.Source.Index != i || message.Scope != "batch-1" {
			t.Fatalf("message %d provenance = %+v", i, message)
		}
	}
	if messages[0].Route != (MessageRoute{Audience: AudienceUnit, Unit: "issue-3"}) {
		t.Fatalf("request route = %+v", messages[0].Route)
	}
	if messages[1].Route != (MessageRoute{Audience: AudienceUnit, Unit: "issue-2"}) || messages[1].Subject.Revision.CandidateRevision != "c2" {
		t.Fatalf("finding route %+v subject %+v, want the owner issue-2 and its exact candidate", messages[1].Route, messages[1].Subject)
	}
	// Replaying the same invocation yields the same identities.
	again := admit(t, commScope(), from, request("issue-3", "need the schema"),
		MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-b2", Category: FindingDefect, Body: "nil map write"})
	if again[0].ID != messages[0].ID || again[1].ID != messages[1].ID {
		t.Fatal("replaying one invocation produced new identities")
	}
	refused(t, commScope(), from, "not another unit", request("issue-9", "x"))
	refused(t, commScope(), from, "not another unit", request("issue-1", "x"))
	refused(t, commScope(), from, "not an admitted handoff", MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-zz", Category: FindingRisk, Body: "x"})
	refused(t, commScope(), source("issue-9", "run-z", 1), "not a unit", request("issue-1", "x"))
}

// A response answers exactly one request addressed to the responder, and goes
// back to its requester. A DecisionRequest cannot be answered this way.
func TestOnlyAnAddressedRequestCanBeAnswered(t *testing.T) {
	asked := admit(t, commScope(), source("issue-1", "run-a", 1), request("issue-2", "which field?"),
		MessageDraft{Kind: KindDecisionRequest, Purpose: "ship it?", Body: "needs a human"})
	scope := commScope(asked...)
	reply := MessageDraft{Kind: KindCollaborationRequest, Target: "issue-1", InReplyTo: asked[0].ID, Purpose: "answer", Body: "the id field"}
	answered := admit(t, scope, source("issue-2", "run-b", 1), reply)
	if answered[0].Route.Unit != "issue-1" || answered[0].InReplyTo != asked[0].ID {
		t.Fatalf("response = %+v", answered[0])
	}
	refused(t, scope, source("issue-3", "run-c", 1), "not an admitted collaboration request addressed to issue-3", reply)
	wrongWay := reply
	wrongWay.Target = "issue-3"
	refused(t, scope, source("issue-2", "run-b", 1), "goes to its requester", wrongWay)
	for _, unit := range []string{"issue-1", "issue-2"} {
		target := "issue-2"
		if unit == "issue-2" {
			target = "issue-1"
		}
		refused(t, scope, source(unit, "run-"+unit, 1), "not an admitted collaboration request",
			MessageDraft{Kind: KindCollaborationRequest, Target: target, InReplyTo: asked[1].ID, Purpose: "decided", Body: "yes, ship it"})
	}
}

// Only a LIVE request can be answered: once its author supersedes it, a
// response to the old one is refused, and the replacement can be answered.
func TestASupersededRequestCannotBeAnswered(t *testing.T) {
	first := admit(t, commScope(), source("issue-1", "run-a", 1), request("issue-2", "use v1?"))
	correction := request("issue-2", "use v2?")
	correction.Supersedes = first[0].ID
	history := append(first, admit(t, commScope(first...), source("issue-1", "run-a", 2), correction)...)
	answer := func(id string) MessageDraft {
		return MessageDraft{Kind: KindCollaborationRequest, Target: "issue-1", InReplyTo: id, Purpose: "answer", Body: "yes"}
	}
	refused(t, commScope(history...), source("issue-2", "run-b", 1), "was superseded", answer(first[0].ID))
	if answered := admit(t, commScope(history...), source("issue-2", "run-b", 1), answer(history[1].ID)); answered[0].InReplyTo != history[1].ID {
		t.Fatalf("the live request was not answerable: %+v", answered[0])
	}
}

// Nothing a worker writes closes a DecisionRequest - not a StateUpdate saying
// it decided, and not a correction, which is a new open question.
func TestADecisionRequestIsAWaitNoWorkerCanSatisfy(t *testing.T) {
	requester := source("issue-1", "run-a", 1)
	asked := admit(t, commScope(), requester, MessageDraft{Kind: KindDecisionRequest, Purpose: "ship it?", Body: "b"})
	if asked[0].Route != (MessageRoute{Audience: AudienceAuthority}) {
		t.Fatalf("decision route = %+v", asked[0].Route)
	}
	history := append(asked, admit(t, commScope(asked...), source("issue-1", "run-a", 2),
		MessageDraft{Kind: KindStateUpdate, Body: "I have decided: ship it"})...)
	if open := OpenDecisions(history); len(open) != 1 || open[0].ID != asked[0].ID {
		t.Fatalf("open decisions = %+v", open)
	}
	corrected := admit(t, commScope(history...), source("issue-1", "run-a", 3),
		MessageDraft{Kind: KindDecisionRequest, Supersedes: asked[0].ID, Purpose: "ship it behind a flag?", Body: "b"})
	history = append(history, corrected...)
	if open := OpenDecisions(history); len(open) != 1 || open[0].ID != corrected[0].ID {
		t.Fatalf("open decisions after correction = %+v", open)
	}
	if inbox := InboxFor("issue-1", history, nil); len(inbox.Requests)+len(inbox.Findings) != 0 {
		t.Fatalf("a decision was routed into a worker inbox: %+v", inbox)
	}
}

// A correction appends; the superseded record stays in history, only its
// author's unit may correct it, and a record is corrected at most once.
func TestCorrectionAppendsAndPreservesHistory(t *testing.T) {
	first := admit(t, commScope(), source("issue-1", "run-a", 1), request("issue-2", "use v1"))
	fix := request("issue-2", "use v2")
	fix.Supersedes = first[0].ID
	second := admit(t, commScope(first...), source("issue-1", "run-a", 2), fix)
	history := append(first, second...)
	if len(history) != 2 || history[0].Body != "use v1" {
		t.Fatal("the corrected record was rewritten")
	}
	if live := Live(history); len(live) != 1 || live[0].ID != second[0].ID {
		t.Fatalf("live = %+v", live)
	}
	refused(t, commScope(history...), source("issue-1", "run-a", 3), "already superseded", fix)
	refused(t, commScope(first...), source("issue-3", "run-c", 1), "not an admitted collaboration_request from issue-3",
		MessageDraft{Kind: KindCollaborationRequest, Target: "issue-2", Supersedes: first[0].ID, Purpose: "p", Body: "x"})
	refused(t, commScope(first...), source("issue-1", "run-a", 2), "not an admitted state_update",
		MessageDraft{Kind: KindStateUpdate, Supersedes: first[0].ID, Body: "x"})
	twice := []MessageDraft{fix, fix}
	refused(t, commScope(first...), source("issue-1", "run-a", 2), "already superseded", twice...)
}

// A Finding bound to an older candidate never applies to a newer one.
func TestAStaleFindingDoesNotApplyToANewerSubject(t *testing.T) {
	findings := admit(t, commScope(), source("issue-1", "run-a", 1),
		MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-b1", Category: FindingDefect, Body: "old bug"},
		MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-b2", Category: FindingRisk, Body: "new risk"})
	newer := commScope().Subjects["handoff-b2"].Revision
	if findings[0].AppliesTo(newer) || !findings[1].AppliesTo(newer) {
		t.Fatal("a finding applied to a subject it is not bound to")
	}
	inbox := InboxFor("issue-2", findings, &newer)
	if len(inbox.Findings) != 1 || inbox.Findings[0].Body != "new risk" || len(inbox.StaleFindings) != 1 || inbox.StaleFindings[0].Body != "old bug" {
		t.Fatalf("inbox = %+v", inbox)
	}
	if inbox := InboxFor("issue-2", findings, nil); len(inbox.Findings) != 0 {
		t.Fatal("a finding applied to a unit with no current subject")
	}
}

// Updates are non-authoritative and shown latest-per-unit; every list and the
// scope history are bounded.
func TestListsAndHistoryAreBounded(t *testing.T) {
	var history []EngineeringMessage
	for attempt := 1; attempt <= 3; attempt++ {
		history = append(history, admit(t, commScope(history...), source("issue-1", "run-a", attempt),
			MessageDraft{Kind: KindStateUpdate, Body: fmt.Sprintf("step %d", attempt)})...)
	}
	if inbox := InboxFor("issue-2", history, nil); len(inbox.Updates) != 1 || inbox.Updates[0].Body != "step 3" {
		t.Fatalf("updates = %+v", inbox.Updates)
	}
	if inbox := InboxFor("issue-1", history, nil); len(inbox.Updates) != 0 {
		t.Fatal("a unit was shown its own updates")
	}
	for attempt := 4; len(history) < maxInboxEntries+4; attempt++ {
		history = append(history, admit(t, commScope(history...), source("issue-1", "run-a", attempt), request("issue-2", "q"))...)
	}
	if inbox := InboxFor("issue-2", history, nil); len(inbox.Requests) != maxInboxEntries || inbox.Omitted == 0 {
		t.Fatalf("requests %d omitted %d", len(inbox.Requests), inbox.Omitted)
	}
	full := commScope(make([]EngineeringMessage, MaxScopeMessages)...)
	refused(t, full, source("issue-1", "run-a", 1), "message bound", request("issue-2", "q"))
	wide := commScope()
	for i := range MaxScopeUnits {
		wide.Units = append(wide.Units, fmt.Sprintf("unit-%d", i))
	}
	refused(t, wide, source("issue-1", "run-a", 1), "unit bound", request("issue-2", "q"))
}

// A stored record whose route contradicts its kind - a Finding pointed away
// from its subject's owner - is corrupt, not a message.
func TestARecordWithAForgedRouteIsInvalid(t *testing.T) {
	finding := admit(t, commScope(), source("issue-1", "run-a", 1),
		MessageDraft{Kind: KindFinding, SubjectHandoff: "handoff-b1", Category: FindingDefect, Body: "x"})[0]
	finding.Route.Unit = "issue-3"
	if err := finding.Validate(); err == nil {
		t.Fatal("a finding routed away from its owner validated")
	}
}

// #508 review P4c: the decision_request grammar extension. A worker MAY
// prescribe the only outcome kind its own question accepts, and a
// selected_option question MUST then also prescribe its own closed,
// exhaustive option set - admitted and frozen into the insert-only message,
// never widened afterward.

func decisionRequest(expectedKind string, options ...string) MessageDraft {
	return MessageDraft{Kind: KindDecisionRequest, Purpose: "which approach?", Body: "two viable options",
		ExpectedOutcomeKind: expectedKind, PermittedOptions: options}
}

// TestADecisionRequestMayPrescribeItsOwnAnswerShape is the accepted shape:
// selected_option together with its own permitted_options admits and is
// frozen into the durable record exactly as written.
func TestADecisionRequestMayPrescribeItsOwnAnswerShape(t *testing.T) {
	asked := admit(t, commScope(), source("issue-1", "run-a", 1),
		decisionRequest(DecisionSelectedOption, "approach-a", "approach-b"))
	if asked[0].ExpectedOutcomeKind != DecisionSelectedOption {
		t.Fatalf("expected_outcome_kind = %q", asked[0].ExpectedOutcomeKind)
	}
	if got := asked[0].PermittedOptions; len(got) != 2 || got[0] != "approach-a" || got[1] != "approach-b" {
		t.Fatalf("permitted_options = %+v", got)
	}
	if err := asked[0].Validate(); err != nil {
		t.Fatalf("a well-formed selected_option decision_request failed its own round-trip validation: %v", err)
	}
}

// TestADecisionRequestLegacyOpenEndedShapeStillValidates is #508 review
// P4c's required compatibility proof: a request naming NEITHER member - the
// only shape #473 could admit before P4c, and every request #473 admitted
// before P4c - validates exactly as it always did.
func TestADecisionRequestLegacyOpenEndedShapeStillValidates(t *testing.T) {
	asked := admit(t, commScope(), source("issue-1", "run-a", 1),
		MessageDraft{Kind: KindDecisionRequest, Purpose: "ship it?", Body: "needs a human"})
	if asked[0].ExpectedOutcomeKind != "" || len(asked[0].PermittedOptions) != 0 {
		t.Fatalf("a legacy decision_request gained a shape it never asked for: %+v", asked[0])
	}
	if err := asked[0].Validate(); err != nil {
		t.Fatalf("a legacy, open-ended decision_request failed validation: %v", err)
	}
}

// TestASelectedOptionDecisionRequestRequiresPermittedOptions is the
// malformed-input refusal #508 review P4c requires: selected_option without
// its own exhaustive option set is refused closed, not interpreted as
// "anything goes" or silently widened to accept any text.
func TestASelectedOptionDecisionRequestRequiresPermittedOptions(t *testing.T) {
	refused(t, commScope(), source("issue-1", "run-a", 1), "requires its permitted_options",
		decisionRequest(DecisionSelectedOption))
}

// TestNonSelectedOptionDecisionRequestsMayNotCarryPermittedOptions proves
// the inverse malformed shape - permitted_options stated for a kind that
// never consults it - is refused rather than silently ignored.
func TestNonSelectedOptionDecisionRequestsMayNotCarryPermittedOptions(t *testing.T) {
	refused(t, commScope(), source("issue-1", "run-a", 1), "may not carry permitted_options",
		decisionRequest(DecisionAllowDeny, "allow"))
	refused(t, commScope(), source("issue-1", "run-a", 1), "requires expected_outcome_kind to be selected_option",
		MessageDraft{Kind: KindDecisionRequest, Purpose: "p", Body: "b", PermittedOptions: []string{"a"}})
}

// TestADecisionRequestUnrecognizedExpectedOutcomeKindIsRefused is the
// unknown-shape fail-closed refusal: an invented fourth kind is never
// interpreted, not admitted as if it were one of the three recognized ones.
func TestADecisionRequestUnrecognizedExpectedOutcomeKindIsRefused(t *testing.T) {
	refused(t, commScope(), source("issue-1", "run-a", 1), "is not", decisionRequest("yesno", "a"))
}

// TestADecisionRequestPermittedOptionsAreBoundedUniqueAndWellFormed proves
// the permitted-option set is itself a closed, finite, deduplicated menu -
// never an unbounded list a worker could use to smuggle an oversized
// document into what is supposed to be a short, authority-facing choice.
func TestADecisionRequestPermittedOptionsAreBoundedUniqueAndWellFormed(t *testing.T) {
	refused(t, commScope(), source("issue-1", "run-a", 1), "repeated",
		decisionRequest(DecisionSelectedOption, "a", "a"))
	refused(t, commScope(), source("issue-1", "run-a", 1), "required",
		decisionRequest(DecisionSelectedOption, "a", ""))
	oversized := make([]string, MaxDecisionRequestOptions+1)
	for i := range oversized {
		oversized[i] = fmt.Sprintf("option-%d", i)
	}
	refused(t, commScope(), source("issue-1", "run-a", 1), "above the", decisionRequest(DecisionSelectedOption, oversized...))
	tooLong := strings.Repeat("x", maxDecisionOptionBytes+1)
	refused(t, commScope(), source("issue-1", "run-a", 1), "bytes, above", decisionRequest(DecisionSelectedOption, tooLong))
}

// TestOnlyADecisionRequestMayCarryItsOwnAnswerShapeMembers proves the other
// three message kinds cannot smuggle an outcome-kind prescription or option
// set through a member that only decision_request ever consults.
func TestOnlyADecisionRequestMayCarryItsOwnAnswerShapeMembers(t *testing.T) {
	refused(t, commScope(), source("issue-1", "run-a", 1), "may not carry expected_outcome_kind",
		MessageDraft{Kind: KindStateUpdate, Body: "b", ExpectedOutcomeKind: DecisionAllowDeny})
	refused(t, commScope(), source("issue-1", "run-a", 1), "may not carry expected_outcome_kind",
		MessageDraft{Kind: KindCollaborationRequest, Target: "issue-2", Purpose: "p", Body: "b", PermittedOptions: []string{"a"}})
}
