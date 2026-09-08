package main

// autonomy is the composition root for the local engineering runtime. It does
// exactly two things:
//
//  1. builds the real components from configuration and hands them to
//     runtime.NewEngineeringRuntime as one Dependencies value, and
//  2. translates what the runtime returns into CLI output and a process exit
//     code.
//
// There is no orchestration here. The reconcile loop, the phase machine, and
// every decision about what to do next belong to the runtime service; this file
// may not grow one. If a change here starts describing WHEN something happens
// rather than WHAT is wired, it belongs in runtime/.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const autonomyUsage = "usage: zenchron-engineering autonomy {agents [--text]|" +
	"plan {issue <number>|show|approve|reject|revise|status <plan>|list} [--template <id>] [--deterministic] [--note <text>]|" +
	"run issue <number> [--agent <id>] [--new-generation]|run issues <n> <n>... [--assign N=agent]|" +
	"status [<run>] [--text]|logs <run> [--follow]|events <run> [--follow]|resume <run>|refresh <run>|" +
	"agent set <run> --agent <id> --reason <text>|" +
	"authorize <run> <request-id> --approve|--reject [--note <text>]|" +
	"stop <run>|stop-all [--reason <text>]|drain|shutdown|watch|doctor [--text]|gc [--dry-run]} " +
	"[--repo owner/name] [--config <path>]"

// Exit statuses this CLI adds to the run-mode exits in runtime.go. They are
// values runtime.go deliberately does not define, because they classify an
// OPERATOR COMMAND rather than a run's disposition: neither can be produced by
// a reconcile, and a run can never exit as either.
const (
	// exitAuthorityRefused is a refused or stale authorization. It is its own
	// status because "the state you approved has moved" is a distinct,
	// actionable operator outcome: nothing is broken, nothing is misconfigured,
	// and retrying the same command verbatim will refuse again.
	exitAuthorityRefused = 13
	// exitRunNotFound is a run identity the durable store has never held. It
	// is separated from the usage status because the command was well formed
	// and the configuration loaded; only the subject is unknown.
	exitRunNotFound = 14
)

// runNotFoundError is the typed form of that outcome. The runtime reports an
// unknown run as an untyped error, so the identity is probed here, against the
// same durable store every command reads, before anything else is attempted.
type runNotFoundError struct{ RunID string }

func (e *runNotFoundError) Error() string {
	return "unknown run " + strconv.Quote(e.RunID) + "; `autonomy run issue <number>` starts one"
}

// exitFor maps a typed diagnostic onto its process exit status. It is the ONE
// place a command's failure becomes a number, so two commands cannot classify
// the same refusal differently.
func exitFor(err error, fallback int) int {
	var refused *runtime.AuthorityRefusedError
	if errors.As(err, &refused) {
		return exitAuthorityRefused
	}
	var missing *runNotFoundError
	if errors.As(err, &missing) {
		return exitRunNotFound
	}
	var identity *runtime.OperatorIdentityError
	if errors.As(err, &identity) {
		return runtime.ExitInvalid
	}
	var config *runtime.ConfigError
	if errors.As(err, &config) {
		return runtime.ExitInvalid
	}
	return fallback
}

// watchedDefaultBranch is the base branch assumed for an enrolled repository.
// A watched repository has no local checkout to read origin/HEAD from and the
// forge adapter exposes no repository-metadata call, so the same fallback
// ResolveRepository uses is applied here.
//
// ponytail: a watched repository whose default branch is not "main" surfaces as
// that repository's own failure in the tick report; ask the forge for the
// default branch if that stops being rare.
const watchedDefaultBranch = "main"

// engineeringRuntime is the runtime surface this CLI consumes. It exists so the
// exit-code translation can be exercised against every disposition without
// having to drive the runtime's state machine into each one; buildEngine
// always returns the real *runtime.EngineeringRuntime.
type engineeringRuntime interface {
	StartOrResumeIssueRun(ctx context.Context, issue int) (string, error)
	StartIssueRun(ctx context.Context, issue int, mode runtime.GenerationMode) (runtime.StartOutcome, error)
	Reconcile(ctx context.Context, runID string) (runtime.Outcome, error)
	Status(runID string) (runtime.StatusReport, error)
	Journal(runID string) ([]runtime.EngineeringEvent, error)
	// PendingAuthorityRequest and Authorize are the human-authority boundary.
	// They are consumed, never re-implemented: the request is a projection the
	// runtime derives, and Authorize records evidence and reports what the
	// evaluator concluded afterwards.
	PendingAuthorityRequest(runID string) (*runtime.AuthorityRequest, error)
	Authorize(ctx context.Context, in runtime.AuthorizeInput) (runtime.AuthorizeResult, error)
}

// watchController is the watch surface this CLI consumes. The loop below owns
// only "tick, then wait"; discovery, claiming, driving, and every per-repository
// failure decision live behind this one method.
type watchController interface {
	Tick(ctx context.Context) (runtime.TickReport, error)
}

// autonomyOverrides is the test seam for the boundaries that would otherwise
// require a network, a paid provider, or a Docker daemon. Production leaves
// every field nil and the real components are built from configuration; a nil
// field is never a silent fallback to something weaker.
type autonomyOverrides struct {
	GitHub    runtime.GitHubAdapter
	Provider  runtime.ExecutionProvider
	Assurance runtime.AssuranceProvider
	// SemanticAssurance replaces the independent semantic producer, so a test
	// can model a configuration with or without one.
	SemanticAssurance runtime.AssuranceProvider
	Runtime           engineeringRuntime
	Watch             watchController
	// ControllerBuild replaces this process's own provenance. Production
	// resolves it from the injected build metadata and the running executable;
	// a test supplies a fixed build so the identity under assertion is the one
	// the test wrote, not whatever binary the test runner happens to be.
	ControllerBuild *runtime.ControllerBuild
	// ControllerBuildResolver replaces the real provenance measurement in
	// doctor. It exists so a test can exercise the one branch the real
	// resolver will not produce on demand: the failure to measure at all.
	ControllerBuildResolver func() (runtime.ControllerBuild, error)
	// WatchWait is the loop's only timing mechanism. Production waits on a
	// real timer; a test supplies its own so the schedule is asserted instead
	// of slept through.
	WatchWait func(ctx context.Context, until time.Time)
}

