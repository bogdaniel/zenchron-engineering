package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// THE EXIT CONTRACT distinguishes severities that a healthy/unhealthy split
// would merge. Zero availability and split-brain authority are not the same
// thing, and automation must be able to tell them apart.
func TestControllerStatusExitCodes(t *testing.T) {
	for _, test := range []struct {
		name   string
		status runtime.ControllerStatus
		want   int
	}{
		{"consistent and serving", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.Serving}, 0},
		{"consistent and not serving", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.NotServing}, 0},
		{"consistent with a drifted pointer", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.NotServing,
			Projection: runtime.ProjectionObservation{State: runtime.ProjectionDrift}}, 0},
		{"nothing observed", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.ServingUnknown}, 0},
		{"no activation recorded", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableUnrecorded, Serving: runtime.ServingUnknown}, 0},
		{"two authorities", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableViolation, Serving: runtime.Serving}, 1},
		{"a torn observation", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableUnstable, Serving: runtime.ServingUnknown}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := statusExitCode(test.status); got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
		})
	}
}

// The rendering says UNKNOWN for what was not observed, rather than leaving a
// blank a reader would take for "none".
func TestControllerStatusRendersUnknownRatherThanBlank(t *testing.T) {
	var out bytes.Buffer
	renderControllerStatus(&out, runtime.ControllerStatus{
		DurableConsistency: runtime.DurableConsistent,
		Serving:            runtime.ServingUnknown,
		Live:               runtime.LiveObservation{Reachable: false, Detail: "no control endpoint is available"},
		Projection:         runtime.ProjectionObservation{State: runtime.ProjectionMissing},
	})
	rendered := out.String()
	for _, want := range []string{"role ownership", "UNKNOWN", "work admission", "serving", "UNKNOWN"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the rendering omits %q:\n%s", want, rendered)
		}
	}
	// A reachable controller that reported neither fact renders the same way.
	out.Reset()
	renderControllerStatus(&out, runtime.ControllerStatus{
		DurableConsistency: runtime.DurableConsistent, Serving: runtime.ServingUnknown,
		Live: runtime.LiveObservation{Reachable: true, Snapshot: &runtime.LiveControllerSnapshot{}},
	})
	if strings.Count(out.String(), "UNKNOWN") < 2 {
		t.Fatalf("unreported facts did not render as unknown:\n%s", out.String())
	}
}

// SERVE INSTALLS THE RECONCILER. Without this the maintenance loop exists,
// the supervisor knows how to drive it, and nothing connects the two - which
// is indistinguishable from not having it, except that it looks maintained.
//
// The binding is also proven to be one-shot here, at the composition level
// where a future refactor would most plausibly call it twice.
func TestServeInstallsTheControllerReconciler(t *testing.T) {
	state := t.TempDir()
	store, err := runtime.OpenSQLiteOperationStore(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	role, err := runtime.AcquireControllerRole(state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = role.Release() })

	built := &composition{
		config: runtime.Config{OperatorConfig: runtime.OperatorConfig{StateDir: state}},
		store:  store, role: role,
	}
	repo, err := runtime.ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	agents, err := runtime.OperatorConfig{
		StateDir:     state,
		DefaultAgent: "codex",
		Agents:       map[string]runtime.AgentConfig{"codex": {Kind: runtime.AgentKindCodexCLI, TrustMode: string(runtime.TrustOperatorTrusted)}},
	}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := runtime.NewSupervisor(runtime.SupervisorDependencies{
		Store: store, Clock: runtime.RealClock{}, Owner: runtime.NewRuntimeOwner(),
		Liveness:          runtime.OwnerLivenessFunc(func(string) bool { return false }),
		MaxConcurrentRuns: 1, PollInterval: time.Minute,
		// The supervisor needs a runtime factory to be constructed at all. It
		// is never called here: this test is about whether serve WIRES the
		// reconciler, not about driving runs.
		Runtime: func(runtime.GitHubRepo, runtime.ResolvedAgent) (*runtime.EngineeringRuntime, error) {
			return nil, fmt.Errorf("no runs are driven by this test")
		},
		Agents:       agents,
		Repositories: []runtime.GitHubRepo{repo},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := built.installControllerReconciler(supervisor, role); err != nil {
		t.Fatalf("serve could not install the reconciler: %v", err)
	}
	// Installed once and only once: a second install is refused rather than
	// silently replacing controller maintenance at runtime.
	if err := built.installControllerReconciler(supervisor, role); err == nil {
		t.Fatal("the reconciler was installed twice")
	}

	// And the supervisor actually drives it: a pass reports one.
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Reconciliation == nil {
		t.Fatal("a supervisor pass reported no reconciliation, so serve's wiring is inert")
	}
}
