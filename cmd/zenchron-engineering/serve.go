package main

// The persistent product surface.
//
// `serve` starts the supervisor that owns the scheduler, drives every active
// run, polls each repository once for all of them, and stays available to
// answer an operator while the work continues. Every other command in this file
// is the operator's window onto that: which workers exist, what all the work is
// doing, what one worker is saying, and the three lifecycle actions that are
// deliberately not the same thing.
//
// This file is composition and translation only, exactly like autonomy.go: it
// wires real components and turns what the runtime returns into output and exit
// codes. The supervisor loop, the concurrency ceiling, the observation sharing
// and every decision about what to do next live in runtime/.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const serveUsage = "usage: zenchron-engineering serve [--repo owner/name] [--config <path>] [--agent <id>]"

// ---------------------------------------------------------------------------
// serve
// ---------------------------------------------------------------------------

// serveCommand runs the persistent supervisor until it is signalled.
//
// A signal is a SHUTDOWN. It stops scheduling and unwinds in-flight work
// through the cancellation the providers and the sandbox already honour; no
// run.cancelled is journalled and every run stays exactly as resumable as its
// own journal says. Cancelling runs is `autonomy stop RUN` and `autonomy
// stop-all`, and neither is what closing this process means.
func serveCommand(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	repositories, err := built.governedRepositories(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	// The control endpoint is opened BEFORE any work is driven, so a second
	// supervisor is refused before it starts competing for leases rather than
	// after.
	listener, err := runtime.ListenControl(built.config.StateDir)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer listener.Close()

	supervisor, err := built.supervisor(repositories)
	if err != nil {
		return runtime.ExitInvalid, err
	}

	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// The endpoint answers on its own goroutine so an operator command is not
	// queued behind a run that is mid-reconcile.
	go func() {
		// A failed Accept kills the control endpoint while the supervisor keeps
		// running. The socket file still exists, so SupervisorRunning keeps
		// answering true and every delegated command hangs to its deadline
		// instead of learning the endpoint is dead. An unreachable endpoint
		// also defeats `stop` and `stop-all`, so this stops the supervisor
		// rather than leaving it un-commandable.
		if err := listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return built.handleControl(ctx, supervisor, stopSignals, request)
		}); err != nil && !errors.Is(err, net.ErrClosed) {
			fmt.Fprintf(stdout, "the control endpoint stopped accepting connections: %v\n", err)
			stopSignals()
		}
	}()

	fmt.Fprintf(stdout, "zenchron-engineering serve\n")
	fmt.Fprintf(stdout, "  state directory   %s\n", built.config.StateDir)
	fmt.Fprintf(stdout, "  control endpoint  %s\n", listener.Path())
	fmt.Fprintf(stdout, "  mechanism         %s\n", runtime.ControlEndpointMechanism)
	fmt.Fprintf(stdout, "  agents            %s (default %s)\n", strings.Join(built.agents.IDs(), ", "), built.agents.Default())
	fmt.Fprintf(stdout, "  repositories      %s\n", strings.Join(repositoryNames(repositories), ", "))
	fmt.Fprintf(stdout, "  discovery         %s\n", discoveryDescription(built))

	err = supervisor.Run(ctx, func(report runtime.SupervisorReport) {
		_ = writeJSON(stdout, report)
	})
	if err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}

func repositoryNames(repositories []runtime.GitHubRepo) []string {
	names := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		names = append(names, repo.String())
	}
	return names
}

// discoveryDescription states the intake policy plainly, because "an issue
// existing is not consent to spend a subscription on it" is a product law an
// operator should be able to read off the startup banner.
func discoveryDescription(built *composition) string {
	settings, err := built.config.WatchSettings()
	if err != nil || len(settings.Repositories) == 0 {
		return "disabled (explicit operator submissions only)"
	}
	return fmt.Sprintf("enabled for %d enrolled repositories, label %q", len(settings.Repositories), settings.Label)
}

