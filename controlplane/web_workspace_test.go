package controlplane

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestWorkspaceNavigationUsesAuthenticatedDurableViews(t *testing.T) {
	w := webFrom(fixture(t, true))
	for _, route := range []string{"/objectives", "/plans", "/decisions", "/agents", "/settings"} {
		t.Run(route, func(t *testing.T) {
			getWeb(t, w, route, "", 401)
			getWeb(t, w, route, "wrong-token", 401)
			body := getWeb(t, w, route, w.Token, 200)
			if !bytes.Contains(body, []byte(`href="/overview"`)) {
				t.Fatal("workspace page lacks shared navigation")
			}
			getWeb(t, w, route+"?offset=-1", w.Token, 404)
			getWeb(t, w, route+"?offset=0&offset=100", w.Token, 404)
		})
	}
}

func TestWorkspacePlanListProjectsExistingPlan(t *testing.T) {
	w := webFrom(fixture(t, true))
	body := getWeb(t, w, "/plans", w.Token, 200)
	if !bytes.Contains(body, []byte("/plans/p")) {
		t.Fatal("existing durable plan missing from plan list")
	}
	body = getWeb(t, w, "/plans?offset=100", w.Token, 200)
	if bytes.Contains(body, []byte("/plans/p")) {
		t.Fatal("plan pagination repeats the first page")
	}
}

func TestActionRequiredPaginatesPlanApprovalsSeparately(t *testing.T) {
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := range webPageSize + 1 {
		plan := domain.EngineeringPlan{SchemaVersion: domain.SchemaVersion, ID: fmt.Sprintf("plan-%03d", i), Revision: 1,
			Objective: "reviewable plan", Subject: domain.Subject{Repository: "example/repo"}}
		plan.Digest, err = plan.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimPlan(plan, time.Unix(int64(i+1), 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	w := &Web{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"),
		Observe: func() (rt.LiveControllerSnapshot, error) { return rt.LiveControllerSnapshot{}, nil }}

	first := string(getWeb(t, w, "/decisions", w.Token, 200))
	getWeb(t, w, "/decisions?plans_offset=-1", w.Token, 404)
	if got := strings.Count(first, "Review plan revision"); got != webPageSize {
		t.Fatalf("first page has %d plan approvals, want %d", got, webPageSize)
	}
	if !strings.Contains(first, `href="/decisions?plans_offset=100"`) {
		t.Fatal("first page lacks next-plan link")
	}
	if strings.Contains(first, `href="/decisions?offset=100"`) {
		t.Fatal("plan pager must not use the WorkGraph offset")
	}

	second := string(getWeb(t, w, "/decisions?plans_offset=100", w.Token, 200))
	if !strings.Contains(second, "/plans/plan-100") || strings.Contains(second, "/plans/plan-000") {
		t.Fatal("second page does not show only the remaining plan approval")
	}
	if !strings.Contains(second, `href="/decisions"`) || strings.Contains(second, "Next plans") {
		t.Fatal("second page must link back without offering another page")
	}
}
