//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runtime

// #238, proved against a real process group rather than a proxy for one.
//
// Every test in this file drives the production path end to end: the real
// bounded process, the real capture buffers, the real inactivity watch, the
// real process-group stop sequence, and the real adapter classification. The
// only double is the CLI CATALOGUE - help text and version, which are local
// string answers - because the scenario is about what the runtime does to a
// process, not about which flags a vendor advertises.
//
// The scenario is the reported one: a coding CLI that is alive, holds its
// process, and says nothing, while the run's outer wall budget is hours away.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// inactivityCLI answers the capability probe from a string and runs the
// invocation as a REAL bounded process.
//
// The split is the point. Nothing about the watchdog, the buffers, the signal
// escalation or the classification is simulated here; if the mechanism were
// removed, no timeout inside this double would stand in for it. See
// TestRemovingTheInactivityPolicyLetsTheSilentProviderRunOn, which proves that
// by removing it.
type inactivityCLI struct {
	script string
	runs   int
}

func (c *inactivityCLI) LookPath(string) error { return nil }

func (c *inactivityCLI) Output(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	if len(args) == 1 && args[0] == "--version" {
		return CommandOutput{Stdout: []byte("1.2.3\n")}, nil
	}
	return CommandOutput{Stdout: []byte(capableHelp)}, nil
}

func (c *inactivityCLI) Run(ctx context.Context, _ string, _ []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	c.runs++
	// PATH is added because the provider's child environment is a strict
	// allowlist that need not resolve `sleep`. Everything else - the process
	// group, the capture, the bound - is production code.
	return OSCommandExecutor{}.Run(ctx, "sh", []string{"-c", c.script}, dir,
		append(append([]string(nil), env...), "PATH="+os.Getenv("PATH")), grace)
}

// silentProviderScript is the reported provider: it records its process-group
// id, refuses SIGTERM, and holds forever without saying anything.
//
// Ignoring SIGTERM is deliberate. It forces the stop sequence past its graceful
// signal into the forced kill, so "no detached child survives" is proved
// against a process that actively resists the polite request.
func silentProviderScript(pidFile string) string {
	return "trap '' TERM\n" +
		"echo $$ > " + pidFile + "\n" +
		"while :; do sleep 30; done\n"
}

// inactivityFixture is one native CLI agent whose invocation is a real process.
func inactivityFixture(t *testing.T, script string) (CLIAgentProvider, ExecutionRequest, *inactivityCLI) {
	t.Helper()
	requireBoundedProcess(t)
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	cli := &inactivityCLI{script: script}
	provider.Executor = cli
	// A short grace so the graceful-then-forced escalation completes inside an
	// ordinary test, not because it changes what is being proved.
	provider.Grace = 150 * time.Millisecond
	// THE OUTER CEILING STAYS WHERE THE REPORTED RUN HAD IT. An hour of wall
	// budget is exactly the bound that discovered the dead provider 8h55m late,
	// and nothing in these tests may be explained by it.
	request.Budgets = ProviderBudget{WallLimit: time.Hour}
	return provider, request, cli
}

// inactivityWindow is the short window these tests measure against. It is small
// so the suite is fast; nothing about the mechanism depends on its size.
const inactivityWindow = 400 * time.Millisecond

// ---------------------------------------------------------------------------
// A. Silent alive provider
// ---------------------------------------------------------------------------

