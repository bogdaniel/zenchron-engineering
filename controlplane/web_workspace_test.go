package controlplane

import (
	"bytes"
	"testing"
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
