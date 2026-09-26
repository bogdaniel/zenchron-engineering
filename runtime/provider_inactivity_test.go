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
			if got := classifyAgentFailure(tc.spec, terminalDiagnostic([]byte(tc.diagnostic))); got != FailureProviderUnavailable {
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
			if got := classifyAgentFailure(spec, terminalDiagnostic([]byte(diagnostic))); got == FailureProviderUnavailable {
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
			if got := classifyAgentFailure(tc.spec, terminalDiagnostic([]byte(tc.diagnostic))); got != tc.want {
				t.Fatalf("classified as %q, want the unchanged %q", got, tc.want)
			}
		})
	}
	// Unknown stays unknown and stays fail-closed.
	if got := classifyAgentFailure(codexSpec, terminalDiagnostic([]byte("the model produced an invalid patch"))); got != FailureUnknown {
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

// abandonExecution is a controller DEATH, not a settlement: the lease is
// dropped and nothing else about the row is touched, which is exactly the
// smallest-true-thing write reclaimAbandoned performs for a driver that died
// between leasing and finishing.
//
// ActiveSince therefore survives, and that survival is the durable evidence
// that the attempt was never observed to end. Everything the successor
// inherits - the charged execution interval and the inactivity datum - is
// keyed off it.
func abandonExecution(t *testing.T, scheduler Scheduler, id string) {
	t.Helper()
	crashed, revision, ok, err := scheduler.Store.Operation(id)
	if err != nil || !ok {
		t.Fatalf("operation %q: %v", id, err)
	}
	if crashed.ActiveSince == nil {
		t.Fatal("the operation was not executing, so there is no crash to simulate")
	}
	crashed.Lease = nil
	if _, written, err := scheduler.Store.PutOperation(crashed, revision); err != nil || !written {
		t.Fatalf("dropping the abandoned lease: %v %v", written, err)
	}
	if _, err := scheduler.Next(crashed.RunID); err != nil {
		t.Fatal(err)
	}
}

// TestARestartDuringSilenceInheritsTheRemainingInactivityAuthority is #238
// acceptance 10, stated as the contract requires rather than as the
// implementation found convenient.
//
// The distinction the whole test turns on is that there are TWO authorities
// here and a restart must not refund either:
//
//	execution authority     ConsumedExecution against WallBudget
//	inactivity authority    LastProgressAt against the no-progress window
//
// Charging the orphaned interval to the first proves the run wall budget did
// not reset. It proves nothing at all about the second - and an earlier draft
// of this change reset LastProgressAt to `now` in Scheduler.Start, so every
// reclaim handed the successor a brand-new full window. A supervisor that
// bounced every few minutes would have reproduced the 8h55m run exactly, one
// clean window at a time, while this test passed.
func TestARestartDuringSilenceInheritsTheRemainingInactivityAuthority(t *testing.T) {
	const window = 10 * time.Minute
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)
	if op.LastProgressAt == nil || !op.LastProgressAt.Equal(clock.Now()) {
		t.Fatalf("a first attempt did not open its inactivity window: %v", op.LastProgressAt)
	}
	if got := ProviderInactivityRemaining(window, op, clock.Now()); got != window {
		t.Fatalf("the first attempt received %s of a %s window", got, window)
	}

	// FOUR MINUTES OF PROVEN SILENCE. Nothing records progress, which is what
	// silence IS: the durable datum stays where the attempt opened it.
	clock.advance(4 * time.Minute)
	if got := ProviderInactivityRemaining(window, op, clock.Now()); got != 6*time.Minute {
		t.Fatalf("the live attempt reports %s remaining after 4m of silence, want 6m", got)
	}

	// The controller dies here. No Finish, no journalled outcome, no
	// classification - nothing observed how that attempt ended.
	abandonExecution(t, scheduler, op.ID)
	resumed, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}

	// THE ASSERTION THIS TEST EXISTS FOR. Six minutes, not a fresh ten.
	//
	// This is what fails if Scheduler.Start resets the progress origin to
	// `now`: the successor would report the full window and the silence the
	// dead attempt had already proven would be forgiven.
	remaining := ProviderInactivityRemaining(window, resumed, clock.Now())
	if remaining == window {
		t.Fatalf("the restart granted a fresh full %s inactivity window; the 4m of proven silence was refunded", window)
	}
	if remaining != 6*time.Minute {
		t.Fatalf("the successor received %s of inactivity authority, want the 6m the dead attempt did not spend", remaining)
	}
	if resumed.LastProgressAt == nil || !resumed.LastProgressAt.Equal(*op.LastProgressAt) {
		t.Fatalf("the inactivity datum was rewritten across the restart: %v then %v", op.LastProgressAt, resumed.LastProgressAt)
	}
	if got := ProviderSilence(resumed, clock.Now()); got != 4*time.Minute {
		t.Fatalf("the successor reports %s of silence, want the 4m it inherited", got)
	}

	// AND THE OTHER AUTHORITY IS STILL CHARGED, separately and as before. The
	// two are different budgets and both must survive the restart; proving one
	// has never been proof of the other.
	if resumed.ConsumedExecution < 4*time.Minute {
		t.Fatalf("the orphaned interval was refunded: consumed %s of a 30m budget", resumed.ConsumedExecution)
	}
	if got := OperationRemaining(resumed, clock.Now()); got != 26*time.Minute {
		t.Fatalf("remaining execution authority = %s, want the 26m the silence did not spend", got)
	}

	// #236/#237 SURVIVE UNCHANGED. The physical attempt identity advances, so
	// the successor cannot address the transcript slot the dead attempt owned,
	// and the evidence store still refuses to reuse it.
	if resumed.AttemptIdentity != op.AttemptIdentity+1 {
		t.Fatalf("attempt identity %d did not advance monotonically from %d", resumed.AttemptIdentity, op.AttemptIdentity)
	}
	store := ArtifactStore{Root: t.TempDir()}
	before := ExecutionAttemptRef{RunID: op.RunID, OperationID: op.ID, Attempt: op.AttemptIdentity}
	after := ExecutionAttemptRef{RunID: op.RunID, OperationID: op.ID, Attempt: resumed.AttemptIdentity}
	first, err := store.StoreExecutionAttemptTranscript("codex", before, []byte("attempt one"), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.StoreExecutionAttemptTranscript("codex", after, []byte("attempt two"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if first[0].Path == second[0].Path {
		t.Fatalf("both attempts addressed one transcript slot: %s", first[0].Path)
	}
	if _, err := store.StoreExecutionAttemptTranscript("codex", before, []byte("overwrite"), nil); err == nil {
		t.Fatal("the dead attempt's transcript was overwritable")
	}
	// The per-physical-attempt progress FINGERPRINT is cleared, which is the
	// other half of the distinction: it is a byte count of one dead process's
	// output, and comparing the successor's first bytes against it would read
	// real progress as "nothing new".
	if resumed.NoProgressKey != "" {
		t.Fatalf("the successor inherited the dead process's output fingerprint %q", resumed.NoProgressKey)
	}
}

// TestASilentRestartCannotBeRepeatedIntoAFreshWindow is the loop the law is
// actually about. Three reclaims in a row, each after four minutes of silence,
// must add up: twelve minutes of proven silence exhausts a ten-minute window
// rather than resetting it three times.
func TestASilentRestartCannotBeRepeatedIntoAFreshWindow(t *testing.T) {
	const window = 10 * time.Minute
	scheduler, clock := deadlineScheduler(t)
	// A generous ATTEMPT ceiling and a generous wall budget, so that neither of
	// them is what stops the loop. The only bound under test here is the
	// inactivity authority.
	planned, _, err := scheduler.Plan(RunOperation{
		RunID: "run-restarts", Kind: OpExecutionInvoke, IdempotencyKey: "invoke-restarts",
		MaxAttempts: 8, WallBudget: 4 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.Next(planned.RunID); err != nil {
		t.Fatal(err)
	}
	op, err := scheduler.Start(planned.ID)
	if err != nil {
		t.Fatal(err)
	}
	opened := *op.LastProgressAt

	for round := 1; round <= 3; round++ {
		clock.advance(4 * time.Minute)
		abandonExecution(t, scheduler, op.ID)
		resumed, err := scheduler.Start(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !resumed.LastProgressAt.Equal(opened) {
			t.Fatalf("round %d moved the inactivity datum to %s", round, resumed.LastProgressAt)
		}
		want := window - time.Duration(round)*4*time.Minute
		if want < 0 {
			want = 0
		}
		if got := ProviderInactivityRemaining(window, resumed, clock.Now()); got != want {
			t.Fatalf("after %d restarts the successor holds %s of inactivity authority, want %s", round, got, want)
		}
	}
	// Twelve minutes of accumulated silence, so there is nothing left to
	// spend. invokeExecution refuses rather than dispatching, because a zero
	// window is how "no bound configured" is spelled downstream and
	// dispatching with one would restore the pre-#238 behaviour exactly where
	// it is least affordable.
	final, _, _, err := scheduler.Store.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProviderInactivityRemaining(window, final, clock.Now()); got != 0 {
		t.Fatalf("twelve minutes of silence left %s of a %s window", got, window)
	}
}

// TestASettledStallStillGetsItsBoundedRetry is the other side of the same
// rule, and it is what keeps the fix above from turning a bounced supervisor
// into a dead run.
//
// An ABANDONED attempt carries its silence forward, because nothing observed
// how it ended. A SETTLED one does not: its outcome was journalled and
// classified, provider_no_progress routes to a bounded RETRY, and a retry that
// inherited an exhausted window would refuse before dispatch forever. The
// attempt ceiling is what bounds it, exactly as it bounds every other
// reattemptable class.
//
// Without this distinction the repair would be self-perpetuating: one long
// reclaim gap would exhaust the window, every successor would refuse without
// calling the provider, and the run would die of attempt exhaustion having
// never dispatched.
func TestASettledStallStillGetsItsBoundedRetry(t *testing.T) {
	const window = 10 * time.Minute
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 4*time.Hour)

	// The whole window is spent in silence and the attempt is TERMINATED and
	// SETTLED - the durable shape the inactivity policy itself produces.
	clock.advance(window + time.Minute)
	if got := ProviderInactivityRemaining(window, op, clock.Now()); got != 0 {
		t.Fatalf("a fully silent attempt still holds %s", got)
	}
	settled, err := scheduler.Finish(op.ID, OperationFailed)
	if err != nil {
		t.Fatal(err)
	}
	// SETTLING IS WHAT MAKES THE DIFFERENCE VISIBLE, and it is one durable
	// fact: Finish clears ActiveSince, so the next Start cannot mistake this
	// attempt for one nobody observed. It is the same fact the pre-dispatch
	// refusal produces - that path fails the operation, which is journalled and
	// finished exactly like this - so a run whose window was exhausted by a
	// long reclaim gap spends ONE attempt naming it and then dispatches for
	// real, rather than refusing forever without ever calling the provider.
	if settled.ActiveSince != nil {
		t.Fatalf("a settled attempt still looks abandoned: ActiveSince=%s", settled.ActiveSince)
	}
	if _, err := scheduler.Next(op.RunID); err != nil {
		t.Fatal(err)
	}
	retried, err := scheduler.Start(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProviderInactivityRemaining(window, retried, clock.Now()); got != window {
		t.Fatalf("a settled stall's bounded retry received %s, want a full %s window", got, window)
	}
	// It is a RETRY, not a refund. The silence it already spent stays charged
	// to the execution budget, and the attempt ceiling is what ends this.
	if retried.ConsumedExecution < window {
		t.Fatalf("settling refunded the silent interval: consumed %s", retried.ConsumedExecution)
	}
	if retried.Attempt <= op.Attempt {
		t.Fatalf("the retry did not spend an attempt: %d then %d", op.Attempt, retried.Attempt)
	}
	if retried.AttemptIdentity != op.AttemptIdentity+1 {
		t.Fatalf("attempt identity %d did not advance from %d", retried.AttemptIdentity, op.AttemptIdentity)
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
	moved, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, "512")
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
	same, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, "512")
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

// ---------------------------------------------------------------------------
// The trust boundary: a transcript is evidence, not an assertion
// ---------------------------------------------------------------------------

// sessionQuotingTransportPhrases is ordinary model-visible content that happens
// to carry a phrase this runtime recognizes: a model quoting an error it read,
// a captured test log, documentation, a review comment, prose about a quota,
// and a source file the worker wrote.
//
// None of it is a statement about the provider's transport, and none of it may
// move a run into a typed external wait. The end-to-end proof is
// TestSessionOutputCannotCreateAnExternalProviderWait, which puts these on
// STDOUT of a real process; the structural reason is that the classification
// surface no longer accepts stdout at all.
var sessionQuotingTransportPhrases = map[string]string{
	"model quoting a transport error": "I ran the integration suite and it printed `dial tcp 127.0.0.1:8080: connect: ECONNREFUSED`, so the fixture server is not running.",
	"a captured test log":             "--- FAIL: TestUpstreamFetch\n    client_test.go:88: getaddrinfo ENOTFOUND api.example.internal\nFAIL\nexit status 1",
	"documentation the worker read":   "The retry table in docs/http.md lists 502 Bad Gateway and 503 Service Unavailable as retryable statuses.",
	"a review comment":                "This handler swallows connection reset by peer, which hides a real outage from the operator.",
	"a quota phrase in prose":         "The rate limiter returns once the usage limit reached state clears; see limiter.go.",
	"a source file the worker wrote":  "// handleUnavailable answers 503 Service Unavailable while the pool drains.",
}

// TestTheTerminalSurfaceExcludesTheSessionRendering states the rule itself
// rather than a consequence of it: stdout is never consulted, stderr is, and
// only its final words are.
func TestTheTerminalSurfaceExcludesTheSessionRendering(t *testing.T) {
	// A genuine offline diagnostic is preserved, which is the whole point of
	// keeping the classification rather than deleting it.
	genuine := "ERROR: stream error: error sending request for url (https://chatgpt.com/backend-api/codex/responses): dns error\n"
	if got := classifyAgentFailure(codexSpec, terminalDiagnostic([]byte(genuine))); got != FailureProviderUnavailable {
		t.Fatalf("a genuine transport diagnostic classified as %q", got)
	}
	// The SAME bytes, arriving as session output instead, assert nothing. This
	// is the pair that makes the boundary a boundary: identical text, opposite
	// answers, decided by which channel it arrived on.
	if got := classifyAgentFailure(codexSpec, terminalDiagnostic(nil)); got != FailureUnknown {
		t.Fatalf("an empty diagnostic stream classified as %q", got)
	}

	// AND ONLY THE TAIL. A phrase buried far behind the process's last words is
	// not the condition it exited with.
	restore := maxTerminalDiagnosticBytes
	t.Cleanup(func() { maxTerminalDiagnosticBytes = restore })
	maxTerminalDiagnosticBytes = 64
	buried := genuine + strings.Repeat("x", 200)
	if got := classifyAgentFailure(codexSpec, terminalDiagnostic([]byte(buried))); got != FailureUnknown {
		t.Fatalf("a phrase outside the terminal window classified as %q", got)
	}
	if got := classifyAgentFailure(codexSpec, terminalDiagnostic([]byte(strings.Repeat("x", 200)+"dns error\n"))); got != FailureProviderUnavailable {
		t.Fatalf("the process's last words classified as %q", got)
	}
}

// TestALeaseHeartbeatCannotRefreshProviderInactivityAuthority is the invariant
// at the API boundary rather than at the one call site that happens to exist.
//
// A lease heartbeat says a CONTROLLER is alive. Provider inactivity authority
// is a claim that the WORK moved. #238 is the cost of letting the first stand
// in for the second, and Scheduler.Heartbeat used to take a progress value and
// advance the durable progress datum whenever it changed - so a supervisor that
// was merely still running could have held a dead provider's window open
// indefinitely. Nothing in production called it that way, which is exactly why
// it needed pinning: an untrue invariant with no current caller is a regression
// waiting for its first one.
func TestALeaseHeartbeatCannotRefreshProviderInactivityAuthority(t *testing.T) {
	const window = 10 * time.Minute
	scheduler, clock := deadlineScheduler(t)
	op := plannedExecution(t, scheduler, 30*time.Minute)

	// Real provider output, recorded durably, opens the window where it should.
	clock.advance(time.Minute)
	if _, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, "512"); err != nil {
		t.Fatal(err)
	}
	observed, _, _, err := scheduler.Store.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	progressAt, fingerprint := *observed.LastProgressAt, observed.NoProgressKey
	if !progressAt.Equal(clock.Now()) || fingerprint != "512" {
		t.Fatalf("provider progress was not recorded: %s %q", progressAt, fingerprint)
	}

	// FOUR MINUTES OF HEARTBEATS AND NO PROVIDER OUTPUT. The lease is renewed
	// every minute, which is what a live controller does; none of it is
	// evidence that the invocation is advancing.
	for minute := 0; minute < 4; minute++ {
		clock.advance(time.Minute)
		beat, err := scheduler.Heartbeat(op.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !beat.Lease.HeartbeatAt.Equal(clock.Now()) {
			t.Fatalf("the lease was not renewed: %s", beat.Lease.HeartbeatAt)
		}
		if beat.LastProgressAt == nil || !beat.LastProgressAt.Equal(progressAt) {
			t.Fatalf("a lease heartbeat moved the inactivity datum to %v", beat.LastProgressAt)
		}
		if beat.NoProgressKey != fingerprint {
			t.Fatalf("a lease heartbeat rewrote the progress fingerprint to %q", beat.NoProgressKey)
		}
	}
	// The window has therefore been spent, exactly as if nothing had called
	// Heartbeat at all.
	spent, _, _, err := scheduler.Store.Operation(op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := ProviderInactivityRemaining(window, spent, clock.Now()); got != 6*time.Minute {
		t.Fatalf("after four minutes of heartbeats the operation holds %s of inactivity authority, want 6m", got)
	}

	// AND REAL OUTPUT STILL DOES BOTH. The narrowing must not have made the
	// durable record unreachable.
	moved, err := scheduler.RecordProviderProgress(op.ID, op.AttemptIdentity, "1024")
	if err != nil {
		t.Fatal(err)
	}
	if moved.LastProgressAt == nil || !moved.LastProgressAt.Equal(clock.Now()) {
		t.Fatalf("observed output did not advance the datum: %v", moved.LastProgressAt)
	}
	if moved.NoProgressKey != "1024" {
		t.Fatalf("observed output did not advance the fingerprint: %q", moved.NoProgressKey)
	}
	if got := ProviderInactivityRemaining(window, moved, clock.Now()); got != window {
		t.Fatalf("recognized progress left %s of the window, want the full %s", got, window)
	}
}
