package runtime

// watch.go is the Phase 9 scheduling and discovery cycle, and it is ONLY that.
//
// One Tick is: read the operator's validated repository registration, poll the
// repositories whose not-before has elapsed, claim the runs their opted-in
// issues name, and hand each claimed run to the EXISTING reconciler. It is not
// a second reconciler. There is no planner here, no operation validator, no
// remediation, no publication, no pull-request observation and no authority
// evaluation: every one of those lives in EngineeringRuntime and is reached
// through exactly one call, Reconcile.
//
// Four properties are structural rather than conventional:
//
//   - Enrolment is operator authority. The loop iterates WatchSettings and
//     nothing else, so a repository that is not registered is never polled,
//     never constructed, and never able to enrol itself. The durable watch
//     state is keyed by the identity configuration supplied and is never
//     enumerated, so a repository the operator dropped cannot come back.
//   - The opt-in label is consent. An issue is a source because an operator
//     labelled it; losing the label withdraws the consent and the run stops
//     being driven. Watch re-checks the label itself rather than trusting the
//     forge's server-side filter to be the authority.
//   - Time is injected. Tick reads the clock once and derives every deadline
//     from that instant. It never sleeps, never retries in a loop, and never
//     calls time.Now, so two watchers cannot disagree because one of them
//     looked at wall time.
//   - No credential is here, and none reaches durable watch state: watch holds
//     observations - a cursor, an ETag, timings, a rate-limit reading and an
//     error class - and hands the credentialed work to the runtime the CLI
//     built.
//
// The one thing watch must NEVER do is turn its own desire for progress into
// authority. A run waiting on a publication decision is reported as an
// actionable diagnostic and left waiting; discovering later that a run needs a
// privilege policy did not grant is a privilege EXPANSION, and #8 refuses it on
// purpose. Nothing below infers permission from a label, from a completed
// provider, from passing tests, or from a stalled queue.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Dependencies
// ---------------------------------------------------------------------------

// WatchDependencies is the complete input to a watch controller. Every external
// system is a seam and nothing is discovered from ambient state.
type WatchDependencies struct {
	Store    *SQLiteOperationStore
	Clock    Clock
	Owner    string
	Liveness OwnerLiveness
	GitHub   GitHubAdapter
	Settings WatchSettings // from OperatorConfig.WatchSettings()
	// Runtime builds the Phase 8 engine for one repository. The CLI supplies
	// this so watch never constructs providers/credentials itself.
	Runtime func(repo GitHubRepo) (*EngineeringRuntime, error)
}

// WatchController performs one deterministic scheduling cycle per Tick. It
// holds no run state, no queue, and no timer: everything that must survive a
// restart is in the durable store, and when to call again is the CLI's
// decision.
type WatchController struct{ deps WatchDependencies }

// watchMaxBackoff bounds the exponential growth. A repository that has been
// failing for hours is still re-checked twice an hour, so an operator fix is
// noticed without a human restarting anything.
const watchMaxBackoff = 30 * time.Minute

// maxWatchDetailBytes bounds a repository diagnostic. Detail can quote a forge
// error, which is untrusted third-party text: it is bounded, flattened, and
// carried as data.
const maxWatchDetailBytes = 240

// The typed waits watch records on an EXISTING run. Both are scheduling facts -
// the operator withdrew consent, or the credential the run needs is gone - so
// they are recorded as a disposition and never as new work.
//
// There is deliberately no sixth "paused" disposition: a run that must not
// proceed is Waiting with a reason, exactly like every other wait.
const (
	WatchWaitingOptInRemoved = "opt_in_removed"
	WatchWaitingGitHubAuth   = "github_auth_required"
)

