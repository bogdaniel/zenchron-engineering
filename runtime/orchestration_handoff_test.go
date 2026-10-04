package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// handoffEvents returns a run's handoff observations, in journal order.
func handoffEvents(t *testing.T, store *SQLiteOperationStore, runID string) []EngineeringEvent {
	t.Helper()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []EngineeringEvent
	for _, event := range events {
		if event.Type == EventHandoffReported || event.Type == EventHandoffRefused {
			out = append(out, event)
		}
	}
	return out
}

func itemFor(t *testing.T, view OrchestrationView, runID string) OrchestrationItemView {
	t.Helper()
	for _, item := range view.Items {
		if item.RunID == runID {
			return item
		}
	}
	t.Fatalf("run %s is not an item of batch %s", runID, view.BatchID)
	return OrchestrationItemView{}
}

// TestProviderSuccessWithoutAValidHandoffIsNotCompletion is #470 acceptance
// E: a worker that succeeds and writes nothing, one that writes a document
// claiming a run identity, and one that writes a perfect document into the
// candidate REPOSITORY instead of the runtime's slot all leave their item
// handoff_pending. None of them is completed from prose, from repository
// content, or from the provider having returned success.
func TestProviderSuccessWithoutAValidHandoffIsNotCompletion(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(4))
	silent, forged, seeded := view.Items[0].RunID, view.Items[1].RunID, view.Items[2].RunID
	fixture.worker.set(silent, fleetNoHandoff)
	fixture.worker.set(forged, fleetMalformedHandoff)
	fixture.worker.set(seeded, fleetRepositorySeeded)
	settled := fixture.drive(supervisor, view.BatchID)

	for runID, kind := range map[string]string{silent: HandoffMissing, forged: HandoffInvalid, seeded: HandoffMissing} {
		item := itemFor(t, settled, runID)
		if item.State != orchestration.ItemHandoffPending || item.Handoff != orchestration.HandoffRefused || item.HandoffID != "" {
			t.Fatalf("issue %d: state %s handoff %s id %q, want handoff_pending/refused with nothing admitted", item.Issue, item.State, item.Handoff, item.HandoffID)
		}
		// The provider DID succeed: its output was committed. That is
		// exactly why this is the case that matters.
		if item.CandidateRevision == "" {
			t.Fatalf("issue %d never produced a candidate, so the test proves nothing about provider success", item.Issue)
		}
		events := handoffEvents(t, fixture.store, runID)
		if len(events) == 0 || events[len(events)-1].Type != EventHandoffRefused {
			t.Fatalf("issue %d journalled %v, want a typed refusal", item.Issue, journalTypes(events))
		}
		refused, err := decodePayload[HandoffRefusedPayload](events[len(events)-1].Payload)
		if err != nil || refused.Kind != kind {
			t.Fatalf("issue %d refusal = %+v (%v), want kind %s", item.Issue, refused, err, kind)
		}
		handoffs, err := fixture.store.RunHandoffs(runID)
		if err != nil || len(handoffs) != 0 {
			t.Fatalf("issue %d has %d admitted handoffs (%v)", item.Issue, len(handoffs), err)
		}
	}
	if settled.Counts.HandoffPending != 3 || settled.Counts.Completed != 1 {
		t.Fatalf("counts = %+v", settled.Counts)
	}
}

