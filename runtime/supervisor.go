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
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// SupervisorDependencies is the complete input. Like the runtime's own
// dependency set, every external system is a seam and nothing is discovered
// from ambient state.
type SupervisorDependencies struct {
	// WorkAdmissionWithheld constructs the supervisor unable to take on work.
	// It is how the successor half of a controller handoff holds the scheduler
	// while it revalidates and proves itself: ownership is permission to
	// perform the transition, and never permission to serve.
	WorkAdmissionWithheld bool
	Store                 *SQLiteOperationStore
	Clock                 Clock
	Owner                 string
	Liveness              OwnerLiveness
	// StateDir is the runtime state directory holding each run's workspace. The
	// plan reconciler reads it ONLY to prove whether one upstream candidate
	// contains another, in the producer's own clone. Absent, that relationship
	// cannot be proven and a stage consuming several candidates blocks - which
	// is the safe direction.
	StateDir string
	// Runtime builds the engine for one repository worked by one agent.
	//
	// It takes BOTH because a supervisor is not single-agent. The whole point
	// of the milestone is that #123 can be worked by codex while #124 is worked
	// by claude, under one process; an engine bound to a single agent would
	// have driven every run with whichever worker the supervisor happened to be
	// started with, silently overriding each run's own durable binding.
	//
	// It is a factory rather than a value because a supervisor governs several
	// repositories and several agents, and each pairing needs its own engine
	// over the SAME store and credentials.
	Runtime func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error)
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
	// AgentProber builds the readiness probe an explicit Submit agent selection
	// is checked against, so a configured-but-uninvocable agent is refused with
	// the same reason `doctor` already reports for it rather than accepted and
	// left to fail deep inside the run it starts. Nil disables the check.
	AgentProber func(ResolvedAgent) AgentProber
	// Plans is the plan lifecycle service, or the zero value when no plan has
	// ever been proposed. It is what the plan reconciler resolves assignments
	// through, and it contacts nothing.
	Plans PlanService
}

// SupervisorReport is one tick's account of what the supervisor did.
type SupervisorReport struct {
	// Reconciliation is what this pass did about the controller's own state,
	// or why it did nothing. It rides the existing report rather than a new
	// channel: an operator reading a tick should see the whole tick.
	Reconciliation *ReconciliationAttempt `json:"reconciliation,omitempty"`
	// Upgrade is what this pass did about replacing this controller with the
	// successor trusted main names. Its Superseded() is how serve learns that
	// this process has given up the role and must stop.
	Upgrade *ControllerUpgradeAttempt `json:"upgrade,omitempty"`
	At      time.Time                 `json:"at"`
	// Driven is the runs whose driving FINISHED and was noticed by this pass.
	// A run started here and finished here appears here, as it always did; a
	// run whose provider spans several passes appears in the pass it finished
	// in, because a pass starts work rather than containing it.
	Driven []RunOutcome `json:"driven,omitempty"`
	// Observed is the feedback polls performed, one per active published run.
	Observed []FeedbackObservation `json:"observed,omitempty"`
	// DiscoveryError is why automatic intake could not run this tick, when it
	// could not. It is separate from Error because the supervisor is still
	// healthy: every run it already owns is driven as usual and only new intake
	// is lost, which is a different thing for an operator to act on.
	DiscoveryError string `json:"discovery_error,omitempty"`
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
	// Plans is what the plan reconciler did this tick, one entry per plan it
	// looked at. A plan awaiting approval appears here saying so, which is how
	// an operator sees that the runtime is waiting on THEM rather than on work.
	Plans []PlanTickReport `json:"plans,omitempty"`
	// Error is a tick that could not enumerate work. It is REPORTED rather
	// than returned, because a supervisor that exited on one unreadable read
	// would take every healthy run down with it - the same isolation rule that
	// already applies to a single failing run, applied to the tick itself.
	Error string `json:"error,omitempty"`
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
	// FeedbackError is why this run's feedback could not be observed, when it
	// could not be. It is reported separately from Error because the run itself
	// is fine: driving continues, and what is lost is only the chance to notice
	// a new review this tick.
	//
	// It exists because silence was ambiguous. A forge that has stopped
	// answering and a pull request nobody has commented on produced exactly the
	// same output, so an operator waiting for their review to reach a worker
	// had no way to tell "nothing was said" from "nothing could be heard".
	FeedbackError string `json:"feedback_error,omitempty"`
}

