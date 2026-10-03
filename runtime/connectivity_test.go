package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConnectivityClassification(t *testing.T) {
	// Timeout is transient through typed transport errors, not only DNS.
	if timeout := net.Error(&net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}); !timeout.Timeout() {
		t.Fatal("fixture is not a typed network timeout")
	}
	for _, err := range []error{&net.DNSError{IsTemporary: true}, &net.DNSError{IsTimeout: true}, syscall.ETIMEDOUT, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}, fmt.Errorf("request: %w", syscall.ENETUNREACH), syscall.ECONNRESET} {
		if !transientConnectivity(err) {
			t.Fatalf("not transient: %v", err)
		}
	}
	for _, err := range []error{&net.DNSError{IsNotFound: true}, &net.DNSError{IsNotFound: true, IsTemporary: true}, x509.UnknownAuthorityError{}, errors.New("proxy: invalid configuration"), errors.New("unknown"), errors.New("ENOTFOUND in test output"), syscall.EACCES, context.DeadlineExceeded, context.Canceled} {
		if transientConnectivity(err) {
			t.Fatalf("guessed transient: %v", err)
		}
	}
	if got := classifyAgentFailure(claudeSpec, "api error: can't reach the api server — check your internet or dns (enotfound)"); got != FailureConnectivity {
		t.Fatalf("incident classified %s", got)
	}
}

func TestConnectivityBackoffSurvivesSerialization(t *testing.T) {
	s, clock := deadlineScheduler(t)
	op := plannedExecution(t, s, time.Hour)
	clock.at = clock.at.Add(time.Second)
	deadline := clock.at.Add(connectivityBackoff(op.Attempt))
	finished, err := s.finishAt(op.ID, OperationFailed, deadline)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(finished)
	if err != nil {
		t.Fatal(err)
	}
	var restored RunOperation
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	s.Store = NewMemoryOperationStore()
	if _, _, err := s.Store.PutOperation(restored, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		next, err := s.Next(op.RunID)
		if err != nil || next != nil {
			t.Fatalf("early retry: %+v %v", next, err)
		}
	}
	stored, _, _, _ := s.Store.Operation(op.ID)
	if stored.Attempt != op.Attempt || stored.ActiveSince != nil || stored.ConsumedExecution != time.Second {
		t.Fatalf("budget changed: %+v", stored)
	}
	clock.at = deadline
	next, err := s.Next(op.RunID)
	if err != nil || next == nil {
		t.Fatalf("eligible retry unavailable: %v", err)
	}
	if connectivityBackoff(1000) != 5*time.Minute {
		t.Fatal("backoff unbounded")
	}
}

func TestConnectivityDeadlinePersistsAcrossSQLiteRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutRun(newJournalRun("run-1")); err != nil {
		t.Fatal(err)
	}
	s, clock := deadlineScheduler(t)
	s.Store = store
	op := plannedExecution(t, s, time.Hour)
	deadline := clock.at.Add(time.Minute)
	if _, err := s.finishAt(op.ID, OperationFailed, deadline); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	s.Store = reopened
	if next, err := s.Next(op.RunID); err != nil || next != nil {
		t.Fatalf("restart bypassed wait: %+v %v", next, err)
	}
	clock.at = deadline
	if next, err := s.Next(op.RunID); err != nil || next == nil {
		t.Fatalf("retry unavailable after recovery: %+v %v", next, err)
	}
	if !externalWaitReasons["connectivity_backoff"] {
		t.Fatal("connectivity wait charged as work")
	}
}

