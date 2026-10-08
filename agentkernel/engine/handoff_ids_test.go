package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// idLog collects hand-off IDs and what each carried, from every port.
type idLog struct {
	mu   sync.Mutex
	seen map[string][]string
}

func (l *idLog) add(id, what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[id] = append(l.seen[id], what)
}

// writeHost is a host write tool whose worker logs each call's ID.
func writeHost(t *testing.T, log *idLog) *tools.Broker {
	t.Helper()
	ch := make(chan tools.HostToolCall)
	go func() {
		for {
			select {
			case c := <-ch:
				log.add(c.ID, "tool "+string(c.Request.Call.Arguments))
				c.Reply <- api.Reply[api.ToolResult]{Value: api.ToolResult{Status: api.ToolOK, Mutated: true}}
			case <-t.Context().Done():
				return
			}
		}
	}()
	tool, err := tools.NewHostTool(tools.HostTool{
		Spec: api.ToolSpec{Name: "write_file", Description: "w", InputSchema: json.RawMessage(
			`{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`)},
		Kind: api.CapabilityFileWrite, PathArgument: "path", Calls: ch,
	})
	if err != nil {
		t.Fatal(err)
	}
	broker, err := tools.NewBroker(tool)
	if err != nil {
		t.Fatal(err)
	}
	return broker
}

func write(id, path string) api.ToolCall {
	return api.ToolCall{ID: id, Name: "write_file", Arguments: json.RawMessage(`{"path":"` + path + `","content":"x"}`)}
}

// TestModelToolCallIDsNeverBecomeHandOffIDs: hand-off IDs are kernel
// sequence numbers, never model output. A model reusing a tool-call ID gets
// the repeat refused (never dispatched), and a model ID shaped like a kernel
// one ("event-3") collides with nothing.
func TestModelToolCallIDsNeverBecomeHandOffIDs(t *testing.T) {
	log := &idLog{seen: map[string][]string{}}
	events := make(chan api.EventDelivery)
	go func() {
		for {
			select {
			case c := <-events:
				log.add(c.ID, "event "+string(c.Request.Kind))
				c.Reply <- nil
			case <-t.Context().Done():
				return
			}
		}
	}()
	steps := []scripted.Step{toolUse(write("w1", "a.txt"), write("w1", "b.txt"), write("event-3", "c.txt")), end("ok")}
	f := newFixture(t, steps, func(c *engine.Config) { c.Broker, c.Events = writeHost(t, log), events })
	req := request()
	req.Mode = api.ModeReadWrite
	req.Grants = []api.Capability{{Handle: "write-all", Kind: api.CapabilityFileWrite, Roots: []string{"."}}}
	res, _ := f.engine.Execute(context.Background(), req)
	want(t, res, api.OutcomeCompleted, api.CauseLoopCompleted)
	log.mu.Lock()
	defer log.mu.Unlock()
	tools := 0
	for id, what := range log.seen {
		if len(what) > 1 {
			t.Fatalf("hand-off ID %q sent for %d different requests: %v", id, len(what), what)
		}
		if strings.HasPrefix(what[0], "tool") {
			tools++
			if strings.Contains(what[0], "b.txt") {
				t.Fatalf("the repeated model tool-call ID was dispatched as %q", id)
			}
		}
	}
	if tools != 2 {
		t.Fatalf("%d tool hand-offs, want 2 (a.txt and c.txt): %v", tools, log.seen)
	}
}

// TestClosedEventReplyIsRecordingFailure: a worker that closes Reply instead
// of answering recorded nothing; the zero value of a closed channel must not
// read as "durable".
func TestClosedEventReplyIsRecordingFailure(t *testing.T) {
	events := make(chan api.EventDelivery)
	go func() {
		for {
			select {
			case c := <-events:
				close(c.Reply)
			case <-t.Context().Done():
				return
			}
		}
	}()
	f := newFixture(t, []scripted.Step{end("ok")}, func(c *engine.Config) { c.Events = events })
	res, _ := f.engine.Execute(context.Background(), request())
	if res.Termination.Cause != api.CauseRecordingFailed || res.EventCount != 0 {
		t.Fatalf("nothing was recorded, yet the result reports %d recorded events and %s", res.EventCount, describeTermination(res))
	}
}
