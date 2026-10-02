package runtime

import (
	"context"
	"testing"
	"time"
)

// ConsoleFleet is FleetStatus's sibling, not a competing fleet: this asserts
// the two agree on the three load-bearing numbers (#63's "3 / 2 active" bug
// was exactly a disagreement like this) and on how many runs each reports.
func TestConsoleFleetMatchesFleetStatusCounts(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	if _, err := fixture.runtime.Reconcile(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	now := fixture.clock.Now()

	fleet, err := FleetStatus(fixture.store, fixture.stateDir, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ConsoleFleet(fixture.store, fixture.stateDir, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if view.Active != fleet.Active {
		t.Fatalf("console active = %d, fleet active = %d", view.Active, fleet.Active)
	}
	if view.Executing != fleet.Executing {
		t.Fatalf("console executing = %d, fleet executing = %d", view.Executing, fleet.Executing)
	}
	if len(view.Runs) != len(fleet.Runs) {
		t.Fatalf("console runs = %d, fleet runs = %d", len(view.Runs), len(fleet.Runs))
	}
	if len(view.Runs) == 0 {
		t.Fatal("HARNESS PRECONDITION: expected at least one run")
	}
	if view.Runs[0].Summary.RunID != fleet.Runs[0].RunID {
		t.Fatalf("console run order disagreed with fleet order: %q vs %q", view.Runs[0].Summary.RunID, fleet.Runs[0].RunID)
	}
}

// A run detail that cannot be found is reported as not-found, not as a zero
// value a caller might mistake for an empty-but-real run.
func TestConsoleRunDetailUnknownRun(t *testing.T) {
	fixture := newPhase8Fixture(t)
	detail, found, err := ConsoleRunDetail(fixture.store, fixture.stateDir, "does-not-exist", fixture.clock.Now())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Fatalf("expected not-found, got a detail: %+v", detail)
	}
}

// THE OVERLAY PREFERS THE LIVE ROW TO THE JOURNAL, exactly like
// EngineeringRuntime.Status's liveOperationRow: while an attempt is genuinely
// Running, progress recorded on its row after the journal's start document
// was written is what the overlay reports, and its source reads "row" rather
// than "journal".
func TestConsoleOperationOverlayPrefersLiveRow(t *testing.T) {
	duringInvocation(t, func(f *phase8Fixture, runID string, journal RunOperation) {
		start := journal.LastProgressAt
		if start == nil {
			t.Fatal("HARNESS PRECONDITION: the journal's start document carries no progress instant")
		}
		f.clock.advance(20 * time.Minute)
		recorded, err := f.runtime.scheduler.RecordProviderProgress(journal.ID, journal.AttemptIdentity, ProviderProgress{Key: "1:42"})
		if err != nil {
			t.Fatal(err)
		}
		if recorded.LastProgressAt == nil || !recorded.LastProgressAt.After(*start) {
			t.Fatalf("progress was not recorded on the row: %v", recorded.LastProgressAt)
		}
		f.clock.advance(time.Minute)

		detail, found, err := ConsoleRunDetail(f.store, f.stateDir, runID, f.clock.Now())
		if err != nil || !found {
			t.Fatalf("ConsoleRunDetail(%q) = found=%v err=%v", runID, found, err)
		}
		if detail.Operation == nil {
			t.Fatal("expected a current operation during invocation")
		}
		if detail.Operation.ProgressSource != "row" {
			t.Fatalf("progress source = %q with a live Running row, want row", detail.Operation.ProgressSource)
		}
		if detail.Operation.LastProgressAt == nil || !detail.Operation.LastProgressAt.Equal(*recorded.LastProgressAt) {
			t.Fatalf("last progress = %v, want the row's %v", detail.Operation.LastProgressAt, recorded.LastProgressAt)
		}
	})
}

func TestPublicationAuthorityFromDecisionsFindsPrefixedKey(t *testing.T) {
	decisions := map[string]AuthorityEvaluation{
		"some.other.action\x00main": {AuthorityEvaluatedPayload: AuthorityEvaluatedPayload{Status: "granted"}},
		PublicationActionType + "\x00main": {
			AuthorityEvaluatedPayload: AuthorityEvaluatedPayload{
				Decision: Ref{ID: "decision-1"}, Status: "granted",
			},
		},
	}
	authority := publicationAuthorityFromDecisions(decisions)
	if authority == nil {
		t.Fatal("expected a publication authority to be found")
	}
	if authority.Decision.ID != "decision-1" {
		t.Fatalf("decision = %+v, want decision-1", authority.Decision)
	}

	if none := publicationAuthorityFromDecisions(map[string]AuthorityEvaluation{
		"some.other.action\x00main": {},
	}); none != nil {
		t.Fatalf("expected no publication authority, got %+v", none)
	}
}