// Supervisor owns the persistent runtime for one state directory.
type Supervisor struct {
	deps SupervisorDependencies

	// mu guards the lifecycle flags and the scheduling bookkeeping: what is
	// draining, where the rotation is, which runs are in flight and which
	// outcomes have not been reported yet. Driving a run happens entirely
	// outside it, so a slow run never blocks an operator command.
	mu sync.Mutex
	// admission is the work-admission gate. It replaced a draining flag that
	// was read under mu and released before the run was created, which made
	// "after the drain returns nothing new is admitted" false by construction.
	// See work_admission.go.
	admission *workAdmissionGate
	// reconciler maintains this controller's own generation state, one attempt
	// per pass. It is bound AFTER construction because the cycle is real: the
	// supervisor owns the admission gate, the controller service needs that
	// gate, and the reconciler needs the service. Binding once at startup
	// resolves it without a second gate, a second lease or a factory callback.
	reconciler *ControllerReconciler
	// upgrade replaces this controller with the successor trusted main names.
	// It is bound after construction for the same cycle as the reconciler: it
	// reaches the controller service, which needs this supervisor's gate.
	upgrade *ControllerUpgrade
	// cursor is the rotation offset into the ACTIVE-run ring. It exists so a
	// ceiling smaller than the active set is a rate limit rather than a fixed
	// prefix; see admit.
	cursor int
	// inflight is the runs being driven right now, by run id. A pass does not
	// start a run that is already in one - driving a run twice would be two
	// workers on one journal - and it does not treat its slot as free either.
	//
	// It is what makes the ceiling hold across passes now that driving outlives
	// the pass that started it. The durable count in AcquireOperation remains
	// the authority on concurrency; this bounds the goroutines, which is the
	// same thing MaxConcurrentRuns always bounded, counted over the right
	// interval.
	inflight map[string]struct{}
	// landed is the outcomes of runs that finished driving and have not been
	// reported yet, with the feedback observations that came with them. A run
	// whose provider spans several passes is reported by the pass it finished
	// in rather than the pass that started it, because those are no longer the
	// same pass.
	landed         []RunOutcome
	landedFeedback []FeedbackObservation
	// driving counts the run goroutines in flight. Nothing waits on it per
	// pass; shutdown waits on it once, which is what keeps dispatching without
	// waiting from leaking a goroutine past the process that owns it.
	driving sync.WaitGroup
	// plansMu serializes everything that WRITES plan state: the reconciler's
	// own pass and an operator decision arriving over the control endpoint.
	// They read a snapshot and then append against it, so running them side by
	// side means one can decide from state the other is changing - a stage
	// started inside the window survives a supersession that changed it, and
	// the invalidations are computed against a plan that has already moved.
	plansMu sync.Mutex
	// enginesMu guards the engine cache, which several run goroutines reach
	// concurrently inside one tick. It is separate from mu on purpose: the
	// lifecycle flags are read by operator commands, and a slow engine
	// construction must not block those.
	enginesMu sync.Mutex
	// engines caches one repository-bound engine per repository. Building one
	// opens nothing and contacts nothing, but caching keeps a tick from
	// rebuilding the same value for every run.
	engines map[string]*engineSlot
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
	// A registry that resolves no default cannot drive anything: every run is
	// bound to an agent, and a legacy run resolves the default. Failing here
	// rather than on the first piece of work means an operator learns it from
	// `serve` refusing to start, not from a run that mysteriously never moves.
	if _, err := d.Agents.Agent(""); err != nil {
		return nil, &DependencyError{Detail: "a supervisor needs a usable agent registry: " + err.Error()}
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
	return &Supervisor{
		deps: d, engines: map[string]*engineSlot{}, inflight: map[string]struct{}{},
		// A supervisor admits work from the start unless it is the successor
		// half of a handoff, which holds the scheduler while it proves itself
		// and is opened by EnableWorkAdmission once the durable record says it
		// is the active generation.
		admission: newWorkAdmissionGate(!d.WorkAdmissionWithheld),
	}, nil
}

// engine returns the engine for one repository worked by one agent, refusing a
// repository this supervisor was not constructed to govern.
//
// An EMPTY agent id resolves the operator's default. That is the documented
// legacy meaning of a run created before the agent registry existed: it was
// worked by whichever single provider the configuration named at the time, and
// the default is the closest honest successor to that.
// engineSlot is one cache entry. Its once is what makes a cache MISS
// single-flight without holding the cache lock across the build.
type engineSlot struct {
	once   sync.Once
	engine *EngineeringRuntime
	err    error
}

func (s *Supervisor) engine(identity, agentID string) (*EngineeringRuntime, error) {
	agent, err := s.deps.Agents.Agent(agentID)
	if err != nil {
		return nil, err
	}
	governed, ok := s.governedRepository(identity)
	if !ok {
		return nil, fmt.Errorf("repository %q is not governed by this supervisor; enrolment is operator configuration, not a request", identity)
	}
	key := identity + "|" + agent.ID

	// The cache lock is held only to find or reserve the slot. Building the
	// engine happens OUTSIDE it, because the factory is not the cheap
	// constructor this cache once assumed: under `serve` it resolves the
	// publication identity, which is a live forge request. Holding the mutex
	// across that call serialized every run goroutine in a tick and every
	// operator submission behind one slow GitHub response.
	s.enginesMu.Lock()
	slot, cached := s.engines[key]
	if !cached {
		slot = &engineSlot{}
		s.engines[key] = slot
	}
	s.enginesMu.Unlock()

	// One build per key even when several goroutines miss at once; the others
	// wait for that build rather than starting their own.
	slot.once.Do(func() { slot.engine, slot.err = s.deps.Runtime(governed, agent) })
	if slot.err != nil {
		// A failed build is not cached forever: a forge that was unreachable
		// for one tick must not make this pairing permanently unusable.
		s.enginesMu.Lock()
		if s.engines[key] == slot {
			delete(s.engines, key)
		}
		s.enginesMu.Unlock()
		return nil, slot.err
	}
	return slot.engine, nil
}

// GovernedRepository is governedRepository for the composition root, which
// needs it to refuse control requests naming a repository this supervisor does
// not govern. A control request selects among what the operator enrolled; it
// can never introduce a repository.
func (s *Supervisor) GovernedRepository(identity string) (GitHubRepo, bool) {
	return s.governedRepository(identity)
}

// governedRepository resolves an identity to an ENROLLED repository. Enrolment
// is operator configuration, so this answers from the configured set and never
// from the request.
func (s *Supervisor) governedRepository(identity string) (GitHubRepo, bool) {
	for _, repo := range s.deps.Repositories {
		if strings.EqualFold(repo.String(), identity) {
			return repo, true
		}
	}
	return GitHubRepo{}, false
}

// Submit is the explicit operator intake path: one issue, one agent, one
// durable run. It creates the run and returns immediately; the tick loop drives
// it, which is what removes the driving terminal from the operator's workflow.
func (s *Supervisor) Submit(ctx context.Context, request ControlRequest) (StartOutcome, error) {
	if request.Issue <= 0 {
		return StartOutcome{}, fmt.Errorf("issue number must be positive, got %d", request.Issue)
	}
	// The agent is resolved against the OPERATOR's registry, and the run is
	// created through an engine bound to THAT agent. A control request selects
	// among agents the operator configured; it can never introduce an
	// executable, a trust mode or a credential.
	agent, err := s.deps.Agents.Agent(request.Agent)
	if err != nil {
		return StartOutcome{}, err
	}
	// Only an EXPLICIT selection is checked: a request naming no agent resolves
	// the operator's default, and a submission must keep working even when that
	// default happens to be uninstalled, exactly as it does today.
	if strings.TrimSpace(request.Agent) != "" && s.deps.AgentProber != nil {
		if err := RefuseUnlessInvocable(ctx, agent, s.deps.AgentProber(agent)); err != nil {
			return StartOutcome{}, err
		}
	}
	engine, err := s.engine(request.Repository, agent.ID)
	if err != nil {
		return StartOutcome{}, err
	}
	mode := AdoptCompatibleGeneration
	if request.NewGeneration {
		mode = NewGeneration
	}
	// THE DECISION AND THE COMMIT UNDER ONE LOCK. Creating the run inside the
	// gate is what makes a drain that has returned mean something: an
	// admission either committed before the close or never happens.
	var outcome StartOutcome
	err = s.admission.admit(func() error {
		created, startErr := engine.StartIssueRun(ctx, request.Issue, mode)
		outcome = created
		return startErr
	})
	return outcome, err
}

// BindControllerReconciler installs the controller-maintenance loop. It is
// STARTUP-ONLY and refuses replacement.
//
// The refusal is the point. A supervisor whose reconciler could be swapped
// while running would be a mechanism for changing controller semantics at
// runtime, which is a governance surface nobody asked for and nothing
// authorizes. Binding once is composition; rebinding would be policy.
func (s *Supervisor) BindControllerReconciler(reconciler *ControllerReconciler) error {
	if reconciler == nil {
		return fmt.Errorf("a controller reconciler is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reconciler != nil {
		return fmt.Errorf("a controller reconciler is already bound to this supervisor")
	}
	s.reconciler = reconciler
	return nil
}

// QuiesceWorkForTransition suspends intake and waits until no work this
// controller started can still append to a journal.
//
// IT IS THE PRECONDITION FOR GIVING UP THE ROLE, and it is two facts rather
// than one. Closing intake stops runs being CREATED; it says nothing about the
// goroutines already inside driveOne, which are in a provider, a workspace or a
// journal write and will finish whatever the gate says. A predecessor that
// released the role with those still running would have a successor activate
// and serve while the previous controller was still writing durable state -
// which is not a stale pointer or a lost update, it is two controllers
// appending to one store.
//
// THE HOLD IS REVERSIBLE BECAUSE THIS HAPPENS BEFORE COMMITMENT. The successor
// evaluates the quiescent head next and may refuse it; that must cost the
// update rather than this controller's ability to serve. The returned release
// puts intake back, and does nothing once the transition has closed the gate on
// its way past the point of no return.
//
// It is bounded. A provider invocation of tens of minutes is ordinary, and an
// unbounded wait would hold intake for as long as the slowest run in the
// fleet - so a wait that does not settle gives intake back and lets the next
// attempt try again, which converges: nothing new is admitted while a hold is
// in place, so the in-flight set only shrinks.
func (s *Supervisor) QuiesceWorkForTransition(ctx context.Context, within time.Duration) (func(), error) {
	release, held := s.admission.hold("a controller transition is evaluating the state its successor would inherit")
	if !held {
		return nil, &WorkAdmissionRefusedError{Reason: "this controller is not admitting work, so it has no intake to suspend for a transition"}
	}
	quiet := make(chan struct{})
	go func() {
		s.driving.Wait()
		close(quiet)
	}()
	timer := time.NewTimer(within)
	defer timer.Stop()
	select {
	case <-quiet:
		return release, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	case <-timer.C:
		release()
		return nil, fmt.Errorf("work this controller started was still running after %s, so the role was not given up", within)
	}
}

// BindControllerUpgrade installs the trusted-main upgrade. Like the
// reconciler it is STARTUP-ONLY and refuses replacement: a supervisor whose
// upgrade path could be swapped while running would be a way to change which
// controller replaces this one, at runtime, with nothing authorizing it.
func (s *Supervisor) BindControllerUpgrade(upgrade *ControllerUpgrade) error {
	if upgrade == nil {
		return fmt.Errorf("a controller upgrade is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.upgrade != nil {
		return fmt.Errorf("a controller upgrade is already bound to this supervisor")
	}
	s.upgrade = upgrade
	return nil
}

func (s *Supervisor) controllerUpgrade() *ControllerUpgrade {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upgrade
}

func (s *Supervisor) controllerReconciler() *ControllerReconciler {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconciler
}

// Drain stops accepting and starting work while letting started work finish.
// It is reversible only by restarting the supervisor, which is deliberate: a
// drain is an operator saying "wind this down", and un-draining silently would
// make that instruction meaningless.
func (s *Supervisor) Drain() {
	// Closing waits for admissions already in progress, which is the point: a
	// drain that returned while a run was being written down would be a drain
	// that did not drain.
	s.admission.close("the supervisor is draining and is not accepting new work")
}

// Draining reports the current lifecycle state.
func (s *Supervisor) Draining() bool { return s.admission.closed() }

// StopAll is the one lifecycle action that actually CANCELS runs. It is
// journalled per run through the same single cancellation path `stop RUN` uses,
// so a fleet cancellation is indistinguishable in the journal from cancelling
// each run individually - because that is exactly what it is.
// WithPlanLock runs one operator plan decision under the same lock the plan
// reconciler holds, so a decision and a tick never interleave their
// read-then-append. It is exported because the composition root - which owns
// the control endpoint - is where an operator's decision arrives.
func (s *Supervisor) WithPlanLock(do func() error) error {
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	return do()
}

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

// Tick is one scheduling pass followed by a wait for everything still being
// driven, which is what a single-shot caller means by "tick": start the work
// and tell me how it went.
//
// The LOOP deliberately does not use it - see Run - because a tick is a
// scheduling PASS and not a unit of work. Nothing about admission, fairness or
// the ceiling lives in the waiting; the wait exists only so that a caller with
// no loop to come back on still gets an account of what it started.
func (s *Supervisor) Tick(ctx context.Context) (SupervisorReport, error) {
	report, err := s.pass(ctx)
	s.driving.Wait()
	s.collect(&report)
	return report, err
}

// pass is one scheduling pass: optional discovery, plan reconciliation, and the
// admission of as many active runs as the operator ceiling still has room for.
// It DISPATCHES and returns.
//
// It does not wait, and that is the whole point. A run's driving is as long as
// its provider - tens of minutes is ordinary - and a pass that waited for the
// slowest one could not make the next pass either, so a run submitted while a
// provider was executing sat at run.created with a free slot beside it until
// that provider returned. Admission was tick-granular while a tick was as long
// as its slowest run.
//
// A per-run failure is REPORTED, never returned. One run whose forge call
// failed, whose provider is unavailable or whose workspace is broken must not
// stop the supervisor or affect a sibling; that isolation is the whole reason
// several tasks can share one process.
func (s *Supervisor) pass(ctx context.Context) (SupervisorReport, error) {
	now := s.deps.Clock.Now()
	report := SupervisorReport{
		At: now, Draining: s.Draining(), Capacity: s.deps.MaxConcurrentRuns,
		NextEligibleAt: now.Add(s.deps.PollInterval),
	}
	// THIS CONTROLLER'S OWN STATE COMES FIRST, AND OUTSIDE THE INTAKE SECTION.
	//
	// Both of these run even while draining - draining stops taking on WORK,
	// not repairing a stale pointer or resuming an activation this process
	// already holds - and neither may run underneath the admission section
	// below. A transition SUSPENDS and then CLOSES intake, which takes the
	// gate's lock exclusively; doing that from inside a section holding it for
	// reading is a self-deadlock, and it is the kind that appears on the first
	// real upgrade rather than in a test.
	//
	// Reconciliation before upgrade: a controller that is not in the state its
	// own records describe repairs that first, because handing the role to a
	// successor is not a way to resolve a transition this process has not
	// finished.
	if reconciler := s.controllerReconciler(); reconciler != nil {
		attempt := reconciler.Attempt(now)
		report.Reconciliation = &attempt
	}
	if upgrade := s.controllerUpgrade(); upgrade != nil {
		attempt := upgrade.Attempt(ctx, now)
		report.Upgrade = &attempt
		if attempt.Superseded() {
			// THE PASS ENDS HERE. This process has given up the role, so every
			// remaining step - discovery, plans, enumerating runs, dispatching
			// them - would be a controller scheduling work it no longer has
			// the authority to schedule. Stopping structurally is worth more
			// than stopping because a caller read the report and reacted.
			report.Draining = true
			s.collect(&report)
			return report, nil
		}
	}
	// INTAKE IS HELD OPEN ACROSS THE WHOLE SECTION. Discovery and plan
	// reconciliation both CREATE runs, so checking the gate once and then
	// creating work afterwards would be the defect this gate exists to close,
	// one layer up from Submit.
	release, admissionErr := s.admission.section()
	if admissionErr == nil {
		defer release()
	} else {
		report.Draining = true
	}
	if s.deps.Discovery != nil && !report.Draining {
		discovery, err := s.deps.Discovery.Tick(ctx)
		if err != nil {
			// Silence is ambiguous. A misconfigured label or a permanently
			// failing poll used to be invisible forever - the operator saw a
			// supervisor discovering nothing and could not tell that from a
			// repository with nothing to discover. Reported separately from
			// report.Error because the supervisor itself is fine: the runs it
			// already owns keep being driven, and what is lost is intake.
			report.DiscoveryError = boundedDetail(err.Error())
		} else {
			report.Discovery = &discovery
		}
	}
	// PLANS are reconciled BEFORE the run list is read, so a stage that became
	// dependency-ready since the last tick gets its run created and then driven
	// in the SAME pass rather than waiting a whole poll interval. Reading the
	// runs first would enumerate the fleet as it was before the plan added to
	// it.
	//
	// This is dependency gating, not scheduling: it creates or associates
	// ordinary EngineeringRuns and stops. Everything below - the ceiling, the
	// rotation, the leases - is unchanged and remains the only thing that
	// decides when a run executes.
	if !report.Draining {
		report.Plans = s.reconcilePlans(ctx)
	}
	runs, err := s.deps.Store.Runs()
	if err != nil {
		report.Error = boundedDetail(err.Error())
		s.collect(&report)
		return report, nil
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
		// call finishes in the goroutine that owns it and is reported by the
		// pass it lands in; work that has not started does not begin.
		s.collect(&report)
		return report, nil
	}
	// Oldest first, then ROTATED between passes.
	//
	// A run is non-terminal for its whole lifetime, not only while it is inside
	// Reconcile, so age order alone is not a queue - it is a fixed prefix. With
	// a ceiling of one and one long-lived active run, that run would be the
	// only one ever driven and every later submission would wait for it to
	// finish. The multi-task workflow this supervisor exists for would have
	// been one task at a time wearing a fleet view.
	//
	// Rotating the starting offset each pass gives every active run a turn
	// while still bounding how many are driven at once. Ordering stays
	// deterministic within a pass; only the entry point moves.
	sort.SliceStable(active, func(i, j int) bool { return active[i].CreatedAt.Before(active[j].CreatedAt) })

	for _, run := range s.admit(active) {
		s.driving.Add(1)
		go func(run EngineeringRun) {
			defer s.driving.Done()
			outcome, observation := s.driveOne(ctx, run)
			s.mu.Lock()
			defer s.mu.Unlock()
			// The slot is released and the outcome parked in the same step, so
			// there is no instant in which a finished run is neither occupying
			// capacity nor accounted for.
			delete(s.inflight, run.ID)
			s.landed = append(s.landed, outcome)
			if observation != nil {
				s.landedFeedback = append(s.landedFeedback, *observation)
			}
		}(run)
	}
	s.collect(&report)
	return report, nil
}

// admit decides which runs this pass starts, and reserves their slots. It is
// the whole fairness mechanism: no priority, no weighting, no starvation.
//
// A run already being driven is neither a candidate nor free capacity: driving
// it again would put two workers on one journal, and counting its slot as free
// would admit past the ceiling now that driving outlives a pass. What is left
// is the room the ceiling still has, and this sweep fills it.
//
// The cursor indexes the ACTIVE ring - every non-terminal run - and the sweep
// STEPS OVER the ones already in flight. It is deliberately not an index into
// the dispatchable subset, which is the same mistake as walking a list while
// deleting from it: that subset shrinks and grows by the in-flight set every
// pass, so the same integer names a different run each time and (cursor, size)
// can settle into a cycle that never lands on one of them. Four runs at a
// ceiling of two, with stable per-run durations - three polling, two executing,
// the ordinary mixed fleet - starved one poller forever while every pass had
// room and handed it to somebody else. The active ring changes only when a run
// is created or settles, so the windows tile it and every run is reached within
// one sweep of it.
//
// Selecting and reserving happen under one lock because they are one decision.
func (s *Supervisor) admit(active []EngineeringRun) []EngineeringRun {
	s.mu.Lock()
	defer s.mu.Unlock()
	room := s.deps.MaxConcurrentRuns - len(s.inflight)
	if room <= 0 || len(active) == 0 {
		// The cursor does NOT advance on a pass that started nothing. Advancing
		// past runs it never considered is how a sweep skips one.
		return nil
	}
	start := s.cursor % len(active)
	selected := make([]EngineeringRun, 0, room)
	considered := 0
	for i := 0; i < len(active) && len(selected) < room; i++ {
		considered = i + 1
		run := active[(start+i)%len(active)]
		if _, busy := s.inflight[run.ID]; busy {
			continue
		}
		selected = append(selected, run)
		s.inflight[run.ID] = struct{}{}
	}
	// The next pass resumes after the last run this one LOOKED AT, whether it
	// started that run or stepped over it, so the sweep keeps moving forward.
	s.cursor = (start + considered) % len(active)
	return selected
}

// collect moves the outcomes of finished runs into the report of the pass that
// noticed them, which is how an operator still sees every run's result in the
// stream now that a result does not necessarily belong to the pass that started
// the run.
func (s *Supervisor) collect(report *SupervisorReport) {
	s.mu.Lock()
	report.Driven = append(report.Driven, s.landed...)
	report.Observed = append(report.Observed, s.landedFeedback...)
	s.landed, s.landedFeedback = nil, nil
	s.mu.Unlock()
	sort.SliceStable(report.Driven, func(i, j int) bool { return report.Driven[i].RunID < report.Driven[j].RunID })
	sort.SliceStable(report.Observed, func(i, j int) bool { return report.Observed[i].RunID < report.Observed[j].RunID })
}

// driveOne observes feedback for one run and then reconciles it. The order
// matters: admission journals what a worker may see, and the reconcile pass
// that follows is what acts on it, so a review left between two ticks is picked
// up by the next one without any second scheduling concept.
func (s *Supervisor) driveOne(ctx context.Context, run EngineeringRun) (RunOutcome, *FeedbackObservation) {
	result := RunOutcome{RunID: run.ID, Repo: run.Repository, Agent: run.AgentID, State: run.Disposition}
	// The run's OWN durable agent binding decides which worker continues it.
	// Driving it with the supervisor's own agent would be a silent provider
	// handoff on every tick - the exact thing the binding exists to prevent.
	engine, err := s.engine(run.Repository, run.AgentID)
	if err != nil {
		result.Error = boundedDetail(err.Error())
		return result, nil
	}
	var observed *FeedbackObservation
	observation, feedbackErr := engine.ObserveFeedback(ctx, run.ID)
	if feedbackErr != nil {
		result.FeedbackError = boundedDetail(feedbackErr.Error())
	} else {
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

// Run is the supervisor loop: pass, wait out the poll interval, repeat, until
// the context is cancelled.
//
// It waits out the POLL INTERVAL rather than the work, which is the difference
// between a fleet that admits a submission as soon as it has a slot and one
// that admits it when the slowest provider happens to return. A pass that
// starts a twenty-minute run is followed by the next pass one poll interval
// later, with the ceiling counted across both.
//
// Cancelling the context is a SHUTDOWN, not a fleet cancellation. It stops
// scheduling and propagates through the same bounded cancellation providers and
// the Docker sandbox already honour; no run.cancelled is journalled, and every
// run stays exactly as resumable as its own journal says it is.
func (s *Supervisor) Run(ctx context.Context, report func(SupervisorReport)) error {
	// Shutdown drains. Work that outlived its pass is unwound by the same
	// context cancellation it always was, and this loop does not return until
	// that unwinding is finished - returning earlier would let `serve` exit
	// while runs were still mid-flight, which is the one thing shutdown is
	// defined not to do. The last outcomes are reported rather than dropped,
	// because an operator watching the stream should see how the work they
	// were told would be let finish actually finished.
	defer func() {
		s.driving.Wait()
		final := SupervisorReport{
			At: s.deps.Clock.Now(), Draining: s.Draining(), Capacity: s.deps.MaxConcurrentRuns,
		}
		s.collect(&final)
		if report != nil && (len(final.Driven) > 0 || len(final.Observed) > 0) {
			report(final)
		}
	}()
	for ctx.Err() == nil {
		tick, err := s.pass(ctx)
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

// decomposeWithAgent performs one decomposition invocation through the agent
// the stage's assignment resolved to.
//
// The workspace is materialized from the plan's exact subject revision and
// removed afterwards: it is derived state, and the snapshot it held is recorded
// in the resulting proposal's provenance.
// planningSeam is the reconciler's Planner, and the one place the plan lock is
// released across a provider call.
//
// The lock serializes read-then-append against an operator decision, which
// takes microseconds; holding it across a repository clone and a live planning
// invocation stalled every run in the fleet for minutes, because the tick takes
// the same lock before it drives anything.
//
// Releasing here is safe because the caller re-reads plan state afterwards and
// ABANDONS the pass if the approved revision moved - a decision that lands in
// the window is acted on rather than written over.
//
// It is a method rather than a closure inside the tick so a test can drive the
// real thing. The replica a test used to hand-write would have kept passing
// through any regression in this code.
func (s *Supervisor) planningSeam(repository string) func(context.Context, PlanDecompositionRequest) (PlannerOutput, error) {
	return func(ctx context.Context, request PlanDecompositionRequest) (PlannerOutput, error) {
		s.plansMu.Unlock()
		defer s.plansMu.Lock()
		return s.decomposeWithAgent(ctx, repository, request)
	}
}

func (s *Supervisor) decomposeWithAgent(ctx context.Context, repository string, request PlanDecompositionRequest) (PlannerOutput, error) {
	engine, err := s.engine(repository, request.Assignment.Agent.ID)
	if err != nil {
		return PlannerOutput{}, err
	}
	workspace, err := engine.MaterializePlanningWorkspace(request.Plan.ID, request.Plan.Subject.Revision)
	if err != nil {
		return PlannerOutput{}, err
	}
	defer workspace.Remove()

	// The ATTEMPT is deliberately not stated here. InvokePlanner derives it
	// from the durable transcript evidence, and hardcoding 1 made every retry
	// collide with the first attempt's create-once transcript: the provider ran
	// again - a real invocation, every tick - and then died storing its answer.
	// The wall bound the stage states - which the assigned profile may have
	// narrowed, and which the plan's remaining headroom may narrow further -
	// bounds the invocation itself. Computing it and not passing it made the
	// narrowing decorative.
	//
	// A stage that states no wall bound, under a plan that states none either,
	// still gets one: the operator's configured run wall limit, which is what
	// every producer invocation is bounded by. Zero here meant NO deadline at
	// all, so the one stage type that runs unattended against a provider was
	// the only one that could run forever.
	// The no-progress window applies to the PLANNER too, and it is the stage
	// that needs it most: a planning invocation runs unattended in a read-only
	// mode against the same CLIs, so a host that loses its network stalls it
	// exactly as it stalled the producer in #238. Leaving it out would have
	// bounded the path that is watched and not the one that is not.
	budgets := ProviderBudget{
		WallLimit:       engine.planningWallLimit(request.WallSeconds),
		InactivityLimit: engine.deps.Budgets.defaults().ProviderInactivityLimit,
	}
	return InvokePlanner(ctx, PlannerInput{
		PlanID: request.Plan.ID, Revision: request.Plan.Revision, Budgets: budgets,
		Agent: engine.PlanningAgent(), Provider: engine.PlanningProvider(),
		ProfileID: request.Assignment.Profile.ID, Model: request.Assignment.Agent.Model,
		Workspace: workspace, Contract: request.Contract,
		Objective:      request.Stage.Objective,
		Base:           Ref{Revision: request.Plan.Subject.Revision},
		SourceSnapshot: Ref{ID: request.Plan.ID, Revision: request.Plan.Digest},
		ControllerID:   engine.ControllerIdentityID(),
		Current:        &request.Plan,
		AvailableRoles: domain.EngineeringRoles(), AvailableCapabilities: domain.EngineeringCapabilities(),
		Artifacts: engine.PlanningArtifacts(),
	})
}

// reconcilePlans advances every plan the supervisor governs.
//
// A per-plan failure is REPORTED, never returned, for exactly the reason a
// per-run failure is: one plan whose agent is unavailable must not stop the
// runtime, and a plan that cannot resolve a stage is a state an operator acts
// on rather than an outage.
func (s *Supervisor) reconcilePlans(ctx context.Context) []PlanTickReport {
	if s.deps.Plans.Store == nil {
		return nil
	}
	s.plansMu.Lock()
	defer s.plansMu.Unlock()
	plans, err := s.deps.Store.Plans()
	if err != nil {
		return []PlanTickReport{{Waiting: boundedDetail(err.Error())}}
	}
	reports := make([]PlanTickReport, 0, len(plans))
	for _, plan := range plans {
		repository, issue, found, err := s.deps.Store.PlanSource(plan.ID)
		if err != nil {
			reports = append(reports, PlanTickReport{PlanID: plan.ID, Waiting: boundedDetail(err.Error())})
			continue
		}
		if !found || issue <= 0 {
			// A plan with no recorded source cannot create a run: every stage
			// run answers the source the plan was proposed for. Saying so is
			// more useful than silently skipping it.
			reports = append(reports, PlanTickReport{PlanID: plan.ID, Waiting: "no source issue is recorded for this plan"})
			continue
		}
		if _, governed := s.governedRepository(repository); !governed {
			reports = append(reports, PlanTickReport{PlanID: plan.ID, Waiting: "repository " + repository + " is not governed by this supervisor"})
			continue
		}
		reconciler := PlanReconciler{
			Store: s.deps.Store, Clock: s.deps.Clock, Service: s.deps.Plans,
			Repository: repository, Issue: issue, StateDir: s.deps.StateDir,
			Engine: func(repository, agentID string) (*EngineeringRuntime, error) {
				return s.engine(repository, agentID)
			},
			// A decomposition stage runs through the engine bound to the agent
			// its assignment resolved to, in the same verified non-mutating mode
			// the initial planner uses. The supervisor supplies the seam and
			// learns nothing about providers.
			Planner: s.planningSeam(repository),
		}
		report, err := reconciler.Reconcile(ctx, plan.ID)
		if err != nil {
			report.PlanID = plan.ID
			report.Waiting = boundedDetail(err.Error())
		}
		reports = append(reports, report)
	}
	return reports
}
