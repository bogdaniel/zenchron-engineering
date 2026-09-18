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