// governedRepositories is the set this supervisor may act on: the enrolled
// repositories when the operator configured any, otherwise the one this
// invocation targets. Enrolment stays operator configuration - a control
// request selects among these and can never introduce one.
// batchTarget picks the one repository a batch submission is for. With a single
// governed repository it is that one; with several, the operator must say which,
// because guessing spends their subscription on the wrong project.
func batchTarget(repositories []runtime.GitHubRepo, stated string) (string, error) {
	if stated = strings.TrimSpace(stated); stated != "" {
		wanted, err := runtime.ParseGitHubRepo(stated)
		if err != nil {
			return "", err
		}
		for _, repo := range repositories {
			if strings.EqualFold(repo.String(), wanted.String()) {
				return repo.String(), nil
			}
		}
		return "", fmt.Errorf("repository %q is not governed by this supervisor; enrolled: %s", wanted, repositoryList(repositories))
	}
	if len(repositories) == 1 {
		return repositories[0].String(), nil
	}
	return "", fmt.Errorf(
		"several repositories are governed (%s), so a batch submission must name one with --repo rather than being sent to whichever is first",
		repositoryList(repositories))
}

func repositoryList(repositories []runtime.GitHubRepo) string {
	names := make([]string, 0, len(repositories))
	for _, repo := range repositories {
		names = append(names, repo.String())
	}
	return strings.Join(names, ", ")
}

func (c *composition) governedRepositories(flags autonomyFlags) ([]runtime.GitHubRepo, error) {
	// A watch configuration that cannot be read is an operator problem, not a
	// reason to quietly fall through to the working directory and govern
	// something else than the configuration says.
	settings, err := c.config.WatchSettings()
	if err != nil {
		return nil, err
	}
	if len(settings.Repositories) > 0 {
		return settings.Repositories, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return nil, err
	}
	repo, err := runtime.ParseGitHubRepo(target.Identity)
	if err != nil {
		return nil, err
	}
	return []runtime.GitHubRepo{repo}, nil
}

// supervisor wires the persistent runtime. The forge is wrapped so every run
// in one repository shares one observation stream instead of each polling
// independently.
func (c *composition) supervisor(repositories []runtime.GitHubRepo) (*runtime.Supervisor, error) {
	c.forge = runtime.NewMultiplexedForge(c.forge, runtime.RealClock{})
	settings, err := c.config.WatchSettings()
	if err != nil {
		return nil, err
	}
	var discovery *runtime.WatchController
	// Automatic issue discovery is one OPTIONAL intake policy, not the reason
	// the runtime exists. It is active only when an operator enrolled
	// repositories for it; new configuration enrols none.
	if len(settings.Repositories) > 0 {
		// INTAKE ONLY. The supervisor below drives every non-terminal run; a
		// discovery controller that also drove would reconcile the runs it just
		// claimed and the supervisor would reconcile them again in the same
		// tick.
		discovery, err = c.watchController(settings, true)
		if err != nil {
			return nil, err
		}
	}
	// The plan lifecycle service the supervisor's plan reconciler resolves
	// through. It is built from the SAME store, the same customization
	// registry and the same workforce every plan command uses, so `serve`
	// cannot resolve a stage differently from what an operator was shown when
	// they approved it.
	plans, err := c.planService()
	if err != nil {
		return nil, err
	}
	return runtime.NewSupervisor(runtime.SupervisorDependencies{
		Store:             c.store,
		Plans:             plans,
		Clock:             runtime.RealClock{},
		Owner:             c.owner,
		Liveness:          runtime.NewLockOwnerLiveness(c.config.StateDir),
		Repositories:      repositories,
		MaxConcurrentRuns: settings.MaxConcurrentRuns,
		PollInterval:      settings.PollInterval,
		Discovery:         discovery,
		Agents:            c.agents,
		Runtime: func(repo runtime.GitHubRepo, agent runtime.ResolvedAgent) (*runtime.EngineeringRuntime, error) {
			return c.engineFor(runtime.RepositoryTarget{
				Identity:      repo.String(),
				Remote:        repo.CloneURL(),
				DefaultBranch: watchedDefaultBranch,
			}, agent)
		},
	})
}

// planService builds the plan lifecycle service for this composition.
//
// Readiness is probed here for the same reason it is probed for a plan
// command: which workers can take a stage is part of what the reconciler
// decides, and a stale answer would block a plan on an agent that has since
// become available.
func (c *composition) planService() (runtime.PlanService, error) {
	registry := c.planning
	agents := runtime.DescribeExecutionAgents(context.Background(), c.agents, func(agent runtime.ResolvedAgent) runtime.AgentProber {
		return runtime.AgentProberFor(agent, c.artifacts, c.config.StateDir)
	})
	if err := registry.Bind(agents); err != nil {
		return runtime.PlanService{}, err
	}
	return runtime.PlanService{
		Store: c.store, Clock: runtime.RealClock{}, Registry: registry,
		Agents: agents, DefaultAgent: c.agents.Default(), Envelope: c.config.PlanEnvelope(),
	}, nil
}