// autonomyFlags is the whole flag surface. It is one struct rather than one
// per subcommand so a flag can never mean two things: --config always names the
// operator configuration, --text always selects the human projection of the
// same structure, and a flag a subcommand does not use is simply unread.
type autonomyFlags struct {
	Repo   string
	Config string
	// Follow tails the journal instead of printing it once.
	Follow bool
	// Text selects the human-readable projection over the same JSON structure.
	Text bool
	// DryRun prints what gc WOULD reclaim, from the same planner that executes.
	DryRun bool
	// Decision is the recorded human answer: "approve" or "reject". Both are
	// evidence; neither is a permission.
	Decision string
	// Note is an optional, untrusted operator annotation. It is an input to
	// nothing.
	Note string
	// NewGeneration asks for a genuinely new run rather than continuing a live
	// generation. It is explicit because the two are different intentions and
	// were previously one command.
	NewGeneration bool
	// Agent selects the named execution agent. Empty resolves the operator's
	// configured default, which is what makes `run issue N` work unchanged.
	Agent string
	// Assign binds issues to agents for a batch, as issue -> agent id.
	Assign map[int]string
	// PermissionBypass explicitly requests the provider's unsafe permission
	// mode for this invocation. It is refused unless the operator's
	// configuration ALSO allows it for that agent: the bypass needs two
	// independent statements, and this flag is only one of them.
	PermissionBypass bool
	// Reason is the operator's stated cause for a governed transition.
	Reason string
	// Template names the operator's reusable EngineeringPlanTemplate for a plan
	// proposal. Empty means the planner compiles from policy and intent alone.
	Template string
	// Revision and Digest name the EXACT plan revision a decision is about.
	// They are what an operator read: without them the CLI would decide
	// whatever the highest revision happened to be at decide time, which can
	// be a proposal that landed after the operator looked.
	Revision int
	Digest   string
	// SubstituteHuman names the blocked agent stage an operator is replacing
	// with an independent human review. It is only ever accepted where POLICY
	// permitted that substitution; the permission comes from the obligation
	// that required the independence, and neither an operator nor a plan may
	// grant it.
	SubstituteHuman string
	// Deterministic compiles a plan with NO model invocation. It is the honest
	// alternative to reasoning rather than a fallback from it: the same
	// obligations, the same validation, and a plan that says a model was not
	// consulted.
	Deterministic bool
	// Detached submits work to a running supervisor instead of driving it in
	// this terminal. It is implied when a supervisor owns the state directory.
	Detached bool
}

