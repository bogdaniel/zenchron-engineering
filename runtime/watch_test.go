package runtime

// Phase 9 §9-§13: the watch scheduling and discovery cycle.
//
// Everything here runs offline against the fixture reconciler_test.go already
// establishes: a real git origin, the fake forge, the fake provider and
// verifier, a temp state directory and an INJECTED clock. There is no network,
// no real GitHub, and no sleep anywhere - every deadline in these tests is a
// clock the test moves by hand.
//
// Assertions are against durable state wherever a durable answer exists: the
// runs table, the watch_state row, and the recorded forge calls. The report is
// checked as the operator-facing summary of that state, not as a substitute
// for it.

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	watchPollInterval = 60 * time.Second
	watchSecondIssue  = 77
	watchRepoB        = "acme/other"
)

var (
	repoA = GitHubRepo{Owner: "acme", Name: "repo"}
	repoB = GitHubRepo{Owner: "acme", Name: "other"}
)

// newWatchFixture is the phase 8 fixture with a frozen clock. Freezing it is
// what makes a not-before assertion exact: the only thing that moves time in
// these tests is an explicit advance.
func newWatchFixture(t *testing.T) *phase8Fixture {
	t.Helper()
	fixture := newPhase8Fixture(t)
	fixture.clock.step = 0
	return fixture
}

// optIn puts the operator's consent label on an issue in the forge double.
func optIn(forge *FakeGitHubAdapter, number int, updated time.Time) {
	forge.Issues[number] = GitHubIssue{
		Number:    number,
		URL:       fmt.Sprintf("https://github.com/acme/repo/issues/%d", number),
		Title:     UntrustedText(fmt.Sprintf("issue %d", number)),
		Body:      "do the thing",
		Labels:    []UntrustedText{"bug", UntrustedText(DefaultWatchLabel)},
		State:     GitHubOpen,
		UpdatedAt: updated,
		Author:    GitHubActor{Login: "operator", ID: 7},
	}
}

func optOut(forge *FakeGitHubAdapter, number int) {
	issue := forge.Issues[number]
	issue.Labels = []UntrustedText{"bug"}
	forge.Issues[number] = issue
}