// handleControl answers one operator request. Every verb here already exists as
// a command; the endpoint exists so those commands can reach a supervisor that
// owns the work instead of driving it in a second terminal.
func (c *composition) handleControl(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	switch request.Command {
	case runtime.ControlPing:
		return controlOK(map[string]string{"state_dir": c.config.StateDir, "agent": c.agent.ID})
	case runtime.ControlSubmit:
		outcome, err := supervisor.Submit(ctx, request)
		if err != nil {
			return controlError(err)
		}
		return controlOK(outcome)
	case runtime.ControlStatus:
		fleet, err := runtime.FleetStatus(c.store, c.config.StateDir, c.maxConcurrentRuns(), time.Now().UTC())
		if err != nil {
			return controlError(err)
		}
		return controlOK(fleet)
	case runtime.ControlAgents:
		return controlOK(c.describeAgents(ctx))
	case runtime.ControlDrain:
		supervisor.Drain()
		return controlOK(map[string]bool{"draining": true})
	case runtime.ControlShutdown:
		// Shutdown stops SCHEDULING. It is not a cancellation, and the runs
		// stay resumable, which is why it does not touch a journal.
		shutdown()
		return controlOK(map[string]bool{"shutting_down": true})
	case runtime.ControlStop:
		scheduler := runtime.Scheduler{Store: c.store, Clock: runtime.RealClock{}, Owner: c.owner}
		outcome, err := runtime.CancelRun(c.store, scheduler, time.Now().UTC(), request.RunID, stopReason)
		if err != nil {
			return controlError(err)
		}
		return controlOK(outcome)
	case runtime.ControlPlanApprove, runtime.ControlPlanReject:
		// Under the reconciler's own lock: a decision and a plan tick both read
		// a snapshot and then append against it, and interleaving them lets one
		// decide from state the other is changing.
		var view runtime.PlanView
		if err := supervisor.WithPlanLock(func() (err error) { view, err = c.decidePlan(request); return err }); err != nil {
			return controlError(err)
		}
		return controlOK(view)
	case runtime.ControlPlanRevise:
		// NOT wrapped in the plan lock: revising runs a planning invocation,
		// and the lock is taken inside, around the durable write alone. A lock
		// held across a provider call stalls every run in the fleet.
		view, err := c.revisePlan(ctx, supervisor, request)
		if err != nil {
			return controlError(err)
		}
		return controlOK(view)
	case runtime.ControlStopAll:
		outcomes, err := supervisor.StopAll(request.Reason)
		if err != nil {
			return controlError(err)
		}
		return controlOK(outcomes)
	default:
		return controlError(fmt.Errorf("unknown control command %q", request.Command))
	}
}

// decidePlan applies an operator's approval or rejection inside the supervisor
// that owns the work. The digest requirement lives in PlanService, so a request
// naming a revision without its content is refused there rather than here.
func (c *composition) decidePlan(request runtime.ControlRequest) (runtime.PlanView, error) {
	plans, err := c.planService()
	if err != nil {
		return runtime.PlanView{}, err
	}
	// The REQUESTER's identity, where they sent one: a decision records who
	// made it, and this process is applying it on their behalf. Falling back to
	// this supervisor's own identity is for a request that carried none.
	operator := strings.TrimSpace(request.Operator)
	if operator == "" {
		resolved, err := c.config.ResolveOperator()
		if err != nil {
			return runtime.PlanView{}, err
		}
		operator = resolved.ID
	}
	decide := plans.Approve
	if request.Command == runtime.ControlPlanReject {
		decide = plans.Reject
	}
	if _, err := decide(request.PlanID, request.Revision, request.Digest, operator, request.Note); err != nil {
		return runtime.PlanView{}, err
	}
	return plans.View(request.PlanID)
}

