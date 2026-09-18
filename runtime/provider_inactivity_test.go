package runtime

// #238: a provider subprocess being alive is not evidence of progress.
//
// The live run this file exists for is
// run-07b3a0390329eb86beb9bdf3d0ed6342: a laptop lost its network, the Codex
// process stayed alive and silent, and Zenchron charged 8h55m16s to its
// active-work budget before the outer run wall budget - the safety ceiling, not
// the stall detector - terminalized it. External wait was 0s for the whole
// period.
//
// The tests here are the deterministic half: classification, routing,
// accounting and the no-progress arithmetic under an injected clock. The half
// that needs a real process group lives in provider_inactivity_process_test.go.

import (
	"context"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// D. An explicit connectivity diagnostic is a typed bounded condition
// ---------------------------------------------------------------------------

// TestRecognizedConnectivityDiagnosticsRouteToABoundedWait is acceptance 8.
//
// The distinction it pins is the whole point of the classification half of
// #238: an explicit transport diagnostic is provider_unavailable, and SILENCE
// is not. Inferring "offline" from a quiet provider is how a model that is
// thinking becomes a permanent wait.
func TestRecognizedConnectivityDiagnosticsRouteToABoundedWait(t *testing.T) {
	for name, tc := range map[string]struct {
		spec       cliAgentSpec
		diagnostic string
	}{
		// Codex is a Rust binary over reqwest/hyper.
		"codex dns":            {codexSpec, "stream error: error sending request for url (https://chatgpt.com/backend-api/codex/responses): dns error: failed to lookup address information: nodename nor servname provided"},
		"codex refused":        {codexSpec, "error sending request for url: tcp connect error: Connection refused (os error 61)"},
		"codex reset":          {codexSpec, "request failed: connection reset by peer (os error 54)"},
		"codex unreachable":    {codexSpec, "error sending request: network is unreachable (os error 51)"},
		"claude enotfound":     {claudeSpec, "API Error: request to https://api.anthropic.com/v1/messages failed, reason: getaddrinfo ENOTFOUND api.anthropic.com"},
		"claude econnrefused":  {claudeSpec, "TypeError: fetch failed\n  cause: Error: connect ECONNREFUSED 127.0.0.1:443"},
		"claude enetunreach":   {claudeSpec, "connect ENETUNREACH 2606:4700::6810:84e5:443"},
		"claude dns temporary": {claudeSpec, "getaddrinfo EAI_AGAIN api.anthropic.com"},
		"gemini econnreset":    {geminiSpec, "FetchError: read ECONNRESET"},
		"qwen ehostunreach":    {qwenSpec, "connect EHOSTUNREACH 140.82.121.5:443"},
		// HTTP's own vocabulary, shared because it is not any vendor's.
		"gateway": {codexSpec, "unexpected status 503 Service Unavailable"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyAgentFailure(tc.spec, []byte(tc.diagnostic), nil); got != FailureProviderUnavailable {
				t.Fatalf("classified as %q, want %q - an unclassified connectivity failure is what became hours of apparent active work", got, FailureProviderUnavailable)
			}
		})
	}

	// It is a BOUNDED EXTERNAL WAIT under the existing #83/#229/#232
	// accounting, not a new mechanism: the route, the reason and the
	// active-work exclusion all come from machinery that already existed.
	if route := RouteFailure(FailureProviderUnavailable); route != RouteWait {
		t.Fatalf("provider_unavailable routes to %q, want a bounded wait", route)
	}
	reason := waitReason(FailureProviderUnavailable)
	if reason != "execution_provider_unavailable" {
		t.Fatalf("wait reason = %q", reason)
	}
	// A host with no network is not performing engineering work. Leaving this
	// out would have fixed the detection and kept the accounting lie.
	if !externalWaitReasons[reason] {
		t.Fatalf("%q spends the active-work budget; #238 is precisely that defect", reason)
	}
	// And it is NOT continuation-eligible: a refused transport produced no
	// interrupted work for a retry to inherit.
	if PriorAttemptContextEligible(FailureProviderUnavailable) {
		t.Fatal("a provider that was never reached handed observations to its retry")
	}
}

