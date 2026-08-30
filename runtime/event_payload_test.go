package runtime

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// TestJournalRefusesOversizedCanonicalPayload is the test that proves the size
// invariant is real. Every payload here uses "reason", a field the run
// disposition schema legitimately declares, so nothing about the field's name
// can be what refuses it: only the byte ceiling can.
func TestJournalRefusesOversizedCanonicalPayload(t *testing.T) {
	_, store := openJournal(t)
	benign := strings.Repeat("a", maxCanonicalPayloadBytes+1)
	// The same oversized body a deny-listed field would have carried, moved to a
	// field name the deny-list has never heard of.
	renamed := strings.Repeat("github_pat_fixturesecret\n+ go test ./...\n", 400)
	for _, refused := range []struct {
		id     string
		name   string
		reason string
	}{
		{"e-benign", "benign field", benign},
		{"e-renamed", "renamed transcript body", renamed},
	} {
		t.Run(refused.name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{"reason": refused.reason})
			if err != nil {
				t.Fatal(err)
			}
			if len(payload) <= maxCanonicalPayloadBytes {
				t.Fatalf("fixture is not oversized: %d bytes", len(payload))
			}
			_, err = store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: refused.id, RunID: "r",
				Type: EventRunCompleted, OccurredAt: time.Unix(105, 0).UTC(), Payload: payload})
			if err == nil {
				t.Fatal("an oversized canonical payload was appended")
			}
			if !strings.Contains(err.Error(), "ceiling") {
				t.Fatalf("expected the byte ceiling to refuse it, got %v", err)
			}
		})
	}
	events, err := store.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused append still wrote %d rows", len(events))
	}
	// The reference form of the same material is the path that is accepted: an
	// artifact is path + sha256, so it never charges against the ceiling.
	if _, err := store.AppendEvent(journalFixture(t, "r")[2]); err != nil {
		t.Fatalf("an artifact reference must be appendable: %v", err)
	}
	// A reason of ordinary length is still accepted, so the ceiling is a ceiling
	// and not a ban on the field.
	small, err := json.Marshal(map[string]string{"reason": "merged"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-ok", RunID: "r",
		Type: EventRunCompleted, OccurredAt: time.Unix(106, 0).UTC(), Payload: small}); err != nil {
		t.Fatalf("a bounded disposition payload must be appendable: %v", err)
	}
}

func TestJournalRefusesPayloadThatDoesNotMatchItsEventType(t *testing.T) {
	operation, err := json.Marshal(RunOperation{SchemaVersion: SchemaVersion, ID: "op-1", RunID: "r", Kind: "provider", IdempotencyKey: "a", State: Pending, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	reason, err := json.Marshal(map[string]string{"reason": "merged"})
	if err != nil {
		t.Fatal(err)
	}
	unknownField, err := json.Marshal(map[string]string{"reason": "merged", "note": "extra"})
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		name  string
		event EngineeringEvent
	}{
		{"operation payload on a disposition event", EngineeringEvent{Type: EventRunCompleted, Payload: operation}},
		{"disposition payload on an operation event", EngineeringEvent{Type: EventOperationPlanned, OperationID: "op-1", Payload: reason}},
		{"unknown payload field", EngineeringEvent{Type: EventRunCompleted, Payload: unknownField}},
		{"payload on an event that carries none", EngineeringEvent{Type: EventRunCreated, Payload: reason}},
		{"missing operation payload", EngineeringEvent{Type: EventOperationBefore, OperationID: "op-1"}},
		{"malformed payload", EngineeringEvent{Type: EventRunCompleted, Payload: json.RawMessage(`{"reason":`)}},
		{"trailing content after the payload", EngineeringEvent{Type: EventRunCompleted, Payload: json.RawMessage(`{"reason":"a"} {}`)}},
		{"disposition payload on a GitHub observation", EngineeringEvent{Type: EventGitHubCIObserved, Payload: reason}},
		{"missing contract payload", EngineeringEvent{Type: EventContractCompiled}},
		{"unknown event type", EngineeringEvent{Type: "run.invented"}},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, store := openJournal(t)
			e := refused.event
			e.SchemaVersion, e.ID, e.RunID, e.OccurredAt = SchemaVersion, "e-1", "r", time.Unix(105, 0).UTC()
			if _, err := store.AppendEvent(e); err == nil {
				t.Fatal("the payload was accepted into a canonical event row")
			}
			events, err := store.Events("r")
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("a refused append still wrote %d rows", len(events))
			}
		})
	}
}

