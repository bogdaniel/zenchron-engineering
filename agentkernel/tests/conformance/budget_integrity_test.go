package conformance

import (
	"context"
	"runtime"
	"strings"
	"testing"

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
