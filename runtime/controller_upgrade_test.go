package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A READY UPDATE IS LAUNCHED, and a committed launch is the end of this
// process's participation.
func TestAReadyUpdateIsLaunchedExactlyOnce(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)}}
	updater := newUpdater(t, harness)
	launches := 0
	upgrade := NewControllerUpgrade(updater, func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch {
		launches++
		return SuccessionLaunch{HandoffID: "handoff-prepared", Committed: true, Served: true}
	})

	var attempt ControllerUpgradeAttempt
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && attempt.Launch == nil {
		attempt = upgrade.Attempt(context.Background(), time.Now().UTC())
		time.Sleep(2 * time.Millisecond)
	}
	if attempt.Launch == nil {
		t.Fatalf("a ready update was never launched: %+v", attempt.Update)
	}
	if !attempt.Superseded() {
		t.Fatal("a committed launch did not report this controller superseded")
	}
	// EVERY LATER PASS REPORTS IT AND STARTS NOTHING. A process that has given
	// up the role does not hand it over a second time.
	for i := 0; i < 5; i++ {
		again := upgrade.Attempt(context.Background(), time.Now().UTC())
		if !again.Superseded() {
			t.Fatal("a superseded controller stopped saying so")
		}
	}
	if launches != 1 {
		t.Fatalf("%d launches, want exactly 1", launches)
	}
}

// AN UPDATE THAT IS NOT READY LAUNCHES NOTHING.
func TestAnUnreadyUpdateLaunchesNothing(t *testing.T) {
	for _, test := range []struct {
		name    string
		harness *updaterHarness
	}{
		{"trusted main is what this controller is", &updaterHarness{
			trusted: RevisionRecord{Revision: runningRevision, Tree: "tree-a"}}},
		{"the build failed", &updaterHarness{
			trusted:  RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
			buildErr: fmt.Errorf("the adopted build failed")}},
		{"the live runs block it", &updaterHarness{
			trusted:      RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
			preflightErr: fmt.Errorf("run run-x cannot be continued")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			updater := newUpdater(t, test.harness)
			launched := false
			upgrade := NewControllerUpgrade(updater, func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch {
				launched = true
				return SuccessionLaunch{}
			})
			for i := 0; i < 20; i++ {
				if upgrade.Attempt(context.Background(), time.Now().UTC()).Superseded() {
					t.Fatal("a controller that upgraded nothing reported itself superseded")
				}
				time.Sleep(2 * time.Millisecond)
			}
			if launched {
				t.Fatal("a transition was launched from an update that was not ready")
			}
		})
	}
}

