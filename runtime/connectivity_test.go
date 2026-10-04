package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
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
	// Timeout is transport loss through typed errors, not only DNS.
	timeout := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ETIMEDOUT}
	if !net.Error(timeout).Timeout() {
		t.Fatal("fixture is not a typed network timeout")
	}
	wrap := func(err error) error { return &url.Error{Op: "Get", URL: "https://api.github.com", Err: err} }
	for _, tc := range []struct {
		err  error
		want TransportCause
	}{
		{&net.DNSError{IsTemporary: true}, TransportDNSTemporary},
		{&net.DNSError{IsTimeout: true}, TransportDNSTemporary},
		{syscall.ETIMEDOUT, TransportTimeout},
		{wrap(timeout), TransportTimeout},
		{fmt.Errorf("request: %w", syscall.ENETUNREACH), TransportUnreachable},
		{wrap(&net.OpError{Op: "dial", Err: syscall.EHOSTUNREACH}), TransportUnreachable},
		{syscall.ECONNRESET, TransportReset},
		{wrap(&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}), TransportRefused},
		// Fail closed.
		{wrap(&net.DNSError{IsNotFound: true}), TransportDNSNotFound},
		{&net.DNSError{IsNotFound: true, IsTemporary: true}, TransportDNSNotFound},
		{wrap(&tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}), TransportTLS},
		{x509.HostnameError{Certificate: &x509.Certificate{}, Host: "h"}, TransportTLS},
		{wrap(&net.OpError{Op: "proxyconnect", Net: "tcp", Err: syscall.ECONNREFUSED}), TransportProxy},
		{wrap(context.Canceled), TransportCallerCancelled},
		{wrap(context.DeadlineExceeded), TransportCallerCancelled},
		{errors.New("proxy: invalid configuration"), TransportUnrecognized},
		{errors.New("ENOTFOUND in test output"), TransportUnrecognized},
		{syscall.EACCES, TransportUnrecognized},
		// A wrapper type decides nothing: a 503 is capacity, not transport.
		{&GitHubTransientError{Status: 503}, TransportUnrecognized},
	} {
		if got := transportCause(tc.err); got != tc.want {
			t.Fatalf("%v: cause %q, want %q", tc.err, got, tc.want)
		}
		if got := transportCause(transportFailure(tc.err, false)); got != tc.want {
			t.Fatalf("%v: TransportError lost the cause: %q", tc.err, got)
		}
		lost := tc.want.Lost()
		if class := failureClassOf(mustMarshal(t, failed(tc.err).result)); (class == FailureConnectivity) != lost {
			t.Fatalf("%v: failed() class %q, lost=%v", tc.err, class, lost)
		}
	}
	if got := classifyAgentFailure(claudeSpec, "api error: can't reach the api server — check your internet or dns (enotfound)"); got != FailureConnectivity {
		t.Fatalf("incident classified %s", got)
	}
}