func TestConnectivitySourceObservationKeepsFiniteAttempts(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return fmt.Errorf("request: %w", syscall.ENETUNREACH) })
	first := f.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "connectivity_backoff" {
		t.Fatalf("outage: %+v", first)
	}
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var failedOp RunOperation
	for _, op := range ops {
		if op.Kind == OpSourceObserve {
			failedOp = op
		}
	}
	if failedOp.Attempt != 1 || failedOp.RetryNotBefore.IsZero() {
		t.Fatalf("missing bounded wait: %+v", failedOp)
	}
	f.runtime = f.newRuntime(f.deps)
	for i := 0; i < 3; i++ {
		if outcome := f.reconcile(runID); outcome.Disposition != Waiting || outcome.Reason != "connectivity_backoff" {
			t.Fatalf("restart: %+v", outcome)
		}
	}
	stored, _, _, _ := f.store.Operation(failedOp.ID)
	if stored.Attempt != 1 || !stored.RetryNotBefore.Equal(failedOp.RetryNotBefore) {
		t.Fatalf("restart spent authority: %+v", stored)
	}
	var last Outcome
	for attempt := 2; attempt <= failedOp.MaxAttempts; attempt++ {
		f.clock.at = stored.RetryNotBefore
		last = f.reconcile(runID)
		stored, _, _, _ = f.store.Operation(failedOp.ID)
		if stored.Attempt != attempt {
			t.Fatalf("attempt %d: %+v", attempt, stored)
		}
	}
	// The last attempt has no successor, so it neither backs off nor reports
	// a connectivity wait: it stops truthfully in the same pass.
	if last.Disposition != Failed || last.Reason != OpSourceObserve+attemptsExhaustedSuffix || !stored.RetryNotBefore.IsZero() {
		t.Fatalf("exhausted authority did not stop: %+v %+v", last, stored)
	}
}

func TestConnectivitySourceObservationRecoversSameOperation(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return syscall.ENETUNREACH })
	f.reconcile(runID)
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var waiting RunOperation
	for _, op := range ops {
		if op.Kind == OpSourceObserve {
			waiting = op
		}
	}
	f.inject(func(GitHubCall) error { return nil })
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	recovered, _, _, err := f.store.Operation(waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != Succeeded || recovered.Attempt != 2 || !recovered.RetryNotBefore.IsZero() {
		t.Fatalf("recovery changed operation or budget: %+v", recovered)
	}
}

func TestConnectivityJournalStoreGapIsExternalWait(t *testing.T) {
	s, clock := deadlineScheduler(t)
	op := plannedExecution(t, s, time.Hour)
	clock.at = clock.at.Add(time.Second)
	observed := clock.at
	retryAt := observed.Add(connectivityBackoff(op.Attempt))
	after := op
	after.State, after.RetryNotBefore, after.Result = OperationFailed, retryAt, json.RawMessage(`{"failure_class":"connectivity_unavailable"}`)
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	_, openSince, work := foldExternalWait([]EngineeringEvent{{Type: EventOperationAfter, OperationID: op.ID, OccurredAt: observed, Payload: raw}})
	if !openSince.Equal(observed) || work != 0 {
		t.Fatalf("journal lost external wait: %s %s", openSince, work)
	}
	clock.at = clock.at.Add(time.Hour)
	settled, err := s.finishAt(op.ID, OperationFailed, retryAt)
	if err != nil {
		t.Fatal(err)
	}
	if settled.ConsumedExecution != time.Second || !settled.RetryNotBefore.Equal(retryAt) {
		t.Fatalf("store recovery charged downtime: %+v", settled)
	}
}