// revisePlan proposes a new revision of a plan this supervisor is executing.
//
// It runs the same propose the command would, through this supervisor's own
// composition: the engine it already built, the store it already owns, and -
// where the operator did not ask for the deterministic compilation - the same
// verified non-mutating planning invocation.
// planSubject is the repository and issue a proposal is about.
//
// An EXISTING plan already answers it: the durable source binding is what every
// stage run answers, and a request cannot redirect it. A FIRST proposal has no
// plan yet, so the request names the subject - and the supervisor still refuses
// a repository it does not govern, because naming one is a selection among what
// the operator enrolled and never an introduction.
func (c *composition) planSubject(supervisor *runtime.Supervisor, request runtime.ControlRequest) (string, int, string, error) {
	if request.PlanID != "" {
		repository, issue, found, err := c.store.PlanSource(request.PlanID)
		if err != nil {
			return "", 0, "", err
		}
		if !found || issue <= 0 {
			return "", 0, "", fmt.Errorf("plan %s records no source issue, so it cannot be revised", request.PlanID)
		}
		branch, err := c.planBaseBranch(request.PlanID)
		if err != nil {
			return "", 0, "", err
		}
		return repository, issue, branch, nil
	}
	if request.Issue <= 0 {
		return "", 0, "", fmt.Errorf("a first proposal names the issue it plans, and this request names none")
	}
	governed, ok := supervisor.GovernedRepository(request.Repository)
	if !ok {
		return "", 0, "", fmt.Errorf("repository %q is not governed by this supervisor", request.Repository)
	}
	branch := strings.TrimSpace(request.DefaultBranch)
	if branch == "" {
		branch = watchedDefaultBranch
	}
	return governed.String(), request.Issue, branch, nil
}

// planBaseBranch is the branch this plan's work is already based on.
//
// The local path resolves origin/HEAD from a checkout; a supervisor has none,
// and assuming "main" made a revision requested through `serve` compile against
// a different base than the same revision requested in a terminal - the same
// works-in-one-terminal-not-the-other divergence reported for the plan's
// remote. The plan's OWN runs answer it from durable state: they were created
// by a path that did resolve it. The enrolment assumption remains the fallback
// for a plan whose stages have not started yet.
func (c *composition) planBaseBranch(planID string) (string, error) {
	// Through the PLAN's own stages, not by loading every run this state
	// directory has ever held. A revise on a long-lived installation would
	// otherwise read the whole run table into memory to answer a question about
	// one plan.
	snapshot, err := c.store.ReplayPlan(planID)
	if err != nil {
		return "", err
	}
	for _, stage := range snapshot.Stages {
		if stage.RunID == "" {
			continue
		}
		run, found, err := c.store.Run(stage.RunID)
		if err != nil {
			return "", err
		}
		if found && strings.TrimSpace(run.Base.ID) != "" {
			return run.Base.ID, nil
		}
	}
	return watchedDefaultBranch, nil
}

func (c *composition) revisePlan(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest) (runtime.PlanView, error) {
	plans, err := c.planService()
	if err != nil {
		return runtime.PlanView{}, err
	}
	repository, issue, defaultBranch, err := c.planSubject(supervisor, request)
	if err != nil {
		return runtime.PlanView{}, err
	}
	repo, err := runtime.ParseGitHubRepo(repository)
	if err != nil {
		return runtime.PlanView{}, err
	}
	// Derived from the plan's own repository IDENTITY, exactly as the local
	// command derives it when a repository is named explicitly.
	//
	// An earlier attempt at this ran `git remote get-url origin` in the STATE
	// DIRECTORY, which is not a checkout of anything: usually it errors and the
	// fallback silently assumed a default branch, and where the state directory
	// happens to sit inside some unrelated git repository it succeeded and bound
	// a governed-remote system to that repository's origin. A supervisor governs
	// several repositories; the plan says which one, and nothing about the
	// process's own working directory does.
	target := runtime.RepositoryTarget{
		Identity: repo.String(), Remote: repo.CloneURL(), DefaultBranch: defaultBranch,
	}
	engine, err := c.engine(target)
	if err != nil {
		return runtime.PlanView{}, err
	}
	composed := &planComposition{
		built: c, engine: engine, service: plans, target: target, release: func() {},
	}
	flags := autonomyFlags{
		Template: request.Template, Deterministic: request.Deterministic,
		SubstituteHuman: request.SubstituteHuman, Note: request.Note,
	}
	// A first proposal has no id yet. It is derived exactly as the local path
	// derives it - from the issue, deterministically - so the view returned
	// afterwards is of the plan this call created rather than of nothing.
	planID := request.PlanID
	if planID == "" {
		if planID, err = engine.PlanID(issue); err != nil {
			return runtime.PlanView{}, err
		}
	}
	serialize := supervisor.WithPlanLock
	if flags.SubstituteHuman != "" {
		if request.PlanID == "" {
			return runtime.PlanView{}, errors.New("substituting a human names the plan whose stage is being substituted")
		}
		// The substitution compiles deterministically - no provider call - so
		// the whole of it is short enough to serialize.
		if err := serialize(func() error {
			_, err := substituteHumanWithComposition(ctx, composed, flags, request.PlanID, io.Discard)
			return err
		}); err != nil {
			return runtime.PlanView{}, err
		}
		return plans.View(request.PlanID)
	}
	if _, err := proposeSerialized(ctx, composed, flags, issue, planID, io.Discard, serialize); err != nil {
		return runtime.PlanView{}, err
	}
	return plans.View(planID)
}

