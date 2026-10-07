package runtime

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #492 proofs: a handoff the strict decoder refused for a protocol
// mistake gets exactly one result-only correction, and nothing about the
// engineering is re-run, re-budgeted or touched.

// repairRun orchestrates one issue whose engineering writes handoff and whose
// repair, if one is invoked, does repair.
func repairRun(t *testing.T, handoff fleetBehaviour, repair fleetRepair) (*fleetFixture, *Supervisor, OrchestrationView, string) {
	t.Helper()
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	fixture.worker.set(runID, handoff)
	fixture.worker.setRepair(runID, repair)
	return fixture, supervisor, view, runID
}

// repairOperations are the run's journalled handoff.repair operations.
func repairOperations(t *testing.T, fixture *fleetFixture, runID string) []RunOperation {
	t.Helper()
	run, _, err := fixture.store.Run(runID)
	if err != nil {
		t.Fatal(err)
	}
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	var out []RunOperation
	for _, op := range snapshot.Operations {
		if op.Kind == OpHandoffRepair {
			out = append(out, op)
		}
	}
	return out
}

// operationBefores counts the run's journalled operation.before records for
// one kind: every start of an operation, whether or not it reached a provider.
func operationBefores(t *testing.T, fixture *fleetFixture, runID, kind string) int {
	t.Helper()
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	starts := 0
	for _, event := range events {
		if event.Type != EventOperationBefore {
			continue
		}
		op, err := decodePayload[RunOperation](event.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind == kind {
			starts++
		}
	}
	return starts
}

// committedCandidate is the commit the run's engineering invocation produced.
func committedCandidate(t *testing.T, fixture *fleetFixture, runID string) CandidateCommittedPayload {
	t.Helper()
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	var found []CandidateCommittedPayload
	for _, event := range events {
		if event.Type == EventCandidateCommitted {
			payload, err := decodePayload[CandidateCommittedPayload](event.Payload)
			if err != nil {
				t.Fatal(err)
			}
			found = append(found, payload)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d candidate commits journalled, want exactly the engineering one", len(found))
	}
	return found[0]
}

// gitIn runs one read-only Git query in the candidate workspace.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestCompletedWithUnresolvedIsRepairedOnceIntoPartial is the #159 dogfood
// shape. The engineering is done and committed; the worker wrote "completed"
// with an unresolved item. One result-only correction rewrites it as partial,
// the exact admission path binds it to the commit the engineering produced,
// and the engineering is never invoked again.
func TestCompletedWithUnresolvedIsRepairedOnceIntoPartial(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairWritesPartial)
	settled := fixture.drive(supervisor, view.BatchID)

	item := itemFor(t, settled, runID)
	if item.State != orchestration.ItemPartial || item.HandoffRepair != HandoffRepairRepaired {
		t.Fatalf("item %s repair %q (%s), want partial/repaired", item.State, item.HandoffRepair, item.Reason)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations, want exactly one of each", engineering, repairs)
	}
	if settled.HandoffRepairs != (HandoffRepairCounts{Started: 1, Repaired: 1}) {
		t.Fatalf("batch repair counts %+v", settled.HandoffRepairs)
	}
	handoff := onlyHandoff(t, fixture, runID)
	committed := committedCandidate(t, fixture, runID)
	if handoff.Subject.CandidateRevision != committed.Commit || handoff.Subject.CandidateTree != committed.Tree {
		t.Fatalf("the repaired handoff binds %+v, not the engineering commit %s/%s", handoff.Subject, committed.Commit, committed.Tree)
	}
	if handoff.ProducerReport.Outcome != orchestration.OutcomePartial || len(handoff.ProducerReport.Unresolved) != 1 {
		t.Fatalf("admitted report %+v", handoff.ProducerReport)
	}
	repairs := repairOperations(t, fixture, runID)
	if len(repairs) != 1 || handoff.ProtocolRepair == nil || handoff.ProtocolRepair.OperationID != repairs[0].ID ||
		handoff.Producer.OperationID == repairs[0].ID {
		t.Fatalf("handoff producer %+v repair %+v; the engineering invocation must stay the producer", handoff.Producer, handoff.ProtocolRepair)
	}

	// WHAT THE REPAIR WAS GIVEN: an empty runtime-owned directory, not the
	// candidate; a fresh slot outside it; no bypass; the exact refusal and
	// the refused bytes as data; the candidate as context only.
	request := fixture.worker.repairRequests[runID]
	candidate := candidateDir(fixture.stateDir, runID)
	if request.CandidateDir == candidate || strings.HasPrefix(request.CandidateDir, candidate+string(filepath.Separator)) ||
		strings.HasPrefix(request.HandoffPath, candidate+string(filepath.Separator)) {
		t.Fatalf("the repair was handed the candidate: dir %s slot %s", request.CandidateDir, request.HandoffPath)
	}
	if entries, err := os.ReadDir(request.CandidateDir); err != nil || len(entries) != 0 {
		t.Fatalf("the repair's working directory holds %d entries (%v), want an empty one", len(entries), err)
	}
	if !request.DenyPermissionBypass || len(request.RequiredTools) != 0 || len(request.Findings) != 0 || request.ScratchDir != "" ||
		request.MessagePath != "" {
		t.Fatalf("the repair request widened its grant: %+v", request)
	}
	if !strings.Contains(request.Objective, "completed and unresolved are different answers") ||
		!strings.Contains(request.Objective, fleetCompletedUnresolvedReport) || request.Candidate.Revision != committed.Commit {
		t.Fatalf("the repair brief lacks the refusal, the refused document or the candidate: %q", request.Objective)
	}
	if prompt := providerPrompt(request); !strings.Contains(prompt, "HANDOFF REPAIR ONLY") || strings.Contains(prompt, "Modify only") {
		t.Fatalf("the repair was prompted as engineering: %q", prompt)
	}
}

// TestMalformedJSONGetsOneRepair: a truncated document is a bounded protocol
// mistake, and one correction completes the item.
func TestMalformedJSONGetsOneRepair(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetMalformedJSON, repairWritesCompleted)
	item := itemFor(t, fixture.drive(supervisor, view.BatchID), runID)
	if item.State != orchestration.ItemCompleted || item.HandoffRepair != HandoffRepairRepaired {
		t.Fatalf("item %s repair %q (%s)", item.State, item.HandoffRepair, item.Reason)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations", engineering, repairs)
	}
}