// TestAValidHandoffIsBoundToWhatTheRuntimeObserved is #470 acceptance F. The
// admitted handoff's run, issue, batch, base, candidate, tree, contract,
// changed paths and producer all come from the runtime's own records; the
// worker contributed the report and nothing else. The item becomes completed
// only once that admission exists, not when the report is first journalled.
func TestAValidHandoffIsBoundToWhatTheRuntimeObserved(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "codex", fleetIssues(1))
	runID := view.Items[0].RunID

	reportedBeforeAdmission := false
	for range 40 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		item := fixture.status(view.BatchID).Items[0]
		if item.Handoff == orchestration.HandoffReported {
			reportedBeforeAdmission = true
			if item.State == orchestration.ItemCompleted {
				t.Fatal("an item completed on a journalled report the runtime had not yet admitted")
			}
		}
		if item.State == orchestration.ItemCompleted {
			break
		}
	}
	if !reportedBeforeAdmission {
		t.Fatal("the report was never observed before its admission, so the ordering was not tested")
	}
	item := fixture.status(view.BatchID).Items[0]
	handoffs, err := fixture.store.RunHandoffs(runID)
	if err != nil || len(handoffs) != 1 || item.State != orchestration.ItemCompleted || item.HandoffID != handoffs[0].ID {
		t.Fatalf("state %s, handoffs %d (%v)", item.State, len(handoffs), err)
	}
	handoff := handoffs[0]

	// The runtime's own record of the commit this handoff must bind to.
	var committed CandidateCommittedPayload
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == EventCandidateCommitted {
			if committed, err = decodePayload[CandidateCommittedPayload](event.Payload); err != nil {
				t.Fatal(err)
			}
		}
	}
	reported := handoffEvents(t, fixture.store, runID)
	payload, err := decodePayload[HandoffReportedPayload](reported[len(reported)-1].Payload)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	want := orchestration.EngineeringHandoff{
		BatchID: view.BatchID, Issue: fleetFirstIssue, RunID: runID,
		Producer: orchestration.HandoffProducer{AgentID: "codex", OperationID: payload.OperationID, Attempt: payload.Attempt},
		Subject:  orchestration.HandoffSubject{BaseRevision: fixture.base, CandidateRevision: committed.Commit, CandidateTree: committed.Tree},
		Governance: orchestration.HandoffGovernance{
			ContractID: projection.Contract.ID, ContractRevision: projection.Contract.Revision,
		},
		Observed: orchestration.HandoffObserved{ChangedPathCount: committed.PathCount, ChangedPathsDigest: committed.PathsDigest},
	}
	got := handoff
	got.SchemaVersion, got.ID, got.ReportSHA256, got.ProducerReport = "", "", "", orchestration.HandoffReport{}
	want.AdmittedAt = got.AdmittedAt
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("admitted bindings\n got %+v\nwant %+v", got, want)
	}
	if committed.Commit == "" || committed.Commit != item.CandidateRevision {
		t.Fatalf("the handoff binds %s and the run's candidate is %s", committed.Commit, item.CandidateRevision)
	}
	decoded, err := orchestration.DecodeHandoffReport([]byte(fleetValidReport))
	if err != nil {
		t.Fatal(err)
	}
	if handoff.ProducerReport.Summary != decoded.Summary || handoff.ProducerReport.Outcome != decoded.Outcome {
		t.Fatalf("producer report = %+v", handoff.ProducerReport)
	}
	// The worker was told where to write, and that path is the runtime's,
	// outside the workspace it was told to modify.
	request := fixture.worker.request(runID)
	if request.HandoffPath == "" || filepath.Dir(request.HandoffPath) == request.CandidateDir ||
		!filepath.IsAbs(request.HandoffPath) || strings.HasPrefix(request.HandoffPath, request.CandidateDir) {
		t.Fatalf("handoff slot %q is not a runtime-owned path outside %q", request.HandoffPath, request.CandidateDir)
	}
	if prompt := providerPrompt(request); !strings.Contains(prompt, request.HandoffPath) || !strings.Contains(prompt, "REQUIRED HANDOFF") {
		t.Fatal("the worker was not told the handoff is required or where it goes")
	}
}