// A LAUNCH THAT DID NOT COMMIT LEAVES THIS CONTROLLER SERVING - and the next
// pass may try again, because nothing was given up.
func TestAnUncommittedLaunchLeavesTheControllerServing(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)}}
	updater := newUpdater(t, harness)
	launches := 0
	upgrade := NewControllerUpgrade(updater, func(context.Context, ControllerHandoff, RevisionRecord) SuccessionLaunch {
		launches++
		return SuccessionLaunch{HandoffID: "handoff-prepared",
			Spawn: LaunchStep{Outcome: StepRefused, Detail: "the successor could not be started"}}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && launches == 0 {
		upgrade.Attempt(context.Background(), time.Now().UTC())
		time.Sleep(2 * time.Millisecond)
	}
	attempt := upgrade.Attempt(context.Background(), time.Now().UTC())
	if attempt.Superseded() {
		t.Fatal("a launch that gave nothing up reported this controller superseded")
	}
	if launches < 2 {
		t.Fatalf("%d launches; an uncommitted attempt is not final and may be retried", launches)
	}
}

// ---------------------------------------------------------------------------
// The published successor
// ---------------------------------------------------------------------------

// measuredBytes is what measureExecutable will report for this content.
func measuredBytes(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func publishGeneration(t *testing.T, root string, provenance AdoptedBuildProvenance, binary []byte) AdoptedBuildProvenance {
	t.Helper()
	directory := filepath.Join(root, AdoptedVersionName(provenance.Source.Revision))
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	provenance.OutputPath = filepath.Join(directory, "zenchron-engineering")
	if err := os.WriteFile(provenance.OutputPath, binary, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteAdoptedBuildProvenance(filepath.Join(directory, "provenance.json"), provenance); err != nil {
		t.Fatal(err)
	}
	return provenance
}

func publishedFixture(t *testing.T) (string, RevisionRecord, AdoptedBuildProvenance) {
	t.Helper()
	root := t.TempDir()
	binary := []byte("the successor")
	subject := RevisionRecord{Revision: strings.Repeat("b", 40), Tree: strings.Repeat("c", 40)}
	provenance := publishGeneration(t, root, AdoptedBuildProvenance{
		Kind: ControllerAdopted, Version: AdoptedVersionName(subject.Revision),
		Source: subject, TrustedMain: subject, BinarySHA256: measuredBytes(binary),
	}, binary)
	return root, subject, provenance
}

// A CRASH BETWEEN PUBLISHING AND RECORDING MUST NOT COST THE UPGRADE. The
// artifact is adopted rather than rebuilt into an immutability refusal.
func TestAnAlreadyPublishedSuccessorIsAdoptedRatherThanRebuilt(t *testing.T) {
	root, subject, published := publishedFixture(t)

	found, ok, err := PublishedAdoptedController(root, subject)
	if err != nil || !ok {
		t.Fatalf("the published successor was not found: ok=%t err=%v", ok, err)
	}
	if found.OutputPath != published.OutputPath || found.BinarySHA256 != published.BinarySHA256 {
		t.Fatalf("the adopted provenance is not the published one: %+v", found)
	}
}

// NOTHING PUBLISHED IS NOT A PROBLEM.
func TestAnUnpublishedSubjectIsSimplyNotFound(t *testing.T) {
	root, _, _ := publishedFixture(t)
	_, ok, err := PublishedAdoptedController(root, RevisionRecord{Revision: strings.Repeat("e", 40), Tree: "tree-e"})
	if ok || err != nil {
		t.Fatalf("ok=%t err=%v, want a plain absence", ok, err)
	}
}

// A DIRECTORY THAT DOES NOT HOLD UP IS A REFUSAL, NOT AN ABSENCE. Reporting it
// as nothing would rebuild into the immutability refusal, or adopt whatever is
// there next time round.
func TestAPublishedDirectoryThatDoesNotHoldUpIsRefused(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(t *testing.T, root string, subject RevisionRecord, published AdoptedBuildProvenance)
		want    string
	}{
		{"the binary does not measure what the record says", func(t *testing.T, root string, _ RevisionRecord, published AdoptedBuildProvenance) {
			if err := os.WriteFile(published.OutputPath, []byte("something else"), 0700); err != nil {
				t.Fatal(err)
			}
		}, "measures"},
		{"the binary is gone", func(t *testing.T, root string, _ RevisionRecord, published AdoptedBuildProvenance) {
			if err := os.Remove(published.OutputPath); err != nil {
				t.Fatal(err)
			}
		}, "could not be measured"},
		{"the record names another revision", func(t *testing.T, root string, subject RevisionRecord, published AdoptedBuildProvenance) {
			published.Source.Tree = strings.Repeat("f", 40)
			directory := filepath.Join(root, AdoptedVersionName(subject.Revision))
			if _, err := WriteAdoptedBuildProvenance(filepath.Join(directory, "provenance.json"), published); err != nil {
				t.Fatal(err)
			}
		}, "and the successor wanted is"},
		{"the record is unreadable", func(t *testing.T, root string, subject RevisionRecord, _ AdoptedBuildProvenance) {
			directory := filepath.Join(root, AdoptedVersionName(subject.Revision))
			if err := os.WriteFile(filepath.Join(directory, "provenance.json"), []byte("{"), 0600); err != nil {
				t.Fatal(err)
			}
		}, "could not be read"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, subject, published := publishedFixture(t)
			test.corrupt(t, root, subject, published)

			_, ok, err := PublishedAdoptedController(root, subject)
			if ok {
				t.Fatal("a directory that does not hold up was adopted as a successor")
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one naming %q", err, test.want)
			}
		})
	}
}
