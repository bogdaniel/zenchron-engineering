package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// ADR-0007 §5: trusted main never moves behind the floor the running controller
// was adopted under. Each test names the deliberate break it catches.

func heldUpdater(t *testing.T, harness *updaterHarness, source, floor string) *ControllerUpdater {
	t.Helper()
	running := attestedBuild(ControllerAdopted, source, "tree-"+shortSHA(source), strings.Repeat("ab", 32))
	return NewControllerUpdater(runningBinding(running), AdoptedBuildRequest{OutputRoot: t.TempDir()},
		ControllerUpdaterPorts{
			ObserveTrustedMain: harness.observe, Build: harness.build,
			Prepare: harness.prepare, Preflight: harness.preflight,
			Floor:      RevisionRecord{Revision: floor},
			IsAncestor: harnessAncestry,
		})
}

func attemptOnce(updater *ControllerUpdater) ControllerUpdate {
	return updater.Attempt(context.Background(), time.Now().UTC())
}

// Break 8: a resolution behind the floor holds, builds nothing, and is
// reported as a regression.
func TestATrustRegressionHoldsAndBuildsNothing(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: runningRevision, Tree: "tree-a"}}
	updater := heldUpdater(t, harness, movedRevision, movedRevision)
	for i := 0; i < 3; i++ {
		update := attemptOnce(updater)
		if update.State != UpdateHeld || !strings.Contains(update.Detail, "trust regression") {
			t.Fatalf("state = %q (%s), want held for a regression", update.State, update.Detail)
		}
	}
	if harness.buildCount() != 0 {
		t.Fatalf("%d builds while trust had regressed", harness.buildCount())
	}
}

// Break 9: the floor is the recorded trusted_main, not the source. A
// controller built from an older source (1) inside a newer trusted main (3)
// must hold when trust resolves between them (2): newer than its source, but a
// regression all the same.
func TestTheFloorIsTheRecordedTrustedMainNotTheSource(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-b"}}
	updater := heldUpdater(t, harness, runningRevision, movedAgain)
	if update := attemptOnce(updater); update.State != UpdateHeld {
		t.Fatalf("state = %q (%s): a resolution behind the recorded trusted main was not held", update.State, update.Detail)
	}
	if harness.buildCount() != 0 {
		t.Fatal("a successor was built behind the recorded trusted main")
	}
}

// Break 13: HOLD clears on its own as soon as trust resolves to the floor or a
// descendant of it, with no acknowledgement step; and while it lasts it is not
// a launch, so it never makes this controller give up serving.
func TestAHoldClearsAutomaticallyOnAForwardResolution(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: runningRevision, Tree: "tree-a"}}
	updater := heldUpdater(t, harness, movedRevision, movedRevision)
	upgrade := NewControllerUpgrade(updater, func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch {
		t.Fatal("a held updater launched a successor")
		return SuccessionLaunch{}
	})
	if attempt := upgrade.Attempt(context.Background(), time.Now().UTC()); attempt.Update.State != UpdateHeld || attempt.Superseded() {
		t.Fatalf("attempt = %+v, want held and still serving", attempt)
	}

	harness.moveTrustedMainTo(movedAgain, "tree-"+shortSHA(movedAgain))
	settle(t, updater, UpdateReady)
	if harness.buildCount() != 1 {
		t.Fatalf("%d builds after the hold cleared, want 1", harness.buildCount())
	}
}

// No accepted revision within the bound, no floor, and a revision the floor
// is not comparable with all hold: none of them is a forward resolution.
func TestEveryUnprovenResolutionHolds(t *testing.T) {
	t.Run("no trusted main within the bound", func(t *testing.T) {
		harness := &updaterHarness{observeErr: fmt.Errorf("%w: 256 first-parent commit(s) inspected", ErrNoTrustedMain)}
		if update := attemptOnce(heldUpdater(t, harness, runningRevision, runningRevision)); update.State != UpdateHeld {
			t.Fatalf("state = %q, want held", update.State)
		}
	})
	t.Run("no floor", func(t *testing.T) {
		harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-b"}}
		if update := attemptOnce(heldUpdater(t, harness, runningRevision, "")); update.State != UpdateHeld {
			t.Fatalf("state = %q, want held", update.State)
		}
	})
	t.Run("not comparable", func(t *testing.T) {
		harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-b"}}
		updater := heldUpdater(t, harness, runningRevision, runningRevision)
		updater.ports.IsAncestor = func(string, string) (bool, error) { return false, nil }
		update := attemptOnce(updater)
		if update.State != UpdateHeld || !strings.Contains(update.Detail, "not comparable") {
			t.Fatalf("state = %q (%s), want held as not comparable", update.State, update.Detail)
		}
	})
	t.Run("an ordinary observation failure is not a hold", func(t *testing.T) {
		harness := &updaterHarness{observeErr: errors.New("the forge did not answer")}
		if update := attemptOnce(heldUpdater(t, harness, runningRevision, runningRevision)); update.State != UpdateObservationFailed {
			t.Fatalf("state = %q, want observation_failed", update.State)
		}
	})
}

