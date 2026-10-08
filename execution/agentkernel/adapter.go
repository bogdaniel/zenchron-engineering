// Package agentkernel is the host adapter for the Agent Execution Kernel
// (ADR-0006, #518): one execution.Port implementation that runs each host
// invocation as one bounded kernel execution. It is the only package of this
// module that imports the kernel (architecture_test.go, R3).
//
// The host keeps every authority ADR-0005 assigns it: run and attempt
// identity, budgets, cancellation provenance, the termination owner,
// recording, candidate authority, waits and retries. The adapter translates
// a Request into a kernel request (request.go), serves the kernel's hand-off
// ports with workers it owns for exactly one Execute (workers.go), and
// translates the settled result back (result.go).
//
// NOT PRODUCTION-ELIGIBLE. Nothing composes or registers this adapter yet,
// and nothing may until the Gate B obligations recorded in
// docs/acceptance/agent-execution-kernel-gate-b.md are closed. In
// particular, a controller crash after the kernel's admission claim leaves
// that claim held for ever: every later attempt of the operation is refused
// until an explicit, never time-based recovery protocol exists. No TTL may
// ever stand in for one.
package agentkernel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

const (
	// toolOutputLimit bounds each tool result's inline output; the full bytes
	// go to the attempt's artifact store.
	toolOutputLimit = 32 << 10
	// probeTimeout bounds one toolchain probe command.
	probeTimeout = 30 * time.Second
	// progressMode names the progress oracle in invocation provenance.
	progressMode = "agentkernel_events"
)

// TranscriptWriter is the host's forensic transcript for one invocation
// (§6 gate b: a durable I/O boundary). WriteEvent appends one kernel event,
// encoded as agentkernel.execution/v0.2 JSON, and returns only once it is
// durable. An error means it was not recorded: the execution then settles
// recording_failed and takes no further side effect.
type TranscriptWriter interface {
	WriteEvent(ctx context.Context, attempt execution.AttemptRef, event []byte) error
}

// CommandRunner is the host's process boundary for granted commands (§6
// gate b: process I/O). It must contain the process and honour ctx; the
// adapter never spawns one. A toolchain probe has a zero Attempt and an
// empty Dir: it names no candidate.
type CommandRunner interface {
	Run(ctx context.Context, cmd Command) (CommandOutput, error)
}

// Command is one host-named command, with argv fixed by the catalogue.
type Command struct {
	Attempt    execution.AttemptRef
	Argv       []string
	Dir        string
	ScratchDir string
	Timeout    time.Duration
}

// CommandOutput is a command's bounded outcome.
type CommandOutput struct {
	ExitCode       int
	Stdout, Stderr []byte
	Truncated      bool
}

// CommandSpec is one operator-declared command the kernel may be granted by
// name. It is granted to an invocation only when that invocation's contract
// requires its executable (Argv[0]).
type CommandSpec struct {
	Name    string
	Argv    []string
	Timeout time.Duration
}

// Limits are the adapter's explicit bounds for every kernel dimension the
// host does not state. They never widen a host bound: a stated host bound
// below a limit wins (request.go).
type Limits struct {
	MaxWall          time.Duration
	MaxIterations    int
	MaxToolCalls     int
	MaxInputTokens   int64
	MaxOutputTokens  int64
	MaxArtifactBytes int64
}

// Config is everything the adapter is built from. Credentials are not here:
// a live provider adapter resolves its own through the api.CredentialSource
// the composition root gives it, and this package looks nothing up.
type Config struct {
	AgentID      string
	ProviderKind string
	AuthMode     string
	TrustMode    execution.TrustMode
	// Binding is the one provider identity the host decided about. The
	// adapter pins it, marks it eligible and states its isolation unproven.
	Binding  api.ProviderBinding
	Provider api.Provider
	// CostCurrency is the currency the host's MaxCostMicros is denominated
	// in. Without it, or without a complete rate card in it, a cost ceiling
	// is refused rather than ignored.
	CostCurrency string
	// StateDir is an absolute directory for the kernel-owned stores: durable
	// admissions (shared by every adapter on this root) and each attempt's
	// tool-output artifacts. The host artifact-store bridge is a Gate B
	// obligation not served here.
	StateDir   string
	Limits     Limits
	Transcript TranscriptWriter
	// Commands is optional; without it no command is ever granted.
	Commands  CommandRunner
	Catalogue []CommandSpec
}

// Adapter implements execution.Port over the Agent Execution Kernel.
type Adapter struct {
	agentID, providerKind, authMode string
	trustMode                       execution.TrustMode
	binding                         api.ProviderBinding
	provider                        api.Provider
	costCurrency                    string
	stateDir                        string
	limits                          Limits
	transcript                      TranscriptWriter
	commands                        CommandRunner
	catalogue                       []CommandSpec
	admissions                      *storage.FileRecords
	clock                           clock
}