// Existing governed material and review authority must survive a mutating
// transport refusal, independently of the refused bytes preserved in quarantine.
func TestConnectivityRecoveryPreservesCandidateFeedbackAndQuarantine(t *testing.T) {
	f, runID := feedbackFixture(t)
	f.clock.step = 0
	before := f.state(runID)
	subject := before.projection.CandidateRevision
	number := before.projection.PullRequest.Number
	f.forge.ConversationComments[number] = []GitHubComment{{ID: 501, Author: GitHubActor{Login: "maintainer", ID: 7}, Body: UntrustedText("add B"), CreatedAt: f.clock.Now()}}
	if observation, err := f.runtime.ObserveFeedback(context.Background(), runID); err != nil || observation.Admitted != 1 {
		t.Fatalf("admission: %+v %v", observation, err)
	}
	pending := f.state(runID).feedbackState().Pending(subject)
	if len(pending) != 1 {
		t.Fatalf("missing obligation: %+v", pending)
	}
	publication := func(s *runState) map[string]RunOperation {
		ops := map[string]RunOperation{}
		for id, op := range s.snapshot.Operations {
			if op.Kind == OpCandidatePush || op.Kind == OpPullRequestCreate || op.Kind == OpPullRequestUpdate {
				ops[id] = op
			}
		}
		return ops
	}
	admitted := f.state(runID)
	if len(quarantinedEvents(t, admitted.events)) != 0 {
		t.Fatal("quarantine before the outage")
	}
	f.provider.mutate = refusedWrite().mutate
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureConnectivity}}
	if outcome := f.reconcile(runID); outcome.Disposition != Waiting || outcome.Reason != "connectivity_backoff" {
		t.Fatalf("outage: %+v", outcome)
	}
	var waiting RunOperation
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke && !op.RetryNotBefore.IsZero() {
			waiting = op
		}
	}
	if waiting.Attempt != 1 || waiting.RetryNotBefore.IsZero() {
		t.Fatalf("missing wait: %+v", waiting)
	}
	state := f.state(runID)
	waitEvents := state.events
	projection := state.projection
	active := state.activeElapsed(f.clock.Now())
	budgets := state.budgets()
	feedback := state.feedbackState()
	if projection.CandidateRevision != subject || !reflect.DeepEqual(state.feedbackRedeliveryFor(waiting.ID), pending) {
		t.Fatal("refusal changed candidate or discharged feedback")
	}
	quarantine := quarantinedEvents(t, state.events)
	if len(quarantine) != 1 || quarantine[0].FailureClass != FailureConnectivity || !quarantine[0].Restored || quarantine[0].Attempt != 1 {
		t.Fatalf("quarantine: %+v", quarantine)
	}
	if !reflect.DeepEqual(publication(state), publication(admitted)) {
		t.Fatal("attempt 1 changed publication operations")
	}
	assertA := func() {
		t.Helper()
		kept, err := os.ReadFile(filepath.Join(f.stateDir, quarantine[0].Location, "files", "refused.go"))
		if err != nil || string(kept) != "package refused // A\n" {
			t.Fatalf("lost A: %q %v", kept, err)
		}
		if _, err := os.Stat(filepath.Join(candidateDir(f.stateDir, runID), "refused.go")); !os.IsNotExist(err) {
			t.Fatalf("A inherited: %v", err)
		}
	}
	assertA()
	calls := len(f.provider.requests)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	f.store, f.deps.Store = reopened, reopened
	f.runtime = f.newRuntime(f.deps)
	f.clock.at = waiting.RetryNotBefore.Add(-time.Nanosecond)
	for i := 0; i < 3; i++ {
		if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "connectivity_backoff" {
			t.Fatalf("early recovery: %+v", out)
		}
	}
	restored, _, _, err := f.store.Operation(waiting.ID)
	if err != nil || !reflect.DeepEqual(restored, waiting) {
		t.Fatalf("operation authority changed: %+v / %+v: %v", waiting, restored, err)
	}
	state = f.state(runID)
	projection.Attempts = state.projection.Attempts // Observation epochs may advance; execution authority is checked above.
	if len(f.provider.requests) != calls || !reflect.DeepEqual(state.projection, projection) || !reflect.DeepEqual(state.feedbackState(), feedback) || !reflect.DeepEqual(state.budgets(), budgets) || state.activeElapsed(f.clock.Now()) != active {
		t.Fatalf("wait changed state: calls=%v projection=%v feedback=%v budgets=%v active=%s/%s", len(f.provider.requests) == calls, reflect.DeepEqual(state.projection, projection), reflect.DeepEqual(state.feedbackState(), feedback), reflect.DeepEqual(state.budgets(), budgets), state.activeElapsed(f.clock.Now()), active)
	}
	for _, kind := range []string{EventAssuranceObserved, EventCandidateCommitted, EventCandidateCheckpointed} {
		if countType(state.events, kind) != countType(before.events, kind) {
			t.Fatalf("transition during wait: %s", kind)
		}
	}
	for _, kind := range []string{EventAuthorityEvaluated, EventHumanAuthorityRecorded, EventReviewContinuationGranted, EventGitHubPRObserved, EventExecutionCompleted} {
		if countType(state.events, kind) != countType(waitEvents, kind) {
			t.Fatalf("transition while waiting: %s", kind)
		}
	}
	if !reflect.DeepEqual(publication(state), publication(admitted)) || len(quarantinedEvents(t, state.events)) != 1 {
		t.Fatal("wait changed publication or quarantine")
	}
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}
	f.provider.mutate = func(dir string) error {
		assertA()
		head, err := gitOutput(dir, "rev-parse", "HEAD")
		if err != nil || strings.TrimSpace(head) != subject {
			t.Fatalf("retry subject: %q %v", head, err)
		}
		return os.WriteFile(filepath.Join(dir, "recovered.go"), []byte("package candidate // B\n"), 0600)
	}
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	recovered, _, _, err := f.store.Operation(waiting.ID)
	if err != nil || recovered.State != Succeeded || recovered.Attempt != 2 || recovered.MaxAttempts != waiting.MaxAttempts || !recovered.RetryNotBefore.IsZero() {
		t.Fatalf("recovery: %+v %v", recovered, err)
	}
	if len(f.provider.requests) != calls+1 || len(f.provider.requests[calls].Feedback) == 0 || !reflect.DeepEqual(f.provider.requests[calls].Feedback, f.provider.requests[calls-1].Feedback) {
		t.Fatal("feedback not redelivered to same retry")
	}
	// Attempt 1 recorded its delivery; the redelivered feedback is consumed
	// exactly once more, by B's attempt, under the same operation.
	var consumed []FeedbackConsumedPayload
	for _, e := range f.state(runID).events[len(waitEvents):] {
		if e.Type == EventFeedbackConsumed {
			var p FeedbackConsumedPayload
			if err := decodeJSON(e.Payload, &p); err != nil {
				t.Fatal(err)
			}
			consumed = append(consumed, p)
		}
	}
	if len(consumed) != 1 || consumed[0].OperationID != waiting.ID || consumed[0].Attempt != 2 || len(consumed[0].Keys) != 1 || consumed[0].Keys[0] != pending[0].Key {
		t.Fatalf("feedback consumption after B: %+v", consumed)
	}
	assertA()
	if !committedTreeHas(t, f, runID, "recovered.go") || committedTreeHas(t, f, runID, "refused.go") {
		t.Fatal("committed material did not separate A and B")
	}
	for _, op := range f.state(runID).snapshot.Operations {
		if op.ID == waiting.ID {
			recovered = op
		}
	}
	var result mutationResult
	if err := decodeJSON(recovered.Result, &result); err != nil || result.PathCount != 1 {
		t.Fatalf("attribution: %+v %v", result, err)
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

// The App token exchange is a transport boundary too: only a typed
// connectivity failure there acquires backoff; TLS, proxy configuration and
// the caller's own cancellation fail closed.
func TestConnectivityGitHubAppTransportFailsClosed(t *testing.T) {
	path, _ := appKeyFile(t, 0o600)
	for name, tc := range map[string]struct {
		err       error
		cancel    bool
		transient bool
	}{
		"refused": {err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, transient: true},
		"tls":     {err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}},
		"proxy":   {err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("invalid proxy configuration")}},
		"cancel":  {err: context.Canceled, cancel: true},
	} {
		t.Run(name, func(t *testing.T) {
			credential := &GitHubAppCredential{AppID: 11, InstallationID: 22, PrivateKeyPath: path,
				HTTP: doerFunc(func(r *http.Request) (*http.Response, error) {
					return nil, &url.Error{Op: r.Method, URL: r.URL.String(), Err: tc.err}
				})}
			ctx, cancel := context.WithCancel(context.Background())
			if tc.cancel {
				cancel()
			}
			defer cancel()
			_, err := credential.AppIdentity(ctx)
			var forge *GitHubTransientError
			if err == nil || errors.As(err, &forge) != tc.transient || transientConnectivity(err) != tc.transient {
				t.Fatalf("App transport %s: transient=%v err=%v", name, transientConnectivity(err), err)
			}
		})
	}
}

