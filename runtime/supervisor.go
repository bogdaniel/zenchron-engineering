package runtime

// The Supervisor is the persistent local Engineering Runtime: one process that
// owns the scheduler, drives every active run, polls each repository once for
// all of them, and stays available to answer an operator while work continues.
//
// It is deliberately NOT a second runtime. There is no second task database, no
// second scheduler, no second authority system and no queue of its own: the
// durable EngineeringRuns in the existing store ARE the queue, and driving one
// is the same Reconcile an operator command has always called. What the
// supervisor adds is ownership, concurrency, a shared observation path and a
// lifecycle - nothing about what a run means.
//
// Three lifecycle operations are distinguished, because collapsing them is how
// a controller restart turns into a fleet of failed runs:
//
//	drain      stop taking and starting work; let started work finish
//	shutdown   stop scheduling and unwind in-flight work through cancellation;
//	           every run stays exactly as resumable as its journal says
//	stop-all   actually CANCEL the selected runs, journalled per run
//
// Only the third is run cancellation. Killing the supervisor is not.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// SupervisorDependencies is the complete input. Like the runtime's own
// dependency set, every external system is a seam and nothing is discovered
// from ambient state.
type SupervisorDependencies struct {
	Store    *SQLiteOperationStore
	Clock    Clock
	Owner    string
	Liveness OwnerLiveness
	// Runtime builds the engine for one repository. It is a factory rather than
	// a value because a supervisor governs several repositories and each needs
	// its own repository-bound engine over the SAME store, provider and
	// credentials.
	Runtime func(GitHubRepo) (*EngineeringRuntime, error)
	// Repositories is the set the supervisor may govern. A submission naming
	// anything else is refused: enrolment is operator authority, and a local
	// control request must not be able to introduce a repository.
	Repositories []GitHubRepo
	// MaxConcurrentRuns is the operator-authorized ceiling on runs driven at
	// once. The scheduler enforces the same ceiling durably; this bounds the
	// goroutines so the process does not start work it cannot lease.
	MaxConcurrentRuns int
	// PollInterval is how often a quiet supervisor looks again.
	PollInterval time.Duration
	// Discovery is the OPTIONAL automatic issue-intake policy. Nil means
	// disabled, which is the default for new configuration: an issue exists
	// because a project records work in issues, and its existence is not
	// consent to spend an operator's subscription on it.
	Discovery *WatchController
	// Agents is the registry a control request resolves an agent id against.
	Agents AgentRegistry
}

// SupervisorReport is one tick's account of what the supervisor did.
type SupervisorReport struct {
	At time.Time `json:"at"`
	// Driven is the runs reconciled in this tick.
	Driven []RunOutcome `json:"driven,omitempty"`
	// Observed is the feedback polls performed, one per active published run.
	Observed []FeedbackObservation `json:"observed,omitempty"`
	// Discovery is the optional intake report, present only when automatic
	// discovery is configured.
	Discovery *TickReport `json:"discovery,omitempty"`
	// Draining reports that the supervisor is finishing started work and
	// accepting none.
	Draining bool `json:"draining"`
	// Capacity is the operator ceiling and how much of it this tick used.
	Capacity int `json:"capacity"`
	Active   int `json:"active"`
	// NextEligibleAt is when the supervisor intends to look again.
	NextEligibleAt time.Time `json:"next_eligible_at"`
}

// RunOutcome pairs a run with what driving it settled on, plus the error if
// driving it failed. A failure to drive ONE run is reported, never propagated:
// a supervisor that stopped because one run's forge call failed would take
// every sibling down with it.
type RunOutcome struct {
	RunID   string      `json:"run_id"`
	Outcome Outcome     `json:"outcome,omitzero"`
	Error   string      `json:"error,omitempty"`
	Agent   string      `json:"agent,omitempty"`
	Repo    string      `json:"repository,omitempty"`
	State   Disposition `json:"disposition,omitempty"`
}

// Supervisor owns the persistent runtime for one state directory.
type Supervisor struct {
	deps SupervisorDependencies

	// mu guards the mutable lifecycle flags only. Run driving happens outside
	// it, so a slow run never blocks an operator command.
	mu       sync.Mutex
	draining bool
	// enginesMu guards the engine cache, which several run goroutines reach
	// concurrently inside one tick. It is separate from mu on purpose: the
	// lifecycle flags are read by operator commands, and a slow engine
	// construction must not block those.
	enginesMu sync.Mutex
	// engines caches one repository-bound engine per repository. Building one
	// opens nothing and contacts nothing, but caching keeps a tick from
	// rebuilding the same value for every run.
	engines map[string]*EngineeringRuntime
}

