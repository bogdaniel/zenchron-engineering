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
	"github.com/bogdaniel/zenchron-engineering/agentkernel/routing"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// sourceItemLimit bounds how many optional items one ContextSource may offer.
const sourceItemLimit = 64

// Config is everything the engine is built from.
//
// Host code is never called on the kernel's goroutine. Every port a host
// implements is an unbuffered channel of api.Calls served by host-owned
// workers (api.ServeEvents, api.ServeContext, api.ServeProvider,
// api.ServeCommands, tools.ServeTool); the kernel waits on each within a
// bound and spawns no goroutine. What the kernel calls directly is
// kernel-owned code whose termination it controls, and New accepts only
// those exact concrete types, so a host type embedding one is refused: the
// broker's tools (package tools), a *storage.MemoryArtifacts or
// *storage.FileArtifacts, and an api.SystemClock or *api.ManualClock.
type Config struct {
	// Providers are hand-off channels keyed by ProviderBinding.ID. Routing
	// chooses among the request's bindings; a chosen binding with no
	// channel blocks.
	Providers map[string]chan<- api.ProviderCall
	// Broker dispatches tool calls; nil means no tools are offered.
	Broker *tools.Broker
	// Artifacts stores tool output; required when Broker is set. Only a
	// *storage.MemoryArtifacts or *storage.FileArtifacts is accepted.
	Artifacts api.ArtifactStore
	Events    chan<- api.EventDelivery
	// Clock is api.SystemClock or a non-nil *api.ManualClock; nothing else.
	Clock api.Clock
	// Sources supply optional context. Their items are always untrusted.
	Sources []chan<- api.ContextRequest
	// Observations are optional routing data; StaleAfter is their maximum age
	// (zero treats every observation as stale, so none is used).
	Observations []routing.Observation
	StaleAfter   time.Duration
	// OutputLimit bounds each tool result's inline output in bytes; required
	// when Broker is set.
	OutputLimit int
	// Admissions holds each execution_id's admission claim and record, so a
	// re-entered or concurrent attempt is refused and a later attempt starts
	// from what earlier ones consumed. Nil means an in-memory store owned by
	// this Engine, enforcing the envelope within this Engine only. A
	// FileRecords store enforces it across restarts and across Engines and
	// processes sharing its root on one local filesystem (admission.go).
	Admissions *storage.FileRecords
	// SettleTimeout is the settlement grace: the longest terminal recording
	// may take, and the longest a hand-off may still wait after a host
	// cancellation. Zero means DefaultSettleTimeout; negative is refused.
	// The kernel never runs past the budget deadline (or a cancellation) by
	// more than this grace.
	SettleTimeout time.Duration
}

// Engine implements api.Executor.
type Engine struct {
	providers     map[string]chan<- api.ProviderCall
	broker        *tools.Broker
	artifacts     api.ArtifactStore
	events        chan<- api.EventDelivery
	clock         api.Clock
	sources       []chan<- api.ContextRequest
	observations  []routing.Observation
	staleAfter    time.Duration
	outputLimit   int
	admissions    storage.Records
	settleTimeout time.Duration
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
	if err := checkPorts(cfg); err != nil {
		return nil, err
	}
	if cfg.Broker != nil && (cfg.Artifacts == nil || cfg.OutputLimit <= 0) {
		return nil, errors.New("engine: a broker needs an artifact store and a positive output limit")
	}
	if cfg.SettleTimeout < 0 {
		return nil, errors.New("engine: settle timeout must not be negative")
	}
	settle := cfg.SettleTimeout
	if settle == 0 {
		settle = DefaultSettleTimeout
	}
	var admissions storage.Records = storage.NewMemoryRecords()
	if cfg.Admissions != nil {
		admissions = cfg.Admissions
	}
	return &Engine{
		providers: cfg.Providers, broker: cfg.Broker, artifacts: cfg.Artifacts, events: cfg.Events,
		clock: cfg.Clock, sources: cfg.Sources, observations: cfg.Observations, staleAfter: cfg.StaleAfter,
		outputLimit: cfg.OutputLimit, admissions: admissions, settleTimeout: settle,
	}, nil
}

