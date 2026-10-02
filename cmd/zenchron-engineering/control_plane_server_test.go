package main

// Acceptance for #395: every endpoint reuses an existing runtime projection
// rather than re-implementing one, the read boundary refuses an unauthorized
// request, pagination is bounded from the first release, a filesystem-enforced
// read-only database fixture proves no write access is required, a controller
// generation mismatch is represented explicitly rather than merged into one
// boolean, and nothing from the process's own environment leaks into a
// response.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func newControlPlaneFixture(t *testing.T) (string, *runtime.SQLiteOperationStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := runtime.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return dir, store
}

func startControlPlane(t *testing.T, server *controlPlaneServer) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(server.handler())
	t.Cleanup(ts.Close)
	return ts
}

func doControlPlaneGet(t *testing.T, ts *httptest.Server, path, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// THERE IS NO EXEMPT ROUTE. A missing token, a wrong one, and the real one are
// three different outcomes, and only the last admits the request.
func TestControlPlaneRefusesUnauthorizedRequests(t *testing.T) {
	dir, store := newControlPlaneFixture(t)
	server := newControlPlaneServer(store, dir, 2, "the-real-token")
	ts := startControlPlane(t, server)

	if status, body := doControlPlaneGet(t, ts, "/v1/runs", ""); status != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401: %s", status, body)
	}
	if status, body := doControlPlaneGet(t, ts, "/v1/runs", "not-the-token"); status != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401: %s", status, body)
	}
	if status, body := doControlPlaneGet(t, ts, "/v1/runs", "the-real-token"); status != http.StatusOK {
		t.Fatalf("correct token: status = %d, want 200: %s", status, body)
	}
}

// THE FLEET ENDPOINT IS runtime.FleetStatus, not a second opinion about it. A
// deterministic fixture read twice produces byte-identical projections, and a
// run's detail view is exactly the entry FleetStatus already computed for it.
func TestControlPlaneRunsAndRunDetailReuseFleetStatus(t *testing.T) {
	dir, store := newControlPlaneFixture(t)
	now := time.Unix(1700000000, 0).UTC()
	seedRun(t, store, "run-a", "fixture goal a", now.Add(-time.Hour))
	seedRun(t, store, "run-b", "fixture goal b", now.Add(-time.Minute))

	server := newControlPlaneServer(store, dir, 2, "tok")
	server.now = func() time.Time { return now }
	ts := startControlPlane(t, server)

	want, err := runtime.FleetStatus(store, dir, 2, now)
	if err != nil {
		t.Fatal(err)
	}

	status, body := doControlPlaneGet(t, ts, "/v1/runs", "tok")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var got runtime.Fleet
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	// An empty omitempty slice has no wire representation to distinguish it
	// from an absent one, so a round trip through JSON turns it nil: that is a
	// property of the encoding, not a divergence in what FleetStatus computed.
	if len(want.Plans) == 0 {
		want.Plans = nil
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("GET /v1/runs diverged from runtime.FleetStatus:\nwant %+v\ngot  %+v", want, got)
	}

	// The same durable state answers identically the second time: the
	// projection is a pure function of the store and the clock, not of
	// anything accumulated between requests.
	status2, body2 := doControlPlaneGet(t, ts, "/v1/runs", "tok")
	if status2 != http.StatusOK || string(body2) != string(body) {
		t.Fatalf("an unchanged fixture produced a different projection on a second read: %s", body2)
	}

	var wantRun runtime.RunSummary
	for _, run := range want.Runs {
		if run.RunID == "run-a" {
			wantRun = run
		}
	}
	if wantRun.RunID == "" {
		t.Fatalf("HARNESS PRECONDITION: run-a is not in the fleet: %+v", want)
	}
	status3, body3 := doControlPlaneGet(t, ts, "/v1/runs/run-a", "tok")
	if status3 != http.StatusOK {
		t.Fatalf("run detail status = %d, body = %s", status3, body3)
	}
	var gotRun runtime.RunSummary
	if err := json.Unmarshal(body3, &gotRun); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(wantRun, gotRun) {
		t.Fatalf("run detail is not the fleet's own entry for the run:\nwant %+v\ngot  %+v", wantRun, gotRun)
	}

	if status4, body4 := doControlPlaneGet(t, ts, "/v1/runs/no-such-run", "tok"); status4 != http.StatusNotFound {
		t.Fatalf("unknown run: status = %d, want 404: %s", status4, body4)
	}
}