// phase8Payloads is the minimal accepted payload for each registered event
// type that carries one, alongside a field that does not exist in its schema
// and a field given the wrong JSON type. One table drives acceptance,
// unknown-field refusal, and malformed refusal, so a type added to the
// registry without all three is visible as a missing row rather than an
// untested path.
func phase8Payloads() []struct {
	eventType string
	valid     map[string]any
	wrongType map[string]any
} {
	return []struct {
		eventType string
		valid     map[string]any
		wrongType map[string]any
	}{
		{EventSourceIntentChanged,
			map[string]any{"previous_digest": "d0", "current_digest": "d1", "reason": "issue edited"},
			map[string]any{"previous_digest": 1, "current_digest": "d1", "reason": "x"}},
		{EventSourceOptInRemoved,
			map[string]any{"issue": 41, "label": "zenchron"},
			map[string]any{"issue": "41", "label": "zenchron"}},
		{EventSourceOptInRestored,
			map[string]any{"issue": 41, "label": "zenchron"},
			map[string]any{"issue": 41, "label": []any{"zenchron"}}},
		{EventContractCompiled,
			map[string]any{"contract": map[string]any{"id": "c", "revision": "1"}, "subject": map[string]any{"repository": "o/r", "revision": "base"}},
			map[string]any{"contract": "c", "subject": map[string]any{"repository": "o/r", "revision": "base"}}},
		{EventCandidateChanged,
			map[string]any{"producer_id": "codex", "purpose": string(InvocationInitial), "outcome": string(Succeeded)},
			map[string]any{"producer_id": "codex", "purpose": 7, "outcome": string(Succeeded)}},
		{EventCandidateCommitted,
			map[string]any{"commit": "c1", "tree": "t1", "path_count": 2, "paths_digest": "pd"},
			map[string]any{"commit": "c1", "tree": "t1", "path_count": "2", "paths_digest": "pd"}},
		{EventCandidateBaseIntegrated,
			map[string]any{"strategy": "rebase", "base_revision": "b1", "commit": "c2", "tree": "t2"},
			map[string]any{"strategy": []any{"rebase"}, "base_revision": "b1", "commit": "c2", "tree": "t2"}},
		{EventCandidateExternalChanged,
			map[string]any{"expected_revision": "c1", "observed_revision": "c9"},
			map[string]any{"expected_revision": true, "observed_revision": "c9"}},
		{EventReassessmentCompleted,
			map[string]any{"material": true, "contract": map[string]any{"id": "c", "revision": "2"}, "deviation_kinds": []any{"scope_expansion"}, "requested_privilege_count": 1},
			map[string]any{"material": "true", "contract": map[string]any{"id": "c", "revision": "2"}, "requested_privilege_count": 1}},
		{EventAssuranceObserved,
			map[string]any{"provider_id": "go", "verifier_definition": "vd", "passed": true, "commit": "c1", "tree": "t1"},
			map[string]any{"provider_id": "go", "verifier_definition": "vd", "passed": "yes", "commit": "c1", "tree": "t1"}},
		{EventAuthorityEvaluated,
			map[string]any{"decision": map[string]any{"id": "d", "revision": "1"}, "action": map[string]any{"type": "publish", "target": "o/r"}, "status": string(domain.AuthorityAuthorized)},
			map[string]any{"decision": map[string]any{"id": "d", "revision": 1}, "action": map[string]any{"type": "publish", "target": "o/r"}, "status": "authorized"}},
		{EventGitHubPRObserved,
			map[string]any{"number": 4, "head_revision": "c1", "base_revision": "b1", "state": "open", "merged": false},
			map[string]any{"number": "4", "head_revision": "c1", "base_revision": "b1", "state": "open"}},
		{EventGitHubCIObserved,
			map[string]any{"head_revision": "c1", "conclusion": "failure", "check_count": 3, "failing_checks": []any{"vet"}},
			map[string]any{"head_revision": "c1", "conclusion": "failure", "check_count": 3, "failing_checks": "vet"}},
		{EventGitHubReviewObserved,
			map[string]any{"head_revision": "c1", "state": "changes_requested", "finding_count": 2},
			map[string]any{"head_revision": "c1", "state": "changes_requested", "finding_count": 1.5}},
		// Phase 10 human authority joins the same table so it is held to the
		// same three refusals as every other registered schema.
		{EventHumanAuthorityRecorded,
			humanAuthorityFixture(nil),
			humanAuthorityFixture(map[string]any{"request": "ar-1"})},
	}
}