func autonomy(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	if len(args) == 0 {
		return runtime.ExitInvalid, errors.New(autonomyUsage)
	}
	command, rest := args[0], args[1:]

	// The subject-free subcommands. Each parses its own flags and returns its
	// own status; none of them names a run.
	switch command {
	case "doctor":
		return autonomyDoctor(rest, overrides, stdout)
	case "agents":
		return autonomyAgents(rest, overrides, stdout)
	case "drain", "shutdown":
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		verb := runtime.ControlDrain
		if command == "shutdown" {
			verb = runtime.ControlShutdown
		}
		return requireSupervisor(flags, overrides, runtime.ControlRequest{Command: verb}, stdout)
	case "stop-all":
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return autonomyStopAll(flags, overrides, stdout)
	case "watch":
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return autonomyWatch(context.Background(), flags, overrides, stdout)
	case "gc":
		return autonomyGC(rest, overrides, stdout)
	case "plan":
		// The plan lifecycle. It is under `autonomy` beside `run` because it is
		// the same operator asking for the same work at a different altitude:
		// `run issue N` starts one governed run, `plan issue N` proposes the
		// decomposition that several of them would execute.
		return autonomyPlan(context.Background(), rest, overrides, stdout)
	}

	// Everything else names exactly one subject: an issue number for `run`, a
	// run id for the rest, and additionally a request id for `authorize`.
	var issue int
	var runID, requestID string
	switch command {
	case "run":
		if len(rest) < 2 {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		// `run issues N M ...` is the batch form. It is a separate subject
		// shape rather than a flag, because "start one issue here" and "hand
		// several issues to the supervisor" are different operator intentions
		// with different terminals attached to them.
		if rest[0] == "issues" {
			var issues []int
			rest = rest[1:]
			for len(rest) > 0 {
				number, err := strconv.Atoi(rest[0])
				if err != nil || number <= 0 {
					break
				}
				issues, rest = append(issues, number), rest[1:]
			}
			if len(issues) == 0 {
				return runtime.ExitInvalid, errors.New(autonomyUsage)
			}
			flags, err := parseAutonomyFlags(rest)
			if err != nil {
				return runtime.ExitInvalid, err
			}
			return autonomyRunIssues(context.Background(), flags, overrides, sortedIssues(issues), stdout)
		}
		if rest[0] != "issue" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil || number <= 0 {
			return runtime.ExitInvalid, fmt.Errorf("issue number must be a positive integer, got %q", rest[1])
		}
		issue, rest = number, rest[2:]
	case "agent":
		// `agent set RUN` is the explicit governed transition. It is spelled as
		// a subcommand rather than a flag on `run` so that changing a live
		// run's worker can never be something an operator does by accident.
		if len(rest) < 2 || rest[0] != "set" || strings.TrimSpace(rest[1]) == "" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		runID, rest = rest[1], rest[2:]
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return autonomyAgentSet(flags, overrides, runID, stdout)
	case "logs":
		if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		runID, rest = rest[0], rest[1:]
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return autonomyLogs(context.Background(), flags, overrides, runID, stdout)
	case "authorize":
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" || strings.TrimSpace(rest[1]) == "" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		runID, requestID, rest = rest[0], rest[1], rest[2:]
	case "status":
		// `status` with no run is the control-room view over every run, which
		// is the question an operator with several workers actually has.
		if len(rest) == 0 || strings.HasPrefix(rest[0], "--") {
			flags, err := parseAutonomyFlags(rest)
			if err != nil {
				return runtime.ExitInvalid, err
			}
			return autonomyFleet(flags, overrides, stdout)
		}
		runID, rest = rest[0], rest[1:]
	case "events", "resume", "refresh", "stop":
		if len(rest) < 1 || strings.TrimSpace(rest[0]) == "" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		runID, rest = rest[0], rest[1:]
	default:
		return runtime.ExitInvalid, errors.New(autonomyUsage)
	}

	flags, err := parseAutonomyFlags(rest)
	if err != nil {
		return runtime.ExitInvalid, err
	}

	// `events` is a pure read and stays one. It opens the durable journal
	// directly rather than through the composition root, so it takes NEITHER
	// the state directory's exclusive ownership NOR any run-driving lease -
	// which is what lets it tail a run another controller is driving.
	if command == "events" && overrides.Runtime == nil {
		return autonomyEvents(context.Background(), flags, runID, stdout)
	}
	if command == "stop" {
		return autonomyStop(flags, overrides, runID, stdout)
	}

	engine, built, release := overrides.Runtime, (*composition)(nil), func() {}
	if engine == nil {
		composed, real, err := buildEngine(flags, overrides)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		engine, built, release = real, composed, composed.release
	}
	defer release()

	ctx := context.Background()
	switch command {
	case "run":
		// Which generation the operator meant is STATED, never inferred. Without
		// --new-generation this continues a live generation only when this
		// controller created it, and refuses - naming the run - when another
		// did. With it, a new run id is always created and the existing
		// generation is left untouched.
		mode := runtime.AdoptCompatibleGeneration
		if flags.NewGeneration {
			mode = runtime.NewGeneration
		}
		// A supervisor that owns this state directory owns the INTAKE too, so
		// it is asked BEFORE anything durable is created.
		//
		// Creating the run first and handing it over afterwards looked
		// equivalent and was not: a draining supervisor refuses new work, and
		// a run created behind that refusal would sit in the store with
		// nothing driving it - an operator would have been told their work
		// started when it had not. Ownership boundaries are only boundaries if
		// they are consulted before the side effect.
		if built != nil && runtime.SupervisorRunning(built.config.StateDir) {
			return submitToSupervisor(built, flags, issue, stdout)
		}
		outcome, err := engine.StartIssueRun(ctx, issue, mode)
		if err != nil {
			return exitFor(err, runtime.ExitFailed), err
		}
		if outcome.Adopted {
			fmt.Fprintf(stdout, "adopted existing generation %s (this controller created it)\n", outcome.RunID)
		} else {
			fmt.Fprintf(stdout, "created generation %s\n", outcome.RunID)
		}
		return reconcile(ctx, engine, outcome.RunID, stdout)
	case "resume":
		return autonomyResume(ctx, engine, built, runID, stdout)
	case "refresh":
		return autonomyRefresh(ctx, engine, built, flags, runID, stdout)
	case "authorize":
		return autonomyAuthorize(ctx, engine, built, flags, runID, requestID, stdout)
	case "status":
		return autonomyStatus(engine, built, flags, runID, stdout)
	default: // events, against an injected runtime
		events, err := engine.Journal(runID)
		if err != nil {
			return runtime.ExitFailed, err
		}
		if err := writeJSON(stdout, events); err != nil {
			return runtime.ExitFailed, err
		}
		return runtime.ExitCompleted, nil
	}
}

// submitToSupervisor hands one issue to the process that owns the state
// directory. The supervisor decides: it refuses while draining, refuses an
// agent the operator did not configure, refuses a repository it does not
// govern, and creates the run through an engine bound to the requested agent.
//
// Nothing durable is created here first, so a refusal leaves no orphaned run.
func submitToSupervisor(built *composition, flags autonomyFlags, issue int, stdout io.Writer) (int, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	delegated, code, err := delegate(built.config.StateDir, runtime.ControlRequest{
		Command:       runtime.ControlSubmit,
		Repository:    target.Identity,
		Issue:         issue,
		Agent:         flags.Agent,
		NewGeneration: flags.NewGeneration,
	}, stdout)
	if !delegated {
		// The supervisor stopped between the probe and the request. Say so
		// rather than silently driving the work here: which process owns a run
		// is not something to decide by a race.
		return runtime.ExitInvalid, fmt.Errorf(
			"the supervisor on %s stopped while this request was being sent; run the command again", built.config.StateDir)
	}
	if err != nil {
		return code, err
	}
	fmt.Fprintln(stdout, "submitted to the running supervisor; follow it with `autonomy status --text` or `autonomy logs RUN --follow`")
	return runtime.ExitWaiting, nil
}

// requireRun probes the run identity against the same durable store every
// command reads. It is the ONE place "this run does not exist" is decided, so
// no command can report an unknown run as an operational failure. A CLI driving
// an injected runtime has no store to probe and skips it.
func requireRun(built *composition, runID string) (runtime.EngineeringRun, error) {
	if built == nil {
		return runtime.EngineeringRun{}, nil
	}
	run, found, err := built.store.Run(runID)
	if err != nil {
		return runtime.EngineeringRun{}, err
	}
	if !found {
		return runtime.EngineeringRun{}, &runNotFoundError{RunID: runID}
	}
	return run, nil
}

// observeFeedback polls GitHub for reviewer feedback before driving a run.
//
// Polling belongs to whoever owns the clock, which is normally the supervisor.
// An operator driving a run in their own terminal owns it instead, and without
// this that run would never see a review comment at all - the feedback loop
// would exist only for supervised work. The capability is optional so an
// injected test runtime does not have to implement it.
//
// A failed poll is not a failure of the run: feedback is an input the run can
// proceed without, and refusing to reconcile because GitHub was briefly
// unreachable would turn an observation into an outage.
func observeFeedback(ctx context.Context, engine engineeringRuntime, runID string) {
	observer, ok := engine.(interface {
		ObserveFeedback(context.Context, string) (runtime.FeedbackObservation, error)
	})
	if !ok {
		return
	}
	_, _ = observer.ObserveFeedback(ctx, runID)
}

func reconcile(ctx context.Context, engine engineeringRuntime, runID string, stdout io.Writer) (int, error) {
	observeFeedback(ctx, engine, runID)
	outcome, err := engine.Reconcile(ctx, runID)
	if err != nil {
		return runtime.ExitFailed, err
	}
	if err := writeJSON(stdout, outcome); err != nil {
		return runtime.ExitFailed, err
	}
	return outcome.ExitCode(), nil
}

func writeJSON(stdout io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, string(encoded))
	return err
}

