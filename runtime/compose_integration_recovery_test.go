package runtime

// Split out of workgraph_integration_test.go (#475 review N2): these test
// commitIntegratedCandidate/composeIntegration's own post-merge containment
// and recovery directly - the window after IntegrateInputs has already
// advanced Git HEAD but before this run's journal write, which has no
// provider invocation to pause at and so cannot be reproduced through the
// full scheduler - a distinct responsibility from the end-to-end WorkGraph
// acceptance scenarios the rest of that file covers.

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/integration"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// TestComposeIntegrationPostMergeFailureIsContainedAndRecovers is Scenario
// B4: once IntegrateInputs returns StatusIntegrated, Git HEAD has already
// advanced. A failure in one of commitIntegratedCandidate's own LATER steps
// (here, integratedChangedPaths, forced by a corrupted candidate revision
// that does not exist) must never be a bare, unowned error: it is routed
// through the existing restoreCandidate/FailureWorkspaceIntegrity
// containment, which both fails the operation and restores the workspace -
// never a false success, never a silently stranded advanced HEAD. A
// subsequent attempt against the now-restored workspace then composes and
// commits cleanly, proving recovery, not merely containment.
//
// This calls commitIntegratedCandidate directly rather than through the full
// scheduler: the window it tests - after a real Git merge, before this run's
// own journal write - has no provider invocation to pause at, exactly as
// R4's own review response documents (see the Scenario F discussion), so a
// full-stack tick-boundary reproduction is not possible; the function under
// test is exercised with the exact same real Git workspace, contract and
// resolver composeIntegration itself would use.
func TestComposeIntegrationPostMergeFailureIsContainedAndRecovers(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	const issueA, issueB = fleetFirstIssue, fleetFirstIssue + 1
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	// An ordinary two-unit graph - no integrator unit at all. This test needs
	// only REAL, current admitted inputs to build a real integration.Contract
	// against; it exercises commitIntegratedCandidate directly, never through
	// WorkGraph activation for a third unit.
	view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		{ID: "b", Purpose: "land b", Role: domain.RoleImplementer, Issue: issueB},
	})
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	a, b := settled.unit(t, "a"), settled.unit(t, "b")
	inputs := orchestration.WorkUnitInputs{
		{UnitID: "a", UnitOutput: orchestration.UnitOutput{HandoffID: a.Output.HandoffID, RunID: a.RunID,
			CandidateRevision: a.Output.CandidateRevision, CandidateTree: a.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
		{UnitID: "b", UnitOutput: orchestration.UnitOutput{HandoffID: b.Output.HandoffID, RunID: b.RunID,
			CandidateRevision: b.Output.CandidateRevision, CandidateTree: b.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
	}
	contract, err := integration.NewContract(view.GraphID, "fault-injection-unit", fixture.base, inputs)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := supervisor.engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	const faultRunID = "run-fault-injection-test"
	created, err := CreateCandidateClone(fixture.stateDir, faultRunID, fixture.deps.Remote.URL, fixture.base, fixture.deps.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ws := &created
	sources, readErr := engine.integrationSources(view.GraphID)
	result, err := IntegrateInputs(ws, contract, sources)
	if *readErr != nil {
		t.Fatal(*readErr)
	}
	if err != nil {
		t.Fatalf("the real merge itself failed, before this test's injected fault: %v", err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("the real merge did not integrate cleanly: %+v", result)
	}
	realCandidate := *result.Candidate
	headAfterRealMerge, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterRealMerge) != realCandidate.Revision {
		t.Fatalf("workspace HEAD %s does not match the real merge commit %s", headAfterRealMerge, realCandidate.Revision)
	}

	// Inject the fault: a candidate revision integratedChangedPaths cannot
	// possibly read, forcing commitIntegratedCandidate's OWN later step to
	// fail - AFTER the real Git merge above already advanced HEAD, exactly
	// B4's window.
	corrupted := realCandidate
	corrupted.Revision = strings.Repeat("f", 40)
	eff := engine.commitIntegratedCandidate(&runState{}, RunOperation{}, ws, contract, corrupted)
	if eff.state != OperationFailed {
		t.Fatalf("a post-merge preparation failure settled as %s, not failed", eff.state)
	}
	if len(eff.events) != 0 {
		t.Fatalf("a contained post-merge failure journalled %d events; want none (no false commit)", len(eff.events))
	}
	// The canonical containment route actually restored the workspace - the
	// real merge commit's advanced HEAD is gone, not silently stranded.
	headAfterContainment, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterContainment) != fixture.base {
		t.Fatalf("workspace HEAD after containment is %s, want restored to base %s", headAfterContainment, fixture.base)
	}

	// Recovery, not merely containment: a fresh attempt against the
	// now-restored workspace composes cleanly again (the restored workspace
	// is genuinely usable, not left in some intermediate state), and the
	// EXACT step this test injected the fault into - integratedChangedPaths -
	// succeeds on the real, uncorrupted result.
	sources2, readErr2 := engine.integrationSources(view.GraphID)
	result2, err := IntegrateInputs(ws, contract, sources2)
	if *readErr2 != nil {
		t.Fatal(*readErr2)
	}
	if err != nil || result2.Status != integration.StatusIntegrated {
		t.Fatalf("the recovery attempt did not integrate cleanly: status=%v err=%v", result2.Status, err)
	}
	if _, err := integratedChangedPaths(ws.Dir, contract.BaseRevision, result2.Candidate.Revision); err != nil {
		t.Fatalf("the exact step this test faulted still fails after recovery: %v", err)
	}
}

// TestComposeIntegrationPostMergeDBReadFailureIsContainedAndRecovers is #475
// review N2: commitIntegratedCandidate's OWN FIRST step,
// integrationInputsStillCurrent, can itself fail with an ordinary
// transient store error - not necessarily a *WorkspaceIntegrityError - and
// that failure happens AFTER the real Git merge already advanced HEAD, the
// identical window TestComposeIntegrationPostMergeFailureIsContainedAndRecovers
// proves for a later step. A genuinely separate, then-closed store
// connection forces this exact read to fail, proving the same containment
// (operation fails, no event, workspace restored) and recovery (a fresh
// attempt through the real, still-open store composes and commits cleanly)
// apply here too.
func TestComposeIntegrationPostMergeDBReadFailureIsContainedAndRecovers(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	const issueA, issueB = fleetFirstIssue, fleetFirstIssue + 1
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		{ID: "b", Purpose: "land b", Role: domain.RoleImplementer, Issue: issueB},
	})
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	a, b := settled.unit(t, "a"), settled.unit(t, "b")
	inputs := orchestration.WorkUnitInputs{
		{UnitID: "a", UnitOutput: orchestration.UnitOutput{HandoffID: a.Output.HandoffID, RunID: a.RunID,
			CandidateRevision: a.Output.CandidateRevision, CandidateTree: a.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
		{UnitID: "b", UnitOutput: orchestration.UnitOutput{HandoffID: b.Output.HandoffID, RunID: b.RunID,
			CandidateRevision: b.Output.CandidateRevision, CandidateTree: b.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
	}
	contract, err := integration.NewContract(view.GraphID, "fault-injection-unit-db", fixture.base, inputs)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := supervisor.engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	const faultRunID = "run-fault-injection-db-test"
	created, err := CreateCandidateClone(fixture.stateDir, faultRunID, fixture.deps.Remote.URL, fixture.base, fixture.deps.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ws := &created
	sources, readErr := engine.integrationSources(view.GraphID)
	result, err := IntegrateInputs(ws, contract, sources)
	if *readErr != nil {
		t.Fatal(*readErr)
	}
	if err != nil {
		t.Fatalf("the real merge itself failed, before this test's injected fault: %v", err)
	}
	if result.Status != integration.StatusIntegrated {
		t.Fatalf("the real merge did not integrate cleanly: %+v", result)
	}
	headAfterRealMerge, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterRealMerge) != result.Candidate.Revision {
		t.Fatalf("workspace HEAD %s does not match the real merge commit %s", headAfterRealMerge, result.Candidate.Revision)
	}

	// A genuinely separate connection, then CLOSED: any query through it -
	// including integrationInputsStillCurrent's own WorkGraphStatus read -
	// fails with an ordinary "database is closed" error, never a
	// *WorkspaceIntegrityError. A second EngineeringRuntime over this
	// broken connection is what commitIntegratedCandidate actually sees;
	// fixture.store itself, and every other engine built from it, is
	// untouched.
	broken, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := broken.Close(); err != nil {
		t.Fatal(err)
	}
	brokenDeps := fixture.deps
	brokenDeps.Store = broken
	brokenEngine := fixture.newRuntime(brokenDeps)

	eff := brokenEngine.commitIntegratedCandidate(&runState{}, RunOperation{}, ws, contract, *result.Candidate)
	if eff.state != OperationFailed {
		t.Fatalf("a post-merge DB-read failure settled as %s, not failed", eff.state)
	}
	if len(eff.events) != 0 {
		t.Fatalf("a contained post-merge DB-read failure journalled %d events; want none (no false commit)", len(eff.events))
	}
	headAfterContainment, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterContainment) != fixture.base {
		t.Fatalf("workspace HEAD after containment is %s, want restored to base %s", headAfterContainment, fixture.base)
	}

	// Recovery, not merely containment: a fresh attempt against the
	// now-restored workspace composes cleanly again through the REAL,
	// still-open store, and the exact step this test faulted
	// (integrationInputsStillCurrent) now succeeds against it.
	sources2, readErr2 := engine.integrationSources(view.GraphID)
	result2, err := IntegrateInputs(ws, contract, sources2)
	if *readErr2 != nil {
		t.Fatal(*readErr2)
	}
	if err != nil || result2.Status != integration.StatusIntegrated {
		t.Fatalf("the recovery attempt did not integrate cleanly: status=%v err=%v", result2.Status, err)
	}
	if current, err := engine.integrationInputsStillCurrent(contract.GraphID, contract.Inputs); err != nil || !current {
		t.Fatalf("the exact step this test faulted still fails after recovery: current=%t err=%v", current, err)
	}
}

// TestComposeIntegrationCrashBeforeAnyRecordRecoversOnRetry is #475 review
// N2's second named gap: composeIntegration's OWN top-level r.workspace(state)
// call - reached on every fresh attempt, before IntegrateInputs ever runs
// again - must ALSO route a *WorkspaceIntegrityError through
// restoreCandidate, not a bare failed(err). This simulates a true crash
// between a successful Git merge and composeIntegration's caller ever
// reaching commitIntegratedCandidate at all (so none of
// TestComposeIntegrationPostMergeFailureIsContainedAndRecovers's or
// TestComposeIntegrationPostMergeDBReadFailureIsContainedAndRecovers's own
// containment ever ran): a fresh runState with nothing journalled
// (CandidateRevision empty) over a workspace ALREADY merged ahead, calling
// the FULL composeIntegration entry point directly.
func TestComposeIntegrationCrashBeforeAnyRecordRecoversOnRetry(t *testing.T) {
	fixture := newFleetFixture(t, 4)
	const issueA, issueB = fleetFirstIssue, fleetFirstIssue + 1
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &integrationWorker{files: map[string]integrationFile{
		"a": {path: "a.go", content: "package candidate\nconst A = 1\n"},
		"b": {path: "b.go", content: "package candidate\nconst B = 1\n"},
	}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, []orchestration.WorkUnit{
		{ID: "a", Purpose: "land a", Role: domain.RoleImplementer, Issue: issueA},
		{ID: "b", Purpose: "land b", Role: domain.RoleImplementer, Issue: issueB},
	})
	settled := fixture.driveGraph(supervisor, view.GraphID, nil)
	for _, unit := range settled.Units {
		if unit.State != orchestration.UnitState(orchestration.ItemCompleted) {
			t.Fatalf("unit %s settled as %s: %s", unit.UnitID, unit.State, unit.Reason)
		}
	}
	a, b := settled.unit(t, "a"), settled.unit(t, "b")
	inputs := orchestration.WorkUnitInputs{
		{UnitID: "a", UnitOutput: orchestration.UnitOutput{HandoffID: a.Output.HandoffID, RunID: a.RunID,
			CandidateRevision: a.Output.CandidateRevision, CandidateTree: a.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
		{UnitID: "b", UnitOutput: orchestration.UnitOutput{HandoffID: b.Output.HandoffID, RunID: b.RunID,
			CandidateRevision: b.Output.CandidateRevision, CandidateTree: b.Output.CandidateTree, Outcome: orchestration.OutcomeCompleted}},
	}
	engine, err := supervisor.engine("acme/repo", "claude")
	if err != nil {
		t.Fatal(err)
	}
	const faultRunID = "run-fault-injection-crash-test"
	origin := &orchestration.BatchOrigin{GraphID: view.GraphID, UnitID: "fault-injection-unit-crash", Inputs: inputs,
		ExecutionKind: orchestration.ExecutionKindIntegrationCompose}
	contract, err := integration.NewContract(origin.GraphID, origin.UnitID, fixture.base, origin.Inputs)
	if err != nil {
		t.Fatal(err)
	}
	created, err := CreateCandidateClone(fixture.stateDir, faultRunID, fixture.deps.Remote.URL, fixture.base, fixture.deps.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	ws := &created
	sources, readErr := engine.integrationSources(view.GraphID)
	result, err := IntegrateInputs(ws, contract, sources)
	if *readErr != nil {
		t.Fatal(*readErr)
	}
	if err != nil || result.Status != integration.StatusIntegrated {
		t.Fatalf("the real merge did not integrate cleanly: status=%v err=%v", result.Status, err)
	}
	headAfterRealMerge, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterRealMerge) != result.Candidate.Revision {
		t.Fatalf("workspace HEAD %s does not match the real merge commit %s", headAfterRealMerge, result.Candidate.Revision)
	}

	// The "crash": a FRESH runState for this exact run, with NOTHING
	// journalled (CandidateRevision empty, BaseRevision the original base) -
	// commitIntegratedCandidate never ran, exactly as if the process had
	// been killed immediately after IntegrateInputs returned above.
	crashed := &runState{
		rt: engine, run: EngineeringRun{ID: faultRunID}, origin: origin,
		projection: RunProjection{BaseRevision: fixture.base},
	}
	eff := engine.composeIntegration(context.Background(), crashed, RunOperation{})
	if eff.state != OperationFailed {
		t.Fatalf("a crash-before-any-record retry settled as %s, not failed", eff.state)
	}
	if len(eff.events) != 0 {
		t.Fatalf("a contained crash-before-any-record retry journalled %d events; want none", len(eff.events))
	}
	headAfterContainment, err := gitOutput(ws.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(headAfterContainment) != fixture.base {
		t.Fatalf("workspace HEAD after containment is %s, want restored to base %s", headAfterContainment, fixture.base)
	}

	// Recovery, not merely containment: the exact step this test faulted -
	// composeIntegration's own top-level r.workspace(state) call - no longer
	// sees a mismatch against the now-restored workspace. freshWs is loaded
	// anew (never the stale local ws, whose own TrustedMetadata still
	// reflects the first merge containment reset it from disk out from
	// under it - exactly why production code always reloads via r.workspace
	// rather than reusing one across attempts) and a fresh real merge
	// against it succeeds again.
	freshWs, err := engine.workspace(crashed)
	if err != nil {
		t.Fatalf("r.workspace still fails against the restored workspace: %v", err)
	}
	sources2, readErr2 := engine.integrationSources(view.GraphID)
	result2, err := IntegrateInputs(freshWs, contract, sources2)
	if *readErr2 != nil {
		t.Fatal(*readErr2)
	}
	if err != nil || result2.Status != integration.StatusIntegrated {
		t.Fatalf("the recovery merge did not integrate cleanly: status=%v err=%v", result2.Status, err)
	}
}
