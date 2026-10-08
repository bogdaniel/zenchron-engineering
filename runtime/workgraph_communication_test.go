package runtime

// #474 acceptance: the cross-unit message scope.
//
// #472 shipped a WorkGraph where each unit's child run is its own one-issue
// #470 batch, and #473 shipped typed messages scoped to a batch - so, until
// this file, a reviewer unit could never even name a producer unit's handoff
// as a Finding's subject: the two units were never in the same scope. Both
// docs/workgraph.md and #473's own batchMessageScope comment name this as the
// seam left for whoever owns it. This is that seam, and nothing else: the
// same Finding/CollaborationRequest/DecisionRequest/StateUpdate vocabulary,
// the same deterministic Finding-to-subject-owner routing, the same
// immutable, stale-on-new-candidate admitted record #473 already defined.

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func reviewPair() []orchestration.WorkUnit {
	return []orchestration.WorkUnit{
		{ID: "producer", Purpose: "produce the candidate", Role: domain.RoleImplementer, Issue: graphUnitA},
		{ID: "reviewer", Purpose: "review the candidate", Role: domain.RoleReviewer, Issue: graphUnitB, DependsOn: []string{"producer"}},
	}
}

// A Finding written by one WorkGraph unit, naming a sibling unit's admitted
// handoff, is routed to that sibling across their two different batches -
// and goes stale the moment that sibling admits a newer handoff. Neither
// half of this was possible before this file: the producer's own one-unit
// batch scope never contained the reviewer, and vice versa.
func TestAFindingCrossesWorkGraphUnitsAndGoesStaleOnANewerHandoff(t *testing.T) {
	fixture, _ := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, reviewPair())
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	producer, reviewer := settled.unit(t, "producer"), settled.unit(t, "reviewer")

	handoffs, err := fixture.store.RunHandoffs(producer.RunID)
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("producer has %d admitted handoffs (%v)", len(handoffs), err)
	}
	owned := handoffs[0]

	// The reviewer's SECOND invocation is where #233 would write its verdict;
	// #474 owns only that the typed Finding it carries is admitted and routed
	// - not the review decision itself.
	simulateInvocation(t, fixture, reviewer.RunID, "op-review-1", messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindFinding, SubjectHandoff: owned.ID, Category: orchestration.FindingDefect, Body: "off by one",
	}))
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	graphMessages, err := fixture.store.ScopeMessages(view.GraphID)
	if err != nil || len(graphMessages) != 1 {
		t.Fatalf("graph scope admitted %d messages (%v)", len(graphMessages), err)
	}
	finding := graphMessages[0]
	wantRoute := orchestration.MessageRoute{Audience: orchestration.AudienceUnit, Unit: orchestration.BatchItemUnit(producer.Issue)}
	if finding.Route != wantRoute || finding.Source.RunID != reviewer.RunID {
		t.Fatalf("finding routed to %+v from %s, want %+v from the reviewer's run", finding.Route, finding.Source.RunID, wantRoute)
	}

	// Never duplicated into the reviewer's OWN one-unit batch scope: the same
	// invocation's messages are admitted exactly once, under the graph.
	reviewerRun, _, err := fixture.store.Run(reviewer.RunID)
	if err != nil {
		t.Fatal(err)
	}
	ownBatchMessages, err := fixture.store.ScopeMessages(reviewerRun.Orchestration.BatchID)
	if err != nil || len(ownBatchMessages) != 0 {
		t.Fatalf("the reviewer's own batch scope carries %d messages, want 0: %v", len(ownBatchMessages), ownBatchMessages)
	}

	shown := contextOf(t, fixture, producer.RunID)
	if len(shown.Inbox.Findings) != 1 || shown.Inbox.Findings[0].ID != finding.ID {
		t.Fatalf("the producer was not shown the reviewer's finding across batches: %+v", shown.Inbox)
	}

	// A new candidate from the producer - the remediation #474's issue frames
	// as the response to a BLOCK - makes the finding stale, never silently
	// carried over to what the producer now owns.
	newer := owned
	newer.ID, newer.Producer.OperationID = "handoff-remediated", "op-producer-2"
	newer.Subject.CandidateRevision, newer.Subject.CandidateTree = strings.Repeat("e", 40), strings.Repeat("f", 40)
	newer.AdmittedAt = fixture.clock.Now()
	if _, err := fixture.store.AdmitHandoff(newer); err != nil {
		t.Fatal(err)
	}
	after := contextOf(t, fixture, producer.RunID).Inbox
	if len(after.Findings) != 0 || len(after.StaleFindings) != 1 {
		t.Fatalf("a finding about the old candidate still applies to the new one: %+v", after)
	}
}

// Restart replay admits nothing twice at the graph scope either - the same
// idempotency #473 already proved at the batch scope.
func TestWorkGraphMessagesSurviveRestartAndReplayIsIdempotent(t *testing.T) {
	fixture, _ := newMessagingFixture(t)
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, reviewPair())
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	producer, reviewer := settled.unit(t, "producer"), settled.unit(t, "reviewer")
	handoffs, err := fixture.store.RunHandoffs(producer.RunID)
	if err != nil || len(handoffs) != 1 {
		t.Fatalf("producer has %d admitted handoffs (%v)", len(handoffs), err)
	}

	simulateInvocation(t, fixture, reviewer.RunID, "op-review-1", messageDocument(t, orchestration.MessageDraft{
		Kind: orchestration.KindFinding, SubjectHandoff: handoffs[0].ID, Category: orchestration.FindingRisk, Body: "needs a second look",
	}))
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.ScopeMessages(view.GraphID)
	if err != nil || len(before) != 1 {
		t.Fatalf("admitted %d messages before restart (%v)", len(before), err)
	}

	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = reopened
	t.Cleanup(func() { reopened.Close() })
	if _, err := fixture.supervisor().Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	after, err := reopened.ScopeMessages(view.GraphID)
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, _ := CanonicalJSON(before)
	afterJSON, _ := CanonicalJSON(after)
	if string(firstJSON) != string(afterJSON) {
		t.Fatalf("restart or replay changed the graph's message history:\n%s\n%s", firstJSON, afterJSON)
	}
}