// watchOver builds the controller. The watch owner is the runtime's own
// scheduler owner, which is what the CLI does: one process, one identity.
func watchOver(t *testing.T, fixture *phase8Fixture, repos []GitHubRepo, engines map[string]*EngineeringRuntime, github GitHubAdapter, asked *[]string) *WatchController {
	t.Helper()
	controller, err := NewWatchController(WatchDependencies{
		Store:    fixture.store,
		Clock:    fixture.clock,
		Owner:    fixture.deps.Owner,
		Liveness: OwnerLivenessFunc(func(string) bool { return true }),
		GitHub:   github,
		Settings: WatchSettings{
			Repositories:      repos,
			Label:             DefaultWatchLabel,
			PollInterval:      watchPollInterval,
			MaxConcurrentRuns: 1,
		},
		Runtime: func(repo GitHubRepo) (*EngineeringRuntime, error) {
			if asked != nil {
				*asked = append(*asked, repo.String())
			}
			engine, ok := engines[repo.String()]
			if !ok {
				return nil, fmt.Errorf("no runtime is configured for %s", repo)
			}
			return engine, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return controller
}

// watchOne is the ordinary single-repository controller.
func watchOne(t *testing.T, fixture *phase8Fixture) *WatchController {
	t.Helper()
	return watchOver(t, fixture, []GitHubRepo{repoA},
		map[string]*EngineeringRuntime{repoA.String(): fixture.runtime}, fixture.forge, nil)
}

func tick(t *testing.T, controller *WatchController) TickReport {
	t.Helper()
	report, err := controller.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}
	return report
}

// only is the single repository report a one-repository tick must produce.
func only(t *testing.T, report TickReport) RepositoryWatchReport {
	t.Helper()
	if len(report.Repositories) != 1 {
		t.Fatalf("tick reported %d repositories, want exactly one", len(report.Repositories))
	}
	return report.Repositories[0]
}

// runIDFor derives the first-generation run identity independently of watch,
// from the frozen Phase 8 derivation, so "did watch create a run" is answered
// by the durable table rather than by watch's own answer.
func runIDFor(t *testing.T, engine *EngineeringRuntime, issue int) string {
	t.Helper()
	id, err := issueRunID(engine.deps.Repository.Identity, issue, engine.deps.ConfigDigest, 0)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func storedRun(t *testing.T, fixture *phase8Fixture, runID string) (EngineeringRun, bool) {
	t.Helper()
	run, ok, err := fixture.store.Run(runID)
	if err != nil {
		t.Fatal(err)
	}
	return run, ok
}

func watchStateOf(t *testing.T, fixture *phase8Fixture, repo GitHubRepo) (WatchState, bool) {
	t.Helper()
	state, _, ok, err := fixture.store.WatchStateFor(repo.String())
	if err != nil {
		t.Fatal(err)
	}
	return state, ok
}

// occupyGlobalSlot makes another live owner hold the single run-driving slot.
// It is how a test isolates discovery from driving, and it is also the durable
// shape the run ceiling is defined against.
func occupyGlobalSlot(t *testing.T, fixture *phase8Fixture) {
	t.Helper()
	at := fixture.clock.at
	_, ok, err := fixture.store.PutOperation(RunOperation{
		SchemaVersion: SchemaVersion, ID: "op-foreign", RunID: "run-foreign",
		Kind: "external.work", IdempotencyKey: "foreign", State: Leased,
		Attempt: 1, MaxAttempts: 1, CreatedAt: at,
		Lease: &Lease{Owner: "other-owner", HeartbeatAt: at, ExpiresAt: at.Add(time.Minute)},
	}, 0)
	if err != nil || !ok {
		t.Fatalf("seeding the foreign lease: ok=%v err=%v", ok, err)
	}
}

func releaseGlobalSlot(t *testing.T, fixture *phase8Fixture) {
	t.Helper()
	op, revision, ok, err := fixture.store.Operation("op-foreign")
	if err != nil || !ok {
		t.Fatalf("reading the foreign lease: ok=%v err=%v", ok, err)
	}
	op.State, op.Lease = Succeeded, nil
	if _, written, err := fixture.store.PutOperation(op, revision); err != nil || !written {
		t.Fatalf("releasing the foreign lease: written=%v err=%v", written, err)
	}
}

func containsRun(driven []string, runID string) bool { return containsString(driven, runID) }

// ---------------------------------------------------------------------------
// Discovery and enrolment
// ---------------------------------------------------------------------------

func TestWatchClaimsAnOptedInIssueAndDrivesItThroughTheReconciler(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())

	report := tick(t, watchOne(t, fixture))
	observed := only(t, report)
	if observed.Discovered != 1 {
		t.Fatalf("discovered %d opted-in issues, want 1 (%q)", observed.Discovered, observed.Detail)
	}
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	run, ok := storedRun(t, fixture, runID)
	if !ok {
		t.Fatal("the opted-in issue produced no durable run")
	}
	if run.Goal != issueGoal("acme/repo", fixture.issue) {
		t.Fatalf("run answers %q", run.Goal)
	}
	if !containsRun(observed.Driven, runID) {
		t.Fatalf("driven runs are %v, want %s (%q)", observed.Driven, runID, observed.Detail)
	}
	if terminalDisposition(run.Disposition) && run.Disposition != Completed {
		t.Fatalf("the driven run settled %s: %s", run.Disposition, run.Reason)
	}
	// Watch drove the run through the existing reconciler: the journal is the
	// reconciler's, not a second one's.
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventContractCompiled) == 0 {
		t.Fatalf("the run was not reconciled: %v", journalTypes(events))
	}
	if observed.NextEligibleAt != fixture.clock.at.Add(watchPollInterval) {
		t.Fatalf("next eligible at %s, want one poll interval after %s", observed.NextEligibleAt, fixture.clock.at)
	}
}

func TestWatchIgnoresAnIssueWithoutTheOptInLabel(t *testing.T) {
	fixture := newWatchFixture(t)
	// The fixture issue carries "bug" only: no operator consent, no source.
	report := tick(t, watchOne(t, fixture))
	observed := only(t, report)
	if observed.Discovered != 0 || len(observed.Driven) != 0 {
		t.Fatalf("an unlabelled issue was picked up: %+v", observed)
	}
	if _, ok := storedRun(t, fixture, runIDFor(t, fixture.runtime, fixture.issue)); ok {
		t.Fatal("an unlabelled issue produced a durable run")
	}
	if _, ok := watchStateOf(t, fixture, repoA); !ok {
		t.Fatal("a successful poll recorded no watch state")
	}
}

func TestWatchNeverPollsAnUnregisteredRepository(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	forgeOther := NewFakeGitHubAdapter()
	optIn(forgeOther, watchSecondIssue, time.Unix(1_700_000_000, 0).UTC())
	engineB := fixture.siblingRuntime(t, watchRepoB, forgeOther)

	// repoB has a runtime, a forge, and an opted-in issue. It is not enrolled,
	// so none of that lets it watch itself.
	var asked []string
	occupyGlobalSlot(t, fixture)
	controller := watchOver(t, fixture, []GitHubRepo{repoA}, map[string]*EngineeringRuntime{
		repoA.String(): fixture.runtime, repoB.String(): engineB,
	}, fixture.forge, &asked)
	report := tick(t, controller)

	if len(report.Repositories) != 1 || report.Repositories[0].Repository != repoA {
		t.Fatalf("tick reported %+v, want repoA only", report.Repositories)
	}
	if containsString(asked, repoB.String()) {
		t.Fatalf("watch constructed a runtime for an unregistered repository: %v", asked)
	}
	if len(forgeOther.Calls) != 0 {
		t.Fatalf("an unregistered repository was contacted: %v", forgeOther.Methods())
	}
	if _, ok := storedRun(t, fixture, runIDFor(t, engineB, watchSecondIssue)); ok {
		t.Fatal("an unregistered repository enrolled itself")
	}
	if _, ok := watchStateOf(t, fixture, repoB); ok {
		t.Fatal("an unregistered repository recorded watch state")
	}
}

func TestWatchDiscoveryIsIdempotent(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)
	controller := watchOne(t, fixture)

	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	if _, ok := storedRun(t, fixture, runID); !ok {
		t.Fatal("the first tick claimed no run")
	}
	fixture.clock.advance(2 * watchPollInterval)
	second := only(t, tick(t, controller))
	if second.Discovered != 1 {
		t.Fatalf("the second tick discovered %d issues, want 1", second.Discovered)
	}
	// One source, one run: the identity is derived, so re-discovery cannot
	// produce a second EngineeringRun for the same issue.
	for generation := 1; generation < 3; generation++ {
		id, err := issueRunID("acme/repo", fixture.issue, fixture.runtime.deps.ConfigDigest, generation)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := storedRun(t, fixture, id); ok {
			t.Fatalf("re-discovery created a second run at generation %d", generation)
		}
	}
}

