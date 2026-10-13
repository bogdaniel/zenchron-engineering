package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestBrowserWorkspace runs a real browser against the composition root, SQLite,
// Unix controller socket and HTTP service. Only forge/provider boundaries use
// the same fixtures as the existing controller tests. Opt in on a Chrome host:
// ZENCHRON_BROWSER_E2E=1 go test ./cmd/zenchron-engineering -run '^TestBrowserWorkspace$' -v
func TestBrowserWorkspace(t *testing.T) {
	if os.Getenv("ZENCHRON_BROWSER_E2E") != "1" {
		t.Skip("set ZENCHRON_BROWSER_E2E=1 on a host with Chrome and Node")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	state, err := os.MkdirTemp("/tmp", "zc-browser-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dir, config := planWorkspaceIn(t, state)
	t.Chdir(dir)
	loaded, err := runtime.LoadConfig(config, dir)
	if err != nil {
		t.Fatal(err)
	}
	writePlanningFile(t, loaded.PlanningDir, "profiles/browser-implementer.json", `{"execution_agent":"openai-responses","capabilities":["code_change","repository_analysis","verification"],"trust_requirement":"protected"}`)
	writePlanningFile(t, loaded.PlanningDir, "profiles/browser-implementer-alt.json", `{"execution_agent":"openai-responses","capabilities":["code_change","repository_analysis","verification"],"trust_requirement":"protected"}`)
	activeRun(t, config, dir, "browser-run")
	activeRun(t, config, dir, "browser-live")
	plan := proposePlan(t, config, 41)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	if _, err := built.store.ClaimPlanAttempt("browser-planning-only", "zenchron/seeded", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built.agent.CredentialPath, []byte("unused-browser-boundary-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	forge := built.forge.(*runtime.FakeGitHubAdapter)
	forge.Issues[42] = runtime.GitHubIssue{Number: 42, URL: "https://github.com/zenchron/seeded/issues/42", Title: "Make widget retry idempotent", Body: "Repeated retries should produce the same result.", State: runtime.GitHubOpen, UpdatedAt: time.Unix(1_700_000_001, 0).UTC(), Author: runtime.GitHubActor{Login: "operator", ID: 7}}
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
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	service, err := built.planService()
	if err != nil {
		t.Fatal(err)
	}
	view, err := service.DurableView(plan, 0)
	if err != nil {
		t.Fatal(err)
	}
	objectiveGraph, err := service.BindObjectivePlan(view.Plan, 41, built.agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	objectiveHold, err := driver.PlaceWorkUnitHold(runtime.ControlRequest{GraphID: objectiveGraph, UnitID: "implementation", Operator: "operator-1", Note: "Inspect exact objective context"})
	if err != nil {
		t.Fatal(err)
	}
	graph, err := (orchestration.WorkGraphProposal{Name: "browser-approval-flow", Revision: 1, Units: []orchestration.WorkUnit{
		{ID: "implementation", Purpose: "Implement widget behavior", Role: domain.RoleImplementer, Issue: 41},
		{ID: "integration", Purpose: "Integrate verified widget", Role: domain.RoleIntegrator, Issue: 42, DependsOn: []string{"implementation"}},
		{ID: "parallel-verification", Purpose: "Verify adjacent behavior", Role: domain.RoleTester, Issue: 43},
	}}).Compose("zenchron/seeded", built.agent.ID, "operator-1", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := built.store.AdoptWorkGraphRevision(graph); err != nil {
		t.Fatal(err)
	}
	hold, err := driver.PlaceWorkUnitHold(runtime.ControlRequest{GraphID: graph.ID, UnitID: "implementation", Operator: "operator-1", Note: "Approve bounded widget work"})
	if err != nil {
		t.Fatal(err)
	}
	runBrowserFixture(t, root, built, driver, []string{"ZENCHRON_E2E_PLAN=" + plan, "ZENCHRON_E2E_GRAPH=" + graph.ID, "ZENCHRON_E2E_DECISION=" + hold.ID, "ZENCHRON_E2E_OBJECTIVE_DECISION=" + objectiveHold.ID, "ZENCHRON_E2E_OBJECTIVE_GRAPH=" + objectiveGraph})
}

func runBrowserFixture(t *testing.T, root string, built *composition, driver *runtime.Supervisor, environment []string) {
	t.Helper()
	state := built.config.StateDir
	socket, err := runtime.ListenControl(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = socket.Close() })
	controllerDone := make(chan struct{})
	go func() {
		defer close(controllerDone)
		_ = socket.Serve(func(r runtime.ControlRequest) runtime.ControlResponse {
			return built.handleControl(context.Background(), driver, func() {}, r)
		})
	}()
	reader, err := runtime.OpenReadStore(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	listener, err := controlplane.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	observe := observeLiveController(state)
	api := &controlplane.API{Store: reader, Token: "browser-test-token", ControllerRoot: filepath.Join(state, "controller"), Observe: observe,
		Send: func(r runtime.ControlRequest) (runtime.ControlResponse, error) { return runtime.SendControl(state, r) }, Listen: listener.Addr().String()}
	web := &controlplane.Web{Store: reader, Token: api.Token, ControllerRoot: api.ControllerRoot, Observe: observe}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api.Handler())
	mux.Handle("/", web.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	serverDone := make(chan struct{})
	go func() { defer close(serverDone); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-serverDone; _ = socket.Close(); <-controllerDone })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(root, "scripts", "browser_e2e.mjs"))
	command.Env = append(os.Environ(), "ZENCHRON_E2E_URL=http://"+listener.Addr().String())
	command.Env = append(command.Env, environment...)
	command.Stderr = os.Stderr
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	scan := bufio.NewScanner(output)
	for scan.Scan() {
		line := scan.Text()
		if line == "CONTROL:disconnect" {
			_ = socket.Close()
			<-controllerDone
			continue
		}
		if line == "CONTROL:reconnect" {
			socket, err = runtime.ListenControl(state)
			if err != nil {
				t.Error(err)
				continue
			}
			controllerDone = make(chan struct{})
			go func() {
				defer close(controllerDone)
				_ = socket.Serve(func(r runtime.ControlRequest) runtime.ControlResponse {
					return built.handleControl(context.Background(), driver, func() {}, r)
				})
			}()
			continue
		}
		if line == "CONTROL:server-disconnect" {
			_ = server.Close()
			continue
		}
		if line == "CONTROL:release-role" {
			_ = built.role.Release()
			continue
		}
		t.Log(line)
	}
	if err := scan.Err(); err != nil {
		t.Error(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
}