// composition holds every component that is shared by ALL repositories this
// invocation may govern: one configuration, one durable store, one ownership
// lock, one forge adapter, one provider, one verifier. Everything a single
// repository needs on top of that is derived by engine().
//
// It exists so `run`/`status`/`resume` (one repository, taken from cwd or
// --repo) and `watch` (one engine per enrolled repository) are the SAME
// construction with a different repository target, rather than two wirings that
// can drift apart.
type composition struct {
	config      runtime.Config
	store       *runtime.SQLiteOperationStore
	owner       string
	model       domain.ProjectModel
	policy      domain.EngineeringPolicy
	artifacts   runtime.ArtifactStore
	credentials runtime.CredentialProvider
	forge       runtime.GitHubAdapter
	provider    runtime.ExecutionProvider
	assurance   runtime.AssuranceProvider
	semantic    runtime.AssuranceProvider
	build       runtime.ControllerBuild
	// agents is the operator's registry, and agent is the one this invocation
	// resolved. Both are here because the two are different questions: which
	// workers exist, and which one this command is driving.
	agents   runtime.AgentRegistry
	agent    runtime.ResolvedAgent
	feedback runtime.FeedbackPolicy
	// planning is the operator's customization registry. It is loaded ONCE
	// here and handed to every engine, because a run executing a plan stage
	// resolves its frozen instruction packs through it: an engine built
	// without it would refuse work the operator approved, on the grounds that
	// a pack it was never given is "no longer installed".
	planning planning.Registry
	storage  runtime.StateStorage
	// sandbox and permissionBypass are kept so an engine can be built for an
	// agent other than the one this invocation resolved. providerInjected
	// marks the test seam: an injected provider is used for every agent,
	// because a test that supplied one meant it.
	sandbox          runtime.DockerSandbox
	permissionBypass bool
	providerInjected bool
	release          func()
}

// newComposition is the wiring. Every failure here is a configuration or usage
// fault, so every caller maps it to ExitInvalid.
func newComposition(flags autonomyFlags, overrides autonomyOverrides) (*composition, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	config, err := runtime.LoadConfig(flags.Config, cwd)
	if err != nil {
		return nil, err
	}
	// Provenance is resolved once, before anything durable is written: a
	// controller that cannot say which binary it is has no business creating a
	// run under a provenance claim it cannot substantiate.
	build := runtime.ControllerBuild{Kind: runtime.ControllerUnattested}
	if overrides.ControllerBuild != nil {
		build = *overrides.ControllerBuild
	} else if build, err = controllerBuild(); err != nil {
		return nil, err
	}
	model, err := analysis.LoadProjectModel(config.ProjectModelPath)
	if err != nil {
		return nil, err
	}
	policy, err := runtime.LoadEngineeringPolicy(config.PolicyPath)
	if err != nil {
		return nil, err
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		return nil, err
	}
	release := func() { _ = store.Close() }

	// The owner identity and the OS ownership lock must be the same string:
	// the lock is the crash-safe evidence NewLockOwnerLiveness reads to decide
	// whether the process that recorded a lease is still alive. Taking it here
	// also refuses a second invocation that would share this identity, and
	// releasing it on shutdown is how a watcher gives ownership back.
	owner := runtime.NewRuntimeOwner()
	lock, err := runtime.AcquireOwnershipLock(config.StateDir, owner)
	if err != nil {
		release()
		return nil, fmt.Errorf("cannot take exclusive ownership of state dir %s; another zenchron-engineering process may already be running against it: %w", config.StateDir, err)
	}
	release = func() { _ = lock.Release(); _ = store.Close() }

	artifacts := runtime.ArtifactStore{Root: filepath.Join(config.StateDir, "artifacts")}
	// StateDir is where a runtime-owned Docker operation RECORD is written, so
	// a crashed controller retains the exact container name it alone may
	// reconcile. It is host controller state: it lives beside the artifact
	// store, is never mounted into a candidate, and holds no credential.
	//
	// Omitting it is what disabled candidate.run in production. The assurance
	// verifier sets both fields inside Assure and therefore worked; the tool
	// broker received this shared value and refused every brokered command with
	// "runtime-owned Docker operation identity and state directory are
	// required", so the model was offered a verification tool that could never
	// run.
	sandbox := runtime.DockerSandbox{
		Image:    config.Assurance.Image,
		Endpoint: runtime.DockerEndpoint{Host: config.Assurance.DockerHost},
		StateDir: filepath.Join(config.StateDir, "artifacts", "docker-operations"),
	}

	credentials := githubCredentials(config.GitHub.CredentialMode, config.GitHub.TokenPath)
	forge := overrides.GitHub
	if forge == nil {
		forge = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    config.GitHub.Endpoint,
			Credentials: credentials,
		}
	}

	registry, err := config.AgentRegistry()
	if err != nil {
		release()
		return nil, err
	}
	agent, err := registry.Agent(flags.Agent)
	if err != nil {
		release()
		return nil, err
	}
	feedback, err := config.FeedbackPolicy()
	if err != nil {
		release()
		return nil, err
	}
	customization, err := planning.LoadRegistry(config.PlanningDir)
	if err != nil {
		release()
		return nil, err
	}
	// NewEngineeringRuntime fails closed on provider isolation, so there is no
	// second check here.
	provider, providerInjected := overrides.Provider, overrides.Provider != nil
	if provider == nil {
		provider = executionProvider(config, agent, artifacts, sandbox, flags.PermissionBypass)
	}
	assurance := overrides.Assurance
	if assurance == nil {
		assurance = runtime.BaselineGoVerifier{
			Sandbox:            sandbox,
			ArtifactStore:      artifacts,
			DependencyCacheDir: config.Assurance.DependencyCacheDir,
		}
	}
	semantic := overrides.SemanticAssurance
	if semantic == nil {
		semantic = semanticAssuranceProvider(config, artifacts)
	}
	return &composition{
		config: config, store: store, owner: owner, model: model, policy: policy,
		artifacts: artifacts, credentials: credentials, build: build,
		forge: forge, provider: provider, assurance: assurance, semantic: semantic,
		agents: registry, agent: agent, feedback: feedback, planning: customization,
		sandbox: sandbox, permissionBypass: flags.PermissionBypass, providerInjected: providerInjected,
		storage: runtime.StateStorage{Dir: config.StateDir, CeilingBytes: config.Storage.MaxStateBytes},
		release: release,
	}, nil
}

