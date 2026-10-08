package runtime

// #508 review P4a (D1/R2-B/R2-C): a worker-written decision_request from an
// attempt the runtime classifies as an incomplete, checkpointed candidate is
// trusted only once the matching checkpoint commit is itself durably
// journalled (commit-before-admit), and only when the report carries
// decision_request content ALONE - any other kind in the same report refuses
// the WHOLE report, immutably, never a partial admission.
//
// checkpointCommitted and admitObservedMessages' fromCheckpoint branch are
// tested directly, on constructed data, rather than by fighting Reconcile's
// own internal pass loop (which runs a checkpoint's invoke and its commit
// back to back within one external Reconcile call, leaving no seam a caller
// can stop at between them): TestACheckpointedDecisionIsAdmittedOnceItsCommitLands,
// below, is the end-to-end proof that eligibility and admission compose
// correctly through the real pipeline.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func checkpointEvent(t *testing.T, eventType, producing string) EngineeringEvent {
	t.Helper()
	raw, err := json.Marshal(CandidateCommittedPayload{Commit: "c", Tree: "t", PathsDigest: "d", Producing: producing})
	if err != nil {
		t.Fatal(err)
	}
	return EngineeringEvent{Type: eventType, Payload: raw}
}

// TestCheckpointCommittedRequiresAMatchingDurableCommit is the pure
// eligibility check: absent, differently-bound, or empty-binding events never
// answer true.
func TestCheckpointCommittedRequiresAMatchingDurableCommit(t *testing.T) {
	cases := []struct {
		name    string
		events  []EngineeringEvent
		binding string
		want    bool
	}{
		{"no events at all", nil, "initial|a", false},
		{"unrelated event type", []EngineeringEvent{{Type: EventRunCreated}}, "initial|a", false},
		{"a checkpoint for a DIFFERENT binding", []EngineeringEvent{checkpointEvent(t, EventCandidateCheckpointed, "initial|other")}, "initial|a", false},
		{"an empty binding never matches, even a real checkpoint", []EngineeringEvent{checkpointEvent(t, EventCandidateCheckpointed, "initial|a")}, "", false},
		{"a matching checkpoint", []EngineeringEvent{checkpointEvent(t, EventCandidateCheckpointed, "initial|a")}, "initial|a", true},
		{"a matching ordinary commit (also durable)", []EngineeringEvent{checkpointEvent(t, EventCandidateCommitted, "initial|a")}, "initial|a", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := checkpointCommitted(c.events, c.binding)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.want {
				t.Fatalf("checkpointCommitted = %t, want %t", got, c.want)
			}
		})
	}
}