// TestSilenceIsNeverClassifiedAsOffline is the negative half, and it is the one
// that keeps the signal list from becoming a substring swamp.
func TestSilenceIsNeverClassifiedAsOffline(t *testing.T) {
	for _, diagnostic := range []string{
		"",
		"\n\n",
		"thinking...",
		"Reading files in the repository",
		"the network of layers was retrained", // contains "network"
		"connection to the database is configured elsewhere",
		"request timed out",  // ambiguous: a slow provider is not an offline one
		"fetch failed",       // undici wraps every transport outcome in this
		"ETIMEDOUT",          // indistinguishable from server slowness
		"error sending mail", // not "error sending request"
	} {
		for name, spec := range map[string]cliAgentSpec{"codex": codexSpec, "claude": claudeSpec, "gemini": geminiSpec, "qwen": qwenSpec} {
			if got := classifyAgentFailure(spec, []byte(diagnostic), nil); got == FailureProviderUnavailable {
				t.Fatalf("%s guessed %q into %q", name, diagnostic, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// E. Quota, rate limiting and the account prerequisite are unchanged
// ---------------------------------------------------------------------------

// TestProviderCapacityClassificationIsUnchangedByConnectivitySignals is
// acceptance 9. The connectivity signals were added to the SAME per-provider
// lists the capacity signals live in, so the risk is real: an ordering mistake
// would reclassify a quota as a transport failure and change which wait an
// operator is told to clear.
func TestProviderCapacityClassificationIsUnchangedByConnectivitySignals(t *testing.T) {
	for name, tc := range map[string]struct {
		spec       cliAgentSpec
		diagnostic string
		want       FailureClass
	}{
		"codex quota":      {codexSpec, "You've hit your usage limit. Try again later.", FailureProviderQuota},
		"codex revoked":    {codexSpec, "your refresh token was revoked", FailureProviderAccountUnavailable},
		"claude quota":     {claudeSpec, "Claude usage limit reached", FailureProviderQuota},
		"claude credit":    {claudeSpec, "Your credit balance is too low", FailureProviderAccountUnavailable},
		"gemini quota":     {geminiSpec, "RESOURCE_EXHAUSTED", FailureProviderQuota},
		"qwen quota":       {qwenSpec, "quota exceeded for this project", FailureProviderQuota},
		"shared ratelimit": {claudeSpec, "429 Too Many Requests", FailureProviderRateLimited},
		// A transcript that mentions BOTH: the provider naming its own
		// allowance wins, because it is a statement about the account rather
		// than about the wire, and only the account statement tells the
		// operator what to do.
		"quota beside a reset": {codexSpec, "connection reset by peer\nusage limit reached", FailureProviderQuota},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyAgentFailure(tc.spec, []byte(tc.diagnostic), nil); got != tc.want {
				t.Fatalf("classified as %q, want the unchanged %q", got, tc.want)
			}
		})
	}
	// Unknown stays unknown and stays fail-closed.
	if got := classifyAgentFailure(codexSpec, []byte("the model produced an invalid patch"), nil); got != FailureUnknown {
		t.Fatalf("an unrecognized diagnostic became %q", got)
	}
}

// ---------------------------------------------------------------------------
// The no-progress classification's own law
// ---------------------------------------------------------------------------

// TestAStalledProviderRetriesWithoutInheritingWorkOrResettingAnything states
// what provider_no_progress means, which is deliberately different from every
// neighbouring class.
func TestAStalledProviderRetriesWithoutInheritingWorkOrResettingAnything(t *testing.T) {
	// A RETRY, bounded by the execution attempt ceiling. Not a wait - nothing
	// external refused anything - and not a stop, because the condition may
	// well be gone by the next attempt.
	if route := RouteFailure(FailureProviderNoProgress); route != RouteRetry {
		t.Fatalf("provider_no_progress routes to %q, want a bounded retry", route)
	}
	if !reattemptable(RouteFailure(FailureProviderNoProgress)) {
		t.Fatal("a stalled provider is not reattemptable, so the attempt ceiling cannot bound it")
	}
	// Silence is NOT interrupted work. A retry that inherited a stalled
	// attempt's observations would be handing the next invocation the record of
	// a provider that said nothing.
	if PriorAttemptContextEligible(FailureProviderNoProgress) {
		t.Fatal("a stalled attempt handed its observations to the retry")
	}
	// And it is not an external wait: the RUNTIME ended this invocation, so the
	// interval it spans is charged exactly as any other execution is. Excluding
	// it would let a stalling provider run for free.
	if externalWaitReasons[waitReason(FailureProviderNoProgress)] {
		t.Fatal("a runtime-terminated stall was excluded from the active-work budget")
	}
}

// ---------------------------------------------------------------------------
// F. Restart during silence
// ---------------------------------------------------------------------------

// TestTheInactivityWindowIsReconstructedFromDurableStateOnly is acceptance 10.
//
// The window a restarted controller applies must come from durable facts - the
// run's persisted budget and the operation's recorded progress - and never from
// a stopwatch or a default invented at read time. A fresh full window granted
// on every restart would be the original defect in a narrower place: silence
// would become free whenever the supervisor bounced.
func TestTheInactivityWindowIsReconstructedFromDurableStateOnly(t *testing.T) {
	const limit = 10 * time.Minute
	at := time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC)
	progress := at.Add(-4 * time.Minute)
	op := RunOperation{ID: "op", LastProgressAt: &progress}

	// REMAINING, not full: four minutes of recorded silence have already been
	// spent out of the window.
	if got := ProviderInactivityRemaining(limit, op, at); got != 6*time.Minute {
		t.Fatalf("remaining window = %s, want the 6m left after 4m of recorded silence", got)
	}
	if got := ProviderSilence(op, at); got != 4*time.Minute {
		t.Fatalf("silence = %s, want 4m", got)
	}
	// EXHAUSTED STAYS EXHAUSTED. Zero is how "no bound configured" is spelled
	// downstream, so the dispatch path refuses rather than running unbounded;
	// what matters here is that a restart cannot recover a window that is gone.
	spent := at.Add(-limit - time.Second)
	if got := ProviderInactivityRemaining(limit, RunOperation{LastProgressAt: &spent}, at); got != 0 {
		t.Fatalf("an exhausted window reconstructed %s of authority", got)
	}
	// An operation that has recorded nothing yet has the whole window, and an
	// unconfigured bound stays absent rather than resolving to a default here.
	if got := ProviderInactivityRemaining(limit, RunOperation{}, at); got != limit {
		t.Fatalf("a fresh operation received %s, want %s", got, limit)
	}
	if got := ProviderInactivityRemaining(0, op, at); got != 0 {
		t.Fatalf("an unconfigured bound invented %s", got)
	}
}

// TestAControllerThatDiesMidSilenceStillPaysForTheSilence is the other half of
// acceptance 10, and the one that makes a restart loop bounded.
//
// A restarted controller does start a NEW attempt, and a new attempt does get
// its own inactivity window - that is ordinary bounded-retry semantics. What
// must not happen is for the silent interval to be forgiven: if it were, a
// supervisor restarting every ten minutes would reproduce the 8h55m run
// exactly, one fresh window at a time.
func TestAControllerThatDiesMidSilenceStillPaysForTheSilence(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)

	// Ten minutes of silence, then the controller dies: no Finish, no
	// settlement, nothing folded by the process that was driving it.
	clock.advance(10 * time.Minute)
	// The abandoned lease is dropped and NOTHING else about the row is touched
	// - the same smallest-true-thing write reclaimAbandoned performs for a
	// driver that died between leasing and finishing. ActiveSince is therefore
	// still set, which is exactly the state a crash leaves behind.
	crashed, revision, ok, err := scheduler.Store.Operation(op.ID)
	if err != nil || !ok {
		t.Fatalf("operation %q: %v", op.ID, err)
	}
	crashed.Lease = nil
	if _, written, err := scheduler.Store.PutOperation(crashed, revision); err != nil || !written {
		t.Fatalf("dropping the abandoned lease: %v %v", written, err)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ConsumedExecution < 10*time.Minute {
		t.Fatalf("the silent interval was refunded: consumed %s of a 30m budget", resumed.ConsumedExecution)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 20*time.Minute {
		t.Fatalf("remaining execution authority = %s, want the 20m the silence did not spend", got)
	}
	// The attempt IDENTITY advanced, per #236/#237, so the resumed invocation
	// cannot address the transcript slot the silent one already owned.
	if resumed.AttemptIdentity <= op.AttemptIdentity {
		t.Fatalf("attempt identity did not advance across the restart: %d then %d", op.AttemptIdentity, resumed.AttemptIdentity)
	}
}

// TestRecognizedProgressAdvancesDurablyAndRepetitionDoesNot is what makes the
// reconstruction above mean anything: the durable instant has to move when the
// provider says something new, and only then.
func TestRecognizedProgressAdvancesDurablyAndRepetitionDoesNot(t *testing.T) {
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	started := *op.LastProgressAt

	clock.advance(time.Minute)
	moved, err := scheduler.RecordProviderProgress(op.ID, "512")
	if err != nil {
		t.Fatal(err)
	}
	if moved.LastProgressAt == nil || !moved.LastProgressAt.After(started) {
		t.Fatalf("observed output did not advance durable progress: %v", moved.LastProgressAt)
	}
	// THE SAME FINGERPRINT IS NOT PROGRESS. Re-observing what the provider
	// already said - and, above all, a lease heartbeat proving a controller is
	// alive - must not refresh the window.
	advanced := *moved.LastProgressAt
	clock.advance(time.Minute)
	same, err := scheduler.RecordProviderProgress(op.ID, "512")
	if err != nil {
		t.Fatal(err)
	}
	if !same.LastProgressAt.Equal(advanced) {
		t.Fatalf("an unchanged progress fingerprint refreshed the window: %s then %s", advanced, same.LastProgressAt)
	}
	// Progress is about the WORK, never about the budget: nothing it touches
	// gives execution authority back.
	if same.ConsumedExecution != moved.ConsumedExecution || same.Attempt != moved.Attempt {
		t.Fatalf("recording progress moved a budget: %#v", same)
	}
}

// ---------------------------------------------------------------------------
// The budget itself: finite, persisted, tighten-only
// ---------------------------------------------------------------------------

// TestTheProviderInactivityBudgetIsFiniteAndTightenOnly states the
// configuration law. There is deliberately no spelling that disables the bound:
// an unattended CLI worker with no inactivity window is the configuration #238
// was filed about.
func TestTheProviderInactivityBudgetIsFiniteAndTightenOnly(t *testing.T) {
	base := func() OperatorConfig {
		return OperatorConfig{
			StateDir: "/state", ProjectModelPath: "/m.json", PolicyPath: "/p.json",
			Assurance: AssuranceConfig{Image: "sha256:" + strings.Repeat("a", 64)},
			Agents:    map[string]AgentConfig{"codex": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)}},
			GitHub:    GitHubConfig{CredentialMode: GitHubCredentialNone},
			Budgets: BudgetConfig{
				WallLimitSeconds: 1800, MaxExecutionAttempts: 2,
				MaxRemediationAttempts: 2, MaxAssuranceAttempts: 2,
			},
		}
	}

	// ABSENT RESOLVES TO THE DEFAULT, because a configuration written before
	// this budget existed must stay loadable - and must not stay unbounded.
	absent := base().Budgets.resolved()
	if absent.ProviderInactivitySeconds != DefaultProviderInactivitySeconds {
		t.Fatalf("absent resolved to %ds, want the %ds default", absent.ProviderInactivitySeconds, DefaultProviderInactivitySeconds)
	}
	// The default is well inside the documented 1800s wall limit, which is the
	// property that makes it a stall detector rather than a second ceiling.
	if DefaultProviderInactivitySeconds >= 1800 {
		t.Fatalf("the default inactivity window %ds does not bite before the documented wall limit", DefaultProviderInactivitySeconds)
	}
	// A run constructed without going through the configuration layer is still
	// bounded. Zero here would mean "this provider may stall forever".
	if got := (RunBudgets{}).defaults().ProviderInactivityLimit; got != DefaultProviderInactivitySeconds*time.Second {
		t.Fatalf("an unconfigured run budget resolved to %s", got)
	}
	// And the effective number reaches the runtime-facing budget.
	stated := base()
	stated.Budgets.ProviderInactivitySeconds = 300
	if err := stated.validate("/config.json"); err != nil {
		t.Fatal(err)
	}
	if got := (Config{OperatorConfig: stated}).RunBudgets().ProviderInactivityLimit; got != 5*time.Minute {
		t.Fatalf("configured 300s reached the runtime as %s", got)
	}
	// A negative value is a mistake and is refused, like every neighbour.
	negative := base()
	negative.Budgets.ProviderInactivitySeconds = -1
	if err := negative.validate("/config.json"); err == nil {
		t.Fatal("a negative inactivity window was accepted")
	}

	// TIGHTEN-ONLY. A repository may ask for a shorter window for its own work
	// and can never ask for a longer one: a repository that could widen it
	// would be choosing how long its own provider may stall.
	operator := base()
	operator.Budgets = operator.Budgets.resolved()
	tighter := 60
	tightened, err := operator.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{ProviderInactivitySeconds: &tighter}})
	if err != nil {
		t.Fatal(err)
	}
	if tightened.Budgets.ProviderInactivitySeconds != 60 {
		t.Fatalf("a tightening proposal was ignored: %ds", tightened.Budgets.ProviderInactivitySeconds)
	}
	wider := DefaultProviderInactivitySeconds + 1
	if _, err := operator.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{ProviderInactivitySeconds: &wider}}); err == nil {
		t.Fatal("a repository widened its own inactivity window")
	}
	// The operator layer is not written through by a tighten.
	if operator.Budgets.ProviderInactivitySeconds != DefaultProviderInactivitySeconds {
		t.Fatalf("tightening mutated the operator layer: %ds", operator.Budgets.ProviderInactivitySeconds)
	}
}

