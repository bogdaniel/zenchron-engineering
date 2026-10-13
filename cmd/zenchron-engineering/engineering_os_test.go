package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// This exercises the actual governed serve handler over its owner-only socket,
// real role lock and journal. No API.Send fake supplies authorization or state.
// The optional browser driver uses exactly this isolated listener and store.
func TestEngineeringOSPlanRejectEndToEnd(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "normal"
		if lostReply {
			name = "reply_lost"
		}
		t.Run(name, func(t *testing.T) { checkEngineeringOSPlanReject(t, lostReply) })
	}
}

func checkEngineeringOSPlanReject(t *testing.T, lostReply bool) {
	t.Helper()
	packageDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot := filepath.Clean(filepath.Join(packageDir, "../.."))
	state, err := os.MkdirTemp("/tmp", "zc-ui")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dir, config := planWorkspaceIn(t, state)
	t.Chdir(dir)
	id := proposePlan(t, config, 41)
	revision, digest, _ := pendingDecision(t, config, id, 41)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	role, err := runtime.AcquireControllerRole(state)
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
	governs(t, built, self)
	binding := runtime.ControllerBuildBinding(self.Build)
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	start := func() *runtime.ControlListener {
		t.Helper()
		listener, err := runtime.ListenControl(state)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			done <- listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
				return built.handleControl(context.Background(), driver, func() { t.Error("unexpected shutdown") }, request)
			})
		}()
		t.Cleanup(func() {
			_ = listener.Close()
			select {
			case err := <-done:
				if err != nil {
					t.Error(err)
				}
			case <-time.After(5 * time.Second):
				t.Error("control listener did not stop")
			}
		})
		return listener
	}
	listener := start()
	reader, err := runtime.OpenReadStore(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	root := filepath.Join(t.TempDir(), "controller-root")
	token := strings.Repeat("c", 64)
	api := &controlplane.API{Store: reader, Token: token, ControllerRoot: root, Observe: observeLiveController(state),
		Send: func(r runtime.ControlRequest) (runtime.ControlResponse, error) { return runtime.SendControl(state, r) }}
	web := &controlplane.Web{Store: reader, Token: token, ControllerRoot: root, Observe: observeLiveController(state)}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api.Handler())
	mux.Handle("/", web.Handler())
	server := httptest.NewUnstartedServer(mux)
	api.Listen = server.Listener.Addr().String()
	server.Start()
	t.Cleanup(server.Close)
	post := func(offeredDigest, offeredBinding string, status int) controlplane.ActionResult {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"revision": revision, "digest": offeredDigest, "controller_binding": offeredBinding})
		req := httptest.NewRequest("POST", "/v1/plans/"+id+"/reject", strings.NewReader(string(body)))
		req.Host = api.Listen
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Origin", "http://"+api.Listen)
		req.Header.Set("Content-Type", "application/json")
		out := httptest.NewRecorder()
		api.Handler().ServeHTTP(out, req)
		if out.Code != status {
			t.Fatalf("reject: %d, want %d: %s", out.Code, status, out.Body.String())
		}
		var result controlplane.ActionResult
		if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if result := post(strings.Repeat("0", 64), binding, 409); result.Code != "digest_mismatch" {
		t.Fatalf("changed subject: %+v", result)
	}
	if result := post(digest, strings.Repeat("0", 64), 409); result.Code != runtime.ControlCodeControllerMismatch {
		t.Fatalf("same-request controller proof: %+v", result)
	}
	buildKind = runtime.ControllerUnattested
	if result := post(digest, binding, 409); result.Code != "controller_not_actionable" {
		t.Fatalf("unattested controller: %+v", result)
	}
	buildKind = runtime.ControllerPreAdoptionBuild
	if err := role.Release(); err != nil {
		t.Fatal(err)
	}
	if result := post(digest, binding, 409); result.Code != "controller_not_actionable" {
		t.Fatalf("released role: %+v", result)
	}
	role, err = runtime.AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	built.role = role
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if result := post(digest, binding, 409); result.Code != "controller_not_actionable" {
		t.Fatalf("disconnected pending decision: %+v", result)
	}
	listener = start()
	if planRejections(t, built, id) != 0 {
		t.Fatal("refusals wrote a decision")
	}
	browser := os.Getenv("ZENCHRON_PLAYWRIGHT_MODULE") != ""
	if lostReply && !browser {
		send := api.Send
		api.Send = func(request runtime.ControlRequest) (runtime.ControlResponse, error) {
			response, err := send(request)
			if err == nil && response.OK {
				return runtime.ControlResponse{}, runtime.ErrControlReplyLost
			}
			return response, err
		}
	}
	tokenFile := filepath.Join(state, "browser-token")
	if err := os.WriteFile(tokenFile, []byte(token), 0600); err != nil {
		t.Fatal(err)
	}
	captureDirectory := os.Getenv("ZENCHRON_LIVE_SCREENSHOTS")
	if captureDirectory == "" {
		captureDirectory = filepath.Join(repositoryRoot, "docs/design/screenshots/batch-01")
	}
	if lostReply {
		captureDirectory = filepath.Join(captureDirectory, "lost-reply")
	}
	runBrowser := func(phase string) {
		t.Helper()
		if !browser {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "node", filepath.Join(repositoryRoot, "scripts/check_live_decision.mjs"))
		cmd.Env = append(os.Environ(), "ZENCHRON_LIVE_URL="+server.URL, "ZENCHRON_LIVE_TOKEN_FILE="+tokenFile,
			"ZENCHRON_LIVE_PLAN="+id, "ZENCHRON_LIVE_PHASE="+phase,
			"ZENCHRON_LIVE_SCREENSHOTS="+captureDirectory)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("browser %s: %v\n%s", phase, err, out)
		} else {
			t.Log(string(out))
		}
	}
	if browser {
		phase := "reject"
		if lostReply {
			phase = "reject-lost-reply"
		}
		runBrowser(phase)
	} else if result := post(digest, binding, 200); result.Outcome != controlplane.OutcomeApplied {
		t.Fatalf("governed rejection: %+v", result)
	}
	if planRejections(t, built, id) != 1 {
		t.Fatal("one rejection must be durably journalled")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopen the reader and controller role/socket against preserved durable
	// state. This is reconnect/reconstruction, not a second runtime.
	server.Close()
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err = runtime.OpenReadStore(state)
	if err != nil {
		t.Fatal(err)
	}
	api.Store, web.Store = reader, reader
	server = httptest.NewUnstartedServer(mux)
	api.Listen = server.Listener.Addr().String()
	server.Start()
	t.Cleanup(server.Close)
	runBrowser("disconnected")
	_ = role.Release()
	built.release()
	built, err = newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	driver, err = built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	role, err = runtime.AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	built.role = role
	_ = start()
	runBrowser("reconnected")
	if result := post(digest, binding, 200); result.Outcome != controlplane.OutcomeAlreadyApplied || planRejections(t, built, id) != 1 {
		t.Fatalf("durable duplicate protection after reconnect: %+v", result)
	}
}