// TestASilentProviderIsTerminatedLongBeforeTheRunWallBudget is acceptance 1-5.
func TestASilentProviderIsTerminatedLongBeforeTheRunWallBudget(t *testing.T) {
	const limit = inactivityWindow
	pidFile := filepath.Join(t.TempDir(), "provider.pid")
	provider, request, cli := inactivityFixture(t, silentProviderScript(pidFile))
	request.Budgets.InactivityLimit = limit

	started := time.Now()
	result, err := provider.Execute(context.Background(), request)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a provider the runtime killed for silence returned no error")
	}
	if cli.runs != 1 {
		t.Fatalf("the provider ran %d times", cli.runs)
	}

	// THE INACTIVITY BOUND IS WHAT STOPPED IT, not the wall budget. An hour of
	// wall budget was available; the whole invocation lasted under a second.
	if elapsed > 10*time.Second {
		t.Fatalf("the invocation took %s: nothing bounded it but the outer budget", elapsed)
	}
	if elapsed < limit {
		t.Fatalf("the invocation lasted %s, less than the %s window: something other than the inactivity policy killed it", elapsed, limit)
	}

	// A TYPED STALL, and specifically not a shutdown, a deadline or an unknown.
	if result.Outcome != OperationFailed {
		t.Fatalf("outcome = %q, want a failed operation", result.Outcome)
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure = %#v, want %q", result.Failure, FailureProviderNoProgress)
	}
	if result.Invocation == nil {
		t.Fatal("no invocation provenance was recorded for a terminated provider")
	}
	if result.Invocation.TerminationCause != "provider_inactivity_limit_reached" {
		t.Fatalf("termination cause = %q, want the inactivity policy named", result.Invocation.TerminationCause)
	}
	if result.Invocation.InactivityLimit != limit {
		t.Fatalf("recorded inactivity limit = %s, want %s", result.Invocation.InactivityLimit, limit)
	}
	if result.Invocation.ProcessID == 0 {
		t.Fatal("the process the runtime owned was not recorded")
	}

	// NO DETACHED CHILD SURVIVES, even one that refused SIGTERM.
	if err := waitForFile(pidFile, 5*time.Second); err != nil {
		t.Fatalf("the provider never started: %v", err)
	}
	if processFromFileAlive(pidFile) {
		t.Fatal("the silent provider outlived its termination and can still mutate the candidate")
	}

	// THE EVIDENCE IS PRESERVED and the stall grants nothing.
	if len(result.Artifacts) == 0 {
		t.Fatal("the attempt transcript was not stored")
	}
	if result.Failure.RawDiagnosticRef != result.Artifacts[0].Path {
		t.Fatalf("the diagnostic does not name the transcript: %q", result.Failure.RawDiagnosticRef)
	}
	if result.Review != nil {
		t.Fatal("a terminated invocation contributed a verdict")
	}
}

// ---------------------------------------------------------------------------
// B. Progress refreshes the window
// ---------------------------------------------------------------------------

// TestAProviderThatKeepsTalkingIsNotKilledForSilence is acceptance 6.
//
// The invocation deliberately outlives the window several times over. If output
// did not refresh the bound, it would be killed; if the bound did not exist, it
// would prove nothing, which is what the run at the bottom of this file is for.
func TestAProviderThatKeepsTalkingIsNotKilledForSilence(t *testing.T) {
	const limit = 300 * time.Millisecond
	provider, request, _ := inactivityFixture(t,
		"i=0\nwhile [ $i -lt 20 ]; do echo working; sleep 0.05; i=$((i+1)); done\n")
	request.Budgets.InactivityLimit = limit

	started := time.Now()
	result, err := provider.Execute(context.Background(), request)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("a provider that was producing output was failed: %v", err)
	}
	if result.Outcome != Succeeded || result.Failure != nil {
		t.Fatalf("outcome = %q failure = %#v, want an untouched success", result.Outcome, result.Failure)
	}
	if result.Invocation.TerminationCause != "provider_returned" {
		t.Fatalf("termination cause = %q, want the provider's own return", result.Invocation.TerminationCause)
	}
	// It ran past the window - so the refresh is what kept it alive, not luck.
	if elapsed <= limit {
		t.Fatalf("the invocation finished in %s, inside the %s window: the refresh was never exercised", elapsed, limit)
	}
	// Each accepted signal extends ONLY the inactivity window. Nothing about
	// the total budget moved.
	if result.Invocation.InactivityLimit != limit {
		t.Fatalf("progress changed the recorded window to %s", result.Invocation.InactivityLimit)
	}
}

// ---------------------------------------------------------------------------
// C. Progress cannot buy time past the total bound
// ---------------------------------------------------------------------------

// TestContinuousProgressStillCannotExceedTheTotalWallBound is acceptance 7.
// Refreshing the inactivity window is not a way to buy execution authority.
func TestContinuousProgressStillCannotExceedTheTotalWallBound(t *testing.T) {
	provider, request, _ := inactivityFixture(t, "while :; do echo working; sleep 0.02; done\n")
	request.Budgets.InactivityLimit = 300 * time.Millisecond
	// A total bound WELL ABOVE the inactivity window, so the only thing that
	// can stop this chatty provider is the deadline.
	deadline := time.Now().Add(900 * time.Millisecond)
	request.Deadline = &deadline

	started := time.Now()
	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("a provider past its deadline returned no error")
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the total bound did not stop a continuously talking provider: %s", elapsed)
	}
	if result.Invocation == nil || result.Invocation.TerminationCause != "deadline_reached" {
		t.Fatalf("termination cause = %#v, want the deadline", result.Invocation)
	}
	// The class stays the EXISTING one for a runtime bound reached with work
	// preserved. A provider that was talking did not stall, and calling it one
	// would tell an operator to look at connectivity.
	if result.Failure == nil || result.Failure.Classification != FailureExecutionIncomplete {
		t.Fatalf("failure = %#v, want %q", result.Failure, FailureExecutionIncomplete)
	}
}

