package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// NewCommand returns the run_command tool (command.run). The model names a
// command; argv and timeout come only from the matching CommandGrant, and the
// process runs through the host's CommandRunner in dir. The full output is
// always recorded as an artifact.
func NewCommand(runner api.CommandRunner, dir string) (Tool, error) {
	if runner == nil {
		return nil, errors.New("command tool needs a CommandRunner")
	}
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return nil, fmt.Errorf("command dir %q must be a clean absolute path", dir)
	}
	return &tool{
		spec: api.ToolSpec{
			Name:        "run_command",
			Description: "Run one host-granted command by name. Arguments and timeout are fixed by the host.",
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["command"],` +
				`"properties":{"command":{"type":"string","description":"name of a granted command"}}}`),
		},
		kind:  api.CapabilityCommand,
		scope: commandScope,
		invoke: func(ctx context.Context, inv Invocation) (api.ToolResult, error) {
			return runCommand(ctx, runner, dir, inv)
		},
	}, nil
}

func commandScope(args json.RawMessage) (Scope, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Scope{}, err
	}
	return Scope{Command: a.Command}, nil
}

func runCommand(ctx context.Context, runner api.CommandRunner, dir string, inv Invocation) (api.ToolResult, error) {
	scope, err := commandScope(inv.Arguments)
	if err != nil {
		return failed("arguments: %v", err), nil
	}
	i := slices.IndexFunc(inv.Grant.Commands, func(c api.CommandGrant) bool { return c.Name == scope.Command })
	if i < 0 || inv.Grant.Kind != api.CapabilityCommand {
		return failed("command %q is not granted", scope.Command), nil
	}
	granted := inv.Grant.Commands[i]
	run, err := runner.Run(ctx, api.CommandRequest{
		Argv: slices.Clone(granted.Argv), Dir: dir, Timeout: time.Duration(granted.TimeoutSeconds) * time.Second,
	})
	// A command may change the workspace; its effect is never assumed absent.
	res := api.ToolResult{Status: api.ToolError, Mutated: true}
	if err != nil {
		return res, fmt.Errorf("command %q did not complete: %w", granted.Name, err)
	}
	full := fmt.Sprintf("exit_code: %d\n--- stdout ---\n%s\n--- stderr ---\n%s\n", run.ExitCode, run.Stdout, run.Stderr)
	ref, err := record(ctx, inv.Artifacts, inv.Producer, []byte(full))
	if err != nil {
		return res, fmt.Errorf("recording command output: %w", err)
	}
	code := run.ExitCode
	res.Output, res.ExitCode, res.FullOutput, res.Truncated = full, &code, &ref, run.Truncated
	if code == 0 {
		res.Status = api.ToolOK
	} else {
		res.Error = fmt.Sprintf("exit status %d", code)
	}
	if run.Truncated {
		res.Error = joinText(res.Error, "the runner truncated output; the artifact holds only the bytes it returned")
	}
	return res, nil
}

func joinText(a, b string) string {
	if a == "" {
		return b
	}
	return a + "; " + b
}
