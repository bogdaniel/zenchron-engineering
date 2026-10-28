package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

const testFeature = "delivery-report"

func featureContract() domain.EngineeringWorkContract {
	contract := dischargeContract()
	delete(contract.RequiredClaims, FileSizePolicyClaimID)
	contract.RequiredClaims[FeatureAcceptanceClaimID] = domain.RequiredClaim{EvidenceClass: AssuranceEvidenceClass}
	return contract
}

func goodFeatureCheck() VerificationCheck {
	check := goodBrowserCheck()
	check.Kind, check.Checker, check.CheckerRevision = CheckKindFeatureAcceptance, featureCheckerID, featureCheckerRevision()
	check.Feature = testFeature
	return check
}

// featureClaims replays a journal for a run whose verified origin declares
// feature, the way a restarted controller would.
func featureClaims(t *testing.T, events []EngineeringEvent, base, feature string, contract domain.EngineeringWorkContract) map[string]domain.EvidenceItem {
	t.Helper()
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	state := &runState{projection: projection, events: events, run: EngineeringRun{ID: "run-d"}, sources: []sourceRecord{{BaseRevision: base}}}
	if feature != "" {
		state.origin = &orchestration.BatchOrigin{GraphID: "g", UnitID: "integrate", ExecutionKind: orchestration.ExecutionKindIntegrationCompose, AcceptanceFeature: feature}
	}
	rt := &EngineeringRuntime{deps: Dependencies{Assurance: BaselineGoVerifier{}}}
	bundles, err := rt.evidenceBundles(state, contract)
	if err != nil {
		t.Fatal(err)
	}
	claims := map[string]domain.EvidenceItem{}
	for _, bundle := range bundles {
		for _, item := range bundle.Evidence {
			claims[item.ClaimID] = item
		}
	}
	return claims
}

// Generic green assurance, an ordinary browser record and every near-miss of the
// feature record leave the feature claim undischarged; only the exact record
// for the declared feature discharges it.
func TestOnlyTheExactFeatureRecordDischargesTheFeatureClaim(t *testing.T) {
	contract := featureContract()
	discharged := func(checks []VerificationCheck, base, feature string) bool {
		_, ok := featureClaims(t, journalFor(t, checks), base, feature, contract)[FeatureAcceptanceClaimID]
		return ok
	}
	if !discharged([]VerificationCheck{goodFeatureCheck()}, dischargeBase, testFeature) {
		t.Fatal("the exact feature record did not discharge the claim")
	}
	if discharged(nil, dischargeBase, testFeature) {
		t.Fatal("a generic green suite discharged the feature claim")
	}
	if discharged([]VerificationCheck{goodBrowserCheck()}, dischargeBase, testFeature) {
		t.Fatal("an ordinary browser record discharged the feature claim")
	}
	asBrowser := goodFeatureCheck()
	asBrowser.Kind, asBrowser.Checker, asBrowser.CheckerRevision = CheckKindBrowserAcceptance, browserCheckerID, browserCheckerRevision()
	if discharged([]VerificationCheck{asBrowser}, dischargeBase, testFeature) {
		t.Fatal("a browser-kind record carrying the feature name discharged the claim")
	}
	for name, mutate := range map[string]func(*VerificationCheck){
		"another feature":  func(c *VerificationCheck) { c.Feature = "another-feature" },
		"no feature":       func(c *VerificationCheck) { c.Feature = "" },
		"another commit":   func(c *VerificationCheck) { c.Commit = "d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2d2" },
		"another tree":     func(c *VerificationCheck) { c.Tree = "9999999999999999999999999999999999999999" },
		"another base":     func(c *VerificationCheck) { c.BaseRevision = "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3" },
		"a failed outcome": func(c *VerificationCheck) { c.Outcome = CheckOutcomeFailed },
		"another checker":  func(c *VerificationCheck) { c.Checker = browserCheckerID },
		"an old checker":   func(c *VerificationCheck) { c.CheckerRevision = "old" },
		"a skipped test":   func(c *VerificationCheck) { c.Results = &CheckResults{Total: 4, Passed: 3, Skipped: 1} },
		"no results":       func(c *VerificationCheck) { c.Results = nil },
		"no artifacts":     func(c *VerificationCheck) { c.ArtifactsSHA256 = "" },
		"no selection":     func(c *VerificationCheck) { c.Selection = "" },
	} {
		check := goodFeatureCheck()
		mutate(&check)
		if discharged([]VerificationCheck{check}, dischargeBase, testFeature) {
			t.Errorf("a feature record with %s discharged the claim", name)
		}
	}
	// The record is judged against the run's verified origin, not against itself.
	if discharged([]VerificationCheck{goodFeatureCheck()}, dischargeBase, "another-feature") {
		t.Error("a record for the declared feature discharged a run declaring another")
	}
	if discharged([]VerificationCheck{goodFeatureCheck()}, dischargeBase, "") {
		t.Error("a run whose origin declares no feature accepted a feature record")
	}
	if discharged([]VerificationCheck{goodFeatureCheck()}, "", testFeature) {
		t.Error("a run with no base revision accepted a feature record")
	}
	// ...and the feature record is not the browser claim's evidence either.
	browserContract := browserContract()
	if _, ok := featureClaims(t, journalFor(t, []VerificationCheck{goodFeatureCheck()}), dischargeBase, testFeature, browserContract)[BrowserAcceptanceClaimID]; ok {
		t.Error("a feature record discharged the ordinary browser claim")
	}
}

