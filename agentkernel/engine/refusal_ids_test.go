package engine_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
)

// TestRefusedRequestNeverReusesAnAdmittedEventID: a re-entered attempt is
// refused under the same execution and attempt IDs as the admitted run. Its
// events must not reuse that run's hand-off IDs, or a host deduplicating by
// ID (as api.Call instructs) would ack the refusal without recording it.
func TestRefusedRequestNeverReusesAnAdmittedEventID(t *testing.T) {
	var mu sync.Mutex
	byID := map[string][]api.EventKind{}
	events := make(chan api.EventDelivery)
	t.Cleanup(func() { close(events) })
	go func() {
		for c := range events {
			mu.Lock()
			byID[c.ID] = append(byID[c.ID], c.Request.Kind)
			mu.Unlock()
			c.Reply <- nil
		}
	}()
	f := newFixture(t, []scripted.Step{end("one")}, func(c *engine.Config) { c.Events = events })
	f.engine.Execute(context.Background(), request())
	for range 2 { // refused twice: refusals must not collide with each other either
		res, _ := f.engine.Execute(context.Background(), request())
		if res.Termination.Cause != api.CauseInvalidRequest {
			t.Fatalf("re-entry settled %s/%s, want blocked/invalid_request", res.Termination.Outcome, res.Termination.Cause)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for id, kinds := range byID {
		if len(kinds) > 1 {
			t.Fatalf("hand-off ID %q sent for %d different events: %v", id, len(kinds), kinds)
		}
	}
}
