package controlplane

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	"github.com/bogdaniel/zenchron-engineering/schemas"
)

func TestBrowserApprovalBindsAssignmentsAndDoesNotAcceptActor(t *testing.T) {
	f := newActionFixture(t)
	body := `{"controller_binding":"binding","revision":1,"digest":"` + f.digest + `","assignments_digest":"assignments","note":"reviewed"}`
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.Command != rt.ControlPlanApprove || request.AssignmentsDigest != "assignments" || request.ExpectedController != "binding" || request.Operator != "" {
			t.Fatalf("incorrect approval binding: %+v", request)
		}
		return rt.ControlResponse{Code: "plan_refused"}, nil
	}
	post(t, f.api, validRequest("/v1/plans/p/approve", body), 409)
	if f.sends.Load() != 1 {
		t.Fatal("approval must send exactly once")
	}
	post(t, f.api, validRequest("/v1/plans/p/approve", strings.TrimSuffix(body, "}")+`,"operator":"intruder"}`), 400)
	if f.sends.Load() != 1 {
		t.Fatal("caller selected actor reached controller")
	}
}

// #639: a plan held after a configuration change is refused by the controller
// with a reason and a recovery. The browser must receive it, not a bare code.
func TestPlanRefusalExplanationReachesTheOperator(t *testing.T) {
	f := newActionFixture(t)
	reason := "configuration_changed: plan p was proposed under a different controller-effective configuration; " +
		"it is not carried across that boundary - propose the issue again under the current configuration"
	f.reply = func(rt.ControlRequest) (rt.ControlResponse, error) {
		return rt.ControlResponse{Code: rt.ControlCodePlanRefused, Error: reason}, nil
	}
	body := `{"controller_binding":"binding","revision":1,"digest":"` + f.digest + `","assignments_digest":"assignments"}`
	got := post(t, f.api, validRequest("/v1/plans/p/approve", body), 409)
	if outcome(got) != "refused" || got["detail"] != reason {
		t.Fatalf("operator was not told why the plan was refused: %v", got)
	}
}

// The explanation is bounded by the schema AFTER the re-read warning is
// composed in: the explanation shrinks, the warning stays.
func TestLongPlanRefusalKeepsTheRereadWarningWithinTheSchema(t *testing.T) {
	f := newActionFixture(t)
	f.reply = func(rt.ControlRequest) (rt.ControlResponse, error) {
		f.api.Store.Close() // the refusal lands; the control plane can no longer re-read
		return rt.ControlResponse{Code: rt.ControlCodePlanRefused, Error: strings.Repeat("x", 900)}, nil
	}
	body := `{"controller_binding":"binding","revision":1,"digest":"` + f.digest + `","assignments_digest":"assignments"}`
	got := post(t, f.api, validRequest("/v1/plans/p/approve", body), 409) // post validates the schema
	detail, _ := got["detail"].(string)
	if outcome(got) != "refused" || code(got) != rt.ControlCodePlanRefused || !strings.HasSuffix(detail, stateUnread) {
		t.Fatalf("refusal or re-read warning lost: %v", got)
	}
}

func TestBrowserReadsRequireLiveControllerAndUseReadOnlyControl(t *testing.T) {
	f := newActionFixture(t)
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.Command != rt.ControlPlanView || request.ExpectedController != "" {
			t.Fatalf("read should not request mutation authority: %+v", request)
		}
		view, err := f.api.Store.PlanView("p", 1)
		if err != nil {
			t.Fatal(err)
		}
		preview := PlanPreviewProjection(view)
		preview.ControllerBinding = request.ExpectedController
		payload, _ := json.Marshal(preview)
		return rt.ControlResponse{OK: true, Payload: payload}, nil
	}
	req := httptest.NewRequest("GET", "/v1/plans/p/preview", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	out := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatal(out.Body.String())
	}
	var value any
	if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("control-plane-plan-preview", value); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceOptionsRemainReadableFromUnattestedDevelopmentController(t *testing.T) {
	f := newActionFixture(t)
	f.observe = func() (rt.LiveControllerSnapshot, error) {
		return rt.LiveControllerSnapshot{Identity: rt.ControllerSelfRecord{Build: rt.ControllerBuild{Kind: rt.ControllerUnattested}, Unattested: true}, Role: rt.RoleHeld}, nil
	}
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.Command != rt.ControlWorkspaceOptions || request.ExpectedController != "" {
			t.Fatalf("read should not request mutation authority: %+v", request)
		}
		payload, _ := json.Marshal(WorkspaceOptions{Repositories: []string{}, Agents: []WorkspaceAgent{}, Profiles: []string{}, Templates: []string{}, InstructionPacks: []string{}, ContextPolicies: []string{}})
		return rt.ControlResponse{OK: true, Payload: payload}, nil
	}
	req := httptest.NewRequest("GET", "/v1/workspace/options", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	out := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(out, req)
	if out.Code != 200 {
		t.Fatalf("development read returned %d: %s", out.Code, out.Body.String())
	}
}