func mustMarshal(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestConnectivityBackoffSurvivesSerialization(t *testing.T) {
	s, clock := deadlineScheduler(t)
	op := plannedExecution(t, s, time.Hour)
	clock.at = clock.at.Add(time.Second)
	deadline := clock.at.Add(connectivityBackoff(op.Attempt))
	finished, err := s.finishAt(op.ID, OperationFailed, deadline, DispositionTransportBackoff, time.Time{})
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
	if _, err := s.finishAt(op.ID, OperationFailed, deadline, DispositionTransportBackoff, time.Time{}); err != nil {
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
	after.State, after.RetryNotBefore, after.RetryDisposition = OperationFailed, retryAt, DispositionTransportBackoff
	raw, err := json.Marshal(after)
	if err != nil {
		t.Fatal(err)
	}
	_, openSince, work := foldExternalWait([]EngineeringEvent{{Type: EventOperationAfter, OperationID: op.ID, OccurredAt: observed, Payload: raw}})
	if !openSince.Equal(observed) || work != 0 {
		t.Fatalf("journal lost external wait: %s %s", openSince, work)
	}
	clock.at = clock.at.Add(time.Hour)
	settled, err := s.finishAt(op.ID, OperationFailed, retryAt, DispositionTransportBackoff, observed)
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
		err    error
		cancel bool
		want   TransportCause
	}{
		"refused": {err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}, want: TransportRefused},
		"tls":     {err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, want: TransportTLS},
		"proxy":   {err: &net.OpError{Op: "proxyconnect", Net: "tcp", Err: errors.New("invalid proxy configuration")}, want: TransportProxy},
		"cancel":  {err: context.Canceled, cancel: true, want: TransportCallerCancelled},
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
			var transport *TransportError
			var forge *GitHubTransientError
			if !errors.As(err, &transport) || transport.Cause != tc.want || errors.As(err, &forge) {
				t.Fatalf("App transport %s: %v", name, err)
			}
			// The REST adapter resolving the App credential keeps the cause too.
			_, err = GitHubRESTAdapter{Credentials: credential}.token(GitHubRepo{Owner: "o", Name: "r"})
			if transportCause(err) != tc.want && !tc.cancel {
				t.Fatalf("token path erased %s: %v", name, err)
			}
			// And the REST adapter's own HTTP.Do goes through the same function.
			rest := GitHubRESTAdapter{HTTP: credential.HTTP, Credentials: staticCredential{secret: testToken}}
			if _, _, restErr := rest.do(ctx, GitHubRepo{Owner: "o", Name: "r"}, http.MethodGet, "/repos/o/r", nil, nil); !errors.As(restErr, &transport) || transport.Cause != tc.want {
				t.Fatalf("REST transport %s: %v", name, restErr)
			}
			if (failureClassOf(mustMarshal(t, failed(err).result)) == FailureConnectivity) != tc.want.Lost() {
				t.Fatalf("%s: backoff class disagrees with cause", name)
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
		"typographic quote": {claudeResultWithAnswer(true, strings.ReplaceAll(incident, "'", "\u2019")), FailureConnectivity},
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

// RetryNotBefore is purely temporal (#87): reason, refund, external wait and
// observation binding come from the recorded disposition, never the timestamp.
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

	// The same derivations, read directly. A future (#87) disposition carries
	// its own reason; a bare timestamp carries none.
	at := f.clock.Now()
	op := RunOperation{ID: "op", Kind: OpSourceObserve, State: OperationFailed, Attempt: 1, MaxAttempts: 3, RetryNotBefore: at.Add(time.Hour)}
	fold := func(op RunOperation) bool {
		_, open, _ := foldExternalWait([]EngineeringEvent{{Type: EventOperationAfter, OperationID: op.ID, OccurredAt: at, Payload: mustMarshal(t, op)}})
		return !open.IsZero()
	}
	binding := func(op RunOperation) string {
		return observationBinding(&runState{snapshot: RunSnapshot{Operations: map[string]RunOperation{op.ID: op}}}, OpSourceObserve)
	}
	op.IdempotencyKey = "kept"
	if waitReasonOf(op) == ReasonConnectivityBackoff || fold(op) || binding(op) == "kept" {
		t.Fatal("a bare RetryNotBefore was read as a connectivity wait")
	}
	op.RetryDisposition = DispositionAccountWait
	op.Result = mustMarshal(t, mutationResult{FailureClass: FailureProviderAccountUnavailable})
	if waitReasonOf(op) != "execution_provider_account_unavailable" || !fold(op) || binding(op) == "kept" {
		t.Fatal("a provider disposition lost its class's reason or its wait")
	}
	op.Result = nil
	op.RetryDisposition, op.RetryNotBefore = DispositionTransportBackoff, time.Time{}
	op.Attempt = op.MaxAttempts
	if binding(op) != "kept" || fold(op) {
		t.Fatal("the last transport attempt minted a binding or opened a wait")
	}
	for d, s := range retryDispositions {
		if s.ResumeCondition == "" || (s.Reason != "" && externalWaitReasons[s.Reason] == s.SpendsActiveWork) {
			t.Fatalf("%s: incomplete accounting row %+v", d, s)
		}
	}
	// A row with no reason of its own waits under its classes' stated reasons,
	// and those must be external exactly as the row says.
	for class := range waitReasons {
		if s, ok := retryDispositions[retryDispositionFor(class)]; ok && s.Reason == "" && externalWaitReasons[waitReason(class)] == s.SpendsActiveWork {
			t.Fatalf("%s: class reason %q does not match its row's accounting", class, waitReason(class))
		}
	}
	// A reason-less row must not register "": a reason-less run.waiting
	// would then pause the active-work budget.
	if externalWaitReasons[""] {
		t.Fatal("the empty reason is registered as external wait")
	}
	if externalWaitReasons["execution_provider_prerequisite_unavailable"] {
		t.Fatal("a reason nothing produces is registered as external wait")
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

// Endpoint capacity (#87): provider_unavailable keeps its reason and its
// refunded wait, now probed through provider_availability_wait.
func TestConnectivityCapacityKeepsRefundedWait(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderUnavailable}}
	if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_unavailable" {
		t.Fatalf("capacity: %+v", out)
	}
	for _, op := range f.state(runID).snapshot.Operations {
		if op.Kind == OpExecutionInvoke && (op.RetryDisposition != DispositionProviderAvailabilityWait || !op.RetryNotBefore.Equal(f.clock.Now().Add(5*time.Minute))) {
			t.Fatalf("capacity probe: %s", retryRow(op))
		}
	}
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke && op.Attempt != 0 {
			t.Fatalf("capacity attempt not refunded: %+v", op)
		}
	}
}

