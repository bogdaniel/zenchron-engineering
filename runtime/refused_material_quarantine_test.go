package runtime

// refused_material_quarantine_test.go pins #390's two binding cross-attempt
// regressions, both driven through the real EngineeringRuntime and asserted
// against the persisted journal and the live candidate workspace.
//
// The shape both reproduce is the live one: an invocation mutates the
// workspace and returns a refusal (provider_background_work_unresolved, as
// #389/#393/#400/#408 did). On main before this repair that operation was
// recorded Succeeded+failure_class+mutated and candidate.commit committed the
// refused bytes; with the admission fix alone, the bytes stayed dirty and the
// same binding's next successful attempt committed them as its own.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// refusedWrite is attempt 1: it creates material A and reports a refusal that
// routes to a same-binding retry.
func refusedWrite() providerAnswer {
	return providerAnswer{
		result: ExecutionResult{
			ProviderID: "test-provider",
			Outcome:    execution.Failed,
			Failure: &ProviderFailure{
				Classification:   FailureProviderBackgroundWorkUnresolved,
				RawDiagnosticRef: "diagnostic-background",
			},
		},
		mutate: func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "refused.go"), []byte("package refused // A\n"), 0o600)
		},
	}
}

func quarantinedEvents(t *testing.T, events []EngineeringEvent) []CandidateQuarantinedPayload {
	t.Helper()
	var out []CandidateQuarantinedPayload
	for _, e := range events {
		if e.Type != EventCandidateQuarantined {
			continue
		}
		var p CandidateQuarantinedPayload
		if err := decodeJSON(e.Payload, &p); err != nil {
			t.Fatalf("candidate.quarantined payload unreadable: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// assertQuarantinedA proves material A was preserved - copied, byte for byte,
// into the journalled location - and is no longer in the candidate workspace.
func assertQuarantinedA(t *testing.T, fixture *phase8Fixture, runID string) {
	t.Helper()
	state := fixture.state(runID)
	quarantined := quarantinedEvents(t, state.events)
	if len(quarantined) != 1 {
		t.Fatalf("journal holds %d candidate.quarantined events, want exactly one: %v", len(quarantined), journalTypes(state.events))
	}
	q := quarantined[0]
	if q.FailureClass != FailureProviderBackgroundWorkUnresolved || q.PathCount != 1 || q.Attempt != 1 || !q.Restored {
		t.Fatalf("quarantine record = %+v, want attempt 1's one refused path under its refusal class", q)
	}
	kept, err := os.ReadFile(filepath.Join(fixture.stateDir, q.Location, "files", "refused.go"))
	if err != nil || string(kept) != "package refused // A\n" {
		t.Fatalf("material A was not preserved at %s: %q, %v", q.Location, kept, err)
	}
	if _, err := os.Stat(filepath.Join(fixture.stateDir, q.Location, "manifest.json")); err != nil {
		t.Fatalf("quarantine carries no manifest: %v", err)
	}
	workspace := candidateDir(fixture.stateDir, runID)
	if _, err := os.Stat(filepath.Join(workspace, "refused.go")); !os.IsNotExist(err) {
		t.Fatalf("material A is still in the candidate workspace after its refusal (err=%v)", err)
	}
}

// committedTreeHas reports whether the run's recorded candidate commit carries
// path, read from Git rather than from the workspace.
func committedTreeHas(t *testing.T, fixture *phase8Fixture, runID, path string) bool {
	t.Helper()
	state := fixture.state(runID)
	if state.projection.CandidateRevision == "" {
		return false
	}
	out, err := gitOutput(candidateDir(fixture.stateDir, runID), "ls-tree", "-r", "--name-only", state.projection.CandidateRevision)
	if err != nil {
		t.Fatalf("ls-tree: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == path {
			return true
		}
	}
	return false
}

// TestRefusedMaterialIsNotAdmittedByALaterNoChangeSuccess: attempt 1 creates A
// and is refused; attempt 2 succeeds and changes nothing. A must never become
// a completed candidate, an assurance subject or a publication subject.
func TestRefusedMaterialIsNotAdmittedByALaterNoChangeSuccess(t *testing.T) {
	if RouteFailure(FailureProviderBackgroundWorkUnresolved) != RouteRetry {
		t.Fatal("this test depends on the refusal routing to a same-binding retry")
	}
	fixture, provider := newRoutingFixture(t, 3,
		refusedWrite(),
		providerAnswer{result: ExecutionResult{ProviderID: "test-provider", Outcome: execution.Succeeded}},
	)
	runID := fixture.start()
	fixture.reconcile(runID)

	if provider.calls < 2 {
		t.Fatalf("the refused attempt was not retried (%d provider calls)", provider.calls)
	}
	assertQuarantinedA(t, fixture, runID)
	state := fixture.state(runID)
	if n := countType(state.events, EventCandidateCommitted) + countType(state.events, EventCandidateCheckpointed); n != 0 {
		t.Fatalf("a no-change success produced %d runtime commit(s); the only material was the refused A: %v", n, journalTypes(state.events))
	}
	if n := countType(state.events, EventAssuranceObserved); n != 0 {
		t.Fatalf("assurance ran %d time(s) with no admitted material: %v", n, journalTypes(state.events))
	}
	if committedTreeHas(t, fixture, runID, "refused.go") {
		t.Fatal("refused material A reached a recorded candidate commit")
	}
}

// TestRefusedMaterialIsNotAttributedToALaterSuccessThatCreatesB: attempt 1
// creates A and is refused; attempt 2 succeeds creating B. The admitted
// candidate carries B and only B.
func TestRefusedMaterialIsNotAttributedToALaterSuccessThatCreatesB(t *testing.T) {
	fixture, _ := newRoutingFixture(t, 3,
		refusedWrite(),
		providerAnswer{
			result: ExecutionResult{ProviderID: "test-provider", Outcome: execution.Succeeded},
			mutate: writesCandidate,
		},
	)
	runID := fixture.start()
	fixture.reconcile(runID)

	assertQuarantinedA(t, fixture, runID)
	state := fixture.state(runID)
	if countType(state.events, EventCandidateCommitted) != 1 {
		t.Fatalf("attempt 2's admitted material B was not committed exactly once: %v", journalTypes(state.events))
	}
	if !committedTreeHas(t, fixture, runID, "candidate.go") {
		t.Fatal("the committed candidate does not carry attempt 2's material B")
	}
	if committedTreeHas(t, fixture, runID, "refused.go") {
		t.Fatal("refused material A was attributed to attempt 2 and committed with B")
	}
	op, _ := durableInvoke(t, fixture, runID)
	var record mutationResult
	if err := decodeJSON(op.Result, &record); err != nil {
		t.Fatal(err)
	}
	if record.PathCount != 1 {
		t.Fatalf("attempt 2's admitted result names %d paths, want exactly its own one", record.PathCount)
	}
}

// TestARefusedMutationIsNotACompletedExecution is the immediate admission
// half: a refusal that mutated is a FAILED operation, never Succeeded, and its
// material never reaches candidate.commit - whatever later attempts do.
func TestARefusedMutationIsNotACompletedExecution(t *testing.T) {
	fixture, _ := newRoutingFixture(t, 1, refusedWrite())
	runID := fixture.start()
	fixture.reconcile(runID)

	op, _ := durableInvoke(t, fixture, runID)
	if op.State != OperationFailed {
		t.Fatalf("a refused, mutating invocation settled %q, want failed", op.State)
	}
	state := fixture.state(runID)
	if n := countType(state.events, EventCandidateCommitted) + countType(state.events, EventCandidateCheckpointed); n != 0 {
		t.Fatalf("refused material was committed %d time(s): %v", n, journalTypes(state.events))
	}
	assertQuarantinedA(t, fixture, runID)
	// The terminal record does not say "nothing is held": it names the
	// quarantine the refused material was preserved in.
	var terminal dispositionRecord
	for _, e := range state.events {
		if e.Type == EventRunFailed {
			if err := decodeJSON(e.Payload, &terminal); err != nil {
				t.Fatal(err)
			}
		}
	}
	if terminal.HeldMaterial == nil || terminal.HeldMaterial.Kind != HeldQuarantined || terminal.HeldMaterial.Location == "" {
		t.Fatalf("terminal held material = %+v, want the quarantine named", terminal.HeldMaterial)
	}
}

// TestQuarantineSurvivesARestart: a controller that replays the journal after
// the refusal sees the quarantine record and a workspace that matches the
// recorded subject - nothing about the separation lives only in memory.
func TestQuarantineSurvivesARestart(t *testing.T) {
	fixture, _ := newRoutingFixture(t, 1, refusedWrite())
	runID := fixture.start()
	fixture.reconcile(runID)

	fixture.runtime = fixture.newRuntime(fixture.deps)
	state := fixture.state(runID)
	if len(quarantinedEvents(t, state.events)) != 1 {
		t.Fatalf("the replayed journal lost the quarantine record: %v", journalTypes(state.events))
	}
	workspace, err := fixture.runtime.workspace(state)
	if err != nil {
		t.Fatalf("the restarted controller cannot rebuild the workspace: %v", err)
	}
	if err := workspace.AssertIntegrity(); err != nil {
		t.Fatalf("quarantine left the workspace metadata outside its trusted baseline: %v", err)
	}
	assertQuarantinedA(t, fixture, runID)
}

// TestAnOperatorStoppedAttemptKeepsItsMaterialInPlace: an attempt an operator
// stop genuinely ended is #203's - its material is held where the producer left
// it, never quarantined, even though the provider mutated before the stop.
func TestAnOperatorStoppedAttemptKeepsItsMaterialInPlace(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	stop := operatorStop(t, f, &runID)
	var p *blockingProvider
	p = &blockingProvider{isolatedProvider: f.provider, during: func(context.Context) {
		dir := p.requests[len(p.requests)-1].CandidateDir
		if err := os.WriteFile(filepath.Join(dir, "stopped.go"), []byte("package stopped\n"), 0o600); err != nil {
			t.Error(err)
		}
		stop()
	}}
	f.runtime.deps.Provider = p
	var outcome Outcome
	for pass := 0; pass < 8 && len(p.requests) == 0; pass++ {
		outcome = f.reconcile(runID)
	}
	assertStoppedExecution(t, f, runID, outcome)
	if n := len(quarantinedEvents(t, f.state(runID).events)); n != 0 {
		t.Fatalf("a stopped attempt's material was quarantined (%d records); #203 holds it in place", n)
	}
	if _, err := os.Stat(filepath.Join(candidateDir(f.stateDir, runID), "stopped.go")); err != nil {
		t.Fatalf("a stopped attempt's material was moved out of the workspace: %v", err)
	}
}

// TestAFailedRestoreKeepsTheQuarantineJournalledAndStops: the copy is complete,
// the restore cannot finish. The quarantine still has its durable identity
// (journalled, restored=false) and the attempt is a STOP - no retry runs on a
// workspace that may still hold the refused material.
func TestAFailedRestoreKeepsTheQuarantineJournalledAndStops(t *testing.T) {
	var locked string
	fixture, provider := newRoutingFixture(t, 3, providerAnswer{
		result: refusedWrite().result,
		mutate: func(dir string) error {
			locked = filepath.Join(dir, "locked")
			if err := os.MkdirAll(locked, 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(locked, "refused.go"), []byte("package refused // A\n"), 0o600); err != nil {
				return err
			}
			// Readable, so the copy succeeds; not writable, so removing the
			// refused file during restore fails.
			return os.Chmod(locked, 0o500)
		},
	})
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	runID := fixture.start()
	fixture.reconcile(runID)

	if provider.calls != 1 {
		t.Fatalf("an attempt whose restore failed was retried (%d provider calls)", provider.calls)
	}
	state := fixture.state(runID)
	quarantined := quarantinedEvents(t, state.events)
	if len(quarantined) != 1 || quarantined[0].Restored {
		t.Fatalf("quarantine records = %+v, want one complete copy journalled with restored=false", quarantined)
	}
	kept, err := os.ReadFile(filepath.Join(fixture.stateDir, quarantined[0].Location, "files", "locked", "refused.go"))
	if err != nil || string(kept) != "package refused // A\n" {
		t.Fatalf("the journalled quarantine does not hold the refused bytes: %q, %v", kept, err)
	}
	op, _ := durableInvoke(t, fixture, runID)
	if class := durableFailureClass(t, op); RouteFailure(class) != RouteStop {
		t.Fatalf("a failed restore settled class %q (route %q), want a stop", class, RouteFailure(class))
	}
	if n := countType(state.events, EventCandidateCommitted); n != 0 {
		t.Fatalf("refused material was committed after a failed restore: %v", journalTypes(state.events))
	}
}

// TestACopyFailureLeavesTheMaterialAndStops: when the quarantine cannot be
// written, nothing is removed from the workspace and the attempt stops.
func TestACopyFailureLeavesTheMaterialAndStops(t *testing.T) {
	fixture, provider := newRoutingFixture(t, 3, refusedWrite())
	runID := fixture.start()
	// The quarantine root is occupied by a FILE, so no quarantine directory
	// can be created under it.
	root := filepath.Join(fixture.stateDir, "runs", runID, "quarantine")
	if err := os.MkdirAll(filepath.Dir(root), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	fixture.reconcile(runID)

	if provider.calls != 1 {
		t.Fatalf("an attempt whose material could not be quarantined was retried (%d provider calls)", provider.calls)
	}
	state := fixture.state(runID)
	if n := len(quarantinedEvents(t, state.events)); n != 0 {
		t.Fatalf("a quarantine that was never written was journalled (%d)", n)
	}
	if _, err := os.Stat(filepath.Join(candidateDir(fixture.stateDir, runID), "refused.go")); err != nil {
		t.Fatalf("a failed copy removed the refused material from the workspace: %v", err)
	}
	op, _ := durableInvoke(t, fixture, runID)
	if class := durableFailureClass(t, op); RouteFailure(class) != RouteStop {
		t.Fatalf("a failed copy settled class %q, want a stop", class)
	}
}

// TestTheFailStopDoesNotDependOnTheResultShape: a generic failed(err) result -
// the shape a post-provider read failure produces - is still forced into a
// non-retryable stop, with the original result kept in the diagnostic.
func TestTheFailStopDoesNotDependOnTheResultShape(t *testing.T) {
	out := failed(errors.New("execution authority could not be read"))
	stopRefusedAttempt(&out, errors.New("refused material could not be quarantined"))
	record, ok := out.result.(executionRecord)
	if !ok {
		t.Fatalf("result is %T, want an executionRecord", out.result)
	}
	if out.state != OperationFailed || record.FailureClass != FailureUnknown || RouteFailure(record.FailureClass) != RouteStop {
		t.Fatalf("state %q class %q: want a failed, non-retryable stop", out.state, record.FailureClass)
	}
	if record.Diagnostic == nil || record.Diagnostic.Route != RouteStop ||
		!strings.Contains(record.Diagnostic.Message, "execution authority could not be read") {
		t.Fatalf("diagnostic = %+v, want route stop with the original result kept", record.Diagnostic)
	}
}

// plantAtDispatch runs plant with the execution operation exactly as it is
// leased, before its handler runs - the moment a recovered controller would
// re-dispatch a crashed attempt.
type plantAtDispatch struct {
	OperationStore
	plant func(RunOperation)
	fired bool
}

func (p *plantAtDispatch) PutOperation(op RunOperation, revision int64) (int64, bool, error) {
	next, written, err := p.OperationStore.PutOperation(op, revision)
	if !p.fired && written && err == nil && op.Kind == OpExecutionInvoke && op.State == Running {
		p.fired = true
		p.plant(op)
	}
	return next, written, err
}

// TestAnOrphanedQuarantineIsAdoptedAfterACrash reproduces the crash seam: a
// controller died after attempt N's quarantine copy completed and before its
// candidate.quarantined event was journalled, leaving a complete copy with no
// durable identity and the refused material A still in the workspace. The
// re-dispatch of THAT operation must adopt the orphan - restore A out of the
// workspace BEFORE any provider runs, and journal the quarantine - so the
// successful attempt commits B and only B.
func TestAnOrphanedQuarantineIsAdoptedAfterACrash(t *testing.T) {
	sawA := false
	fixture, provider := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: execution.Succeeded},
		mutate: func(dir string) error {
			if _, err := os.Stat(filepath.Join(dir, "refused.go")); err == nil {
				sawA = true
			}
			return writesCandidate(dir)
		},
	})
	runID := fixture.start()
	var location string
	hook := &plantAtDispatch{OperationStore: fixture.runtime.scheduler.Store, plant: func(op RunOperation) {
		// The crashed EARLIER attempt of this very operation.
		crashed := op.AttemptIdentity - 1
		location = quarantineDir(runID, op.ID, crashed)
		workspace := candidateDir(fixture.stateDir, runID)
		if err := os.WriteFile(filepath.Join(workspace, "refused.go"), []byte("package refused // A\n"), 0o600); err != nil {
			t.Error(err)
			return
		}
		manifest := QuarantineManifest{OperationID: op.ID, Attempt: crashed, Subject: "unknown", FailureClass: FailureProviderBackgroundWorkUnresolved}
		if err := quarantineRefusedMaterial(workspace, filepath.Join(fixture.stateDir, location), manifest, []string{"refused.go"}); err != nil {
			t.Error(err)
		}
	}}
	fixture.runtime.scheduler.Store = hook
	fixture.reconcile(runID)

	if !hook.fired || provider.calls != 1 {
		t.Fatalf("the crash state was not planted before the dispatch (fired=%v, calls=%d)", hook.fired, provider.calls)
	}
	if sawA {
		t.Fatal("the provider ran on a workspace still holding the orphaned refused material A")
	}
	state := fixture.state(runID)
	quarantined := quarantinedEvents(t, state.events)
	if len(quarantined) != 1 || !quarantined[0].Adopted || !quarantined[0].Restored || quarantined[0].Location != location {
		t.Fatalf("quarantine records = %+v, want the orphan at %s adopted and restored", quarantined, location)
	}
	if !committedTreeHas(t, fixture, runID, "candidate.go") || committedTreeHas(t, fixture, runID, "refused.go") {
		t.Fatal("the committed candidate does not carry exactly B after adopting the orphaned quarantine")
	}
}

// TestAnUnrelatedQuarantineIsNotAdopted: a manifest alone grants nothing. A
// quarantine of another operation, one from this operation's current (not
// earlier) attempt, and one whose directory does not match what its manifest
// claims are all left alone by this dispatch.
func TestAnUnrelatedQuarantineIsNotAdopted(t *testing.T) {
	fixture, provider := newRoutingFixture(t, 3, providerAnswer{
		result: ExecutionResult{ProviderID: "test-provider", Outcome: execution.Succeeded},
		mutate: writesCandidate,
	})
	runID := fixture.start()
	hook := &plantAtDispatch{OperationStore: fixture.runtime.scheduler.Store, plant: func(op RunOperation) {
		plant := func(location string, manifest QuarantineManifest) {
			dest := filepath.Join(fixture.stateDir, location)
			if err := os.MkdirAll(filepath.Join(dest, "files"), 0o700); err != nil {
				t.Error(err)
			}
			document, _ := json.Marshal(manifest)
			if err := os.WriteFile(filepath.Join(dest, "manifest.json"), document, 0o600); err != nil {
				t.Error(err)
			}
		}
		other := "another-operation"
		plant(quarantineDir(runID, other, 0), QuarantineManifest{OperationID: other, Attempt: 0, Paths: map[string]string{"x.go": "file"}})
		plant(quarantineDir(runID, op.ID, op.AttemptIdentity), QuarantineManifest{OperationID: op.ID, Attempt: op.AttemptIdentity, Paths: map[string]string{"x.go": "file"}})
		plant(filepath.Join("runs", runID, "quarantine", "mislabelled"), QuarantineManifest{OperationID: op.ID, Attempt: 0, Paths: map[string]string{"x.go": "file"}})
	}}
	fixture.runtime.scheduler.Store = hook
	fixture.reconcile(runID)

	if !hook.fired || provider.calls != 1 {
		t.Fatalf("dispatch was not reached or was refused (fired=%v, calls=%d)", hook.fired, provider.calls)
	}
	if n := len(quarantinedEvents(t, fixture.state(runID).events)); n != 0 {
		t.Fatalf("%d unrelated quarantine(s) were adopted by a dispatch they do not belong to", n)
	}
}

// Restore the live #389/#415 sequence through the real Claude classifier and
// feed its result into the runtime admission boundary, retaining the mutation.
func TestClaudeAutomaticDetachmentQuarantinesBeforeAdmission(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	fake.outputs = []CommandOutput{{Stdout: []byte(
		claudeAssistant("M1", "", claudeToolUse("X")) + "\n" +
			claudeAutomaticResult("X", "", `{"backgroundTaskId":"bielbpgpe","timedOutAfterMs":120000,"interrupted":false}`) + "\n" +
			claudeResultWithAnswer(false, "go test ./... is still running in the background and I'll confirm once it completes.") + "\n")}}
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	answer := refusedWrite()
	answer.result = result
	fixture, _ := newRoutingFixture(t, 1, answer)
	runID := fixture.start()
	fixture.reconcile(runID)
	op, _ := durableInvoke(t, fixture, runID)
	if op.State != OperationFailed || durableFailureClass(t, op) != FailureProviderBackgroundWorkUnresolved {
		t.Fatalf("automatic detachment admitted: %+v", op)
	}
	assertQuarantinedA(t, fixture, runID)
	state := fixture.state(runID)
	for _, kind := range []string{EventCandidateCommitted, EventCandidateCheckpointed, EventAssuranceObserved, EventGitHubPRObserved} {
		if countType(state.events, kind) != 0 {
			t.Fatalf("refused mutation reached %s", kind)
		}
	}
}
