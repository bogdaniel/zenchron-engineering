package main

// The operator surface for a base rebinding.
//
// The #119 dogfood refused with "a revision cannot move the plan onto a
// different subject" and showed neither subject anywhere: not in the text, not
// in the JSON, not on the refused attempt. Diagnosing it meant reading the
// validator's source and querying SQLite. These tests drive the real CLI and
// hold both surfaces to answering it from durable state alone.

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// seedRebind proposes one plan at base A and then a revision at base B, through
// the ordinary service path, with the observation that authorizes the move.
func seedRebind(t *testing.T, configPath, dir, planID, baseA, baseB string) {
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
	service := runtime.PlanService{
		Store: store, Clock: runtime.RealClock{}, DefaultAgent: "codex",
		Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 2, MaxProviderInvocations: 8},
	}
	for _, base := range []string{baseA, baseB} {
		if _, err := service.Propose(context.Background(), rebindInput(planID, base, base)); err != nil {
			t.Fatalf("proposing at base %s was refused: %v", short(base), err)
		}
	}
}

// seedRefusedRebind proposes at base A and then attempts base B with NO
// observation behind it, which is the refusal the record has to explain.
func seedRefusedRebind(t *testing.T, configPath, dir, planID, baseA, baseB string) {
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
	service := runtime.PlanService{
		Store: store, Clock: runtime.RealClock{}, DefaultAgent: "codex",
		Envelope: domain.PlanBudgetEnvelope{MaxChildRuns: 4, MaxConcurrency: 2, MaxProviderInvocations: 8},
	}
	if _, err := service.Propose(context.Background(), rebindInput(planID, baseA, baseA)); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Propose(context.Background(), rebindInput(planID, baseB, "")); err == nil {
		t.Fatal("an unobserved base was accepted")
	}
}

func rebindInput(planID, base, observed string) runtime.ProposeInput {
	subject := domain.Subject{Repository: "zenchron/seeded", Revision: base}
	contract := domain.EngineeringWorkContract{
		SchemaVersion: domain.SchemaVersion, ID: "contract-plan", Revision: "1",
		Objective: "Resolve the M2 hardening cohort.", AcceptanceIntent: []string{"The cohort is resolved."},
		Subject: subject,
		Scope: domain.ContractScope{
			Stage: domain.StageObserved, AllowedPaths: []string{"."}, ProhibitedPaths: []string{},
		},
		Facts: []string{}, Invariants: map[string]domain.Requirement{},
		Obligations:         map[string]domain.Requirement{},
		RequiredClaims:      map[string]domain.RequiredClaim{"verification": {EvidenceClass: "automated_test"}},
		Permissions:         []domain.Action{},
		Prohibitions:        []domain.Action{},
		AuthorityConditions: []domain.AuthorityCondition{},
		Provenance: domain.ContractProvenance{
			ProjectModel: domain.ObjectRevision{ID: "project", Revision: "1"},
			Policy:       domain.ObjectRevision{ID: "policy", Revision: "1"}, CompilerVersion: "compiler-v0.1",
		},
	}
	return runtime.ProposeInput{
		PlanID: planID, Objective: contract.Objective, Subject: subject,
		ObservedBase: observed, Repository: "zenchron/seeded", Contract: contract, Issue: 119,
		Model: domain.ProjectModel{
			SchemaVersion: domain.SchemaVersion, ID: "project", Revision: "1", Subject: subject,
		},
		Reasoned: []domain.PlanStage{
			{ID: "harden-runtime", Kind: domain.StageAgent, Role: domain.RoleImplementer,
				Objective: "harden", InvocationMode: domain.InvocationModeMutating},
			{ID: "verify-runtime", Kind: domain.StageAgent, Role: domain.RoleReviewer,
				Objective: "verify", DependsOn: []string{"harden-runtime"},
				InvocationMode: domain.InvocationModeMutating,
				Independence: &domain.IndependenceRequirement{
					Dimension: domain.IndependenceExecutionAgent, DifferentFrom: []string{"harden-runtime"},
				}},
		},
		Reasoning: &domain.PlanReasoningProvenance{
			AgentID: "codex", ProviderKind: "codex_cli", VendorFamily: "openai",
			TrustMode: domain.TrustRequirementOperatorTrusted, Model: "gpt-5",
			InvocationMode:        domain.InvocationModeNonMutatingPlanning,
			WorkspaceDigestBefore: strings.Repeat("d", 64), WorkspaceDigestAfter: strings.Repeat("d", 64),
			WorkspaceUnchanged: true,
		},
	}
}

