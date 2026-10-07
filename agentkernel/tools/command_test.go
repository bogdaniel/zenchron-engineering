package tools

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

func commandFixture(t *testing.T, runner *fakeRunner) (*Broker, Env, *storage.MemoryArtifacts) {
	t.Helper()
	cmd, err := NewCommand(runner, "/work/space")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBroker(cmd)
	if err != nil {
		t.Fatal(err)
	}
	store, err := storage.NewMemoryArtifacts(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	g := api.Capability{Handle: "tests", Kind: api.CapabilityCommand, Commands: []api.CommandGrant{
		{Name: "unit", Argv: []string{"go", "test", "./..."}, TimeoutSeconds: 30},
	}}
	return b, Env{Grants: []api.Capability{g}, Mode: api.ModeReadWrite, Artifacts: store, Producer: "e/a/c", OutputLimit: 40}, store
}

func TestCommandArgvAndTimeoutComeOnlyFromGrant(t *testing.T) {
	runner := &fakeRunner{result: api.CommandResult{ExitCode: 0, Stdout: []byte("ok")}}
	b, env, _ := commandFixture(t, runner)
	refused := []string{
		`{"command":"unit","argv":["rm","-rf","/"]}`,
		`{"command":"unit","timeout_seconds":99999}`,
		`{"command":"unit","grant":"tests"}`,
		`{"command":"rm"}`,
		`{"command":["unit"]}`,
	}
	for _, args := range refused {
		res, err := b.Dispatch(context.Background(), call("run_command", args), env)
		if err != nil || res.Status != api.ToolRefused {
			t.Errorf("%s: %+v, %v; want refused", args, res, err)
		}
	}
	if len(runner.requests) != 0 {
		t.Fatalf("runner reached by refused calls: %+v", runner.requests)
	}
	res, err := b.Dispatch(context.Background(), call("run_command", `{"command":"unit"}`), env)
	if err != nil || res.Status != api.ToolOK || res.Grant != "tests" || !res.Mutated {
		t.Fatalf("granted command: %+v, %v", res, err)
	}
	req := runner.requests[0]
	if !slices.Equal(req.Argv, []string{"go", "test", "./..."}) || req.Timeout != 30*time.Second || req.Dir != "/work/space" {
		t.Fatalf("runner request %+v", req)
	}
	env.Mode = api.ModeReadOnly
	if res, _ := b.Dispatch(context.Background(), call("run_command", `{"command":"unit"}`), env); res.Status != api.ToolRefused {
		t.Fatalf("read_only ran a command: %+v", res)
	}
	if specs := b.Specs(env.Grants, api.ModeReadOnly); len(specs) != 0 {
		t.Fatalf("read_only offers %v", specs)
	}
}

func TestCommandFailurePreservesExitStderrAndArtifact(t *testing.T) {
	stderr := strings.Repeat("FAIL: TestX\n", 20)
	runner := &fakeRunner{result: api.CommandResult{ExitCode: 2, Stdout: []byte("partial"), Stderr: []byte(stderr), Truncated: true}}
	b, env, store := commandFixture(t, runner)
	res, err := b.Dispatch(context.Background(), call("run_command", `{"command":"unit"}`), env)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != api.ToolError || res.ExitCode == nil || *res.ExitCode != 2 || !res.Truncated || len(res.Output) > 40 {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(res.Error, "exit status 2") || !strings.Contains(res.Error, "runner truncated") {
		t.Fatalf("error %q", res.Error)
	}
	full, err := store.Get(context.Background(), *res.FullOutput)
	if err != nil || !strings.Contains(string(full), stderr) || res.FullOutput.Digest != api.Digest(full) {
		t.Fatalf("artifact: %v", err)
	}
}

func TestCommandUnknownOutcomes(t *testing.T) {
	runner := &fakeRunner{err: errors.New("killed")}
	b, env, _ := commandFixture(t, runner)
	if res, err := b.Dispatch(context.Background(), call("run_command", `{"command":"unit"}`), env); err == nil || !res.Mutated {
		t.Fatalf("runner failure must be an unknown outcome: %+v, %v", res, err)
	}
	runner.err = nil
	env.Artifacts = failingStore{}
	if _, err := b.Dispatch(context.Background(), call("run_command", `{"command":"unit"}`), env); err == nil {
		t.Fatal("recording failure after a command must be an unknown outcome")
	}
}

func TestNewCommandValidatesConstruction(t *testing.T) {
	if _, err := NewCommand(nil, "/x"); err == nil {
		t.Fatal("nil runner accepted")
	}
	if _, err := NewCommand(&fakeRunner{}, "rel"); err == nil {
		t.Fatal("relative dir accepted")
	}
}