func controlOK(payload any) runtime.ControlResponse {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return controlError(err)
	}
	return runtime.ControlResponse{OK: true, Payload: encoded}
}

func controlError(err error) runtime.ControlResponse {
	return runtime.ControlResponse{Error: err.Error()}
}

func (c *composition) maxConcurrentRuns() int {
	settings, err := c.config.WatchSettings()
	if err != nil {
		return 1
	}
	return settings.MaxConcurrentRuns
}

// ---------------------------------------------------------------------------
// agents
// ---------------------------------------------------------------------------

// describeAgents answers readiness for every configured agent WITHOUT spending
// anything: an executable is found or not, a capability is advertised or not, a
// credential file exists or not. A readiness listing that made a paid call to
// prove a worker exists would be charging an operator for a question.
func (c *composition) describeAgents(ctx context.Context) []runtime.AgentStatus {
	return runtime.DescribeAgents(ctx, c.agents, func(agent runtime.ResolvedAgent) runtime.AgentProber {
		return runtime.AgentProberFor(agent, c.artifacts, operatorHome())
	})
}

func autonomyAgents(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	flags, err := parseAutonomyFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	statuses := built.describeAgents(context.Background())
	if !flags.Text {
		if err := writeJSON(stdout, statuses); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	fmt.Fprintf(stdout, "%-12s %-14s %-17s %-6s %-24s %-20s %s\n",
		"AGENT", "KIND", "TRUST", "READY", "VERSION", "AUTH", "DETAIL")
	for _, status := range statuses {
		name := status.ID
		if status.Default {
			name += " *"
		}
		ready := "no"
		if status.Available {
			ready = "yes"
		}
		fmt.Fprintf(stdout, "%-12s %-14s %-17s %-6s %-24s %-20s %s\n",
			name, status.Kind, status.TrustMode, ready,
			shortVersion(status.Version), orDash(status.AuthMode), status.Detail)
	}
	fmt.Fprintln(stdout, "\n* is the default agent.")
	fmt.Fprintln(stdout, "`ready` means the worker can be invoked, not that its account has budget: account state is only")
	fmt.Fprintln(stdout, "observable by making a paid request, which this command never does. `auth` is what was OBSERVED of")
	fmt.Fprintln(stdout, "each CLI's own state; `unknown` is a truthful answer and is never guessed into `subscription`.")
	return runtime.ExitCompleted, nil
}

func shortVersion(version string) string {
	if version == "" {
		return "-"
	}
	if len(version) > 23 {
		return version[:23]
	}
	return version
}

// ---------------------------------------------------------------------------
// fleet status
// ---------------------------------------------------------------------------

// autonomyFleet is `status` with no run named: the control-room view over every
// run. It is a pure read over durable state, so it is safe while a supervisor
// drives the same runs.
func autonomyFleet(flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	fleet, err := runtime.FleetStatus(built.store, built.config.StateDir, built.maxConcurrentRuns(), time.Now().UTC())
	if err != nil {
		return runtime.ExitFailed, err
	}
	if !flags.Text {
		if err := writeJSON(stdout, fleet); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
	fmt.Fprintf(stdout, "ZENCHRON ENGINEERING\n\n")
	supervisor := "not running"
	if fleet.SupervisorRunning {
		supervisor = "running"
	}
	fmt.Fprintf(stdout, "Supervisor: %s   Workers: %d / %d active\n\n", supervisor, fleet.Active, fleet.Capacity)
	fmt.Fprintf(stdout, "%-8s %-10s %-18s %-24s %-10s %s\n", "ISSUE", "AGENT", "STATE", "BRANCH / PR", "ELAPSED", "REASON")
	for _, run := range fleet.Runs {
		fmt.Fprintf(stdout, "%-8s %-10s %-18s %-24s %-10s %s\n",
			issueLabel(run), orDash(run.Agent), stateLabel(run), locationLabel(run),
			elapsedLabel(run.Elapsed), run.Reason)
	}
	// PLANS, beside the runs. A plan awaiting approval is the runtime waiting on
	// a PERSON, and that has to be visible where an operator looks to see
	// whether anything is happening at all.
	if len(fleet.Plans) > 0 {
		fmt.Fprintf(stdout, "\n%-38s %-6s %-18s %-22s %s\n", "PLAN", "REV", "STATE", "STAGES", "CHILD RUNS")
		for _, plan := range fleet.Plans {
			fmt.Fprintf(stdout, "%-38s %-6s %-18s %-22s %d\n",
				plan.PlanID, revisionLabel(plan), plan.State, stageLabel(plan), len(plan.Runs))
		}
		fmt.Fprintln(stdout, "\n`autonomy plan show PLAN --text` explains one plan; a plan awaiting approval executes nothing until `autonomy plan approve PLAN`.")
	}
	fmt.Fprintln(stdout, "\n`autonomy status RUN --text` explains one run; `autonomy logs RUN` shows what its worker is saying.")
	return runtime.ExitCompleted, nil
}

// revisionLabel shows the governing revision beside the latest one when they
// differ, because "executing r2 while r3 waits for you" is the whole state.
func revisionLabel(plan runtime.PlanSummary) string {
	if plan.ApprovedRevision != 0 && plan.ApprovedRevision != plan.Revision {
		return fmt.Sprintf("%d<%d", plan.ApprovedRevision, plan.Revision)
	}
	return strconv.Itoa(plan.Revision)
}

func stageLabel(plan runtime.PlanSummary) string {
	states := make([]string, 0, len(plan.Stages))
	for state, count := range plan.Stages {
		states = append(states, fmt.Sprintf("%d %s", count, state))
	}
	sort.Strings(states)
	if len(states) == 0 {
		return "-"
	}
	return strings.Join(states, ", ")
}

func issueLabel(run runtime.RunSummary) string {
	if run.Issue == 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", run.Issue)
}

func stateLabel(run runtime.RunSummary) string {
	if run.Disposition == runtime.Active && run.Operation != "" {
		return string(run.Disposition) + ":" + run.Operation
	}
	return string(run.Disposition)
}

func locationLabel(run runtime.RunSummary) string {
	if run.PullRequest > 0 {
		return fmt.Sprintf("PR #%d", run.PullRequest)
	}
	return orDash(run.Branch)
}

func elapsedLabel(elapsed time.Duration) string {
	if elapsed <= 0 {
		return "-"
	}
	return elapsed.Round(time.Second).String()
}

func orDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

// ---------------------------------------------------------------------------
// logs
// ---------------------------------------------------------------------------

// autonomyLogs shows what a worker is actually saying, without an operator
// having to know where an artifact path is built.
//
// It reads the SANITIZED transcript beside each raw one. The raw artifact is
// local-only forensic material and is deliberately never rendered here: `logs`
// is a surface pointed at a screen, and the sanitized copy is the one that has
// been through redaction.
func autonomyLogs(ctx context.Context, flags autonomyFlags, overrides autonomyOverrides, runID string, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	if _, err := requireRun(built, runID); err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	printed := map[string]int{}
	for {
		logs, err := runtime.RunAttemptLogs(built.artifacts, runID, true)
		if err != nil {
			return runtime.ExitFailed, err
		}
		for _, entry := range logs {
			key := entry.OperationID + "#" + fmt.Sprint(entry.Attempt)
			if len(entry.Text) <= printed[key] {
				continue
			}
			if printed[key] == 0 {
				// Attempt boundaries are visible, and each names the agent that
				// produced it: a run whose provider changed generation would
				// otherwise read as one continuous conversation.
				fmt.Fprintf(stdout, "\n=== %s attempt %d (%s) ===\n", entry.OperationID, entry.Attempt, entry.AgentID)
			}
			fmt.Fprint(stdout, entry.Text[printed[key]:])
			printed[key] = len(entry.Text)
		}
		if !flags.Follow {
			return runtime.ExitCompleted, nil
		}
		select {
		case <-ctx.Done():
			return runtime.ExitCompleted, nil
		case <-time.After(time.Second):
		}
	}
}

// ---------------------------------------------------------------------------
// delegation
// ---------------------------------------------------------------------------

// delegate submits an operator request to a running supervisor. It returns
// false when no supervisor owns this state directory, which is the signal to
// drive the work in this terminal exactly as before `serve` existed.
func delegate(stateDir string, request runtime.ControlRequest, stdout io.Writer) (bool, int, error) {
	delegated, payload, err := delegatePayload(stateDir, request)
	if !delegated || err != nil {
		return delegated, runtime.ExitFailed, err
	}
	if len(payload) > 0 {
		if _, err := fmt.Fprintln(stdout, string(payload)); err != nil {
			return true, runtime.ExitFailed, err
		}
	}
	return true, runtime.ExitCompleted, nil
}

// delegatePayload is the same submission with the answer RETURNED rather than
// printed, for a command that renders its own view. It exists so a delegated
// command and a locally executed one produce the same output: an operator
// should not be able to tell which process applied their decision.
func delegatePayload(stateDir string, request runtime.ControlRequest) (bool, json.RawMessage, error) {
	delegated, payload, _, err := delegatePayloadSent(stateDir, request)
	return delegated, payload, err
}

// delegatePayloadSent additionally reports whether the request was actually
// PUT ON THE WIRE. "The supervisor may have applied this and lost the reply" is
// only true of a request that was sent; a request refused before any connection
// was made was not applied by anybody, and telling an operator to consult the
// durable record for it invites the opposite error.
func delegatePayloadSent(stateDir string, request runtime.ControlRequest) (delegated bool, payload json.RawMessage, sent bool, err error) {
	running, endpointPresent := runtime.SupervisorPresence(stateDir)
	if !running {
		if endpointPresent {
			// The endpoint EXISTS and could not be reached. Deciding locally
			// here would write beside a supervisor that may be alive, outside
			// the lock that exists to prevent it, because one dial failed.
			//
			// Neither remedy in the old wording works after a crash: retrying
			// dials the same dead socket, and there is no supervisor left to
			// stop. Restarting one reclaims the socket; removing the file is
			// the manual equivalent.
			return true, nil, false, fmt.Errorf(
				"a supervisor endpoint exists at %s and could not be reached; nothing was sent and the decision was not applied - start a supervisor with `zenchron-engineering serve`, which reclaims the socket, or remove that file if no supervisor will run again",
				runtime.ControlSocketPath(stateDir))
		}
		return false, nil, false, nil
	}
	response, err := runtime.SendControl(stateDir, request)
	if err != nil {
		return true, nil, true, err
	}
	if !response.OK {
		return true, nil, true, errors.New(response.Error)
	}
	return true, response.Payload, true, nil
}

// requireSupervisor is for the lifecycle verbs that have no meaning without a
// running supervisor. Drain and shutdown are instructions TO a supervisor; with
// none running there is nothing to instruct, and saying so is better than
// silently succeeding.
func requireSupervisor(flags autonomyFlags, overrides autonomyOverrides, request runtime.ControlRequest, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir := built.config.StateDir
	built.release()

	delegated, code, err := delegate(stateDir, request, stdout)
	if !delegated {
		return runtime.ExitInvalid, fmt.Errorf("no supervisor is running on %s; start one with `zenchron-engineering serve`", stateDir)
	}
	return code, err
}

// autonomyStopAll is the one lifecycle action that CANCELS runs, and it is
// deliberately a separate command from shutting the supervisor down. It works
// with or without a supervisor: with one, it is delegated so the process that
// owns the leases performs it; without one, it is performed here through the
// same single cancellation path.
func autonomyStopAll(flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	if delegated, code, err := delegate(built.config.StateDir, runtime.ControlRequest{
		Command: runtime.ControlStopAll, Reason: flags.Reason,
	}, stdout); delegated {
		return code, err
	}
	reason := flags.Reason
	if strings.TrimSpace(reason) == "" {
		reason = "operator_stop_all"
	}
	runs, err := built.store.Runs()
	if err != nil {
		return runtime.ExitFailed, err
	}
	scheduler := runtime.Scheduler{Store: built.store, Clock: runtime.RealClock{}, Owner: built.owner}
	var outcomes []runtime.Outcome
	for _, run := range runs {
		if run.Disposition == runtime.Completed || run.Disposition == runtime.Failed || run.Disposition == runtime.Cancelled {
			continue
		}
		outcome, err := runtime.CancelRun(built.store, scheduler, time.Now().UTC(), run.ID, reason)
		if err != nil {
			return runtime.ExitFailed, err
		}
		outcomes = append(outcomes, outcome)
	}
	if err := writeJSON(stdout, outcomes); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCancelled, nil
}

// ---------------------------------------------------------------------------
// agent transition
// ---------------------------------------------------------------------------

// autonomyAgentSet is the explicit operator action for changing a live run's
// execution agent. In this milestone it always refuses, and the refusal is the
// product: a complete typed transition record is journalled and printed, so the
// attempt is auditable and the operator is told to start a new generation.
func autonomyAgentSet(flags autonomyFlags, overrides autonomyOverrides, runID string, stdout io.Writer) (int, error) {
	if strings.TrimSpace(flags.Agent) == "" || strings.TrimSpace(flags.Reason) == "" {
		return runtime.ExitInvalid, errors.New("changing a run's agent requires --agent <id> and --reason <text>")
	}
	built, engine, err := buildEngine(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	record, err := engine.RequestAgentHandoff(runID, flags.Agent, flags.Reason)
	if err := writeJSON(stdout, record); err != nil {
		return runtime.ExitFailed, err
	}
	var refused *runtime.AgentHandoffRefusedError
	if errors.As(err, &refused) {
		return exitAuthorityRefused, err
	}
	if err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}

// ---------------------------------------------------------------------------
// batch submission
// ---------------------------------------------------------------------------

// autonomyRunIssues starts several issues under ONE scheduler owner. It is the
// answer to "I have three tickets and one afternoon": the operator assigns
// agents deterministically and does not manage three terminals.
//
// Assignment is explicit. There is no router here choosing which model suits
// which ticket - that is a decision an operator makes, and a system that made
// it silently would be spending their subscriptions on its own opinion.
func autonomyRunIssues(ctx context.Context, flags autonomyFlags, overrides autonomyOverrides, issues []int, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir := built.config.StateDir
	repositories, repoErr := built.governedRepositories(flags)
	built.release()
	if repoErr != nil {
		return runtime.ExitInvalid, repoErr
	}
	if !runtime.SupervisorRunning(stateDir) {
		return runtime.ExitInvalid, fmt.Errorf(
			"starting several issues at once needs a supervisor to own them; run `zenchron-engineering serve` in another terminal, "+
				"or start them one at a time with `autonomy run issue N --agent X`. State directory: %s", stateDir)
	}
	// The batch path resolves its target the same way the single-issue path
	// does. Taking repositories[0] sent every issue to whichever repository
	// happened to be enrolled first, silently ignoring --repo: `run issues 5 7
	// --repo owner/name` submitted to somebody else's repository and said
	// nothing. An ambiguous target is refused rather than guessed.
	repository, err := batchTarget(repositories, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	failures := 0
	for _, issue := range issues {
		request := runtime.ControlRequest{
			Command: runtime.ControlSubmit, Repository: repository, Issue: issue,
			Agent: flags.Assign[issue], NewGeneration: flags.NewGeneration,
		}
		if request.Agent == "" {
			request.Agent = flags.Agent
		}
		delegated, _, err := delegate(stateDir, request, stdout)
		switch {
		case err != nil:
			fmt.Fprintf(stdout, "issue %d was not submitted: %v\n", issue, err)
			failures++
		case !delegated:
			// The supervisor was probed once before the loop. If it exited
			// mid-batch every remaining delegate returns "not delegated" with
			// no error, and counting those as submitted made the command exit
			// 0 with the work never created.
			fmt.Fprintf(stdout, "issue %d was not submitted: the supervisor stopped accepting work mid-batch\n", issue)
			failures++
		}
	}
	if failures > 0 {
		return runtime.ExitFailed, fmt.Errorf("%d of %d submissions failed", failures, len(issues))
	}
	return runtime.ExitCompleted, nil
}

// sortedIssues keeps submission order deterministic.
func sortedIssues(issues []int) []int {
	out := append([]int(nil), issues...)
	sort.Ints(out)
	return out
}
