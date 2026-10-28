package runtime

// The whole governed journey of a failed final-integration feature acceptance,
// on a real Git/SQLite objective plan: the integrator composes cleanly, the
// operator-declared feature fails against the exact integrated tree and HOLDS
// (no refund, no retry of the same tree, no false goal), the operator attributes
// the failure to one exact upstream input - which authorizes nothing - the
// existing epoch_authorization decision is still required, the upstream unit
// remediates, the integrator reintegrates automatically, and the feature then
// passes on the new tree and discharges the claim.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

const (
	journeyFeature = "delivery-report"
	// journeyFixFile is what the corrected upstream unit adds. The feature
	// tests of this fixture pass exactly when the integrated tree carries it.
	journeyFixFile = "corrected.go"
)

// featureJourneyWorker writes two producers' distinct files, so the integrator
// composes without a textual conflict. A producer's later run (its remediation
// epoch) also writes journeyFixFile.
type featureJourneyWorker struct {
	mu       sync.Mutex
	firstRun map[string]string
	// noFix makes a producer's remediation epoch change nothing, so the
	// integrator recomposes to the identical tree.
	noFix bool
	// alwaysFix makes every producer carry the correction from its first run, so
	// the first integration already passes the feature.
	alwaysFix bool
}

func (w *featureJourneyWorker) WritesTypedResults() bool { return true }

func (w *featureJourneyWorker) Isolation() ProviderIsolation {
	return ProviderIsolation{FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven, NetworkDenied: IsolationProven, CredentialScope: IsolationProven}
}

func (w *featureJourneyWorker) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	w.mu.Lock()
	if w.firstRun == nil {
		w.firstRun = map[string]string{}
	}
	for _, marker := range []string{"a", "b"} {
		if !strings.Contains(request.Objective, integrationUnitMarker(marker)) {
			continue
		}
		if _, claimed := w.firstRun[marker]; !claimed {
			w.firstRun[marker] = request.RunID
		}
		original := w.firstRun[marker] == request.RunID
		w.mu.Unlock()
		files := map[string]string{marker + "-part.go": "package candidate\nconst Part = \"" + marker + "\"\n"}
		if (!original && !w.noFix) || w.alwaysFix {
			files[journeyFixFile] = "package candidate\n// corrected\n"
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, name), []byte(content), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		return finishJourneyWorker(request)
	}
	w.mu.Unlock()
	return finishJourneyWorker(request)
}

func finishJourneyWorker(request ExecutionRequest) (ExecutionResult, error) {
	result := ExecutionResult{ProviderID: "feature-journey-worker", Outcome: execution.Succeeded}
	if request.HandoffPath == "" {
		return result, nil
	}
	return result, os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
}

// featureAssurance is the verifier double: the Go suite passes everywhere, and
// the declared feature passes exactly when the exact checked-out tree carries
// the correction. It records what it was asked, so the test can count real
// verifications.
type featureAssurance struct {
	mu       sync.Mutex
	requests []AssuranceRequest
	// ignoreFeature makes the verifier a provider that never heard of the
	// feature request: it passes the Go suite and returns no feature record.
	ignoreFeature bool
	// mutate corrupts the passing feature record into a structurally valid but
	// mismatched one.
	mutate func(*VerificationCheck)
}

// journeyManifest is the feature's manifest on the trusted base of the journey.
var journeyManifest = []string{"TestDeliveryReportExports", "TestDeliveryReportRenders"} // byte-sorted, as the verifier reads it

func journeySeed(origin string) {
	dir := filepath.Join(origin, featureManifestDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(dir, journeyFeature+".txt"), []byte(strings.Join(journeyManifest, "\n")+"\n"), 0o644); err != nil {
		panic(err)
	}
}

func (a *featureAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return []domain.EvidenceClass{AssuranceEvidenceClass}
}

func (a *featureAssurance) featureRequests() []AssuranceRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []AssuranceRequest
	for _, request := range a.requests {
		if request.FeatureAcceptance != "" {
			out = append(out, request)
		}
	}
	return out
}