// ---------------------------------------------------------------------------
// G / H. Crash boundary and immutable evidence
// ---------------------------------------------------------------------------

// TestAStallTerminationLeavesNoSecondProcessAndNoOverwrittenTranscript is
// acceptances 11 and 12, and the #236/#237 laws this must not weaken.
//
// The crash boundary is the interval between the termination DECISION and the
// durable settlement of the operation. Nothing is settled here on purpose: the
// test asks what a controller that died in that window would leave behind.
func TestAStallTerminationLeavesNoSecondProcessAndNoOverwrittenTranscript(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "first.pid")
	provider, request, _ := inactivityFixture(t, silentProviderScript(first))
	request.Budgets.InactivityLimit = inactivityWindow

	first1, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("the silent provider was not terminated")
	}
	transcript := first1.Artifacts[0].Path
	before, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}

	// THE PROCESS GROUP IS ALREADY GONE before the adapter returned, so the
	// crash window cannot contain a live provider at all. That is what makes
	// "no duplicate provider process" a structural answer rather than a race
	// the next attempt has to win.
	if processFromFileAlive(first) {
		t.Fatal("a provider survived the termination decision into the crash window")
	}

	// The controller dies here: no operation.after, no Finish. Recovery
	// allocates the next PHYSICAL attempt identity, per #237, and dispatches
	// again.
	second := filepath.Join(dir, "second.pid")
	retry, retryRequest, cli := inactivityFixture(t, silentProviderScript(second))
	retry.ArtifactStore = provider.ArtifactStore
	retryRequest.RunID, retryRequest.OperationID = request.RunID, request.OperationID
	retryRequest.Attempt = request.Attempt + 1
	retryRequest.Budgets.InactivityLimit = inactivityWindow
	second2, err := retry.Execute(context.Background(), retryRequest)
	if err == nil {
		t.Fatal("the retry was not terminated either")
	}
	if cli.runs != 1 {
		t.Fatalf("the retry started %d provider processes", cli.runs)
	}
	if processFromFileAlive(second) {
		t.Fatal("the retry's provider outlived its termination")
	}

	// A FRESH IMMUTABLE IDENTITY, and the earlier transcript byte-identical.
	if second2.Artifacts[0].Path == transcript {
		t.Fatalf("the retry addressed the previous attempt's transcript slot %q", transcript)
	}
	after, err := os.ReadFile(transcript)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("the retry mutated the previous attempt's transcript")
	}
	// And the create-once refusal is still in force for the slot that exists.
	if _, err := provider.ArtifactStore.StoreExecutionAttemptTranscript(
		provider.Agent.ID, request.AttemptRef(), []byte("overwrite"), nil); err == nil {
		t.Fatal("an existing attempt transcript was overwritable")
	}
}

// ---------------------------------------------------------------------------
// I. Mutation / vacuity proof
// ---------------------------------------------------------------------------

