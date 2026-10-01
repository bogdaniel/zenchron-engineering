package runtime

// transient_connectivity_test.go pins the #380 dogfood regression: a
// recognized transient connectivity failure during source.observe must enter
// DURABLE BOUNDED BACKOFF, not merely a single bounded wait.
//
// In the failed run, a DNS/network-unreachable failure under
// source.observe#epoch-37 consumed attempts 1, 2 and 3 in the same second and
// the run terminalized as source.observe_attempts_exhausted, because the
// handler's failed(err) recorded no FailureClass at all - the same unrouted
// shape retry_routing_test.go's #29 regression exercises for execution.invoke,
// reached here through a different operation.
//
// Recognizing the condition (FailureForgeUnavailable, adapters.go/github.go/
// github_rest.go) is only half of #380. The other half, pinned here, is that
// a recognized condition must not be left eligible to retry the instant its
// attempt is restored: it needs a durable, restart-safe retry instant
// (TestSourceObserveConnectivityWaitBlocksRetryUntilBackoffElapses,
// TestSourceObserveConnectivityWaitSurvivesControllerRestart), a bound on how
// many times it may recur before the run stops truthfully rather than waiting
// forever (TestSourceObserveConnectivityWaitExhaustsAfterOwnAttemptCeiling),
// and the #83 guarantee that none of this costs the run's active-work budget
// (TestSourceObserveConnectivityWaitExcludesTimeFromActiveWork).
//
// A NOTE ON EPOCHS, because every test below is shaped by it. plan()'s
// idempotency key for source.observe is "epoch-<N>" (epochKey, bindSourceObserve)
// and recordDisposition appends a new non-operation event - which is what
// advances the epoch - ONLY when the run's disposition or reason actually
// CHANGES. So the very FIRST connectivity failure always opens a new epoch (the
// run was Active and is now Waiting), and that first failed attempt is never
// retried again under that identity. Every SUBSEQUENT failure that keeps
// recording the SAME wait reason does NOT advance the epoch - recordDisposition
// sees no change - so the operation planned for that stable epoch is the SAME
// durable operation across every following pass, and it is THAT operation
// whose streak, backoff and exhaustion these tests exercise. This is exactly
// the shape of "at most one attempt per Reconcile" the reviewer observed
// already held true for a single failure; the durable gate is what the second
// and later repeats need.

import (
	"testing"
)

