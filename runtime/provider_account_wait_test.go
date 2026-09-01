package runtime

// Regression proof for the third #32 dogfood, run-43b90fd6440f16f42a3dde294ceb6297.
//
// The run reached the provider, the tool surface passed server validation, and
// the provider answered:
//
//	HTTP 429, error.code credit_balance_exhausted
//
// That is a recoverable EXTERNAL ACCOUNT prerequisite: not transient, not a
// producer failure, not an authority failure, and not terminal. The runtime
// classified it `unknown`, which routes to stop, so the run went
// disposition=failed reason=execution.invoke_failure_not_retryable and status
// then blamed the human-authority boundary for refusing a terminal run.
//
// Everything here runs against a fake transport and a fake command executor.
// No API request is made, no billing action is taken, and no container starts.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// creditExhausted429 is the exact provider answer the real run received.
const creditExhausted429 = `{"error":{"message":"Your credit balance is too low to access the API.","type":"invalid_request_error","param":null,"code":"credit_balance_exhausted"}}`

// switchableTransport serves one scripted state until a test switches it. It is
// the external world: nothing in the runtime can change what it answers, which
// is the whole point of the condition being external.
type switchableTransport struct {
	mu       sync.Mutex
	calls    int
	status   int
	bodies   []string
	repeat   string
	requests [][]byte
}

func (t *switchableTransport) Do(request *http.Request) (*http.Response, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if request.Body != nil {
		body, _ := io.ReadAll(request.Body)
		t.requests = append(t.requests, body)
	}
	payload := t.repeat
	if len(t.bodies) > 0 {
		payload, t.bodies = t.bodies[0], t.bodies[1:]
	}
	return &http.Response{StatusCode: t.status, Body: io.NopCloser(strings.NewReader(payload)), Header: http.Header{}}, nil
}

func (t *switchableTransport) restoreAccount(status int, bodies ...string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status, t.bodies, t.repeat = status, bodies, ""
}

func (t *switchableTransport) callCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func exhaustedAccountTransport() *switchableTransport {
	return &switchableTransport{status: http.StatusTooManyRequests, repeat: creditExhausted429}
}

// executionOperation returns the run's execution.invoke operation as the
// SCHEDULER holds it, which is where the attempt budget lives.
func executionOperation(t *testing.T, store OperationStore, runID string) RunOperation {
	t.Helper()
	ops, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke {
			return op
		}
	}
	t.Fatalf("run %s has no %s operation", runID, OpExecutionInvoke)
	return RunOperation{}
}

// TestCreditBalanceExhaustedIsAProviderAccountWait is the classification proof.
func TestCreditBalanceExhaustedIsAProviderAccountWait(t *testing.T) {
	if got := classifyOpenAIFailure(openaiCreditBalanceExhausted, []byte(creditExhausted429)); got != FailureProviderAccountUnavailable {
		t.Fatalf("credit_balance_exhausted classified as %q, want %q", got, FailureProviderAccountUnavailable)
	}
	if got := RouteFailure(FailureProviderAccountUnavailable); got != RouteWait {
		t.Fatalf("route = %q, want %q", got, RouteWait)
	}
	for _, forbidden := range []FailureRoute{RouteRetry, RouteStop, RouteReassess, RouteProviderRemediation, RouteGofmt, RouteRestore} {
		if RouteFailure(FailureProviderAccountUnavailable) == forbidden {
			t.Fatalf("the provider-account condition must not route to %q", forbidden)
		}
	}
	if FailureProviderAccountUnavailable == FailureAuthorityWait {
		t.Fatal("the provider-account condition must not reuse the authority wait")
	}
	if waitReason(FailureProviderAccountUnavailable) != "execution_provider_account_unavailable" {
		t.Fatalf("wait reason = %q", waitReason(FailureProviderAccountUnavailable))
	}
}

// TestOtherProviderFailuresKeepTheirExistingClassification proves the repair is
// narrow: only the one observed code changed meaning. Nothing here invents a
// classification for a code this runtime has never seen.
func TestOtherProviderFailuresKeepTheirExistingClassification(t *testing.T) {
	for name, tc := range map[string]struct {
		code string
		raw  string
		want FailureClass
	}{
		"transient capacity keeps the transient retry class": {
			code: "", raw: `{"error":{"message":"the selected model is at capacity"}}`, want: FailureTransientProvider,
		},
		"ordinary rate limiting keeps its existing conservative class": {
			code: "rate_limit_exceeded", raw: `{"error":{"code":"rate_limit_exceeded"}}`, want: FailureUnknown,
		},
		"malformed request keeps its existing conservative class": {
			code: "invalid_value", raw: `{"error":{"code":"invalid_value","param":"tools[0].name"}}`, want: FailureUnknown,
		},
		"an unknown code keeps its existing conservative class": {
			code: "some_future_code", raw: `{"error":{"code":"some_future_code"}}`, want: FailureUnknown,
		},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyOpenAIFailure(tc.code, []byte(tc.raw)); got != tc.want {
				t.Fatalf("classified as %q, want %q", got, tc.want)
			}
		})
	}
	// A 429 is not what classifies anything: the same status carries both an
	// exhausted balance and ordinary throttling, and they route oppositely.
	if classifyOpenAIFailure("rate_limit_exceeded", nil) == classifyOpenAIFailure(openaiCreditBalanceExhausted, nil) {
		t.Fatal("every HTTP 429 was classified identically")
	}
}

