package conformance

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// TestA05ReentryAcrossRestartCannotRenewBudget: with a durable admission
// store, a restarted kernel refuses to re-run an admitted attempt, and a new
// attempt of the same execution spends only what earlier attempts left.
func TestA05ReentryAcrossRestartCannotRenewBudget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("file-backed stores are not supported on windows in Gate A")
	}
	root := t.TempDir()
	open := func() *storage.FileRecords {
		r, err := storage.OpenFileRecords(root)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	read := func(id string) scripted.Step {
		return toolUse(call(id, "read_file", map[string]any{"path": "notes.txt"}))
	}
	req := request("a05-reentry")
	req.Budget.MaxIterations = 3

	before := scripted.New(read("c1"), end("first attempt done"))
	k := newKernel(t, config{providers: providers(before), admissions: open()})
	want(t, k.run(t, context.Background(), req), api.OutcomeCompleted, api.CauseLoopCompleted)

	// Restart: a new kernel over the same admission store.
	after := scripted.New(read("c2"), read("c3"), end("never"))
	k = newKernel(t, config{providers: providers(after), admissions: open()})
	res := k.run(t, context.Background(), req)
	want(t, res, api.OutcomeBlocked, api.CauseInvalidRequest)
	if !strings.Contains(res.Termination.Detail, "already admitted") || len(after.Requests()) != 0 {
		t.Fatalf("re-entered attempt: detail %q, %d provider calls", res.Termination.Detail, len(after.Requests()))
	}
	req.AttemptID = "att-2"
	res, _ = k.engine.Execute(context.Background(), req) // same sink: k.run's single-settlement check does not apply
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionIterations || len(after.Requests()) != 1 {
		t.Fatalf("second attempt: dimension %q after %d calls; 2 of 3 iterations were already spent",
			res.Termination.Dimension, len(after.Requests()))
	}
}

// TestA05DeadlineIsAttemptScoped (v0.2): a new attempt of an admitted
// execution may carry a later deadline and starts from what earlier attempts
// consumed; the same attempt with a later deadline is still refused, and a
// later deadline does not let a cumulative bound widen.
func TestA05DeadlineIsAttemptScoped(t *testing.T) {
	read := func(id string) scripted.Step {
		return toolUse(call(id, "read_file", map[string]any{"path": "notes.txt"}))
	}
	provider := scripted.New(read("c1"), end("first attempt done"), read("c2"), end("never"))
	k := newKernel(t, config{providers: providers(provider)})
	req := request("a05-deadline")
	req.Budget.MaxIterations = 3
	want(t, k.run(t, context.Background(), req), api.OutcomeCompleted, api.CauseLoopCompleted)

	later := req
	later.Budget.Deadline = req.Budget.Deadline.Add(time.Hour)
	res, _ := k.engine.Execute(context.Background(), later) // same sink: k.run's single-settlement check does not apply
	if res.Termination.Outcome != api.OutcomeBlocked || !strings.Contains(res.Termination.Detail, "already admitted") {
		t.Fatalf("same attempt with a later deadline: %+v", res.Termination)
	}
	widened := later
	widened.AttemptID, widened.Budget.MaxToolCalls = "att-2", req.Budget.MaxToolCalls+1
	res, _ = k.engine.Execute(context.Background(), widened)
	if res.Termination.Outcome != api.OutcomeBlocked || !strings.Contains(res.Termination.Detail, "widens max_tool_calls") {
		t.Fatalf("later deadline with a widened tool-call bound: %+v", res.Termination)
	}
	later.AttemptID = "att-3"
	res, _ = k.engine.Execute(context.Background(), later)
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionIterations || len(provider.Requests()) != 3 {
		t.Fatalf("new attempt with a later deadline: dimension %q after %d calls; 2 of 3 iterations were already spent",
			res.Termination.Dimension, len(provider.Requests()))
	}
}