// RUN EVENTS ARE A PAGE, NEVER THE UNBOUNDED STREAM, from the first release:
// limit is always populated and clamped, truncated says whether more exist,
// and next_after chains to exactly where the next page should start.
func TestControlPlaneRunEventsIsPaginatedAndBounded(t *testing.T) {
	dir, store := newControlPlaneFixture(t)
	seedRun(t, store, "run-events", "fixture goal", time.Now()) // sequence 1: run.created
	for i := 0; i < 3; i++ {
		if _, err := store.AppendEvent(runtime.EngineeringEvent{
			SchemaVersion: runtime.SchemaVersion,
			ID:            fmt.Sprintf("run-events-extra-%d", i),
			RunID:         "run-events",
			Type:          runtime.EventRunCreated,
		}); err != nil {
			t.Fatalf("HARNESS PRECONDITION: could not seed event %d: %v", i, err)
		}
	}
	// Four events now exist, sequence 1..4.

	server := newControlPlaneServer(store, dir, 1, "tok")
	ts := startControlPlane(t, server)

	status, body := doControlPlaneGet(t, ts, "/v1/runs/run-events/events?limit=2", "tok")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var page controlPlaneRunEvents
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.Limit != 2 {
		t.Fatalf("limit = %d, want 2", page.Limit)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(page.Events))
	}
	if !page.Truncated {
		t.Fatal("a page with more events behind it did not report truncated")
	}
	if page.NextAfter == nil || *page.NextAfter != page.Events[1].Sequence {
		t.Fatalf("next_after = %v, want %d", page.NextAfter, page.Events[1].Sequence)
	}

	status2, body2 := doControlPlaneGet(t, ts, fmt.Sprintf("/v1/runs/run-events/events?after=%d&limit=2", *page.NextAfter), "tok")
	if status2 != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status2, body2)
	}
	var page2 controlPlaneRunEvents
	if err := json.Unmarshal(body2, &page2); err != nil {
		t.Fatal(err)
	}
	if len(page2.Events) != 2 {
		t.Fatalf("remaining events = %d, want 2", len(page2.Events))
	}
	if page2.Truncated {
		t.Fatal("the last page reported truncated")
	}

	status3, body3 := doControlPlaneGet(t, ts, fmt.Sprintf("/v1/runs/run-events/events?limit=%d", controlPlaneMaxEventLimit+500), "tok")
	if status3 != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status3, body3)
	}
	var page3 controlPlaneRunEvents
	if err := json.Unmarshal(body3, &page3); err != nil {
		t.Fatal(err)
	}
	if page3.Limit != controlPlaneMaxEventLimit {
		t.Fatalf("limit = %d, want the clamp %d", page3.Limit, controlPlaneMaxEventLimit)
	}

	if status4, _ := doControlPlaneGet(t, ts, "/v1/runs/run-events/events?after=-1", "tok"); status4 != http.StatusBadRequest {
		t.Fatalf("negative after: status = %d, want 400", status4)
	}
	if status5, _ := doControlPlaneGet(t, ts, "/v1/runs/run-events/events?limit=0", "tok"); status5 != http.StatusBadRequest {
		t.Fatalf("zero limit: status = %d, want 400", status5)
	}
	if status6, _ := doControlPlaneGet(t, ts, "/v1/runs/no-such-run/events", "tok"); status6 != http.StatusNotFound {
		t.Fatalf("unknown run: status = %d, want 404", status6)
	}
}

// THE CONTROLLER ENDPOINT IS runtime.DescribeControllerStatus, and a
// generation mismatch is a DISTINCT, explicit verdict - DurableViolation -
// never folded into a plain healthy/unhealthy boolean.
func TestControlPlaneControllerReusesDescribeControllerStatusAndRepresentsGenerationMismatch(t *testing.T) {
	dir, store := newControlPlaneFixture(t)

	g1 := runtime.ControllerBuild{Version: "g1"}
	g2 := runtime.ControllerBuild{Version: "g2"}
	handoff := runtime.ControllerHandoff{
		ID:            "handoff-g1-g2",
		Predecessor:   runtime.HandoffParty{Binding: runtime.ControllerBinding{Controller: "c1", Build: &g1}},
		Successor:     runtime.HandoffParty{Binding: runtime.ControllerBinding{Controller: "c2", Build: &g2}},
		Phase:         runtime.HandoffRevalidated,
		RecoveryOwner: "c2",
		UpdatedAt:     time.Unix(1700000000, 0).UTC(),
	}
	if ok, err := store.PutControllerHandoff(handoff, ""); err != nil || !ok {
		t.Fatalf("HARNESS PRECONDITION: could not seed the handoff: ok=%v err=%v", ok, err)
	}
	activated := handoff
	activated.Phase = runtime.HandoffActivated
	if ok, err := store.ActivateControllerHandoff(activated, runtime.HandoffRevalidated); err != nil || !ok {
		t.Fatalf("HARNESS PRECONDITION: could not activate the handoff: ok=%v err=%v", ok, err)
	}
	// Durable authority is now generation g2.

	// A LIVE PROCESS OF GENERATION g1 - the drained predecessor - admitting
	// work and holding the role anyway: overlapping liveness during an upgrade
	// is ordinary, overlapping AUTHORITY is exactly what classify() convicts.
	live := runtime.LiveControllerSnapshot{
		Identity:      runtime.ControllerSelfRecord{Build: g1},
		Role:          runtime.RoleHeld,
		WorkAdmission: runtime.AdmissionOpen,
	}
	observe := func() (runtime.LiveControllerSnapshot, error) { return live, nil }

	server := newControlPlaneServer(store, dir, 1, "tok")
	now := time.Unix(1700000100, 0).UTC()
	server.now = func() time.Time { return now }
	server.observeLiveController = observe

	want, err := runtime.DescribeControllerStatus(store, server.controllerRoot, observe, now)
	if err != nil {
		t.Fatal(err)
	}
	if want.DurableConsistency != runtime.DurableViolation {
		t.Fatalf("HARNESS PRECONDITION: the fixture did not produce a violation: %+v", want)
	}

	ts := startControlPlane(t, server)
	status, body := doControlPlaneGet(t, ts, "/v1/controller", "tok")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var got runtime.ControllerStatus
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("GET /v1/controller diverged from runtime.DescribeControllerStatus:\nwant %+v\ngot  %+v", want, got)
	}
	if got.DurableConsistency != runtime.DurableViolation {
		t.Fatalf("the generation mismatch is not represented explicitly: durable_consistency = %q", got.DurableConsistency)
	}
}