// TestASecondInvalidHandoffEndsTheRepair: the corrected document crosses the
// SAME strict decoder - one restating the candidate is refused exactly as an
// engineering worker's would be - and a refused correction is the end. No
// third invocation, however long the supervisor keeps running.
func TestASecondInvalidHandoffEndsTheRepair(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairRestatesCandidate)
	fixture.drive(supervisor, view.BatchID)
	for range 5 {
		if _, err := fixture.supervisor().Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	item := itemFor(t, fixture.status(view.BatchID), runID)
	if item.State != orchestration.ItemHandoffPending || item.Handoff != orchestration.HandoffRefused || item.HandoffRepair != HandoffRepairRefused {
		t.Fatalf("item %s handoff %s repair %q", item.State, item.Handoff, item.HandoffRepair)
	}
	if !strings.Contains(item.Reason, "handoff protocol repair refused") || !strings.Contains(item.Reason, "candidate_revision") {
		t.Fatalf("the refused repair is not visible: %q", item.Reason)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations, want one each", engineering, repairs)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted (%v)", len(handoffs), err)
	}
}

// TestAnUnrepairableRefusalNeverReachesAModel: an oversized document, a
// missing one, and a repairable mistake from an invocation with no committed
// candidate to bind all settle handoff_pending without a repair invocation.
func TestAnUnrepairableRefusalNeverReachesAModel(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(3))
	want := map[string]string{
		view.Items[0].RunID: "",
		view.Items[1].RunID: "",
		view.Items[2].RunID: HandoffRepairIneligible,
	}
	fixture.worker.set(view.Items[0].RunID, fleetOversizedHandoff)
	fixture.worker.set(view.Items[1].RunID, fleetNoHandoff)
	fixture.worker.set(view.Items[2].RunID, fleetNoChangeInvalidHandoff)
	for runID := range want {
		fixture.worker.setRepair(runID, repairWritesPartial)
	}
	settled := fixture.drive(supervisor, view.BatchID)
	for runID, state := range want {
		item := itemFor(t, settled, runID)
		if _, repairs := fixture.worker.counts(runID); repairs != 0 || len(repairOperations(t, fixture, runID)) != 0 {
			t.Fatalf("issue %d: %d repair invocations", item.Issue, repairs)
		}
		if item.State != orchestration.ItemHandoffPending || item.HandoffRepair != state {
			t.Fatalf("issue %d: %s repair %q (%s), want handoff_pending repair %q", item.Issue, item.State, item.HandoffRepair, item.HandoffRepairDetail, state)
		}
	}
	if settled.HandoffRepairs != (HandoffRepairCounts{}) {
		t.Fatalf("batch repair counts %+v", settled.HandoffRepairs)
	}
}

