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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// refusedWrite is attempt 1: it creates material A and reports a refusal that
// routes to a same-binding retry.
func refusedWrite() providerAnswer {
	return providerAnswer{
		result: ExecutionResult{
			ProviderID: "test-provider",
			Outcome:    OperationFailed,
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
	if q.FailureClass != FailureProviderBackgroundWorkUnresolved || q.PathCount != 1 || q.Attempt != 1 {
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
		providerAnswer{result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}},
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
			result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded},
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
