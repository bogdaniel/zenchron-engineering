package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// attestedBuild makes this test binary an attested controller for the test's
// duration, so controllerSelf measures it exactly as `serve` measures itself.
func attestedBuild(t *testing.T) {
	t.Helper()
	kind, rev, tree := buildKind, sourceRevision, sourceTree
	t.Cleanup(func() { buildKind, sourceRevision, sourceTree = kind, rev, tree })
	buildKind, sourceRevision, sourceTree = runtime.ControllerPreAdoptionBuild, strings.Repeat("a", 40), strings.Repeat("b", 40)
}

func cancellations(t *testing.T, stateDir, runID string) int {
	t.Helper()
	n := 0
	for _, e := range journalOf(t, stateDir, runID) {
		if e.Type == runtime.EventRunCancelled {
			n++
		}
	}
	return n
}

// TestGovernedStopProvesTheControllerOnTheSameRequest is #398's server half:
// a request naming a controller binding is executed only by that exact,
// attested generation, inside its controller-role authority, and a completed
// run keeps #443's refusal through the control plane. Mutations: delete the
// binding comparison or the unattested refusal in governedControl, or run the
// verb outside WithAuthority - a run.cancelled then appears where it must not.
func TestGovernedStopProvesTheControllerOnTheSameRequest(t *testing.T) {
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	stateDir := activeRun(t, configPath, dir, "live")
	for _, id := range []string{"other", "unheld", "finishing"} {
		activeRun(t, configPath, dir, id)
	}
	store, err := runtime.OpenSQLiteOperationStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(runtime.EngineeringEvent{SchemaVersion: runtime.SchemaVersion, ID: "finishing-done", RunID: "finishing", Type: runtime.EventRunCompleted}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	built, err := newComposition(autonomyFlags{Config: configPath}, offlineOverrides())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	role, err := runtime.AcquireControllerRole(stateDir)
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
	binding := runtime.ControllerBuildBinding(self.Build)
	if binding == "" {
		t.Fatal("precondition: the attested test build has no binding")
	}
	var sends atomic.Int32
	handle := func(request runtime.ControlRequest) runtime.ControlResponse {
		sends.Add(1)
		return built.handleControl(context.Background(), nil, func() { t.Error("a governed request reached shutdown") }, request)
	}
	stop := func(run, expected string) runtime.ControlResponse {
		return handle(runtime.ControlRequest{Command: runtime.ControlStop, RunID: run, ExpectedController: expected})
	}

	if r := stop("live", strings.Repeat("0", 64)); r.OK || r.Code != runtime.ControlCodeControllerMismatch {
		t.Fatalf("a foreign binding: %+v", r)
	}
	if r := handle(runtime.ControlRequest{Command: runtime.ControlShutdown, ExpectedController: binding}); r.OK || r.Code != runtime.ControlCodeUnsupported {
		t.Fatalf("a governed shutdown: %+v", r)
	}
	buildKind = runtime.ControllerUnattested
	if r := stop("live", binding); r.OK || r.Code != runtime.ControlCodeControllerUnattested {
		t.Fatalf("an unattested controller: %+v", r)
	}
	buildKind = runtime.ControllerPreAdoptionBuild
	if cancellations(t, stateDir, "live") != 0 {
		t.Fatal("a refused governed stop cancelled the run")
	}

	// End to end through the control plane's HTTP boundary, with this
	// controller's handler as the socket.
	reader, err := runtime.OpenReadStore(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	api := &controlplane.API{Store: reader, Token: "t", Listen: "127.0.0.1:8787",
		Send: func(r runtime.ControlRequest) (runtime.ControlResponse, error) { return handle(r), nil }}
	post := func(run string, status int) map[string]any {
		t.Helper()
		req := httptest.NewRequest("POST", "/v1/runs/"+run+"/stop", strings.NewReader(`{"controller_binding":"`+binding+`"}`))
		req.Host = "127.0.0.1:8787"
		req.Header.Set("Authorization", "Bearer t")
		req.Header.Set("Origin", "http://127.0.0.1:8787")
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		api.Handler().ServeHTTP(out, req)
		if out.Code != status {
			t.Fatalf("stop %s: %d, want %d: %s", run, out.Code, status, out.Body.String())
		}
		var v map[string]any
		if err := json.Unmarshal(out.Body.Bytes(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	before := sends.Load()
	if v := post("live", 200); v["outcome"] != "applied" {
		t.Fatalf("governed stop: %v", v)
	}
	if v := post("live", 200); v["outcome"] != "already_applied" || sends.Load() != before+1 || cancellations(t, stateDir, "live") != 1 {
		t.Fatalf("duplicate governed stop: %v sends=%d", v, sends.Load()-before)
	}
	// #443: a completed run is refused, with its typed code, not cancelled.
	if v := post("finishing", 409); v["outcome"] != "refused" || v["code"] != runtime.ControlCodeRunTerminal {
		t.Fatalf("stop on a completed run: %v", v)
	}
	if cancellations(t, stateDir, "finishing") != 0 || runDocument(t, stateDir, "finishing").Disposition == runtime.Cancelled {
		t.Fatal("a governed stop rewrote a completed run")
	}

	// The CLI's request (no binding) is unchanged by any of this.
	if r := stop("other", ""); !r.OK {
		t.Fatalf("an ungoverned stop: %+v", r)
	}

	// A released role refuses the governed verb, and nothing is journalled.
	if err := role.Release(); err != nil {
		t.Fatal(err)
	}
	if r := stop("unheld", binding); r.OK || r.Code != runtime.ControlCodeRoleNotHeld {
		t.Fatalf("a released role: %+v", r)
	}
	if cancellations(t, stateDir, "unheld") != 0 {
		t.Fatal("a stop without the controller role cancelled the run")
	}
}