// NewSupervisor validates the dependency set.
func NewSupervisor(d SupervisorDependencies) (*Supervisor, error) {
	if d.Store == nil {
		return nil, &DependencyError{Detail: "a durable operation store is required"}
	}
	if d.Runtime == nil {
		return nil, &DependencyError{Detail: "a per-repository runtime factory is required"}
	}
	if strings.TrimSpace(d.Owner) == "" {
		return nil, &DependencyError{Detail: "a scheduler owner identity is required"}
	}
	if len(d.Repositories) == 0 {
		return nil, &DependencyError{Detail: "a supervisor governs a stated set of repositories, and none was given"}
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.PollInterval <= 0 {
		d.PollInterval = DefaultWatchPollSeconds * time.Second
	}
	// The ceiling is resolved through the SAME rule the scheduler uses, so a
	// supervisor can never drive more runs at once than the operator
	// authorized - and a request can only lower it.
	d.MaxConcurrentRuns = resolveMaxConcurrentRuns(d.MaxConcurrentRuns, d.MaxConcurrentRuns)
	return &Supervisor{deps: d, engines: map[string]*EngineeringRuntime{}}, nil
}

// engine returns the repository-bound engine, refusing a repository this
// supervisor was not constructed to govern.
func (s *Supervisor) engine(identity string) (*EngineeringRuntime, error) {
	s.enginesMu.Lock()
	defer s.enginesMu.Unlock()
	if engine, ok := s.engines[identity]; ok {
		return engine, nil
	}
	for _, repo := range s.deps.Repositories {
		if !strings.EqualFold(repo.String(), identity) {
			continue
		}
		engine, err := s.deps.Runtime(repo)
		if err != nil {
			return nil, err
		}
		s.engines[repo.String()] = engine
		return engine, nil
	}
	return nil, fmt.Errorf("repository %q is not governed by this supervisor; enrolment is operator configuration, not a request", identity)
}

// Submit is the explicit operator intake path: one issue, one agent, one
// durable run. It creates the run and returns immediately; the tick loop drives
// it, which is what removes the driving terminal from the operator's workflow.
func (s *Supervisor) Submit(ctx context.Context, request ControlRequest) (StartOutcome, error) {
	s.mu.Lock()
	draining := s.draining
	s.mu.Unlock()
	if draining {
		return StartOutcome{}, errors.New("the supervisor is draining and is not accepting new work")
	}
	if request.Issue <= 0 {
		return StartOutcome{}, fmt.Errorf("issue number must be positive, got %d", request.Issue)
	}
	engine, err := s.engine(request.Repository)
	if err != nil {
		return StartOutcome{}, err
	}
	// The agent is resolved against the OPERATOR's registry. A control request
	// selects among agents the operator configured; it can never introduce an
	// executable, a trust mode or a credential.
	if request.Agent != "" {
		agent, err := s.deps.Agents.Agent(request.Agent)
		if err != nil {
			return StartOutcome{}, err
		}
		if engine.deps.Agent.ID != agent.ID {
			return StartOutcome{}, fmt.Errorf("this supervisor drives agent %q; submit to a supervisor configured for %q, or start that run explicitly", engine.deps.Agent.ID, agent.ID)
		}
	}
	mode := AdoptCompatibleGeneration
	if request.NewGeneration {
		mode = NewGeneration
	}
	return engine.StartIssueRun(ctx, request.Issue, mode)
}

// Drain stops accepting and starting work while letting started work finish.
// It is reversible only by restarting the supervisor, which is deliberate: a
// drain is an operator saying "wind this down", and un-draining silently would
// make that instruction meaningless.
func (s *Supervisor) Drain() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
}

// Draining reports the current lifecycle state.
func (s *Supervisor) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draining
}