// checkPorts refuses a hand-off channel that is nil or buffered (a buffered
// send is not a worker taking the call, so ErrNotTaken could no longer mean
// "the host never saw it"), and any clock or artifact store that is not the
// kernel's own concrete type: the engine calls those synchronously.
func checkPorts(cfg Config) error {
	for id, p := range cfg.Providers {
		if p == nil || cap(p) != 0 {
			return fmt.Errorf("engine: provider %q must be a non-nil unbuffered channel", id)
		}
	}
	if cap(cfg.Events) != 0 {
		return errors.New("engine: events must be an unbuffered channel")
	}
	for i, s := range cfg.Sources {
		if s == nil || cap(s) != 0 {
			return fmt.Errorf("engine: source %d must be a non-nil unbuffered channel", i)
		}
	}
	switch c := cfg.Clock.(type) {
	case api.SystemClock:
	case *api.ManualClock:
		if c == nil {
			return errors.New("engine: clock is a nil *api.ManualClock")
		}
	default:
		return fmt.Errorf("engine: clock %T is not api.SystemClock or *api.ManualClock", cfg.Clock)
	}
	switch a := cfg.Artifacts.(type) {
	case nil:
	case *storage.MemoryArtifacts:
		if a == nil {
			return errors.New("engine: artifacts is a nil *storage.MemoryArtifacts")
		}
	case *storage.FileArtifacts:
		if a == nil {
			return errors.New("engine: artifacts is a nil *storage.FileArtifacts")
		}
	default:
		return fmt.Errorf("engine: artifacts %T is not a store of package storage", cfg.Artifacts)
	}
	return nil
}

// Execute runs one bounded execution. The error is always nil: every request
// the engine can evaluate, a refused one included, yields a settled result.
func (e *Engine) Execute(ctx context.Context, req api.ExecutionRequest) (api.ExecutionResult, error) {
	r := newRun(e, ctx, req)
	if err := req.Validate(e.clock.Now()); err != nil {
		return r.refuse(ctx, r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, err.Error())), nil
	}
	if t, ok := r.admit(ctx); !ok {
		return r.refuse(ctx, t), nil
	}
	runCtx, cancel := context.WithDeadline(ctx, req.Budget.Deadline)
	defer cancel()
	return r.finish(ctx, r.execute(runCtx)), nil
}

// refuse settles a request that never started: it records the refusal and
// settles without any provider, tool or source call.
func (r *run) refuse(ctx context.Context, t api.Termination) api.ExecutionResult {
	r.beginSettlement()
	if err := r.emit(ctx, api.Event{Kind: api.EventRefused, Detail: t.Detail}); err != nil && t.Cause != api.CauseRecordingFailed {
		t = r.recordingFailed(describe(t))
	}
	return r.finish(ctx, t)
}

// execute binds, routes and compiles, then runs the loop.
func (r *run) execute(ctx context.Context) api.Termination {
	if err := r.emit(ctx, api.Event{Kind: api.EventStarted, Detail: fmt.Sprintf("%d bindings", len(r.req.Providers))}); err != nil {
		return r.recordingFailed("")
	}
	binding, calls, t, ok := r.route(ctx)
	if !ok {
		return t
	}
	var specs []api.ToolSpec
	if r.e.broker != nil {
		specs = r.e.broker.Specs(r.req.Grants, r.req.Mode)
	}
	messages, t, ok := r.compile(ctx, binding, specs)
	if !ok {
		return t
	}
	return r.loop(ctx, call{calls: calls, binding: binding, specs: specs}, messages)
}

func (r *run) route(ctx context.Context) (api.ProviderBinding, chan<- api.ProviderCall, api.Termination, bool) {
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
	calls, ok := r.e.providers[binding.ID]
	if !ok {
		// No silent switch to another binding: the host's ranking stands.
		return binding, nil, r.termination(api.OutcomeBlocked, api.CauseNoEligibleProvider,
			fmt.Sprintf("binding %q was chosen but no adapter is configured for it", binding.ID)), false
	}
	r.provenance.ProviderID, r.provenance.ProviderKind = binding.ID, binding.Kind
	r.provenance.Model, r.provenance.ModelVersionBound = binding.Model, binding.ModelVersion
	r.provenance.ConfigFingerprint, r.provenance.Isolation = binding.ConfigFingerprint, binding.Isolation
	return binding, calls, api.Termination{}, true
}