// TestALeftoverHandoffCannotBeInherited is #470 acceptance G: the slot the
// runtime is about to hand an invocation is emptied first, and a document in
// any OTHER attempt's slot is never read as this one's.
func TestALeftoverHandoffCannotBeInherited(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	engine, err := fixture.supervisor().engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	earlier, err := HandoffReportPath(fixture.stateDir, ExecutionAttemptRef{RunID: runID, OperationID: "op-x", Attempt: 1})
	if err != nil {
		t.Fatal(err)
	}
	current, err := HandoffReportPath(fixture.stateDir, ExecutionAttemptRef{RunID: runID, OperationID: "op-x", Attempt: 2})
	if err != nil {
		t.Fatal(err)
	}
	if earlier == current {
		t.Fatal("two attempts share one handoff slot")
	}
	for _, path := range []string{earlier, current} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(fleetValidReport), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	prepared, err := engine.prepareHandoffSlot(state, "op-x", 2)
	if err != nil || prepared != current {
		t.Fatalf("prepared %q (%v), want %q", prepared, err, current)
	}
	if _, err := os.Stat(current); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the leftover in this attempt's slot survived preparation: %v", err)
	}
	if observed := handoffObservation(prepared, "op-x", 2); observed.Type != EventHandoffRefused {
		t.Fatalf("attempt 2 inherited attempt 1's handoff: %s", observed.Type)
	}
	// A planted link is refused rather than followed.
	if err := os.Symlink(earlier, current); err != nil {
		t.Fatal(err)
	}
	observed := handoffObservation(current, "op-x", 2)
	if refused, ok := observed.Payload.(HandoffRefusedPayload); !ok || refused.Kind != HandoffInvalid {
		t.Fatalf("a symlinked slot was read: %+v", observed)
	}
	// A run no batch created is given no slot at all.
	plain := fixture.start()
	plainState, err := engine.load(plain)
	if err != nil {
		t.Fatal(err)
	}
	if path, err := engine.prepareHandoffSlot(plainState, "op-y", 1); err != nil || path != "" {
		t.Fatalf("an ordinary run was given handoff slot %q (%v)", path, err)
	}
}

// TestARestartRecreatesNothingAndRecoversWhatDidNotLand is #470 acceptance
// D: a supervisor that died after writing the batch and before creating every
// child is replaced by one built from the reopened store, which creates
// exactly the missing children under the identities the batch already names.
// Admitted handoffs and completed items survive a further restart unchanged,
// and nothing completed is executed again.
func TestARestartRecreatesNothingAndRecoversWhatDidNotLand(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	first := fixture.supervisor()
	engine, err := first.engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	id, err := orchestration.BatchID("acme/repo", "claude", fleetIssues(10))
	if err != nil {
		t.Fatal(err)
	}
	planned, err := engine.planOrchestrationBatch(id, fleetIssues(10), "operator@example", nil)
	if err != nil {
		t.Fatal(err)
	}
	batch, created, err := fixture.store.CreateOrchestrationBatch(planned)
	if err != nil || !created {
		t.Fatalf("created=%t err=%v", created, err)
	}
	for _, item := range batch.Items[:4] {
		if err := engine.materializeOrchestratedRun(context.Background(), batch.ID, item); err != nil {
			t.Fatal(err)
		}
	}
	// Before anything recovers it, a second batch naming an issue whose child
	// was never created must not decide the same run identity.
	_, err = first.Orchestrate(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: "claude", Issues: []int{batch.Items[7].Issue, fleetFirstIssue + 50},
	})
	var conflict *OrchestrationConflictError
	if !errors.As(err, &conflict) || conflict.Batch != batch.ID || conflict.RunID != batch.Items[7].RunID {
		t.Fatalf("err = %v, want a conflict naming batch %s's uncreated run", err, batch.ID)
	}
	// The process dies here. The store is closed and reopened.
	reopen := func() {
		if err := fixture.store.Close(); err != nil {
			t.Fatal(err)
		}
		store, err := OpenSQLiteOperationStore(fixture.stateDir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { store.Close() })
		fixture.store = store
	}
	reopen()
	if view := fixture.status(batch.ID); view.Counts.NotCreated != 6 {
		t.Fatalf("after the crash: %+v", view.Counts)
	}
	settled := fixture.drive(fixture.supervisor(), batch.ID)
	runs, err := fixture.store.Runs()
	if err != nil || len(runs) != 10 {
		t.Fatalf("%d runs after recovery (%v), want exactly the ten the batch names", len(runs), err)
	}
	for i, item := range settled.Items {
		if item.RunID != batch.Items[i].RunID || item.State != orchestration.ItemCompleted {
			t.Fatalf("item %d: run %s state %s, want the batch's own run %s completed", item.Issue, item.RunID, item.State, batch.Items[i].RunID)
		}
	}
	fixture.worker.mu.Lock()
	before := len(fixture.worker.invocations)
	invoked := map[string]int{}
	for run, n := range fixture.worker.invocations {
		invoked[run] = n
	}
	fixture.worker.mu.Unlock()

	reopen()
	again := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(10))
	restarted := fixture.supervisor()
	for range 3 {
		if _, err := restarted.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}
	after := fixture.status(batch.ID)
	if again.BatchID != batch.ID || after.Counts != settled.Counts {
		t.Fatalf("the restart changed the batch: %+v then %+v", settled.Counts, after.Counts)
	}
	for i := range after.Items {
		if after.Items[i].HandoffID != settled.Items[i].HandoffID || after.Items[i].HandoffID == "" {
			t.Fatalf("issue %d handoff %q became %q across a restart", after.Items[i].Issue, settled.Items[i].HandoffID, after.Items[i].HandoffID)
		}
	}
	fixture.worker.mu.Lock()
	defer fixture.worker.mu.Unlock()
	if len(fixture.worker.invocations) != before {
		t.Fatalf("the restart invoked %d new children", len(fixture.worker.invocations)-before)
	}
	for run, n := range fixture.worker.invocations {
		if invoked[run] != n {
			t.Fatalf("completed run %s was executed again after the restart", run)
		}
	}
}

