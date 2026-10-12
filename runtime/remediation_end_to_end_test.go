package runtime

// #581 Batch 2's decisive end-to-end proof: the exact real Git/SQLite
// scenario PR #580 proved blocked (TestDeliveredConflictFindingNeverReactivatesTheBlamedProducer)
// now recovers, through the real runtime - never a direct database edit,
// never an operator relaying a message by hand. Builds on #475's own
// proven fixture (integrationGraphUnits/integrationWorker pattern,
// workgraph_integration_test.go) with one addition: a controlled worker
// whose SECOND invocation for a unit writes a genuinely non-conflicting
// fix, so a real `git merge` actually succeeds the second time, never a
// mocked conflict flag.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// remediationWorker is integrationWorker's sibling: a unit's SECOND
// invocation (its remediation epoch) stops touching the shared, conflicting
// file and writes its own distinct file instead - a genuine, independently
// verifiable resolution, never an identical retry. Symmetric across both
// producer markers, so the test is robust to WHICHEVER side the real
// textual-conflict detector blames.
type remediationWorker struct {
	mu       sync.Mutex
	attempts map[string]int
}

func (w *remediationWorker) WritesTypedResults() bool { return true }

func (w *remediationWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (w *remediationWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	for _, marker := range []string{"a", "b"} {
		if !strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			continue
		}
		w.mu.Lock()
		w.attempts[marker]++
		attempt := w.attempts[marker]
		w.mu.Unlock()
		var file integrationFile
		if attempt == 1 {
			file = integrationFile{path: "shared.go", content: "package candidate\nconst Shared = \"" + marker + "\"\n"}
		} else {
			file = integrationFile{path: marker + "-fixed.go", content: "package candidate\nconst Fixed = \"" + marker + "\"\n"}
		}
		if err := os.WriteFile(filepath.Join(request.CandidateDir, file.path), []byte(file.content), 0o600); err != nil {
			return ExecutionResult{}, err
		}
		break
	}
	if request.HandoffPath == "" {
		return ExecutionResult{ProviderID: "remediation-fleet-worker", Outcome: execution.Succeeded}, nil
	}
	return ExecutionResult{ProviderID: "remediation-fleet-worker", Outcome: execution.Succeeded},
		os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
}

// governedRecoveryFixture is the real-conflict fixture both the positive
// end-to-end test and the external-replacement negative regression drive
// to the same point: a real textual conflict, a governed decision, and a
// fully reintegrated final candidate.
type governedRecoveryFixture struct {
	fixture             *fleetFixture
	supervisor          *Supervisor
	view                WorkGraphView
	ctx                 context.Context
	blamedID            string
	blamedOriginalRunID string
}