var _ execution.Port = (*Adapter)(nil)

// New validates cfg and opens the durable admission store. Nothing is
// defaulted.
func New(cfg Config) (*Adapter, error) {
	if cfg.AgentID == "" || cfg.ProviderKind == "" || cfg.Binding.ID == "" {
		return nil, errors.New("agentkernel: agent id, provider kind and binding id are required")
	}
	if cfg.Provider == nil || cfg.Transcript == nil {
		return nil, errors.New("agentkernel: a provider and a transcript writer are required")
	}
	if !filepath.IsAbs(cfg.StateDir) || filepath.Clean(cfg.StateDir) != cfg.StateDir {
		return nil, fmt.Errorf("agentkernel: state dir %q must be a clean absolute path", cfg.StateDir)
	}
	if err := cfg.Limits.validate(); err != nil {
		return nil, err
	}
	if err := validateCatalogue(cfg.Catalogue, cfg.Commands != nil); err != nil {
		return nil, err
	}
	admissions, err := storage.OpenFileRecords(filepath.Join(cfg.StateDir, "admissions"))
	if err != nil {
		return nil, fmt.Errorf("agentkernel: admissions: %w", err)
	}
	return &Adapter{
		agentID: cfg.AgentID, providerKind: cfg.ProviderKind, authMode: cfg.AuthMode, trustMode: cfg.TrustMode,
		binding: cfg.Binding, provider: cfg.Provider, costCurrency: cfg.CostCurrency, stateDir: cfg.StateDir,
		limits: cfg.Limits, transcript: cfg.Transcript, commands: cfg.Commands,
		catalogue: slices.Clone(cfg.Catalogue), admissions: admissions, clock: systemClock,
	}, nil
}

func (l Limits) validate() error {
	if l.MaxWall <= 0 || l.MaxIterations <= 0 || l.MaxToolCalls <= 0 ||
		l.MaxInputTokens <= 0 || l.MaxOutputTokens <= 0 || l.MaxArtifactBytes <= 0 {
		return errors.New("agentkernel: every limit must be explicit and positive")
	}
	return nil
}

func validateCatalogue(catalogue []CommandSpec, hasRunner bool) error {
	if len(catalogue) > 0 && !hasRunner {
		return errors.New("agentkernel: a command catalogue needs a command runner")
	}
	names := map[string]bool{}
	for _, c := range catalogue {
		if !api.ValidIdentifier(c.Name) || names[c.Name] || len(c.Argv) == 0 ||
			c.Timeout < time.Second || c.Timeout%time.Second != 0 {
			return fmt.Errorf("agentkernel: command %q needs a unique name, argv and a whole positive number of seconds", c.Name)
		}
		names[c.Name] = true
	}
	return nil
}

// Execute runs one host invocation as one bounded kernel execution.
func (a *Adapter) Execute(ctx context.Context, req execution.Request) (execution.Result, error) {
	if ctx.Err() != nil {
		return a.notStarted(req, context.Cause(ctx))
	}
	now := time.Now()
	ctxDeadline, _ := ctx.Deadline()
	kreq, err := a.translate(req, now, ctxDeadline)
	if err != nil {
		return a.refused(req), err
	}
	if !kreq.Budget.Deadline.After(now) {
		return a.notStarted(req, context.DeadlineExceeded)
	}
	return a.run(ctx, req, kreq)
}

func (a *Adapter) run(ctx context.Context, req execution.Request, kreq api.ExecutionRequest) (execution.Result, error) {
	state := newAttempt(req.AttemptRef(), a.transcript, execution.ProgressRecorder(ctx), a.clock)
	w := newWorkers()
	defer w.stop()
	// The kernel context ends only through supervise, which keeps the host
	// cause. Its deadline is already in the kernel budget.
	kctx, kcancel := context.WithCancelCause(context.WithoutCancel(ctx))
	defer kcancel(nil)
	supervise(w, ctx, kcancel, req.Budgets.InactivityLimit, state)
	eng, err := a.engine(w, state, req, kreq.ExecutionID)
	if err != nil {
		return a.refused(req), err
	}
	started := time.Now()
	kres, err := eng.Execute(kctx, kreq)
	completed := time.Now()
	w.stop()
	if err != nil {
		return a.refused(req), fmt.Errorf("agentkernel: engine: %w", err)
	}
	state.recordFinal()
	var hostCause error
	if kctx.Err() != nil {
		hostCause = context.Cause(kctx)
	}
	return a.result(req, kreq, kres, state, hostCause, started, completed)
}