// StopAll is the one lifecycle action that actually CANCELS runs. It is
// journalled per run through the same single cancellation path `stop RUN` uses,
// so a fleet cancellation is indistinguishable in the journal from cancelling
// each run individually - because that is exactly what it is.
func (s *Supervisor) StopAll(reason string) ([]Outcome, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "operator_stop_all"
	}
	runs, err := s.deps.Store.Runs()
	if err != nil {
		return nil, err
	}
	scheduler := Scheduler{Store: s.deps.Store, Clock: s.deps.Clock, Owner: s.deps.Owner}
	var outcomes []Outcome
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			continue
		}
		outcome, err := CancelRun(s.deps.Store, scheduler, s.deps.Clock.Now(), run.ID, reason)
		if err != nil {
			return outcomes, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

// Tick drives one pass: optional discovery, then feedback observation and
// reconciliation for every active run, bounded by the operator ceiling.
//
// A per-run failure is REPORTED, never returned. One run whose forge call
// failed, whose provider is unavailable or whose workspace is broken must not
// stop the supervisor or affect a sibling; that isolation is the whole reason
// several tasks can share one process.
func (s *Supervisor) Tick(ctx context.Context) (SupervisorReport, error) {
	now := s.deps.Clock.Now()
	report := SupervisorReport{
		At: now, Draining: s.Draining(), Capacity: s.deps.MaxConcurrentRuns,
		NextEligibleAt: now.Add(s.deps.PollInterval),
	}
	if s.deps.Discovery != nil && !report.Draining {
		discovery, err := s.deps.Discovery.Tick(ctx)
		if err == nil {
			report.Discovery = &discovery
		}
	}
	runs, err := s.deps.Store.Runs()
	if err != nil {
		return report, err
	}
	active := make([]EngineeringRun, 0, len(runs))
	for _, run := range runs {
		if !terminalDisposition(run.Disposition) {
			active = append(active, run)
		}
	}
	report.Active = len(active)
	if report.Draining {
		// A draining supervisor starts nothing. Work already inside a Reconcile
		// call finishes because this function waits for it below; work that has
		// not started does not begin.
		return report, nil
	}
	// Oldest first, so a long-queued run is not starved by newer submissions.
	sort.SliceStable(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })
	if len(active) > s.deps.MaxConcurrentRuns {
		active = active[:s.deps.MaxConcurrentRuns]
	}

	var mu sync.Mutex
	var wait sync.WaitGroup
	for _, run := range active {
		wait.Add(1)
		go func(run EngineeringRun) {
			defer wait.Done()
			outcome, observation := s.driveOne(ctx, run)
			mu.Lock()
			defer mu.Unlock()
			report.Driven = append(report.Driven, outcome)
			if observation != nil {
				report.Observed = append(report.Observed, *observation)
			}
		}(run)
	}
	wait.Wait()
	sort.SliceStable(report.Driven, func(i, j int) bool { return report.Driven[i].RunID < report.Driven[j].RunID })
	sort.SliceStable(report.Observed, func(i, j int) bool { return report.Observed[i].RunID < report.Observed[j].RunID })
	return report, nil
}

// driveOne observes feedback for one run and then reconciles it. The order
// matters: admission journals what a worker may see, and the reconcile pass
// that follows is what acts on it, so a review left between two ticks is picked
// up by the next one without any second scheduling concept.
func (s *Supervisor) driveOne(ctx context.Context, run EngineeringRun) (RunOutcome, *FeedbackObservation) {
	result := RunOutcome{RunID: run.ID, Repo: run.Repository, Agent: run.AgentID, State: run.Disposition}
	engine, err := s.engine(run.Repository)
	if err != nil {
		result.Error = boundedDetail(err.Error())
		return result, nil
	}
	var observed *FeedbackObservation
	if observation, err := engine.ObserveFeedback(ctx, run.ID); err == nil {
		observed = &observation
	}
	outcome, err := engine.Reconcile(ctx, run.ID)
	if err != nil {
		result.Error = boundedDetail(err.Error())
		return result, observed
	}
	result.Outcome, result.State = outcome, outcome.Disposition
	return result, observed
}

// Run is the supervisor loop: tick, wait, repeat, until the context is
// cancelled.
//
// Cancelling the context is a SHUTDOWN, not a fleet cancellation. It stops
// scheduling and propagates through the same bounded cancellation providers and
// the Docker sandbox already honour; no run.cancelled is journalled, and every
// run stays exactly as resumable as its own journal says it is.
func (s *Supervisor) Run(ctx context.Context, report func(SupervisorReport)) error {
	for ctx.Err() == nil {
		tick, err := s.Tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if report != nil {
			report(tick)
		}
		if ctx.Err() != nil {
			return nil
		}
		delay := time.Until(tick.NextEligibleAt)
		if delay < time.Second {
			delay = time.Second
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
	return nil
}