func (a *featureAssurance) Assure(_ context.Context, request AssuranceRequest) (AssuranceResult, error) {
	a.mu.Lock()
	a.requests = append(a.requests, request)
	a.mu.Unlock()
	result := AssuranceResult{ProviderID: "feature-journey-verifier", VerifierDefinition: "verifier-v1", Passed: true}
	if request.FeatureAcceptance == "" || a.ignoreFeature {
		return result, nil
	}
	_, statErr := os.Stat(filepath.Join(request.CheckoutDir, journeyFixFile))
	passed := statErr == nil
	record := VerificationCheck{
		Kind: CheckKindFeatureAcceptance, Checker: featureCheckerID, CheckerRevision: featureCheckerRevision(), Feature: request.FeatureAcceptance,
		Outcome: CheckOutcomePassed, Commit: request.Commit, Tree: request.Tree, BaseRevision: request.FeatureAcceptanceBase,
		Selection: browserSelectionDigest(journeyManifest), Surface: strings.Repeat("2", 64), ArtifactsSHA256: strings.Repeat("3", 64),
		Environment: "docker-network-none@sha256:journey", Artifact: "baseline-go-feature/journey", Results: &CheckResults{Total: len(journeyManifest), Passed: len(journeyManifest)},
	}
	if passed && a.mutate != nil {
		a.mutate(&record)
	}
	if !passed {
		record.Outcome, record.Results = CheckOutcomeFailed, &CheckResults{Total: len(journeyManifest), Passed: 1, Failed: 1}
		result.Passed, result.FailureClass = false, FailureFeatureAcceptance
		result.ArtifactRef, result.FailureSignature = "baseline-go-feature/journey", "journey-failure"
	}
	result.Checks = []VerificationCheck{record}
	return result, nil
}

type featureJourney struct {
	t          *testing.T
	fleet      *fleetFixture
	plan       *planRunFixture
	supervisor *Supervisor
	assurance  *featureAssurance
	graphID    string
	ctx        context.Context
}

func newFeatureJourney(t *testing.T, worker *featureJourneyWorker) *featureJourney {
	return newFeatureJourneyWith(t, worker, featureJourneyOptions{})
}

type featureJourneyOptions struct {
	ignoreFeature, withoutRule bool
	mutate                     func(*VerificationCheck)
}

func newFeatureJourneyWith(t *testing.T, worker *featureJourneyWorker, options featureJourneyOptions) *featureJourney {
	fleet := newFleetFixture(t, 4, journeySeed)
	fleet.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	fleet.deps.Provider = worker
	assurance := &featureAssurance{ignoreFeature: options.ignoreFeature, mutate: options.mutate}
	fleet.deps.Assurance = assurance
	// The operator-activated rule: the runtime-derived fact requires the claim.
	rules := map[string]domain.PolicyRule{}
	for id, rule := range fleet.deps.Policy.Rules {
		rules[id] = rule
	}
	if !options.withoutRule {
		rules["FEATURE-FINAL-INTEGRATION-001"] = featureRule()
	}
	fleet.deps.Policy.Rules = rules
	fleet.runtime = fleet.newRuntime(fleet.deps)

	stages := objectiveIntegrationStages()
	stages[2].AcceptanceFeature = journeyFeature
	plan := newPlanRunFixtureWith(t, fleet.phase8Fixture, stages, fleet.deps.ConfigDigest,
		domain.PlanBudgetEnvelope{MaxChildRuns: 8, MaxConcurrency: 3, MaxProviderInvocations: 16})
	graphID, err := plan.service.BindObjectivePlan(plan.plan, plan.issue, plan.service.DefaultAgent)
	if err != nil {
		t.Fatal(err)
	}
	plan.approve(t)
	supervisor := fleet.supervisor()
	supervisor.deps.Plans = plan.service
	return &featureJourney{t: t, fleet: fleet, plan: plan, supervisor: supervisor, assurance: assurance, graphID: graphID, ctx: context.Background()}
}

// tick is one plan reconciliation, one supervisor pass and a clock step.
func (j *featureJourney) tick() {
	j.t.Helper()
	j.plan.reconcile(j.t)
	if _, err := j.supervisor.Tick(j.ctx); err != nil {
		j.t.Fatal(err)
	}
	j.fleet.clock.advance(61 * time.Second)
}

// until ticks until done reports true, failing the test after the bound.
func (j *featureJourney) until(what string, done func() bool) {
	j.t.Helper()
	for range 80 {
		if done() {
			return
		}
		j.tick()
	}
	if !done() {
		j.t.Fatalf("never reached: %s", what)
	}
}

