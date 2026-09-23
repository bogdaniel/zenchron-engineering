package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// updaterHarness is a trusted main the test moves, a build the test controls,
// and counters for the two things that must not happen twice: building and
// handing off.
type updaterHarness struct {
	mu           sync.Mutex
	trusted      RevisionRecord
	observeErr   error
	builds       int
	buildGate    chan struct{} // when set, a build blocks until released
	buildErr     error
	produced     func(RevisionRecord) AdoptedBuildProvenance
	prepareErr   error
	preflightErr error
	prepared     int
}

func (h *updaterHarness) observe(context.Context) (RevisionRecord, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.trusted, h.observeErr
}

func (h *updaterHarness) moveTrustedMainTo(revision, tree string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.trusted = RevisionRecord{Revision: revision, Tree: tree}
}

func (h *updaterHarness) build(_ context.Context, request AdoptedBuildRequest) (AdoptedBuildProvenance, error) {
	h.mu.Lock()
	h.builds++
	gate, err, produce := h.buildGate, h.buildErr, h.produced
	subject := RevisionRecord{Revision: request.Revision}
	h.mu.Unlock()
	if gate != nil {
		<-gate
	}
	if err != nil {
		return AdoptedBuildProvenance{}, err
	}
	if produce != nil {
		return produce(subject), nil
	}
	return AdoptedBuildProvenance{
		Version:      "main-" + shortSHA(subject.Revision),
		Source:       RevisionRecord{Revision: subject.Revision, Tree: "tree-" + shortSHA(subject.Revision)},
		BinarySHA256: strings.Repeat("cd", 32),
		OutputPath:   "/controller/main-" + shortSHA(subject.Revision) + "/zenchron-engineering",
		SelfProbe:    SelfProbeRecord{Matched: true},
	}, nil
}

func (h *updaterHarness) prepare(context.Context, ControllerBinding, string) (ControllerHandoff, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.prepared++
	if h.prepareErr != nil {
		return ControllerHandoff{}, h.prepareErr
	}
	return ControllerHandoff{ID: "handoff-prepared", Phase: HandoffPrepared}, nil
}

func (h *updaterHarness) preflight(ControllerHandoff) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.preflightErr
}

func (h *updaterHarness) buildCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.builds
}

const (
	runningRevision = "1111111111111111111111111111111111111111"
	movedRevision   = "2222222222222222222222222222222222222222"
	movedAgain      = "3333333333333333333333333333333333333333"
)

func newUpdater(t *testing.T, harness *updaterHarness) *ControllerUpdater {
	t.Helper()
	running := attestedBuild(ControllerAdopted, runningRevision, "tree-a", strings.Repeat("ab", 32))
	return NewControllerUpdater(
		ControllerSelfRecord{Build: running, Measured: running.BinarySHA256},
		AdoptedBuildRequest{OutputRoot: t.TempDir()},
		ControllerUpdaterPorts{
			ObserveTrustedMain: harness.observe, Build: harness.build,
			Prepare: harness.prepare, Preflight: harness.preflight,
		})
}

// settle waits for the asynchronous build to finish, because a build does not
// block the supervisor pass that started it.
func settle(t *testing.T, updater *ControllerUpdater, want UpdateState) ControllerUpdate {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var update ControllerUpdate
	for time.Now().Before(deadline) {
		update = updater.Attempt(context.Background(), time.Now().UTC())
		if update.State == want {
			return update
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("state = %q, want %q (%s)", update.State, want, update.Detail)
	return update
}

// MAIN UNCHANGED, NOTHING BUILT.
func TestNoBuildWhileTrustedMainIsTheRunningController(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: runningRevision, Tree: "tree-a"}}
	updater := newUpdater(t, harness)
	for i := 0; i < 5; i++ {
		if update := updater.Attempt(context.Background(), time.Now().UTC()); update.State != UpdateIdle {
			t.Fatalf("state = %q, want idle", update.State)
		}
	}
	if harness.buildCount() != 0 {
		t.Fatalf("%d builds for a controller that is already trusted main", harness.buildCount())
	}
}

