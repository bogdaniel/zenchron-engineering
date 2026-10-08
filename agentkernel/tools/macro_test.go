package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestReadFilesRunsStepsInOrder(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "AAA\n", "b.txt": "BBB\n"})
	env := f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))
	res, err := f.broker.Dispatch(context.Background(), call("read_files", `{"paths":["a.txt","b.txt"]}`), env)
	if err != nil || res.Status != api.ToolOK || res.Grant != "r" {
		t.Fatal(res, err)
	}
	if !strings.Contains(res.Output, "AAA") || strings.Index(res.Output, "AAA") > strings.Index(res.Output, "BBB") {
		t.Fatalf("output %q", res.Output)
	}
}

func TestReadFilesStopsAtFirstFailureWithCompletedSteps(t *testing.T) {
	f := newFixture(t, map[string]string{"a.txt": "AAA\n", "c.txt": "CCC\n"})
	env := f.env(api.ModeReadOnly, grant("r", api.CapabilityFileRead, "."))
	res, err := f.broker.Dispatch(context.Background(), call("read_files", `{"paths":["a.txt","missing.txt","c.txt"]}`), env)
	if err != nil || res.Status != api.ToolError {
		t.Fatal(res, err)
	}
	if !strings.Contains(res.Output, "AAA") || strings.Contains(res.Output, "CCC") || !strings.Contains(res.Error, "step 1") {
		t.Fatalf("partial result %+v", res)
	}
}

func TestReadFilesRechecksCapabilityAndBudgetPerStep(t *testing.T) {
	f := newFixture(t, map[string]string{"a/x.txt": "ax\n", "b/x.txt": "bx\n"})
	macro, err := f.ws.ReadFiles()
	if err != nil {
		t.Fatal(err)
	}
	// Invoked directly, bypassing the outer broker: each step must still be
	// admitted under the call's grant, which covers only "a".
	inv := Invocation{
		Call: api.ToolCall{ID: "m"}, Grant: grant("ga", api.CapabilityFileRead, "a"), Artifacts: f.store,
		Producer: "p", OutputLimit: 1000, Arguments: []byte(`{"paths":["b/x.txt"]}`),
	}
	res, err := macro.Invoke(context.Background(), inv)
	if err != nil || res.Status != api.ToolError || strings.Contains(res.Output, "bx") {
		t.Fatalf("step outside grant ran: %+v, %v", res, err)
	}
	// The first step rides on the call's own budget; the next is charged and
	// refused, so exactly one step completes.
	charges := 0
	inv.StepBudget = func(context.Context) error {
		charges++
		return errors.New("tool budget exhausted")
	}
	inv.Arguments = []byte(`{"paths":["a/x.txt","a/x.txt","a/x.txt"]}`)
	res, err = macro.Invoke(context.Background(), inv)
	if err != nil || res.Status != api.ToolError || !strings.Contains(res.Error, "step 1") ||
		!strings.Contains(res.Error, "budget") || charges != 1 || strings.Count(res.Output, "ax") != 1 {
		t.Fatalf("step budget not honoured: %+v, %v, charges %d", res, err, charges)
	}
}