// writeMessageReportFile writes a raw message report document directly to an
// invocation's runtime-owned slot, mirroring what a real worker would have
// written, without driving a real provider invocation.
func writeMessageReportFile(t *testing.T, stateDir, runID, operationID string, attempt int, document string) string {
	t.Helper()
	path, err := MessageReportPath(stateDir, ExecutionAttemptRef{RunID: runID, OperationID: operationID, Attempt: attempt})
	if err != nil {
		t.Fatal(err)
	}
	if err := clearResultSlot(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAdmitObservedMessagesFromCheckpointAcceptsDecisionRequestOnly is #508
// review P4a's R2-C proof at the unit level: a checkpoint-sourced report
// containing ONLY decision_request admits normally; one carrying ANY other
// kind alongside it refuses the WHOLE report, with a reason naming why.
func TestAdmitObservedMessagesFromCheckpointAcceptsDecisionRequestOnly(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	scope, _, err := batchMessageScope(fixture.store, batch)
	if err != nil {
		t.Fatal(err)
	}
	source := orchestration.MessageSource{Unit: orchestration.BatchItemUnit(fleetFirstIssue), RunID: runID, AgentID: "claude", OperationID: "op-checkpoint", Attempt: 1}
	now := fixture.clock.Now()

	only := messageDocument(t, orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "which way?", Body: "pick one"})
	path := writeMessageReportFile(t, fixture.stateDir, runID, source.OperationID, source.Attempt, only)
	_, digest, present, err := readMessageReport(path)
	if err != nil || !present {
		t.Fatalf("test report unreadable: present=%t err=%v", present, err)
	}
	admitted, reason, err := admitObservedMessages(fixture.stateDir, scope, source, digest, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Fatalf("a decision_request-only checkpoint report was refused: %s", reason)
	}
	if len(admitted) != 1 || admitted[0].Kind != orchestration.KindDecisionRequest {
		t.Fatalf("expected exactly one admitted decision_request, got %+v", admitted)
	}

	mixed := messageDocument(t,
		orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "which way?", Body: "pick one"},
		orchestration.MessageDraft{Kind: orchestration.KindStateUpdate, Body: "a progress note"},
	)
	source2 := source
	source2.OperationID = "op-checkpoint-2"
	path2 := writeMessageReportFile(t, fixture.stateDir, runID, source2.OperationID, source2.Attempt, mixed)
	_, digest2, present2, err := readMessageReport(path2)
	if err != nil || !present2 {
		t.Fatalf("test report unreadable: present=%t err=%v", present2, err)
	}
	admitted2, reason2, err := admitObservedMessages(fixture.stateDir, scope, source2, digest2, now, true)
	if err != nil {
		t.Fatal(err)
	}
	if reason2 == "" {
		t.Fatalf("a mixed-kind checkpoint report was admitted instead of wholly refused: %+v", admitted2)
	}
	if len(admitted2) != 0 {
		t.Fatalf("a refused report must admit nothing, got %+v", admitted2)
	}
	if !strings.Contains(reason2, string(orchestration.KindDecisionRequest)) || !strings.Contains(reason2, string(orchestration.KindStateUpdate)) {
		t.Fatalf("refusal reason does not name both kinds involved: %q", reason2)
	}

	// The SAME document, admitted without fromCheckpoint (the ordinary,
	// unchanged completing-invocation path), is unaffected by this rule.
	source3 := source
	source3.OperationID = "op-ordinary"
	path3 := writeMessageReportFile(t, fixture.stateDir, runID, source3.OperationID, source3.Attempt, mixed)
	_, digest3, present3, err := readMessageReport(path3)
	if err != nil || !present3 {
		t.Fatalf("test report unreadable: present=%t err=%v", present3, err)
	}
	admitted3, reason3, err := admitObservedMessages(fixture.stateDir, scope, source3, digest3, now, false)
	if err != nil {
		t.Fatal(err)
	}
	if reason3 != "" {
		t.Fatalf("an ORDINARY (non-checkpoint) mixed-kind report must admit normally: %s", reason3)
	}
	if len(admitted3) != 2 {
		t.Fatalf("expected both messages of the ordinary report admitted, got %+v", admitted3)
	}
}

// TestACheckpointedDecisionIsAdmittedOnceItsCommitLands is the end-to-end
// proof through the REAL pipeline: a worker checkpoints and asks in the same
// attempt; once the engine's own candidate.commit durably journals that
// checkpoint, the batch admission step admits the question.
func TestACheckpointedDecisionIsAdmittedOnceItsCommitLands(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	worker := &messagingWorker{fleetProvider: fixture.worker, documents: map[string]string{}, seeded: map[string]bool{}}
	fixture.deps.Provider = worker
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, fleetCheckpointThenComplete)
	worker.say(runID, decisionWaitDocument(t), false)

	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context(), runID); err != nil {
		t.Fatal(err)
	}

	foundCommit := false
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == EventCandidateCheckpointed {
			foundCommit = true
		}
	}
	if !foundCommit {
		t.Fatal("test precondition violated: the checkpoint commit never landed")
	}

	run, found, err := fixture.store.Run(runID)
	if err != nil || !found || run.Orchestration == nil {
		t.Fatalf("run %s has no orchestration batch: found=%t err=%v", runID, found, err)
	}
	batch, found, err := fixture.store.OrchestrationBatch(run.Orchestration.BatchID)
	if err != nil || !found {
		t.Fatalf("batch unreadable: found=%t err=%v", found, err)
	}
	if err := admitOrchestratedMessages(fixture.store, fixture.stateDir, batch, fixture.clock.Now()); err != nil {
		t.Fatal(err)
	}
	open, err := fixture.store.OpenDecisionRequestsForRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("expected exactly one admitted decision_request once its checkpoint commit landed, got %d", len(open))
	}
}