// MAIN MOVES, EXACTLY ONE BUILD - however many passes observe it.
func TestATrustedMainAdvanceBuildsExactlyOnce(t *testing.T) {
	harness := &updaterHarness{trusted: RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)}}
	updater := newUpdater(t, harness)

	ready := settle(t, updater, UpdateReady)
	if ready.Subject.Revision != movedRevision {
		t.Fatalf("subject = %s, want the observed trusted main", shortSHA(ready.Subject.Revision))
	}
	if ready.Handoff == "" || ready.Artifact == "" {
		t.Fatalf("a ready update names no artifact or transition: %+v", ready)
	}
	for i := 0; i < 5; i++ {
		if update := updater.Attempt(context.Background(), time.Now().UTC()); update.State != UpdateReady {
			t.Fatalf("state = %q on a settled subject", update.State)
		}
	}
	if harness.buildCount() != 1 {
		t.Fatalf("%d builds for one trusted-main advance, want 1", harness.buildCount())
	}
}

// A BUILD FAILURE COSTS AN UPDATE, NOT AVAILABILITY - and is not retried on
// every poll.
func TestABuildFailureLeavesTheControllerServing(t *testing.T) {
	harness := &updaterHarness{
		trusted:  RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
		buildErr: fmt.Errorf("the adopted build failed, so no binary was produced"),
	}
	updater := newUpdater(t, harness)

	refused := settle(t, updater, UpdateRefused)
	if !strings.Contains(refused.Detail, "adopted build failed") {
		t.Fatalf("the refusal lost its cause: %q", refused.Detail)
	}
	for i := 0; i < 5; i++ {
		updater.Attempt(context.Background(), time.Now().UTC())
	}
	if harness.buildCount() != 1 {
		t.Fatalf("a failed build was retried %d times on the poll interval", harness.buildCount())
	}
	// It does retry once the interval has passed: a fixed cause should take.
	updater.Attempt(context.Background(), time.Now().UTC().Add(updateRetryInterval+time.Second))
	settle(t, updater, UpdateRefused)
	if harness.buildCount() != 2 {
		t.Fatalf("builds = %d, want a single retry after the interval", harness.buildCount())
	}
}

// AN UNOBSERVABLE TRUST ROOT IS NOT AN UPGRADE AND NOT AN OUTAGE.
func TestAnUnobservableTrustedMainRefusesVisiblyAndBuildsNothing(t *testing.T) {
	harness := &updaterHarness{observeErr: fmt.Errorf("no governance credential is authorized")}
	updater := newUpdater(t, harness)
	update := updater.Attempt(context.Background(), time.Now().UTC())
	if update.State != UpdateObservationFailed {
		t.Fatalf("state = %q, want observation_failed", update.State)
	}
	if !strings.Contains(update.Describe(), "governance credential") {
		t.Fatalf("the operator is not told why: %s", update.Describe())
	}
	if harness.buildCount() != 0 {
		t.Fatal("something was built without an observable trust root")
	}
}

// MAIN MOVES DURING THE BUILD. The result is never activated, and it is never
// validated against the newer observation either.
func TestASuccessorSupersededWhileBuildingIsNotActivated(t *testing.T) {
	gate := make(chan struct{})
	harness := &updaterHarness{
		trusted:   RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
		buildGate: gate,
	}
	updater := newUpdater(t, harness)

	building := updater.Attempt(context.Background(), time.Now().UTC())
	if building.State != UpdateBuilding {
		t.Fatalf("state = %q, want building", building.State)
	}
	// Main advances mid-build, and a pass notices without interrupting it.
	harness.moveTrustedMainTo(movedAgain, "tree-"+shortSHA(movedAgain))
	noticed := updater.Attempt(context.Background(), time.Now().UTC())
	if noticed.State != UpdateBuilding {
		t.Fatalf("state = %q: a moving branch must not interrupt a running build", noticed.State)
	}
	if !strings.Contains(noticed.Detail, "will be superseded") {
		t.Fatalf("the pass did not notice the move: %q", noticed.Detail)
	}
	if noticed.Subject.Revision != movedRevision {
		t.Fatal("the attempt's subject changed underneath it")
	}
	close(gate)

	// Read WITHOUT advancing: the next Attempt would legitimately start a
	// build for the new subject, and this assertion is about the old one.
	superseded := awaitState(t, updater, UpdateSuperseded)
	if superseded.Subject.Revision != movedRevision {
		t.Fatalf("the superseded result is bound to %s, want the revision it was built from",
			shortSHA(superseded.Subject.Revision))
	}
	if superseded.Handoff != "" {
		t.Fatal("a superseded successor was prepared for handoff")
	}
	if harness.prepared != 0 {
		t.Fatal("a superseded successor reached preparation")
	}

	// AND THE NEW SUBJECT IS THEN BUILT. Superseding one attempt is not
	// abandoning the upgrade; the world moved and the updater follows it.
	next := settle(t, updater, UpdateReady)
	if next.Subject.Revision != movedAgain {
		t.Fatalf("the next attempt is bound to %s, want the new trusted main", shortSHA(next.Subject.Revision))
	}
	if harness.buildCount() != 2 {
		t.Fatalf("builds = %d, want one per subject", harness.buildCount())
	}
}