// TestRemovingTheInactivityPolicyLetsTheSilentProviderRunOn is the proof that
// TestASilentProviderIsTerminatedLongBeforeTheRunWallBudget observes the
// mechanism it claims to certify.
//
// It is the IDENTICAL fixture with exactly one variable changed: no inactivity
// policy on the context. If any other bound - the grace period, a lease, the
// executor, os/exec's WaitDelay, the test binary's own timeout - were what
// actually killed the silent provider above, this invocation would end too.
// It does not: the provider is still alive after several windows' worth of
// silence, which is the pre-#238 behaviour, and the run that produced the
// 8h55m active-work charge.
//
// #237 shipped a test that structurally could not observe its own mechanism.
// This is the answer to that: the assertion is about the mechanism's ABSENCE.
func TestRemovingTheInactivityPolicyLetsTheSilentProviderRunOn(t *testing.T) {
	const limit = inactivityWindow
	pidFile := filepath.Join(t.TempDir(), "provider.pid")
	provider, request, _ := inactivityFixture(t, silentProviderScript(pidFile))

	// THE ONE CHANGED VARIABLE: no inactivity bound on the request. Everything
	// else is byte-for-byte the fixture the passing test uses.
	request.Budgets.InactivityLimit = 0
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan ExecutionResult, 1)
	go func() {
		result, _ := provider.Execute(ctx, request)
		done <- result
	}()
	if err := waitForFile(pidFile, 5*time.Second); err != nil {
		t.Fatalf("the provider never started: %v", err)
	}

	// Several windows of silence later, nothing has stopped it.
	time.Sleep(4 * limit)
	select {
	case result := <-done:
		cancel()
		t.Fatalf("something other than the inactivity policy ended the silent provider: %#v", result.Invocation)
	default:
	}
	if !processFromFileAlive(pidFile) {
		cancel()
		t.Fatal("the silent provider died without an inactivity policy, so the policy is not what kills it above")
	}

	// Clean up through the same process-group cancellation, and confirm the
	// cause is a plain cancellation rather than a stall - the two must never be
	// the same answer.
	cancel()
	result := <-done
	if result.Failure != nil && result.Failure.Classification == FailureProviderNoProgress {
		t.Fatal("an ordinary cancellation was classified as a provider stall")
	}
	if result.Invocation != nil && strings.Contains(result.Invocation.TerminationCause, "inactivity") {
		t.Fatalf("a cancellation was recorded as an inactivity termination: %q", result.Invocation.TerminationCause)
	}
	if processFromFileAlive(pidFile) {
		t.Fatal("the provider outlived the cancellation")
	}
}

// TestTheInactivityCauseIsDistinctFromEveryOtherCancellation pins the one thing
// context.Err() alone could never say. A stall, a shutdown and a deadline are
// three different facts about a run, and collapsing them is what made an
// unreachable provider look like eight hours of engineering.
func TestTheInactivityCauseIsDistinctFromEveryOtherCancellation(t *testing.T) {
	bounded, release := withProviderInactivity(context.Background(), time.Minute, nil)
	defer release()
	if providerInactivityCause(bounded) {
		t.Fatal("a live invocation reports an inactivity termination")
	}
	if providerInactivityLimit(bounded) != time.Minute {
		t.Fatalf("the bound did not reach the context: %s", providerInactivityLimit(bounded))
	}

	// A shutdown.
	shutdown, cancel := context.WithCancel(bounded)
	cancel()
	if providerInactivityCause(shutdown) {
		t.Fatal("a cancelled controller was reported as a stalled provider")
	}
	// A deadline.
	expired, stop := context.WithDeadline(bounded, time.Now().Add(-time.Second))
	defer stop()
	if providerInactivityCause(expired) {
		t.Fatal("an expired deadline was reported as a stalled provider")
	}
	if !errors.Is(context.Cause(expired), context.DeadlineExceeded) {
		t.Fatalf("deadline cause = %v", context.Cause(expired))
	}
	// And a zero bound binds nothing at all: a run persisted before this
	// budget existed keeps its previous behaviour exactly.
	unbounded, none := withProviderInactivity(context.Background(), 0, nil)
	defer none()
	if providerInactivityLimit(unbounded) != 0 {
		t.Fatal("an absent bound resolved to a window")
	}
}

// TestObservedOutputReachesTheDurableProgressRecorder closes the loop between
// the two halves of the policy: the in-process watch that keeps a talking
// provider alive, and the durable note that lets status and a restart know when
// it last spoke.
//
// The recorder is supplied by the CALLER through the context, never by the
// adapter, because only the caller knows what durable operation state exists.
// An invocation whose caller keeps none - the planner, a probe - is still
// bounded; it simply records nothing.
func TestObservedOutputReachesTheDurableProgressRecorder(t *testing.T) {
	provider, request, _ := inactivityFixture(t,
		"i=0\nwhile [ $i -lt 8 ]; do echo working; sleep 0.05; i=$((i+1)); done\n")
	// A short window so the throttle - a quarter of it - lets several
	// observations through inside one invocation.
	request.Budgets.InactivityLimit = 200 * time.Millisecond

	var mu sync.Mutex
	var keys []string
	ctx := withProviderProgressRecorder(context.Background(), func(key string) {
		mu.Lock()
		defer mu.Unlock()
		keys = append(keys, key)
	})
	if _, err := provider.Execute(ctx, request); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(keys) == 0 {
		t.Fatal("output arrived and nothing durable recorded that the work was moving")
	}
	// The key is a FINGERPRINT that only advances when the provider said
	// something new, which is what stops "we asked again" from counting as
	// progress. It is never the same value twice in a row.
	for i := 1; i < len(keys); i++ {
		if keys[i] == keys[i-1] {
			t.Fatalf("the progress fingerprint repeated: %v", keys)
		}
	}
}

