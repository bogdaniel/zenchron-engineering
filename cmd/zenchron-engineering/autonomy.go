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
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const autonomyUsage = "usage: zenchron-engineering autonomy {run issue <number>|status <run> [--text]|events <run> [--follow]|resume <run>|refresh <run>|authorize <run> <request-id> --approve|--reject [--note <text>]|stop <run>|watch|doctor [--text]|gc [--dry-run]} [--repo owner/name] [--config <path>]"

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
	Runtime   engineeringRuntime
	Watch     watchController
	// ControllerBuild replaces this process's own provenance. Production
	// resolves it from the injected build metadata and the running executable;
	// a test supplies a fixed build so the identity under assertion is the one
	// the test wrote, not whatever binary the test runner happens to be.
	ControllerBuild *runtime.ControllerBuild
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
	case "watch":
		flags, err := parseAutonomyFlags(rest)
		if err != nil {
			return runtime.ExitInvalid, err
		}
		return autonomyWatch(context.Background(), flags, overrides, stdout)
	case "gc":
		return autonomyGC(rest, overrides, stdout)
	}

	// Everything else names exactly one subject: an issue number for `run`, a
	// run id for the rest, and additionally a request id for `authorize`.
	var issue int
	var runID, requestID string
	switch command {
	case "run":
		if len(rest) < 2 || rest[0] != "issue" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		number, err := strconv.Atoi(rest[1])
		if err != nil || number <= 0 {
			return runtime.ExitInvalid, fmt.Errorf("issue number must be a positive integer, got %q", rest[1])
		}
		issue, rest = number, rest[2:]
	case "authorize":
		if len(rest) < 2 || strings.TrimSpace(rest[0]) == "" || strings.TrimSpace(rest[1]) == "" {
			return runtime.ExitInvalid, errors.New(autonomyUsage)
		}
		runID, requestID, rest = rest[0], rest[1], rest[2:]
	case "status", "events", "resume", "refresh", "stop":
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
		id, err := engine.StartOrResumeIssueRun(ctx, issue)
		if err != nil {
			return runtime.ExitFailed, err
		}
		return reconcile(ctx, engine, id, stdout)
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

func reconcile(ctx context.Context, engine engineeringRuntime, runID string, stdout io.Writer) (int, error) {
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
	build       runtime.ControllerBuild
	release     func()
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
	sandbox := runtime.DockerSandbox{Image: config.Assurance.Image, Endpoint: runtime.DockerEndpoint{Host: config.Assurance.DockerHost}}

	credentials := githubCredentials(config.GitHub.CredentialMode)
	forge := overrides.GitHub
	if forge == nil {
		forge = runtime.GitHubRESTAdapter{
			HTTP:        &http.Client{Timeout: 30 * time.Second},
			Endpoint:    config.GitHub.Endpoint,
			Credentials: credentials,
		}
	}

	// NewEngineeringRuntime fails closed on provider isolation, so there is no
	// second check here.
	provider := overrides.Provider
	if provider == nil {
		provider = executionProvider(config, artifacts, sandbox)
	}
	assurance := overrides.Assurance
	if assurance == nil {
		assurance = runtime.BaselineGoVerifier{
			Sandbox:            sandbox,
			ArtifactStore:      artifacts,
			DependencyCacheDir: config.Assurance.DependencyCacheDir,
		}
	}
	return &composition{
		config: config, store: store, owner: owner, model: model, policy: policy,
		artifacts: artifacts, credentials: credentials, build: build,
		forge: forge, provider: provider, assurance: assurance, release: release,
	}, nil
}

// engine binds the shared composition to one repository. It opens nothing and
// contacts nothing, so building one per enrolled repository is cheap.
func (c *composition) engine(target runtime.RepositoryTarget) (*runtime.EngineeringRuntime, error) {
	remote, err := runtime.GovernedRemote(target.Remote)
	if err != nil {
		return nil, err
	}
	return runtime.NewEngineeringRuntime(runtime.Dependencies{
		Store:           c.store,
		Clock:           runtime.RealClock{},
		Owner:           c.owner,
		Liveness:        runtime.NewLockOwnerLiveness(c.config.StateDir),
		GitHub:          c.forge,
		Provider:        c.provider,
		Assurance:       c.assurance,
		Artifacts:       c.artifacts,
		ProjectModel:    c.model,
		Policy:          c.policy,
		StateDir:        c.config.StateDir,
		Repository:      target,
		Remote:          remote,
		Credentials:     c.credentials,
		ControllerID:    "zenchron-engineering/" + version,
		ControllerBuild: c.build,
		ConfigDigest:    c.config.Digest,
		Budgets:         c.config.RunBudgets(),
	})
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

func githubCredentials(mode string) runtime.CredentialProvider {
	if mode == runtime.GitHubCredentialCLI {
		return runtime.GitHubCLICredential{}
	}
	// Nil is the documented "github_auth_required" state, not anonymous access.
	return nil
}

// executionProvider builds the configured AI provider. The credential is only
// ever a path from the operator layer; no token value passes through here.
func executionProvider(config runtime.Config, artifacts runtime.ArtifactStore, sandbox runtime.DockerSandbox) runtime.ExecutionProvider {
	if config.Provider.Kind == runtime.ProviderNativeCodex {
		return runtime.NativeCodexProvider{
			ArtifactStore: artifacts,
			Model:         config.Provider.Model,
			AuthMode:      config.Provider.AuthMode,
			CodexHome:     config.Provider.CredentialPath,
		}
	}
	return candidateBoundProvider{base: runtime.OpenAIProvider{
		ArtifactStore: artifacts,
		Model:         config.Provider.Model,
		AuthMode:      config.Provider.AuthMode,
		APIKeyFile:    config.Provider.CredentialPath,
		Endpoint:      config.Provider.Endpoint,
		HTTP:          &http.Client{Timeout: 10 * time.Minute},
		Broker:        runtime.ToolBroker{Sandbox: sandbox},
		Timeout:       10 * time.Minute,
	}}
}

// candidateBoundProvider binds the tool broker to the candidate workspace named
// by each request. OpenAIProvider refuses a broker bound to any other tree, and
// the tree only exists once a run has been created, so the binding cannot be
// made when the provider is constructed.
type candidateBoundProvider struct{ base runtime.OpenAIProvider }

func (p candidateBoundProvider) Isolation() runtime.ProviderIsolation { return p.base.Isolation() }

func (p candidateBoundProvider) Execute(ctx context.Context, request runtime.ExecutionRequest) (runtime.ExecutionResult, error) {
	bound := p.base
	bound.Broker.CandidateDir = request.CandidateDir
	return bound.Execute(ctx, request)
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
		case "--dry-run":
			flags.DryRun, args = true, args[1:]
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
		real, err := built.watchController(settings)
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
func (c *composition) watchController(settings runtime.WatchSettings) (*runtime.WatchController, error) {
	return runtime.NewWatchController(runtime.WatchDependencies{
		Store:    c.store,
		Clock:    runtime.RealClock{},
		Owner:    c.owner,
		Liveness: runtime.NewLockOwnerLiveness(c.config.StateDir),
		GitHub:   c.forge,
		Settings: settings,
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

// cancelRun records durable operator cancellation intent for one run and stops
// its scheduling. It is shared by `stop` and by the generation boundary
// `refresh` records, so there is exactly one cancellation path and both are
// idempotent in the same way.
//
// Three things happen, in this order, and nothing else does:
//
//  1. run.cancelled is appended through the journal, which is the authority
//     every later replay reads. Because it is durable, the run is still
//     cancelled after a restart.
//  2. the run document is settled as cancelled, which is what stops the
//     scheduler from handing this run out again.
//  3. every operation the store still believes is active has cancellation
//     REQUESTED on it through the scheduler's existing mechanism. That is the
//     one the runtime already honours: Scheduler.Next refuses to hand out an
//     operation with cancel_requested set, so the bounded operation stops
//     being scheduled and a controller still holding it unwinds through the
//     cancellation semantics it already implements. No second cancellation
//     mechanism is introduced here, and no lease another process owns is
//     written out from under it.
func cancelRun(built *composition, runID, reason string) (runtime.Outcome, error) {
	run, err := requireRun(built, runID)
	if err != nil {
		return runtime.Outcome{}, err
	}
	outcome := runtime.Outcome{RunID: runID, Disposition: runtime.Cancelled, Reason: reason}
	// Cancelling an already cancelled run is a no-op rather than a second
	// journal entry, so a retried command reports the same answer.
	if run.Disposition == runtime.Cancelled {
		outcome.Reason = run.Reason
		return outcome, nil
	}
	now := time.Now().UTC()
	if _, err := built.store.AppendEvent(runtime.EngineeringEvent{
		SchemaVersion: runtime.SchemaVersion,
		ID:            fmt.Sprintf("%s-%s-%d", runID, reason, now.UnixNano()),
		RunID:         runID,
		Type:          runtime.EventRunCancelled,
		OccurredAt:    now,
		Payload:       json.RawMessage(`{"reason":"` + reason + `"}`),
	}); err != nil {
		return runtime.Outcome{}, err
	}
	run.Disposition, run.Reason, run.UpdatedAt = runtime.Cancelled, reason, now
	if err := built.store.PutRun(run); err != nil {
		return runtime.Outcome{}, err
	}
	if err := cancelActiveOperations(built, runID); err != nil {
		return runtime.Outcome{}, err
	}
	return outcome, nil
}

// cancelActiveOperations requests cancellation of the run's leased or running
// operations through the scheduler, which is the mechanism the runtime already
// has for exactly this. It writes no lease and finishes no operation another
// process may still be executing.
func cancelActiveOperations(built *composition, runID string) error {
	operations, err := built.store.Operations(runID)
	if err != nil {
		return err
	}
	scheduler := runtime.Scheduler{Store: built.store, Clock: runtime.RealClock{}, Owner: built.owner}
	for _, op := range operations {
		if op.State != runtime.Leased && op.State != runtime.Running {
			continue
		}
		if _, err := scheduler.RequestCancel(op.ID); err != nil {
			return err
		}
	}
	return nil
}
