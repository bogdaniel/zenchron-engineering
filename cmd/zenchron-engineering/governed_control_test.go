package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// governs records self as the generation the durable record names, as an
// adoption would, so the control plane's observation matches it.
func governs(t *testing.T, built *composition, self runtime.ControllerSelfRecord) {
	t.Helper()
	build := self.Build
	if _, err := built.store.ReadoptController(runtime.ControllerReadoption{ID: "test", Reason: "test", Binding: runtime.ControllerBinding{Controller: "test", Build: &build}, RecordedAt: time.Now().UTC()}, nil); err != nil {
		t.Fatal(err)
	}
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
	governs(t, built, self)
	api := &controlplane.API{Store: reader, Token: "t", Listen: "127.0.0.1:8787", ControllerRoot: filepath.Join(t.TempDir(), "root"),
		Observe: func() (runtime.LiveControllerSnapshot, error) {
			// No supervisor runs in this test; admission is the one fact it
			// would add to this process's own snapshot.
			snapshot := built.controllerSnapshot(nil)
			snapshot.WorkAdmission = runtime.AdmissionOpen
			return snapshot, nil
		},
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

func planRejections(t *testing.T, built *composition, planID string) int {
	t.Helper()
	events, err := built.store.PlanEvents(planID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == runtime.EventPlanRejected {
			n++
		}
	}
	return n
}

// TestGovernedPlanRejectIsDecidedOnceServerSide is the serve half of a
// governed reject: identity first, then - under the plan lock - the revision
// must still await a decision, so two clients racing (two control-plane
// processes, each with its own lock) append ONE plan.rejected, and a reject
// after an approval appends nothing. The CLI keeps approve-after-reject.
// Mutation: drop the governed RefuseUnlessAwaitingDecision check in
// dispatchControl - the racing pair then appends two rejections.
func TestGovernedPlanRejectIsDecidedOnceServerSide(t *testing.T) {
	stateDir, err := os.MkdirTemp("/private/tmp/z398", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	planID := proposePlan(t, configPath, 41)
	revision, digest, assignments := pendingDecision(t, configPath, planID, 41)

	built, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	role, err := runtime.AcquireControllerRole(built.config.StateDir)
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
	listener, err := runtime.ListenControl(built.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return built.handleControl(context.Background(), driver, func() {}, request)
		})
	}()
	reject := func(d string) runtime.ControlRequest {
		return runtime.ControlRequest{Command: runtime.ControlPlanReject, PlanID: planID, Revision: revision, Digest: d, ExpectedController: binding}
	}
	handle := func(r runtime.ControlRequest) runtime.ControlResponse {
		return built.handleControl(context.Background(), driver, func() {}, r)
	}

	buildKind = runtime.ControllerUnattested
	if r := handle(reject(digest)); r.OK || r.Code != runtime.ControlCodeControllerUnattested {
		t.Fatalf("unattested: %+v", r)
	}
	buildKind = runtime.ControllerPreAdoptionBuild
	if r := handle(reject(strings.Repeat("0", 64))); r.OK || r.Code != runtime.ControlCodePlanRefused {
		t.Fatalf("digest mismatch: %+v", r)
	}
	if n := planRejections(t, built, planID); n != 0 {
		t.Fatalf("a refused governed reject appended %d rejection(s)", n)
	}

	// Two clients over the real socket, racing the same governed reject.
	var wg sync.WaitGroup
	responses := make([]runtime.ControlResponse, 2)
	for i := range responses {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := runtime.SendControl(built.config.StateDir, reject(digest))
			if err != nil {
				t.Error(err)
			}
			responses[i] = r
		}()
	}
	wg.Wait()
	applied, refused := 0, 0
	for _, r := range responses {
		switch {
		case r.OK:
			applied++
		case r.Code == runtime.ControlCodeNotAwaitingDecision:
			refused++
		}
	}
	if n := planRejections(t, built, planID); applied != 1 || refused != 1 || n != 1 {
		t.Fatalf("racing governed rejects: %+v, %d rejection(s)", responses, n)
	}

	// The CLI may still approve what was rejected (delegated, ungoverned)...
	var out strings.Builder
	if _, err := autonomy([]string{"plan", "approve", planID, "--revision", strconv.Itoa(revision),
		"--digest", digest, "--assignments", assignments, "--text", "--config", configPath}, planOverrides(t, 41), &out); err != nil {
		t.Fatalf("approve after reject through the CLI: %v\n%s", err, out.String())
	}
	// ...and a governed reject after that approval appends nothing.
	if r := handle(reject(digest)); r.OK || r.Code != runtime.ControlCodeNotAwaitingDecision {
		t.Fatalf("governed reject after an approval: %+v", r)
	}
	if planRejections(t, built, planID) != 1 {
		t.Fatal("a governed reject after an approval was journalled")
	}
}
