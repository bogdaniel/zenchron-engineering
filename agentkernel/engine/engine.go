// Package engine is the bounded execution loop of the Agent Execution Kernel:
// validate, bind, compile context, route, then alternate provider calls and
// brokered tool calls inside the host's unchanged envelope, and settle once
// with an observation-only result.
package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	kcontext "github.com/bogdaniel/zenchron-engineering/agentkernel/context"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/routing"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// sourceItemLimit bounds how many optional items one ContextSource may offer.
const sourceItemLimit = 64

// Config is everything the engine is built from. All ports are injected.
type Config struct {
	// Providers are adapters keyed by ProviderBinding.ID. Routing chooses among
	// the request's bindings; a chosen binding with no adapter blocks.
	Providers map[string]api.Provider
	// Broker dispatches tool calls; nil means no tools are offered.
	Broker *tools.Broker
	// Artifacts stores tool output; required when Broker is set.
	Artifacts api.ArtifactStore
	Events    api.EventSink
	Clock     api.Clock
	// Sources supply optional context. Their items are always untrusted.
	Sources []api.ContextSource
	// Observations are optional routing data; StaleAfter is their maximum age
	// (zero treats every observation as stale, so none is used).
	Observations []routing.Observation
	StaleAfter   time.Duration
	// OutputLimit bounds each tool result's inline output in bytes; required
	// when Broker is set.
	OutputLimit int
}

// Engine implements api.Executor.
type Engine struct {
	providers    map[string]api.Provider
	broker       *tools.Broker
	artifacts    api.ArtifactStore
	events       api.EventSink
	clock        api.Clock
	sources      []api.ContextSource
	observations []routing.Observation
	staleAfter   time.Duration
	outputLimit  int
}

var _ api.Executor = (*Engine)(nil)

// New validates cfg. Nothing is defaulted: a missing port is an error.
func New(cfg Config) (*Engine, error) {
	if cfg.Events == nil || cfg.Clock == nil {
		return nil, errors.New("engine: events and clock are required")
	}
	if len(cfg.Providers) == 0 {
		return nil, errors.New("engine: at least one provider is required")
	}
	for id, p := range cfg.Providers {
		if p == nil {
			return nil, fmt.Errorf("engine: provider %q is nil", id)
		}
	}
	if cfg.Broker != nil && (cfg.Artifacts == nil || cfg.OutputLimit <= 0) {
		return nil, errors.New("engine: a broker needs an artifact store and a positive output limit")
	}
	for i, s := range cfg.Sources {
		if s == nil {
			return nil, fmt.Errorf("engine: source %d is nil", i)
		}
	}
	return &Engine{
		providers: cfg.Providers, broker: cfg.Broker, artifacts: cfg.Artifacts, events: cfg.Events,
		clock: cfg.Clock, sources: cfg.Sources, observations: cfg.Observations, staleAfter: cfg.StaleAfter,
		outputLimit: cfg.OutputLimit,
	}, nil
}

// Execute runs one bounded execution. The error is always nil: every request
// the engine can evaluate, a refused one included, yields a settled result.
func (e *Engine) Execute(ctx context.Context, req api.ExecutionRequest) (api.ExecutionResult, error) {
	r := newRun(e, ctx, req)
	if err := req.Validate(e.clock.Now()); err != nil {
		t := r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, err.Error())
		if rerr := r.emit(ctx, api.Event{Kind: api.EventRefused, Detail: err.Error()}); rerr != nil {
			t = r.recordingFailed(describe(t))
		}
		return r.finish(ctx, t), nil
	}
	runCtx, cancel := context.WithDeadline(ctx, req.Budget.Deadline)
	defer cancel()
	return r.finish(ctx, r.execute(runCtx)), nil
}

// execute binds, routes and compiles, then runs the loop.
func (r *run) execute(ctx context.Context) api.Termination {
	if err := r.emit(ctx, api.Event{Kind: api.EventStarted, Detail: fmt.Sprintf("%d bindings", len(r.req.Providers))}); err != nil {
		return r.recordingFailed("")
	}
	binding, provider, t, ok := r.route(ctx)
	if !ok {
		return t
	}
	var specs []api.ToolSpec
	if r.e.broker != nil {
		specs = r.e.broker.Specs(r.req.Grants, r.req.Mode)
	}
	messages, t, ok := r.compile(ctx, binding, estimatorFor(provider), specs)
	if !ok {
		return t
	}
	return r.loop(ctx, call{provider: provider, binding: binding, specs: specs, estimate: estimatorFor(provider)}, messages)
}

func (r *run) route(ctx context.Context) (api.ProviderBinding, api.Provider, api.Termination, bool) {
	decision := routing.Select(routing.Input{
		Bindings:                   r.req.Providers,
		RequireHostProvenIsolation: r.req.Constraints.RequireHostProvenIsolation,
		RequiredFeatures:           r.req.Constraints.RequiredFeatures,
		Observations:               r.e.observations,
		Now:                        r.e.clock.Now(),
		StaleAfter:                 r.e.staleAfter,
	})
	r.routing = &decision
	detail := "chosen " + decision.Chosen
	if decision.Chosen == "" {
		detail = "blocked: " + decision.Blocked
	}
	if err := r.emit(ctx, api.Event{Kind: api.EventRoutingDecided, Detail: detail}); err != nil {
		return api.ProviderBinding{}, nil, r.recordingFailed(""), false
	}
	if decision.Chosen == "" {
		return api.ProviderBinding{}, nil, r.termination(api.OutcomeBlocked, api.CauseNoEligibleProvider, decision.Blocked), false
	}
	var binding api.ProviderBinding
	for _, b := range r.req.Providers {
		if b.ID == decision.Chosen {
			binding = b
		}
	}
	provider, ok := r.e.providers[binding.ID]
	if !ok {
		// No silent switch to another binding: the host's ranking stands.
		return binding, nil, r.termination(api.OutcomeBlocked, api.CauseNoEligibleProvider,
			fmt.Sprintf("binding %q was chosen but no adapter is configured for it", binding.ID)), false
	}
	r.provenance.ProviderID, r.provenance.ProviderKind = binding.ID, binding.Kind
	r.provenance.Model, r.provenance.ModelVersionBound = binding.Model, binding.ModelVersion
	r.provenance.ConfigFingerprint, r.provenance.Isolation = binding.ConfigFingerprint, binding.Isolation
	return binding, provider, api.Termination{}, true
}

// estimatorFor prefers the provider's own counter; the fallback is the
// context compiler's approximate bytes/4, always marked inexact.
func estimatorFor(p api.Provider) kcontext.Estimator {
	if est, ok := p.(api.TokenEstimator); ok {
		return est.EstimateTokens
	}
	return kcontext.ApproximateTokens
}