// ---------------------------------------------------------------------------
// The classification trust boundary, end to end
// ---------------------------------------------------------------------------

// shellQuoted makes one string safe inside a single-quoted shell word.
func shellQuoted(text string) string {
	return "'" + strings.ReplaceAll(text, "'", `'\''`) + "'"
}

// TestSessionOutputCannotCreateAnExternalProviderWait is the negative
// regression review 5246735510 asked for, driven through a real process so the
// two streams are the real two streams.
//
// The provider writes a recognized transport phrase into its SESSION OUTPUT -
// which is what a model quoting an error, a test log, or a documentation
// excerpt looks like from here - and then fails for an unrelated reason. The
// run must not be parked as execution_provider_unavailable, because nothing
// about the provider's transport failed and the accounting transition that
// class causes would be false.
func TestSessionOutputCannotCreateAnExternalProviderWait(t *testing.T) {
	for name, session := range sessionQuotingTransportPhrases {
		t.Run(name, func(t *testing.T) {
			// stdout carries the session; stderr carries the real, unrelated
			// failure; the process exits non-zero, so this is a genuinely
			// failed invocation rather than one nobody classifies.
			provider, request, _ := inactivityFixture(t,
				"echo "+shellQuoted(session)+"\n"+
					"echo 'error: the patch did not apply' >&2\n"+
					"exit 2\n")

			result, err := provider.Execute(context.Background(), request)
			if err == nil {
				t.Fatal("a non-zero exit returned no error")
			}
			if result.Failure == nil {
				t.Fatal("a failed invocation recorded no failure")
			}
			switch result.Failure.Classification {
			case FailureProviderUnavailable, FailureProviderQuota, FailureProviderRateLimited, FailureProviderAccountUnavailable:
				t.Fatalf("session output asserted the provider condition %q; a transcript is evidence, not an assertion",
					result.Failure.Classification)
			}
			// UNKNOWN IS THE CORRECT ANSWER and it fails closed: the run stops
			// for a human rather than being parked on an external condition
			// that does not exist.
			if result.Failure.Classification != FailureUnknown {
				t.Fatalf("classification = %q, want the fail-closed %q", result.Failure.Classification, FailureUnknown)
			}
			if RouteFailure(result.Failure.Classification) == RouteWait {
				t.Fatal("untrusted session output routed the run to an external wait")
			}
			// The bytes are still EVIDENCE. Excluding them from classification
			// must not exclude them from the transcript.
			raw, readErr := os.ReadFile(result.Artifacts[0].Path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !strings.Contains(string(raw), strings.SplitN(session, "\n", 2)[0]) {
				t.Fatal("the session output was dropped from the attempt transcript")
			}
		})
	}
}

// TestAGenuineTransportDiagnosticStillReachesABoundedWait is the other half:
// narrowing the surface must not have deleted the capability.
func TestAGenuineTransportDiagnosticStillReachesABoundedWait(t *testing.T) {
	provider, request, _ := inactivityFixture(t,
		"echo 'Reading the repository'\n"+
			"echo 'ERROR: error sending request for url (https://chatgpt.com/backend-api/codex/responses): dns error' >&2\n"+
			"exit 1\n")

	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("a non-zero exit returned no error")
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderUnavailable {
		t.Fatalf("failure = %#v, want %q from the provider's own terminal diagnostic", result.Failure, FailureProviderUnavailable)
	}
	if RouteFailure(result.Failure.Classification) != RouteWait {
		t.Fatal("a genuine transport failure no longer waits")
	}
}

// ---------------------------------------------------------------------------
// Precedence: a stated condition outranks silence
// ---------------------------------------------------------------------------

// TestARecognizedConditionSurvivesTheInactivityTermination is #238's
// distinction between an explicit provider diagnostic and mere silence.
//
// Each provider here states its condition on its own diagnostic stream and then
// hangs, so the inactivity policy is what ends the process. Both facts are
// durable and they say different things: TerminationCause records that policy
// ended it, and the CLASSIFICATION records what the provider said was wrong.
// Replacing the second with the first would send an operator to investigate a
// stalled provider when their allowance had simply run out - and would spend a
// retry on a condition no retry can clear.
func TestARecognizedConditionSurvivesTheInactivityTermination(t *testing.T) {
	for name, tc := range map[string]struct {
		diagnostic string
		want       FailureClass
		route      FailureRoute
	}{
		"quota then silence":       {"ERROR: You've hit your usage limit.", FailureProviderQuota, RouteWait},
		"unavailable then silence": {"ERROR: error sending request: dns error", FailureProviderUnavailable, RouteWait},
		"account then silence":     {"ERROR: your refresh token was revoked", FailureProviderAccountUnavailable, RouteWait},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, _ := inactivityFixture(t,
				"trap '' TERM\n"+
					"echo "+shellQuoted(tc.diagnostic)+" >&2\n"+
					"while :; do sleep 30; done\n")
			request.Budgets.InactivityLimit = inactivityWindow

			result, err := provider.Execute(context.Background(), request)
			if err == nil {
				t.Fatal("the hung provider was not terminated")
			}
			// The policy DID end it, and that stays recorded.
			if result.Invocation == nil || result.Invocation.TerminationCause != "provider_inactivity_limit_reached" {
				t.Fatalf("the inactivity policy is not recorded as the terminator: %#v", result.Invocation)
			}
			// And the CONDITION is the one the provider stated.
			if result.Failure == nil || result.Failure.Classification != tc.want {
				t.Fatalf("classification = %#v, want the stated %q", result.Failure, tc.want)
			}
			if got := RouteFailure(result.Failure.Classification); got != tc.route {
				t.Fatalf("route = %q, want %q", got, tc.route)
			}
		})
	}
}