// retryRow is what the store row and the journal must agree on about a retry.
func retryRow(op RunOperation) string {
	return fmt.Sprintf("%s|%d/%d|%s|%s", op.State, op.Attempt, op.MaxAttempts, op.RetryNotBefore.UTC().Format(time.RFC3339Nano), op.RetryDisposition)
}

// G1: the disposition is persisted on the store row as well as the journal.
func TestConnectivityStoreRowCarriesTheDisposition(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return fmt.Errorf("request: %w", syscall.ENETUNREACH) })
	f.reconcile(runID)
	checked := 0
	for id, journal := range f.state(runID).snapshot.Operations {
		if journal.Kind != OpSourceObserve {
			continue
		}
		stored, _, _, err := f.store.Operation(id)
		if err != nil || journal.RetryDisposition != DispositionTransportBackoff || retryRow(stored) != retryRow(journal) {
			t.Fatalf("store %q != journal %q (%v)", retryRow(stored), retryRow(journal), err)
		}
		checked++
	}
	if checked != 1 {
		t.Fatalf("checked %d observations", checked)
	}
}

// G2(a): store-lag recovery charges the attempt up to its journalled after
// record, not the controller's downtime, and only for the SAME attempt; the
// recovered row carries the journal's retry fields.
func TestConnectivityStoreLagUsesTheJournalledEnd(t *testing.T) {
	for name, tc := range map[string]struct {
		attemptSkew int
		want        time.Duration
	}{
		"same attempt":      {0, time.Second},
		"different attempt": {1, time.Second + time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			_, store := openJournal(t)
			if err := store.PutRun(newJournalRun("run-1")); err != nil {
				t.Fatal(err)
			}
			s, clock := deadlineScheduler(t)
			s.Store = store
			op := plannedExecution(t, s, time.Hour)
			clock.at = clock.at.Add(time.Second)
			after := op
			after.State, after.Lease = OperationFailed, nil
			after.Attempt += tc.attemptSkew
			after.RetryNotBefore, after.RetryDisposition = clock.at.Add(connectivityBackoff(op.Attempt)), DispositionTransportBackoff
			events := []EngineeringEvent{{Type: EventOperationAfter, OperationID: op.ID, OccurredAt: clock.at, Payload: mustMarshal(t, after)}}
			clock.at = clock.at.Add(time.Hour) // the controller was down
			engine := EngineeringRuntime{deps: Dependencies{Store: store}, scheduler: s}
			state := &runState{run: newJournalRun(op.RunID), snapshot: RunSnapshot{Operations: map[string]RunOperation{op.ID: after}}, events: events}
			if err := engine.reconcileStoreLag(state); err != nil {
				t.Fatal(err)
			}
			stored, _, _, err := s.Store.Operation(op.ID)
			if err != nil || stored.ConsumedExecution != tc.want {
				t.Fatalf("consumed %s, want %s (%v)", stored.ConsumedExecution, tc.want, err)
			}
			if stored.RetryDisposition != after.RetryDisposition || !stored.RetryNotBefore.Equal(after.RetryNotBefore) {
				t.Fatalf("store-lag lost the retry fields: %q vs %q", retryRow(stored), retryRow(after))
			}
		})
	}
}

