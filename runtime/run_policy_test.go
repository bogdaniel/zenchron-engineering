package runtime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// frozenPolicy states every category R member, so each one is checked.
var frozenPolicy = RunBudgets{
	WallLimit:                 time.Hour,
	AttemptWallLimit:          10 * time.Minute,
	LifecycleDeadline:         2 * time.Hour,
	MaxExecutionAttempts:      2,
	MaxExecutionContinuations: 3,
	MaxRemediationAttempts:    2,
	MaxAssuranceAttempts:      2,
	ProviderInactivityLimit:   5 * time.Minute,
}

// INVARIANT (ADR-0003 B1): every RunPolicy member is frozen at run creation. A
// configuration edit in EITHER direction, followed by a restart, changes
// neither the budgets the run is judged by, nor the ceilings its next
// operations freeze, nor what status reports, nor its RunPolicyDigest.
func TestEveryRunPolicyMemberIsFrozenAtCreation(t *testing.T) {
	fixture, _ := wallFixture(t, frozenPolicy)
	runID := fixture.start()
	created, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if created.RunPolicy.Source != RunPolicyRecorded || !isSHA256Hex(created.RunPolicy.SHA256) {
		t.Fatalf("a new run did not record its RunPolicyDigest in genesis: %+v", created.RunPolicy)
	}
	want := fixture.state(runID).budgets()
	if want != frozenPolicy {
		t.Fatalf("the run froze %+v, want %+v", want, frozenPolicy)
	}
	wider := RunBudgets{WallLimit: 5 * time.Hour, AttemptWallLimit: time.Hour, LifecycleDeadline: 10 * time.Hour,
		MaxExecutionAttempts: 9, MaxExecutionContinuations: 9, MaxRemediationAttempts: 9, MaxAssuranceAttempts: 9,
		ProviderInactivityLimit: time.Hour}
	narrower := RunBudgets{WallLimit: time.Minute, AttemptWallLimit: time.Minute, LifecycleDeadline: time.Minute,
		MaxExecutionAttempts: 1, MaxExecutionContinuations: 1, MaxRemediationAttempts: 1, MaxAssuranceAttempts: 1,
		ProviderInactivityLimit: time.Minute}
	for name, edited := range map[string]RunBudgets{"wider": wider, "narrower": narrower} {
		deps := fixture.deps
		deps.Budgets = edited
		fixture.runtime = fixture.newRuntime(deps) // a restart under the edited configuration
		state := fixture.state(runID)
		if got := state.budgets(); got != frozenPolicy {
			t.Errorf("%s edit: the run reads %+v, want its frozen %+v", name, got, frozenPolicy)
		}
		for kind, ceiling := range map[string]int{
			OpExecutionInvoke: 2, OpRemediationGofmt: 2, OpAssuranceGo: 2, OpAssuranceSemantic: 2,
		} {
			if got := state.attemptsFor(kind); got != ceiling {
				t.Errorf("%s edit: a new %s operation freezes %d attempts, want the run's %d", name, kind, got, ceiling)
			}
		}
		status, err := fixture.runtime.Status(runID)
		if err != nil {
			t.Fatal(err)
		}
		if status.Budgets != frozenPolicy {
			t.Errorf("%s edit: status reports %+v, want the run's frozen %+v", name, status.Budgets, frozenPolicy)
		}
		if status.RunPolicy != created.RunPolicy {
			t.Errorf("%s edit: the RunPolicyDigest moved across a restart: %+v, was %+v", name, status.RunPolicy, created.RunPolicy)
		}
	}
}

// INVARIANT: the lifecycle deadline is the frozen one. A config edit neither
// shortens a live run's calendar bound nor removes it.
func TestTheLifecycleDeadlineIsReadFrozen(t *testing.T) {
	for _, tc := range []struct {
		name           string
		frozen, edited time.Duration
		elapsed        time.Duration
		wantExhausted  bool
	}{
		{"a narrower edit does not shorten it", 2 * time.Hour, time.Minute, 10 * time.Minute, false},
		{"removing it does not widen it", 5 * time.Minute, 0, 10 * time.Minute, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			budgets := frozenPolicy
			budgets.LifecycleDeadline = tc.frozen
			fixture, _ := wallFixture(t, budgets)
			runID := fixture.start()
			deps := fixture.deps
			deps.Budgets.LifecycleDeadline = tc.edited
			fixture.runtime = fixture.newRuntime(deps)
			fixture.clock.advance(tc.elapsed)
			_, reason := fixture.state(runID).conditions()
			if exhausted := reason == "run_lifecycle_deadline_exhausted"; exhausted != tc.wantExhausted {
				t.Fatalf("frozen %s, configured %s, after %s: reason %q, want exhausted=%t",
					tc.frozen, tc.edited, tc.elapsed, reason, tc.wantExhausted)
			}
		})
	}
}