// A hold and an idle controller both say where main_head is and why the newest
// commit past trusted main is not trusted.
func TestTheReportShowsMainHeadAndWhyItIsNotTrusted(t *testing.T) {
	update := ControllerUpdate{State: UpdateIdle, Subject: RevisionRecord{Revision: runningRevision},
		Trust: &TrustReport{MainHead: movedAgain, SkippedTotal: 2,
			Skipped: []SkippedRevision{{movedAgain, "T2 pending"}, {movedRevision, "T2 failure (run 3 attempt 1)"}}}}
	got := update.Describe()
	if !strings.Contains(got, "main_head "+shortSHA(movedAgain)) || !strings.Contains(got, "2 newer commit(s)") || !strings.Contains(got, "T2 pending") {
		t.Fatalf("report = %q", got)
	}
}

// Break 9 at the composition: the floor serve reads from the running
// controller's provenance is its recorded trusted_main. A controller built
// from an older source inside a newer trusted main floors at the trusted main;
// a v1 record floors at its legacy trusted_main; a record that does not prove
// itself gives no floor at all - never the source.
func TestTheFloorComesFromTheRecordedTrustedMain(t *testing.T) {
	f := newAdoptedFixture(t)
	parent := adoptedGit(t, f.dir, "rev-parse", f.head+"^")
	request := f.request(t)
	request.Revision = parent
	older, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{Kind: ControllerUnattested})
	if err != nil {
		t.Fatal(err)
	}
	if older.Source.Revision != parent || older.TrustedMain.Revision != f.head {
		t.Fatalf("fixture: source %s trusted %s", shortSHA(older.Source.Revision), shortSHA(older.TrustedMain.Revision))
	}
	if floor, err := older.TrustFloor(); err != nil || floor != older.TrustedMain {
		t.Fatalf("v2 floor = %+v, %v; want the recorded trusted main %s, not the source", floor, err, shortSHA(f.head))
	}

	var legacy AdoptedBuildProvenance
	if err := json.Unmarshal([]byte(v1AdoptedRecord), &legacy); err != nil {
		t.Fatal(err)
	}
	if floor, err := legacy.TrustFloor(); err != nil || floor != legacy.TrustedMain {
		t.Fatalf("v1 floor = %+v, %v; want the projected legacy trusted main", floor, err)
	}

	invalid := older
	evidence := *older.TrustEvidence.Observation
	evidence.Eligible = false
	invalid.TrustEvidence = &TrustEvidenceRecord{Kind: TrustEvidenceT2, Observation: &evidence}
	if floor, err := invalid.TrustFloor(); err == nil || floor != (RevisionRecord{}) {
		t.Fatalf("an invalid v2 record gave floor %+v, %v; want none", floor, err)
	}
}

// Why there is no floor reaches the operator, not just that there is none.
func TestAMissingFloorSaysWhy(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-b"}}
	updater := heldUpdater(t, harness, runningRevision, "")
	updater.ports.FloorError = errors.New("adopted-build/2 record for 1111111 is invalid: its T2 evidence is not eligible")
	update := attemptOnce(updater)
	if update.State != UpdateHeld || !strings.Contains(update.Detail, "is not eligible") ||
		update.Trust == nil || !strings.Contains(update.Trust.FloorError, "is not eligible") {
		t.Fatalf("update = %+v", update)
	}
}

// The report is bounded: the newest skipped commits and the total, never all
// 256.
func TestTheTrustReportIsBounded(t *testing.T) {
	skipped := make([]SkippedRevision, 40)
	for i := range skipped {
		skipped[i] = SkippedRevision{Revision: shaOf('a'), Reason: "T2 pending"}
	}
	harness := &updaterHarness{trusted: RevisionRecord{Revision: runningRevision, Tree: "tree-a"}}
	updater := heldUpdater(t, harness, runningRevision, runningRevision)
	report := updater.trustReport(TrustedMainView{TrustedMain: harness.trusted, MainHead: shaOf('a'), Skipped: skipped})
	if len(report.Skipped) != maxReportedSkipped || report.SkippedTotal != 40 {
		t.Fatalf("report carries %d of %d skipped", len(report.Skipped), report.SkippedTotal)
	}
	lines := report.Lines()
	if last := lines[len(lines)-1]; !strings.Contains(last[1], "30 older") {
		t.Fatalf("last line = %v", last)
	}
}

// Doctor shows the serving controller's own trust state: a hold warns with its
// reason, an unheld controller passes, and no answer is not a pass.
func TestDoctorReportsTheTrustedMainHold(t *testing.T) {
	held := &ControllerUpdate{State: UpdateHeld, Subject: RevisionRecord{Revision: runningRevision},
		Detail: "trust regression: trusted main resolved to 1111111, behind the floor 2222222",
		Trust:  &TrustReport{Floor: movedRevision, TrustedMain: runningRevision, MainHead: movedAgain}}
	for name, tc := range map[string]struct {
		in       DoctorInput
		status   DoctorStatus
		contains string
	}{
		"held":        {DoctorInput{ControllerUpdate: held}, DoctorWarn, "trust floor " + shortSHA(movedRevision)},
		"idle":        {DoctorInput{ControllerUpdate: &ControllerUpdate{State: UpdateIdle}}, DoctorPass, "trusted main is the running controller"},
		"no answer":   {DoctorInput{ControllerUpdateError: errors.New("no supervisor is running")}, DoctorWarn, "no serving controller answered"},
		"not yet run": {DoctorInput{}, DoctorWarn, "no trusted-main observation yet"},
	} {
		check := doctorTrust(tc.in)
		if check.Status != tc.status || !strings.Contains(check.Reason, tc.contains) {
			t.Errorf("%s: %s %q", name, check.Status, check.Reason)
		}
	}
}