func TestWatchPaginationDoesNotLoseOptedInIssues(t *testing.T) {
	fixture := newWatchFixture(t)
	updated := time.Unix(1_700_000_000, 0).UTC()
	optIn(fixture.forge, 11, updated)
	optIn(fixture.forge, 12, updated)
	optIn(fixture.forge, 13, updated)
	// A multi-page observation: complete, and deliberately without a cursor.
	fixture.forge.Discoveries = []DiscoveryResult{{
		Issues: []GitHubIssue{fixture.forge.Issues[11], fixture.forge.Issues[12], fixture.forge.Issues[13]},
		Pages:  2,
	}}
	occupyGlobalSlot(t, fixture)

	observed := only(t, tick(t, watchOne(t, fixture)))
	if observed.Discovered != 3 {
		t.Fatalf("discovered %d of 3 paginated issues (%q)", observed.Discovered, observed.Detail)
	}
	for _, issue := range []int{11, 12, 13} {
		if _, ok := storedRun(t, fixture, runIDFor(t, fixture.runtime, issue)); !ok {
			t.Fatalf("issue %d was lost across pages", issue)
		}
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.ETag != "" {
		t.Fatalf("a multi-page observation persisted the cursor %q", state.ETag)
	}
	if state.Cursor != "11,12,13" {
		t.Fatalf("watched set is %q, want the complete paginated set", state.Cursor)
	}
}

// ---------------------------------------------------------------------------
// Conditional discovery
// ---------------------------------------------------------------------------

func TestWatchReusesTheETagAndA304DoesNotStopReconciliation(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	fixture.forge.Discoveries = []DiscoveryResult{{
		ETag: "etag-1", Issues: []GitHubIssue{fixture.forge.Issues[fixture.issue]},
	}}
	controller := watchOne(t, fixture)

	tick(t, controller)
	state, _ := watchStateOf(t, fixture, repoA)
	if state.ETag != "etag-1" {
		t.Fatalf("the discovery cursor was not persisted: %+v", state)
	}
	runID := runIDFor(t, fixture.runtime, fixture.issue)

	fixture.clock.advance(2 * watchPollInterval)
	second := only(t, tick(t, controller))

	replayed := false
	for _, call := range fixture.forge.Calls {
		if call.Method == "DiscoverIssues" && call.ETag == "etag-1" {
			replayed = true
		}
	}
	if !replayed {
		t.Fatalf("the stored ETag was never replayed: %v", fixture.forge.Calls)
	}
	// The 304 says "nothing changed about the opted-in SET". It says nothing
	// about the pull request, the checks, or the reviews of a run already
	// under way, so reconciliation proceeds regardless.
	if !containsRun(second.Driven, runID) {
		t.Fatalf("a 304 stopped reconciliation of an active run: %+v", second)
	}
	if second.Discovered != 1 {
		t.Fatalf("a 304 lost the remembered opted-in set: %d", second.Discovered)
	}
}

// ---------------------------------------------------------------------------
// Backoff
// ---------------------------------------------------------------------------

func TestWatchRespectsRetryAfter(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubTransientError{Status: 429, Detail: "secondary rate limit",
				RateLimit: RateLimitObservation{RetryAfter: 5 * time.Minute, Secondary: true}}
		}
		return nil
	})
	controller := watchOne(t, fixture)
	start := fixture.clock.at

	observed := only(t, tick(t, controller))
	if observed.ErrorClass != WatchErrorRateLimited {
		t.Fatalf("error class is %q, want rate_limited", observed.ErrorClass)
	}
	if observed.NextEligibleAt != start.Add(5*time.Minute) {
		t.Fatalf("next eligible at %s, want the Retry-After instant %s", observed.NextEligibleAt, start.Add(5*time.Minute))
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.NotBefore != start.Add(5*time.Minute) {
		t.Fatalf("the not-before was not persisted: %+v", state)
	}
	// Inside the window the forge is not contacted again.
	polls := countMethod(fixture.forge.Calls, "DiscoverIssues")
	fixture.clock.advance(4 * time.Minute)
	skipped := only(t, tick(t, controller))
	if got := countMethod(fixture.forge.Calls, "DiscoverIssues"); got != polls {
		t.Fatalf("watch polled inside the Retry-After window (%d -> %d)", polls, got)
	}
	if skipped.NextEligibleAt != start.Add(5*time.Minute) {
		t.Fatalf("the standing backoff was not reported: %s", skipped.NextEligibleAt)
	}
	// And it is honoured again once it has elapsed.
	fixture.clock.advance(2 * time.Minute)
	tick(t, controller)
	if got := countMethod(fixture.forge.Calls, "DiscoverIssues"); got != polls+1 {
		t.Fatalf("watch did not resume polling after the window (%d -> %d)", polls, got)
	}
}