// driveToGovernedRecovery drives the real supervisor through every stage up
// through a successful, fully governed recovery: real conflict -> delivered
// Finding -> decision_request -> an unauthorized attempt refused -> a real
// operator's approval -> the blamed producer's new epoch -> the
// integrator's own automatic reintegration -> a final admitted candidate.
// Shared so the positive test and the negative (external-replacement)
// regression both start from the IDENTICAL, already-proven-governed state.
func driveToGovernedRecovery(t *testing.T) *governedRecoveryFixture {
	t.Helper()
	const issueA, issueB, issueC = fleetFirstIssue, fleetFirstIssue + 1, fleetFirstIssue + 2
	fixture := newFleetFixture(t, 4)
	fixture.setIntegrationIssue(issueA, "a")
	fixture.setIntegrationIssue(issueB, "b")
	fixture.deps.Provider = &remediationWorker{attempts: map[string]int{}}
	supervisor := fixture.supervisor()
	view := fixture.adoptGraph(supervisor, "claude", 1, integrationGraphUnits(issueA, issueB, issueC))
	ctx := context.Background()

	// Stage A/B (reused, unchanged mechanism): drive to the real parked
	// conflict, exactly as TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff does.
	var cRunID string
	var parked bool
	for range 60 {
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if c := fixture.graph(view.GraphID).unit(t, "c"); c.RunID != "" {
			cRunID = c.RunID
		}
		if cRunID == "" {
			continue
		}
		events, err := fixture.store.Events(cRunID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type != EventRunWaiting {
				continue
			}
			var payload dispositionRecord
			if err := decodeJSON(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Reason == ReasonDeterministicFailureUnchanged {
				parked = true
			}
		}
		if parked {
			break
		}
	}
	if !parked {
		t.Fatal("the blocked integration unit never parked on its unchanged deterministic conflict")
	}

	// Stage C (#579, reused, unchanged): one more tick for delivery, then
	// determine which unit the Finding blames.
	if _, err := supervisor.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	cRun, ok := storedRun(t, fixture.phase8Fixture, fixture.graph(view.GraphID).unit(t, "c").RunID)
	if !ok || cRun.Orchestration == nil {
		t.Fatal("integrator run not found or not orchestrated")
	}
	messages, err := fixture.store.ScopeMessages(cRun.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var blamedID string
	for _, message := range messages {
		if message.Kind == orchestration.KindFinding {
			blamedID = message.Route.Unit
		}
	}
	if blamedID != "a" && blamedID != "b" {
		t.Fatalf("integrator's finding routes to %q, not one of the conflicting units", blamedID)
	}

	// Stage D: drive further reconcile passes until the governed
	// epoch_authorization decision_request appears in the blamed producer's
	// own inbox - the real reconcileRemediationEligibility path, never a
	// direct message insert.
	blamedUnit := fixture.graph(view.GraphID).unit(t, blamedID)
	blamedRun, ok := storedRun(t, fixture.phase8Fixture, blamedUnit.RunID)
	if !ok || blamedRun.Orchestration == nil {
		t.Fatalf("producer %s's run not found or not orchestrated", blamedID)
	}
	var requestID string
	for range 20 {
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		own, err := fixture.store.ScopeMessages(blamedRun.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(own) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				requestID = message.ID
			}
		}
		if requestID != "" {
			break
		}
	}
	if requestID == "" {
		t.Fatal("no epoch_authorization decision_request ever appeared in the blamed producer's own inbox")
	}

	// A genuinely unauthorized attempt is refused: no operator identity, no
	// resolution (ADR-0008's own never-cut, proven against the real
	// control action before the real one is exercised).
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, DecisionOutcomeKind: orchestration.DecisionSelectedOption,
		DecisionOutcomeValue: orchestration.EpochAuthorizationApprove,
	}); err == nil {
		t.Fatal("a decision resolution with no operator identity was accepted")
	}

	// The real governed action: an actual operator approves.
	if _, err := supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, Operator: "operator@example.com",
		DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: orchestration.EpochAuthorizationApprove,
	}); err != nil {
		t.Fatal(err)
	}

	// Stage E: drive until the final integrated candidate admits - the
	// producer's own corrected epoch 2, then the integrator's own automatic
	// reintegration epoch, both through the ordinary scheduler/op-DAG, never
	// a special-cased path.
	for range 60 {
		if _, err := supervisor.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if c := fixture.graph(view.GraphID).unit(t, "c"); c.Output != nil {
			break
		}
	}
	if c := fixture.graph(view.GraphID).unit(t, "c"); c.Output == nil {
		t.Fatal("the integrator never admitted a corrected handoff after remediation")
	}
	return &governedRecoveryFixture{
		fixture: fixture, supervisor: supervisor, view: view, ctx: ctx,
		blamedID: blamedID, blamedOriginalRunID: blamedUnit.RunID,
	}
}

// TestRemediationEpochRecoversFromARealConflictThroughGovernedReintegration
// is #581 Batch 2's decisive positive path: real Git conflict -> delivered
// Finding -> governed decision_request -> genuine operator approval ->
// durable RemediationAuthorization -> a brand-new EngineeringRun under a
// NEW epoch, reconstructed from the original contract and base candidate ->
// a corrected handoff admitted through NORMAL validation -> the integrator
// unit, invalidated by the exact upstream change, automatically and
// deterministically reintegrated -> a final integrated candidate that
// never existed before this recovery.
func TestRemediationEpochRecoversFromARealConflictThroughGovernedReintegration(t *testing.T) {
	g := driveToGovernedRecovery(t)
	fixture, view, blamedID := g.fixture, g.view, g.blamedID

	final := fixture.graph(view.GraphID)
	a, b, c := final.unit(t, "a"), final.unit(t, "b"), final.unit(t, "c")
	// The blamed producer's own view now reflects its SECOND epoch's run -
	// a genuinely NEW RunID, never the original, exactly as a new
	// EngineeringRun under a new epoch requires.
	blamedFinal := final.unit(t, blamedID)
	if blamedFinal.RunID == g.blamedOriginalRunID {
		t.Fatalf("blamed producer %s's run is unchanged after remediation: %s", blamedID, blamedFinal.RunID)
	}
	if a.State != orchestration.UnitState(orchestration.ItemCompleted) || b.State != orchestration.UnitState(orchestration.ItemCompleted) {
		t.Fatalf("producers did not both settle completed after remediation: a=%s b=%s", a.State, b.State)
	}
	// Real evidence, not a status flag: the actual composed candidate on
	// disk carries the blamed unit's FIXED file and still carries the
	// unaffected unit's shared.go - a genuine merge, not a pass-through.
	run, ok := storedRun(t, fixture.phase8Fixture, c.RunID)
	if !ok {
		t.Fatal("integrator's final run not found")
	}
	dir := candidateDir(fixture.stateDir, run.ID)
	if _, err := os.Stat(filepath.Join(dir, blamedID+"-fixed.go")); err != nil {
		t.Fatalf("final composed candidate is missing the corrected file %s-fixed.go: %v", blamedID, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "shared.go")); err != nil {
		t.Fatalf("final composed candidate is missing shared.go: %v", err)
	}
	// The producer's historical epoch 1 remains a permanent, immutable row -
	// never rewritten, never deleted - alongside the new current epoch.
	epochs, err := fixture.store.WorkUnitRemediationEpochs(view.GraphID, blamedID)
	if err != nil || len(epochs) != 1 || epochs[0].Epoch != 2 {
		t.Fatalf("remediation epochs for %s = %+v (%v), want exactly one epoch-2 row", blamedID, epochs, err)
	}
	authorizations, err := fixture.store.RemediationAuthorizationsFor(view.GraphID, blamedID)
	if err != nil || len(authorizations) != 1 || authorizations[0].AuthorizedBy != RemediationAuthorizedByDecision {
		t.Fatalf("remediation authorizations for %s = %+v (%v)", blamedID, authorizations, err)
	}
	if authorizations[0].RaisesCeilingTo == nil || *authorizations[0].RaisesCeilingTo != 2 {
		t.Fatalf("the approved authorization did not record its own ceiling raise: %+v", authorizations[0])
	}
	// The integrator's own reintegration was policy-authorized, automatic,
	// never routed through a second human decision.
	integratorAuthorizations, err := fixture.store.RemediationAuthorizationsFor(view.GraphID, "c")
	if err != nil || len(integratorAuthorizations) != 1 || integratorAuthorizations[0].AuthorizedBy != RemediationAuthorizedByPolicy {
		t.Fatalf("integrator's own remediation authorization = %+v (%v), want exactly one policy-authorized grant", integratorAuthorizations, err)
	}
}

