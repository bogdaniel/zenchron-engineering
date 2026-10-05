package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// configChange is the #58 shape: the same repository and store, a second
// engine whose configuration digest differs - the pre-#54 → post-#54 move.
func configChange(t *testing.T, fixture *phase8Fixture) *EngineeringRuntime {
	t.Helper()
	deps := fixture.deps
	deps.ConfigDigest = ConfigDigest{Global: "g2-after-change", Repository: "r2-after-change"}
	return fixture.newRuntime(deps)
}

func runsFor(t *testing.T, fixture *phase8Fixture, issue int) []EngineeringRun {
	t.Helper()
	runs, err := fixture.store.SourceRuns("acme/repo", issueGoal("acme/repo", issue))
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// settledUnder creates the source's run under the ORIGINAL configuration and
// settles it as the journal would record it.
func settledUnder(t *testing.T, fixture *phase8Fixture, issue int, disposition Disposition, reason string) string {
	t.Helper()
	runID, err := fixture.runtime.StartOrResumeIssueRun(context.Background(), issue)
	if err != nil {
		t.Fatal(err)
	}
	state := fixture.state(runID)
	if _, err := fixture.runtime.settle(state, disposition, reason); err != nil {
		t.Fatal(err)
	}
	return runID
}

// TestAConfigurationChangeDoesNotReEnrolSettledSources is #58's reproduction:
// two standing opted-in sources (#42, #49 in the original finding) whose runs
// finished under the previous configuration are NOT new work for a watcher
// running under the changed one - completed, cancelled and failed alike.
func TestAConfigurationChangeDoesNotReEnrolSettledSources(t *testing.T) {
	for _, terminal := range []struct {
		disposition Disposition
		reason      string
	}{
		{Completed, "merged"},
		{Cancelled, "operator_stop"},
		{Failed, "assurance.go_failure_not_retryable"},
	} {
		t.Run(string(terminal.disposition), func(t *testing.T) {
			fixture := newWatchFixture(t)
			for _, issue := range []int{42, 49} {
				optIn(fixture.forge, issue, time.Unix(1_700_000_000, 0).UTC())
				settledUnder(t, fixture, issue, terminal.disposition, terminal.reason)
			}
			changed := configChange(t, fixture)
			watcher := watchOver(t, fixture, []GitHubRepo{repoA},
				map[string]*EngineeringRuntime{repoA.String(): changed}, fixture.forge, nil)
			report := only(t, tick(t, watcher))
			for _, issue := range []int{42, 49} {
				if runs := runsFor(t, fixture, issue); len(runs) != 1 {
					t.Fatalf("issue %d has %d runs after a configuration change: the settled source was re-enrolled", issue, len(runs))
				}
				decision, err := changed.sourceDecision(issue)
				if err != nil || decision.State != SourceSettled || len(decision.History) != 1 {
					t.Fatalf("issue %d decision %+v (%v), want settled over its one historical run", issue, decision, err)
				}
			}
			if len(report.Driven) != 0 {
				t.Fatalf("watch drove %v after a configuration change", report.Driven)
			}
		})
	}
}

// TestLiveWorkUnderAnotherConfigurationIsNeverParalleled: a source whose live
// run belongs to another identity space is a typed boundary for discovery and
// for an ordinary start alike. An explicit new generation is still the
// operator's to ask for.
func TestLiveWorkUnderAnotherConfigurationIsNeverParalleled(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	live, err := fixture.runtime.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	changed := configChange(t, fixture)

	watcher := watchOver(t, fixture, []GitHubRepo{repoA},
		map[string]*EngineeringRuntime{repoA.String(): changed}, fixture.forge, nil)
	report := only(t, tick(t, watcher))
	if runs := runsFor(t, fixture, fixture.issue); len(runs) != 1 {
		t.Fatalf("discovery created a parallel run beside live %s: %d runs", live, len(runs))
	}
	if !strings.Contains(report.Detail, live) {
		t.Fatalf("the boundary was not surfaced: %q", report.Detail)
	}

	_, err = changed.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	var elsewhere *SourceLiveElsewhereError
	if !errors.As(err, &elsewhere) || elsewhere.RunID != live {
		t.Fatalf("an ordinary start under the changed configuration returned %v, want the boundary naming %s", err, live)
	}
	if runs := runsFor(t, fixture, fixture.issue); len(runs) != 1 {
		t.Fatal("the refused start created a run")
	}

	fresh, err := changed.StartIssueRun(context.Background(), fixture.issue, NewGeneration)
	if err != nil || fresh.RunID == live {
		t.Fatalf("an explicit new generation was refused or reused the live run: %+v %v", fresh, err)
	}
}

// TestAnUnseenSourceIsClaimedExactlyOnceAcrossConfigurations: only a source
// with no history at all is new, and two discovery claims under DIFFERENT
// configurations - which derive different run identities, so ClaimRun alone
// cannot collide them - still create exactly one run.
func TestAnUnseenSourceIsClaimedExactlyOnceAcrossConfigurations(t *testing.T) {
	fixture := newWatchFixture(t)
	changed := configChange(t, fixture)
	var wg sync.WaitGroup
	claims := make(chan bool, 16)
	for i := 0; i < 16; i++ {
		engine := fixture.runtime
		if i%2 == 1 {
			engine = changed
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, claimed, err := engine.claimUnseenSource(context.Background(), fixture.issue)
			if err != nil {
				t.Error(err)
			}
			claims <- claimed
		}()
	}
	wg.Wait()
	close(claims)
	if runs := runsFor(t, fixture, fixture.issue); len(runs) != 1 {
		t.Fatalf("%d runs for one unseen source", len(runs))
	}
	won := 0
	for claimed := range claims {
		if claimed {
			won++
		}
	}
	if won == 0 {
		t.Fatal("no claim created the run")
	}
	// Whoever won, the source is now seen by both identity spaces.
	for _, engine := range []*EngineeringRuntime{fixture.runtime, changed} {
		if decision, err := engine.sourceDecision(fixture.issue); err != nil || decision.State == SourceUnseen {
			t.Fatalf("after the claim the source reads %+v (%v)", decision, err)
		}
	}
}

// TestUnreadableSourceHistoryFailsClosed: history this process cannot replay
// is not history it may treat as absent.
func TestUnreadableSourceHistoryFailsClosed(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, fixture.issue, time.Unix(1_700_000_000, 0).UTC())
	runID := settledUnder(t, fixture, fixture.issue, Completed, "merged")
	if _, err := fixture.store.db.Exec(`UPDATE events SET event_hash = 'tampered' WHERE run_id = ? AND sequence = 1`, runID); err != nil {
		t.Fatal(err)
	}
	changed := configChange(t, fixture)
	if _, err := changed.sourceDecision(fixture.issue); err == nil {
		t.Fatal("an unreadable history was classified")
	}
	watcher := watchOver(t, fixture, []GitHubRepo{repoA},
		map[string]*EngineeringRuntime{repoA.String(): changed}, fixture.forge, nil)
	tick(t, watcher)
	if runs := runsFor(t, fixture, fixture.issue); len(runs) != 1 {
		t.Fatalf("unreadable history was treated as unseen: %d runs", len(runs))
	}
}

func TestClassifySource(t *testing.T) {
	live := SourceRun{Run: EngineeringRun{ID: "here"}, Disposition: Active}
	waiting := SourceRun{Run: EngineeringRun{ID: "there"}, Disposition: Waiting}
	done := SourceRun{Run: EngineeringRun{ID: "old"}, Disposition: Completed}
	current := map[string]bool{"here": true}
	for _, tc := range []struct {
		history []SourceRun
		want    SourceState
		run     string
	}{
		{nil, SourceUnseen, ""},
		{[]SourceRun{done}, SourceSettled, ""},
		{[]SourceRun{done, live}, SourceLive, "here"},
		{[]SourceRun{done, waiting}, SourceLiveElsewhere, "there"},
		// This space's live run is resumed even when another space also has one.
		{[]SourceRun{waiting, live}, SourceLive, "here"},
	} {
		got := classifySource(tc.history, current)
		if got.State != tc.want || got.RunID != tc.run {
			t.Errorf("%v: got %s %q, want %s %q", tc.history, got.State, got.RunID, tc.want, tc.run)
		}
	}
}