// TestProviderAccountUnavailableWaitsInsteadOfFailing is the run-semantics
// proof for one pass: one attempt, no retry, waiting rather than failed, and
// every binding still owned by the same run.
func TestProviderAccountUnavailableWaitsInsteadOfFailing(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := exhaustedAccountTransport()
	useRealProvider(t, fixture, runID, api, nil)

	outcome := fixture.reconcile(runID)
	if outcome.Disposition != Waiting {
		t.Fatalf("disposition = %q (%q), want waiting", outcome.Disposition, outcome.Reason)
	}
	if outcome.Reason != "execution_provider_account_unavailable" {
		t.Fatalf("wait reason = %q", outcome.Reason)
	}
	if api.callCount() != 1 {
		t.Fatalf("the provider was called %d times; a provider-account wait performs no immediate retry", api.callCount())
	}
	events := journalOf(t, fixture.runtime, runID)
	if attempts := countExecutionAttempts(events); attempts != 1 {
		t.Fatalf("execution.invoke ran %d attempts, want exactly one", attempts)
	}
	if countType(events, EventRunFailed) != 0 {
		t.Fatalf("a recoverable provider-account condition failed the run: %v", journalTypes(events))
	}

	// The durable diagnostic names the condition, and the run still owns
	// everything it owned before.
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	d := projection.ExecutionDiagnostic
	if d == nil || d.FailureClass != FailureProviderAccountUnavailable || d.Route != RouteWait {
		t.Fatalf("the diagnostic does not name the provider-account wait: %#v", d)
	}
	if d.HTTPStatus != http.StatusTooManyRequests || d.ProviderErrorCode != openaiCreditBalanceExhausted {
		t.Fatalf("the diagnostic lost the observed provider facts: %#v", d)
	}
	after := fixture.state(runID)
	if after.baseRevision() != fixture.base {
		t.Fatalf("the wait moved the base binding: %s, want %s", after.baseRevision(), fixture.base)
	}
	if after.projection.Contract == (Ref{}) {
		t.Fatal("the wait lost the compiled contract binding")
	}
	if after.source == nil || after.source.Issue != fixture.issue {
		t.Fatalf("the wait lost the pinned source: %#v", after.source)
	}
	if after.projection.CandidateRevision != "" {
		t.Fatalf("a provider-account condition produced a candidate commit: %q", after.projection.CandidateRevision)
	}
	// No permission or authority was granted by waiting.
	if len(after.projection.AuthorityDecisions) != 0 {
		t.Fatalf("the wait produced authority decisions: %#v", after.projection.AuthorityDecisions)
	}
}

// TestRepeatedTicksDuringProviderAccountWaitDoNotBurnAttempts is the watch
// proof. A run left waiting on an external account must survive being polled:
// each pass may ask the provider once, because there is no free way to observe
// an account, but the run's execution budget must not shrink for it.
func TestRepeatedTicksDuringProviderAccountWaitDoNotBurnAttempts(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := exhaustedAccountTransport()
	useRealProvider(t, fixture, runID, api, nil)

	// More ticks than the operation's own attempt ceiling: if a tick consumed
	// budget, the run would be failed as attempts_exhausted before this ends.
	const ticks = 8
	for tick := 1; tick <= ticks; tick++ {
		outcome := fixture.reconcile(runID)
		if outcome.Disposition != Waiting || outcome.Reason != "execution_provider_account_unavailable" {
			t.Fatalf("tick %d left the run %q (%q); the wait did not hold", tick, outcome.Disposition, outcome.Reason)
		}
		op := executionOperation(t, fixture.store, runID)
		if tick == 1 && op.MaxAttempts >= ticks {
			t.Fatalf("this proof needs more ticks than the ceiling of %d", op.MaxAttempts)
		}
		if op.Attempt >= op.MaxAttempts {
			t.Fatalf("tick %d burned the execution budget: attempt %d of %d", tick, op.Attempt, op.MaxAttempts)
		}
	}
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventRunFailed) != 0 {
		t.Fatalf("polling a waiting run failed it: %v", journalTypes(events))
	}
	// Exactly one provider attempt per pass, never two in one pass: there is no
	// free way to observe an account, so a pass asks once and then waits.
	if attempts := countExecutionAttempts(events); attempts != ticks {
		t.Fatalf("%d execution attempts over %d passes; a pass must make exactly one", attempts, ticks)
	}
}

