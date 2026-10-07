package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// MaxMacroSteps bounds the steps of one macro call.
const MaxMacroSteps = 16

// ReadFiles returns the read_files macro (file.read): read_file over several
// paths, each step dispatched through a broker under only the grant this call
// was admitted with, so capability is rechecked per step. beforeStep, when not
// nil, is the caller's per-step budget hook; an error stops the macro. The
// first step rides on the macro call's own budget; each later step is charged
// through Invocation.StepBudget, so a macro cannot hide work from the ledger.
//
// Partial failure: steps run in order and stop at the first refused or failed
// step; the result is an error that names it and carries every completed
// step's output. Steps are idempotent reads, so a retry repeats no effect.
func (w *Workspace) ReadFiles(beforeStep func(ctx context.Context, step int) error) (Tool, error) {
	steps, err := NewBroker(w.ReadFile())
	if err != nil {
		return nil, err
	}
	return &tool{
		spec: api.ToolSpec{
			Name:        "read_files",
			Description: fmt.Sprintf("Read up to %d workspace files in order with read_file.", MaxMacroSteps),
			InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["paths"],` +
				`"properties":{"paths":{"type":"array","items":{"type":"string"}}}}`),
		},
		kind:  api.CapabilityFileRead,
		scope: pathsScope,
		invoke: func(ctx context.Context, inv Invocation) (api.ToolResult, error) {
			return readSteps(ctx, steps, beforeStep, inv)
		},
	}, nil
}

func pathsScope(args json.RawMessage) (Scope, error) {
	var a struct {
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return Scope{}, err
	}
	if len(a.Paths) == 0 || len(a.Paths) > MaxMacroSteps {
		return Scope{}, fmt.Errorf("paths must name 1-%d files", MaxMacroSteps)
	}
	return Scope{Paths: a.Paths}, nil
}

func readSteps(ctx context.Context, steps *Broker, beforeStep func(context.Context, int) error,
	inv Invocation) (api.ToolResult, error) {
	scope, err := pathsScope(inv.Arguments)
	if err != nil {
		return failed("arguments: %v", err), nil
	}
	env := Env{
		Grants: []api.Capability{inv.Grant}, Mode: api.ModeReadOnly, Artifacts: inv.Artifacts,
		OutputLimit: max(inv.OutputLimit/len(scope.Paths), 1),
	}
	var out strings.Builder
	for i, p := range scope.Paths {
		if i > 0 && inv.StepBudget != nil {
			if err := inv.StepBudget(ctx); err != nil {
				return stepFailed(out.String(), i, p, "budget: "+err.Error()), nil
			}
		}
		if beforeStep != nil {
			if err := beforeStep(ctx, i); err != nil {
				return stepFailed(out.String(), i, p, "budget: "+err.Error()), nil
			}
		}
		args, err := json.Marshal(map[string]string{"path": p})
		if err != nil {
			return failed("%v", err), nil
		}
		env.Producer = fmt.Sprintf("%s/step-%d", inv.Producer, i)
		call := api.ToolCall{ID: fmt.Sprintf("%s.%d", inv.Call.ID, i), Name: "read_file", Arguments: args}
		res, err := steps.Dispatch(ctx, call, env)
		if err != nil {
			return stepFailed(out.String(), i, p, err.Error()), err
		}
		fmt.Fprintf(&out, "=== %s ===\n%s\n", p, res.Output)
		if res.FullOutput != nil {
			fmt.Fprintf(&out, "[truncated; full output %s, %d bytes]\n", res.FullOutput.Digest, res.FullOutput.Size)
		}
		if res.Status != api.ToolOK {
			return stepFailed(out.String(), i, p, res.Error), nil
		}
	}
	return api.ToolResult{Status: api.ToolOK, Output: out.String()}, nil
}

func stepFailed(completed string, step int, p, reason string) api.ToolResult {
	res := failed("step %d (%s) failed after %d completed steps: %s", step, p, step, reason)
	res.Output = completed
	return res
}