func TestAFeatureRecordIsAValidTypedRecordOnlyWhenComplete(t *testing.T) {
	if err := validateVerificationChecks([]VerificationCheck{goodFeatureCheck()}); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*VerificationCheck){
		"no feature":  func(c *VerificationCheck) { c.Feature = "" },
		"no results":  func(c *VerificationCheck) { c.Results = nil },
		"no artifact": func(c *VerificationCheck) { c.Artifact = "" },
	} {
		check := goodFeatureCheck()
		mutate(&check)
		if validateVerificationChecks([]VerificationCheck{check}) == nil {
			t.Errorf("a feature record with %s was accepted", name)
		}
	}
	browser := goodBrowserCheck()
	browser.Feature = testFeature
	if validateVerificationChecks([]VerificationCheck{browser}) == nil {
		t.Error("a browser record naming a feature was accepted")
	}
}

func TestAContractThatRequiresFeatureAcceptanceRefusesARunThatCannotProveItsDeclaration(t *testing.T) {
	required, plain := featureContract(), dischargeContract()
	for name, tc := range map[string]struct {
		contract      domain.EngineeringWorkContract
		feature, base string
		refused       bool
	}{
		"declared with a base":          {required, testFeature, dischargeBase, false},
		"required but nothing declared": {required, "", dischargeBase, true},
		"required but no base":          {required, testFeature, "", true},
		"not required":                  {plain, "", "", false},
	} {
		if err := featureBaseRefusal(tc.contract, tc.feature, tc.base); (err != nil) != tc.refused {
			t.Errorf("%s: refusal = %v", name, err)
		}
	}
	if featureAcceptanceFeature(plain, testFeature) != "" || featureAcceptanceBase(plain, dischargeBase) != "" {
		t.Error("a contract that does not require the claim still asked the verifier for the feature")
	}
	if featureAcceptanceFeature(required, testFeature) != testFeature || featureAcceptanceBase(required, dischargeBase) != dischargeBase {
		t.Error("a contract that requires the claim did not ask the verifier for the feature")
	}
}

func TestTheFeatureBindingChangesWithTheFeatureAndTheBase(t *testing.T) {
	reference := featureBindingSuffixFor(testFeature, dischargeBase)
	for name, other := range map[string]string{
		"another feature": featureBindingSuffixFor("another-feature", dischargeBase),
		"another base":    featureBindingSuffixFor(testFeature, "d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3d3"),
	} {
		if other == reference {
			t.Errorf("%s left the assurance binding unchanged", name)
		}
	}
	if (&runState{}).featureBindingSuffix() != "" {
		t.Error("a run with no runtime or origin changed its binding")
	}
}