// humanAuthorityFixture is the minimal accepted human authority payload, with
// the named members overridden. Every value is an identity, a reference, or a
// short closed enum: there is no approved flag and no bulk content.
func humanAuthorityFixture(override map[string]any) map[string]any {
	payload := map[string]any{
		"schema_version": SchemaVersion,
		"evidence_id":    "ev-1",
		"request":        map[string]any{"id": "ar-1", "revision": "binding-digest"},
		"operator": map[string]any{
			"id": "operator-a", "account_name": "local-account", "host": "workstation",
			"provenance": string(ProvenanceLocalUnverified)},
		"decision":     "approve",
		"action":       map[string]any{"type": "publish", "target": "owner/name"},
		"repository":   "owner/name",
		"controller":   map[string]any{"id": "controller-a", "revision": strings.Repeat("4", 64)},
		"candidate":    map[string]any{"branch": "candidate", "revision": "c1", "tree": "t1"},
		"contract":     map[string]any{"id": "contract", "revision": "1"},
		"state_sha256": strings.Repeat("5", 64),
		"occurred_at":  time.Unix(105, 0).UTC().Format(time.RFC3339),
	}
	for name, value := range override {
		payload[name] = value
	}
	return payload
}

func marshalPayload(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestPhase8PayloadSchemas proves the registry was widened by closed schemas
// only: each accepts its minimal payload and each still refuses an unknown
// field and a wrong-typed field.
func TestPhase8PayloadSchemas(t *testing.T) {
	for _, schema := range phase8Payloads() {
		t.Run(schema.eventType, func(t *testing.T) {
			_, store := openJournal(t)
			accepted := EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-ok", RunID: "r", Type: schema.eventType,
				OccurredAt: time.Unix(105, 0).UTC(), Payload: marshalPayload(t, schema.valid)}
			if _, err := store.AppendEvent(accepted); err != nil {
				t.Fatalf("the minimal valid payload was refused: %v", err)
			}
			// The extra member is one no schema declares. "note" would not do:
			// the human authority schema legitimately declares it, and this
			// subtest has to prove the registry refuses a member that is not in
			// the schema, not one that happens to be unused elsewhere.
			unknown := map[string]any{"unregistered_member": "extra"}
			for name, value := range schema.valid {
				unknown[name] = value
			}
			for _, refused := range []struct {
				name    string
				payload json.RawMessage
			}{
				{"unknown field", marshalPayload(t, unknown)},
				{"wrong-typed field", marshalPayload(t, schema.wrongType)},
				{"malformed json", json.RawMessage(`{"head_revision":`)},
			} {
				t.Run(refused.name, func(t *testing.T) {
					_, store := openJournal(t)
					if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-bad", RunID: "r",
						Type: schema.eventType, OccurredAt: time.Unix(106, 0).UTC(), Payload: refused.payload}); err == nil {
						t.Fatalf("%s was accepted into a canonical event row", refused.name)
					}
					events, err := store.Events("r")
					if err != nil {
						t.Fatal(err)
					}
					if len(events) != 0 {
						t.Fatalf("a refused append still wrote %d rows", len(events))
					}
				})
			}
		})
	}
}

