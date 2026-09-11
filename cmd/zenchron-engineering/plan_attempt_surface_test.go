package main

// The operator surface for a plan that never reached a plan.
//
// The #120 dogfood ended with `plan list` saying "no plans" and `plan show`
// saying "no such plan" about work an operator had just spent a provider
// invocation on. These tests drive the real CLI against real durable state and
// hold it to the opposite: the attempt is listed, it is readable, and it is
// unmistakably not an approvable plan.

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// seedRefusedAttempt records one refused reasoning proposal in the operator's
// own state directory, exactly as a refused `plan issue N` would.
func seedRefusedAttempt(t *testing.T, configPath, dir, planID string) {
	t.Helper()
	seedProposal(t, configPath, dir, planID, true)
}

// seedRecoveredPlan proposes a COMPILABLE revision for the same plan identity,
// which is what an operator does after reading a refused attempt.
func seedRecoveredPlan(t *testing.T, configPath, dir, planID string) {
	t.Helper()
	seedProposal(t, configPath, dir, planID, false)
}

func seedProposal(t *testing.T, configPath, dir, planID string, defective bool) {
	t.Helper()
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	subject := domain.Subject{Repository: "zenchron/seeded", Revision: strings.Repeat("a", 40)}
	contract := domain.EngineeringWorkContract{
		SchemaVersion: domain.SchemaVersion, ID: "contract-plan", Revision: "1",
		Objective: "Resolve the M2 hardening cohort.", AcceptanceIntent: []string{"The cohort is resolved."},
		Subject: subject,
		Scope: domain.ContractScope{
			Stage: domain.StageObserved, AllowedPaths: []string{"."}, ProhibitedPaths: []string{},
		},
		Facts: []string{}, Invariants: map[string]domain.Requirement{},
		Obligations:    map[string]domain.Requirement{},
		RequiredClaims: map[string]domain.RequiredClaim{"verification": {EvidenceClass: "automated_test"}},
		Permissions:    []domain.Action{}, Prohibitions: []domain.Action{},
		AuthorityConditions: []domain.AuthorityCondition{},
		Provenance: domain.ContractProvenance{
			ProjectModel: domain.ObjectRevision{ID: "project", Revision: "1"},
			Policy:       domain.ObjectRevision{ID: "policy", Revision: "1"}, CompilerVersion: "compiler-v0.1",
		},
	}
	service := runtime.PlanService{
		Store: store, Clock: runtime.RealClock{}, DefaultAgent: "codex",
		Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 2, MaxProviderInvocations: 8},
	}
	transcript := filepath.Join(config.StateDir, "planner-transcript.log")
	writePlanningFile(t, config.StateDir, "planner-transcript.log", "the planner said things")
	empty := func() *domain.IndependenceRequirement {
		return &domain.IndependenceRequirement{Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{}}
	}
	independence := empty
	if !defective {
		independence = func() *domain.IndependenceRequirement { return nil }
	}
	_, err = service.Propose(context.Background(), runtime.ProposeInput{
		PlanID: planID, Objective: contract.Objective, Subject: subject,
		Repository: "zenchron/seeded", Contract: contract, Issue: 119,
		Model: domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject},
		Reasoned: []domain.PlanStage{
			{ID: "harden-runtime-transitions", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "harden", Independence: independence()},
			{ID: "correct-operator-views", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "correct", DependsOn: []string{"harden-runtime-transitions"}, Independence: independence()},
		},
		Reasoning: &domain.PlanReasoningProvenance{
			AgentID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
			TrustMode: domain.TrustRequirementOperatorTrusted, Model: "gpt-5",
			InvocationMode:        domain.InvocationModeNonMutatingPlanning,
			WorkspaceDigestBefore: strings.Repeat("d", 64), WorkspaceDigestAfter: strings.Repeat("d", 64),
			WorkspaceUnchanged: true,
		},
		Evidence: []runtime.Artifact{{
			Path: transcript, SHA256: strings.Repeat("e", 64), MediaType: "text/plain", LocalOnly: true,
		}},
		References: []runtime.PlanSourceReferencePayload{
			{Repository: "zenchron/seeded", Issue: 110, Digest: strings.Repeat("f", 64), Available: true},
			{Repository: "zenchron/seeded", Issue: 112, Detail: "no issue 112 in zenchron/seeded"},
		},
	})
	if !defective {
		if err != nil {
			t.Fatalf("the corrected proposal was refused: %v", err)
		}
		return
	}
	if err == nil {
		t.Fatal("the dogfood proposal compiled")
	}
	if !strings.Contains(err.Error(), "autonomy plan show "+planID) {
		t.Fatalf("the refusal does not point at the attempt: %v", err)
	}
}

// `plan list` names the plan whose every attempt was refused, instead of
// reporting that no plans exist.
func TestPlanListNamesAPlanThatOnlyHasRefusedAttempts(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-refused")

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "list", "--text", "--config", configPath}, planOverrides(t, 41), &out)
	if err != nil {
		t.Fatalf("list: %v\n%s", err, out.String())
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit = %d", code)
	}
	printed := out.String()
	if strings.Contains(printed, "no plans") {
		t.Fatalf("a refused attempt is still reported as no plans at all:\n%s", printed)
	}
	for _, want := range []string{"plan-refused", "refused reasoning attempt", "autonomy plan show plan-refused"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the list does not state %q:\n%s", want, printed)
		}
	}
}