// #87 slice 2: the provider dispositions are produced by exactly the four
// provider wait classes, and every other class keeps its route alone.
func TestConnectivityProviderDispositionsAreProducedByProviderClasses(t *testing.T) {
	want := map[FailureClass]RetryDisposition{
		FailureConnectivity:               DispositionTransportBackoff,
		FailureProviderAccountUnavailable: DispositionAccountWait,
		FailureProviderQuota:              DispositionRateLimitWait,
		FailureProviderRateLimited:        DispositionRateLimitWait,
		FailureProviderUnavailable:        DispositionProviderAvailabilityWait,
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	classes := 0
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			spec, ok := n.(*ast.ValueSpec)
			if !ok {
				return true
			}
			if typ, ok := spec.Type.(*ast.Ident); !ok || typ.Name != "FailureClass" {
				return true
			}
			for _, value := range spec.Values {
				if lit, ok := value.(*ast.BasicLit); ok {
					classes++
					class := FailureClass(strings.Trim(lit.Value, `"`))
					if got := retryDispositionFor(class); got != want[class] {
						t.Errorf("class %s produces %q, want %q", class, got, want[class])
					}
				}
			}
			return true
		})
	}
	if classes < 20 {
		t.Fatalf("found only %d failure classes; the scan is not reading the declarations", classes)
	}
	// Declared for a genuine prerequisite (slice 3), produced by nothing here.
	if s := retryDispositions[DispositionProviderPrerequisiteWait]; s.Delay != nil || s.Reason != "" {
		t.Fatalf("provider_prerequisite_wait was given slice 3's cadence or reason: %+v", s)
	}
	for class, d := range want {
		s := retryDispositions[d]
		if d == DispositionTransportBackoff {
			continue
		}
		// The accounting the frozen table states: refunded, external, no
		// finite attempt authority, a fixed 5 minute probe whatever the attempt.
		if RouteFailure(class) != RouteWait || s.SpendsAttempt || s.SpendsActiveWork || s.FiniteAttemptAuthority || s.Delay == nil || s.Delay(1) != 5*time.Minute || s.Delay(9) != 5*time.Minute ||
			!strings.HasSuffix(s.ResumeCondition, fmt.Sprintf("; probed every %d minutes", providerWaitProbe(1)/time.Minute)) {
			t.Errorf("%s/%s: accounting %+v", class, d, s)
		}
	}
}

// #87: status reports only the retry the run is actually waiting on.
func TestConnectivityRetryStatusIsTheRunsOwnWait(t *testing.T) {
	at := time.Date(2027, 1, 15, 8, 0, 0, 0, time.UTC)
	quota := mustMarshal(t, mutationResult{FailureClass: FailureProviderQuota})
	waitOp := func(id, kind string, created time.Time) RunOperation {
		return RunOperation{ID: id, Kind: kind, State: OperationFailed, MaxAttempts: 2, CreatedAt: created, Result: quota,
			RetryDisposition: DispositionRateLimitWait, RetryNotBefore: created.Add(5 * time.Minute)}
	}
	state := func(disposition Disposition, reason string, ops ...RunOperation) *runState {
		s := &runState{snapshot: RunSnapshot{Operations: map[string]RunOperation{}}}
		s.snapshot.Disposition, s.snapshot.Reason = disposition, reason
		for _, op := range ops {
			s.snapshot.Operations[op.ID] = op
		}
		return s
	}
	stale := waitOp("a", OpExecutionInvoke, at)
	later := RunOperation{ID: "b", Kind: OpExecutionInvoke, State: Succeeded, CreatedAt: at.Add(time.Hour)}
	if got := retryStatus(state(Waiting, "awaiting_authority", stale, later)); got != nil {
		t.Fatalf("a superseded provider wait masked the authority wait: %+v", got)
	}
	// Same reason, but the binding moved on: still history.
	if got := retryStatus(state(Waiting, "execution_provider_quota", stale, later)); got != nil {
		t.Fatalf("a superseded provider wait was reported: %+v", got)
	}
	// Still the latest of its kind, but the run waits on something else.
	if got := retryStatus(state(Waiting, "awaiting_authority", stale)); got != nil {
		t.Fatalf("a provider wait masked the run's own reason: %+v", got)
	}
	if got := retryStatus(state(Failed, "execution_provider_quota", stale)); got != nil {
		t.Fatalf("a run that is not waiting reported a probe: %+v", got)
	}
	newer := waitOp("c", OpSourceObserve, at.Add(time.Minute))
	got := retryStatus(state(Waiting, "execution_provider_quota", stale, newer))
	if got == nil || got.Operation != "c" || !got.NotBefore.Equal(newer.RetryNotBefore) || got.ResumeCondition != "the provider allowance returns; probed every 5 minutes" {
		t.Fatalf("two awaiting operations did not report the latest: %+v", got)
	}
}

// #87 (frozen): restoring the account does not bypass RetryNotBefore.
// `autonomy resume` is exactly Reconcile, so it is no hidden --now.
func TestConnectivityResumeDoesNotBringTheProbeForward(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderAccountUnavailable}}
	if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_account_unavailable" {
		t.Fatalf("account: %+v", out)
	}
	waiting := executionOperation(t, f.store, runID)
	calls := len(f.provider.requests)
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded} // the operator restored it
	f.clock.at = waiting.RetryNotBefore.Add(-time.Second)
	out, err := f.runtime.Reconcile(context.Background(), runID) // what `autonomy resume` runs
	if err != nil || out.Disposition != Waiting || out.Reason != "execution_provider_account_unavailable" || len(f.provider.requests) != calls {
		t.Fatalf("resume brought the probe forward: %+v calls=%d %v", out, len(f.provider.requests)-calls, err)
	}
	if after := executionOperation(t, f.store, runID); !after.RetryNotBefore.Equal(waiting.RetryNotBefore) {
		t.Fatalf("resume moved the probe: %s -> %s", waiting.RetryNotBefore, after.RetryNotBefore)
	}
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	if after := executionOperation(t, f.store, runID); after.ID != waiting.ID || after.State != Succeeded || len(f.provider.requests) != calls+1 {
		t.Fatalf("the probe did not resume the same operation: %s", retryRow(after))
	}
}