// A judged feature failure is a hold: it is a candidate verdict, it never routes
// to a wait that would refund what it spent, nor to a retry of the same tree.
func TestAJudgedFeatureFailureRoutesToAHoldNotAWaitOrARetry(t *testing.T) {
	if RouteFailure(FailureFeatureAcceptance) != RouteHold {
		t.Fatalf("a feature failure routes to %q", RouteFailure(FailureFeatureAcceptance))
	}
	if reattemptable(RouteHold) {
		t.Error("a hold is reattemptable")
	}
	result, err := json.Marshal(assuranceRecord{FailureClass: FailureFeatureAcceptance})
	if err != nil {
		t.Fatal(err)
	}
	if _, waiting := waitRoutedFailure(result); waiting {
		t.Error("a feature failure is wait-routed, so its spent work would be refunded")
	}
	if !CandidateVerdict(AssuranceResult{FailureClass: FailureFeatureAcceptance}) {
		t.Error("a feature failure is not a candidate verdict")
	}
	if !externalWaitReasons[ReasonFeatureAcceptanceFailed] {
		t.Error("the hold spends the run's wall budget while an operator decides")
	}
}

// The fact comes from the verified origin alone, at both stages, for that run
// only; a run without a declaration compiles exactly as before.
func TestTheDeclarationFactReachesTheContractOnlyForTheDeclaredRun(t *testing.T) {
	model, base := activationFixtures(t)
	rule := domain.PolicyRule{
		When: domain.PolicyCondition{Fact: FinalIntegrationFactKey, Equals: domain.FactTrue},
		Effect: domain.PolicyEffect{
			RequiredClaims: &map[string]domain.RequiredClaim{FeatureAcceptanceClaimID: {EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true}},
			Obligations: &map[string]domain.PolicyRequirement{"feature-acceptance": {
				Statement: "The declared feature must pass its acceptance tests on the exact integrated candidate.", RequiredClaims: &[]string{FeatureAcceptanceClaimID}, Material: true}},
		},
	}
	rules := map[string]domain.PolicyRule{}
	for id, existing := range base.Rules {
		rules[id] = existing
	}
	rules["FEATURE-FINAL-INTEGRATION-001"] = rule
	withRule := base
	withRule.Rules, withRule.Revision = rules, base.Revision+"-feature"

	compile := func(feature string) (predicted, observed domain.EngineeringWorkContract) {
		flow := KernelFlow{}
		if feature != "" {
			flow = KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(analysis.CriticalBoundaryDetector{}, declaredFeatureDetector{feature: feature})}
		}
		state, err := flow.Compile(SourceSnapshot{ID: "src", Objective: "Integrate.", AcceptanceIntent: []string{"Works."}, PredictedPaths: []string{predictedScopePlaceholder}}, model, withRule, "contract-run-d", "1")
		if err != nil {
			t.Fatal(err)
		}
		next, err := flow.ObserveCandidate(state, model, withRule, domain.Subject{Repository: model.Subject.Repository, Revision: dischargeCommit}, []string{"runtime/feature_acceptance.go"})
		if err != nil {
			t.Fatal(err)
		}
		return state.Contract, next.Contract
	}
	predicted, observed := compile(testFeature)
	for stage, contract := range map[string]domain.EngineeringWorkContract{"predicted": predicted, "observed": observed} {
		if !featureAcceptanceRequired(contract) || !contract.Obligations["feature-acceptance"].Material {
			t.Errorf("the %s contract of a declared run lacks the material feature obligation: %+v", stage, contract.Obligations)
		}
	}
	plainPredicted, plainObserved := compile("")
	for stage, contract := range map[string]domain.EngineeringWorkContract{"predicted": plainPredicted, "observed": plainObserved} {
		if featureAcceptanceRequired(contract) {
			t.Errorf("the %s contract of an undeclared run requires the feature: %+v", stage, contract.RequiredClaims)
		}
	}
	// An undeclared run compiles byte-for-byte as it did before the rule existed.
	oldPredicted, err := KernelFlow{}.Compile(SourceSnapshot{ID: "src", Objective: "Integrate.", AcceptanceIntent: []string{"Works."}, PredictedPaths: []string{predictedScopePlaceholder}}, model, base, "contract-run-d", "1")
	if err != nil {
		t.Fatal(err)
	}
	left, _ := json.Marshal(oldPredicted.Contract.RequiredClaims)
	right, _ := json.Marshal(plainPredicted.RequiredClaims)
	if !strings.EqualFold(string(left), string(right)) {
		t.Errorf("an undeclared run's required claims changed: %s vs %s", left, right)
	}
}

