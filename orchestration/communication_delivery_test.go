package orchestration

import "testing"

func deliverableFinding(t *testing.T) EngineeringMessage {
	t.Helper()
	from := source("issue-2", "run-b", 1)
	messages := admit(t, commScope(), from, MessageDraft{
		Kind: KindFinding, SubjectHandoff: "handoff-b1", Category: FindingDefect, Body: "shared.go conflicts",
	})
	if len(messages) != 1 {
		t.Fatalf("admitted %d messages, want 1", len(messages))
	}
	return messages[0]
}

func TestDeliverFindingRewritesRouteSubjectOwnerAndRevision(t *testing.T) {
	original := deliverableFinding(t)
	destinationRevision := HandoffSubject{BaseRevision: "real-base", CandidateRevision: "c1", CandidateTree: "tree-c1"}
	delivered, err := DeliverFinding(original, "batch-2", "issue-5", destinationRevision)
	if err != nil {
		t.Fatal(err)
	}
	if delivered.ID == original.ID {
		t.Fatal("a delivered finding reused the original's own global id")
	}
	if delivered.Scope != "batch-2" {
		t.Fatalf("delivered scope = %q, want batch-2", delivered.Scope)
	}
	if delivered.Route != (MessageRoute{Audience: AudienceUnit, Unit: "issue-5"}) {
		t.Fatalf("delivered route = %+v, want unit-routed to issue-5", delivered.Route)
	}
	if delivered.Subject == nil || delivered.Subject.Owner != "issue-5" {
		t.Fatalf("delivered subject owner = %+v, want issue-5", delivered.Subject)
	}
	if delivered.Subject.Handoff != original.Subject.Handoff {
		t.Fatalf("delivered subject handoff = %q, want the original's %q", delivered.Subject.Handoff, original.Subject.Handoff)
	}
	if delivered.Subject.Revision != destinationRevision {
		t.Fatalf("delivered subject revision = %+v, want %+v", delivered.Subject.Revision, destinationRevision)
	}
	if delivered.Body != original.Body || delivered.Category != original.Category || delivered.Source != original.Source {
		t.Fatalf("delivered finding changed its own observation or provenance: %+v", delivered)
	}
	if err := delivered.Validate(); err != nil {
		t.Fatalf("a delivered finding must validate on its own: %v", err)
	}
}

func TestDeliverFindingIsIdempotentAcrossRepeatedCalls(t *testing.T) {
	original := deliverableFinding(t)
	revision := HandoffSubject{BaseRevision: "b", CandidateRevision: "c", CandidateTree: "t"}
	first, err := DeliverFinding(original, "batch-2", "issue-5", revision)
	if err != nil {
		t.Fatal(err)
	}
	second, err := DeliverFinding(original, "batch-2", "issue-5", revision)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("delivering the same finding to the same destination twice named two different ids: %s, %s", first.ID, second.ID)
	}
}

func TestDeliverFindingDifferentDestinationsNameDifferentIDs(t *testing.T) {
	original := deliverableFinding(t)
	revision := HandoffSubject{BaseRevision: "b", CandidateRevision: "c", CandidateTree: "t"}
	toFive, err := DeliverFinding(original, "batch-2", "issue-5", revision)
	if err != nil {
		t.Fatal(err)
	}
	toSix, err := DeliverFinding(original, "batch-3", "issue-6", revision)
	if err != nil {
		t.Fatal(err)
	}
	if toFive.ID == toSix.ID {
		t.Fatal("delivering to two different destinations produced the same id")
	}
}

func TestDeliverFindingRefusesANonFinding(t *testing.T) {
	update := admit(t, commScope(), source("issue-1", "run-a", 1), MessageDraft{Kind: KindStateUpdate, Body: "halfway"})[0]
	if _, err := DeliverFinding(update, "batch-2", "issue-5", HandoffSubject{}); err == nil {
		t.Fatal("delivered a state_update as though it were a finding")
	}
}

func TestDeliverFindingRefusesTheSameScopeAsItsOrigin(t *testing.T) {
	original := deliverableFinding(t)
	if _, err := DeliverFinding(original, original.Scope, "issue-5", HandoffSubject{}); err == nil {
		t.Fatal("delivered a finding into the scope it was already admitted in")
	}
}

func TestDeliverFindingRefusesAnEmptyDestination(t *testing.T) {
	original := deliverableFinding(t)
	if _, err := DeliverFinding(original, "", "issue-5", HandoffSubject{}); err == nil {
		t.Fatal("delivered a finding with no destination scope")
	}
	if _, err := DeliverFinding(original, "batch-2", "", HandoffSubject{}); err == nil {
		t.Fatal("delivered a finding with no destination unit")
	}
}

func TestDeliverFindingRefusesASubjectlessMessage(t *testing.T) {
	subjectless := deliverableFinding(t)
	subjectless.Subject = nil
	if _, err := DeliverFinding(subjectless, "batch-2", "issue-5", HandoffSubject{}); err == nil {
		t.Fatal("delivered a finding carrying no subject at all")
	}
}