// #87 (frozen): a recognized provider wait refunds the attempt always, and the
// execution time only when the provider did not run (ProviderExecuted=false).
func TestConnectivityProviderWaitRefundsExecutionOnlyWhenNothingRan(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want time.Duration
	}{
		"provider ran":          {nil, 10 * time.Minute},
		"refused before it ran": {errors.New("refused before dispatch"), 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPhase8Fixture(t)
			runID := f.start()
			f.clock.step = 0
			f.provider.mutate = func(string) error { f.clock.at = f.clock.at.Add(10 * time.Minute); return nil }
			f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderQuota}}
			f.provider.Err = tc.err
			if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_quota" {
				t.Fatalf("quota: %+v", out)
			}
			op := executionOperation(t, f.store, runID)
			if op.Attempt != 0 || op.ConsumedExecution != tc.want || op.RetryDisposition != DispositionRateLimitWait {
				t.Fatalf("attempt %d consumed %s (want 0, %s): %s", op.Attempt, op.ConsumedExecution, tc.want, retryRow(op))
			}
		})
	}
}

// #87 slice 2: a quota wait is durable. Its probe time survives a store
// reopen and a new runtime, nothing invokes the provider before it, no attempt
// or active work is spent, and the SAME operation resumes once it passes.
func TestConnectivityProviderQuotaWaitIsDurable(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureProviderQuota}}
	if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_quota" {
		t.Fatalf("quota: %+v", out)
	}
	var waiting RunOperation
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if op.Kind == OpExecutionInvoke {
			waiting = op
		}
	}
	if waiting.State != OperationFailed || waiting.Attempt != 0 || waiting.RetryDisposition != DispositionRateLimitWait ||
		!waiting.RetryNotBefore.Equal(f.clock.Now().Add(5*time.Minute)) {
		t.Fatalf("quota wait not durable or not refunded: %s", retryRow(waiting))
	}
	report, err := f.runtime.Status(runID)
	if err != nil || report.Retry == nil || report.Retry.Operation != waiting.ID || !report.Retry.NotBefore.Equal(waiting.RetryNotBefore) ||
		report.Retry.Disposition != DispositionRateLimitWait || report.Retry.ResumeCondition != "the provider allowance returns; probed every 5 minutes" {
		t.Fatalf("status: %+v %v", report.Retry, err)
	}
	calls := len(f.provider.requests)
	active := f.state(runID).activeElapsed(f.clock.Now())

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
		if out := f.reconcile(runID); out.Disposition != Waiting || out.Reason != "execution_provider_quota" {
			t.Fatalf("early probe: %+v", out)
		}
	}
	stored, _, _, err := f.store.Operation(waiting.ID)
	if err != nil || !reflect.DeepEqual(stored, waiting) || len(f.provider.requests) != calls {
		t.Fatalf("restart re-probed early: calls %d->%d %s / %s %v", calls, len(f.provider.requests), retryRow(waiting), retryRow(stored), err)
	}
	if got := f.state(runID).activeElapsed(f.clock.Now()); got != active {
		t.Fatalf("the wait was charged as active work: %s -> %s", active, got)
	}

	f.provider.Result = ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	recovered, _, _, err := f.store.Operation(waiting.ID)
	if err != nil || recovered.State != Succeeded || recovered.Attempt != 1 || !recovered.RetryNotBefore.IsZero() || recovered.RetryDisposition != "" || len(f.provider.requests) != calls+1 {
		t.Fatalf("same operation did not resume: %s calls=%d %v", retryRow(recovered), len(f.provider.requests)-calls, err)
	}
}

// G5: an http.Client.Timeout is a deadline the caller set, not transport loss.
func TestConnectivityClientTimeoutIsCallerCancelled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			defer conn.Close() // accept and never answer
		}
	}()
	_, err = (&http.Client{Timeout: 100 * time.Millisecond}).Get("http://" + listener.Addr().String())
	if cause := transportCause(err); cause != TransportCallerCancelled {
		t.Fatalf("client timeout %v classified %q", err, cause)
	}
}
