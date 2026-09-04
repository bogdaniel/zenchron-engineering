package runtime

// Every test here drives OpenAIProvider against a fake Responses API and a real
// temporary workspace. No paid API call is made and no container is started, so
// the whole file passes under `docker run --network none`. The only test that
// talks to the real API is the explicitly opt-in one at the bottom.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureAPIKey = "sk-zenchron-fixture-provider-credential-9c3"

func jsonArgs(t testing.TB, fields map[string]any) []byte {
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// fakeResponsesAPI is the injected Doer. It records what actually crossed the
// control-plane boundary, which is how the credential assertions are made.
type fakeResponsesAPI struct {
	bodies   []string
	statuses []int
	requests [][]byte
	headers  []http.Header
	err      error
	// repeat is served forever once the scripted bodies run out, which is what
	// the iteration and no-progress bounds run against.
	repeat string
}

func (f *fakeResponsesAPI) Do(request *http.Request) (*http.Response, error) {
	if err := request.Context().Err(); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	f.requests = append(f.requests, body)
	f.headers = append(f.headers, request.Header.Clone())
	if f.err != nil {
		return nil, f.err
	}
	status, payload := http.StatusOK, f.repeat
	if len(f.bodies) > 0 {
		payload, f.bodies = f.bodies[0], f.bodies[1:]
	}
	if len(f.statuses) > 0 {
		status, f.statuses = f.statuses[0], f.statuses[1:]
	}
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(payload)), Header: http.Header{}}, nil
}

func scriptedToolCalls(t testing.TB, id string, tokens int64, calls ...[2]string) string {
	t.Helper()
	items := []any{}
	for i, call := range calls {
		items = append(items, map[string]any{"type": "function_call", "call_id": fmt.Sprintf("fc_%s_%d", id, i), "name": call[0], "arguments": call[1]})
	}
	return string(jsonArgs(t, map[string]any{"id": "resp_" + id, "model": "gpt-fixture", "output": items, "usage": map[string]any{"total_tokens": tokens}}))
}

func scriptedFinalMessage(t testing.TB, id string, tokens int64) string {
	t.Helper()
	message := map[string]any{"type": "message", "content": []any{map[string]any{"type": "output_text", "text": "objective addressed"}}}
	return string(jsonArgs(t, map[string]any{"id": "resp_" + id, "model": "gpt-fixture", "output": []any{message}, "usage": map[string]any{"total_tokens": tokens}}))
}

