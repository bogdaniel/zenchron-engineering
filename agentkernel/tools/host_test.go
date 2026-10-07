package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// hostWorker is how the host side of a hand-off behaves in a test.
type hostWorker int

const (
	neverTakes   hostWorker = iota // nobody reads the channel
	neverAnswers                   // takes the call and hangs until the test ends
	answers                        // answers at once
)

// serve returns a channel driven per w; answer is the normal reply.
func serve[Q, R any](t *testing.T, w hostWorker, answer R) chan<- api.Call[Q, R] {
	calls := make(chan api.Call[Q, R])
	ctx := t.Context()
	switch w {
	case neverAnswers:
		go func() {
			select {
			case <-calls:
				<-ctx.Done()
			case <-ctx.Done():
			}
		}()
	case answers:
		return api.Serve(ctx, func(context.Context, Q) R { return answer })
	}
	return calls
}

func hostTool(t *testing.T, kind api.CapabilityKind, calls chan<- HostToolCall) *Broker {
	t.Helper()
	tool, err := NewHostTool(HostTool{
		Spec: api.ToolSpec{Name: "host_op", Description: "test", InputSchema: json.RawMessage(
			`{"type":"object","additionalProperties":false,"required":["doc"],"properties":{"doc":{"type":"string"}}}`)},
		Kind: kind, PathArgument: "doc", Calls: calls,
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewBroker(tool)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestHostToolHandOff: a host tool the host never takes did not run; one it
// takes but never answers is an unknown outcome when the kind mutates; a
// normal answer is bounded like any tool output and cannot smuggle an
// unverified artifact reference. Each returns once the call's context ends.
func TestHostToolHandOff(t *testing.T) {
	ok := api.Reply[api.ToolResult]{Value: api.ToolResult{Status: api.ToolOK, Output: "found",
		FullOutput: &api.ArtifactRef{Digest: "sha256:forged"}}}
	cases := map[string]struct {
		worker    hostWorker
		kind      api.CapabilityKind
		status    api.ToolStatus
		unknown   bool
		errorText string
	}{
		"never_taken_write":     {worker: neverTakes, kind: api.CapabilityFileWrite, status: api.ToolError, errorText: "did not take"},
		"never_answered_write":  {worker: neverAnswers, kind: api.CapabilityFileWrite, status: api.ToolError, unknown: true},
		"never_answered_search": {worker: neverAnswers, kind: api.CapabilityFileSearch, status: api.ToolError},
		"answered":              {worker: answers, kind: api.CapabilityFileSearch, status: api.ToolOK},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil)
			b := hostTool(t, c.kind, serve[HostInvocation](t, c.worker, ok))
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			res, err := b.Dispatch(ctx, call("host_op", `{"doc":"docs/a.txt"}`), f.env(api.ModeReadWrite, grant("g", c.kind, "docs")))
			if time.Since(start) > 2*time.Second {
				t.Fatalf("dispatch outlived its context by %s", time.Since(start))
			}
			if res.Status != c.status || (err != nil) != c.unknown || !strings.Contains(res.Error, c.errorText) {
				t.Fatalf("result %+v, err %v", res, err)
			}
			if res.FullOutput != nil {
				t.Fatalf("host-supplied artifact reference kept: %+v", res.FullOutput)
			}
		})
	}
}

// TestCommandHandOff: a command the runner never takes did not run and is
// not reported as a mutation; one it takes but never answers is.
func TestCommandHandOff(t *testing.T) {
	for name, c := range map[string]struct {
		worker  hostWorker
		mutated bool
	}{"never_taken": {worker: neverTakes}, "never_answered": {worker: neverAnswers, mutated: true}} {
		t.Run(name, func(t *testing.T) {
			cmd, err := NewCommand(serve[api.CommandRequest](t, c.worker, api.Reply[api.CommandResult]{}), "/work/space")
			if err != nil {
				t.Fatal(err)
			}
			b, err := NewBroker(cmd)
			if err != nil {
				t.Fatal(err)
			}
			_, env, _ := commandFixture(t, &fakeRunner{})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			res, err := b.Dispatch(ctx, call("run_command", `{"command":"unit"}`), env)
			if res.Status != api.ToolError || res.Mutated != c.mutated || (err != nil) != c.mutated {
				t.Fatalf("result %+v, err %v", res, err)
			}
		})
	}
}
