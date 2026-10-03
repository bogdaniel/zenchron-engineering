package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	"github.com/bogdaniel/zenchron-engineering/schemas"
)

func fixture(t *testing.T, populated bool) *API {
	t.Helper()
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 0).UTC()
	if populated {
		run := rt.EngineeringRun{SchemaVersion: rt.SchemaVersion, ID: "r", Repository: "example/repo", Goal: "SECRET_PRIVATE_GOAL", Reason: "SECRET_DIAGNOSTIC", Phase: rt.Execute, Disposition: rt.Active, ControllerSHA256: strings.Repeat("4", 64), CreatedAt: at, UpdatedAt: at}
		if err := store.PutRun(run); err != nil {
			t.Fatal(err)
		}
		op := rt.RunOperation{SchemaVersion: rt.SchemaVersion, ID: "op", RunID: "r", Kind: "provider", IdempotencyKey: "key", State: rt.Running, Attempt: 1, MaxAttempts: 2, StartedAt: &at, LastProgressAt: &at}
		payload, _ := json.Marshal(op)
		for _, e := range []rt.EngineeringEvent{
			{ID: "created", Type: rt.EventRunCreated},
			{ID: "operation", Type: rt.EventOperationPlanned, OperationID: "op", Payload: payload},
			{ID: "secret", Type: rt.EventRunWaiting, Payload: json.RawMessage(`{"reason":"SECRET_PAYLOAD"}`)},
		} {
			e.SchemaVersion = rt.SchemaVersion
			e.RunID = "r"
			e.OccurredAt = at
			if _, err := store.AppendEvent(e); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := store.PutOperation(op, 0); err != nil {
			t.Fatal(err)
		}
		plan := domain.EngineeringPlan{SchemaVersion: domain.SchemaVersion, ID: "p", Revision: 1, Objective: "SECRET_PLAN_OBJECTIVE", Subject: domain.Subject{Repository: "example/repo"}}
		plan.Digest, err = plan.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.ClaimPlan(plan, at); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "private-transcript"), []byte("SECRET_TRANSCRIPT"), 0600); err != nil {
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
	t.Cleanup(func() { reader.Close() })
	return &API{Store: reader, Token: "test-token", ControllerRoot: filepath.Join(dir, "controller"), Now: func() time.Time { return at }, Observe: func() (rt.LiveControllerSnapshot, error) {
		return rt.LiveControllerSnapshot{}, fmt.Errorf("SECRET_OBSERVATION")
	}}
}
func wire(t *testing.T, a *API, path, method, token, schema string, status int) []byte {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	out := httptest.NewRecorder()
	a.Handler().ServeHTTP(out, req)
	if out.Code != status {
		t.Fatalf("%s: %d: %s", path, out.Code, out.Body.String())
	}
	var value any
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("control-plane-"+schema, value); err != nil {
		t.Fatalf("%s: %v: %s", path, err, out.Body.String())
	}
	if bytes.Contains(out.Body.Bytes(), []byte("SECRET")) {
		t.Fatalf("secret escaped: %s", out.Body.String())
	}
	return out.Body.Bytes()
}
func TestHTTPWireSchemasAndSecretBoundary(t *testing.T) {
	t.Setenv("PROVIDER_API_KEY", "SECRET_ENVIRONMENT")
	a := fixture(t, true)
	for _, tc := range []struct{ path, schema string }{
		{"/v1/controller", "controller"}, {"/v1/runs", "runs"}, {"/v1/runs/r", "run"}, {"/v1/runs/r/events?limit=2", "run-events"}, {"/v1/plans/p", "plan"}, {"/v1/plans/p/detail", "plan-detail"}, {"/v1/plans/p/detail?revision=1", "plan-detail"},
	} {
		first := wire(t, a, tc.path, "GET", a.Token, tc.schema, 200)
		second := wire(t, a, tc.path, "GET", a.Token, tc.schema, 200)
		if !bytes.Equal(first, second) {
			t.Fatal("nondeterministic projection")
		}
	}
	detail := wire(t, a, "/v1/runs/r", "GET", a.Token, "run", 200)
	if !bytes.Contains(detail, []byte(`"progress_source":"row"`)) {
		t.Fatalf("lost live progress semantics: %s", detail)
	}
	for _, path := range []string{"/v1/runs", "/v1/controller", "/v1/runs/r", "/v1/runs/r/events", "/v1/plans/p", "/v1/plans/p/detail"} {
		wire(t, a, path, "GET", "", "error", 401)
		wire(t, a, path, "GET", "wrong", "error", 401)
	}
	for _, path := range []string{"/v1/runs?limit=501", "/v1/runs/r/events?after=-1", "/v1/runs/r/events?limit=0", "/v1/runs/r/events?limit=1&limit=2"} {
		wire(t, a, path, "GET", a.Token, "error", 400)
	}
	wire(t, a, "/missing", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/runs/missing", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/plans/missing", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/plans/missing/detail", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/plans/p/detail?revision=9", "GET", a.Token, "error", 404)
	wire(t, a, "/v1/plans/p/detail?revision=-1", "GET", a.Token, "error", 400)
	wire(t, a, "/v1/runs", "POST", a.Token, "error", 405)
	wire(t, a, "/v1/runs/r/events?after=999", "GET", a.Token, "run-events", 200)
	a.Store.Close()
	wire(t, a, "/v1/runs", "GET", a.Token, "error", 500)
}
func TestEmptyFleetWireIsArray(t *testing.T) {
	a := fixture(t, false)
	b := wire(t, a, "/v1/runs", "GET", a.Token, "runs", 200)
	if !bytes.Contains(b, []byte(`"runs":[]`)) {
		t.Fatal(string(b))
	}
}
func TestControllerGenerationMismatchProjection(t *testing.T) {
	durable := rt.ControllerBuild{Version: "one"}
	s := rt.ControllerStatus{DurableConsistency: rt.DurableViolation, Serving: rt.Serving, Durable: rt.DurableActive{Generation: &durable}, Live: rt.LiveObservation{Reachable: true, Snapshot: &rt.LiveControllerSnapshot{Identity: rt.ControllerSelfRecord{Build: rt.ControllerBuild{Version: "two"}}, Role: rt.RoleHeld, WorkAdmission: rt.AdmissionOpen}}}
	out := controllerProjection(s)
	if out.GenerationMatch != "mismatch" || out.DurableConsistency != rt.DurableViolation || out.Serving != rt.Serving || out.Role != rt.RoleHeld {
		t.Fatal(out)
	}
	s.DurableConsistency = rt.DurableUnstable
	if controllerProjection(s).GenerationMatch != "unknown" {
		t.Fatal("torn observation claimed a generation")
	}
}
func TestTokenAndLoopbackBoundary(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	token, err := LoadToken(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadToken(dir)
	if err != nil || token != again || len(token) != 64 {
		t.Fatal("token not persisted", err)
	}
	path := filepath.Join(dir, TokenFile)
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("insecure token", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(dir); err == nil {
		t.Fatal("accepted public token")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadToken(dir); err == nil {
		t.Fatal("accepted token symlink")
	}
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "192.0.2.1:0", "localhost:0"} {
		if l, err := Listen(address); err == nil {
			l.Close()
			t.Fatal("accepted non-loopback literal", address)
		}
	}
	// Address validation is tested without opening sockets in the offline sandbox.
}

func TestEventHTTPPagesChainRunSequence(t *testing.T) {
	a := fixture(t, true)
	var cursor int64
	for i := int64(1); i <= 3; i++ {
		b := wire(t, a, fmt.Sprintf("/v1/runs/r/events?after=%d&limit=1", cursor), "GET", a.Token, "run-events", 200)
		var page Events
		if err := json.Unmarshal(b, &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Events) != 1 || page.Events[0].Sequence != i || page.Next != i || page.HasMore != (i < 3) {
			t.Fatalf("invalid page: %+v", page)
		}
		cursor = page.Next
	}
	wire(t, a, "/v1//runs", "GET", a.Token, "error", 404)
}