// TestReservedEventTypesStayReserved is the other half of every widening: the
// registry was not loosened globally, so a catalogue type with no implemented
// payload is still refused with or without one. The reserved set is derived
// from the catalogue rather than listed, so registering a schema retires
// exactly that type from this test and no other.
//
// Phase 10 registered human.authority_recorded, which was the last reserved
// type, so the derived set is now empty. The original guard against that -
// failing when nothing is reserved - existed so the test could never go
// vacuously green. It is replaced here by the assertions that make the same
// property non-vacuous in both directions rather than by dropping it: every
// catalogue type now HAS a schema (nothing was left reserved by accident),
// every schema is IN the catalogue (the registry cannot be widened past it),
// and a type in neither map is still refused outright. The reserved loop below
// is unchanged and runs again the moment a new type is catalogued ahead of its
// schema, which is the case it was written for.
func TestReservedEventTypesStayReserved(t *testing.T) {
	for eventType := range eventPayloads {
		if !eventTypes[eventType] {
			t.Fatalf("payload schema %q is not in the event catalogue: the registry is wider than the catalogue", eventType)
		}
	}
	var reserved []string
	for eventType := range eventTypes {
		if _, implemented := eventPayloads[eventType]; !implemented {
			reserved = append(reserved, eventType)
		}
	}
	if len(reserved) == 0 && len(eventPayloads) != len(eventTypes) {
		t.Fatalf("nothing is reserved but the registry has %d schemas for %d catalogue types", len(eventPayloads), len(eventTypes))
	}
	// A type that is in neither map is refused as unknown, so "closed" is not
	// only a property of the two maps agreeing with each other.
	_, uncatalogued := openJournal(t)
	if _, err := uncatalogued.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r",
		Type: "human.authority_invented", OccurredAt: time.Unix(105, 0).UTC()}); err == nil ||
		!strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("an uncatalogued event type must be refused as unknown, got %v", err)
	}
	for _, eventType := range reserved {
		for _, payload := range []json.RawMessage{nil, marshalPayload(t, map[string]any{"reason": "x"})} {
			_, store := openJournal(t)
			_, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r",
				Type: eventType, OccurredAt: time.Unix(105, 0).UTC(), Payload: payload})
			if err == nil {
				t.Fatalf("reserved event type %q was accepted", eventType)
			}
			if !strings.Contains(err.Error(), "reserved") {
				t.Fatalf("event type %q must be refused as reserved, got %v", eventType, err)
			}
		}
	}
}

// TestPhase8PayloadListBounds proves the list bounds are enforced rather than
// documented: neither too many elements nor one oversized element is accepted,
// and both fixtures stay far below the 8 KiB canonical ceiling so only the
// bound itself can be what refuses them.
func TestPhase8PayloadListBounds(t *testing.T) {
	for _, bounded := range []struct {
		name      string
		eventType string
		payload   map[string]any
	}{
		{"too many failing checks", EventGitHubCIObserved, map[string]any{
			"head_revision": "c1", "conclusion": "failure", "check_count": 99,
			"failing_checks": repeatedList("check", maxPayloadListItems+1, 5)}},
		{"oversized failing check name", EventGitHubCIObserved, map[string]any{
			"head_revision": "c1", "conclusion": "failure", "check_count": 1,
			"failing_checks": repeatedList("check", 1, maxPayloadListItemBytes+1)}},
		{"too many deviation kinds", EventReassessmentCompleted, map[string]any{
			"material": true, "contract": map[string]any{"id": "c", "revision": "2"},
			"deviation_kinds": repeatedList("kind", maxPayloadListItems+1, 5), "requested_privilege_count": 0}},
		{"oversized deviation kind", EventReassessmentCompleted, map[string]any{
			"material": true, "contract": map[string]any{"id": "c", "revision": "2"},
			"deviation_kinds": repeatedList("kind", 1, maxPayloadListItemBytes+1), "requested_privilege_count": 0}},
	} {
		t.Run(bounded.name, func(t *testing.T) {
			_, store := openJournal(t)
			payload := marshalPayload(t, bounded.payload)
			if len(payload) >= maxCanonicalPayloadBytes {
				t.Fatalf("fixture is %d bytes: the ceiling could refuse it instead of the list bound", len(payload))
			}
			_, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r",
				Type: bounded.eventType, OccurredAt: time.Unix(105, 0).UTC(), Payload: payload})
			if err == nil {
				t.Fatal("an unbounded list was accepted into a canonical event row")
			}
			if !strings.Contains(err.Error(), "bound") {
				t.Fatalf("expected the list bound to refuse it, got %v", err)
			}
		})
	}
}

// repeatedList builds count elements of exactly size bytes each.
func repeatedList(prefix string, count, size int) []any {
	out := make([]any, 0, count)
	for i := 0; i < count; i++ {
		element := prefix + strings.Repeat("x", size)
		out = append(out, element[:size])
	}
	return out
}

// humanAuthorityEvent is one human authority evidence event on run "r".
func humanAuthorityEvent(t *testing.T, id string, override map[string]any) EngineeringEvent {
	t.Helper()
	return EngineeringEvent{SchemaVersion: SchemaVersion, ID: id, RunID: "r", Type: EventHumanAuthorityRecorded,
		OccurredAt: time.Unix(110, 0).UTC(), Payload: marshalPayload(t, humanAuthorityFixture(override))}
}

