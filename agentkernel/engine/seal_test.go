package engine_test

import (
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// Host types that would satisfy the kernel-owned ports by embedding a kernel
// type and overriding a method with host code.
type hostClock struct{ api.SystemClock }

func (hostClock) Now() time.Time { select {} }

type hostArtifacts struct{ *storage.MemoryArtifacts }

// newConfig is a valid Config for construction-only tests.
func newConfig(t *testing.T) engine.Config {
	arts, err := storage.NewMemoryArtifacts(1 << 10)
	if err != nil {
		t.Fatal(err)
	}
	return engine.Config{
		Providers: map[string]chan<- api.ProviderCall{"p1": make(chan api.ProviderCall)},
		Broker:    &tools.Broker{}, Artifacts: arts, Events: make(chan api.EventDelivery),
		Clock: api.SystemClock{}, OutputLimit: 64,
	}
}

// TestNewRefusesHostCodeInKernelPorts: the engine calls its clock and
// artifact store synchronously, so it accepts only the kernel's own
// concrete types; a host type embedding one is refused, as is a hand-off
// channel with a buffer (a buffered send is not a worker taking the call,
// so "not taken" could no longer mean "the host never saw it").
func TestNewRefusesHostCodeInKernelPorts(t *testing.T) {
	cases := map[string]func(*engine.Config){
		"embedded_clock":     func(c *engine.Config) { c.Clock = hostClock{} },
		"nil_manual_clock":   func(c *engine.Config) { c.Clock = (*api.ManualClock)(nil) },
		"embedded_artifacts": func(c *engine.Config) { c.Artifacts = hostArtifacts{c.Artifacts.(*storage.MemoryArtifacts)} },
		"buffered_provider": func(c *engine.Config) {
			c.Providers = map[string]chan<- api.ProviderCall{"p1": make(chan api.ProviderCall, 1)}
		},
		"buffered_events": func(c *engine.Config) { c.Events = make(chan api.EventDelivery, 1) },
		"buffered_source": func(c *engine.Config) { c.Sources = []chan<- api.ContextRequest{make(chan api.ContextRequest, 1)} },
	}
	if _, err := engine.New(newConfig(t)); err != nil {
		t.Fatalf("valid config refused: %v", err)
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := newConfig(t)
			mutate(&cfg)
			if _, err := engine.New(cfg); err == nil || !strings.Contains(err.Error(), "engine:") {
				t.Fatalf("engine.New accepted it (err %v)", err)
			}
		})
	}
}