// openaiFixture wires a provider whose credential lives OUTSIDE the candidate
// workspace, which is the operator-controlled reference the design requires.
func openaiFixture(t *testing.T, api *fakeResponsesAPI) (OpenAIProvider, ExecutionRequest, *fakeCommandExecutor, string) {
	t.Helper()
	candidate := surfaceWorkspace(t)
	control := t.TempDir()
	keyFile := filepath.Join(control, "openai.key")
	if err := os.WriteFile(keyFile, []byte(fixtureAPIKey+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCommandExecutor{found: true}
	broker := ToolBroker{CandidateDir: candidate, Sandbox: DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "openai-operation", StateDir: filepath.Join(control, "state")}}
	provider := OpenAIProvider{
		ArtifactStore: ArtifactStore{Root: filepath.Join(control, "artifacts")},
		Model:         "gpt-fixture",
		AuthMode:      "operator_key_file",
		APIKeyFile:    keyFile,
		Endpoint:      "https://api.invalid/v1/responses",
		HTTP:          api,
		Broker:        broker,
	}
	// MaxCostMicros is deliberately left nil: this provider has no trusted
	// cost oracle, so a nonzero ceiling here would refuse before any request
	// is made. The dedicated cost-budget tests set it explicitly.
	tokens := int64(1_000_000)
	request := ExecutionRequest{
		RunID:                 "run-9c3",
		OperationID:           "run-9c3:execution.invoke:execution.invoke#initial|1|base-rev-9c3",
		Attempt:               1,
		SourceSnapshot:        Ref{ID: "issue-29", Revision: "snap-9c3"},
		ControllerID:          "controller-9c3",
		Base:                  Ref{ID: "main", Revision: "base-rev-9c3"},
		Candidate:             Candidate{Branch: "zenchron/run-9c3", Revision: "candidate-rev-9c3", Tree: "candidate-tree-9c3"},
		CandidateDir:          candidate,
		Contract:              Ref{ID: "contract-9c3", Revision: "contract-rev-9c3"},
		Objective:             "objective-9c3",
		AcceptanceObligations: []string{"obligation-9c3"},
		Constraints:           []string{"constraint-9c3"},
		Prohibitions:          []string{"prohibition-9c3"},
		Permissions:           []string{"permission-9c3"},
		TrustedInstructions:   "trusted-9c3",
		Purpose:               InvocationInitial,
		Budgets:               ProviderBudget{MaxTokens: &tokens, WallLimit: time.Minute},
	}
	return provider, request, fake, keyFile
}

func stopReason(t *testing.T, err error) ProviderStop {
	t.Helper()
	var stop *ProviderStopError
	if !errors.As(err, &stop) {
		t.Fatalf("loop did not end with a typed, diagnosable outcome: %v", err)
	}
	return stop.Reason
}

// TestOpenAIProviderDrivesEveryToolThroughTheBroker is the full round trip: the
// model requests each of the five M0 capabilities, Zenchron performs them, and
// the results are fed back as function_call_output items.
func TestOpenAIProviderDrivesEveryToolThroughTheBroker(t *testing.T) {
	patch := "diff --git a/added.txt b/added.txt\nnew file mode 100644\n--- /dev/null\n+++ b/added.txt\n@@ -0,0 +1 @@\n+added-9c3\n"
	api := &fakeResponsesAPI{bodies: []string{
		scriptedToolCalls(t, "1", 10, [2]string{openaiToolRepoRead, `{"path":"hello.txt"}`}),
		scriptedToolCalls(t, "2", 10, [2]string{openaiToolRepoSearch, `{"pattern":"candidate-content-9c3","scope":[]}`}),
		scriptedToolCalls(t, "3", 10, [2]string{openaiToolCandidateApplyPatch, string(jsonArgs(t, map[string]any{"patch": patch}))}),
		scriptedToolCalls(t, "4", 10, [2]string{openaiToolCandidateDiff, `{"paths":[]}`}),
		scriptedToolCalls(t, "5", 10, [2]string{openaiToolCandidateRun, `{"command":["go","test","./..."]}`}),
		scriptedFinalMessage(t, "6", 10),
	}}
	provider, request, fake, _ := openaiFixture(t, api)
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("brokered reasoning loop failed: %v", err)
	}
	if result.ProviderID != openaiProviderID || result.Outcome != Succeeded || result.Model != "gpt-fixture" {
		t.Fatalf("provider observation is wrong: %#v", result)
	}
	if result.Tokens == nil || *result.Tokens != 60 {
		t.Fatalf("usage was not recorded: %#v", result.Tokens)
	}
	// CostMicros is deliberately nil: the Responses API returns no cost.
	if result.CostMicros != nil {
		t.Fatalf("provider invented a cost the API does not report: %#v", result.CostMicros)
	}
	if len(result.Artifacts) != 2 || result.Failure != nil {
		t.Fatalf("transcript artifacts or failure state are wrong: %#v", result)
	}
	// The patch really reached the workspace, so the loop drove ToolBroker and
	// not a mock of it.
	added, err := os.ReadFile(filepath.Join(request.CandidateDir, "added.txt"))
	if err != nil || string(added) != "added-9c3\n" {
		t.Fatalf("candidate.apply_patch did not reach the workspace: %v %q", err, added)
	}
	if args := createdContainerArgs(t, fake); args[len(args)-3] != "go" {
		t.Fatalf("candidate.run did not reach the sandbox: %#v", args)
	}
	// Every tool result was returned to the model as a function_call_output.
	last := string(api.requests[len(api.requests)-1])
	for _, want := range []string{openaiToolRepoRead, openaiToolRepoSearch, openaiToolCandidateApplyPatch, openaiToolCandidateDiff, openaiToolCandidateRun, "function_call_output", "candidate-content-9c3", "patch applied", "exit=0"} {
		if !strings.Contains(last, want) {
			t.Fatalf("tool round trip %q never reached the model: %s", want, last)
		}
	}
	if len(api.requests) != 6 {
		t.Fatalf("expected one request per reasoning iteration: %d", len(api.requests))
	}
}