func TestWatchRespectsTheRateLimitReset(t *testing.T) {
	fixture := newWatchFixture(t)
	start := fixture.clock.at
	reset := start.Add(9 * time.Minute)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubTransientError{Status: 403, Detail: "rate limited",
				RateLimit: RateLimitObservation{Remaining: 0, ResetAt: reset}}
		}
		return nil
	})
	observed := only(t, tick(t, watchOne(t, fixture)))
	if observed.ErrorClass != WatchErrorRateLimited {
		t.Fatalf("error class is %q, want rate_limited", observed.ErrorClass)
	}
	if observed.NextEligibleAt != reset {
		t.Fatalf("next eligible at %s, want the reported reset %s", observed.NextEligibleAt, reset)
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.RateLimit.ResetAt != reset || state.RateLimit.ObservedAt != start {
		t.Fatalf("the rate-limit reading was not persisted: %+v", state.RateLimit)
	}
}

func TestWatchBacksOffTransientFailuresWithoutFreezingRuns(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)
	controller := watchOne(t, fixture)

	// A healthy tick claims the run; the ceiling keeps it undriven.
	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	if _, ok := storedRun(t, fixture, runID); !ok {
		t.Fatal("the healthy tick claimed no run")
	}
	releaseGlobalSlot(t, fixture)

	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubTransientError{Status: 502, Detail: "bad gateway"}
		}
		return nil
	})
	fixture.clock.advance(2 * watchPollInterval)
	first := only(t, tick(t, controller))
	if first.ErrorClass != WatchErrorTransient {
		t.Fatalf("error class is %q, want transient", first.ErrorClass)
	}
	if first.NextEligibleAt != fixture.clock.at.Add(watchPollInterval) {
		t.Fatalf("first backoff is %s, want one poll interval", first.NextEligibleAt.Sub(fixture.clock.at))
	}
	// A transient discovery failure must not freeze a run that already exists.
	if !containsRun(first.Driven, runID) {
		t.Fatalf("a transient discovery failure stopped run reconciliation: %+v", first)
	}

	fixture.clock.advance(2 * watchPollInterval)
	second := only(t, tick(t, controller))
	if second.NextEligibleAt != fixture.clock.at.Add(2*watchPollInterval) {
		t.Fatalf("second backoff is %s, want exponential growth", second.NextEligibleAt.Sub(fixture.clock.at))
	}
	// Growth is bounded: it never becomes "never retry".
	controller.deps.Settings.PollInterval = watchPollInterval
	if capped := controller.backoff(64, RateLimitObservation{}, fixture.clock.at); capped != watchMaxBackoff {
		t.Fatalf("backoff after 64 failures is %s, want the %s bound", capped, watchMaxBackoff)
	}
}