// engine binds the shared composition to one repository. It opens nothing and
// contacts nothing, so building one per enrolled repository is cheap.
func (c *composition) engine(target runtime.RepositoryTarget) (*runtime.EngineeringRuntime, error) {
	return c.engineFor(target, c.agent)
}

// engineFor binds the shared composition to one repository worked by ONE agent.
//
// The agent is a parameter because a supervisor is not single-agent: it drives
// each run with the worker that run is bound to, so the provider adapter has to
// be built per pairing rather than once for whichever agent this process was
// started with.
func (c *composition) engineFor(target runtime.RepositoryTarget, agent runtime.ResolvedAgent) (*runtime.EngineeringRuntime, error) {
	remote, err := runtime.GovernedRemote(target.Remote)
	if err != nil {
		return nil, err
	}
	feedback := c.feedbackPolicyFor(target.Identity)
	// An injected provider is a test seam and stays authoritative. Otherwise
	// the adapter is built for THIS agent, which is what makes one supervisor
	// able to run codex and claude side by side.
	provider := c.provider
	if !c.providerInjected {
		provider = executionProvider(c.config, agent, c.artifacts, c.sandbox, c.permissionBypass)
	}
	return runtime.NewEngineeringRuntime(runtime.Dependencies{
		Store:             c.store,
		Agent:             agent,
		Agents:            c.agents,
		Planning:          c.planning,
		Feedback:          feedback,
		Storage:           c.storage,
		Clock:             runtime.RealClock{},
		Owner:             c.owner,
		Liveness:          runtime.NewLockOwnerLiveness(c.config.StateDir),
		GitHub:            c.forge,
		Provider:          provider,
		Assurance:         c.assurance,
		SemanticAssurance: c.semantic,
		Artifacts:         c.artifacts,
		ProjectModel:      c.model,
		Policy:            c.policy,
		StateDir:          c.config.StateDir,
		Repository:        target,
		Remote:            remote,
		Credentials:       c.credentials,
		ControllerID:      "zenchron-engineering/" + version,
		ControllerBuild:   c.build,
		ConfigDigest:      c.config.Digest,
		Budgets:           c.config.RunBudgets(),
	})
}

// feedbackPolicyFor adds the identity this runtime's own credential acts as in
// THIS repository to the self-loop set.
//
// It is resolved per repository because the credential is repository-scoped:
// there is no ambient forge session, and asking for a viewer without naming a
// repository would be asking a question the credential boundary does not
// answer. A lookup that fails simply leaves the operator-configured self logins
// in place - the gate still refuses everything below the permission threshold,
// so a failed lookup narrows what the runtime can recognize about itself rather
// than widening what it admits.
// feedbackPolicyFor is the operator's DECLARED feedback policy. It deliberately
// does not resolve the runtime's own publishing account.
//
// That identity is resolved by the runtime at observation time instead, against
// the credential actually in use. Binding it here bound it once, at engine
// construction, while the credential is re-read from its file on every request:
// a token rotated while `serve` was alive left the self-loop guard recognizing
// an account the runtime no longer was, and the account it had become would
// have its own comments admitted. It also made engine construction perform a
// live, uncancellable forge request.
func (c *composition) feedbackPolicyFor(string) runtime.FeedbackPolicy {
	return c.feedback
}

// buildEngine is the single-repository entry point: the repository comes from
// --repo or the cwd, and everything else is the shared composition. The
// composition is returned alongside the engine because the operator reads need
// the SAME durable store the engine drives - status reports the repository's
// watch observation state, and every command probes the run identity through it
// - and opening a second handle would be a second view of the same state.
func buildEngine(flags autonomyFlags, overrides autonomyOverrides) (*composition, *runtime.EngineeringRuntime, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}
	target, err := repositoryTarget(cwd, flags.Repo)
	if err != nil {
		return nil, nil, err
	}
	built, err := newComposition(flags, overrides)
	if err != nil {
		return nil, nil, err
	}
	engine, err := built.engine(target)
	if err != nil {
		built.release()
		return nil, nil, err
	}
	return built, engine, nil
}

