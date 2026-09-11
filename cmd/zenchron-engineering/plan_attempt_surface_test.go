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
	"encoding/json"
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

// seedHostileAttempt records a refused attempt whose proposed stage metadata AND
// whose refusal reason both carry terminal control sequences.
//
// The proposal is a CYCLE on purpose. Most refusal constructors wrap a stage id
// in %q, which escapes controls where the message is built; planning/graph.go
// joins the unresolved ids into the cycle message with nothing around them, so
// that is the reason shape that actually carries raw model bytes into the
// journal and then onto a terminal.
func seedHostileAttempt(t *testing.T, configPath, dir, planID, hostile string) {
	t.Helper()
	seedProposalNamed(t, configPath, dir, planID, true, hostile, true)
}

func seedProposal(t *testing.T, configPath, dir, planID string, defective bool) {
	t.Helper()
	seedProposalNamed(t, configPath, dir, planID, defective, "harden-runtime-transitions", false)
}

func seedProposalNamed(t *testing.T, configPath, dir, planID string, defective bool, firstStage string, cyclic bool) {
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
	proposed := []domain.PlanStage{
		{ID: firstStage, Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "harden", Independence: independence()},
		{ID: "correct-operator-views", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective: "correct", DependsOn: []string{firstStage}, Independence: independence()},
	}
	if cyclic {
		// Mutually dependent producers, stating no independence: the compiler
		// adds no edge of its own, so the CYCLE LAW is what refuses this and
		// the message it builds carries the proposed ids verbatim.
		proposed = []domain.PlanStage{
			{ID: firstStage, Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "harden", DependsOn: []string{"correct-operator-views"}},
			{ID: "correct-operator-views", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "correct", DependsOn: []string{firstStage}},
		}
	}
	_, err = service.Propose(context.Background(), runtime.ProposeInput{
		PlanID: planID, Objective: contract.Objective, Subject: subject,
		Repository: "zenchron/seeded", Contract: contract, Issue: 119,
		Model:    domain.ProjectModel{SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject},
		Reasoned: proposed,
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

// The JSON surface says the same thing the text surface says about a revision
// flag it could not honour.
//
// A JSON reader that was silently given something other than what it asked for
// has no way to know, which is the same defect the text notice exists to
// prevent.
func TestBothSurfacesReportAnIgnoredRevisionFlag(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	seedRefusedAttempt(t, configPath, dir, "plan-refused")

	var text bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-refused", "--revision", "7", "--text", "--config", configPath},
		planOverrides(t, 41), &text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "--revision 7 was ignored") {
		t.Fatalf("the text surface does not report the ignored flag:\n%s", text.String())
	}

	var encoded bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-refused", "--revision", "7", "--config", configPath},
		planOverrides(t, 41), &encoded); err != nil {
		t.Fatal(err)
	}
	var view runtime.PlanAttemptsView
	if err := json.Unmarshal(encoded.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v\n%s", err, encoded.String())
	}
	if view.RequestedRevisionIgnored != 7 {
		t.Fatalf("the JSON surface reports requested_revision_ignored = %d, want 7", view.RequestedRevisionIgnored)
	}
	// And an ordinary read says nothing about a flag nobody passed.
	var plain bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-refused", "--config", configPath},
		planOverrides(t, 41), &plain); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plain.String(), "requested_revision_ignored") {
		t.Fatalf("a read with no --revision reported one:\n%s", plain.String())
	}
}

// A refused proposal cannot drive the operator's terminal.
//
// #120 deliberately makes INVALID model output inspectable product state, so
// the strings this view renders are exactly the ones most likely to be
// malformed or hostile. Nothing upstream helps: the durable bounding path is
// ToValidUTF8 plus a byte truncation, and ESC, CSI, OSC and CR are all valid
// UTF-8. The evidence keeps the exact bounded shape; the renderer refuses to
// execute it.
func TestARefusedAttemptCannotDriveTheTerminal(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	// ESC, CR and a bare CSI introducer, in stage metadata AND in the shape of
	// reason that carries model text unquoted: planning/graph.go joins proposed
	// stage ids into the cycle message with no %q around them.
	hostile := "harden\x1b[2J\rall\u009b31m"
	seedHostileAttempt(t, configPath, dir, "plan-hostile", hostile)

	var text bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-hostile", "--text", "--config", configPath},
		planOverrides(t, 41), &text); err != nil {
		t.Fatal(err)
	}
	printed := text.String()
	for _, raw := range []string{"\x1b", "\r", "\u009b", "\x7f"} {
		if strings.Contains(printed, raw) {
			t.Fatalf("the text surface emitted raw control %q:\n%q", raw, printed)
		}
	}
	// Escaped VISIBLY, not silently deleted: what an operator reads has to be
	// what the record holds.
	for _, want := range []string{`\x1b`, `\x0d`, `\x9b`, "harden", "all"} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the text surface does not show %q:\n%s", want, printed)
		}
	}

	// The DURABLE record is untouched: the evidence is the exact bounded
	// proposal, and escaping belongs to the renderer alone.
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.ReplayPlan("plan-hostile")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, stage := range snapshot.Attempts[0].Stages {
		if strings.Contains(stage.ID, "\x1b") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the durable record lost the exact proposed stage id: %#v", snapshot.Attempts[0].Stages)
	}
	if !strings.Contains(strings.Join(snapshot.Attempts[0].Errors, " "), "\x1b") {
		t.Fatalf("the durable record lost the exact refusal reason: %#v", snapshot.Attempts[0].Errors)
	}

	// And JSON stays valid, escaped by encoding/json as it always was.
	var encoded bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-hostile", "--config", configPath},
		planOverrides(t, 41), &encoded); err != nil {
		t.Fatal(err)
	}
	var view runtime.PlanAttemptsView
	if err := json.Unmarshal(encoded.Bytes(), &view); err != nil {
		t.Fatalf("the JSON surface is not valid JSON: %v\n%s", err, encoded.String())
	}
	if strings.Contains(encoded.String(), "\x1b") {
		t.Fatalf("the JSON surface emitted a raw escape:\n%q", encoded.String())
	}
	if !strings.Contains(view.Attempts[0].Stages[0].ID, "\x1b") {
		t.Fatal("the JSON surface lost the exact proposed stage id")
	}
}

// terminalSafe leaves ordinary text alone, including non-ASCII: escaping past
// the control range would make honest text unreadable to protect against
// nothing.
func TestTerminalSafeLeavesOrdinaryTextAlone(t *testing.T) {
	for _, ordinary := range []string{
		"harden-runtime-transitions", "réviseur indépendant", "独立した検証", "a/b_c.d-e:f", "",
	} {
		if got := terminalSafe(ordinary); got != ordinary {
			t.Fatalf("terminalSafe(%q) = %q", ordinary, got)
		}
	}
	if got := terminalSafe("a\tb"); got != `a\x09b` {
		t.Fatalf("a tab was not escaped: %q", got)
	}
}
