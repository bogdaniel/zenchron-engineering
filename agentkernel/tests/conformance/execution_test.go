package conformance

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// TestA01StandaloneScriptedExecution: a consumer builds the kernel from its
// own packages only (no Zenchron object, configuration or database), runs a
// multi-step scripted task and gets one settled, observation-only result
// with resolvable artifact references.
func TestA01StandaloneScriptedExecution(t *testing.T) {
	p := scripted.New(
		toolUse(call("c1", "read_file", map[string]any{"path": "notes.txt"})),
		toolUse(call("c2", "write_file", map[string]any{"path": "out/summary.txt", "content": "greek letters\n", "expected_sha256": "absent"})),
		end("wrote out/summary.txt"),
	)
	k := newKernel(t, config{providers: providers(p)})
	res := k.run(t, context.Background(), request("a01"))
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	if res.FinalText != "wrote out/summary.txt" || res.Usage.ToolCalls != 2 || res.Usage.ProviderCalls != 3 {
		t.Fatalf("result %+v", res.Usage)
	}
	if k.sink.count(api.EventSettled) != 1 {
		t.Fatal("not settled exactly once")
	}
	for _, ref := range res.Artifacts {
		if _, err := k.artifacts.Get(context.Background(), ref); err != nil {
			t.Fatalf("artifact %s unresolvable: %v", ref.Digest, err)
		}
	}
	if res.Provenance.ProviderID != "p1" || res.Provenance.Isolation != api.IsolationUnproven {
		t.Fatalf("provenance %+v", res.Provenance)
	}
}

// TestA04RefusedBeforeAnySideEffect: unsupported version, feature or
// capability is refused before any provider call, context source query or
// tool side effect.
func TestA04RefusedBeforeAnySideEffect(t *testing.T) {
	cases := map[string]struct {
		mutate func(*api.ExecutionRequest)
		cause  api.Cause
	}{
		"unknown version": {func(r *api.ExecutionRequest) { r.Version = "agentkernel.execution/v9" }, api.CauseInvalidRequest},
		"v0.1 version":    {func(r *api.ExecutionRequest) { r.Version = "agentkernel.execution/v0.1" }, api.CauseInvalidRequest},
		"unsupported feature": {func(r *api.ExecutionRequest) {
			r.Constraints.RequiredFeatures = []string{"streaming"}
		}, api.CauseInvalidRequest},
		"unknown capability kind": {func(r *api.ExecutionRequest) {
			r.Grants = append(r.Grants, api.Capability{Handle: "net", Kind: "network.fetch", Roots: []string{"."}})
		}, api.CauseInvalidRequest},
		"write grant in read-only mode": {func(r *api.ExecutionRequest) { r.Mode = api.ModeReadOnly }, api.CauseInvalidRequest},
		"duplicate grant handle": {func(r *api.ExecutionRequest) {
			r.Grants = append(r.Grants, api.Capability{Handle: "read-all", Kind: api.CapabilityFileRead, Roots: []string{"."}})
		}, api.CauseInvalidRequest},
		"unenforceable money ceiling": {func(r *api.ExecutionRequest) {
			r.Budget.Money = &api.MoneyCeiling{Currency: "USD", MaxMicros: 1000}
		}, api.CauseInvalidRequest},
		"provider lacks mandatory capability": {func(r *api.ExecutionRequest) {
			r.Constraints.RequiredFeatures = []string{api.FeatureTools}
			r.Providers[0].Features = nil
		}, api.CauseNoEligibleProvider},
		"host-proven isolation unavailable": {func(r *api.ExecutionRequest) {
			r.Constraints.RequireHostProvenIsolation = true
		}, api.CauseNoEligibleProvider},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := scripted.New(toolUse(call("c1", "write_file", map[string]any{
				"path": "out/x.txt", "content": "x", "expected_sha256": "absent"})), end("done"))
			src := &countingSource{}
			k := newKernel(t, config{providers: providers(p), sources: []api.ContextSource{src}})
			before := treeDigest(t, k.dir)
			req := request("a04")
			c.mutate(&req)
			res := k.run(t, context.Background(), req)
			want(t, res, api.OutcomeBlocked, c.cause)
			if n := len(p.Requests()); n != 0 {
				t.Fatalf("provider received %d requests before refusal", n)
			}
			if src.asked() != 0 {
				t.Fatal("context source queried before refusal")
			}
			if treeDigest(t, k.dir) != before {
				t.Fatal("workspace changed")
			}
			if k.sink.count(api.EventToolProposed)+k.sink.count(api.EventToolExecuted) != 0 || len(res.Artifacts) != 0 {
				t.Fatal("tool activity before refusal")
			}
		})
	}
}