// `plan show` answers every question an operator has about a failed planning
// attempt, from the durable record and without an artifact directory.
func TestPlanShowRendersARefusedAttempt(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-refused")

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "show", "plan-refused", "--text", "--config", configPath},
		planOverrides(t, 41), &out)
	if err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	if code == runtime.ExitCompleted {
		t.Fatal("a plan with no executable revision exited as though it had one")
	}
	printed := out.String()
	for _, want := range []string{
		// plan and attempt identity
		"plan plan-refused", "attempt attempt-plan-refused-r1-1", "issue #119",
		// whether an executable EngineeringPlan exists
		"executable plan NONE",
		"nothing here is approvable, and nothing here has executed",
		// which reasoning agent, and its provenance
		"reasoned by     codex (codex_cli", "model gpt-5", "non_mutating_planning", "workspace       unchanged",
		// validation status and the typed errors
		"validation      refused", "different_from", "omit its independence requirement",
		// the proposed decomposition, with the ambiguous shorthand visible
		"harden-runtime-transitions (agent, role implementer, independent of nothing in execution_agent)",
		"correct-operator-views (agent, role implementer, after harden-runtime-transitions",
		// referenced-issue provenance, including what could not be read
		"context         zenchron/seeded issue #110 (pinned", "issue #112 (UNAVAILABLE",
		// the transcript, by path
		"evidence        ",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the attempt view does not state %q:\n%s", want, printed)
		}
	}
	// The refusal names the cause rather than the symptom.
	if strings.Contains(printed, "form a cycle") {
		t.Fatalf("the attempt is still explained as a cycle:\n%s", printed)
	}
}

// A plan id nothing ever attempted is still missing. The attempt surface must
// not turn every typo into a readable plan.
func TestAPlanNobodyAttemptedIsStillMissing(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-refused")

	code, err := autonomy([]string{"plan", "show", "plan-never", "--text", "--config", configPath},
		planOverrides(t, 41), &bytes.Buffer{})
	if err == nil {
		t.Fatal("an unattempted plan was shown")
	}
	if code != exitRunNotFound {
		t.Fatalf("exit = %d, want %d", code, exitRunNotFound)
	}
}

// A provider that is ineligible for planning spent nothing, so it leaves no
// attempt. The attempt record is evidence of an invocation, not a log of every
// configuration mistake.
func TestAnIneligiblePlannerLeavesNoAttempt(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)

	var refused bytes.Buffer
	if _, err := autonomy([]string{"plan", "issue", "41", "--text", "--config", configPath},
		planOverrides(t, 41), &refused); err == nil {
		t.Fatalf("a provider with no non-mutating mode produced a plan: %s", refused.String())
	}

	var listed bytes.Buffer
	if _, err := autonomy([]string{"plan", "list", "--text", "--config", configPath},
		planOverrides(t, 41), &listed); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listed.String(), "no plans") {
		t.Fatalf("an ineligible planner created durable plan state:\n%s", listed.String())
	}
}

// A plan that DID reach a revision still shows what it took to get there.
func TestAnApprovableRevisionStillNamesTheAttemptsBeforeIt(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-recovered")

	// The SAME plan identity, proposed again and compiling this time.
	seedRecoveredPlan(t, configPath, dir, "plan-recovered")

	// And the refused attempt is still readable beside the plan that followed.
	var shown bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-recovered", "--text", "--config", configPath},
		planOverrides(t, 41), &shown); err != nil {
		t.Fatalf("show: %v\n%s", err, shown.String())
	}
	if !strings.Contains(shown.String(), "attempt-plan-recovered-r1-1") {
		t.Fatalf("the refused attempt is not visible from the plan surface:\n%s", shown.String())
	}
}

// A plan that HAS an executable revision reports a missing revision as missing.
//
// Falling through to the attempt view would print "executable plan NONE" about
// a plan that has one, which is a worse answer than the refusal it replaced.
func TestAMissingRevisionOfAnExecutablePlanIsStillMissing(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-recovered")
	seedRecoveredPlan(t, configPath, dir, "plan-recovered")

	var out bytes.Buffer
	code, err := autonomy([]string{"plan", "show", "plan-recovered", "--revision", "9", "--text", "--config", configPath},
		planOverrides(t, 41), &out)
	if err == nil {
		t.Fatalf("a revision that does not exist was shown:\n%s", out.String())
	}
	if !strings.Contains(err.Error(), "revision 9 does not exist") {
		t.Fatalf("refusal = %v", err)
	}
	if strings.Contains(out.String(), "executable plan NONE") {
		t.Fatalf("a plan with an executable revision was rendered as having none:\n%s", out.String())
	}
	if code == runtime.ExitCompleted {
		t.Fatalf("exit = %d", code)
	}
}