// awaitState waits for the asynchronous build to settle without calling
// Attempt, which would start the next one.
func awaitState(t *testing.T, updater *ControllerUpdater, want UpdateState) ControllerUpdate {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if update, ok := updater.Current(); ok && update.State == want {
			return update
		}
		time.Sleep(2 * time.Millisecond)
	}
	update, _ := updater.Current()
	t.Fatalf("state = %q, want %q (%s)", update.State, want, update.Detail)
	return update
}

// A BUILD THAT PRODUCED A DIFFERENT SUBJECT IS NOT THIS ATTEMPT'S SUCCESSOR.
func TestAnArtifactThatIsNotTheBoundSubjectIsRefused(t *testing.T) {
	for _, test := range []struct {
		name     string
		produced func(RevisionRecord) AdoptedBuildProvenance
		want     string
	}{
		{"a different revision", func(RevisionRecord) AdoptedBuildProvenance {
			return AdoptedBuildProvenance{
				Source:    RevisionRecord{Revision: movedAgain, Tree: "tree-" + shortSHA(movedRevision)},
				SelfProbe: SelfProbeRecord{Matched: true},
			}
		}, "bound to"},
		{"a different tree", func(subject RevisionRecord) AdoptedBuildProvenance {
			return AdoptedBuildProvenance{
				Source:    RevisionRecord{Revision: subject.Revision, Tree: "someone-elses-tree"},
				SelfProbe: SelfProbeRecord{Matched: true},
			}
		}, "bound to"},
		{"a controller that cannot prove what it is", func(subject RevisionRecord) AdoptedBuildProvenance {
			return AdoptedBuildProvenance{
				Source: RevisionRecord{Revision: subject.Revision, Tree: "tree-" + shortSHA(subject.Revision)},
			}
		}, "did not report the generation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := &updaterHarness{
				trusted:  RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
				produced: test.produced,
			}
			updater := newUpdater(t, harness)
			refused := settle(t, updater, UpdateRefused)
			if !strings.Contains(refused.Detail, test.want) {
				t.Fatalf("detail = %q, want one naming %q", refused.Detail, test.want)
			}
			if harness.prepared != 0 {
				t.Fatal("an unvalidated artifact reached preparation")
			}
		})
	}
}

// A VALIDATED SUCCESSOR THE LIVE RUNS CANNOT MOVE TO blocks the update and not
// the controller.
func TestAnIncompatibleSuccessorBlocksTheUpdateNotTheController(t *testing.T) {
	harness := &updaterHarness{
		trusted:      RevisionRecord{Revision: movedRevision, Tree: "tree-" + shortSHA(movedRevision)},
		preflightErr: fmt.Errorf("run run-x cannot be continued by the successor"),
	}
	updater := newUpdater(t, harness)
	blocked := settle(t, updater, UpdateBlocked)
	if !strings.Contains(blocked.Detail, "run-x") {
		t.Fatalf("the operator is not told which run is in the way: %q", blocked.Detail)
	}
	if blocked.Artifact == "" {
		t.Fatal("a blocked update discarded the artifact it validated")
	}
	// And it is not rebuilt every pass.
	for i := 0; i < 3; i++ {
		updater.Attempt(context.Background(), time.Now().UTC())
	}
	if harness.buildCount() != 1 {
		t.Fatalf("a blocked successor was rebuilt %d times", harness.buildCount())
	}
}
