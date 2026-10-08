package scripted

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{
		Build: func(t *testing.T, env conformance.Env) api.Provider {
			steps := make([]Step, len(env.Script))
			for i, ex := range env.Script {
				switch {
				case ex.Response != nil:
					steps[i] = Step{Response: *ex.Response}
				case ex.Reply.Block:
					steps[i] = Step{Block: true}
				default:
					t.Fatalf("exchange %d has no in-process form", i)
				}
			}
			return New(steps...)
		},
		Bodies: func(p api.Provider, _ conformance.Env) [][]byte {
			var out [][]byte
			for _, r := range p.(*Provider).Requests() {
				data, _ := json.Marshal(r)
				out = append(out, data)
			}
			return out
		},
		Inspect: conformance.InspectNeutral,
	})
}

// TestConcurrentCallsConsumeEachStepOnce runs under -race in CI.
func TestConcurrentCallsConsumeEachStepOnce(t *testing.T) {
	const n = 64
	steps := make([]Step, n)
	for i := range steps {
		steps[i] = Step{Response: api.ProviderResponse{Text: string(rune('A' + i%26)), Stop: api.StopEnd}}
	}
	p := New(steps...)
	var wg sync.WaitGroup
	errs := make(chan error, n+1)
	for range n + 1 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := p.Complete(t.Context(), api.ProviderRequest{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	failed := 0
	for err := range errs {
		if err != nil {
			failed++
		}
	}
	if failed != 1 || len(p.Requests()) != n+1 {
		t.Fatalf("failed=%d requests=%d; want exactly one call beyond the script to fail", failed, len(p.Requests()))
	}
}
