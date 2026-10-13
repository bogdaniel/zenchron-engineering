package controlplane

import (
	"bytes"
	"path/filepath"
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