// TestOpenAIProviderBindsTheSessionToTheFullRunIdentity keeps a reasoning
// session from being reusable against a different run, base, candidate or
// contract.
func TestOpenAIProviderBindsTheSessionToTheFullRunIdentity(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, _ := openaiFixture(t, api)
	request.Purpose = InvocationRemediation
	request.Findings = []Finding{{Classification: FailureCompileTest, Signature: "finding-9c3", ArtifactRef: "artifact-9c3"}}
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	body := string(api.requests[0])
	for _, want := range []string{
		"run-9c3", "issue-29", "controller-9c3", "base-rev-9c3", "candidate-rev-9c3", "candidate-tree-9c3",
		"contract-9c3", "contract-rev-9c3", "objective-9c3", "obligation-9c3", "constraint-9c3",
		"prohibition-9c3", "permission-9c3", "trusted-9c3", "finding-9c3", string(InvocationRemediation),
		"max_tokens=1000000", "wall_limit=1m0s",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("session binding is missing %q: %s", want, body)
		}
	}
	// No cost ceiling was requested, so none is advertised in the prompt.
	if strings.Contains(body, "max_cost_micros") {
		t.Fatalf("an unrequested cost ceiling leaked into the prompt: %s", body)
	}
	// The advertised surface is exactly the M0 tools and nothing shell-shaped.
	for _, forbidden := range []string{`"type":"local_shell"`, `"type":"code_interpreter"`, `"type":"shell"`, `"type":"file_search"`, `"type":"web_search"`, `"type":"computer_use_preview"`} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("an unrestricted hosted tool was offered to the model: %s", forbidden)
		}
	}
	// An incomplete binding is refused before any request is made.
	for name, mutate := range map[string]func(*ExecutionRequest){
		"run id":    func(r *ExecutionRequest) { r.RunID = "" },
		"base":      func(r *ExecutionRequest) { r.Base.Revision = "" },
		"candidate": func(r *ExecutionRequest) { r.Candidate.Revision = "" },
		"contract":  func(r *ExecutionRequest) { r.Contract.ID = "" },
		"purpose":   func(r *ExecutionRequest) { r.Purpose = "" },
		"findings":  func(r *ExecutionRequest) { r.Findings = nil },
	} {
		broken := request
		mutate(&broken)
		if _, err := provider.Execute(context.Background(), broken); err == nil {
			t.Fatalf("an execution request missing its %s was accepted", name)
		}
	}
}

// TestOpenAIProviderSuppliesSameBindingPriorAttemptObservations proves that a
// scheduler retry receives durable, bounded context from its predecessor while
// keeping the current request's binding authoritative. It exercises persisted
// artifacts rather than provider-side conversation state.
func TestOpenAIProviderSuppliesSameBindingPriorAttemptObservations(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{
		scriptedToolCalls(t, "first", 5, [2]string{openaiToolRepoRead, `{"path":"hello.txt"}`}),
		scriptedFinalMessage(t, "second", 5),
	}}
	provider, request, _, _ := openaiFixture(t, api)
	provider.MaxIterations = 1
	if got := stopReason(t, mustFail(t, provider, request)); got != StopIterationBudget {
		t.Fatalf("first attempt stopped for %s, want iteration budget", got)
	}

	request.Attempt = 2
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatalf("retry failed: %v", err)
	}
	if len(api.requests) != 2 {
		t.Fatalf("got %d API requests, want one per attempt", len(api.requests))
	}
	retry := string(api.requests[1])
	for _, want := range []string{
		"UNTRUSTED PRIOR-ATTEMPT OBSERVATIONS",
		"prior scheduler attempt 1",
		"hello.txt",
		"candidate-content-9c3",
		// The current request remains independently present and authoritative.
		"candidate-rev-9c3",
		"contract-rev-9c3",
	} {
		if !strings.Contains(retry, want) {
			t.Fatalf("retry omitted %q: %s", want, retry)
		}
	}
	if strings.Contains(string(api.requests[0]), "PRIOR-ATTEMPT") {
		t.Fatal("attempt one was given invented prior-attempt context")
	}
}

// TestPriorAttemptContextDoesNotCrossOperationBindings pins the continuation
// boundary: two operations in one run are different attempt-history scopes.
func TestPriorAttemptContextDoesNotCrossOperationBindings(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	first := attemptRef("run-context", "run-context:execution.invoke:initial", 1)
	if _, err := store.StoreExecutionAttemptTranscript(openaiProviderID, first, []byte("old binding observation\n"), nil); err != nil {
		t.Fatal(err)
	}
	otherBinding := attemptRef("run-context", "run-context:execution.invoke:continuation", 2)
	context, err := store.PriorExecutionAttemptContext(openaiProviderID, otherBinding)
	if err != nil {
		t.Fatal(err)
	}
	if context != "" {
		t.Fatalf("context crossed execution binding: %q", context)
	}
}