// boundHumanAuthorityFixture is the fixture bound to the run openJournal
// creates, so the recorded evidence can be revalidated by the frozen
// HumanAuthorityBinding rather than only by the payload schema.
func boundHumanAuthorityFixture(override map[string]any) map[string]any {
	run := newJournalRun("r")
	bound := map[string]any{
		"candidate": map[string]any{"branch": run.Candidate.Branch, "revision": run.Candidate.Revision, "tree": run.Candidate.Tree},
		"contract":  map[string]any{"id": run.Contract.ID, "revision": run.Contract.Revision},
	}
	for name, value := range override {
		bound[name] = value
	}
	return humanAuthorityFixture(bound)
}

func decodeHumanAuthority(t *testing.T, e EngineeringEvent) HumanAuthorityRecordedPayload {
	t.Helper()
	var payload HumanAuthorityRecordedPayload
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

// TestHumanAuthorityPayloadRefusesEvidenceThatIsNotABinding proves the schema
// records a binding rather than an approval: every reference that makes the
// evidence specific is required, the decision is closed to the two values the
// frozen HumanAuthorityBinding accepts, and no flag stands in for any of it.
func TestHumanAuthorityPayloadRefusesEvidenceThatIsNotABinding(t *testing.T) {
	for _, refused := range []struct {
		name   string
		mutate func(map[string]any)
		wants  string
	}{
		{"absent request binding", func(p map[string]any) { delete(p, "request") }, "request.id"},
		{"request id without its binding digest", func(p map[string]any) {
			p["request"] = map[string]any{"id": "ar-1", "revision": ""}
		}, "request.revision"},
		{"absent evidence id", func(p map[string]any) { delete(p, "evidence_id") }, "evidence_id"},
		{"candidate without a tree", func(p map[string]any) {
			p["candidate"] = map[string]any{"branch": "candidate", "revision": "c1"}
		}, "candidate.tree"},
		{"contract id without a revision", func(p map[string]any) { p["contract"] = map[string]any{"id": "contract"} }, "contract.revision"},
		{"controller without a revision", func(p map[string]any) { p["controller"] = map[string]any{"id": "controller-a"} }, "controller.revision"},
		{"absent subject state digest", func(p map[string]any) { delete(p, "state_sha256") }, "state_sha256"},
		{"absent repository", func(p map[string]any) { delete(p, "repository") }, "repository"},
		{"absent action target", func(p map[string]any) { p["action"] = map[string]any{"type": "publish"} }, "action.target"},
		{"operator without an identity", func(p map[string]any) {
			p["operator"] = map[string]any{"provenance": string(ProvenanceLocalUnverified)}
		}, "operator.id"},
		{"an identity claiming to be verified", func(p map[string]any) {
			p["operator"] = map[string]any{"id": "operator-a", "provenance": "cryptographically_verified"}
		}, "provenance"},
		{"a decision outside the closed set", func(p map[string]any) { p["decision"] = "approved" }, "approve or reject"},
		{"no decision at all", func(p map[string]any) { delete(p, "decision") }, "approve or reject"},
		{"a schema version this runtime does not write", func(p map[string]any) { p["schema_version"] = "9.9" }, "schema_version"},
		{"absent boundary clock reading", func(p map[string]any) { delete(p, "occurred_at") }, "occurred_at"},
		{"an unbounded note", func(p map[string]any) { p["note"] = strings.Repeat("n", maxPayloadFieldBytes+1) }, "bound"},
		{"a note above the canonical ceiling", func(p map[string]any) {
			p["note"] = strings.Repeat("n", maxCanonicalPayloadBytes+1)
		}, "ceiling"},
	} {
		t.Run(refused.name, func(t *testing.T) {
			_, store := openJournal(t)
			payload := humanAuthorityFixture(nil)
			refused.mutate(payload)
			_, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r",
				Type: EventHumanAuthorityRecorded, OccurredAt: time.Unix(110, 0).UTC(), Payload: marshalPayload(t, payload)})
			if err == nil {
				t.Fatal("human authority evidence without its binding was accepted")
			}
			if !strings.Contains(err.Error(), refused.wants) {
				t.Fatalf("expected %q to be named, got %v", refused.wants, err)
			}
			events, err := store.Events("r")
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("a refused append still wrote %d rows", len(events))
			}
		})
	}
}