// NewWatchController validates the dependency set. The registration is
// re-validated here rather than trusted, so a hand-built WatchSettings cannot
// smuggle in an identity the runtime is not allowed to contact.
func NewWatchController(d WatchDependencies) (*WatchController, error) {
	if d.Store == nil {
		return nil, &DependencyError{Detail: "a durable operation store is required"}
	}
	if d.GitHub == nil {
		return nil, &DependencyError{Detail: "a GitHub adapter is required"}
	}
	if d.Runtime == nil {
		return nil, &DependencyError{Detail: "a per-repository runtime factory is required"}
	}
	if strings.TrimSpace(d.Owner) == "" {
		return nil, &DependencyError{Detail: "a scheduler owner identity is required"}
	}
	if d.Settings.PollInterval < MinWatchPollSeconds*time.Second {
		return nil, &DependencyError{Detail: fmt.Sprintf("the watch poll interval must be at least %ds", MinWatchPollSeconds)}
	}
	if strings.TrimSpace(d.Settings.Label) == "" {
		d.Settings.Label = DefaultWatchLabel
	}
	d.Settings.MaxConcurrentRuns = resolveMaxConcurrentRuns(0, d.Settings.MaxConcurrentRuns)
	enrolled := map[string]bool{}
	for _, repo := range d.Settings.Repositories {
		if _, err := repo.identity(); err != nil {
			return nil, &DependencyError{Detail: "watch repository: " + err.Error()}
		}
		key := strings.ToLower(repo.String())
		if enrolled[key] {
			return nil, &DependencyError{Detail: fmt.Sprintf("watch repository %q is enrolled more than once", repo)}
		}
		enrolled[key] = true
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	if d.Liveness == nil {
		// An absent liveness source must never read as "the owner is dead":
		// that is the same one-sided rule the scheduler applies.
		d.Liveness = NewProcessOwnerLiveness()
	}
	return &WatchController{deps: d}, nil
}

// ---------------------------------------------------------------------------
// Report
// ---------------------------------------------------------------------------

// TickReport is what one cycle observed. The CLI decides when to call again;
// NextEligibleAt is the earliest instant at which calling would do anything.
type TickReport struct {
	Repositories   []RepositoryWatchReport
	ActiveRuns     int
	NextEligibleAt time.Time
}

// RepositoryWatchReport is one repository's cycle. Discovered is the size of
// the opted-in set this tick acted on - the freshly observed set after a
// complete poll, or the remembered set when the poll was skipped or answered
// 304. Driven names the runs handed to the reconciler, in the same
// deterministic order they were driven.
type RepositoryWatchReport struct {
	Repository     GitHubRepo
	LastPollAt     time.Time
	NextEligibleAt time.Time
	Discovered     int
	Driven         []string // run ids driven this tick
	ErrorClass     WatchErrorClass
	Detail         string
	RateLimit      RateLimitObservation
}

// note appends one bounded diagnostic. Diagnostics accumulate rather than
// overwrite, so a repository that both backed off and reported a waiting run
// says both.
func (r *RepositoryWatchReport) note(detail string) {
	if r.Detail != "" {
		detail = r.Detail + "; " + detail
	}
	r.Detail = watchDetail(detail)
}

func watchDetail(detail string) string {
	return boundUntrusted(singleLine(detail), maxWatchDetailBytes)
}

// ---------------------------------------------------------------------------
// The cycle
// ---------------------------------------------------------------------------

// Tick performs one scheduling cycle:
//
//	registration -> per repository: not-before? -> discover -> claim
//	  -> capacity -> Reconcile -> persist observation -> report
//
// A failure against one repository is recorded in that repository's report and
// the loop continues, so one broken forge, credential, or configuration cannot
// stop the others being processed.
func (w *WatchController) Tick(ctx context.Context) (TickReport, error) {
	if err := ctx.Err(); err != nil {
		return TickReport{}, err
	}
	// One clock read per cycle. Every deadline this tick records derives from
	// it, so a report is internally consistent and a test is deterministic.
	now := w.deps.Clock.Now()
	active, elsewhere, err := w.drivenRuns()
	if err != nil {
		return TickReport{}, err
	}
	// The global run-driving capacity. Runs a live OTHER owner is driving
	// already occupy the ceiling; this is the cheap early exit, and the
	// durable AcquireOperation inside the scheduler remains the authority.
	capacity := elsewhere < w.deps.Settings.MaxConcurrentRuns
	report := TickReport{ActiveRuns: active}
	for _, repo := range w.deps.Settings.Repositories {
		if err := ctx.Err(); err != nil {
			return TickReport{}, err
		}
		observed := w.tickRepository(ctx, repo, now, capacity)
		report.Repositories = append(report.Repositories, observed)
		if next := observed.NextEligibleAt; !next.IsZero() && (report.NextEligibleAt.IsZero() || next.Before(report.NextEligibleAt)) {
			report.NextEligibleAt = next
		}
	}
	return report, nil
}

// drivenRuns counts the runs currently holding a run-driving slot, and how many
// of those belong to another live owner. A lease whose owner cannot be proved
// dead counts as alive, exactly as the scheduler treats it.
func (w *WatchController) drivenRuns() (active int, elsewhere int, err error) {
	operations, err := w.deps.Store.AllOperations()
	if err != nil {
		return 0, 0, err
	}
	driving, foreign := map[string]bool{}, map[string]bool{}
	for _, op := range operations {
		if (op.State != Leased && op.State != Running) || op.Lease == nil {
			continue
		}
		if !w.deps.Liveness.Alive(op.Lease.Owner) {
			continue
		}
		driving[op.RunID] = true
		if op.Lease.Owner != w.deps.Owner {
			foreign[op.RunID] = true
		}
	}
	return len(driving), len(foreign), nil
}

func (w *WatchController) tickRepository(ctx context.Context, repo GitHubRepo, now time.Time, capacity bool) RepositoryWatchReport {
	report := RepositoryWatchReport{Repository: repo}
	state, revision, _, err := w.deps.Store.WatchStateFor(repo.String())
	if err != nil {
		report.ErrorClass, report.Detail = WatchErrorTransient, watchDetail(err.Error())
		return report
	}
	// Start from what durable state already says, so a skipped poll still
	// reports the standing backoff and the last diagnostic.
	report.LastPollAt, report.NextEligibleAt = state.LastSuccessAt, state.NotBefore
	report.ErrorClass, report.Detail = state.LastErrorClass, state.LastErrorDetail
	report.RateLimit = RateLimitObservation{Remaining: state.RateLimit.Remaining, ResetAt: state.RateLimit.ResetAt}

	engine, err := w.deps.Runtime(repo)
	if err != nil {
		// A repository whose runtime cannot be built is an operator problem,
		// not something to route around: it is recorded with a bounded backoff
		// so a fixed configuration is picked up without a restart.
		w.recordFailure(&report, state, revision, now, WatchErrorPermanent, err.Error(), RateLimitObservation{})
		return report
	}
	// The set the last COMPLETE observation of this repository returned. It is
	// what makes "the label was removed from everything" distinguishable from
	// "nothing changed", which a 304 by itself cannot answer.
	watched := parseWatchedIssues(state.Cursor)

	if now.Before(state.NotBefore) {
		// The forge is not contacted for discovery before its not-before. The
		// instant is durable, so a restart does not reset a backoff the forge
		// asked for. Already-claimed runs are still driven: discovery cadence
		// and run reconciliation are independent.
		report.Discovered = len(watched)
		w.drive(ctx, engine, &report, w.claim(ctx, engine, &report, watched), capacity && contactable(state.LastErrorClass))
		return report
	}

	result, err := w.deps.GitHub.DiscoverIssues(ctx, DiscoveryQuery{
		Repo: repo, Label: w.deps.Settings.Label, ETag: state.ETag,
	})
	if err != nil {
		class, rate := classifyWatchError(err)
		w.recordFailure(&report, state, revision, now, class, err.Error(), rate)
		if class == WatchErrorAuth {
			// The credential is gone. An EXISTING run that needs GitHub waits
			// with a typed reason: no provider is invoked, no candidate is
			// mutated, and nothing is retried harder. A repository with no run
			// yet gets a repository-level diagnostic and a bounded backoff -
			// deliberately NOT a fabricated EngineeringRun standing in for the
			// watcher's own auth state. When the credential returns, discovery
			// resumes and a coherent waiting run resumes through the ordinary
			// reconciliation below.
			for _, issue := range watched {
				w.markWaiting(engine, &report, issue, WatchWaitingGitHubAuth)
			}
			return report
		}
		// A transient discovery failure must not freeze runs that are already
		// making progress, for the same reason a 304 must not.
		report.Discovered = len(watched)
		w.drive(ctx, engine, &report, w.claim(ctx, engine, &report, watched), capacity && contactable(class))
		return report
	}

	observed := watched
	if !result.NotModified {
		// A complete view: this IS the consented set now.
		observed = optedIn(result, w.deps.Settings.Label)
		for _, issue := range watched {
			if containsInt(observed, issue) {
				continue
			}
			// Consent withdrawn. The run stops being driven and says why. A
			// later restoration puts the issue back in the observed set, where
			// it resumes through ordinary reconciliation - which is what runs
			// the coherence checks that decide whether it may be active at all
			// (a pinned source that moved meanwhile still waits on the
			// operator, because edited intent is never silently absorbed).
			w.markWaiting(engine, &report, issue, WatchWaitingOptInRemoved)
		}
		for _, issue := range observed {
			// Back in the consented set after having left it. Only the run
			// that is actually waiting on withdrawn consent is restored, so a
			// newly labelled issue is not mistaken for a restoration.
			if !containsInt(watched, issue) {
				w.markOptInRestored(engine, &report, issue)
			}
		}
	}
	w.recordSuccess(&report, state, revision, now, result, observed)
	report.Discovered = len(observed)
	w.drive(ctx, engine, &report, w.claim(ctx, engine, &report, observed), capacity)
	return report
}

// ---------------------------------------------------------------------------
// Claiming and driving
// ---------------------------------------------------------------------------

// claim resolves the durable run for each consented issue, creating one where
// none exists. Creation goes through StartOrResumeIssueRun, which derives the
// identity from the repository, the issue and the configuration digest and
// takes it with the atomic source claim - so two watchers that discover the
// same issue converge on one run instead of racing to create two.
//
// Claiming makes no forge call and holds no capacity: it is discovery, and it
// happens whether or not there is a slot to drive anything in.
func (w *WatchController) claim(ctx context.Context, engine *EngineeringRuntime, report *RepositoryWatchReport, issues []int) []string {
	runIDs := make([]string, 0, len(issues))
	for _, issue := range issues {
		runID, live, exhausted, err := issueRun(engine, issue)
		if err != nil {
			report.note(fmt.Sprintf("issue %d: %v", issue, err))
			continue
		}
		switch {
		case live:
			runIDs = append(runIDs, runID)
		case exhausted:
			// Every generation of this issue has already terminated. Discovery
			// does not re-run finished work: starting a new generation is an
			// explicit operator action, not something a standing label repeats.
		default:
			created, err := engine.StartOrResumeIssueRun(ctx, issue)
			if err != nil {
				report.note(fmt.Sprintf("issue %d: %v", issue, err))
				continue
			}
			runIDs = append(runIDs, created)
		}
	}
	return runIDs
}

// drive hands each claimed run to the existing reconciler, in the order claim
// produced - ascending issue number, which is a stable persisted ordering.
//
// This is the whole fairness model, and it rests on what Reconcile already
// guarantees: it returns when the run reaches a stop condition, and the
// scheduler has released the run's operation before it does. A run that is
// waiting on CI, on authority, on a credential, or on an operator therefore
// holds no slot the moment it returns, and the NEXT run is driven inside the
// same tick. Nothing here holds capacity merely because a durable run exists,
// so with the M0 ceiling of one a later run is delayed, never starved.
func (w *WatchController) drive(ctx context.Context, engine *EngineeringRuntime, report *RepositoryWatchReport, runIDs []string, capacity bool) {
	if !capacity {
		return
	}
	for _, runID := range runIDs {
		if err := ctx.Err(); err != nil {
			return
		}
		outcome, err := engine.Reconcile(ctx, runID)
		if err != nil {
			report.note(runID + ": " + err.Error())
			continue
		}
		report.Driven = append(report.Driven, runID)
		if outcome.Disposition != Active && outcome.Reason != "" {
			// The wait is reported, never resolved. A run waiting on a
			// publication decision stays waiting: watch has no authority to
			// grant one, and wanting progress is not evidence of permission.
			report.note(runID + ": " + string(outcome.Disposition) + "(" + outcome.Reason + ")")
		}
	}
}

// markWaiting records a typed wait on a run that ALREADY exists. It reconciles
// nothing: no operation is planned, no provider is invoked, no candidate is
// mutated and no forge call is made. An absent run is left absent - watch never
// fabricates one to carry a scheduling fact.
func (w *WatchController) markWaiting(engine *EngineeringRuntime, report *RepositoryWatchReport, issue int, reason string) {
	state, ok := w.liveRun(engine, report, issue)
	if !ok {
		return
	}
	// Withdrawn consent is journalled BEFORE the wait, so the durable record
	// says why the run stopped being driven rather than leaving a disposition
	// whose cause has to be inferred from a report that is not persisted.
	if reason == WatchWaitingOptInRemoved && !w.appendOptIn(engine, report, state, EventSourceOptInRemoved, issue) {
		return
	}
	if err := engine.recordDisposition(state, Waiting, reason); err != nil {
		report.note(state.run.ID + ": " + err.Error())
		return
	}
	report.note(state.run.ID + ": waiting(" + reason + ")")
}

// markOptInRestored records the other half of the transition: consent came
// back. It journals the fact and nothing else - whether the run may be ACTIVE
// again is the reconciler's coherence checks to decide, so watch deliberately
// does not touch the disposition here. A run that is not waiting on withdrawn
// consent has nothing to restore.
func (w *WatchController) markOptInRestored(engine *EngineeringRuntime, report *RepositoryWatchReport, issue int) {
	state, ok := w.liveRun(engine, report, issue)
	if !ok || state.snapshot.Disposition != Waiting || state.snapshot.Reason != WatchWaitingOptInRemoved {
		return
	}
	w.appendOptIn(engine, report, state, EventSourceOptInRestored, issue)
}

// liveRun replays the run that ALREADY exists for one issue, or answers that
// there is nothing to record on. An absent run is left absent and a terminal
// run is left terminal: watch never fabricates or revives one.
func (w *WatchController) liveRun(engine *EngineeringRuntime, report *RepositoryWatchReport, issue int) (*runState, bool) {
	runID, live, _, err := issueRun(engine, issue)
	if err != nil {
		report.note(fmt.Sprintf("issue %d: %v", issue, err))
		return nil, false
	}
	if !live {
		return nil, false
	}
	state, err := engine.load(runID)
	if err != nil {
		report.note(runID + ": " + err.Error())
		return nil, false
	}
	if terminalDisposition(state.snapshot.Disposition) {
		return nil, false
	}
	return state, true
}

// appendOptIn journals one opt-in transition. The payload is identity only -
// the issue number and the operator's label - so the untrusted issue never
// reaches a durable row through the consent path either.
func (w *WatchController) appendOptIn(engine *EngineeringRuntime, report *RepositoryWatchReport, state *runState, eventType string, issue int) bool {
	if err := engine.append(state, eventType, "", SourceOptInChangedPayload{Issue: issue, Label: w.deps.Settings.Label}, nil); err != nil {
		report.note(state.run.ID + ": " + err.Error())
		return false
	}
	return true
}

// issueRun resolves the durable run for one issue WITHOUT creating anything. It
// probes the same deterministic identity space StartOrResumeIssueRun does and
// answers one of three things: a live run to resume, that every generation of
// this issue has terminated, or that this issue has no run at all.
func issueRun(engine *EngineeringRuntime, issue int) (runID string, live bool, exhausted bool, err error) {
	goal := issueGoal(engine.deps.Repository.Identity, issue)
	for generation := 0; generation < maxRunGenerations; generation++ {
		id, err := issueRunID(engine.deps.Repository.Identity, issue, engine.deps.ConfigDigest, generation)
		if err != nil {
			return "", false, false, err
		}
		run, ok, err := engine.deps.Store.Run(id)
		if err != nil {
			return "", false, false, err
		}
		if !ok {
			return "", false, generation > 0, nil
		}
		if run.Repository != engine.deps.Repository.Identity || run.Goal != goal {
			return "", false, false, &RunConflictError{RunID: id, Detail: "durable run describes different work"}
		}
		snapshot, err := engine.deps.Store.Replay(id)
		if err != nil {
			return "", false, false, err
		}
		if !terminalDisposition(snapshot.Disposition) {
			return id, true, false, nil
		}
	}
	return "", false, true, nil
}

// ---------------------------------------------------------------------------
// Durable observation
// ---------------------------------------------------------------------------

func (w *WatchController) recordSuccess(report *RepositoryWatchReport, state WatchState, revision int64, now time.Time, result DiscoveryResult, observed []int) {
	state.Repository = report.Repository.String()
	state.LastSuccessAt = now
	state.ConsecutiveFailures = 0
	state.LastErrorClass, state.LastErrorDetail, state.LastErrorAt = WatchErrorNone, "", time.Time{}
	state.NotBefore = now.Add(w.deps.Settings.PollInterval)
	// The ETag is replaced on every complete view - including with the empty
	// string a multi-page read hands back, which is the adapter refusing to let
	// a cursor stand for a partially observed set. On a 304 the stored cursor
	// is kept unless the forge restated one.
	if !result.NotModified || result.ETag != "" {
		state.ETag = result.ETag
	}
	if !result.NotModified {
		state.Cursor = formatWatchedIssues(observed)
	}
	if reported(result.RateLimit) {
		state.RateLimit = WatchRateLimit{Remaining: result.RateLimit.Remaining, ResetAt: result.RateLimit.ResetAt, ObservedAt: now}
	}
	report.LastPollAt, report.NextEligibleAt = now, state.NotBefore
	report.ErrorClass, report.Detail, report.RateLimit = WatchErrorNone, "", result.RateLimit
	w.put(report, state, revision)
}

func (w *WatchController) recordFailure(report *RepositoryWatchReport, state WatchState, revision int64, now time.Time, class WatchErrorClass, detail string, rate RateLimitObservation) {
	state.Repository = report.Repository.String()
	state.ConsecutiveFailures++
	state.LastErrorClass, state.LastErrorDetail, state.LastErrorAt = class, watchDetail(detail), now
	state.NotBefore = now.Add(w.backoff(state.ConsecutiveFailures, rate, now))
	if reported(rate) {
		state.RateLimit = WatchRateLimit{Remaining: rate.Remaining, ResetAt: rate.ResetAt, ObservedAt: now}
	}
	report.ErrorClass, report.Detail = class, state.LastErrorDetail
	report.NextEligibleAt, report.RateLimit = state.NotBefore, rate
	w.put(report, state, revision)
}

// put is the compare-and-set write. A lost race means another watcher recorded
// its own observation of this repository first; this one is dropped rather than
// re-read and re-applied, because overwriting the winner is the one outcome the
// CAS exists to prevent.
func (w *WatchController) put(report *RepositoryWatchReport, state WatchState, revision int64) {
	if _, _, err := w.deps.Store.PutWatchState(state, revision); err != nil {
		report.note("watch state: " + err.Error())
	}
}

// backoff is how long this repository must be left alone. The forge's own
// instruction wins whenever it asked for LONGER: Retry-After first, then the
// rate-limit reset. Otherwise it is bounded exponential growth from the
// configured poll interval, which is also the floor - so a one-second
// Retry-After can never turn into a busy retry loop. Nothing sleeps: the
// instant is persisted and the CLI decides when to tick again.
func (w *WatchController) backoff(failures int, rate RateLimitObservation, now time.Time) time.Duration {
	wait := w.deps.Settings.PollInterval
	for i := 1; i < failures && wait < watchMaxBackoff; i++ {
		wait *= 2
	}
	if wait > watchMaxBackoff {
		wait = watchMaxBackoff
	}
	if rate.RetryAfter > wait {
		wait = rate.RetryAfter
	}
	if reset := rate.ResetAt; !reset.IsZero() && reset.After(now.Add(wait)) {
		wait = reset.Sub(now)
	}
	return wait
}

// ---------------------------------------------------------------------------
// Classification and the watched set
// ---------------------------------------------------------------------------

// classifyWatchError routes a discovery failure into the watch vocabulary. The
// three typed forge errors are deliberately routed differently: a credential
// needs an operator, a rate limit needs the forge to be left alone until reset,
// and a 5xx needs nothing but bounded backoff. Anything unrecognized is treated
// as transient, which is the recoverable reading.
func classifyWatchError(err error) (WatchErrorClass, RateLimitObservation) {
	var auth *GitHubAuthError
	if errors.As(err, &auth) {
		return WatchErrorAuth, RateLimitObservation{}
	}
	var transient *GitHubTransientError
	if errors.As(err, &transient) {
		rate := transient.RateLimit
		if transient.Status == 429 || rate.RetryAfter > 0 || !rate.ResetAt.IsZero() {
			return WatchErrorRateLimited, rate
		}
		return WatchErrorTransient, rate
	}
	var api *GitHubAPIError
	if errors.As(err, &api) && api.Status >= 400 && api.Status < 500 {
		// Removed, renamed, or never visible to this credential: it will not
		// start answering by itself.
		return WatchErrorPermanent, RateLimitObservation{}
	}
	return WatchErrorTransient, RateLimitObservation{}
}

// contactable answers whether this repository's runs may be driven at all right
// now. A credential the operator must fix, a forge asking to be left alone, and
// a repository that is gone are all reasons not to: every run operation would
// only hit the same wall. A transient failure is not one of them - a single bad
// discovery response must never freeze runs that are making progress, for the
// same reason a 304 must not.
func contactable(class WatchErrorClass) bool {
	return class == WatchErrorNone || class == WatchErrorTransient
}

// reported distinguishes "the forge said nothing about the budget" from a
// genuine reading, without comparing time values for equality.
func reported(rate RateLimitObservation) bool {
	return rate.Remaining > 0 || rate.RetryAfter > 0 || !rate.ResetAt.IsZero()
}

// optedIn is the consent filter. The adapter already promises a complete,
// label-checked set; re-checking here means an adapter that widened its filter
// still cannot enrol an issue nobody opted in.
func optedIn(result DiscoveryResult, label string) []int {
	seen := map[int]bool{}
	issues := make([]int, 0, len(result.Issues))
	for _, issue := range result.Issues {
		if issue.Number <= 0 || seen[issue.Number] || !hasLabel(issue.Labels, label) {
			continue
		}
		seen[issue.Number] = true
		issues = append(issues, issue.Number)
	}
	sort.Ints(issues)
	return issues
}

// The watched set is persisted in WatchState.Cursor: the opted-in issue numbers
// the last complete observation returned, ascending. It is an OBSERVATION -
// what the last poll saw - never an enrolment and never a credential.
//
// ponytail: the whole set, comma separated. A repository that opts thousands of
// issues in writes a large row; store a bounded window if that ever happens.
func formatWatchedIssues(issues []int) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, strconv.Itoa(issue))
	}
	return strings.Join(parts, ",")
}

func parseWatchedIssues(cursor string) []int {
	var issues []int
	for _, field := range strings.Split(cursor, ",") {
		number, err := strconv.Atoi(strings.TrimSpace(field))
		if err == nil && number > 0 {
			issues = append(issues, number)
		}
	}
	sort.Ints(issues)
	return issues
}

func containsInt(values []int, want int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