// G — `plan show --text` states the exact base transition and what it costs.
func TestPlanShowStatesTheBaseTransition(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	baseA, baseB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	seedRebind(t, configPath, dir, "plan-rebound", baseA, baseB)

	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-rebound", "--text", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	printed := out.String()
	for _, want := range []string{
		"base: zenchron/seeded " + short(baseA) + " -> " + short(baseB),
		"newly observed trusted base",
		"work performed against the old base is not evidence for the new one and is redone",
	} {
		if !strings.Contains(printed, want) {
			t.Fatalf("the base transition is not stated (%q missing):\n%s", want, printed)
		}
	}
}

// A plan whose base never moved states the base it IS bound to. The exact base
// decides what every result under the plan is a statement about, and no operator
// surface used to name it at all.
func TestPlanShowStatesTheBaseWhenItDidNotMove(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	baseA := strings.Repeat("a", 40)
	seedRebind(t, configPath, dir, "plan-steady", baseA, baseA)

	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-steady", "--text", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	printed := out.String()
	if !strings.Contains(printed, "base: zenchron/seeded "+short(baseA)) {
		t.Fatalf("the plan does not state the base it is bound to:\n%s", printed)
	}
	if strings.Contains(printed, "->") && strings.Contains(printed, "newly observed trusted base") {
		t.Fatalf("a plan at one base reported a transition:\n%s", printed)
	}
}

// G — the JSON surface carries the same transition, exactly, for a reader that
// is not parsing text.
func TestPlanShowJSONCarriesTheBaseTransition(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	baseA, baseB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	seedRebind(t, configPath, dir, "plan-rebound-json", baseA, baseB)

	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-rebound-json", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	var view struct {
		Plan struct {
			Subject domain.Subject `json:"subject"`
		} `json:"plan"`
		BaseChange *runtime.PlanBaseChange `json:"base_change"`
	}
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatalf("decode the view: %v\n%s", err, out.String())
	}
	if view.BaseChange == nil {
		t.Fatalf("the JSON view carries no base transition:\n%s", out.String())
	}
	// EXACT, not shortened: the text abbreviates for a terminal and the JSON is
	// what a machine reads.
	if view.BaseChange.From != baseA || view.BaseChange.To != baseB {
		t.Fatalf("the JSON transition is not exact: %#v", view.BaseChange)
	}
	if view.Plan.Subject.Revision != baseB {
		t.Fatalf("the document is not bound to the new base: %#v", view.Plan.Subject)
	}
}

// G — a REFUSED rebind names both subjects, which is the question the dogfood's
// refusal could not answer from any surface.
func TestPlanShowStatesBothSubjectsOfARefusedRebind(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	baseA, baseB := strings.Repeat("a", 40), strings.Repeat("b", 40)
	seedRefusedRebind(t, configPath, dir, "plan-refused-rebind", baseA, baseB)

	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-refused-rebind", "--text", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("show: %v\n%s", err, out.String())
	}
	printed := out.String()
	if !strings.Contains(printed, short(baseB)) || !strings.Contains(printed, short(baseA)) {
		t.Fatalf("the refused rebind does not name both subjects:\n%s", printed)
	}
	if !strings.Contains(printed, "it was bound to") {
		t.Fatalf("the refused attempt does not say what it was bound to:\n%s", printed)
	}

	// And in JSON, exactly.
	var jsonOut bytes.Buffer
	if _, err := autonomy([]string{"plan", "show", "plan-refused-rebind", "--config", configPath},
		planOverrides(t, 41), &jsonOut); err != nil {
		t.Fatalf("show: %v\n%s", err, jsonOut.String())
	}
	var view struct {
		State struct {
			Attempts []struct {
				Subject         *domain.Subject `json:"subject"`
				PreviousSubject *domain.Subject `json:"previous_subject"`
			} `json:"attempts"`
		} `json:"state"`
	}
	if err := json.Unmarshal(jsonOut.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v\n%s", err, jsonOut.String())
	}
	if len(view.State.Attempts) == 0 {
		t.Fatalf("no attempt was recorded:\n%s", jsonOut.String())
	}
	attempt := view.State.Attempts[0]
	if attempt.Subject == nil || attempt.PreviousSubject == nil {
		t.Fatalf("the attempt records only one side of the comparison: %#v", attempt)
	}
	if attempt.Subject.Revision != baseB || attempt.PreviousSubject.Revision != baseA {
		t.Fatalf("the attempt's subjects are wrong: %#v and %#v", attempt.Subject, attempt.PreviousSubject)
	}
}