// The incident's provider surface: the Claude CLI reports the outage as the
// whole text of an is_error stream-json result, not on stderr.
func TestConnectivityClaudeIncidentResultEnvelope(t *testing.T) {
	incident := "API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)"
	t.Run("process exit 1", func(t *testing.T) {
		// A quoted heredoc writes the exact line: no shell quoting to escape.
		provider, request := claudeProcess(t, "cat <<'EOF'\n"+claudeResultWithAnswer(true, incident)+"\nEOF\nexit 1\n")
		result, _ := provider.Execute(context.Background(), request)
		if result.Failure == nil || result.Failure.Classification != FailureConnectivity || result.Invocation == nil || !result.Invocation.FinalResultObserved {
			t.Fatalf("incident failure = %#v %#v", result.Failure, result.Invocation)
		}
	})
	for name, tc := range map[string]struct {
		stdout string
		want   FailureClass
	}{
		"zero exit":         {claudeResultWithAnswer(true, incident), FailureConnectivity},
		"quoted in prose":   {claudeResultWithAnswer(true, "I saw: "+incident), FailureUnknown},
		"model text":        {claudeAssistant("m", "", `{"type":"text","text":"`+incident+`"}`) + "\n" + claudeResult(true, "success", 0), FailureUnknown},
		"not an error":      {claudeResultWithAnswer(false, incident), ""},
		"generic ENOTFOUND": {claudeResultWithAnswer(true, "ENOTFOUND"), FailureUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := agentFixture(t, AgentKindClaudeCode)
			fake.outputs = []CommandOutput{{Stdout: []byte(tc.stdout + "\n")}}
			result, err := provider.Execute(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			var got FailureClass
			if result.Failure != nil {
				got = result.Failure.Classification
			}
			if got != tc.want {
				t.Fatalf("classified %q, want %q", got, tc.want)
			}
		})
	}
}