func (j *featureJourney) integratorRunID() string {
	return j.fleet.graph(j.graphID).unit(j.t, "integrate").RunID
}

func (j *featureJourney) integratorScope() []orchestration.EngineeringMessage {
	j.t.Helper()
	run, ok := storedRun(j.t, j.fleet.phase8Fixture, j.integratorRunID())
	if !ok || run.Orchestration == nil {
		j.t.Fatal("the integrator run is not orchestrated")
	}
	messages, err := j.fleet.store.ScopeMessages(run.Orchestration.BatchID)
	if err != nil {
		j.t.Fatal(err)
	}
	return orchestration.Live(messages)
}

func (j *featureJourney) attributionRequest() (orchestration.EngineeringMessage, bool) {
	for _, message := range j.integratorScope() {
		if message.Kind == orchestration.KindDecisionRequest && message.Purpose == FeatureAttributionPurpose {
			return message, true
		}
	}
	return orchestration.EngineeringMessage{}, false
}

func (j *featureJourney) grants(unit string) ([]RemediationAuthorization, []RemediationEpoch) {
	j.t.Helper()
	authorizations, err := j.fleet.store.RemediationAuthorizationsFor(j.graphID, unit)
	if err != nil {
		j.t.Fatal(err)
	}
	epochs, err := j.fleet.store.WorkUnitRemediationEpochs(j.graphID, unit)
	if err != nil {
		j.t.Fatal(err)
	}
	return authorizations, epochs
}

func (j *featureJourney) resolve(requestID, value, operator string) error {
	_, err := j.supervisor.ResolveDecision(ControlRequest{
		DecisionID: requestID, Operator: operator,
		DecisionOutcomeKind: orchestration.DecisionSelectedOption, DecisionOutcomeValue: value,
	})
	return err
}

func (j *featureJourney) restart() {
	j.fleet.runtime = j.fleet.newRuntime(j.fleet.deps)
	j.supervisor = j.fleet.supervisor()
	j.supervisor.deps.Plans = j.plan.service
}