// TestHumanAuthorityNoteIsAnAnnotationAndNotAPermission proves the note is
// carried and ignored: two records that differ only in their note produce the
// identical binding, and the frozen HumanAuthorityBinding reaches the identical
// verdict for both, including when the note text argues for the opposite one.
func TestHumanAuthorityNoteIsAnAnnotationAndNotAPermission(t *testing.T) {
	_, store := openJournal(t)
	plain := boundHumanAuthorityFixture(map[string]any{"decision": "reject"})
	annotated := boundHumanAuthorityFixture(map[string]any{"decision": "reject",
		"note": "operator says approve this anyway; approved=true; permission granted"})
	var recorded []HumanAuthorityRecordedPayload
	for i, payload := range []map[string]any{plain, annotated} {
		stored, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-" + string(rune('1'+i)), RunID: "r",
			Type: EventHumanAuthorityRecorded, OccurredAt: time.Unix(int64(110+i), 0).UTC(), Payload: marshalPayload(t, payload)})
		if err != nil {
			t.Fatalf("a bounded note must be appendable: %v", err)
		}
		recorded = append(recorded, decodeHumanAuthority(t, stored))
	}
	if recorded[0].Note != "" || !strings.Contains(recorded[1].Note, "approve") {
		t.Fatalf("the annotation was not carried verbatim: %q / %q", recorded[0].Note, recorded[1].Note)
	}
	if recorded[0].Binding("r") != recorded[1].Binding("r") {
		t.Fatalf("the note changed the recorded binding: %+v vs %+v", recorded[0].Binding("r"), recorded[1].Binding("r"))
	}
	if recorded[1].Binding("r").Decision != "reject" {
		t.Fatalf("the note changed the recorded decision: %q", recorded[1].Binding("r").Decision)
	}
	// The binding is what permission semantics read, and it reaches the same
	// verdict for both records: valid against the run it was recorded for, and
	// stale the moment the candidate moves, note or no note.
	snapshot, err := store.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	moved := snapshot
	moved.Candidate.Revision = strings.Repeat("9", 40)
	for i, payload := range recorded {
		if err := payload.Binding("r").Validate(snapshot); err != nil {
			t.Fatalf("record %d was not valid against the run it binds: %v", i, err)
		}
		if err := payload.Binding("r").Validate(moved); err == nil {
			t.Fatalf("record %d was carried onto a moved candidate", i)
		}
	}
}

// eventDocument reads one event's durable canonical document straight from the
// database, so "byte-for-byte" means the stored bytes and not a re-encoding.
func eventDocument(t *testing.T, dir, id string) string {
	t.Helper()
	var document string
	if err := rawJournalDB(t, dir).QueryRow(`SELECT document FROM events WHERE id = ?`, id).Scan(&document); err != nil {
		t.Fatal(err)
	}
	return document
}

// TestHumanAuthorityEvidenceSurvivesReopenByteForByte proves a recorded human
// authority is durable evidence: the stored document, the event hash, the
// decoded payload, and the replayed state are identical across close, reopen,
// and replay.
func TestHumanAuthorityEvidenceSurvivesReopenByteForByte(t *testing.T) {
	dir, store := openJournal(t)
	stored, err := store.AppendEvent(humanAuthorityEvent(t, "e-1", map[string]any{"note": "reviewed the diff"}))
	if err != nil {
		t.Fatal(err)
	}
	live, err := store.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	liveDocument := eventDocument(t, dir, "e-1")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if restoredDocument := eventDocument(t, dir, "e-1"); restoredDocument != liveDocument {
		t.Fatalf("the durable document changed across reopen:\n%s\n%s", liveDocument, restoredDocument)
	}
	events, err := reopened.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly the one authority event, got %d", len(events))
	}
	restoredCanonical, err := CanonicalJSON(events[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(restoredCanonical) != liveDocument {
		t.Fatalf("the replayed event is not the stored bytes:\n%s\n%s", liveDocument, restoredCanonical)
	}
	if events[0].EventHash != stored.EventHash {
		t.Fatalf("event hash changed across reopen: %q vs %q", stored.EventHash, events[0].EventHash)
	}
	if decodeHumanAuthority(t, events[0]) != decodeHumanAuthority(t, stored) {
		t.Fatalf("the recorded authority changed across reopen: %+v", decodeHumanAuthority(t, events[0]))
	}
	restored, err := reopened.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if restored.StateSHA256 != live.StateSHA256 {
		t.Fatalf("state digest changed across reopen: %q vs %q", live.StateSHA256, restored.StateSHA256)
	}
}