func TestWatchRestartPreservesTheCursorAndTheBackoff(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)
	tick(t, watchOne(t, fixture))

	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubTransientError{Status: 429, Detail: "slow down",
				RateLimit: RateLimitObservation{RetryAfter: 10 * time.Minute}}
		}
		return nil
	})
	fixture.clock.advance(2 * watchPollInterval)
	before := only(t, tick(t, watchOne(t, fixture)))

	// A restart is a new controller over the same durable state.
	restarted := watchOne(t, fixture)
	polls := countMethod(fixture.forge.Calls, "DiscoverIssues")
	fixture.clock.advance(time.Minute)
	after := only(t, tick(t, restarted))
	if got := countMethod(fixture.forge.Calls, "DiscoverIssues"); got != polls {
		t.Fatalf("a restart reset the backoff and polled (%d -> %d)", polls, got)
	}
	if after.NextEligibleAt != before.NextEligibleAt {
		t.Fatalf("the not-before did not survive the restart: %s vs %s", after.NextEligibleAt, before.NextEligibleAt)
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.Cursor != fmt.Sprint(fixture.issue) {
		t.Fatalf("the watched set did not survive the restart: %q", state.Cursor)
	}
	if after.Discovered != 1 {
		t.Fatalf("the restarted watcher lost the remembered set: %d", after.Discovered)
	}
}

// ---------------------------------------------------------------------------
// Credentials
// ---------------------------------------------------------------------------

func TestWatchAuthExpiryCreatesNoRunAndBacksOff(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubAuthError{Detail: "the local GitHub CLI is not authenticated"}
		}
		return nil
	})
	start := fixture.clock.at
	observed := only(t, tick(t, watchOne(t, fixture)))

	if observed.ErrorClass != WatchErrorAuth {
		t.Fatalf("error class is %q, want auth", observed.ErrorClass)
	}
	if observed.NextEligibleAt != start.Add(watchPollInterval) {
		t.Fatalf("auth failure did not back off: %s", observed.NextEligibleAt)
	}
	if _, ok := storedRun(t, fixture, runIDFor(t, fixture.runtime, fixture.issue)); ok {
		t.Fatal("watch fabricated an EngineeringRun to carry its own auth state")
	}
	if len(fixture.provider.requests) != 0 {
		t.Fatal("an auth failure reached the execution provider")
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.LastErrorClass != WatchErrorAuth || state.Cursor != "" {
		t.Fatalf("watch state after an auth failure: %+v", state)
	}
	// The diagnostic is an observation. No credential material is in it.
	if state.LastErrorDetail == "" {
		t.Fatal("the auth failure recorded no operator-facing diagnostic")
	}
}

func TestWatchAuthExpiryWaitsAnExistingRunAndResumesOnRestoration(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)
	controller := watchOne(t, fixture)
	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	if _, ok := storedRun(t, fixture, runID); !ok {
		t.Fatal("the healthy tick claimed no run")
	}
	releaseGlobalSlot(t, fixture)

	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubAuthError{Detail: "credential resolution failed"}
		}
		return nil
	})
	calls := len(fixture.forge.Calls)
	fixture.clock.advance(2 * watchPollInterval)
	expired := only(t, tick(t, controller))

	run, _ := storedRun(t, fixture, runID)
	if run.Disposition != Waiting || run.Reason != WatchWaitingGitHubAuth {
		t.Fatalf("the existing run settled %s/%s, want waiting(%s)", run.Disposition, run.Reason, WatchWaitingGitHubAuth)
	}
	if len(expired.Driven) != 0 {
		t.Fatalf("an auth-expired repository still drove runs: %v", expired.Driven)
	}
	if len(fixture.provider.requests) != 0 {
		t.Fatal("an auth-expired run reached the execution provider")
	}
	if got := len(fixture.forge.Calls) - calls; got != 1 {
		t.Fatalf("an auth-expired tick made %d forge calls, want the one refused discovery", got)
	}

	// The credential comes back. Discovery resumes and the coherent waiting run
	// resumes through ordinary reconciliation - no operator surgery.
	fixture.inject(func(GitHubCall) error { return nil })
	fixture.clock.advance(2 * watchPollInterval)
	restored := only(t, tick(t, controller))
	if !containsRun(restored.Driven, runID) {
		t.Fatalf("a restored credential did not resume the run: %+v", restored)
	}
	resumed, _ := storedRun(t, fixture, runID)
	if resumed.Reason == WatchWaitingGitHubAuth {
		t.Fatalf("the run is still waiting on a credential that came back: %s", resumed.Reason)
	}
}

// ---------------------------------------------------------------------------
// Consent
// ---------------------------------------------------------------------------

