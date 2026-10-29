package controlplane

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// marshaled fails the test unless v encodes cleanly, and returns the bytes -
// every projection test below inspects the ACTUAL wire bytes, not the Go
// struct, because a field excluded from JSON by a missing accessor is not
// the same guarantee as a field that was never read from the runtime value
// in the first place.
func marshaled(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertNeverContains(t *testing.T, b []byte, needles ...string) {
	t.Helper()
	for _, needle := range needles {
		if bytes.Contains(b, []byte(needle)) {
			t.Fatalf("boundary leak: %q escaped into %s", needle, b)
		}
	}
}

// TestRunDetailProjectionExcludesPathsAndDiagnosticProse pins the exact leaks
// review 5397796709 on #397 found: a local workspace path, free-form
// execution-diagnostic prose and an artifact path must never reach a
// template or a wire response, even though runtime.StatusReport itself
// carries them.
func TestRunDetailProjectionExcludesPathsAndDiagnosticProse(t *testing.T) {
	report := rt.StatusReport{
		RunID: "r", Repository: "example/repo",
		Worker: rt.WorkerIdentity{Agent: "claude", ProviderKind: "anthropic", Workspace: "/home/operator/.zenchron/SECRET_WORKSPACE_PATH"},
		ExecutionDiagnostic: &rt.ExecutionDiagnostic{
			Stage: "provider_request", Message: "SECRET_DIAGNOSTIC_MESSAGE", Code: "SECRET_CODE",
			ArtifactRef: "/home/operator/.zenchron/SECRET_ARTIFACT_PATH",
		},
		HeldMaterial: &rt.HeldMaterial{Kind: rt.HeldUncommitted, BlockedBy: "run_wall_budget_exhausted", Location: "/home/operator/.zenchron/SECRET_QUARANTINE_PATH", Disposition: rt.HeldDisposition},
	}
	out := runDetailProjection(report)
	if out.Worker == nil || !out.Worker.WorkspaceBound {
		t.Fatalf("expected a bounded presence fact for the workspace, got %+v", out.Worker)
	}
	if out.ExecutionDiagnostic == nil || out.ExecutionDiagnostic.Stage != "provider_request" {
		t.Fatalf("expected the diagnostic's classification preserved, got %+v", out.ExecutionDiagnostic)
	}
	b := marshaled(t, out)
	assertNeverContains(t, b,
		"SECRET_WORKSPACE_PATH", "SECRET_DIAGNOSTIC_MESSAGE", "SECRET_CODE", "SECRET_ARTIFACT_PATH", "SECRET_QUARANTINE_PATH",
		"/home/operator",
	)
}

// TestRunProjectionReplacesRawErrorWithUnavailable pins the runs-list leak:
// RunSummary.Error is raw Go error text, and the list must report a bounded
// fact about the run instead of repeating it.
func TestRunProjectionReplacesRawErrorWithUnavailable(t *testing.T) {
	summary := rt.RunSummary{RunID: "r", Workspace: "/home/operator/.zenchron/SECRET_WORKSPACE_PATH", Error: "SECRET_REPLAY_ERROR: open /home/operator/.zenchron/db: permission denied"}
	out := runProjection(summary)
	if !out.Unavailable {
		t.Fatal("expected Unavailable to report the replay failure")
	}
	assertNeverContains(t, marshaled(t, out), "SECRET_REPLAY_ERROR", "SECRET_WORKSPACE_PATH", "/home/operator")
}

// TestPlanProjectionReplacesRawErrorWithUnavailable mirrors the run-list
// fix for the overview's plan list (overview.html's own former `.Error`
// render, review 5397796709).
func TestPlanProjectionReplacesRawErrorWithUnavailable(t *testing.T) {
	summary := rt.PlanSummary{PlanID: "p", State: "executing", Error: "SECRET_PLAN_REPLAY_ERROR"}
	out := planProjection(summary)
	if !out.Unavailable {
		t.Fatal("expected Unavailable to report the replay failure")
	}
	assertNeverContains(t, marshaled(t, out), "SECRET_PLAN_REPLAY_ERROR")
}

// TestFleetProjectionExcludesControlEndpointPath pins the overview leak:
// runtime.Fleet.ControlEndpoint is a literal local socket path concatenated
// with its mechanism description, and it must never cross into Fleet.
func TestFleetProjectionExcludesControlEndpointPath(t *testing.T) {
	fleet := rt.Fleet{
		Capacity: 2, Executing: 1, Active: 1, SupervisorRunning: true,
		ControlEndpoint: "/home/operator/.zenchron/state/SECRET_CONTROL_SOCKET (unix domain socket, owner-only)",
		Runs:            []rt.RunSummary{{RunID: "r", Disposition: rt.Active}},
		Plans:           []rt.PlanSummary{{PlanID: "p", State: "executing", Error: "SECRET_PLAN_ERROR"}},
	}
	out := fleetProjection(fleet)
	assertNeverContains(t, marshaled(t, out), "SECRET_CONTROL_SOCKET", "/home/operator", "SECRET_PLAN_ERROR")
}

// TestControllerProjectionExcludesLocalPaths pins the controller-status
// leak: ProjectionObservation.Path/Target and DurableActive.ArtifactDir are
// local filesystem paths, never reported by controllerProjection.
func TestControllerProjectionExcludesLocalPaths(t *testing.T) {
	status := rt.ControllerStatus{
		DurableConsistency: rt.DurableConsistent, Serving: rt.Serving,
		Durable: rt.DurableActive{
			Generation:  &rt.ControllerBuild{Kind: "release", Version: "v1", SourceRevision: "abc123"},
			ArtifactDir: "/home/operator/.zenchron/state/SECRET_ARTIFACT_DIR",
		},
		Projection: rt.ProjectionObservation{
			State: rt.ProjectionCurrent, Path: "/home/operator/.zenchron/state/SECRET_PROJECTION_PATH",
			Target: "/home/operator/.zenchron/state/SECRET_PROJECTION_TARGET",
		},
		Live: rt.LiveObservation{Reachable: true, Snapshot: &rt.LiveControllerSnapshot{
			Identity: rt.ControllerSelfRecord{
				Build:          rt.ControllerBuild{Kind: "release", Version: "v1", SourceRevision: "abc123"},
				ExecutablePath: "/home/operator/.zenchron/state/SECRET_EXECUTABLE_PATH",
			},
			Role: rt.RoleHeld, WorkAdmission: rt.AdmissionOpen, ObservedAt: time.Now(),
		}},
	}
	out := controllerProjection(status)
	if out.GenerationMatch != "match" {
		t.Fatalf("expected a matching generation, got %q", out.GenerationMatch)
	}
	assertNeverContains(t, marshaled(t, out),
		"SECRET_ARTIFACT_DIR", "SECRET_PROJECTION_PATH", "SECRET_PROJECTION_TARGET", "SECRET_EXECUTABLE_PATH", "/home/operator",
	)
}

// TestPublicationAuthorityProjectionIsBounded checks the one domain-typed
// member of RunDetail is flattened to its bounded Action fields, never a
// free-form payload.
func TestPublicationAuthorityProjectionIsBounded(t *testing.T) {
	report := rt.StatusReport{
		RunID: "r",
		PublicationAuthority: &rt.PublicationAuthority{
			Status: domain.AuthorityAuthorized,
			Action: domain.Action{Type: "pull_request", Target: "open"},
		},
	}
	out := runDetailProjection(report)
	if out.PublicationAuthority == nil || out.PublicationAuthority.ActionType != "pull_request" || out.PublicationAuthority.ActionTarget != "open" {
		t.Fatalf("expected the bounded action fields, got %+v", out.PublicationAuthority)
	}
}