// TestOpenAIProviderReturnsToolRefusalsToTheModel proves an inadmissible
// request is a recoverable tool result, not a panic and not a silent
// substitution of some other operation.
func TestOpenAIProviderReturnsToolRefusalsToTheModel(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{
		scriptedToolCalls(t, "1", 5, [2]string{"shell", `{"command":["sh","-c","cat /etc/passwd"]}`}),
		scriptedToolCalls(t, "2", 5, [2]string{openaiToolRepoRead, `{"path":"hello.txt","follow_symlinks":true}`}),
		scriptedToolCalls(t, "3", 5, [2]string{openaiToolRepoRead, `{"path":"../outside/data.txt"}`}),
		scriptedToolCalls(t, "4", 5, [2]string{openaiToolRepoRead, `{"path":"hello.txt"}`}),
		scriptedFinalMessage(t, "5", 5),
	}}
	provider, request, fake, _ := openaiFixture(t, api)
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("refusals must be recoverable, not fatal: %v", err)
	}
	if result.Outcome != Succeeded {
		t.Fatalf("the loop recovered but did not complete: %#v", result)
	}
	body := string(api.requests[len(api.requests)-1])
	for _, want := range []string{"unknown tool", "malformed tool arguments", "unsafe brokered path"} {
		if !strings.Contains(body, want) {
			t.Fatalf("refusal %q was not returned to the model: %s", want, body)
		}
	}
	if strings.Contains(body, "root:x:0:0") || strings.Contains(body, "runtime-state-9c3") {
		t.Fatalf("a refused request still surfaced out-of-workspace content: %s", body)
	}
	// The recovery path still works, and nothing executed on the refusals.
	if !strings.Contains(body, "candidate-content-9c3") {
		t.Fatalf("the model could not recover after a refusal: %s", body)
	}
	for _, call := range fake.calls {
		if call.name == "docker" && len(call.args) > 0 && call.args[0] == "create" {
			t.Fatalf("a refused tool request still executed a command: %#v", call.args)
		}
	}
}