func TestWatchLabelRemovalWaitsAndRestorationRunsCoherenceChecks(t *testing.T) {
	fixture := newWatchFixture(t)
	updated := time.Unix(1_700_000_000, 0).UTC()
	optIn(fixture.forge, fixture.issue, updated)
	occupyGlobalSlot(t, fixture)
	controller := watchOne(t, fixture)
	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	if _, ok := storedRun(t, fixture, runID); !ok {
		t.Fatal("the opted-in issue claimed no run")
	}
	releaseGlobalSlot(t, fixture)

	optOut(fixture.forge, fixture.issue)
	fixture.clock.advance(2 * watchPollInterval)
	removed := only(t, tick(t, controller))

	run, _ := storedRun(t, fixture, runID)
	if run.Disposition != Waiting || run.Reason != WatchWaitingOptInRemoved {
		t.Fatalf("after opt-in removal the run is %s/%s, want waiting(%s)", run.Disposition, run.Reason, WatchWaitingOptInRemoved)
	}
	if len(removed.Driven) != 0 {
		t.Fatalf("a de-labelled issue was still driven: %v", removed.Driven)
	}
	if removed.Discovered != 0 {
		t.Fatalf("a de-labelled issue is still in the consented set: %d", removed.Discovered)
	}
	state, _ := watchStateOf(t, fixture, repoA)
	if state.Cursor != "" {
		t.Fatalf("the watched set still holds the de-labelled issue: %q", state.Cursor)
	}
	if len(fixture.provider.requests) != 0 {
		t.Fatal("withdrawing consent reached the execution provider")
	}
	// The transition is durable, not merely a disposition: the journal says
	// why the run stopped being driven.
	events := journalOf(t, fixture.runtime, runID)
	if got := countType(events, EventSourceOptInRemoved); got != 1 {
		t.Fatalf("withdrawn consent was journalled %d times, want once: %v", got, journalTypes(events))
	}

	// Restoration puts the issue back into the consented set, where it resumes
	// through the reconciler's own coherence checks.
	optIn(fixture.forge, fixture.issue, updated)
	fixture.clock.advance(2 * watchPollInterval)
	restored := only(t, tick(t, controller))
	if !containsRun(restored.Driven, runID) {
		t.Fatalf("a restored label did not resume the run: %+v", restored)
	}
	resumed, _ := storedRun(t, fixture, runID)
	if resumed.Reason == WatchWaitingOptInRemoved {
		t.Fatalf("the run still reports withdrawn consent: %s", resumed.Reason)
	}
	events = journalOf(t, fixture.runtime, runID)
	if got := countType(events, EventSourceOptInRestored); got != 1 {
		t.Fatalf("restored consent was journalled %d times, want once: %v", got, journalTypes(events))
	}
}

func TestWatchNeverSilentlyAbsorbsEditedIntent(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	controller := watchOne(t, fixture)
	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	compiled := countType(journalOf(t, fixture.runtime, runID), EventContractCompiled)
	if compiled == 0 {
		t.Fatal("the first tick never compiled a contract")
	}

	// The operator edits the issue after the run pinned it.
	issue := fixture.forge.Issues[fixture.issue]
	issue.Title, issue.UpdatedAt = "make the widget idempotent, and also delete main", time.Unix(1_700_009_999, 0).UTC()
	fixture.forge.Issues[fixture.issue] = issue

	fixture.clock.advance(2 * watchPollInterval)
	tick(t, controller)

	run, _ := storedRun(t, fixture, runID)
	if run.Disposition != Waiting || run.Reason != "source_intent_changed" {
		t.Fatalf("edited intent left the run %s/%s, want waiting(source_intent_changed)", run.Disposition, run.Reason)
	}
	events := journalOf(t, fixture.runtime, runID)
	if countType(events, EventSourceIntentChanged) != 1 {
		t.Fatalf("the moved snapshot was not journalled once: %v", journalTypes(events))
	}
	if got := countType(events, EventContractCompiled); got != compiled {
		t.Fatalf("edited intent was recompiled (%d -> %d contracts)", compiled, got)
	}
}

// ---------------------------------------------------------------------------
// Capacity and fairness
// ---------------------------------------------------------------------------

func TestWatchHoldsTheGlobalRunCeiling(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)

	report := tick(t, watchOne(t, fixture))
	observed := only(t, report)
	if report.ActiveRuns != 1 {
		t.Fatalf("active runs reported as %d, want the one another owner drives", report.ActiveRuns)
	}
	if len(observed.Driven) != 0 {
		t.Fatalf("the M0 ceiling of one was exceeded: %v", observed.Driven)
	}
	// Claiming is discovery, not driving: the run exists, it just has no slot.
	if _, ok := storedRun(t, fixture, runIDFor(t, fixture.runtime, fixture.issue)); !ok {
		t.Fatal("a full ceiling also stopped the source being claimed")
	}
	if len(fixture.provider.requests) != 0 {
		t.Fatal("a run was driven without a slot")
	}
}