// TestARunKeepsTheTighterWindowItWasCreatedUnder proves the persisted budget
// narrows the configured one, exactly as the wall limit does. A run created
// under a tighter window must not silently widen because the operator later
// relaxed their configuration.
func TestARunKeepsTheTighterWindowItWasCreatedUnder(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.Budgets.ProviderInactivityLimit = 10 * time.Minute
	engine := fixture.newRuntime(deps)

	runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	state, err := engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.budgets().ProviderInactivityLimit; got != 10*time.Minute {
		t.Fatalf("the configured window reached the run as %s", got)
	}
	// A run persisted under a TIGHTER window keeps it.
	state.run.Budgets = &RunBudgets{ProviderInactivityLimit: 2 * time.Minute}
	if got := state.budgets().ProviderInactivityLimit; got != 2*time.Minute {
		t.Fatalf("the run's tighter window was overruled by configuration: %s", got)
	}
	// A run persisted under a WIDER one does not widen the configured bound.
	state.run.Budgets = &RunBudgets{ProviderInactivityLimit: time.Hour}
	if got := state.budgets().ProviderInactivityLimit; got != 10*time.Minute {
		t.Fatalf("a persisted run widened its own window to %s", got)
	}
	// A run persisted BEFORE this budget existed carries none, and must get the
	// configured window rather than an unbounded one.
	state.run.Budgets = &RunBudgets{}
	if got := state.budgets().ProviderInactivityLimit; got != 10*time.Minute {
		t.Fatalf("a pre-existing run resolved to %s, want the configured window", got)
	}
}
