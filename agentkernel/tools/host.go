package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// HostInvocation is what a host tool is told about one admitted call: the
// call and the grant the broker selected. It carries no kernel callback, so
// a worker answering late can never reach the execution.
type HostInvocation struct {
	Call  api.ToolCall
	Grant api.Capability
}

// HostToolCall hands one admitted call to a host tool's worker. Its ID is the
// call's producer ("<execution>/<attempt>/<call>").
type HostToolCall = api.Call[HostInvocation, api.Reply[api.ToolResult]]

// HostTool is a tool the host implements. The kernel reaches it only through
// Calls; the broker validates arguments, selects the grant and bounds the
// output exactly as for a built-in tool.
type HostTool struct {
	Spec api.ToolSpec
	// Kind is a file capability kind; commands go through NewCommand.
	Kind api.CapabilityKind
	// PathArgument names the string argument holding the one
	// workspace-relative path a call touches; the grant is selected for it.
	PathArgument string
	Calls        chan<- HostToolCall
}

// NewHostTool returns the broker-dispatchable form of h.
func NewHostTool(h HostTool) (Tool, error) {
	if h.Calls == nil {
		return nil, errors.New("host tool needs a calls channel")
	}
	if !h.Kind.Known() || h.Kind == api.CapabilityCommand {
		return nil, fmt.Errorf("host tool %q: kind %q is not a file capability", h.Spec.Name, h.Kind)
	}
	if h.PathArgument == "" {
		return nil, fmt.Errorf("host tool %q: no path argument named", h.Spec.Name)
	}
	return &tool{
		spec:  h.Spec,
		kind:  h.Kind,
		scope: func(args json.RawMessage) (Scope, error) { return argumentPath(args, h.PathArgument) },
		invoke: func(ctx context.Context, inv Invocation) (api.ToolResult, error) {
			return invokeHost(ctx, h, inv)
		},
	}, nil
}

func argumentPath(args json.RawMessage, name string) (Scope, error) {
	var a map[string]json.RawMessage
	if err := json.Unmarshal(args, &a); err != nil {
		return Scope{}, err
	}
	var p string
	if err := json.Unmarshal(a[name], &p); err != nil {
		return Scope{}, fmt.Errorf("argument %q must name a path", name)
	}
	return Scope{Paths: []string{p}}, nil
}

// invokeHost hands the call to the host and waits while ctx lives. A call
// the host never took did not run; one it took but did not answer is an
// unknown outcome, which stops the execution when the kind mutates.
func invokeHost(ctx context.Context, h HostTool, inv Invocation) (api.ToolResult, error) {
	call := HostToolCall{ID: inv.Producer, Context: ctx, Request: HostInvocation{Call: inv.Call, Grant: inv.Grant}}
	reply, err := handoff.Exchange("host tool "+h.Spec.Name, h.Calls, call, handoff.Bound{Shorten: ctx.Done()})
	if errors.Is(err, handoff.ErrNotTaken) {
		return failed("%v", err), nil
	}
	if err != nil {
		return api.ToolResult{Status: api.ToolError, Mutated: h.Kind.Mutating()}, err
	}
	res := reply.Value
	// Only the kernel records artifacts; a host reference is unverified.
	res.FullOutput = nil
	return res, reply.Err
}

// ServeTool starts the host-owned worker for a host tool (see api.Serve).
func ServeTool(ctx context.Context, invoke func(context.Context, HostInvocation) (api.ToolResult, error)) chan<- HostToolCall {
	return api.Serve(ctx, func(ctx context.Context, inv HostInvocation) api.Reply[api.ToolResult] {
		res, err := invoke(ctx, inv)
		return api.Reply[api.ToolResult]{Value: res, Err: err}
	})
}
