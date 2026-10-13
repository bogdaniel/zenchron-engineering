package controlplane

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestObjectiveWorkspaceReconstructsDurablePlanWithoutInventingExecution(t *testing.T) {
	w := webFrom(fixture(t, true))
	list := getWeb(t, w, "/objectives", w.Token, 200)
	if !bytes.Contains(list, []byte(`href="/objectives/p"`)) {
		t.Fatal("durable objective is unreachable from intake")
	}
	getWeb(t, w, "/objectives/p", "", 401)
	getWeb(t, w, "/objectives/missing", w.Token, 404)
	getWeb(t, w, "/objectives/p?revision=-1", w.Token, 404)
	for range 2 {
		body := getWeb(t, w, "/objectives/p", w.Token, 200)
		for _, text := range []string{"example/repo", "/plans/p?revision=1", "No objective WorkGraph binding is recorded", "Actions unavailable", "Changes and PR"} {
			if !bytes.Contains(body, []byte(text)) {
				t.Fatalf("objective workspace missing %q", text)
			}
		}
		if bytes.Contains(body, []byte("SECRET_PLAN_OBJECTIVE")) {
			t.Fatal("workspace bypassed sanitized plan boundary")
		}
	}
}

func TestObjectiveWorkspaceUsesExplicitGraphBindingAndExposesRevisionMismatch(t *testing.T) {
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := domain.EngineeringPlan{SchemaVersion: domain.SchemaVersion, ID: "objective", Revision: 1, Objective: "private", Subject: domain.Subject{Repository: "acme/repo"}}
	plan.Digest, err = plan.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPlan(plan, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	graph, err := (orchestration.WorkGraphProposal{Name: "bounded-feature", Revision: 1, Units: []orchestration.WorkUnit{{ID: "producer", Purpose: "Implement bounded feature", Role: domain.RoleImplementer, Issue: 41}}}).Compose("acme/repo", "claude", "operator", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	graph.ObjectivePlan = &domain.PlanRef{ID: plan.ID, Revision: 2, Digest: "other-revision"}
	if _, _, err := store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	w := &Web{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller")}
	body := getWeb(t, w, "/objectives/objective", w.Token, 200)
	for _, text := range []string{"Implement bounded feature", "/workgraphs/" + graph.ID, "differs from the execution graph binding"} {
		if !bytes.Contains(body, []byte(text)) {
			t.Fatalf("missing %q", text)
		}
	}
}

func TestDecisionControlsDisappearForIneligibleControllerOrPlanBinding(t *testing.T) {
	f := newActionFixture(t)
	at := time.Unix(100, 0).UTC()
	graph, err := (orchestration.WorkGraphProposal{Name: "decision-eligibility", Revision: 1, Units: []orchestration.WorkUnit{{ID: "build", Purpose: "bounded work", Role: domain.RoleImplementer, Issue: 41}}}).Compose("example/repo", "claude", "operator-1", at)
	if err != nil {
		t.Fatal(err)
	}
	graph.ObjectivePlan = &domain.PlanRef{ID: "p", Revision: 1, Digest: f.digest}
	if _, _, err := f.store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	id, err := orchestration.WorkUnitHoldID(graph.ID, "build")
	if err != nil {
		t.Fatal(err)
	}
	hold := orchestration.WorkUnitHold{SchemaVersion: orchestration.WorkUnitHoldSchemaVersion, ID: id, GraphID: graph.ID, UnitID: "build", Purpose: "approve bounded work", RequestedAt: at, RequestedBy: orchestration.DecisionResolutionAuthority{Actor: "operator-1", AuthorityKind: orchestration.AuthorityKindOperator, Provenance: "local_control_endpoint"}}
	if _, _, err := f.store.PlaceWorkUnitHold(hold); err != nil {
		t.Fatal(err)
	}
	w := webFrom(f.api)
	target := []byte(`data-action="/v1/decisions/` + id + `/resolve"`)
	for _, route := range []string{"/objectives/p", "/decisions"} {
		if !bytes.Contains(getWeb(t, w, route, w.Token, 200), target) {
			t.Fatalf("eligible decision absent at %s", route)
		}
	}
	observe := f.observe
	f.observe = func() (rt.LiveControllerSnapshot, error) {
		live, err := observe()
		live.Role = rt.RoleNotHeld
		return live, err
	}
	for _, route := range []string{"/objectives/p", "/decisions"} {
		body := getWeb(t, w, route, w.Token, 200)
		if bytes.Contains(body, target) || !bytes.Contains(body, []byte("Resolution unavailable")) {
			t.Fatalf("role-less controller offered decision at %s", route)
		}
	}
}

func TestDecisionControlsDisappearForUnverifiedPlanBinding(t *testing.T) {
	f := newActionFixture(t)
	at := time.Unix(100, 0).UTC()
	graph, err := (orchestration.WorkGraphProposal{Name: "unverified-plan", Revision: 1, Units: []orchestration.WorkUnit{{ID: "build", Purpose: "bounded work", Role: domain.RoleImplementer, Issue: 41}}}).Compose("example/repo", "claude", "operator-1", at)
	if err != nil {
		t.Fatal(err)
	}
	graph.ObjectivePlan = &domain.PlanRef{ID: "p", Revision: 1, Digest: strings.Repeat("0", 64)}
	if _, _, err := f.store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	id, err := orchestration.WorkUnitHoldID(graph.ID, "build")
	if err != nil {
		t.Fatal(err)
	}
	hold := orchestration.WorkUnitHold{SchemaVersion: orchestration.WorkUnitHoldSchemaVersion, ID: id, GraphID: graph.ID, UnitID: "build", Purpose: "approve bounded work", RequestedAt: at, RequestedBy: orchestration.DecisionResolutionAuthority{Actor: "operator-1", AuthorityKind: orchestration.AuthorityKindOperator, Provenance: "local_control_endpoint"}}
	if _, _, err := f.store.PlaceWorkUnitHold(hold); err != nil {
		t.Fatal(err)
	}
	w := webFrom(f.api)
	target := []byte(`data-action="/v1/decisions/` + id + `/resolve"`)
	for _, check := range []struct{ route, reason string }{
		{"/objectives/p", "differs from the execution graph binding"},
		{"/decisions", "exact plan binding cannot be verified"},
	} {
		body := getWeb(t, w, check.route, w.Token, 200)
		if bytes.Contains(body, target) || !bytes.Contains(body, []byte(check.reason)) {
			t.Fatalf("unverified plan binding became actionable at %s: %s", check.route, body)
		}
	}
}