// TestOpenAIProviderEndsOnEveryBound walks each bound and requires a typed
// outcome. None of these may loop forever.
func TestOpenAIProviderEndsOnEveryBound(t *testing.T) {
	read := scriptedToolCalls(t, "loop", 5, [2]string{openaiToolRepoRead, `{"path":"hello.txt"}`})
	fail := scriptedToolCalls(t, "loop", 5, [2]string{openaiToolRepoRead, `{"path":"../outside/data.txt"}`})

	t.Run("iterations", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: read}
		provider, request, _, _ := openaiFixture(t, api)
		provider.MaxIterations = 3
		result, err := provider.Execute(context.Background(), request)
		if got := stopReason(t, err); got != StopIterationBudget {
			t.Fatalf("iteration bound did not end the loop: %s", got)
		}
		if len(api.requests) != 3 || result.Outcome != OperationFailed || result.Failure == nil {
			t.Fatalf("iteration bound was not enforced at the configured ceiling: %d %#v", len(api.requests), result)
		}
	})

	t.Run("tool calls", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: scriptedToolCalls(t, "loop", 5,
			[2]string{openaiToolRepoRead, `{"path":"hello.txt"}`},
			[2]string{openaiToolRepoRead, `{"path":"hello.txt"}`},
			[2]string{openaiToolRepoRead, `{"path":"hello.txt"}`})}
		provider, request, _, _ := openaiFixture(t, api)
		provider.MaxIterations = 100
		provider.MaxToolCalls = 2
		if got := stopReason(t, mustFail(t, provider, request)); got != StopToolCallBudget {
			t.Fatalf("tool call bound did not end the loop: %s", got)
		}
	})

	t.Run("tokens", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: scriptedToolCalls(t, "loop", 5_000_000, [2]string{openaiToolRepoRead, `{"path":"hello.txt"}`})}
		provider, request, _, _ := openaiFixture(t, api)
		provider.MaxIterations = 100
		if got := stopReason(t, mustFail(t, provider, request)); got != StopTokenBudget {
			t.Fatalf("token budget did not end the loop: %s", got)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: read}
		provider, request, _, _ := openaiFixture(t, api)
		provider.Timeout = time.Nanosecond
		result, err := provider.Execute(context.Background(), request)
		if got := stopReason(t, err); got != StopDeadlineExceeded {
			t.Fatalf("operation timeout did not end the loop: %s", got)
		}
		if result.Outcome != OperationCancelled {
			t.Fatalf("a timed-out loop is not reported as cancelled: %#v", result)
		}
	})

	t.Run("wall limit from the request budget", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: read}
		provider, request, _, _ := openaiFixture(t, api)
		request.Budgets.WallLimit = time.Nanosecond
		if got := stopReason(t, mustFail(t, provider, request)); got != StopDeadlineExceeded {
			t.Fatalf("the request wall limit was not applied: %s", got)
		}
	})

	t.Run("cancellation", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: read}
		provider, request, _, _ := openaiFixture(t, api)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := provider.Execute(ctx, request)
		if got := stopReason(t, err); got != StopCancelled {
			t.Fatalf("cancellation did not end the loop: %s", got)
		}
		if result.Outcome != OperationCancelled {
			t.Fatalf("a cancelled loop is not reported as cancelled: %#v", result)
		}
	})

	t.Run("no progress", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: fail}
		provider, request, _, _ := openaiFixture(t, api)
		provider.MaxIterations = 100
		provider.NoProgressLimit = 1
		if got := stopReason(t, mustFail(t, provider, request)); got != StopNoProgress {
			t.Fatalf("no-progress detection did not end the loop: %s", got)
		}
		if len(api.requests) > 3 {
			t.Fatalf("no-progress detection fired far too late: %d", len(api.requests))
		}
	})

	t.Run("provider error", func(t *testing.T) {
		api := &fakeResponsesAPI{statuses: []int{http.StatusServiceUnavailable}, bodies: []string{"the selected model is at capacity. please try again"}}
		provider, request, _, _ := openaiFixture(t, api)
		result, err := provider.Execute(context.Background(), request)
		if got := stopReason(t, err); got != StopProviderError {
			t.Fatalf("an API failure did not end the loop: %s", got)
		}
		if result.Failure == nil || result.Failure.Classification != FailureTransientProvider {
			t.Fatalf("transient capacity was not classified: %#v", result.Failure)
		}
	})

	t.Run("unbounded configuration still terminates", func(t *testing.T) {
		api := &fakeResponsesAPI{repeat: read}
		provider, request, _, _ := openaiFixture(t, api)
		// Every bound left at its zero value: the defaults must still be finite.
		if got := stopReason(t, mustFail(t, provider, request)); got != StopIterationBudget {
			t.Fatalf("an unconfigured provider was not bounded: %s", got)
		}
	})
}

// explodingDoer proves the absence of an HTTP call rather than merely
// inferring it: any invocation fails the test immediately, so a refusal that
// happened to occur AFTER a request was already sent would be caught here,
// not just by an empty request count.
type explodingDoer struct{ t *testing.T }

func (e explodingDoer) Do(*http.Request) (*http.Response, error) {
	e.t.Helper()
	e.t.Fatal("provider must refuse an unenforceable cost ceiling before issuing any HTTP request")
	return nil, nil
}

// TestOpenAIProviderRefusesUnenforceableCostBudget is the defect this change
// fixes: MaxCostMicros > 0 with no trusted cost oracle configured must refuse
// before any provider inference, not execute as if the ceiling were honored.
func TestOpenAIProviderRefusesUnenforceableCostBudget(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, _ := openaiFixture(t, api)
	// The exploding Doer replaces the fake API entirely: if the refusal does
	// not happen before the first HTTP request, this test fails immediately
	// rather than passing on an unexamined empty request slice.
	provider.HTTP = explodingDoer{t}
	cost := int64(1)
	request.Budgets.MaxCostMicros = &cost
	result, err := provider.Execute(context.Background(), request)
	if got := stopReason(t, err); got != StopCostBudgetUnenforceable {
		t.Fatalf("an unenforceable cost ceiling was not refused with a typed stop: %s", got)
	}
	if result.Outcome == Succeeded {
		t.Fatalf("a refused cost ceiling must not be reported as success: %#v", result)
	}
}

