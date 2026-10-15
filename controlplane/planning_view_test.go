package controlplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/planning"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

const testProfileDocument = `{
  "execution_agent": "claude",
  "capabilities": ["repository_analysis"],
  "trust_requirement": "operator_trusted"
}`

// newPlanningActionFixture is a store with an attested controller binding and
// an API whose Send runs the real planning.Write/LoadRegistry against a real
// temp directory - the same "fake socket runs the real verb" shape
// actionFixture uses for run/plan actions.
func newPlanningActionFixture(t *testing.T) *API {
	t.Helper()
	dir := t.TempDir()
	planningDir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	at := time.Unix(400, 0).UTC()
	build := rt.ControllerBuild{Kind: rt.ControllerAdopted, Version: "v1", SourceRevision: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40), BinarySHA256: strings.Repeat("c", 64)}
	if _, err := store.ReadoptController(rt.ControllerReadoption{ID: "fixture", Reason: "fixture", Binding: rt.ControllerBinding{Controller: "fixture", Build: &build}, RecordedAt: at}, nil); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	return &API{Store: reader, Token: "test-token", Listen: testListen, ControllerRoot: dir + "/controller", Now: func() time.Time { return at },
		Observe: func() (rt.LiveControllerSnapshot, error) {
			return rt.LiveControllerSnapshot{Identity: rt.ControllerSelfRecord{Build: build}, Role: rt.RoleHeld, WorkAdmission: rt.AdmissionOpen, ObservedAt: at}, nil
		},
		Send: func(request rt.ControlRequest) (rt.ControlResponse, error) {
			switch request.Command {
			case rt.ControlPlanningArtifactPropose:
				if request.PlanningArtifact == nil {
					return rt.ControlResponse{Error: "missing artifact", Code: "invalid_request"}, nil
				}
				payload, err := planning.Write(planningDir, *request.PlanningArtifact)
				if err != nil {
					return rt.ControlResponse{Error: err.Error(), Code: "runtime_refused"}, nil
				}
				return rt.ControlResponse{OK: true, Payload: payload}, nil
			case rt.ControlPlanningReload:
				registry, err := planning.LoadRegistry(planningDir)
				if err != nil {
					return rt.ControlResponse{Error: err.Error(), Code: "runtime_refused"}, nil
				}
				payload, _ := json.Marshal(map[string][]string{"profiles": registry.ProfileIDs()})
				return rt.ControlResponse{OK: true, Payload: payload}, nil
			default:
				return rt.ControlResponse{Error: fmt.Sprintf("unexpected %q", request.Command), Code: "unsupported_command"}, nil
			}
		}}
}

func planningArtifactBody(kind, id, document string) string {
	return fmt.Sprintf(`{"controller_binding":%q,"kind":%q,"id":%q,"document":%s}`, testBinding, kind, id, document)
}

// TestProposePlanningArtifactWritesAndReloadSeesIt is #471/#479's dashboard
// path end to end at the HTTP layer: an operator writes a profile, then
// reloads, and the registry a new plan would compile against reflects it -
// without hand-editing a file or restarting the process.
func TestProposePlanningArtifactWritesAndReloadSeesIt(t *testing.T) {
	a := newPlanningActionFixture(t)
	applied := post(t, a, validRequest("/v1/planning/artifacts", planningArtifactBody("agent_profile", "reviewer", testProfileDocument)), 200)
	if outcome(applied) != "applied" {
		t.Fatalf("propose: outcome %q, body %+v", outcome(applied), applied)
	}
	state, ok := applied["state"].(map[string]any)
	if !ok || state["id"] != "reviewer" {
		t.Fatalf("propose response carries no sealed artifact state: %+v", applied)
	}

	reloaded := post(t, a, validRequest("/v1/planning/reload", `{"controller_binding":"`+testBinding+`"}`), 200)
	if outcome(reloaded) != "applied" {
		t.Fatalf("reload: outcome %q, body %+v", outcome(reloaded), reloaded)
	}
	reloadedState, ok := reloaded["state"].(map[string]any)
	if !ok {
		t.Fatalf("reload response carries no state: %+v", reloaded)
	}
	profiles, ok := reloadedState["profiles"].([]any)
	if !ok || len(profiles) != 1 || profiles[0] != "reviewer" {
		t.Fatalf("reload must see the profile just written: %+v", reloadedState)
	}
}

// TestProposePlanningArtifactValidatesBeforeTheSocket mirrors the
// closed-shape precondition every other mutation enforces.
func TestProposePlanningArtifactValidatesBeforeTheSocket(t *testing.T) {
	a := newPlanningActionFixture(t)
	for name, body := range map[string]string{
		"no kind":     planningArtifactBody("", "reviewer", testProfileDocument),
		"no id":       planningArtifactBody("agent_profile", "", testProfileDocument),
		"no document": `{"controller_binding":"` + testBinding + `","kind":"agent_profile","id":"reviewer"}`,
		"refused doc": planningArtifactBody("agent_profile", "reviewer", `{"unknown_field": true}`),
	} {
		t.Run(name, func(t *testing.T) {
			status := 400
			if name == "refused doc" {
				status = 409
			}
			post(t, a, validRequest("/v1/planning/artifacts", body), status)
		})
	}
}