// TestTheRepairDecisionIsAFunctionOfTheJournal drives the classifier the
// planner binds through over a real run's journal, cut at the points a
// controller can be lost. Before the repair is planned, and while it is only
// planned, a restarted controller may start it: no provider was reached. Once
// its operation.before is journalled the budget is spent for every controller
// after. And a refusal the candidate has moved past, or a later engineering
// invocation exists for, is never repaired at all.
func TestTheRepairDecisionIsAFunctionOfTheJournal(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairWritesPartial)
	fixture.drive(supervisor, view.BatchID)
	run, _, err := fixture.store.Run(runID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	planned, started := -1, -1
	for i, event := range all {
		if event.Type != EventOperationPlanned && event.Type != EventOperationBefore {
			continue
		}
		op, err := decodePayload[RunOperation](event.Payload)
		if err != nil || op.Kind != OpHandoffRepair {
			continue
		}
		if event.Type == EventOperationPlanned && planned < 0 {
			planned = i
		}
		if event.Type == EventOperationBefore && started < 0 {
			started = i
		}
	}
	if planned < 0 || started < 0 {
		t.Fatal("the run never planned and started its repair")
	}
	// plan replays the journal up to replayed, reads it up to read, and asks
	// the planner whether it wants a repair.
	plan := func(replayed, read []EngineeringEvent, mutate func(map[string]RunOperation)) (handoffRepairTarget, bool) {
		snapshot, err := Reduce(run, replayed)
		if err != nil {
			t.Fatal(err)
		}
		projection, err := Project(replayed)
		if err != nil {
			t.Fatal(err)
		}
		if mutate != nil {
			mutate(snapshot.Operations)
		}
		state := &runState{run: run, events: read, snapshot: snapshot, projection: projection}
		target, err := inspectHandoffRepair(read, snapshot.Operations, state.providerCeiling())
		if err != nil {
			t.Fatal(err)
		}
		_, wanted := bindHandoffRepair(state)
		return target, wanted
	}
	refused, onlyPlanned, spent := all[:planned], all[:planned+1], all[:started+1]
	for name, cut := range map[string][]EngineeringEvent{"refused": refused, "planned": onlyPlanned} {
		if target, wanted := plan(cut, cut, nil); !target.eligible || !wanted {
			t.Fatalf("a controller restarted after the journal reads %s is not offered the repair: %+v", name, target)
		}
	}
	if target, wanted := plan(spent, spent, nil); wanted || target.state != HandoffRepairRunning {
		t.Fatalf("a controller restarted after the repair started was offered another: %+v", target)
	}
	moved := append(append([]EngineeringEvent(nil), refused...), EngineeringEvent{
		Type: EventCandidateBaseIntegrated, RunID: runID, Payload: []byte(`{}`),
	})
	if target, wanted := plan(refused, moved, nil); wanted || target.state != HandoffRepairIneligible || !strings.HasPrefix(target.detail, "superseded") {
		t.Fatalf("a candidate moved past the refused commit was still repaired: %+v", target)
	}
	// The deterministic gofmt commit of the same output formats the work; it
	// does not replace it, so it supersedes nothing. Any other commit does.
	for producer, superseded := range map[string]bool{OpRemediationGofmt: false, OpExecutionInvoke: true} {
		commit := append(append([]EngineeringEvent(nil), refused...), EngineeringEvent{
			Type: EventCandidateCommitted, RunID: runID, OperationID: "op-commit-2", Payload: []byte(`{}`),
		})
		followed := func(operations map[string]RunOperation) {
			operations["op-producer-2"] = RunOperation{ID: "op-producer-2", Kind: producer}
			operations["op-commit-2"] = RunOperation{ID: "op-commit-2", Kind: OpCandidateCommit,
				IdempotencyKey: operationKey(OpCandidateCommit, "op-producer-2")}
		}
		if _, wanted := plan(refused, commit, followed); wanted == superseded {
			t.Fatalf("a later %s commit: repair wanted=%t", producer, wanted)
		}
	}
	// A zero-delta continuation binds only through the checkpoint it proves it
	// completed (#489). One naming a checkpoint the journal never recorded is
	// unbindable, and the planner says so rather than repairing it.
	unprovenCheckpoint := func(operations map[string]RunOperation) {
		for id, op := range operations {
			switch op.Kind {
			case OpCandidateCommit:
				delete(operations, id)
			case OpExecutionInvoke:
				op.IdempotencyKey = operationKey(OpExecutionInvoke, invocationContinuationPrefix+"0123456789abcdef")
				op.Result = []byte(`{"mutated":false,"checkpoint_resolved":true}`)
				operations[id] = op
			}
		}
	}
	if target, wanted := plan(refused, refused, unprovenCheckpoint); wanted || target.state != HandoffRepairIneligible ||
		!strings.HasPrefix(target.detail, "unbindable: the continuation's checkpoint") {
		t.Fatalf("an unproven checkpoint binding was still repaired: %+v", target)
	}
	// A LATER ENGINEERING START is decided by journal order, never by clock:
	// the later invocation here was created in the very same instant as the
	// one that wrote the refused handoff, and still supersedes it.
	var producer RunOperation
	for _, op := range snapshotOf(t, run, refused).Operations {
		if op.Kind == OpExecutionInvoke {
			producer = op
		}
	}
	sameInstant := producer
	sameInstant.ID, sameInstant.Attempt, sameInstant.AttemptIdentity = "op-later", 1, 1
	sameInstant.IdempotencyKey = operationKey(OpExecutionInvoke, "remediation|same-instant")
	startedLater, err := marshalPayloadJSON(sameInstant)
	if err != nil {
		t.Fatal(err)
	}
	if !sameInstant.CreatedAt.Equal(producer.CreatedAt) {
		t.Fatal("the regression needs two invocations created in the same instant")
	}
	later := append(append([]EngineeringEvent(nil), refused...), EngineeringEvent{
		Type: EventOperationBefore, RunID: runID, OperationID: sameInstant.ID, Payload: startedLater,
	})
	if target, wanted := plan(refused, later, nil); wanted || target.state != HandoffRepairIneligible || !strings.Contains(target.detail, "started after") {
		t.Fatalf("a refusal a same-instant later engineering invocation superseded was still repaired: %+v", target)
	}
	// MaxProviderInvocations is a run total the repair spends from: with the
	// engineering invocation having spent the last one, an otherwise eligible
	// repair is refused before any provider, and says why.
	run.Budgets.MaxProviderInvocations = 1
	if target, wanted := plan(refused, refused, nil); wanted || target.state != HandoffRepairUnbudgeted ||
		!strings.Contains(target.detail, "spent 1 of its 1 provider invocations") {
		t.Fatalf("a repair past the provider-invocation ceiling was still offered: %+v", target)
	}
	run.Budgets.MaxProviderInvocations = 2
	if _, wanted := plan(refused, refused, nil); !wanted {
		t.Fatal("a repair within the provider-invocation ceiling was refused")
	}
	run.Budgets.MaxProviderInvocations = 0
	run.Orchestration = nil
	if _, wanted := plan(refused, refused, nil); wanted {
		t.Fatal("a run no batch created was offered a handoff repair")
	}
}