// TestOpenAIProviderZeroCostCeilingPermitsExecution proves the other half of
// the semantics: no cost ceiling requested (nil, and explicitly zero) must
// not block execution, and the token/iteration/tool-call/timeout/no-progress
// budgets stay enforced exactly as before (covered by
// TestOpenAIProviderEndsOnEveryBound, which never sets a cost ceiling).
func TestOpenAIProviderZeroCostCeilingPermitsExecution(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, _ := openaiFixture(t, api)
	zero := int64(0)
	request.Budgets.MaxCostMicros = &zero
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("a zero cost ceiling must not block execution: %v", err)
	}
	if result.Outcome != Succeeded || len(api.requests) != 1 {
		t.Fatalf("a zero cost ceiling did not permit normal execution: %#v requests=%d", result, len(api.requests))
	}
}

func mustFail(t *testing.T, provider OpenAIProvider, request ExecutionRequest) error {
	t.Helper()
	_, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("a bounded loop ended without a diagnosable outcome")
	}
	return err
}

// TestOpenAIProviderCredentialNeverLeavesTheControlPlane is the security
// invariant. The key may appear only on the outbound Authorization header.
func TestOpenAIProviderCredentialNeverLeavesTheControlPlane(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-ambient-must-not-be-used-9c3")
	api := &fakeResponsesAPI{bodies: []string{
		// The model tries every way it has to ask for the credential.
		scriptedToolCalls(t, "1", 5,
			[2]string{openaiToolRepoRead, `{"path":"../openai.key"}`},
			[2]string{openaiToolRepoRead, `{"path":"openai.key"}`},
			[2]string{openaiToolRepoSearch, `{"pattern":"sk-zenchron","scope":[]}`},
			[2]string{openaiToolCandidateRun, `{"command":["env"]}`},
			[2]string{openaiToolCandidateRun, `{"command":["cat","/proc/self/environ"]}`}),
		scriptedFinalMessage(t, "2", 5),
	}}
	provider, request, fake, keyFile := openaiFixture(t, api)
	absolute := string(jsonArgs(t, map[string]any{"path": keyFile}))
	api.bodies = append([]string{scriptedToolCalls(t, "0", 5, [2]string{openaiToolRepoRead, absolute})}, api.bodies...)
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}

	// 1. The credential never reaches a candidate command environment, and the
	//    ambient environment is never inherited either.
	var executed []string
	for _, call := range fake.calls {
		executed = append(executed, strings.Join(call.args, " "), strings.Join(call.env, " "))
	}
	for _, forbidden := range []string{fixtureAPIKey, "sk-ambient-must-not-be-used-9c3", "OPENAI_API_KEY", "Authorization", keyFile} {
		if strings.Contains(strings.Join(executed, "\n"), forbidden) {
			t.Fatalf("provider credential reached a candidate command: %q", forbidden)
		}
	}

	// 2. The credential never reaches an artifact, raw or sanitized.
	if len(result.Artifacts) == 0 {
		t.Fatal("no transcript artifact was stored")
	}
	for _, artifact := range result.Artifacts {
		data, err := os.ReadFile(artifact.Path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), fixtureAPIKey) {
			t.Fatalf("provider credential was written into artifact %s", artifact.Path)
		}
	}

	// 3. The credential never comes back through a tool result. Every attempt
	//    above is refused, and the refusals reach the model without the value.
	body := string(api.requests[len(api.requests)-1])
	if strings.Contains(body, fixtureAPIKey) {
		t.Fatalf("provider credential was returned to the model through a tool result: %s", body)
	}
	if !strings.Contains(body, "tool error:") {
		t.Fatalf("credential probes were not refused: %s", body)
	}

	// 4. The credential DOES reach the control-plane request, as a header only,
	//    so the assertions above are about confinement and not about absence.
	if len(api.headers) == 0 || api.headers[0].Get("Authorization") != "Bearer "+fixtureAPIKey {
		t.Fatalf("the control plane did not authenticate with the operator key file: %v", api.headers)
	}
	for _, request := range api.requests {
		if strings.Contains(string(request), fixtureAPIKey) {
			t.Fatal("the credential was serialized into a request body instead of staying a header")
		}
	}
}

