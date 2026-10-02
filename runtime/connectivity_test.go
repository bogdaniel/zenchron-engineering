package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"
)

func TestConnectivityClassificationIsClosed(t *testing.T) {
	for _, err := range []error{&net.DNSError{Err: "no such host"}, fmt.Errorf("wrapped: %w", syscall.ENETUNREACH)} {
		if !transientConnectivity(err) {
			t.Fatalf("not classified: %v", err)
		}
	}
	for _, err := range []error{errors.New("unknown"), errors.New("ENOTFOUND"), context.Canceled} {
		if transientConnectivity(err) {
			t.Fatalf("guessed: %v", err)
		}
	}
	for _, attempt := range []int{1, 2, 3, 4, 1000000} {
		delay := connectivityBackoff(attempt)
		if delay < 30*time.Second || delay > 5*time.Minute {
			t.Fatal(delay)
		}
	}
}

func TestClaudeObservedConnectivityEnvelope(t *testing.T) {
	const diagnostic = "API Error: Can't reach the API server — check your internet or DNS (ENOTFOUND)"
	for _, tc := range []struct {
		isError bool
		text    string
		want    FailureClass
	}{
		{true, diagnostic, FailureProviderUnavailable},
		{false, diagnostic, FailureUnknown},
		{true, "quoted: " + diagnostic, FailureUnknown},
		{true, "API Error: unknown", FailureUnknown},
	} {
		provider, request, fake := agentFixture(t, AgentKindClaudeCode)
		fake.outputs = []CommandOutput{{Stdout: []byte(claudeResultWithAnswer(tc.isError, tc.text) + "\n")}}
		result, err := provider.Execute(context.Background(), request)
		if err != nil {
			t.Fatal(err)
		}
		class := FailureUnknown
		if result.Failure != nil {
			class = result.Failure.Classification
		}
		if class != tc.want {
			t.Fatalf("%+v: %s", tc, class)
		}
	}
}

func TestConnectivityBackoffSurvivesRestartAndExhausts(t *testing.T) {
	f, p := newRoutingFixture(t, 2, classifiedFailure(FailureProviderUnavailable))
	f.clock.step = 0
	id := f.start()
	first := f.reconcile(id)
	if first.Disposition != Waiting {
		t.Fatal(first)
	}
	op, _ := durableInvoke(t, f, id)
	if op.Attempt != 1 || op.RetryNotBefore.IsZero() {
		t.Fatalf("%+v", op)
	}
	before := f.state(id)
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
	for i := 0; i < 5; i++ {
		f.reconcile(id)
	}
	if p.calls != 1 {
		t.Fatalf("early calls: %d", p.calls)
	}
	after := f.state(id)
	if before.projection.Contract != after.projection.Contract || before.baseRevision() != after.baseRevision() {
		t.Fatal("bindings changed")
	}
	f.clock.at = op.RetryNotBefore.Add(-time.Nanosecond)
	f.reconcile(id)
	if p.calls != 1 {
		t.Fatal("retried before deadline")
	}
	f.clock.at = op.RetryNotBefore
	outcome := f.reconcile(id)
	if p.calls != 2 || outcome.Disposition != Failed || outcome.Reason != OpExecutionInvoke+attemptsExhaustedSuffix {
		t.Fatalf("%d %+v", p.calls, outcome)
	}
}

func TestConnectivityBackoffRecovers(t *testing.T) {
	f, p := newRoutingFixture(t, 3, classifiedFailure(FailureProviderUnavailable), providerAnswer{result: ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}, mutate: writesCandidate})
	f.clock.step = 0
	id := f.start()
	f.reconcile(id)
	op, _ := durableInvoke(t, f, id)
	before := f.state(id)
	active := before.activeElapsed(f.clock.Now())
	f.clock.at = op.RetryNotBefore.Add(time.Hour)
	waiting := f.state(id)
	if got := waiting.activeElapsed(f.clock.Now()); got != active {
		t.Fatalf("wait charged: %v vs %v", got, active)
	}
	f.reconcile(id)
	if p.calls != 2 || countType(f.state(id).events, EventCandidateCommitted) != 1 {
		t.Fatal("did not recover")
	}
}

func TestSourceConnectivityKeepsFiniteOperationAcrossEpochs(t *testing.T) {
	f := newPhase8Fixture(t)
	f.clock.step = 0
	id := f.start()
	calls := 0
	f.forge.Fail = func(call GitHubCall) error { calls++; return &net.DNSError{Err: "no such host"} }
	var firstID string
	for attempt := 1; attempt <= 3; attempt++ {
		outcome := f.reconcile(id)
		state := f.state(id)
		var pending RunOperation
		for _, op := range state.snapshot.Operations {
			if op.Kind == OpSourceObserve {
				pending = op
			}
		}
		if firstID == "" {
			firstID = pending.ID
		}
		if pending.ID != firstID || pending.Attempt != attempt {
			t.Fatalf("new budget: %+v", pending)
		}
		if attempt == 3 {
			if outcome.Disposition != Failed || calls != 3 {
				t.Fatalf("%+v calls=%d", outcome, calls)
			}
			break
		}
		if outcome.Disposition != Waiting {
			t.Fatal(outcome)
		}
		f.runtime = f.newRuntime(f.deps)
		f.reconcile(id)
		if calls != attempt {
			t.Fatal("early observation retry")
		}
		f.clock.at = pending.RetryNotBefore
	}
}

// A provider may leave valuable uncommitted material before losing transport.
// Neither waiting nor exhaustion can promote it to a completed candidate.
func TestConnectivityPreservesPartialMaterial(t *testing.T) {
	answer := classifiedFailure(FailureProviderUnavailable)
	answer.mutate = writesCandidate
	f, p := newRoutingFixture(t, 2, answer)
	f.clock.step = 0
	id := f.start()
	f.reconcile(id)
	op, _ := durableInvoke(t, f, id)
	before := f.state(id)
	if op.State != OperationFailed || countType(before.events, EventCandidateCommitted) != 0 {
		t.Fatal("partial failure promoted")
	}
	var result mutationResult
	if err := decodeJSON(op.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Mutated || result.ContentDigest == "" {
		t.Fatal("missing material identity")
	}
	f.reconcile(id)
	same, _ := durableInvoke(t, f, id)
	if string(same.Result) != string(op.Result) || p.calls != 1 {
		t.Fatal("wait changed material record")
	}
	f.clock.at = op.RetryNotBefore
	out := f.reconcile(id)
	if out.Disposition != Failed {
		t.Fatal(out)
	}
	state := f.state(id)
	if countType(state.events, EventCandidateCommitted) != 0 {
		t.Fatal("exhaustion promoted material")
	}
	if state.snapshot.HeldMaterial == nil {
		t.Fatal("exhaustion hid held material")
	}
}

func TestConnectivityJournalClosesCrashAccountingGap(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	raw, err := marshalPayloadJSON(RunOperation{State: OperationFailed, RetryNotBefore: at.Add(30 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	events := []EngineeringEvent{{Type: EventOperationAfter, OccurredAt: at, OperationID: "op", Payload: raw}}
	excluded, since, work := foldExternalWait(events)
	if excluded != 0 || !since.Equal(at) || work != 0 {
		t.Fatalf("%v %v %v", excluded, since, work)
	}
}