// TestARepairCannotMutateTheCandidate: a repair invocation that reaches past
// its empty working directory into the candidate workspace has its document
// refused, and the candidate - head, tree and every work-tree byte - is
// exactly the engineering commit afterwards.
func TestARepairCannotMutateTheCandidate(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairMutatesCandidate)
	item := itemFor(t, fixture.drive(supervisor, view.BatchID), runID)
	if item.State != orchestration.ItemHandoffPending || item.HandoffRepair != HandoffRepairFailed ||
		!strings.Contains(item.HandoffRepairDetail, "changed the candidate") {
		t.Fatalf("item %s repair %q (%s)", item.State, item.HandoffRepair, item.HandoffRepairDetail)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("a document from a repair that mutated the candidate was admitted: %d (%v)", len(handoffs), err)
	}
	committed := committedCandidate(t, fixture, runID)
	dir := candidateDir(fixture.stateDir, runID)
	if head, tree := gitIn(t, dir, "rev-parse", "HEAD"), gitIn(t, dir, "rev-parse", "HEAD^{tree}"); head != committed.Commit || tree != committed.Tree {
		t.Fatalf("candidate is %s/%s, want %s/%s", head, tree, committed.Commit, committed.Tree)
	}
	if status := gitIn(t, dir, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("the candidate work tree differs from its commit:\n%s", status)
	}
	if got := gitIn(t, dir, "show", "HEAD:candidate.go"); strings.Contains(got, "rewritten by the repair") {
		t.Fatal("the repair's write reached the committed candidate")
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations", engineering, repairs)
	}
}