// TestOpenAIProviderRefusesAnyCredentialItDoesNotOwn covers the configuration
// side of the same invariant.
func TestOpenAIProviderRefusesAnyCredentialItDoesNotOwn(t *testing.T) {
	api := &fakeResponsesAPI{bodies: []string{scriptedFinalMessage(t, "1", 5)}}
	provider, request, _, _ := openaiFixture(t, api)

	inline := provider
	inline.APIKeyFile = ""
	if _, err := inline.Execute(context.Background(), request); err == nil || !strings.Contains(err.Error(), "not an inline credential") {
		t.Fatalf("a provider with no operator key file was accepted: %v", err)
	}
	literal := provider
	literal.APIKeyFile = fixtureAPIKey
	if _, err := literal.Execute(context.Background(), request); err == nil {
		t.Fatal("an inline credential value was accepted as a key file path")
	}
	directory := provider
	directory.APIKeyFile = t.TempDir()
	if _, err := directory.Execute(context.Background(), request); err == nil {
		t.Fatal("a directory was accepted as a key file")
	}
	// A key inside the candidate workspace would be repository-supplied.
	repository := provider
	repository.APIKeyFile = filepath.Join(request.CandidateDir, "repo-supplied.key")
	if err := os.WriteFile(repository.APIKeyFile, []byte("sk-repository-supplied-9c3"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Execute(context.Background(), request); err == nil || !strings.Contains(err.Error(), "must not come from the candidate workspace") {
		t.Fatalf("a repository-supplied credential was accepted: %v", err)
	}
	empty := provider
	empty.APIKeyFile = filepath.Join(t.TempDir(), "blank.key")
	if err := os.WriteFile(empty.APIKeyFile, []byte("  \n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.Execute(context.Background(), request); err == nil {
		t.Fatal("an empty key file was accepted")
	}
}

// TestProtectedIsolationAcceptsTheBrokeredProviderAndStillRefusesCodex is the
// eligibility rule this whole provider exists to satisfy.
func TestProtectedIsolationAcceptsTheBrokeredProviderAndStillRefusesCodex(t *testing.T) {
	if err := RequireProtectedIsolation(OpenAIProvider{}); err != nil {
		t.Fatalf("the brokered provider was refused for protected autonomous execution: %v", err)
	}
	isolation := OpenAIProvider{}.Isolation()
	if isolation.FilesystemRead != IsolationProven || isolation.FilesystemWrite != IsolationProven || isolation.NetworkDenied != IsolationProven || isolation.CredentialScope != IsolationProven {
		t.Fatalf("the brokered provider does not claim a fully proven boundary: %#v", isolation)
	}
	err := RequireProtectedIsolation(NativeCodexProvider{})
	if err == nil || !strings.Contains(err.Error(), "filesystem read confinement is unproven") {
		t.Fatalf("NativeCodexProvider must remain ineligible: %v", err)
	}
}

// TestOpenAIProviderReachesTheRealAPIWhenConfigured is the only test that spends
// money. It is skipped unless an operator opts in with a key file, matching the
// Docker sandbox tests' gating.
func TestOpenAIProviderReachesTheRealAPIWhenConfigured(t *testing.T) {
	keyFile := os.Getenv("ZENCHRON_OPENAI_TEST_KEY_FILE")
	model := os.Getenv("ZENCHRON_OPENAI_TEST_MODEL")
	if keyFile == "" || model == "" {
		t.Skip("set ZENCHRON_OPENAI_TEST_KEY_FILE and ZENCHRON_OPENAI_TEST_MODEL to exercise the real API")
	}
	api := &fakeResponsesAPI{}
	provider, request, _, _ := openaiFixture(t, api)
	provider.APIKeyFile, provider.Model, provider.Endpoint, provider.HTTP = keyFile, model, "", &http.Client{Timeout: time.Minute}
	provider.MaxIterations, provider.MaxToolCalls = 4, 8
	result, err := provider.Execute(context.Background(), request)
	var stop *ProviderStopError
	if err != nil && !errors.As(err, &stop) {
		t.Fatalf("real API call failed without a diagnosable outcome: %v", err)
	}
	for _, artifact := range result.Artifacts {
		data, readErr := os.ReadFile(artifact.Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		key, readErr := os.ReadFile(keyFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if strings.Contains(string(data), strings.TrimSpace(string(key))) {
			t.Fatalf("the real credential was written into artifact %s", artifact.Path)
		}
	}
}