// TestABatchRefusesAnIssueThatAlreadyHasLiveWork: orchestration never adopts
// or races work it did not create, and a refused request writes nothing.
func TestABatchRefusesAnIssueThatAlreadyHasLiveWork(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	fixture.issue = fleetFirstIssue + 2
	existing := fixture.start()
	supervisor := fixture.supervisor()
	_, err := supervisor.Orchestrate(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: "claude", Issues: fleetIssues(5),
	})
	var conflict *OrchestrationConflictError
	if !errors.As(err, &conflict) || conflict.RunID != existing {
		t.Fatalf("err = %v, want a conflict naming %s", err, existing)
	}
	batches, err := fixture.store.OrchestrationBatches()
	if err != nil || len(batches) != 0 {
		t.Fatalf("a refused batch was written: %d (%v)", len(batches), err)
	}
	runs, err := fixture.store.Runs()
	if err != nil || len(runs) != 1 {
		t.Fatalf("a refused batch created runs: %d (%v)", len(runs), err)
	}
	// A live run of a named issue created under a DIFFERENT configuration
	// lives in another identity space; it is still live work on that issue.
	otherDeps := fixture.deps
	otherDeps.ConfigDigest = ConfigDigest{Global: "g2", Repository: "r2"}
	other := fixture.newRuntime(otherDeps)
	elsewhere, err := other.StartOrResumeIssueRun(context.Background(), fleetFirstIssue+20)
	if err != nil {
		t.Fatal(err)
	}
	_, err = supervisor.Orchestrate(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: "claude", Issues: []int{fleetFirstIssue + 20},
	})
	if !errors.As(err, &conflict) || conflict.RunID != elsewhere {
		t.Fatalf("err = %v, want a conflict naming %s, the live run under another configuration", err, elsewhere)
	}
	for name, request := range map[string]ControlRequest{
		"no agent":        {Repository: "acme/repo", Issues: []int{150}},
		"unknown agent":   {Repository: "acme/repo", Agent: "gpt", Issues: []int{150}},
		"ungoverned repo": {Repository: "other/repo", Agent: "claude", Issues: []int{150}},
		"duplicate issue": {Repository: "acme/repo", Agent: "claude", Issues: []int{150, 150}},
	} {
		if _, err := supervisor.Orchestrate(context.Background(), request); err == nil {
			t.Errorf("%s: the request was accepted", name)
		}
	}
}
