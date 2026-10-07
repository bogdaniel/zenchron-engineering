package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// A macro must be metered like the calls it performs: each step beyond the
// first is a tool call against the budget, so a three-step macro under a
// two-call budget stops at its third step.
func TestMacroStepsAreChargedToTheToolBudget(t *testing.T) {
	args, _ := json.Marshal(map[string][]string{"paths": {"a.txt", "a.txt", "a.txt"}})
	macro := api.ToolCall{ID: "m1", Name: "read_files", Arguments: args}
	f := newFixture(t, []scripted.Step{toolUse(macro), end("done")})
	ws, err := tools.NewWorkspace(f.dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ws.ReadFiles(nil)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(ws.ReadFile(), read)
	if err != nil {
		t.Fatal(err)
	}
	f.engine, err = engine.New(engine.Config{
		Providers: map[string]api.Provider{"p1": f.provider}, Broker: broker, Artifacts: f.artifacts,
		Events: f.sink, Clock: f.clock, OutputLimit: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	req := request()
	req.Budget.MaxToolCalls = 2
	res := f.run(t, context.Background(), req)
	if res.Usage.ToolCalls != 2 {
		t.Fatalf("tool calls = %d, want 2 (macro call plus one charged step)", res.Usage.ToolCalls)
	}
	reqs := f.provider.Requests()
	last := reqs[len(reqs)-1].Messages
	if msg := last[len(last)-1].Content; !strings.Contains(msg, "budget") {
		t.Fatalf("macro result does not report the budget stop: %s", msg)
	}
}