// TestSilenceWithNothingStatedIsStillNoProgress keeps the preservation above
// from swallowing the rule it qualifies, and proves the preservation is not
// reachable from untrusted text.
func TestSilenceWithNothingStatedIsStillNoProgress(t *testing.T) {
	// A provider that says nothing recognizable and hangs.
	provider, request, _ := inactivityFixture(t,
		"trap '' TERM\necho 'thinking' >&2\nwhile :; do sleep 30; done\n")
	request.Budgets.InactivityLimit = inactivityWindow
	result, err := provider.Execute(context.Background(), request)
	if err == nil {
		t.Fatal("the hung provider was not terminated")
	}
	if result.Failure == nil || result.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("failure = %#v, want %q", result.Failure, FailureProviderNoProgress)
	}

	// A provider whose SESSION quotes a quota phrase and then hangs is still a
	// stall. Preserving a recognized condition must not become a channel for
	// untrusted output to outrank a bound the runtime actually enforced.
	quoted, request2, _ := inactivityFixture(t,
		"trap '' TERM\necho 'The docs say: usage limit reached'\nwhile :; do sleep 30; done\n")
	request2.Budgets.InactivityLimit = inactivityWindow
	result2, err := quoted.Execute(context.Background(), request2)
	if err == nil {
		t.Fatal("the hung provider was not terminated")
	}
	if result2.Failure == nil || result2.Failure.Classification != FailureProviderNoProgress {
		t.Fatalf("session output outranked the inactivity bound: %#v", result2.Failure)
	}
}

// ---------------------------------------------------------------------------
// The completion/expiry boundary
// ---------------------------------------------------------------------------