// repositoryTarget selects the repository this invocation governs. An explicit
// --repo wins; otherwise the cwd's origin is used only when it is unambiguous;
// otherwise the invocation is refused.
//
// ResolveRepository takes the identity from the explicit argument but leaves
// Remote as the cwd's own origin URL. Cloning that origin while reporting the
// explicit identity would run the whole pipeline against a different repository
// than the one named, so the remote is re-derived from the identity that won.
func repositoryTarget(cwd, explicit string) (runtime.RepositoryTarget, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" && len(strings.Split(explicit, "/")) != 2 {
		return runtime.RepositoryTarget{}, fmt.Errorf("--repo must be owner/name, got %q", explicit)
	}
	target, err := runtime.ResolveRepository(cwd, explicit)
	if err != nil {
		return runtime.RepositoryTarget{}, err
	}
	if explicit != "" {
		target.Remote = "https://github.com/" + target.Identity
	}
	return target, nil
}

// operatorHome is the directory holding the operator's own already-
// authenticated CLI state. It is read once, here in the composition root,
// rather than inside an adapter: which account's session a worker uses is a
// wiring decision, and an adapter that discovered it from the environment would
// be making that decision itself.
func operatorHome() string {
	if home, err := os.UserHomeDir(); err == nil {
		return home
	}
	return os.Getenv("HOME")
}

func githubCredentials(mode string, tokenPath string) runtime.CredentialProvider {
	switch mode {
	case runtime.GitHubCredentialCLI:
		return runtime.GitHubCLICredential{}
	case runtime.GitHubCredentialToken:
		// A publication identity of the runtime's own, so the operator stays a
		// distinct actor whose review is admissible feedback.
		return runtime.GitHubTokenFileCredential{Path: tokenPath}
	}
	// Nil is the documented "github_auth_required" state, not anonymous access.
	return nil
}

// semanticAssuranceProvider builds the INDEPENDENT semantic acceptance
// producer. It is available exactly when the operator configured a brokered
// OpenAI provider with a credential file: the same trusted controller
// credential serves both providers, and no second mandatory secret is invented.
// The candidate never receives it, and the two providers keep distinct
// identities despite the shared account - which is why this file says M0
// independence and never says vendor independence.
func semanticAssuranceProvider(config runtime.Config, artifacts runtime.ArtifactStore) runtime.AssuranceProvider {
	// The producer is available exactly when the operator configured a
	// brokered agent with a credential - whether they wrote it as the pre-#63
	// `provider` block or as an entry in the agent registry. Reading only the
	// old spelling would have made migrating to `agents` silently remove the
	// semantic acceptance producer, so a contract requiring it would start
	// being refused for a reason the operator never chose.
	//
	// It does NOT have to be the agent doing the work, and usually should not
	// be: the same trusted controller credential serves both, no second
	// mandatory secret is invented, and the two keep distinct producer
	// identities despite the shared account. That is M0 independence, and this
	// file has never claimed it is vendor independence.
	agent, ok := brokeredAgent(config)
	if !ok {
		return nil
	}
	return runtime.OpenAISemanticVerifier{
		ArtifactStore: artifacts,
		Model:         agent.Model,
		AuthMode:      agent.DeclaredAuthMode,
		APIKeyFile:    agent.CredentialPath,
		Endpoint:      agent.Endpoint,
		HTTP:          &http.Client{Timeout: 5 * time.Minute},
		Timeout:       5 * time.Minute,
	}
}

// brokeredAgent is the configured protected provider, if there is one. The
// default agent wins when it is brokered; otherwise the first brokered agent in
// deterministic order is used, so the answer does not depend on map iteration.
func brokeredAgent(config runtime.Config) (runtime.ResolvedAgent, bool) {
	registry, err := config.AgentRegistry()
	if err != nil {
		return runtime.ResolvedAgent{}, false
	}
	usable := func(agent runtime.ResolvedAgent) bool {
		return agent.Kind == runtime.AgentKindOpenAIResponses && strings.TrimSpace(agent.CredentialPath) != ""
	}
	if agent, err := registry.Agent(""); err == nil && usable(agent) {
		return agent, true
	}
	for _, agent := range registry.All() {
		if usable(agent) {
			return agent, true
		}
	}
	return runtime.ResolvedAgent{}, false
}

// executionProvider builds the adapter for ONE resolved agent. It is the
// composition root's whole provider-specific surface: adding a provider adds a
// case here, an adapter, and configuration - and touches nothing in the
// scheduler, the kernel or the authority evaluator.
//
// The credential is only ever a path from the operator layer; no token value
// passes through here, and a native CLI is handed none at all because it
// authenticates itself.
func executionProvider(config runtime.Config, agent runtime.ResolvedAgent, artifacts runtime.ArtifactStore, sandbox runtime.DockerSandbox, bypass bool) runtime.ExecutionProvider {
	if agent.NativeCLI() {
		return runtime.CLIAgentProvider{
			Agent:             agent,
			ArtifactStore:     artifacts,
			OperatorHome:      operatorHome(),
			PermissionBypass:  bypass,
			LegacyEnvironment: agent.Legacy && agent.Kind == runtime.AgentKindCodexCLI,
		}
	}
	return candidateBoundProvider{base: runtime.OpenAIProvider{
		ArtifactStore: artifacts,
		Model:         agent.Model,
		AuthMode:      agent.DeclaredAuthMode,
		APIKeyFile:    agent.CredentialPath,
		Endpoint:      agent.Endpoint,
		HTTP:          &http.Client{Timeout: 10 * time.Minute},
		Broker: runtime.ToolBroker{
			Sandbox: sandbox,
			// The same operator cache assurance verifies against, attached to
			// brokered commands read-only. Without it a brokered `go test` has
			// no offline module source at all.
			DependencyCacheDir: config.Assurance.DependencyCacheDir,
		},
		Timeout: 10 * time.Minute,
	}}
}