// TestSourceObserveConnectivityWaitBlocksRetryUntilBackoffElapses is the
// durable half of #380's invariant: a pass INSIDE the backoff window a prior
// connectivity failure opened must make no network call and spend no
// attempt. Once the window elapses, an ordinary pass resumes and the run
// recovers.
func TestSourceObserveConnectivityWaitBlocksRetryUntilBackoffElapses(t *testing.T) {
	fixture := newPhase8Fixture(t)
	failing := 2
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" && failing > 0 {
			failing--
			return &GitHubTransientError{Detail: "github endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()

	// Pass 1: the run's first failure, which opens the wait (and the epoch it
	// stabilizes at).
	first := fixture.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "source_forge_unavailable" {
		t.Fatalf("first outcome = %#v, want waiting/source_forge_unavailable", first)
	}

	// Pass 2: the SAME wait reason repeats, so the epoch does not move again -
	// this is the first real attempt of the durable operation the rest of this
	// test exercises, and it is what opens its bounded backoff.
	second := fixture.reconcile(runID)
	if second.Disposition != Waiting || second.Reason != "source_forge_unavailable" {
		t.Fatalf("second outcome = %#v, want still waiting on the forge", second)
	}
	calls := countMethod(fixture.forge.Calls, "Issue")
	if calls != 2 {
		t.Fatalf("forge called %d times for the two real failures, want exactly 2", calls)
	}

	// Pass 3: immediately again, well inside the backoff window pass 2 opened.
	// This is the exact shape of a fast serve loop hammering the run.
	third := fixture.reconcile(runID)
	if third.Disposition != Waiting || third.Reason != "source_forge_unavailable" {
		t.Fatalf("third outcome = %#v, want still waiting on the forge", third)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got != calls {
		t.Fatalf("forge called %d times after an immediate third pass, want still %d: a pass inside the backoff window must make no network call", got, calls)
	}

	// Once the window elapses, the gate passes and - the forge's scripted
	// failures already spent - the run resumes (and, once unblocked, replans
	// source.observe again at every later epoch exactly as it always does, so
	// the forge is asked for the issue more than once from here on).
	fixture.clock.advance(connectivityWaitMax)
	fourth := fixture.reconcile(runID)
	if fourth.Disposition == Failed {
		t.Fatalf("fourth outcome = %#v, want the run to recover once the forge answers", fourth)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got <= calls {
		t.Fatalf("forge called %d times once the backoff elapsed, want more than %d: the gate must not still be blocking a real attempt", got, calls)
	}
}

// TestSourceObserveConnectivityWaitSurvivesControllerRestart is #380's
// explicit restart requirement: "a controller restart cannot erase it". The
// retry instant is read from the same durable journal every other replayed
// fact comes from, so a FRESH runtime bound to the same store - a restart,
// with no in-memory timer of its own - must see the identical instant rather
// than retrying immediately.
func TestSourceObserveConnectivityWaitSurvivesControllerRestart(t *testing.T) {
	fixture := newPhase8Fixture(t)
	failing := 2
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" && failing > 0 {
			failing--
			return &GitHubTransientError{Detail: "github endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()

	first := fixture.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "source_forge_unavailable" {
		t.Fatalf("first outcome = %#v, want waiting/source_forge_unavailable", first)
	}
	second := fixture.reconcile(runID)
	if second.Disposition != Waiting || second.Reason != "source_forge_unavailable" {
		t.Fatalf("second outcome = %#v, want still waiting on the forge", second)
	}
	calls := countMethod(fixture.forge.Calls, "Issue")

	// Simulate the restart: a brand-new EngineeringRuntime bound to the same
	// durable store, exactly as a replacement controller process would be.
	// Unlike controller_reconcile_loop.go's OWN self-maintenance backoff -
	// which a restart is explicitly allowed to forget, because it is
	// operational convenience rather than authority - this retry instant
	// names a GOVERNED run's state and must not reset.
	fixture.runtime = fixture.newRuntime(fixture.deps)

	third := fixture.reconcile(runID)
	if third.Disposition != Waiting || third.Reason != "source_forge_unavailable" {
		t.Fatalf("third outcome (post-restart) = %#v, want still waiting on the forge", third)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got != calls {
		t.Fatalf("forge called %d times after a restart inside the backoff window, want still %d: a restart must not erase the wait", got, calls)
	}

	fixture.clock.advance(connectivityWaitMax)
	fourth := fixture.reconcile(runID)
	if fourth.Disposition == Failed {
		t.Fatalf("fourth outcome = %#v, want the run to recover once the forge answers", fourth)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got <= calls {
		t.Fatalf("forge called %d times once the backoff elapsed post-restart, want more than %d: the gate must not still be blocking a real attempt", got, calls)
	}
}

// TestSourceObserveConnectivityWaitExhaustsAfterOwnAttemptCeiling is #380's
// finite-authority requirement: this is not daemon-style infinite retry. A
// connectivity failure that recurs more times in a row than the operation's
// OWN attempt ceiling allows (existing authority - attemptsFor's default of 3
// for source.observe, the same number the live incident's "attempts 1, 2 and
// 3" names) - each time running out a full bounded backoff and never once
// seeing the forge answer - stops waiting and fails truthfully through the
// SAME attempts-exhausted settle every other operation uses, rather than
// waiting on a condition that has shown no sign of clearing.
func TestSourceObserveConnectivityWaitExhaustsAfterOwnAttemptCeiling(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" {
			return &GitHubTransientError{Detail: "github endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()

	// Pass 1 opens the wait (and its stable epoch). Passes 2-4 are the SAME
	// durable operation's streak reaching 1, 2 and 3 - still within
	// source.observe's own attempt ceiling, so each still waits.
	first := fixture.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "source_forge_unavailable" {
		t.Fatalf("pass 1 outcome = %#v, want waiting/source_forge_unavailable", first)
	}
	for i := 0; i < 3; i++ {
		fixture.clock.advance(connectivityWaitMax)
		outcome := fixture.reconcile(runID)
		if outcome.Disposition != Waiting || outcome.Reason != "source_forge_unavailable" {
			t.Fatalf("pass %d outcome = %#v, want waiting/source_forge_unavailable", i+2, outcome)
		}
	}
	// Pass 5: the streak now exceeds the ceiling. The connectivity failure has
	// recurred more times in a row than source.observe is ever allowed to
	// attempt, each time running a full bounded backoff and never once seeing
	// the forge answer - #380 requires this to stop truthfully.
	fixture.clock.advance(connectivityWaitMax)
	final := fixture.reconcile(runID)
	wantReason := OpSourceObserve + attemptsExhaustedSuffix
	if final.Disposition != Failed || final.Reason != wantReason {
		t.Fatalf("final outcome = %#v, want failed/%s: a connectivity failure that never clears must exhaust, not wait forever", final, wantReason)
	}
	if got := countMethod(fixture.forge.Calls, "Issue"); got != 5 {
		t.Fatalf("forge was asked %d times before exhaustion, want exactly 5 (the opening failure, 3 bounded waits, and the attempt that exceeded the ceiling)", got)
	}
}

// TestSourceObserveConnectivityWaitExcludesTimeFromActiveWork is #83's
// guarantee applied to #380: time spent waiting for the forge to become
// reachable is an external availability condition, not engineering work, and
// must not be charged to the run's active-work budget - however long the
// backoff runs for. It also pins that the wait makes no progress of its own:
// the run's identity is exactly what it was before the forge went
// unreachable, and no held material is fabricated for a run that has not yet
// produced anything to hold.
func TestSourceObserveConnectivityWaitExcludesTimeFromActiveWork(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" {
			return &GitHubTransientError{Detail: "github endpoint could not be reached"}
		}
		return nil
	})
	runID := fixture.start()

	first := fixture.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != "source_forge_unavailable" {
		t.Fatalf("first outcome = %#v, want waiting/source_forge_unavailable", first)
	}
	before := fixture.state(runID)

	// A long external wait - well past the backoff ceiling - must still cost
	// the run's active-work budget nothing.
	advance := 3 * connectivityWaitMax
	fixture.clock.advance(advance)
	second := fixture.reconcile(runID)
	if second.Disposition != Waiting || second.Reason != "source_forge_unavailable" {
		t.Fatalf("second outcome = %#v, want still waiting on the forge", second)
	}
	after := fixture.state(runID)
	if active := ActiveElapsed(after.run, after.events, fixture.clock.Now()); active >= connectivityWaitMax {
		t.Fatalf("active-work elapsed is %s after a %s connectivity wait, want it excluded (#83)", active, advance)
	}

	// No progress of its own: the run's identity is unchanged, and a run that
	// has not yet produced a candidate holds nothing.
	if after.projection.Contract != before.projection.Contract || after.projection.CandidateRevision != before.projection.CandidateRevision {
		t.Fatalf("run identity moved across a connectivity wait: before=%+v after=%+v", before.projection, after.projection)
	}
	if after.snapshot.HeldMaterial != nil {
		t.Fatalf("a connectivity wait fabricated held material: %+v", after.snapshot.HeldMaterial)
	}
}

// TestSourceObserveUnrecognizedFailureStaysUnrouted proves the fix is narrow:
// an error the forge boundary has not been told to recognize as transient
// connectivity keeps today's behaviour - an unclassified failure under
// budget-only retry, exhausting its attempts inside one pass - rather than
// being guessed into the connectivity wait. This is the fail-closed half of
// #380: only a RECOGNIZED condition gets the durable wait.
func TestSourceObserveUnrecognizedFailureStaysUnrouted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.inject(func(call GitHubCall) error {
		if call.Method == "Issue" {
			return &GitHubAPIError{Status: 422, Detail: "the forge rejected the request"}
		}
		return nil
	})
	runID := fixture.start()
	outcome := fixture.reconcile(runID)

	if outcome.Disposition != Failed || outcome.Reason != OpSourceObserve+"_attempts_exhausted" {
		t.Fatalf("outcome = %#v, want the pre-existing budget-only-retry behaviour (attempts exhausted in one pass)", outcome)
	}

	observed := soleSourceObserveOperation(t, fixture.state(runID))
	if class := durableFailureClass(t, observed); class != "" {
		t.Fatalf("durable failure class = %q, want unclassified for an unrecognized forge fault", class)
	}
}

// currentSourceObserveOperation reads back the run's source.observe operation
// planned under its CURRENT epoch - the same key bindSourceObserve computes
// and the one connectivityWaitPending's caller leases - rather than any
// earlier epoch's now-orphaned attempt. It is only meaningful while the run is
// still parked in the SAME wait: a terminal settle (run.failed, run.completed)
// is itself an epoch-advancing event, so it must never be used to look up the
// operation that produced that very terminal outcome.
func currentSourceObserveOperation(t *testing.T, state *runState) RunOperation {
	t.Helper()
	op, ok := state.operationByKey(OpSourceObserve, state.epochKey())
	if !ok {
		t.Fatal("run has no source.observe operation under its current epoch")
	}
	return op
}

// soleSourceObserveOperation reads back the run's one and only source.observe
// operation, for a test whose failure never opens a wait and so never advances
// the epoch beyond the one the operation was itself planned under.
func soleSourceObserveOperation(t *testing.T, state *runState) RunOperation {
	t.Helper()
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpSourceObserve {
			return op
		}
	}
	t.Fatal("run has no source.observe operation")
	return RunOperation{}
}