// TestCompletionBeatsTheInactivityTimerDeterministically is the race-boundary
// regression, and it is deterministic rather than probabilistic.
//
// The property under test is not "the timer usually loses". It is: ONCE
// COMPLETION HAS BEEN RECORDED, NO CANCELLATION CAN EVER BE PUBLISHED. The
// caller reads context.Cause after the executor returns, so anything a watcher
// can still do after that point turns a natural exit into a stall.
//
// The old shape failed this exactly: watch() computed `remaining` and cancelled
// BEFORE looking at its stop channel, so a watcher scheduled after completion
// with an already-expired window cancelled unconditionally - and stop() was a
// bare channel close that never waited for the watcher at all.
func TestCompletionBeatsTheInactivityTimerDeterministically(t *testing.T) {
	// A window that is already expired the instant the watcher runs, so the
	// timer branch is permanently ready and there is nothing to wait for.
	ctx, release := withProviderInactivity(context.Background(), time.Nanosecond, nil)
	defer release()
	watch := armInactivityWatch(ctx)
	if watch == nil {
		t.Fatal("no watch was armed")
	}
	time.Sleep(time.Millisecond) // the window is now unambiguously past
	if watch.silent() < watch.policy.limit {
		t.Fatal("the window has not expired, so this proves nothing")
	}

	// THE PROCESS COMPLETED FIRST. This is exactly what the executor records
	// the moment runBoundedProcess returns, and nothing else has happened yet.
	watch.complete()
	// A watcher scheduled only now, with a permanently ready timer, must do
	// nothing at all. Under the old shape it computed its remaining window and
	// cancelled before ever looking at its stop channel.
	watch.watch()

	if providerInactivityCause(ctx) {
		t.Fatal("a watcher that ran after completion cancelled the invocation; a natural exit would be reported as provider_no_progress")
	}
	if ctx.Err() != nil {
		t.Fatalf("the completed invocation's context was cancelled: %v", ctx.Err())
	}
	// Recording completion is idempotent: the executor must be able to say it
	// on every path without coordinating.
	watch.complete()
	if providerInactivityCause(ctx) {
		t.Fatal("a repeated completion published a cancellation")
	}
}

// TestTheWatcherIsJoinedBeforeTheCallerClassifies proves the second half of the
// lifecycle: the stop the executor calls does not merely signal the watcher, it
// WAITS for it. Signalling alone leaves the watcher free to publish a
// cancellation after the executor has returned, which is after the caller has
// already read the cause.
func TestTheWatcherIsJoinedBeforeTheCallerClassifies(t *testing.T) {
	for round := 0; round < 200; round++ {
		ctx, release := withProviderInactivity(context.Background(), time.Nanosecond, nil)
		watch := armInactivityWatch(ctx)
		stop := watch.watchUntilComplete()
		// The watcher is racing a permanently expired window. Whatever it is
		// doing, stop must leave nothing that can still be running.
		stop()
		// THE WATCHER HAS ALREADY EXITED. It closes `stopped` on its way out,
		// so this receive is ready only if stop waited for it - which is the
		// difference between signalling and joining, and the difference
		// between a caller that may classify now and one that may not.
		select {
		case <-watch.stopped:
		default:
			t.Fatalf("round %d: stop returned while the watcher was still running; nothing joined it before the caller classifies", round)
		}
		cause := providerInactivityCause(ctx)
		for i := 0; i < 50; i++ {
			if providerInactivityCause(ctx) != cause {
				t.Fatalf("round %d: the cause changed after the watcher was joined", round)
			}
		}
		release()
	}
}

// TestANaturallyExitingProviderIsNeverReportedAsStalled drives the same
// boundary through the real executor, repeatedly.
//
// Every round arms a real watcher, runs a real process group to a natural exit,
// and joins the watcher through the executor's own path. The window is
// comfortably larger than the process runtime, so the ONLY way a round can
// report a stall is a watcher that was still able to act after completion -
// which is the defect. The primitive-level proof that completion wins is
// TestCompletionBeatsTheInactivityTimerDeterministically; this is the proof
// that the executor wires it up.
func TestANaturallyExitingProviderIsNeverReportedAsStalled(t *testing.T) {
	const rounds = 60
	for round := 0; round < rounds; round++ {
		// The provider speaks once and exits on its own. The window is short
		// enough that an armed watcher is doing real work in every round, and
		// long enough that a correct one never reaches its expiry.
		provider, request, _ := inactivityFixture(t, "echo working\nexit 0\n")
		request.Budgets.InactivityLimit = 5 * time.Second

		result, err := provider.Execute(context.Background(), request)
		if err != nil {
			t.Fatalf("round %d: a provider that exited cleanly failed: %v", round, err)
		}
		if result.Invocation == nil {
			t.Fatalf("round %d: no provenance", round)
		}
		if result.Invocation.TerminationCause != "provider_returned" {
			t.Fatalf("round %d: termination cause = %q, want the provider's own return - a natural exit was attributed to the inactivity policy",
				round, result.Invocation.TerminationCause)
		}
		if result.Failure != nil {
			t.Fatalf("round %d: a clean exit recorded failure %#v", round, result.Failure)
		}
	}
}