// candidateBoundProvider binds the tool broker to the candidate workspace named
// by each request. OpenAIProvider refuses a broker bound to any other tree, and
// the tree only exists once a run has been created, so the binding cannot be
// made when the provider is constructed.
type candidateBoundProvider struct{ base runtime.OpenAIProvider }

func (p candidateBoundProvider) Isolation() runtime.ProviderIsolation { return p.base.Isolation() }

// Execute binds the two things the broker cannot supply itself: WHICH workspace
// this invocation may touch, and WHICH runtime operation owns the Docker
// lifecycle of anything it brokers.
//
// Both are refused when absent rather than defaulted. A brokered command with
// no owning operation has no durable record a crashed controller could
// reconcile against, and the alternatives - a fixed global id, the process id,
// a random or model-supplied string - would each let recovery target a
// container this operation does not own.
func (p candidateBoundProvider) Execute(ctx context.Context, request runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	bound, err := p.bind(request)
	if err != nil {
		return runtime.ExecutionResult{}, err
	}
	return bound.Execute(ctx, request)
}

// bind is the binding itself, separated so a test can assert what the provider
// would have been given without contacting a model or a daemon.
func (p candidateBoundProvider) bind(request runtime.ExecutionRequest) (runtime.OpenAIProvider, error) {
	if strings.TrimSpace(request.CandidateDir) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires the runtime-owned candidate workspace")
	}
	if strings.TrimSpace(request.OperationID) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires the runtime operation that authorized it; without it a brokered container has no exact identity to reconcile")
	}
	if strings.TrimSpace(p.base.Broker.Sandbox.StateDir) == "" {
		return runtime.OpenAIProvider{}, fmt.Errorf("brokered execution requires a runtime-owned state directory for its Docker operation record")
	}
	bound := p.base
	bound.Broker.CandidateDir = request.CandidateDir
	bound.Broker.Sandbox.OperationID = request.OperationID
	return bound, nil
}

func parseAutonomyFlags(args []string) (autonomyFlags, error) {
	var flags autonomyFlags
	for len(args) > 0 {
		switch args[0] {
		case "--follow":
			flags.Follow, args = true, args[1:]
			continue
		case "--text":
			flags.Text, args = true, args[1:]
			continue
		case "--new-generation":
			flags.NewGeneration, args = true, args[1:]
			continue
		case "--detached":
			flags.Detached, args = true, args[1:]
			continue
		case "--dangerous-permission-bypass":
			// Named for what it is. It is one half of the two independent
			// statements an unsafe provider mode requires; without standing
			// operator configuration for that agent it is refused before any
			// process starts.
			flags.PermissionBypass, args = true, args[1:]
			continue
		case "--dry-run":
			flags.DryRun, args = true, args[1:]
			continue
		case "--deterministic":
			// Compile the plan with NO model invocation. It is the honest
			// alternative to reasoning, not a fallback from it: a provider that
			// cannot plan is refused with its reason rather than quietly
			// producing a deterministic plan the operator did not ask for.
			flags.Deterministic, args = true, args[1:]
			continue
		case "--approve", "--reject":
			// The two decisions are mutually exclusive flags rather than one
			// --decision value, so a typo is a usage error instead of an
			// unrecognised answer reaching the authority boundary.
			if flags.Decision != "" {
				return autonomyFlags{}, errors.New("exactly one of --approve or --reject may be given")
			}
			flags.Decision, args = strings.TrimPrefix(args[0], "--"), args[1:]
			continue
		}
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return autonomyFlags{}, errors.New(autonomyUsage)
		}
		switch args[0] {
		case "--repo":
			flags.Repo = args[1]
		case "--config":
			flags.Config = args[1]
		case "--note":
			flags.Note = args[1]
		case "--agent":
			flags.Agent = args[1]
		case "--reason":
			flags.Reason = args[1]
		case "--template":
			flags.Template = args[1]
		case "--substitute-human":
			flags.SubstituteHuman = args[1]
		case "--digest":
			flags.Digest = args[1]
		case "--revision":
			revision, err := strconv.Atoi(args[1])
			if err != nil || revision < 1 {
				return autonomyFlags{}, fmt.Errorf("--revision must be a positive integer, got %q", args[1])
			}
			flags.Revision = revision
		case "--assign":
			issue, agent, ok := strings.Cut(args[1], "=")
			number, err := strconv.Atoi(strings.TrimSpace(issue))
			if !ok || err != nil || number <= 0 || strings.TrimSpace(agent) == "" {
				return autonomyFlags{}, fmt.Errorf("--assign must be ISSUE=AGENT, got %q", args[1])
			}
			if flags.Assign == nil {
				flags.Assign = map[int]string{}
			}
			flags.Assign[number] = strings.TrimSpace(agent)
		default:
			return autonomyFlags{}, errors.New(autonomyUsage)
		}
		args = args[2:]
	}
	return flags, nil
}

// ---------------------------------------------------------------------------
// watch
// ---------------------------------------------------------------------------