func TestWatchYieldsTheGlobalSlotToTheNextRun(t *testing.T) {
	fixture := newWatchFixture(t)
	updated := time.Unix(1_700_000_000, 0).UTC()
	optIn(fixture.forge, fixture.issue, updated)
	optIn(fixture.forge, watchSecondIssue, updated)

	observed := only(t, tick(t, watchOne(t, fixture)))
	first := runIDFor(t, fixture.runtime, fixture.issue)
	second := runIDFor(t, fixture.runtime, watchSecondIssue)
	if !containsRun(observed.Driven, first) || !containsRun(observed.Driven, second) {
		t.Fatalf("driven runs are %v, want both %s and %s (%q)", observed.Driven, first, second, observed.Detail)
	}
	// Ordering is deterministic: ascending issue number, which is a stable
	// persisted ordering rather than a map iteration.
	if observed.Driven[0] != first {
		t.Fatalf("driving order is %v, want the lower issue number first", observed.Driven)
	}
	// Each run reached a stop condition and released the slot before the next
	// one was driven: no run holds capacity merely by existing.
	for _, runID := range []string{first, second} {
		run, _ := storedRun(t, fixture, runID)
		if run.Disposition == Active {
			t.Fatalf("run %s still holds the slot as %s", runID, run.Disposition)
		}
	}
}

// TestWatchReportsAnAuthorityWaitWithoutGrantingIt is §13: a run that needs a
// publication decision policy did not authorize stays waiting. Watch reports it
// as an actionable diagnostic and does nothing else - wanting progress is not
// evidence of permission, and discovering the need later is a privilege
// EXPANSION that #8 refuses on purpose.
func TestWatchReportsAnAuthorityWaitWithoutGrantingIt(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	occupyGlobalSlot(t, fixture)
	controller := watchOne(t, fixture)
	tick(t, controller)
	runID := runIDFor(t, fixture.runtime, fixture.issue)
	releaseGlobalSlot(t, fixture)

	// The publication action is awaiting an authority the runtime cannot give
	// itself.
	if err := fixture.runtime.append(fixture.state(runID), EventAuthorityEvaluated, "", AuthorityEvaluatedPayload{
		Decision: Ref{ID: "decision-1", Revision: "1"},
		Action:   domain.Action{Type: PublicationActionType, Target: fixture.branch},
		Status:   domain.AuthorityAwaitingAuthority,
	}, nil); err != nil {
		t.Fatal(err)
	}

	fixture.clock.advance(2 * watchPollInterval)
	observed := only(t, tick(t, controller))

	run, _ := storedRun(t, fixture, runID)
	if run.Disposition != Waiting || run.Reason != "awaiting_authority" {
		t.Fatalf("the run settled %s/%s, want waiting(awaiting_authority)", run.Disposition, run.Reason)
	}
	if !strings.Contains(observed.Detail, "awaiting_authority") {
		t.Fatalf("the authority wait was not surfaced as a diagnostic: %q", observed.Detail)
	}
	if countMethod(fixture.forge.Calls, "CreatePullRequest") != 0 || len(fixture.provider.requests) != 0 {
		t.Fatal("watch published or produced while the publication authority was pending")
	}
	// The decision is exactly the one policy recorded. Watch upgraded nothing.
	decision, ok := fixture.state(runID).publicationDecision()
	if !ok || decision.Status != domain.AuthorityAwaitingAuthority {
		t.Fatalf("the publication decision is now %+v", decision)
	}
}

// ---------------------------------------------------------------------------
// Isolation between repositories
// ---------------------------------------------------------------------------

func TestOneRepositoryFailureDoesNotBlockAnother(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "DiscoverIssues" {
			return &GitHubAuthError{Detail: "credential resolution failed"}
		}
		return nil
	})
	forgeOther := NewFakeGitHubAdapter()
	optIn(forgeOther, watchSecondIssue, time.Unix(1_700_000_000, 0).UTC())
	engineB := fixture.siblingRuntime(t, watchRepoB, forgeOther)
	occupyGlobalSlot(t, fixture)

	controller := watchOver(t, fixture, []GitHubRepo{repoA, repoB}, map[string]*EngineeringRuntime{
		repoA.String(): fixture.runtime, repoB.String(): engineB,
	}, routingForge{repoA: fixture.forge, repoB: forgeOther}, nil)
	report := tick(t, controller)

	if len(report.Repositories) != 2 {
		t.Fatalf("tick reported %d repositories, want both", len(report.Repositories))
	}
	if report.Repositories[0].ErrorClass != WatchErrorAuth {
		t.Fatalf("the failing repository reported %q", report.Repositories[0].ErrorClass)
	}
	healthy := report.Repositories[1]
	if healthy.ErrorClass != WatchErrorNone || healthy.Discovered != 1 {
		t.Fatalf("the healthy repository was blocked by its neighbour: %+v", healthy)
	}
	if _, ok := storedRun(t, fixture, runIDFor(t, engineB, watchSecondIssue)); !ok {
		t.Fatal("the healthy repository claimed no run")
	}
	// Repository order is configuration order, so a report is comparable
	// between ticks.
	if report.Repositories[0].Repository != repoA || healthy.Repository != repoB {
		t.Fatalf("repositories reported out of configuration order: %+v", report.Repositories)
	}
}