// TestProviderFailureDuringRepairRerunsNothing: the repair's provider fails.
// The one repair is spent, the engineering is not invoked again, and the item
// settles handoff_pending with the failure visible.
func TestProviderFailureDuringRepairRerunsNothing(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairFails)
	settled := fixture.drive(supervisor, view.BatchID)
	item := itemFor(t, settled, runID)
	if item.State != orchestration.ItemHandoffPending || item.HandoffRepair != HandoffRepairFailed ||
		!strings.Contains(item.Reason, "the provider did not complete the repair") {
		t.Fatalf("item %s repair %q reason %q", item.State, item.HandoffRepair, item.Reason)
	}
	if settled.HandoffRepairs != (HandoffRepairCounts{Started: 1}) {
		t.Fatalf("batch repair counts %+v", settled.HandoffRepairs)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations", engineering, repairs)
	}
}

// TestTheRepairBudgetSurvivesRestart: every pass is a freshly built supervisor
// over the durable store, as a restarted `serve` is. The repair happens once
// and is never offered again.
func TestTheRepairBudgetSurvivesRestart(t *testing.T) {
	fixture, _, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairRestatesCandidate)
	for range 20 {
		if _, err := fixture.supervisor().Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations across restarts", engineering, repairs)
	}
	if starts := operationBefores(t, fixture, runID, OpHandoffRepair); starts != 1 {
		t.Fatalf("the repair operation was started %d times", starts)
	}
	if item := itemFor(t, fixture.status(view.BatchID), runID); item.HandoffRepair != HandoffRepairRefused {
		t.Fatalf("repair %q", item.HandoffRepair)
	}
}

// TestAControllerLostMidRepairNeverRepairsAgain: the controller dies while
// the repair's provider is running - after the budget was spent, before any
// result. Every restarted controller after it sees an interrupted repair and
// never invokes another.
func TestAControllerLostMidRepairNeverRepairsAgain(t *testing.T) {
	fixture, _, view, runID := repairRun(t, fleetCompletedWithUnresolved, repairWritesPartial)
	fixture.worker.crashOnRepair = true
	for range 20 {
		if _, err := fixture.supervisor().Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	if engineering, repairs := fixture.worker.counts(runID); engineering != 1 || repairs != 1 {
		t.Fatalf("%d engineering and %d repair invocations after a lost controller", engineering, repairs)
	}
	if starts := operationBefores(t, fixture, runID, OpHandoffRepair); starts != 1 {
		t.Fatalf("the repair operation was started %d times", starts)
	}
	item := itemFor(t, fixture.status(view.BatchID), runID)
	if item.HandoffRepair != HandoffRepairInterrupted && item.HandoffRepair != HandoffRepairRunning {
		t.Fatalf("repair %q (%s)", item.HandoffRepair, item.Reason)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs (%v)", len(handoffs), err)
	}
}

// TestAValidHandoffIsNeverRepaired: the ordinary path is exactly as before -
// no repair operation, no repair state, nothing recorded on the handoff.
func TestAValidHandoffIsNeverRepaired(t *testing.T) {
	fixture, supervisor, view, runID := repairRun(t, fleetValidHandoff, repairWritesPartial)
	settled := fixture.drive(supervisor, view.BatchID)
	item := itemFor(t, settled, runID)
	if item.State != orchestration.ItemCompleted || item.HandoffRepair != "" || settled.HandoffRepairs != (HandoffRepairCounts{}) {
		t.Fatalf("item %s repair %q counts %+v", item.State, item.HandoffRepair, settled.HandoffRepairs)
	}
	if _, repairs := fixture.worker.counts(runID); repairs != 0 || len(repairOperations(t, fixture, runID)) != 0 {
		t.Fatal("a valid handoff was repaired")
	}
	if onlyHandoff(t, fixture, runID).ProtocolRepair != nil {
		t.Fatal("a valid handoff records a protocol repair")
	}
}

// snapshotOf replays one cut of a run's journal.
func snapshotOf(t *testing.T, run EngineeringRun, events []EngineeringEvent) RunSnapshot {
	t.Helper()
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// crashRepair ends the driving goroutine mid-invocation, as a lost controller
// does: no result, no operation.after, and nothing else of that pass runs.
func crashRepair() { goruntime.Goexit() }