// INVARIANT: the RunPolicyDigest is one stable identity over the frozen
// policy. Identical policies have identical digests, a different policy has a
// different one, and the recorded digest is exactly the digest of the durable
// run facts - there is no second source to disagree with.
func TestTheRunPolicyDigestIsOneStableIdentity(t *testing.T) {
	digest := func(budgets RunBudgets) (string, *phase8Fixture, string) {
		fixture, _ := wallFixture(t, budgets)
		runID := fixture.start()
		status, err := fixture.runtime.Status(runID)
		if err != nil {
			t.Fatal(err)
		}
		return status.RunPolicy.SHA256, fixture, runID
	}
	first, fixture, runID := digest(frozenPolicy)
	second, _, _ := digest(frozenPolicy)
	if first != second {
		t.Fatalf("identical policies recorded different digests: %s vs %s", first, second)
	}
	other := frozenPolicy
	other.MaxAssuranceAttempts = 3
	if third, _, _ := digest(other); third == first {
		t.Fatal("a different policy recorded the same digest")
	}
	state := fixture.state(runID)
	recomputed, err := state.run.policy(fixture.deps.ConfigDigest.Repository).Digest()
	if err != nil {
		t.Fatal(err)
	}
	if recomputed != first {
		t.Fatalf("the recorded digest %s is not the digest of the run's own frozen facts %s", first, recomputed)
	}
	// The genesis payload is additive: it still passes its own strict schema,
	// and a pre-B1 bare-ControllerBuild genesis still reads back its build.
	for _, event := range state.events {
		if event.Type == EventRunCreated {
			if err := validateEventPayload(event); err != nil {
				t.Fatalf("the recorded genesis fails its own schema: %v", err)
			}
		}
	}
	legacyBuild := attestedBuild(ControllerAdopted, "rev", "tree", strings.Repeat("ab", 32))
	raw, _ := json.Marshal(legacyBuild)
	legacy := &runState{events: []EngineeringEvent{{Type: EventRunCreated, Payload: raw}}}
	if got := legacy.recordedControllerBuild(); got != legacyBuild {
		t.Fatalf("a pre-B1 genesis read back %+v, want %+v", got, legacyBuild)
	}
	if err := validateEventPayload(EngineeringEvent{Type: EventRunCreated, Payload: raw}); err != nil {
		t.Fatalf("a pre-B1 genesis payload no longer validates: %v", err)
	}
}

// INVARIANT (ADR-0003 §5): a legacy run keeps a defined interpretation and is
// never handed a wider value. A member with a frozen value is read exactly; a
// member it predates is the value its controller binding identifies; a member
// whose zero is a frozen meaning keeps that meaning.
func TestTheLegacyRunPolicyRule(t *testing.T) {
	live := RunBudgets{WallLimit: 10 * time.Minute, LifecycleDeadline: time.Hour, MaxExecutionAttempts: 5,
		MaxRemediationAttempts: 4, MaxAssuranceAttempts: 3, ProviderInactivityLimit: 7 * time.Minute,
		AttemptWallLimit: 2 * time.Minute, MaxProviderInvocations: 6}
	rt := &EngineeringRuntime{deps: Dependencies{Budgets: live}}

	t.Run("persisted run.Budgets, members that predate their fields", func(t *testing.T) {
		persisted := RunBudgets{WallLimit: 30 * time.Minute, MaxExecutionAttempts: 1}
		state := &runState{rt: rt, run: EngineeringRun{ID: "legacy", Budgets: &persisted}}
		got := state.budgets()
		want := RunBudgets{
			WallLimit:            30 * time.Minute, // exact, though the configuration is narrower
			MaxExecutionAttempts: 1,                // exact
			// Predate their fields: the binding's value.
			MaxRemediationAttempts: 4, MaxAssuranceAttempts: 3, ProviderInactivityLimit: 7 * time.Minute,
			// The pre-#54 continuation rule: the run's own attempt budget.
			MaxExecutionContinuations: 1,
			// Zero IS the frozen meaning: no lifecycle deadline, unbounded
			// invocations, the pre-#328 attempt rule. Never the live value.
		}
		if got != want {
			t.Fatalf("legacy run budgets\n got %+v\nwant %+v", got, want)
		}
		policy := state.runPolicy()
		derived, _ := state.run.policy("").Digest()
		if policy.Source != RunPolicyLegacyRunBudgets || policy.SHA256 != derived || policy.RepositoryConfig != "" {
			t.Fatalf("a pre-B1 run with budgets reported %+v, want a derived legacy_run_budgets digest", policy)
		}
	})

	t.Run("nil run.Budgets", func(t *testing.T) {
		state := &runState{rt: rt, run: EngineeringRun{ID: "oldest"}}
		got := state.budgets()
		want := live.defaults()
		want.AttemptWallLimit, want.MaxProviderInvocations = 0, 0
		want.MaxExecutionContinuations = live.MaxExecutionAttempts
		if got != want {
			t.Fatalf("nil-budget run\n got %+v\nwant %+v", got, want)
		}
		if policy := state.runPolicy(); policy.Source != RunPolicyLegacyControllerBinding || policy.SHA256 != "" {
			t.Fatalf("a nil-budget run claimed a RunPolicyDigest it never had: %+v", policy)
		}
	})
}

// INVARIANT: a configuration edit that is also a controller change cannot
// widen a live run. The new controller parks it controller_changed before
// planning anything, and its frozen policy is unchanged - which is also why
// the legacy fallback above only ever runs under the binding that created the
// run.
func TestAConfigEditUnderAChangedControllerDoesNotWidenALiveRun(t *testing.T) {
	fixture, provider := wallFixture(t, frozenPolicy)
	runID := fixture.start()
	deps := fixture.deps
	deps.ConfigDigest.Global = "g2"
	deps.Budgets = RunBudgets{WallLimit: 5 * time.Hour, MaxExecutionAttempts: 9, MaxRemediationAttempts: 9,
		MaxAssuranceAttempts: 9, ProviderInactivityLimit: time.Hour}
	fixture.runtime = fixture.newRuntime(deps)
	outcome, err := fixture.runtime.Reconcile(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Reason != "controller_changed" || len(provider.requests) != 0 {
		t.Fatalf("a changed controller acted on the run: %#v, %d invocations", outcome, len(provider.requests))
	}
	if got := fixture.state(runID).budgets(); got != frozenPolicy {
		t.Fatalf("the changed controller widened the run to %+v, want %+v", got, frozenPolicy)
	}
}