func TestAFailedFeatureHoldsIsAttributedByTheOperatorAndReintegratesAfterGovernedRemediation(t *testing.T) {
	j := newFeatureJourney(t, &featureJourneyWorker{})

	// 1. The integrator composes cleanly and the declared feature judges the
	// exact integrated tree and fails it.
	var heldRunID string
	j.until("the integrator holds a judged feature failure", func() bool {
		heldRunID = j.integratorRunID()
		if heldRunID == "" {
			return false
		}
		return failedFeatureObservation(j.fleet.state(heldRunID).projection) != nil
	})
	held := j.fleet.state(heldRunID)
	if held.origin == nil || held.origin.AcceptanceFeature != journeyFeature {
		t.Fatalf("the integrator run's verified origin does not declare the feature: %+v", held.origin)
	}
	failure := failedFeatureObservation(held.projection)
	failedTree, failedCommit := failure.Tree, failure.Commit

	// The verdict consumed its attempt: the assurance operation SUCCEEDED as a
	// recorded verdict, and its budget attempt was not given back.
	operations, err := j.fleet.store.Operations(heldRunID)
	if err != nil {
		t.Fatal(err)
	}
	var assuranceOps int
	for _, op := range operations {
		if op.Kind != OpAssuranceGo {
			continue
		}
		assuranceOps++
		if op.State != Succeeded || op.Attempt != 1 {
			t.Fatalf("the judged verdict left the assurance operation %s at attempt %d: a refund or a retry", op.State, op.Attempt)
		}
	}
	if assuranceOps != 1 {
		t.Fatalf("expected one assurance operation for the failed tree, found %d", assuranceOps)
	}
	verifications := len(j.assurance.featureRequests())
	if verifications != 2 {
		t.Fatalf("the failed tree was verified %d times, want the verification and its one confirmation", verifications)
	}

	// 2. A hold, not a goal, and no authority was granted by the verdict alone.
	j.until("the attribution decision is asked", func() bool { _, asked := j.attributionRequest(); return asked })
	request, _ := j.attributionRequest()
	if !slices.Equal(request.PermittedOptions, []string{"backend", "frontend", attributionNone}) || !request.Source.System {
		t.Fatalf("the attribution decision offers %v (system=%t)", request.PermittedOptions, request.Source.System)
	}
	for range 6 {
		j.tick()
	}
	if after := len(j.assurance.featureRequests()); after != verifications {
		t.Fatalf("the same failed tree was verified again: %d -> %d feature verifications", verifications, after)
	}
	for _, unit := range []string{"backend", "frontend", "integrate"} {
		authorizations, epochs := j.grants(unit)
		if len(authorizations) != 0 || len(epochs) != 0 {
			t.Fatalf("a failed verdict alone granted remediation for %s: %+v %+v", unit, authorizations, epochs)
		}
	}
	run, _ := storedRun(t, j.fleet.phase8Fixture, heldRunID)
	if run.Disposition != Waiting || run.Reason == ReasonGoalStateReached {
		t.Fatalf("the failed integrator settled %s/%s", run.Disposition, run.Reason)
	}

	// 3. The decision is the operator's: no identity, an option outside the
	// frozen menu and the reserved word for a unit are all refused.
	if err := j.resolve(request.ID, "backend", ""); err == nil {
		t.Fatal("an attribution with no operator identity was accepted")
	}
	if err := j.resolve(request.ID, "release-manager", "operator@example.com"); err == nil {
		t.Fatal("an attribution naming an input the integration never consumed was accepted")
	}

	// 4. A restart changes nothing: the same hold, the same single request, no
	// new verification and no duplicate message.
	scopeBefore := len(j.integratorScope())
	j.restart()
	for range 6 {
		j.tick()
	}
	if len(j.integratorScope()) != scopeBefore || len(j.assurance.featureRequests()) != verifications {
		t.Fatalf("a restart re-asked or re-verified: scope %d -> %d, verifications %d -> %d",
			scopeBefore, len(j.integratorScope()), verifications, len(j.assurance.featureRequests()))
	}
	if restored := failedFeatureObservation(j.fleet.state(heldRunID).projection); restored == nil || restored.Tree != failedTree || restored.Commit != failedCommit {
		t.Fatalf("the replayed hold is %+v", restored)
	}

	// The recorded declaration is a copy of the pinned plan's: a disagreement
	// either way is corrupt state, never a value to prefer.
	for name, feature := range map[string]string{"another feature": "another-feature", "no feature": ""} {
		forged := *held.origin
		forged.AcceptanceFeature = feature
		if err := verifyAcceptanceFeatureOrigin(j.fleet.store, &forged); err == nil {
			t.Errorf("an origin recording %s was accepted against its pinned plan", name)
		}
	}
	if err := verifyAcceptanceFeatureOrigin(j.fleet.store, held.origin); err != nil {
		t.Fatalf("the genuine origin was refused: %v", err)
	}
	producer := *held.origin
	producer.UnitID = "backend"
	if err := verifyAcceptanceFeatureOrigin(j.fleet.store, &producer); err == nil {
		t.Error("an intermediate producer was accepted as carrying the final integration's declaration")
	}

	// The journal already holds a verdict for this exact tree, so a run that
	// recomposes to it is told so instead of verifying it again; a different
	// tree, or a different base, is not covered by it.
	sibling := &runState{rt: j.fleet.runtime, run: EngineeringRun{ID: "sibling", Goal: held.run.Goal}, origin: held.origin,
		sources: []sourceRecord{{BaseRevision: held.pinnedBase()}}}
	if prior, identical, err := j.fleet.runtime.priorFailedFeatureVerdict(sibling, failedTree); err != nil || !identical || prior != heldRunID {
		t.Fatalf("the identical failed tree was not recognised: %q %t %v", prior, identical, err)
	}
	if _, identical, _ := j.fleet.runtime.priorFailedFeatureVerdict(sibling, "9999999999999999999999999999999999999999"); identical {
		t.Fatal("a different tree was treated as already failed")
	}
	moved := *sibling
	moved.sources = []sourceRecord{{BaseRevision: "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"}}
	if _, identical, _ := j.fleet.runtime.priorFailedFeatureVerdict(&moved, failedTree); identical {
		t.Fatal("a verdict made against another base was reused")
	}

	// 5. The operator selects ONE exact input. That grants nothing.
	if err := j.resolve(request.ID, "backend", "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	var epochRequestID string
	j.until("the existing epoch_authorization decision is raised for the selected unit", func() bool {
		backendRun, ok := storedRun(t, j.fleet.phase8Fixture, j.fleet.graph(j.graphID).unit(t, "backend").RunID)
		if !ok || backendRun.Orchestration == nil {
			return false
		}
		messages, err := j.fleet.store.ScopeMessages(backendRun.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				epochRequestID = message.ID
			}
		}
		return epochRequestID != ""
	})
	for range 6 {
		j.tick()
	}
	for _, unit := range []string{"backend", "frontend", "integrate"} {
		authorizations, epochs := j.grants(unit)
		if len(authorizations) != 0 || len(epochs) != 0 {
			t.Fatalf("selecting an input authorized remediation for %s: %+v %+v", unit, authorizations, epochs)
		}
	}
	// The unselected input received nothing.
	if _, _, found := frontendFinding(t, j); found {
		t.Fatal("the unselected input received a Finding")
	}
	// With the question answered, the run rests on the named hold - not on a
	// goal it never reached - and that hold alone frees a new epoch.
	if resting := mustRun(t, j, heldRunID); resting.Disposition != Waiting || resting.Reason != ReasonFeatureAcceptanceFailed {
		t.Fatalf("the answered integrator rests as %s/%s, want waiting/%s", resting.Disposition, resting.Reason, ReasonFeatureAcceptanceFailed)
	}
	if settled := (j.fleet.runtime).epochRunSettled(mustRun(t, j, heldRunID)); !settled {
		t.Fatal("the authenticated feature hold does not count as settled for a new epoch")
	}

	// 6. Only the existing governed decision authorizes the epoch.
	if err := j.resolve(epochRequestID, orchestration.EpochAuthorizationApprove, "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	var passedRunID string
	j.until("the integrator reintegrates the corrected output and admits it", func() bool {
		unit := j.fleet.graph(j.graphID).unit(t, "integrate")
		if unit.Output == nil || unit.RunID == heldRunID {
			return false
		}
		passedRunID = unit.RunID
		return true
	})
	authorizations, epochs := j.grants("backend")
	if len(authorizations) != 1 || authorizations[0].AuthorizedBy != RemediationAuthorizedByDecision || len(epochs) != 1 || epochs[0].Epoch != 2 {
		t.Fatalf("backend authorizations %+v epochs %+v", authorizations, epochs)
	}
	if integrator, _ := j.grants("integrate"); len(integrator) != 1 || integrator[0].AuthorizedBy != RemediationAuthorizedByPolicy {
		t.Fatalf("the integrator's reintegration grant is %+v", integrator)
	}
	if other, otherEpochs := j.grants("frontend"); len(other) != 0 || len(otherEpochs) != 0 {
		t.Fatalf("the unselected input was remediated: %+v %+v", other, otherEpochs)
	}

	// 7. The feature passed on a different tree and discharges the claim with
	// the exact record; the earlier failure stays journalled and unrefunded.
	final := j.fleet.state(passedRunID)
	a := final.projection.Assurance
	if a == nil || !a.Passed || a.Tree == failedTree {
		t.Fatalf("the reintegrated candidate's assurance is %+v (failed tree %s)", a, failedTree)
	}
	if !featureCheckProves(a, final.projection.CandidateRevision, final.pinnedBase(), journeyFeature, featureExpectation{}) {
		t.Fatalf("the passing observation does not prove the feature for the exact candidate: %+v", a.Checks)
	}
	contract, err := j.fleet.runtime.contractFor(final)
	if err != nil {
		t.Fatal(err)
	}
	if !featureAcceptanceRequired(contract) {
		t.Fatalf("the reintegrated run's contract no longer requires the feature: %+v", contract.RequiredClaims)
	}
	bundles, err := j.fleet.runtime.evidenceBundles(final, contract)
	if err != nil {
		t.Fatal(err)
	}
	discharged := false
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			discharged = discharged || item.ClaimID == FeatureAcceptanceClaimID
		}
	}
	if !discharged {
		t.Fatal("the passing feature record did not discharge the claim")
	}
	if got := len(j.assurance.featureRequests()); got != verifications+1 {
		t.Fatalf("feature verifications = %d, want %d: the failed tree's verification and confirmation plus one for the corrected tree", got, verifications+1)
	}
}