// TestExternalReplacementAfterRemediationNeverAutoReintegrates is the
// direct regression for the independent review's second P1 finding on PR
// #585: a dependency that has ALREADY remediated once, and is LATER
// externally replaced again (#472's own replaceAdmittedOutput path - a
// second admitted handoff on the SAME run, never through #581's governed
// epoch mechanism at all), must NOT auto-reintegrate the integrator a
// second time. "Has remediated at some point in its history" is not the
// same fact as "the CURRENT change is caused by that exact remediation",
// and only the latter may ever auto-authorize anything.
func TestExternalReplacementAfterRemediationNeverAutoReintegrates(t *testing.T) {
	g := driveToGovernedRecovery(t)
	fixture, view, blamedID := g.fixture, g.view, g.blamedID

	before := fixture.graph(view.GraphID)
	blamedUnit := before.unit(t, blamedID)
	cBefore := before.unit(t, "c")
	integratorAuthorizationsBefore, err := fixture.store.RemediationAuthorizationsFor(view.GraphID, "c")
	if err != nil {
		t.Fatal(err)
	}

	// External replacement of the ALREADY-REMEDIATED producer's current
	// (epoch 2) run - never through ActivateRemediationEpoch, never
	// through any #581 path, exactly #472's own existing mechanism
	// (replaceAdmittedOutput, runtime/workgraph_test.go) already proves
	// must stay terminal for an ordinary external replacement.
	replaceAdmittedOutput(t, fixture, view.GraphID, blamedUnit)

	for range 20 {
		if _, err := g.supervisor.Tick(g.ctx); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
	}

	after := fixture.graph(view.GraphID)
	if state := after.unit(t, "c").State; state != orchestration.UnitInvalidated {
		t.Fatalf("integrator is %s after an external replacement of its already-remediated dependency, want invalidated: %s",
			state, after.unit(t, "c").Reason)
	}
	// The decisive assertion: NO new automatic reintegration authorization,
	// and NO new epoch, for the integrator - the external replacement must
	// never be mistaken for a governed remediation.
	integratorAuthorizationsAfter, err := fixture.store.RemediationAuthorizationsFor(view.GraphID, "c")
	if err != nil {
		t.Fatal(err)
	}
	if len(integratorAuthorizationsAfter) != len(integratorAuthorizationsBefore) {
		t.Fatalf("a new automatic reintegration authorization was granted after an unrelated external replacement: before=%+v after=%+v",
			integratorAuthorizationsBefore, integratorAuthorizationsAfter)
	}
	cEpochs, err := fixture.store.WorkUnitRemediationEpochs(view.GraphID, "c")
	if err != nil || len(cEpochs) != 1 {
		t.Fatalf("integrator epochs = %+v (%v), want exactly the one legitimate reintegration epoch from before the external replacement", cEpochs, err)
	}
	if after.unit(t, "c").RunID != cBefore.RunID {
		t.Fatalf("integrator's run changed after an unrelated external replacement: %s -> %s", cBefore.RunID, after.unit(t, "c").RunID)
	}
}