func TestObjectiveForwardsProfilePreferenceAndDeterministicChoice(t *testing.T) {
	f := newActionFixture(t)
	var received rt.ControlRequest
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		received = request
		return rt.ControlResponse{Code: "plan_refused"}, nil
	}
	body := `{"controller_binding":"binding","repository":"example/repo","issue":41,"profile":"implementer","template":"bounded","deterministic":true}`
	post(t, f.api, validRequest("/v1/objectives", body), 409)
	if received.Command != rt.ControlPlanRevise || received.Profile != "implementer" || received.Template != "bounded" || !received.Deterministic {
		t.Fatalf("objective preferences did not reach the controller: %+v", received)
	}
	if f.sends.Load() != 1 {
		t.Fatalf("planning request sent %d times, want exactly once", f.sends.Load())
	}
}

func TestNewBrowserBodiesRemainClosed(t *testing.T) {
	f := newActionFixture(t)
	for _, path := range []string{"/v1/objectives", "/v1/plans/p/approve", "/v1/plans/p/revise", "/v1/decisions/d/resolve", "/v1/runs/s/authorize", "/v1/runs/s/pause", "/v1/runs/s/unpause"} {
		post(t, f.api, validRequest(path, `{"controller_binding":"binding","operator":"intruder"}`), 400)
	}
	if f.sends.Load() != 0 {
		t.Fatal("invalid action reached controller")
	}
}

func TestWorkspaceContractSchemas(t *testing.T) {
	options := WorkspaceOptions{Repositories: []string{"example/repo"}, Agents: []WorkspaceAgent{}, Profiles: []string{}, Templates: []string{}, InstructionPacks: []string{}, ContextPolicies: []string{}}
	// A nonzero bounded envelope is operator configuration, never a browser grant.
	options.Budget.MaxChildRuns = 1
	options.Budget.MaxConcurrency = 1
	options.Budget.MaxProviderInvocations = 1
	b, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	var value any
	if err := json.Unmarshal(b, &value); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("control-plane-workspace-options", value); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("control-plane-authority-request", nil); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"controller_binding":"b","revision":1,"digest":"d","assignments_digest":"a"}`,
		`{"controller_binding":"b","repository":"example/repo","issue":1}`,
		`{"controller_binding":"b","repository":"example/repo","issue":1,"profile":"implementer","deterministic":true}`,
		`{"controller_binding":"b","request_id":"r","digest":"d","decision":"approve"}`,
		`{"controller_binding":"b","outcome_kind":"allow_deny","outcome_value":"allow"}`,
	} {
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		if err := schemas.Validate("control-plane-action-request", value); err != nil {
			t.Fatal(err)
		}
	}
}

// TestObjectiveIntakeForwardsProductID proves the HTTP decode step for
// Product -> WorkGraph identity (#471/#479 Step 2): a product_id the operator
// chose at intake reaches the ControlRequest the controller acts on, rather
// than being dropped on the way from the browser body to the runtime.
func TestObjectiveIntakeForwardsProductID(t *testing.T) {
	f := newActionFixture(t)
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.Command != rt.ControlPlanRevise || request.ProductID != "storefront" {
			t.Fatalf("product id did not reach the control request: %+v", request)
		}
		return rt.ControlResponse{Code: "plan_refused"}, nil
	}
	body := `{"controller_binding":"binding","repository":"example/repo","issue":1,"product_id":"storefront"}`
	post(t, f.api, validRequest("/v1/objectives", body), 409)
	if f.sends.Load() != 1 {
		t.Fatal("objective intake must send exactly once")
	}
}

func TestPlanningDoesNotBlockStop(t *testing.T) {
	f := newActionFixture(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	f.reply = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		if request.Command == rt.ControlPlanRevise {
			close(entered)
			<-release
			return rt.ControlResponse{Code: "plan_refused"}, nil
		}
		return f.apply(request)
	}
	planned := make(chan struct{})
	go func() {
		defer close(planned)
		req := validRequest("/v1/objectives", `{"controller_binding":"binding","repository":"example/repo","issue":1}`)
		f.api.Handler().ServeHTTP(httptest.NewRecorder(), req)
	}()
	<-entered
	stopped := make(chan int, 1)
	go func() {
		out := httptest.NewRecorder()
		f.api.Handler().ServeHTTP(out, validRequest("/v1/runs/s/stop", stopBody()))
		stopped <- out.Code
	}()
	select {
	case code := <-stopped:
		if code != 200 {
			t.Fatalf("stop: %d", code)
		}
	case <-time.After(3 * time.Second):
		close(release)
		<-planned
		t.Fatal("stop waited behind planning")
	}
	close(release)
	<-planned
}

func TestCanceledMutationNeverReachesController(t *testing.T) {
	f := newActionFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := httptest.NewRecorder()
	f.api.Handler().ServeHTTP(out, validRequest("/v1/runs/s/stop", stopBody()).WithContext(ctx))
	if out.Code != 408 || f.sends.Load() != 0 {
		t.Fatalf("canceled request dispatched: code=%d sends=%d", out.Code, f.sends.Load())
	}
}

func TestApprovalRejectsIgnoredPresentationFields(t *testing.T) {
	f := newActionFixture(t)
	for _, field := range []string{`"objective":"changed"`, `"stage_objectives":{}`, `"stage_rationales":{}`} {
		body := `{"controller_binding":"binding","revision":1,"digest":"` + f.digest + `","assignments_digest":"a",` + field + `}`
		post(t, f.api, validRequest("/v1/plans/p/approve", body), 400)
	}
	if f.sends.Load() != 0 {
		t.Fatal("presentation fields accepted as approval input")
	}
}