// RetryNotBefore is not the connectivity flag (#87): a wait of another class
// that carries one keeps its own reason and its refund.
func TestConnectivityRetryNotBeforeIsNotTheConnectivityFlag(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderQuota}}
	if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_quota" {
		t.Fatalf("quota: %+v", out)
	}
	var quota RunOperation
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke {
			quota = op
		}
	}
	stored, version, _, err := f.store.Operation(quota.ID)
	if err != nil {
		t.Fatal(err)
	}
	stored.RetryNotBefore = f.clock.Now().Add(time.Hour)
	if _, _, err := f.store.PutOperation(stored, version); err != nil {
		t.Fatal(err)
	}
	if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_quota" {
		t.Fatalf("quota wait relabelled: %+v", out)
	}
	after, _, _, _ := f.store.Operation(quota.ID)
	if after.Attempt != stored.Attempt {
		t.Fatalf("refund lost: %d -> %d", stored.Attempt, after.Attempt)
	}
	journalled := f.state(runID).snapshot.Operations[quota.ID]
	journalled.RetryNotBefore = stored.RetryNotBefore
	raw, err := json.Marshal(journalled)
	if err != nil {
		t.Fatal(err)
	}
	if _, open, _ := foldExternalWait([]EngineeringEvent{{Type: EventOperationAfter, OperationID: quota.ID, OccurredAt: f.clock.Now(), Payload: raw}}); !open.IsZero() {
		t.Fatal("a quota RetryNotBefore opened a connectivity wait")
	}
}

// omitzero: an operation with no deadline serializes none, and a legacy
// zero-valued record still replays as none.
func TestConnectivityRetryNotBeforeOmittedWhenZero(t *testing.T) {
	raw, err := json.Marshal(RunOperation{ID: "op"})
	if err != nil || strings.Contains(string(raw), "retry_not_before") {
		t.Fatalf("zero deadline serialized: %s %v", raw, err)
	}
	var legacy RunOperation
	if err := json.Unmarshal([]byte(`{"id":"op","retry_not_before":"0001-01-01T00:00:00Z"}`), &legacy); err != nil || !legacy.RetryNotBefore.IsZero() {
		t.Fatalf("legacy replay: %+v %v", legacy, err)
	}
}