func mustRun(t *testing.T, j *featureJourney, runID string) EngineeringRun {
	t.Helper()
	run, ok := storedRun(t, j.fleet.phase8Fixture, runID)
	if !ok {
		t.Fatalf("run %s is missing", runID)
	}
	return run
}

// frontendFinding looks for any Finding delivered into the unselected input's
// own scope.
func frontendFinding(t *testing.T, j *featureJourney) (orchestration.EngineeringMessage, bool, bool) {
	t.Helper()
	run, ok := storedRun(t, j.fleet.phase8Fixture, j.fleet.graph(j.graphID).unit(t, "frontend").RunID)
	if !ok || run.Orchestration == nil {
		return orchestration.EngineeringMessage{}, false, false
	}
	messages, err := j.fleet.store.ScopeMessages(run.Orchestration.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range orchestration.Live(messages) {
		if message.Kind == orchestration.KindFinding {
			return message, true, true
		}
	}
	return orchestration.EngineeringMessage{}, false, false
}

// A producer's remediation that changes nothing recomposes to the identical
// failed tree. The journal already holds that verdict, so the integrator holds
// again WITHOUT spending another verification, and asks the operator again about
// the new execution rather than looping.
func TestAnIdenticalFailedIntegrationIsHeldAgainWithoutVerifyingItAgain(t *testing.T) {
	j := newFeatureJourney(t, &featureJourneyWorker{noFix: true})
	var firstRunID string
	j.until("the first integration holds a judged failure", func() bool {
		firstRunID = j.integratorRunID()
		return firstRunID != "" && failedFeatureObservation(j.fleet.state(firstRunID).projection) != nil
	})
	failedTree := failedFeatureObservation(j.fleet.state(firstRunID).projection).Tree
	j.until("the attribution is asked", func() bool { _, asked := j.attributionRequest(); return asked })
	request, _ := j.attributionRequest()
	if err := j.resolve(request.ID, "backend", "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	var epochRequestID string
	j.until("the epoch authorization is raised", func() bool {
		backendRun, ok := storedRun(t, j.fleet.phase8Fixture, j.fleet.graph(j.graphID).unit(t, "backend").RunID)
		if !ok || backendRun.Orchestration == nil {
			return false
		}
		messages, err := j.fleet.store.ScopeMessages(backendRun.Orchestration.BatchID)
		if err != nil {
			t.Fatal(err)
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind == orchestration.KindDecisionRequest && message.Purpose == orchestration.EpochAuthorizationPurpose {
				epochRequestID = message.ID
			}
		}
		return epochRequestID != ""
	})
	verifications := len(j.assurance.featureRequests())
	if err := j.resolve(epochRequestID, orchestration.EpochAuthorizationApprove, "operator@example.com"); err != nil {
		t.Fatal(err)
	}
	var secondRunID string
	j.until("the integrator reintegrates the unchanged remediation", func() bool {
		secondRunID = j.integratorRunID()
		if secondRunID == "" || secondRunID == firstRunID {
			return false
		}
		return failedFeatureObservation(j.fleet.state(secondRunID).projection) != nil
	})
	second := failedFeatureObservation(j.fleet.state(secondRunID).projection)
	if second.Tree != failedTree {
		t.Fatalf("the recomposed tree is %s, want the identical failed tree %s", second.Tree, failedTree)
	}
	if !strings.HasPrefix(second.FailureSignature, "identical_failed_candidate:") || !strings.HasSuffix(second.FailureSignature, firstRunID) {
		t.Fatalf("the repeated failure does not name the verdict it repeats: %q", second.FailureSignature)
	}
	for range 6 {
		j.tick()
	}
	if after := len(j.assurance.featureRequests()); after != verifications {
		t.Fatalf("the identical failed tree was verified again: %d -> %d", verifications, after)
	}
	if run := mustRun(t, j, secondRunID); run.Disposition != Waiting || run.Reason == ReasonGoalStateReached {
		t.Fatalf("the repeated failure settled %s/%s", run.Disposition, run.Reason)
	}
	if again, asked := j.attributionRequest(); !asked || again.ID == request.ID {
		t.Fatalf("the new execution did not ask its own attribution: %+v asked=%t", again.ID, asked)
	}
}

// featureRule is the operator-activated policy rule: the runtime-derived fact
// requires the claim, and the obligation is material.
func featureRule() domain.PolicyRule {
	return domain.PolicyRule{
		When: domain.PolicyCondition{Fact: FinalIntegrationFactKey, Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{
			RequiredClaims: &map[string]domain.RequiredClaim{FeatureAcceptanceClaimID: {EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true}},
			Obligations: &map[string]domain.PolicyRequirement{"feature-acceptance": {
				Statement: "The declared feature must pass its acceptance tests on the exact integrated candidate.", RequiredClaims: &[]string{FeatureAcceptanceClaimID}, Material: true}},
		},
	}
}

// A declaration with nothing to enforce it, and a verifier that ignores it, both
// fail closed: the integrator never reaches a generic pass and never hands off.
func TestADeclaredFeatureNothingEnforcesNeverReleasesTheIntegrator(t *testing.T) {
	for name, options := range map[string]featureJourneyOptions{
		"the policy rule was never activated": {withoutRule: true},
		"the verifier ignores the feature":    {ignoreFeature: true},
	} {
		t.Run(name, func(t *testing.T) {
			j := newFeatureJourneyWith(t, &featureJourneyWorker{}, options)
			for range 40 {
				j.tick()
			}
			unit := j.fleet.graph(j.graphID).unit(t, "integrate")
			if unit.Output != nil {
				t.Fatalf("the integrator handed off with no feature acceptance: %+v", unit.Output)
			}
			if unit.RunID == "" {
				t.Fatal("the integrator never started, so nothing was tested")
			}
			state := j.fleet.state(unit.RunID)
			if a := state.projection.Assurance; a != nil && a.Passed {
				t.Fatalf("a passing assurance was recorded for a candidate that never proved its feature: %+v", a)
			}
		})
	}
}

// A verifier that returns Passed with a structurally valid but mismatched
// feature record must not admit an integration handoff: every field of the
// current binding is individually load-bearing.
func TestAMismatchedFeatureRecordNeverAdmitsAnIntegrationHandoff(t *testing.T) {
	other := strings.Repeat("e", 40)
	// The control: the same fixture with an exact record IS admitted, so a refusal
	// below is the mismatch's and nothing else's.
	t.Run("an exact record", func(t *testing.T) {
		j := newFeatureJourneyWith(t, &featureJourneyWorker{alwaysFix: true}, featureJourneyOptions{})
		j.until("the exact record releases the integrator", func() bool {
			return j.fleet.graph(j.graphID).unit(t, "integrate").Output != nil
		})
	})
	for name, mutate := range map[string]func(*VerificationCheck){
		"the wrong base":           func(c *VerificationCheck) { c.BaseRevision = other },
		"a stale checker revision": func(c *VerificationCheck) { c.CheckerRevision = "stale-checker-revision" },
		"another commit":           func(c *VerificationCheck) { c.Commit = other },
		"another tree":             func(c *VerificationCheck) { c.Tree = other },
		"another selection":        func(c *VerificationCheck) { c.Selection = strings.Repeat("9", 64) },
		"a skipped test":           func(c *VerificationCheck) { c.Results = &CheckResults{Total: 2, Passed: 1, Skipped: 1} },
		"fewer tests than the manifest": func(c *VerificationCheck) {
			c.Results = &CheckResults{Total: 1, Passed: 1}
		},
		"another feature":     func(c *VerificationCheck) { c.Feature = "another-feature" },
		"another record kind": func(c *VerificationCheck) { c.Kind, c.Checker = CheckKindBrowserAcceptance, browserCheckerID },
		"no artifacts":        func(c *VerificationCheck) { c.ArtifactsSHA256 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			j := newFeatureJourneyWith(t, &featureJourneyWorker{alwaysFix: true}, featureJourneyOptions{mutate: mutate})
			// The corrected tree is the one that passes, so drive until the
			// integrator has been verified at least once.
			j.until("the integrator was verified", func() bool { return len(j.assurance.featureRequests()) > 0 })
			for range 30 {
				j.tick()
			}
			unit := j.fleet.graph(j.graphID).unit(t, "integrate")
			if unit.Output != nil {
				t.Fatalf("the integrator handed off on %s: %+v", name, unit.Output)
			}
			if a := j.fleet.state(unit.RunID).projection.Assurance; a != nil && a.Passed {
				t.Fatalf("a passing observation was journalled on %s: %+v", name, a)
			}
		})
	}
}