func activationFixtures(t *testing.T) (domain.ProjectModel, domain.EngineeringPolicy) {
	t.Helper()
	read := func(name string) []byte {
		data, err := os.ReadFile(filepath.Join("..", "fixtures", "v0.1", "valid", name))
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	model, err := domain.Decode[domain.ProjectModel](read("security-sensitive.project-model.json"))
	if err != nil {
		t.Fatal(err)
	}
	policy, err := domain.Decode[domain.EngineeringPolicy](read("security-sensitive.engineering-policy.json"))
	if err != nil {
		t.Fatal(err)
	}
	return model, policy
}

// The attribution decision fails closed and says why: a scope with no room, an
// integration that consumed too many inputs, or none, asks nothing and writes
// nothing.
func TestTheAttributionDecisionRefusesWhenItCannotBeAskedCompletely(t *testing.T) {
	inputs := func(n int) orchestration.WorkUnitInputs {
		out := make(orchestration.WorkUnitInputs, n)
		for i := range out {
			out[i] = orchestration.WorkUnitInput{UnitID: "unit-" + string(rune('a'+i))}
		}
		return out
	}
	batch := orchestration.Batch{ID: "batch-1", Items: []orchestration.BatchItem{{Issue: 7, RunID: "run-1"}}}
	for name, tc := range map[string]struct {
		inputs orchestration.WorkUnitInputs
		scope  int
	}{
		"no inputs":          {inputs(0), 0},
		"too many inputs":    {inputs(maxAttributedInputs + 1), 0},
		"a full message cap": {inputs(2), orchestration.MaxScopeMessages},
	} {
		origin := orchestration.BatchOrigin{GraphID: "g", UnitID: "integrate", AcceptanceFeature: testFeature, Inputs: tc.inputs}
		// A nil store proves nothing was written: the refusal precedes any write.
		if err := admitFeatureAttributionRequest(nil, batch, origin, "run-1", "request", "c", "t", tc.scope, time.Now()); err == nil {
			t.Errorf("%s: the decision was asked", name)
		}
	}
}

func TestPlanApprovalRefusesAnIntegrationTheAttributionCannotName(t *testing.T) {
	stage := func(id string, deps ...string) domain.PlanStage {
		return domain.PlanStage{ID: id, Kind: domain.StageAgent, Role: domain.RoleImplementer, Objective: id,
			InvocationMode: domain.InvocationModeMutating, DependsOn: deps}
	}
	plan := func(producers int, producerID string) domain.EngineeringPlan {
		var stages []domain.PlanStage
		var deps []string
		for i := 0; i < producers; i++ {
			id := fmt.Sprintf("unit-%02d", i)
			if i == 0 && producerID != "" {
				id = producerID
			}
			stages = append(stages, stage(id))
			deps = append(deps, id)
		}
		stages = append(stages, domain.PlanStage{ID: "integrate", Kind: domain.StageAgent, Role: domain.RoleIntegrator, Objective: "compose",
			ExecutionKind: string(orchestration.ExecutionKindIntegrationCompose), DependsOn: deps, AcceptanceFeature: testFeature, InvocationMode: domain.InvocationModeMutating})
		return domain.EngineeringPlan{Stages: stages}
	}
	if err := validateAcceptanceFeaturePlan(plan(maxAttributedInputs, "")); err != nil {
		t.Fatalf("exactly %d inputs were refused: %v", maxAttributedInputs, err)
	}
	if err := validateAcceptanceFeaturePlan(plan(maxAttributedInputs+1, "")); err == nil {
		t.Errorf("%d inputs were accepted", maxAttributedInputs+1)
	}
	if err := validateAcceptanceFeaturePlan(plan(2, attributionNone)); err == nil {
		t.Error("an input named with the reserved word was accepted")
	}
	// The ordinary WorkGraph capacity is not what bounds an undeclared plan.
	undeclared := plan(maxAttributedInputs+1, "")
	undeclared.Stages[len(undeclared.Stages)-1].AcceptanceFeature = ""
	if err := validateAcceptanceFeaturePlan(undeclared); err != nil {
		t.Errorf("an undeclared plan was held to the attribution bound: %v", err)
	}
}