// autonomyWatch runs the daemon loop: tick, render, wait until the controller
// says the next poll is eligible, repeat. That is the WHOLE loop. Which
// repositories are polled, when each one is next eligible, what a rate-limit or
// auth failure does to its backoff, and which runs are claimed and driven are
// all decisions inside runtime.WatchController - this function cannot see them
// and must not learn to.
//
// Two failure kinds are deliberately distinguished. A GLOBAL configuration
// fault is unrecoverable and ends the process. A per-repository fault - auth,
// rate limit, a repository that no longer exists - is reported by the
// controller INSIDE the tick report and never as an error, so it can never end
// the loop or affect the other enrolled repositories.
func autonomyWatch(parent context.Context, flags autonomyFlags, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	// Global configuration is validated, the durable store is opened, and the
	// ownership lock is taken BEFORE anything is polled.
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	settings, err := built.watchSettings()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	controller := overrides.Watch
	if controller == nil {
		real, err := built.watchController(settings, false)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		controller = real
	}

	// SIGINT/SIGTERM stops the WATCHER. Cancelling this context stops
	// discovery and stops scheduling, and it is the same context the controller
	// threads into the runs it is driving, so an in-flight operation unwinds
	// through the cancellation semantics providers and the Docker sandbox
	// already honour. It is NOT a cancellation of the runs themselves: no
	// run.cancelled is appended here, the durable journal is left exactly as
	// the last completed step wrote it, and every run stays resumable.
	// `autonomy stop <run>` is the only thing that cancels a run.
	ctx, stopSignals := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	wait := overrides.WatchWait
	if wait == nil {
		wait = waitUntil
	}
	for ctx.Err() == nil {
		report, err := controller.Tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break // the signal arrived mid-tick; that is a shutdown, not a fault
			}
			var configErr *runtime.ConfigError
			if errors.As(err, &configErr) {
				return runtime.ExitInvalid, err
			}
			return runtime.ExitFailed, err
		}
		if err := writeJSON(stdout, report); err != nil {
			return runtime.ExitFailed, err
		}
		if ctx.Err() != nil {
			break
		}
		wait(ctx, report.NextEligibleAt)
	}
	return runtime.ExitCompleted, nil
}

// watchSettings is the effective watch configuration, refused through the same
// typed *runtime.ConfigError every other configuration fault uses so the
// operator gets one actionable message shape.
func (c *composition) watchSettings() (runtime.WatchSettings, error) {
	settings, err := c.config.WatchSettings()
	if err != nil {
		return runtime.WatchSettings{}, &runtime.ConfigError{Path: c.config.OperatorPath, Detail: err.Error()}
	}
	if len(settings.Repositories) == 0 {
		return runtime.WatchSettings{}, &runtime.ConfigError{
			Path:   c.config.OperatorPath,
			Detail: "watch.repositories is empty: watch observes only repositories an operator enrolled, so there is nothing to watch",
		}
	}
	return settings, nil
}

// watchController hands the controller its dependencies, including the engine
// factory. Watch never builds a provider or a credential of its own: every
// engine it drives comes out of the same composition, so a watched repository
// is governed by exactly the configuration this invocation validated.
// watchController builds the discovery controller. intakeOnly separates its two
// callers: standalone `autonomy watch` is the only thing running and therefore
// both discovers and drives, while under `serve` the supervisor owns driving
// and discovery contributes intake alone.
func (c *composition) watchController(settings runtime.WatchSettings, intakeOnly bool) (*runtime.WatchController, error) {
	return runtime.NewWatchController(runtime.WatchDependencies{
		Store:      c.store,
		Clock:      runtime.RealClock{},
		Owner:      c.owner,
		Liveness:   runtime.NewLockOwnerLiveness(c.config.StateDir),
		GitHub:     c.forge,
		Settings:   settings,
		IntakeOnly: intakeOnly,
		Runtime: func(repo runtime.GitHubRepo) (*runtime.EngineeringRuntime, error) {
			return c.engine(runtime.RepositoryTarget{
				Identity:      repo.String(),
				Remote:        repo.CloneURL(),
				DefaultBranch: watchedDefaultBranch,
			})
		},
	})
}

// waitUntil blocks until the controller's next eligible instant or until the
// process is asked to stop. The one-second floor is not a policy: it only stops
// a controller that reports an instant already in the past from turning the
// loop into a busy wait.
func waitUntil(ctx context.Context, until time.Time) {
	delay := time.Until(until)
	if delay < time.Second {
		delay = time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// ---------------------------------------------------------------------------
// stop
// ---------------------------------------------------------------------------

// stopReason is the recorded cause of an operator cancellation. It distinguishes
// a run a person stopped from a run the source itself cancelled, and from the
// generation boundary an explicit source refresh records.
const stopReason = "operator_stop"

// autonomyStop is the counterpart that makes watch's shutdown semantics
// meaningful: stopping the daemon stops WATCHING, while this - and only this -
// cancels a RUN. The intent is recorded in the run's own durable journal, which
// is the same log `events` and `status` read; there is no second log.
//
// Cancelling is durable, so it survives a restart: the journal holds
// run.cancelled and the run document reads cancelled, and every later pass -
// including the runtime's own conditions() - re-derives the cancellation from
// that. It is idempotent: a second stop appends nothing and reports the same
// answer. Scheduling stops because a cancelled run is terminal, and the active
// bounded operation is cancelled through the existing mechanism rather than a
// new one - see cancelRun.
func autonomyStop(flags autonomyFlags, overrides autonomyOverrides, runID string, stdout io.Writer) (int, error) {
	built, err := newComposition(flags, overrides)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	defer built.release()

	outcome, err := cancelRun(built, runID, stopReason)
	if err != nil {
		return exitFor(err, runtime.ExitFailed), err
	}
	if err := writeJSON(stdout, outcome); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCancelled, nil
}

// cancelRun records durable operator cancellation intent for one run. The
// mechanism lives in the runtime, because `stop RUN` and the supervisor's
// explicit stop-all action must be one cancellation path rather than two
// answers to "is this run cancelled".
func cancelRun(built *composition, runID, reason string) (runtime.Outcome, error) {
	if _, err := requireRun(built, runID); err != nil {
		return runtime.Outcome{}, err
	}
	scheduler := runtime.Scheduler{Store: built.store, Clock: runtime.RealClock{}, Owner: built.owner}
	return runtime.CancelRun(built.store, scheduler, time.Now().UTC(), runID, reason)
}