// A READ-ONLY FIXTURE STILL ANSWERS EVERY READ. The store behind the handler
// was opened by OpenSQLiteOperationStoreReadOnly against a database file that
// is ALSO unwritable at the OS level, so a control plane that secretly needed
// write access would fail here outright rather than merely decline to
// exercise it.
func TestControlPlaneServesReadsFromAReadOnlyDatabaseFixture(t *testing.T) {
	dir := t.TempDir()
	writable, err := runtime.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	seedRun(t, writable, "run-ro", "fixture goal", time.Now())
	if err := writable.Close(); err != nil {
		t.Fatal(err)
	}
	// The database FILE is made unwritable at the OS level too - belt and
	// suspenders beside the mode=ro connection string - without touching the
	// directory: the directory stays writable, exactly as it is in a real
	// state directory a supervisor still owns, so this isolates the one claim
	// under test, that the CONNECTION never issues a write, from the
	// unrelated question of whether a wal/shm sidecar can be created.
	dbPath := filepath.Join(dir, "runtime.db")
	if err := os.Chmod(dbPath, 0o400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dbPath, 0o600) })

	readOnly, err := runtime.OpenSQLiteOperationStoreReadOnly(dir)
	if err != nil {
		t.Fatalf("a read-only connection could not open the fixture: %v", err)
	}
	t.Cleanup(func() { _ = readOnly.Close() })

	// The connection itself refuses to mutate - this is the property the
	// whole control plane is built on, proven directly rather than inferred
	// from handlers never calling a write method.
	if err := readOnly.PutRun(runtime.EngineeringRun{
		SchemaVersion: runtime.SchemaVersion, ID: "run-should-not-write", Repository: "r",
		Phase: runtime.Execute, Disposition: runtime.Active, CreatedAt: time.Now(),
	}); err == nil {
		t.Fatal("a store opened read-only accepted a write")
	}

	server := newControlPlaneServer(readOnly, dir, 1, "tok")
	ts := startControlPlane(t, server)
	status, body := doControlPlaneGet(t, ts, "/v1/runs", "tok")
	if status != http.StatusOK {
		t.Fatalf("status = %d, body = %s", status, body)
	}
	var fleet runtime.Fleet
	if err := json.Unmarshal(body, &fleet); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, run := range fleet.Runs {
		if run.RunID == "run-ro" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the control plane did not read the seeded run through the unwritable fixture: %+v", fleet)
	}
}

// NOTHING FROM THE PROCESS'S OWN ENVIRONMENT REACHES A RESPONSE. Every
// handler answers from a typed runtime projection it was handed at
// construction, never from os.Environ or a raw configuration object, and this
// plants fixture-secret-shaped values exactly where a provider credential
// would live to prove it.
func TestControlPlaneDoesNotLeakProcessEnvironmentIntoAnyResponse(t *testing.T) {
	const secret = "sk-control-plane-fixture-secret-9c3"
	t.Setenv("OPENAI_API_KEY", secret)
	t.Setenv("ANTHROPIC_API_KEY", secret)
	t.Setenv("GITHUB_TOKEN", secret)

	dir, store := newControlPlaneFixture(t)
	now := time.Unix(1700000000, 0).UTC()
	seedRun(t, store, "run-secret", "fixture goal", now)

	plan := domain.EngineeringPlan{
		SchemaVersion: domain.SchemaVersion, ID: "plan-secret", Revision: 1,
		Objective: "fixture objective",
		Subject:   domain.Subject{Repository: "acme/repo", Revision: "rev-a"},
	}
	digest, err := plan.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	plan.Digest = digest
	if ok, err := store.ClaimPlan(plan, now); err != nil || !ok {
		t.Fatalf("HARNESS PRECONDITION: could not seed the plan: ok=%v err=%v", ok, err)
	}

	server := newControlPlaneServer(store, dir, 1, "tok")
	server.now = func() time.Time { return now }
	ts := startControlPlane(t, server)

	for _, path := range []string{
		"/v1/controller", "/v1/runs", "/v1/runs/run-secret",
		"/v1/runs/run-secret/events", "/v1/plans/plan-secret",
	} {
		_, body := doControlPlaneGet(t, ts, path, "tok")
		if strings.Contains(string(body), secret) {
			t.Fatalf("%s leaked the fixture secret:\n%s", path, body)
		}
	}
}
