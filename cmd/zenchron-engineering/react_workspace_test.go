package main

import (
	"context"
	"encoding/json"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real composition/controller/socket/journal/browser, with external producers
// and forge modeled offline. This never publishes, merges or touches production.
func TestReactEngineeringOSGovernedWorkspace(t *testing.T) {
	if os.Getenv("ZENCHRON_REACT_BROWSER_E2E") != "1" {
		t.Skip("opt-in browser host required")
	}
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "zc-react-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dir, config := planWorkspaceIn(t, state)
	t.Chdir(dir)
	loaded, err := runtime.LoadConfig(config, dir)
	if err != nil {
		t.Fatal(err)
	}
	writePlanningFile(t, loaded.PlanningDir, "profiles/browser-implementer.json", `{"execution_agent":"openai-responses","capabilities":["code_change","repository_analysis","verification"],"trust_requirement":"protected"}`)
	for _, args := range [][]string{{"checkout", "-b", "main"}, {"config", "user.name", "isolated-fixture"}, {"config", "user.email", "fixture@example.test"}} {
		reactFixtureGit(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("isolated base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reactFixtureGit(t, dir, "add", "README.md")
	reactFixtureGit(t, dir, "commit", "--no-gpg-sign", "-m", "isolated base")
	base := reactFixtureGit(t, dir, "rev-parse", "HEAD")
	plan := proposePlan(t, config, 41)
	activeRun(t, config, dir, "browser-run")
	overrides := planOverrides(t, 41)
	built, err := newComposition(autonomyFlags{Config: config}, overrides)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	if err := os.WriteFile(built.agent.CredentialPath, []byte("unused-isolated-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	forge := built.forge.(*runtime.FakeGitHubAdapter)
	forge.Refs["main"] = base
	forge.Issues[42] = runtime.GitHubIssue{Number: 42, URL: "https://github.com/zenchron/seeded/issues/42", Title: "Make widget retry idempotent", Body: "Repeated retries preserve one result.", State: runtime.GitHubOpen, UpdatedAt: time.Unix(1700000001, 0).UTC(), Author: runtime.GitHubActor{Login: "operator", ID: 7}}
	role, err := runtime.AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = role.Release() })
	built.role = role
	attestedBuild(t)
	self, err := controllerSelf()
	if err != nil {
		t.Fatal(err)
	}
	build := self.Build
	if _, err := built.store.ReadoptController(runtime.ControllerReadoption{ID: "isolated-react", Reason: "isolated browser acceptance", Binding: runtime.ControllerBinding{Controller: "test", Build: &build, Config: built.config.Digest}, RecordedAt: time.Now().UTC()}, nil); err != nil {
		t.Fatal(err)
	}
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	service, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.DurableView(plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	graphID, err := service.BindObjectivePlan(view.Plan, 41, built.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	hold, err := driver.PlaceWorkUnitHold(runtime.ControlRequest{GraphID: graphID, UnitID: "implementation", Operator: "operator-1", Note: "Inspect exact objective context"})
	if err != nil {
		t.Fatal(err)
	}
	publication := seedReactPublication(t, built, forge, dir, base)
	runBrowserFixture(t, root, built, driver, []string{"ZENCHRON_E2E_REACT=1", "ZENCHRON_E2E_PLAN=" + plan, "ZENCHRON_E2E_OBJECTIVE_GRAPH=" + graphID, "ZENCHRON_E2E_OBJECTIVE_DECISION=" + hold.ID, "ZENCHRON_E2E_PUBLICATION_RUN=" + publication})
	resolutions, found, err := built.store.DecisionResolutionByRequestID(hold.ID)
	if err != nil || !found || resolutions.Outcome != (orchestration.DecisionOutcome{Kind: orchestration.DecisionAllowDeny, Value: orchestration.DecisionAllow}) {
		t.Fatalf("decision receipt not durable: found=%t err=%v", found, err)
	}
	events, err := built.store.Events(publication)
	if err != nil {
		t.Fatal(err)
	}
	human := 0
	for _, e := range events {
		if e.Type == runtime.EventHumanAuthorityRecorded {
			human++
		}
		if e.Type == runtime.EventGitHubPRObserved {
			t.Fatal("human evidence test published a PR")
		}
	}
	if human != 1 {
		t.Fatalf("human evidence records %d, want one", human)
	}
	checkReactDeliveryReceipts(t, built, publication)
}

func checkReactDeliveryReceipts(t *testing.T, built *composition, publication string) {
	t.Helper()
	plans, err := built.store.Plans()
	if err != nil {
		t.Fatal(err)
	}
	events, err := built.store.Events(publication)
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		rows, err := built.store.PlanEvents(plan.ID)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, rows...)
	}
	receipts := []runtime.OperatorActionReceipt{}
	for _, event := range events {
		if event.Type != runtime.EventPlanOperatorActionReceipt && event.Type != runtime.EventRunOperatorActionReceipt {
			continue
		}
		if event.StateBefore != event.StateAfter {
			t.Fatal("delivery observation changed workflow state")
		}
		var receipt runtime.OperatorActionReceipt
		if err := json.Unmarshal(event.Payload, &receipt); err != nil {
			t.Fatal(err)
		}
		receipts = append(receipts, receipt)
	}
	if len(receipts) != 4 {
		t.Fatalf("want one delivery receipt per affected action, got %d", len(receipts))
	}
	if err := built.store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := runtime.OpenReadStore(built.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, expected := range receipts {
		got, err := reader.OperatorActionReceipt(expected.OperationID)
		if err != nil || got == nil || *got != expected {
			t.Fatalf("receipt not reconstructed: %v", err)
		}
	}
}
func reactFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	out, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// The provider writes a real candidate. It provides no acceptance evidence.
type reactCandidateProvider struct{ provenProvider }

func (p reactCandidateProvider) Execute(_ context.Context, r runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	if err := os.WriteFile(filepath.Join(r.CandidateDir, "candidate.go"), []byte("package candidate\n"), 0600); err != nil {
		return runtime.ExecutionResult{}, err
	}
	return runtime.ExecutionResult{ProviderID: "isolated-producer", Outcome: execution.Succeeded, ChangedPaths: []string{"candidate.go"}}, nil
}
func seedReactPublication(t *testing.T, c *composition, forge *runtime.FakeGitHubAdapter, origin, base string) string {
	t.Helper()
	boundaries := map[string]domain.CriticalBoundary{"service": {Type: "service", Paths: []string{"internal/service/**"}}}
	c.model = domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "isolated-model", Revision: "1", Subject: domain.Subject{Repository: "zenchron/seeded", Revision: base}, CriticalBoundaries: &boundaries}
	claims := map[string]domain.RequiredClaim{"verification": {EvidenceClass: runtime.AssuranceEvidenceClass, IndependentFromChangeProducer: true}, "acceptance": {EvidenceClass: runtime.SemanticEvidenceClass, IndependentFromChangeProducer: true}, "publication-human": {EvidenceClass: runtime.HumanApprovalEvidenceClass, IndependentFromChangeProducer: true}}
	permissions := []domain.Action{{Type: runtime.PublicationActionType, Target: "main"}}
	conditions := []domain.AuthorityCondition{{Action: permissions[0], RequiredClaims: []string{"verification", "publication-human"}}}
	discharge := []string{"acceptance"}
	effect := domain.PolicyEffect{RequiredClaims: &claims, Permissions: &permissions, AuthorityConditions: &conditions, AcceptanceDischargeClaims: &discharge}
	rules := map[string]domain.PolicyRule{}
	for name, value := range map[string]domain.FactValue{"unknown": domain.FactUnknown, "clear": domain.FactFalse, "touched": domain.FactTrue} {
		rules[name] = domain.PolicyRule{When: domain.PolicyCondition{Fact: "service.boundary_modified", Equals: value}, Effect: effect}
	}
	c.policy = domain.EngineeringPolicy{SchemaVersion: domain.SchemaVersion, ID: "isolated-human-publication", Revision: "1", Rules: rules}
	c.provider = reactCandidateProvider{}
	c.providerInjected = true
	c.assurance = &runtime.FakeAssuranceProvider{Results: []runtime.AssuranceResult{{ProviderID: "isolated-independent-verifier", VerifierDefinition: "verifier-v1", Passed: true}}}
	c.semantic = &runtime.FakeSemanticAssuranceProvider{}
	forge.Issues[71] = runtime.GitHubIssue{Number: 71, URL: "https://github.com/zenchron/seeded/issues/71", Title: "Bounded publication candidate", Body: "Produce one isolated candidate.", State: runtime.GitHubOpen, UpdatedAt: time.Unix(1700000002, 0).UTC(), Author: runtime.GitHubActor{Login: "operator", ID: 7}}
	engine, err := c.engine(runtime.RepositoryTarget{Identity: "zenchron/seeded", Remote: origin, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := engine.StartOrResumeIssueRun(context.Background(), 71)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := engine.Reconcile(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	request, err := engine.PendingAuthorityRequest(id)
	if err != nil || request == nil {
		t.Fatalf("real candidate did not reach human authority: disposition=%s err=%v", outcome.Disposition, err)
	}
	return id
}