// TestProviderAccountWaitResumesAfterTheAccountIsRestored is the full restart
// and resume proof: waiting, store closed, reopened from the journal alone, the
// external condition corrected outside the runtime, and then an ordinary
// reconcile continues THE SAME run.
func TestProviderAccountWaitResumesAfterTheAccountIsRestored(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	api := exhaustedAccountTransport()
	useRealProvider(t, fixture, runID, api, nil)

	if outcome := fixture.reconcile(runID); outcome.Disposition != Waiting {
		t.Fatalf("disposition = %q (%q), want waiting", outcome.Disposition, outcome.Reason)
	}
	workspaceBefore := candidateDir(fixture.stateDir, runID)
	before := fixture.state(runID)
	contractBefore, sourceBefore := before.projection.Contract.ID, *before.source

	// Restart: nothing in memory survives.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	fixture.store = reopened
	fixture.deps.Store = reopened

	// The run is still waiting on the same reason, read from the journal alone.
	events, err := reopened.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(EngineeringRun{ID: runID}, events)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Disposition != Waiting || snapshot.Reason != "execution_provider_account_unavailable" {
		t.Fatalf("the wait did not survive the restart: %q (%q)", snapshot.Disposition, snapshot.Reason)
	}

	// The external condition is corrected OUTSIDE the runtime, and the same run
	// is reconciled again through the ordinary path.
	patch := "diff --git a/provider-account-resume.txt b/provider-account-resume.txt\nnew file mode 100644\n--- /dev/null\n+++ b/provider-account-resume.txt\n@@ -0,0 +1 @@\n+resumed-after-funding\n"
	api.restoreAccount(http.StatusOK,
		scriptedToolCalls(t, "resume", 10, [2]string{openaiToolCandidateApplyPatch, string(jsonArgs(t, map[string]any{"patch": patch}))}),
		scriptedFinalMessage(t, "resume-done", 10))
	useRealProvider(t, fixture, runID, api, func(p *OpenAIProvider) { p.MaxIterations = 4 })
	callsBefore := api.callCount()
	fixture.reconcile(runID)
	if api.callCount() <= callsBefore {
		t.Fatal("resume never reached the provider: the wait was permanent")
	}

	// The same run continued: one workspace, the same bindings, and execution
	// actually produced a mutation this time.
	if got := candidateDir(fixture.stateDir, runID); got != workspaceBefore {
		t.Fatalf("resume created a second candidate workspace: %s vs %s", got, workspaceBefore)
	}
	after := fixture.state(runID)
	if after.run.ID != runID {
		t.Fatalf("resume started a different run: %s", after.run.ID)
	}
	// The contract REVISION may legitimately advance - a produced candidate is
	// reassessed - but the contract identity, and the source the run is pinned
	// to, are bindings a wait must never have moved.
	if after.projection.Contract.ID != contractBefore {
		t.Fatalf("resume moved the contract binding: %v, want %s", after.projection.Contract, contractBefore)
	}
	if after.source == nil || *after.source != sourceBefore {
		t.Fatalf("resume moved the pinned source: %#v", after.source)
	}
	resumed := journalOf(t, fixture.runtime, runID)
	if !executionProduced(t, resumed) {
		t.Fatalf("execution never proceeded from the preserved run: %v", journalTypes(resumed))
	}
	if snapshot, err := Reduce(EngineeringRun{ID: runID}, resumed); err != nil {
		t.Fatal(err)
	} else if snapshot.Reason == "execution_provider_account_unavailable" {
		t.Fatal("the run is still waiting on an account condition that was corrected")
	}
}

// countExecutionAttempts counts started execution.invoke attempts in the
// journal. operation.before is appended once per attempt, so this is the
// durable record of how many times the provider was actually asked.
func countExecutionAttempts(events []EngineeringEvent) int {
	n := 0
	for _, e := range events {
		if e.Type != EventOperationBefore {
			continue
		}
		var op RunOperation
		if json.Unmarshal(e.Payload, &op) == nil && op.Kind == OpExecutionInvoke {
			n++
		}
	}
	return n
}

// executionProduced reports whether an execution.invoke recorded a mutation,
// which is the proof that reasoning actually ran rather than being refused
// again at the account boundary.
func executionProduced(t *testing.T, events []EngineeringEvent) bool {
	t.Helper()
	for _, record := range executionDiagnostics(t, events) {
		if record.Mutated {
			return true
		}
	}
	return false
}
