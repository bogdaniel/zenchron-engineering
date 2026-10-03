package controlplane

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	"github.com/bogdaniel/zenchron-engineering/schemas"
)

const (
	testListen  = "127.0.0.1:8787"
	testBinding = "b1nd1ng"
)

// actionFixture is a store with an active run "s" (no operations) and plan
// "p" whose revision 1 awaits a decision, a writable handle standing in for
// `serve`, and an API whose socket is a counting fake.
type actionFixture struct {
	api    *API
	store  *rt.SQLiteOperationStore
	digest string
	sends  atomic.Int32
	sent   []rt.ControlRequest
	mu     sync.Mutex
	// reply is what the fake socket does with one request.
	reply func(rt.ControlRequest) (rt.ControlResponse, error)
}

func newActionFixture(t *testing.T) *actionFixture {
	t.Helper()
	dir := t.TempDir()
	store, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	at := time.Unix(100, 0).UTC()
	for _, id := range []string{"s", "s2"} {
		if err := store.PutRun(rt.EngineeringRun{SchemaVersion: rt.SchemaVersion, ID: id, Repository: "example/repo", Phase: rt.Execute, Disposition: rt.Active, CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AppendEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: id + "-created", RunID: id, Type: rt.EventRunCreated, OccurredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	plan := domain.EngineeringPlan{SchemaVersion: domain.SchemaVersion, ID: "p", Revision: 1, Subject: domain.Subject{Repository: "example/repo"}, Stages: []domain.PlanStage{{ID: "build", Kind: domain.StageAgent}}}
	if plan.Digest, err = plan.ContentDigest(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPlan(plan, at); err != nil {
		t.Fatal(err)
	}
	proposed, _ := json.Marshal(rt.PlanProposedPayload{Revision: 1, Digest: plan.Digest, ObjectiveDigest: strings.Repeat("f", 64), Origin: domain.ProposalOrigins()[0], StageCount: 1, AgentStageCount: 1, Budget: rt.PlanBudgetPayload{MaxChildRuns: 1, MaxConcurrency: 1, MaxProviderInvocations: 1}})
	if _, err := store.AppendPlanEvent(rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: "p-proposed", PlanID: "p", Type: rt.EventPlanProposed, OccurredAt: at, Payload: proposed}); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	f := &actionFixture{store: store, digest: plan.Digest}
	f.reply = f.apply
	f.api = &API{Store: reader, Token: "test-token", Listen: testListen, ControllerRoot: dir + "/controller", Now: func() time.Time { return at },
		Observe: func() (rt.LiveControllerSnapshot, error) {
			return rt.LiveControllerSnapshot{}, fmt.Errorf("unobserved")
		},
		Send: func(request rt.ControlRequest) (rt.ControlResponse, error) {
			f.sends.Add(1)
			f.mu.Lock()
			f.sent = append(f.sent, request)
			f.mu.Unlock()
			return f.reply(request)
		}}
	return f
}

// apply is what `serve` does with a request: the real runtime verb against
// the real store.
func (f *actionFixture) apply(request rt.ControlRequest) (rt.ControlResponse, error) {
	var err error
	switch request.Command {
	case rt.ControlStop:
		_, err = rt.CancelRun(f.store, rt.Scheduler{Store: f.store, Clock: rt.RealClock{}, Owner: "test"}, time.Now().UTC(), request.RunID, "operator_stop")
	case rt.ControlPlanReject:
		_, err = rt.PlanService{Store: f.store}.Reject(request.PlanID, request.Revision, request.Digest, request.AssignmentsDigest, "operator-1", request.Note)
	default:
		err = fmt.Errorf("unexpected %q", request.Command)
	}
	if err != nil {
		return rt.ControlResponse{Error: err.Error(), Code: rt.ControlCode(err)}, nil
	}
	return rt.ControlResponse{OK: true}, nil
}

func (f *actionFixture) events(t *testing.T, kind, runID string) int {
	t.Helper()
	var events []rt.EngineeringEvent
	var err error
	if runID != "" {
		events, err = f.store.Events(runID)
	} else {
		events, err = f.store.PlanEvents("p")
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range events {
		if e.Type == kind {
			n++
		}
	}
	return n
}

func validRequest(path, body string) *http.Request {
	r := httptest.NewRequest("POST", path, strings.NewReader(body))
	r.Host = testListen
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Origin", "http://"+testListen)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

// post sends one action and validates the answer against its wire schema.
func post(t *testing.T, a *API, req *http.Request, status int) map[string]any {
	t.Helper()
	out := httptest.NewRecorder()
	a.Handler().ServeHTTP(out, req)
	if out.Code != status {
		t.Fatalf("%s %s: %d, want %d: %s", req.Method, req.URL.Path, out.Code, status, out.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	schema := "control-plane-action-result"
	if _, ok := value["error"]; ok {
		schema = "control-plane-error"
	}
	if err := schemas.Validate(schema, value); err != nil {
		t.Fatalf("%v: %s", err, out.Body.String())
	}
	return value
}

func outcome(v map[string]any) string { s, _ := v["outcome"].(string); return s }
func code(v map[string]any) string    { s, _ := v["code"].(string); return s }

func stopBody() string { return `{"controller_binding":"` + testBinding + `"}` }
func rejectBody(revision int, digest string) string {
	return fmt.Sprintf(`{"controller_binding":%q,"revision":%d,"digest":%q,"note":"no"}`, testBinding, revision, digest)
}

// Every CSRF/rebinding case is refused before the socket: zero sends and an
// unchanged journal. Mutations: accept the cookie for POST, or drop the Host,
// Origin, Sec-Fetch-Site or Content-Type check - one case then reaches the
// fake socket.
func TestActionCSRFBoundaryRefusesBeforeTheSocket(t *testing.T) {
	f := newActionFixture(t)
	for name, tc := range map[string]struct {
		edit   func(*http.Request)
		status int
	}{
		"cookie only": {func(r *http.Request) {
			r.Header.Del("Authorization")
			r.Header.Set("Cookie", webTokenCookie+"=test-token")
		}, 401},
		"foreign origin":  {func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }, 403},
		"no origin":       {func(r *http.Request) { r.Header.Del("Origin") }, 403},
		"rebound host":    {func(r *http.Request) { r.Host = "evil.example:8787" }, 403},
		"text plain":      {func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		"cross-site":      {func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		"wrong bearer":    {func(r *http.Request) { r.Header.Set("Authorization", "Bearer nope") }, 401},
		"no listen bound": {nil, 403},
	} {
		t.Run(name, func(t *testing.T) {
			api := *f.api
			if tc.edit == nil {
				api.Listen = ""
			}
			req := validRequest("/v1/runs/s/stop", stopBody())
			if tc.edit != nil {
				tc.edit(req)
			}
			post(t, &api, req, tc.status)
		})
	}
	if f.sends.Load() != 0 || f.events(t, rt.EventRunCancelled, "s") != 0 {
		t.Fatalf("a refused mutation reached the socket: sends=%d", f.sends.Load())
	}
}

// The routes are a closed list and no input names a command. Mutation: a
// generic POST /v1/{verb} passthrough, or decoding without
// DisallowUnknownFields.
func TestActionRoutesAreAClosedAllowlist(t *testing.T) {
	f := newActionFixture(t)
	for _, path := range []string{"/v1/runs/s/resume", "/v1/runs/s/shutdown", "/v1/runs/s/stop-all", "/v1/runs/s/drain",
		"/v1/plans/p/approve", "/v1/plans/p/revise", "/v1/actions/stop", "/v1/shutdown", "/v1/stop", "/v1/runs/s/stop/"} {
		post(t, f.api, validRequest(path, stopBody()), 404)
	}
	for _, body := range []string{
		`{"controller_binding":"b","command":"shutdown"}`,
		`{"controller_binding":"b","revision":1}`,
		`{"controller_binding":"b"} {"controller_binding":"b"}`,
		`not json`,
	} {
		post(t, f.api, validRequest("/v1/runs/s/stop", body), 400)
	}
	post(t, f.api, validRequest("/v1/plans/p/reject", `{"controller_binding":"b","revision":1,"digest":"d","assignments_digest":"x"}`), 400)
	post(t, f.api, validRequest("/v1/plans/p/reject", `{"controller_binding":"b","digest":"d"}`), 400)
	if f.sends.Load() != 0 {
		t.Fatalf("an unsupported request reached the socket %d time(s)", f.sends.Load())
	}
}

// One stop sends exactly the CLI's request plus the binding, and a duplicate
// is answered from durable state without a second send. Mutations: remove the
// already-applied pre-check (two sends), forward a binding other than the
// body's, or drop the process lock (the concurrent pair sends twice).
func TestStopSendsOnceAndADuplicateIsAlreadyApplied(t *testing.T) {
	f := newActionFixture(t)
	first := post(t, f.api, validRequest("/v1/runs/s/stop", stopBody()), 200)
	if outcome(first) != OutcomeApplied || first["state"].(map[string]any)["disposition"] != "cancelled" {
		t.Fatalf("first stop: %v", first)
	}
	if want := (rt.ControlRequest{Command: rt.ControlStop, RunID: "s", ExpectedController: testBinding}); f.sent[0] != want {
		t.Fatalf("sent %+v, want the CLI's stop plus the observed binding %+v", f.sent[0], want)
	}
	state, _ := json.Marshal(first["state"])
	var decoded any
	_ = json.Unmarshal(state, &decoded)
	if err := schemas.Validate("control-plane-run", decoded); err != nil {
		t.Fatalf("state is not a run projection: %v", err)
	}
	second := post(t, f.api, validRequest("/v1/runs/s/stop", stopBody()), 200)
	if outcome(second) != OutcomeAlreadyApplied || f.sends.Load() != 1 || f.events(t, rt.EventRunCancelled, "s") != 1 {
		t.Fatalf("duplicate stop: %v sends=%d", second, f.sends.Load())
	}

	// Two concurrent submits: the lock makes the second see the first's effect.
	f.reply = func(r rt.ControlRequest) (rt.ControlResponse, error) {
		time.Sleep(50 * time.Millisecond)
		return f.apply(r)
	}
	var wg sync.WaitGroup
	results := make([]map[string]any, 2)
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = post(t, f.api, validRequest("/v1/runs/s2/stop", stopBody()), 200)
		}()
	}
	wg.Wait()
	if f.sends.Load() != 2 || f.events(t, rt.EventRunCancelled, "s2") != 1 {
		t.Fatalf("concurrent duplicate submits sent %d request(s) in total, want 2 (one per run): %v", f.sends.Load(), results)
	}
	post(t, f.api, validRequest("/v1/runs/missing/stop", stopBody()), 404)
}

// Reject binds plan, revision and digest exactly. A stale digest or a
// revision not awaiting a decision is refused without a send; a duplicate is
// already_applied. Mutations: forward the store's digest instead of the
// body's, or remove the digest / awaiting-decision pre-check.
func TestRejectBindsTheExactRevisionAndDigest(t *testing.T) {
	f := newActionFixture(t)
	stale := post(t, f.api, validRequest("/v1/plans/p/reject", rejectBody(1, strings.Repeat("0", 64))), 409)
	if code(stale) != "digest_mismatch" {
		t.Fatalf("stale digest: %v", stale)
	}
	post(t, f.api, validRequest("/v1/plans/p/reject", rejectBody(2, f.digest)), 404)
	if f.sends.Load() != 0 {
		t.Fatal("a stale binding reached the socket")
	}
	applied := post(t, f.api, validRequest("/v1/plans/p/reject", rejectBody(1, f.digest)), 200)
	if outcome(applied) != OutcomeApplied || applied["state"].(map[string]any)["shown"] != "rejected" {
		t.Fatalf("reject: %v", applied)
	}
	want := rt.ControlRequest{Command: rt.ControlPlanReject, PlanID: "p", Revision: 1, Digest: f.digest, Note: "no", ExpectedController: testBinding}
	if f.sent[0] != want {
		t.Fatalf("sent %+v, want %+v", f.sent[0], want)
	}
	again := post(t, f.api, validRequest("/v1/plans/p/reject", rejectBody(1, f.digest)), 200)
	if outcome(again) != OutcomeAlreadyApplied || f.sends.Load() != 1 || f.events(t, rt.EventPlanRejected, "") != 1 {
		t.Fatalf("duplicate reject: %v sends=%d", again, f.sends.Load())
	}
	other := post(t, f.api, validRequest("/v1/plans/p/reject", rejectBody(1, strings.Repeat("1", 64))), 409)
	if code(other) != "not_awaiting_decision" || f.sends.Load() != 1 {
		t.Fatalf("a decided revision: %v", other)
	}
	// The premise of the guard: the plan service itself does not deduplicate.
	if _, err := (rt.PlanService{Store: f.store}).Reject("p", 1, f.digest, "", "operator-1", ""); err != nil {
		t.Fatal(err)
	}
	if f.events(t, rt.EventPlanRejected, "") != 2 {
		t.Fatal("PlanService now deduplicates a second reject; the control plane pre-check may be revisited")
	}
}

// A lost reply is settled from durable state, never by sending again.
// Mutation: retry on error - the fake then sees two sends.
func TestAnAmbiguousReplyIsSettledByReReadWithoutResend(t *testing.T) {
	f := newActionFixture(t)
	f.reply = func(r rt.ControlRequest) (rt.ControlResponse, error) {
		_, _ = f.apply(r)
		return rt.ControlResponse{}, fmt.Errorf("%w: EOF", rt.ErrControlReplyLost)
	}
	settled := post(t, f.api, validRequest("/v1/runs/s/stop", stopBody()), 200)
	if outcome(settled) != OutcomeApplied || f.sends.Load() != 1 {
		t.Fatalf("applied-then-lost: %v sends=%d", settled, f.sends.Load())
	}
	f.reply = func(rt.ControlRequest) (rt.ControlResponse, error) {
		return rt.ControlResponse{}, fmt.Errorf("%w: EOF", rt.ErrControlReplyLost)
	}
	unknown := post(t, f.api, validRequest("/v1/runs/s2/stop", stopBody()), 504)
	if outcome(unknown) != OutcomeUnknown || f.sends.Load() != 2 || f.events(t, rt.EventRunCancelled, "s2") != 0 {
		t.Fatalf("lost and not applied: %v sends=%d", unknown, f.sends.Load())
	}
	f.reply = func(rt.ControlRequest) (rt.ControlResponse, error) {
		return rt.ControlResponse{}, errors.New("dial: no such file")
	}
	down := post(t, f.api, validRequest("/v1/runs/s2/stop", stopBody()), 503)
	if outcome(down) != OutcomeUnreachable || f.sends.Load() != 3 {
		t.Fatalf("unreachable: %v sends=%d", down, f.sends.Load())
	}
}

// A controller refusal is shown with its typed code and the re-read state,
// and an unattested controller is refused here before any send. #443's
// terminal-run refusal is a refusal, not a cancellation.
func TestControllerRefusalsAreTypedAndUnattestedNeverSends(t *testing.T) {
	f := newActionFixture(t)
	unattested := post(t, f.api, validRequest("/v1/runs/s/stop", `{"controller_binding":""}`), 409)
	if code(unattested) != rt.ControlCodeControllerUnattested || f.sends.Load() != 0 {
		t.Fatalf("unattested: %v", unattested)
	}
	for _, refusal := range []string{rt.ControlCodeControllerMismatch, rt.ControlCodeRoleNotHeld, rt.ControlCodeRunTerminal} {
		f.reply = func(rt.ControlRequest) (rt.ControlResponse, error) {
			return rt.ControlResponse{Error: "refused", Code: refusal}, nil
		}
		got := post(t, f.api, validRequest("/v1/runs/s/stop", stopBody()), 409)
		if outcome(got) != OutcomeRefused || code(got) != refusal || got["state"].(map[string]any)["disposition"] != "active" {
			t.Fatalf("%s: %v", refusal, got)
		}
	}
	if f.events(t, rt.EventRunCancelled, "s") != 0 {
		t.Fatal("a refused stop cancelled the run")
	}
}

// The page offers an action only for an attested, serving, role-holding,
// generation-matching controller, and the button carries that binding.
func TestActionsAreDisabledUnlessTheControllerIsProvable(t *testing.T) {
	good := Controller{LiveReachable: true, Serving: rt.Serving, Role: rt.RoleHeld, GenerationMatch: "match", ControllerBinding: strings.Repeat("a", 64), LiveBuild: &Build{Kind: "adopted", Version: "v1"}}
	if why := good.ActionsDisabled(); why != "" {
		t.Fatalf("a provable controller was disabled: %s", why)
	}
	for name, edit := range map[string]func(*Controller){
		"unattested":  func(c *Controller) { c.ControllerBinding = "" },
		"unreachable": func(c *Controller) { c.LiveReachable = false },
		"role":        func(c *Controller) { c.Role = rt.RoleNotHeld },
		"generation":  func(c *Controller) { c.GenerationMatch = "mismatch" },
		"not serving": func(c *Controller) { c.Serving = rt.NotServing },
	} {
		c := good
		edit(&c)
		if c.ActionsDisabled() == "" {
			t.Errorf("%s: actions were offered", name)
		}
	}
	enabled := renderTemplate(t, runDetailTemplate, runDetailData{Status: RunDetail{ID: "r", Disposition: rt.Active}, Controller: good})
	if !strings.Contains(enabled, `data-action="/v1/runs/r/stop"`) || !strings.Contains(enabled, `data-binding="`+good.ControllerBinding+`"`) {
		t.Fatal("an enabled stop does not carry its route and binding")
	}
	disabled := renderTemplate(t, runDetailTemplate, runDetailData{Status: RunDetail{ID: "r", Disposition: rt.Active}, StopDisabled: "the serving controller is unattested"})
	if strings.Contains(disabled, "data-action=") || !strings.Contains(disabled, "stop unavailable: the serving controller is unattested") {
		t.Fatal("a disabled stop rendered a button")
	}
	plan := renderTemplate(t, planDetailTemplate, planDetailData{Plan: PlanDetail{ID: "p", Revision: 2, Digest: "d2", Shown: "proposed"}, Controller: good})
	if !strings.Contains(plan, `data-action="/v1/plans/p/reject"`) || !strings.Contains(plan, `data-revision="2"`) || !strings.Contains(plan, `data-digest="d2"`) {
		t.Fatal("reject does not bind the exact revision and digest")
	}
}

// The request bodies the console sends are the published request schema.
func TestActionRequestSchema(t *testing.T) {
	for body, valid := range map[string]bool{
		stopBody():         true,
		rejectBody(1, "d"): true,
		`{"controller_binding":"b","command":"x"}`:             false,
		`{"controller_binding":"b","revision":0,"digest":"d"}`: false,
		`{}`: false,
	} {
		var v any
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatal(err)
		}
		if err := schemas.Validate("control-plane-action-request", v); (err == nil) != valid {
			t.Errorf("%s: valid=%v err=%v", body, valid, err)
		}
	}
}
