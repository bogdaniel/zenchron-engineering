package agentkernel

import (
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/execution"
)

// The host's inactivity bound has no kernel equivalent; the adapter enforces it
// from kernel progress and reports the existing class and owner. Whether an
// invocation exists depends on whether the provider was reached, so the two
// contracts are tested separately and neither depends on a wall-clock race
// between the limit and the adapter's own setup.

// assertInactivityFailure holds what every inactivity termination reports,
// whether or not the provider was reached.
func assertInactivityFailure(t *testing.T, res execution.Result, err error) {
	t.Helper()
	te := terminationOf(t, err)
	if te.Termination.Outcome != api.OutcomeCancelled || te.Termination.Cancellation != api.CancelUnknown {
		t.Errorf("kernel termination %+v", te.Termination)
	}
	if res.Outcome != execution.Failed || res.Failure == nil || res.Failure.Classification != execution.FailureProviderNoProgress {
		t.Errorf("result %+v, want a failed result classified %q", res, execution.FailureProviderNoProgress)
	}
}

// (a) The provider was reached and then went silent for the whole limit: the
// invocation exists and names inactivity as its owner.
func TestInactivityAfterProviderEntryNamesTheInactivityOwner(t *testing.T) {
	fc := newFakeClock()
	entered := make(chan struct{})
	provider := scripted.New(scripted.Step{Block: true, Entered: entered})
	a := newTestAdapter(t, testConfig(t, provider))
	a.clock = fc.clock()
	req := testRequest(t)
	req.Budgets.InactivityLimit = testLimit

	type outcome struct {
		res execution.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := a.Execute(t.Context(), req)
		done <- outcome{res, err}
	}()
	timer := fc.armed(t)
	select {
	case <-entered:
	case <-time.After(bounded):
		t.Fatal("the provider was never reached")
	}
	fc.advance(testLimit)
	timer.fire <- time.Time{}
	var got outcome
	select {
	case got = <-done:
	case <-time.After(bounded):
		t.Fatal("inactivity never ended the attempt")
	}

	assertInactivityFailure(t, got.res, got.err)
	inv := got.res.Invocation
	if inv == nil {
		t.Fatalf("the provider was reached, so the invocation must exist: %+v", got.res)
	}
	if inv.TerminationCause != string(execution.OwnerInactivity) || inv.InactivityLimit != testLimit {
		t.Errorf("invocation %+v, want owner %q and limit %v", inv, execution.OwnerInactivity, testLimit)
	}
}

// (b) Inactivity before the provider was reached: nothing ran, so there is no
// invocation to describe (as for any request nothing ran for), and the failure
// is still classified and attributed to inactivity.
func TestInactivityBeforeProviderEntryHasNoInvocation(t *testing.T) {
	a := newTestAdapter(t, testConfig(t, scripted.New()))
	req := testRequest(t)
	req.Budgets.InactivityLimit = time.Millisecond
	kreq, err := a.translate(req, time.Now(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	state := newAttempt(req.AttemptRef(), &transcript{}, nil, systemClock)
	kres := api.ExecutionResult{Termination: api.Termination{
		Outcome: api.OutcomeCancelled, Cause: api.CauseHostCancelled, Cancellation: api.CancelUnknown,
	}}
	res, err := a.result(req, kreq, kres, state, execution.ErrProviderInactive, time.Now(), time.Now())
	assertInactivityFailure(t, res, err)
	if res.Invocation != nil || res.Executed {
		t.Errorf("a provider that was never reached left an invocation or executed work: %+v", res)
	}
}

// Whichever side of provider entry a real expiry lands on, the result is
// internally consistent: an invocation exists exactly when the provider was
// called, and it names inactivity. The limit is a nanosecond, so the expiry
// races provider entry on every run; the assertion holds for either order.
func TestInactivityNeverContradictsWhetherTheProviderWasReached(t *testing.T) {
	for i := range 32 {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			provider := scripted.New(scripted.Step{Block: true})
			req := testRequest(t)
			req.Budgets.InactivityLimit = time.Nanosecond
			res, err := newTestAdapter(t, testConfig(t, provider)).Execute(t.Context(), req)
			assertInactivityFailure(t, res, err)
			reached := len(provider.Requests()) > 0
			switch {
			case reached && res.Invocation == nil:
				t.Errorf("the provider was called but no invocation was reported: %+v", res)
			case !reached && res.Invocation != nil:
				t.Errorf("the provider was never called but an invocation was reported: %+v", res.Invocation)
			case reached && res.Invocation.TerminationCause != string(execution.OwnerInactivity):
				t.Errorf("invocation owner %q, want %q", res.Invocation.TerminationCause, execution.OwnerInactivity)
			}
		})
	}
}