// routingForge dispatches discovery to the per-repository double, which is what
// a real multi-repository watcher does with one adapter.
type routingForge map[GitHubRepo]*FakeGitHubAdapter

func (r routingForge) adapter(repo GitHubRepo) GitHubAdapter { return r[repo] }

func (r routingForge) Issue(ctx context.Context, repo GitHubRepo, number int) (GitHubIssue, error) {
	return r.adapter(repo).Issue(ctx, repo, number)
}
func (r routingForge) DiscoverIssues(ctx context.Context, query DiscoveryQuery) (DiscoveryResult, error) {
	return r.adapter(query.Repo).DiscoverIssues(ctx, query)
}
func (r routingForge) FindPullRequests(ctx context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error) {
	return r.adapter(repo).FindPullRequests(ctx, repo, headRef, baseRef)
}
func (r routingForge) CreatePullRequest(ctx context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error) {
	return r.adapter(repo).CreatePullRequest(ctx, repo, request)
}
func (r routingForge) UpdatePullRequest(ctx context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error) {
	return r.adapter(repo).UpdatePullRequest(ctx, repo, number, update)
}
func (r routingForge) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	return r.adapter(repo).PullRequest(ctx, repo, number)
}
func (r routingForge) Checks(ctx context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error) {
	return r.adapter(repo).Checks(ctx, repo, headSHA)
}
func (r routingForge) Reviews(ctx context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error) {
	return r.adapter(repo).Reviews(ctx, repo, number, headSHA)
}
func (r routingForge) CommentOnPullRequest(ctx context.Context, repo GitHubRepo, number int, body Publication) error {
	return r.adapter(repo).CommentOnPullRequest(ctx, repo, number, body)
}
func (r routingForge) RefSHA(ctx context.Context, repo GitHubRepo, ref string) (RefObservation, error) {
	return r.adapter(repo).RefSHA(ctx, repo, ref)
}

var _ GitHubAdapter = routingForge{}

// siblingRuntime is a second runtime over the SAME durable store, bound to a
// different repository identity and configuration digest.
func (f *phase8Fixture) siblingRuntime(t *testing.T, identity string, forge GitHubAdapter) *EngineeringRuntime {
	t.Helper()
	deps := f.deps
	deps.GitHub = forge
	deps.Repository = RepositoryTarget{Identity: identity, Remote: f.origin, DefaultBranch: f.branch}
	deps.ConfigDigest = ConfigDigest{Global: "g1", Repository: identity}
	return f.newRuntime(deps)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNewWatchControllerRefusesAnUnusableRegistration(t *testing.T) {
	fixture := newWatchFixture(t)
	base := WatchDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1", GitHub: fixture.forge,
		Settings: WatchSettings{Repositories: []GitHubRepo{repoA}, PollInterval: watchPollInterval},
		Runtime:  func(GitHubRepo) (*EngineeringRuntime, error) { return fixture.runtime, nil },
	}
	for name, mutate := range map[string]func(*WatchDependencies){
		"no store":            func(d *WatchDependencies) { d.Store = nil },
		"no forge":            func(d *WatchDependencies) { d.GitHub = nil },
		"no runtime factory":  func(d *WatchDependencies) { d.Runtime = nil },
		"no owner":            func(d *WatchDependencies) { d.Owner = " " },
		"polls too often":     func(d *WatchDependencies) { d.Settings.PollInterval = time.Second },
		"duplicate enrolment": func(d *WatchDependencies) { d.Settings.Repositories = []GitHubRepo{repoA, repoA} },
		"ungoverned identity": func(d *WatchDependencies) { d.Settings.Repositories = []GitHubRepo{{Owner: "acme"}} },
	} {
		t.Run(name, func(t *testing.T) {
			deps := base
			mutate(&deps)
			if _, err := NewWatchController(deps); err == nil {
				t.Fatal("an unusable watch registration was accepted")
			}
		})
	}
	// The label defaults rather than watching everything.
	controller, err := NewWatchController(base)
	if err != nil {
		t.Fatal(err)
	}
	if controller.deps.Settings.Label != DefaultWatchLabel {
		t.Fatalf("effective label is %q", controller.deps.Settings.Label)
	}
	if controller.deps.Settings.MaxConcurrentRuns != 1 {
		t.Fatalf("the M0 ceiling defaulted to %d", controller.deps.Settings.MaxConcurrentRuns)
	}
}