// TestA04StrictDecodeRefusesAmbiguousJSON: unknown fields, duplicate keys and
// case-variant keys never reach validation.
func TestA04StrictDecodeRefusesAmbiguousJSON(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown field": `{"version":"agentkernel.execution/v0.2","authority":"accept"}`,
		"duplicate key": `{"version":"agentkernel.execution/v0.2","version":"x"}`,
		"case variant":  `{"version":"agentkernel.execution/v0.2","Mode":"read_write"}`,
		"trailing data": `{"version":"agentkernel.execution/v0.2"} {}`,
	} {
		if _, err := api.DecodeRequest([]byte(doc)); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// loopingProvider proposes one read forever and reports usage, so only the
// kernel's bounds can end an execution.
type loopingProvider struct {
	mu    sync.Mutex
	calls int
}

func (p *loopingProvider) Complete(ctx context.Context, _ api.ProviderRequest) (api.ProviderResponse, error) {
	p.mu.Lock()
	p.calls++
	n := p.calls
	p.mu.Unlock()
	return api.ProviderResponse{
		Stop: api.StopToolUse, Usage: usage(50, 40),
		ToolCalls: []api.ToolCall{call(fmt.Sprintf("c%d", n), "read_file", map[string]any{"path": "notes.txt"})},
	}, nil
}

// TestA05ConcurrentExecutionsCannotOverspend: executions sharing one engine
// and provider each stay inside their own envelope under -race.
func TestA05ConcurrentExecutionsCannotOverspend(t *testing.T) {
	p := &loopingProvider{}
	k := newKernel(t, config{providers: providers(p)})
	const n = 16
	results := make([]api.ExecutionResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := request(fmt.Sprintf("a05-%d", i))
			req.Budget.MaxIterations, req.Budget.MaxToolCalls, req.Budget.MaxOutputTokens = 4, 3, 1000
			res, err := k.engine.Execute(context.Background(), req)
			if err != nil {
				t.Error(err)
			}
			results[i] = res
		}()
	}
	wg.Wait()
	total := 0
	for i, res := range results {
		u := res.Usage
		if res.Termination.Outcome != api.OutcomeExhausted {
			t.Errorf("%d: termination %+v", i, res.Termination)
		}
		if u.ToolCalls > 3 || u.Iterations > 4 || u.ProviderCalls > 4 {
			t.Errorf("%d: overspent: %+v", i, u)
		}
		if u.Reported.Output == nil || *u.Reported.Output > 1000 {
			t.Errorf("%d: output tokens %v over budget", i, u.Reported.Output)
		}
		total += u.ProviderCalls
	}
	if total != p.calls {
		t.Fatalf("results account %d provider calls, provider served %d", total, p.calls)
	}
}

// TestA05RetriesNeverRenewBudget: a retryable failure is retried only while
// the request's retry allowance lasts; each retry spends from the same
// envelope.
func TestA05RetriesNeverRenewBudget(t *testing.T) {
	cases := map[string]struct {
		err   *api.ProviderError
		cause api.Cause
	}{
		"server":       {&api.ProviderError{Class: api.ProviderServer, Status: 500, Retryable: true}, api.CauseProviderFailed},
		"rate limited": {&api.ProviderError{Class: api.ProviderRateLimited, Status: 429, Retryable: true}, api.CauseProviderUnavailable},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			steps := make([]scripted.Step, 10)
			for i := range steps {
				steps[i] = scripted.Step{Err: c.err}
			}
			p := scripted.New(steps...)
			k := newKernel(t, config{providers: providers(p)})
			req := request("a05-retry")
			req.Budget.MaxProviderRetries = 2
			res := k.run(t, context.Background(), req)
			if res.Termination.Cause != c.cause {
				t.Fatalf("termination %+v", res.Termination)
			}
			if got := len(p.Requests()); got != 3 || res.Usage.Retries != 2 || res.Usage.Iterations != 1 {
				t.Fatalf("provider calls %d, usage %+v", got, res.Usage)
			}
			if res.Usage.Reported.Input != nil || res.Usage.Cost.Known {
				t.Fatalf("failed calls' usage must stay unknown: %+v", res.Usage)
			}
		})
	}
}

// TestA06ObservedTerminationSurvivesLateCancel: a provider completion or
// failure observed before a cancellation is what the result settles, and no
// goroutine outlives Execute.
func TestA06ObservedTerminationSurvivesLateCancel(t *testing.T) {
	cases := map[string]struct {
		step    scripted.Step
		outcome api.Outcome
		cause   api.Cause
	}{
		"completion": {end("done"), api.OutcomeCompleted, api.CauseLoopCompleted},
		"failure": {scripted.Step{Err: &api.ProviderError{Class: api.ProviderAuth, Status: 401}},
			api.OutcomeFailed, api.CauseProviderFailed},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			c.step.Before = func() { cancel(api.Cancellation(api.CancelControllerShutdown)) }
			k := newKernel(t, config{providers: providers(scripted.New(c.step))})
			// Counted after newKernel: its port workers are the host's goroutines.
			before := runtime.NumGoroutine()
			res := k.run(t, ctx, request("a06"))
			want(t, res, c.outcome, c.cause)
			if res.Termination.Cancellation != "" {
				t.Fatalf("late cancellation rewrote termination: %+v", res.Termination)
			}
			waitGoroutines(t, before)
		})
	}
}

// TestA06CancelInFlightIsTypedAndReleased: a cancellation during a provider
// call settles cancelled with the host's provenance, and a bare cancel stays
// unknown rather than guessed.
func TestA06CancelInFlightIsTypedAndReleased(t *testing.T) {
	for name, tc := range map[string]struct {
		cause error
		want  api.CancellationProvenance
	}{
		"operator": {api.Cancellation(api.CancelOperatorStop), api.CancelOperatorStop},
		"bare":     {nil, api.CancelUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			entered := make(chan struct{})
			p := scripted.New(scripted.Step{Block: true, Entered: entered})
			k := newKernel(t, config{providers: providers(p)})
			before := runtime.NumGoroutine()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan api.ExecutionResult, 1)
			go func() { done <- k.run(t, ctx, request("a06-inflight")) }()
			<-entered
			cancel(tc.cause)
			var res api.ExecutionResult
			select {
			case res = <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("execution did not settle after cancellation")
			}
			want(t, res, api.OutcomeCancelled, api.CauseHostCancelled)
			if res.Termination.Cancellation != tc.want {
				t.Fatalf("cancellation provenance %q, want %q", res.Termination.Cancellation, tc.want)
			}
			waitGoroutines(t, before)
		})
	}
}

// waitGoroutines allows goroutines that are already exiting to finish, with
// a bounded wait, then fails if any remain.
func waitGoroutines(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d -> %d", before, runtime.NumGoroutine())
		}
		runtime.Gosched()
	}
}