// engine composes the kernel for one invocation: kernel file tools on the
// candidate, the command tool over the host runner, and every hand-off
// channel served by this invocation's workers.
func (a *Adapter) engine(w *workers, s *attempt, req execution.Request, execID string) (*engine.Engine, error) {
	ws, err := tools.NewWorkspace(req.CandidateDir)
	if err != nil {
		return nil, fmt.Errorf("agentkernel: candidate workspace: %w", err)
	}
	readFiles, err := ws.ReadFiles()
	if err != nil {
		return nil, err
	}
	toolset := []tools.Tool{ws.ReadFile(), ws.Search(), readFiles, ws.WriteFile(), ws.ApplyPatch()}
	if a.commands != nil {
		command, err := tools.NewCommand(serve(w, s.runCommand(a.commands, req.ScratchDir)), req.CandidateDir)
		if err != nil {
			return nil, err
		}
		toolset = append(toolset, command)
	}
	broker, err := tools.NewBroker(toolset...)
	if err != nil {
		return nil, err
	}
	artifacts, err := storage.OpenFileArtifacts(filepath.Join(a.stateDir, "artifacts", execID), a.limits.MaxArtifactBytes)
	if err != nil {
		return nil, fmt.Errorf("agentkernel: artifacts: %w", err)
	}
	return engine.New(engine.Config{
		Providers:   map[string]chan<- api.ProviderCall{a.binding.ID: serve(w, s.complete(a.provider))},
		Broker:      broker,
		Artifacts:   artifacts,
		Events:      serve(w, s.recordEvent),
		Clock:       api.SystemClock{},
		OutputLimit: toolOutputLimit,
		Admissions:  a.admissions,
	})
}

func (a *Adapter) result(req execution.Request, kreq api.ExecutionRequest, kres api.ExecutionResult, s *attempt,
	hostCause error, started, completed time.Time) (execution.Result, error) {
	perr, reached, executed, events := s.observed()
	outcome, class := classify(kres.Termination, perr, hostCause)
	tokens, cost := usage(kres.Usage)
	res := execution.Result{
		ProviderID: a.agentID, Model: a.binding.Model, AuthMode: a.authMode, Attempt: req.Attempt,
		Outcome: outcome, Tokens: tokens, CostMicros: cost, Executed: executed,
	}
	unsure := uncertain(kres)
	if reached || unsure {
		deadline := kreq.Budget.Deadline
		res.Invocation = &execution.InvocationProvenance{
			AgentID: a.agentID, ProviderKind: a.providerKind, TrustMode: a.trustMode, Model: a.binding.Model,
			InvocationObservation: domain.InvocationObservation{
				Executable: "agentkernel", Version: api.KernelVersion, AuthMode: a.authMode, WorkspaceBound: true,
				Deadline: &deadline, StartedAt: &started, CompletedAt: &completed, Elapsed: completed.Sub(started),
				OverranDeadline:  completed.After(deadline),
				TerminationCause: string(ownerOf(kres.Termination, hostCause)),
				InactivityLimit:  req.Budgets.InactivityLimit, ProgressMode: progressMode, StructuredEvents: events,
			},
		}
	}
	if outcome == execution.Succeeded {
		res.Answer = kres.FinalText
		return res, nil
	}
	res.Failure = &execution.Failure{Classification: class}
	return res, &TerminationError{Termination: kres.Termination, ProviderError: perr, UncertainSideEffect: unsure}
}

// refused is the result of a request nothing ran for: no invocation, nothing
// executed, and a class that stops rather than retries or waits.
func (a *Adapter) refused(req execution.Request) execution.Result {
	return execution.Result{
		ProviderID: a.agentID, Model: a.binding.Model, AuthMode: a.authMode, Attempt: req.Attempt,
		Outcome: execution.Failed, Failure: &execution.Failure{Classification: execution.FailureUnknown},
	}
}

func (a *Adapter) notStarted(req execution.Request, cause error) (execution.Result, error) {
	ns := &execution.NotStartedError{Cause: cause}
	return execution.NotStartedResult(a.agentID, a.binding.Model, a.authMode, req.Attempt, ns), ns
}

// MissingTools answers which required tools the kernel could not run here:
// one with no catalogue command, or one the host's command boundary cannot
// resolve. Without a runner every tool is missing. It is the
// ToolchainProber capability the host asserts (#522).
func (a *Adapter) MissingTools(ctx context.Context, required []string) []string {
	var missing []string
	for _, tool := range required {
		if !a.catalogued(tool) || !a.resolves(ctx, tool) {
			missing = append(missing, tool)
		}
	}
	return missing
}

func (a *Adapter) catalogued(tool string) bool {
	return slices.ContainsFunc(a.catalogue, func(c CommandSpec) bool { return c.Argv[0] == tool })
}

func (a *Adapter) resolves(ctx context.Context, tool string) bool {
	if a.commands == nil {
		return false
	}
	out, err := a.commands.Run(ctx, Command{Argv: []string{"sh", "-c", `command -v "$1"`, "sh", tool}, Timeout: probeTimeout})
	return err == nil && out.ExitCode == 0
}
