package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// twoGenerationFixture runs a REAL second upgrade: G1 hands to G2, then G2
// hands to G3, both through the choreography. It exists because the sequence
// itself was broken - the contradictory-activation scan read H1 as evidence
// against H2, so a second upgrade could not complete at all.
type twoGenerationFixture struct {
	t      *testing.T
	state  string
	root   string
	store  *SQLiteOperationStore
	first  ControllerHandoff // G1 -> G2
	second ControllerHandoff // G2 -> G3
	g2     ControllerSelfRecord
	g3     ControllerSelfRecord
}

func newTwoGenerationFixture(t *testing.T) *twoGenerationFixture {
	t.Helper()
	inner := newChoreography(t)
	config := inner.record.Successor.Binding.Config

	// The first upgrade, all the way to activated.
	released, err := BeginHandoff(inner.predecessorPorts(), inner.record)
	if err != nil {
		t.Fatal(err)
	}
	firstService := startServiceAs(t, inner.state, inner.root, inner.store, inner.self)
	first, err := firstService.ActivateSuccessor(mustExpect(t, released), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := firstService.DrainAndReleaseRole(); err != nil {
		t.Fatal(err)
	}

	// A third generation, with a real artifact beside the others.
	g3Dir := filepath.Join(inner.root, "main-cccccccc")
	if err := os.MkdirAll(g3Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	g3Artifact := filepath.Join(g3Dir, "zenchron-engineering")
	if err := os.WriteFile(g3Artifact, []byte(g3Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	g3Build := attestedBuild(ControllerAdopted, strangerRevision, "tree-c", strings.Repeat("ef", 32))
	g3 := HandoffParty{
		Binding:      ControllerBinding{Controller: first.Successor.Binding.Controller, Build: &g3Build, Config: config},
		ArtifactPath: g3Artifact,
	}

	// The second upgrade: G2 -> G3, prepared and released by G2.
	prepared, err := PreflightControllerHandoff(inner.store, HandoffPreflightInput{
		Predecessor: first.Successor, Successor: g3,
		TrustedMain: RevisionRecord{Revision: strangerRevision, Tree: "tree-c"},
		IsAncestor:  ancestorAlways, Activated: storeTransitionActivated(inner.store),
		Now: time.Unix(1700000100, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Phase != HandoffPrepared {
		t.Fatalf("the second upgrade was refused at preflight: %v", prepared.Blockers())
	}
	secondPredecessor := startServiceAs(t, inner.state, inner.root, inner.store, inner.self)
	released2, err := BeginHandoff(HandoffPorts{
		Store: inner.store, Self: inner.self, ControllerRoot: inner.root,
		Now:                func() time.Time { return time.Unix(1700000101, 0).UTC() },
		DrainWorkAdmission: func() error { return nil },
		ReleaseOwnership:   func() error { return secondPredecessor.lease.Release() },
	}, prepared)
	if err != nil {
		t.Fatal(err)
	}

	g3Self := ControllerSelfRecord{Build: g3Build, ExecutablePath: g3Artifact, Measured: g3Build.BinarySHA256}
	successorService := startServiceAs(t, inner.state, inner.root, inner.store, g3Self)
	second, err := successorService.ActivateSuccessor(mustExpect(t, released2), nil)
	if err != nil {
		t.Fatalf("THE SECOND UPGRADE WAS REFUSED: %v", err)
	}
	if err := successorService.lease.Release(); err != nil {
		t.Fatal(err)
	}
	return &twoGenerationFixture{
		t: t, state: inner.state, root: inner.root, store: inner.store,
		first: first, second: second, g2: inner.self, g3: g3Self,
	}
}

func startServiceAs(t *testing.T, state, root string, store *SQLiteOperationStore, self ControllerSelfRecord) *ControllerService {
	t.Helper()
	service, err := StartControllerService(state, root, store, self, newFakeAdmission())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.lease.Release() })
	service.now = func() time.Time { return time.Unix(1700000102, 0).UTC() }
	return service
}

func mustExpect(t *testing.T, record ControllerHandoff) ActivationExpectation {
	t.Helper()
	expect, err := Expect(record)
	if err != nil {
		t.Fatal(err)
	}
	return expect
}

// A SECOND UPGRADE MUST BE POSSIBLE. The previous contradictory-activation
// check refused it: H1 activated a different generation than H2's successor,
// which is what an older activation is for.
func TestASecondUpgradeCompletes(t *testing.T) {
	fixture := newTwoGenerationFixture(t)
	current, found, err := fixture.store.CurrentControllerActivation()
	if err != nil || !found {
		t.Fatalf("no activation governs after two upgrades: %v found=%v", err, found)
	}
	if current.ID != fixture.second.ID {
		t.Fatalf("the governing activation is %s, want the second upgrade %s", current.ID, fixture.second.ID)
	}
	// And the first remains historically true.
	first, found, err := fixture.store.ControllerHandoff(fixture.first.ID)
	if err != nil || !found {
		t.Fatal(err)
	}
	if first.Phase != HandoffActivated {
		t.Fatalf("the first transition stopped being activated: %q", first.Phase)
	}
}

// A SUPERSEDED GENERATION CANNOT RESURRECT ITSELF. G2 was legitimately
// activated once, the record still says so, and none of the three
// authority-bearing paths may act on that fact now.
func TestSupersededGenerationCannotActOnItsOldActivation(t *testing.T) {
	fixture := newTwoGenerationFixture(t)
	projectionBefore := mustReadProjection(t, fixture.root)

	// G2 starts and takes the free role - nothing prevents that.
	g2 := startServiceAs(t, fixture.state, fixture.root, fixture.store, fixture.g2)

	if _, err := g2.RecoverActivated(fixture.first.ID); err == nil {
		t.Fatal("a superseded generation recovered its old activation")
	} else if !strings.Contains(err.Error(), "governs now") {
		t.Fatalf("error = %v, want one naming the governing activation", err)
	}
	if _, err := ActivateControllerGeneration(fixture.store, fixture.first.ID, fixture.g2, fixture.root); err == nil {
		t.Fatal("a superseded generation repointed the stable entrypoint at itself")
	}
	if err := g2.EnableWorkAdmission(fixture.first.ID); err == nil {
		t.Fatal("a superseded generation opened its own gate")
	}
	if g2.AdmittingWork() {
		t.Fatal("a superseded generation is admitting work")
	}
	if after := mustReadProjection(t, fixture.root); after != projectionBefore {
		t.Fatalf("the projection moved to %s, want it to stay at %s", after, projectionBefore)
	}
}

// THE POSITIVE SIDE: the generation that does govern can resume and serve.
func TestGoverningGenerationCanResumeAndServe(t *testing.T) {
	fixture := newTwoGenerationFixture(t)
	g3 := startServiceAs(t, fixture.state, fixture.root, fixture.store, fixture.g3)

	if _, err := g3.RecoverActivated(fixture.second.ID); err != nil {
		t.Fatalf("the governing generation could not resume: %v", err)
	}
	if err := g3.EnableWorkAdmission(fixture.second.ID); err != nil {
		t.Fatalf("the governing generation could not serve: %v", err)
	}
	if !g3.AdmittingWork() {
		t.Fatal("the governing generation is not admitting work")
	}
}

// A STALE INTENT IS REFUSED BY THE RUNTIME, not by the watcher. The executor
// is unchanged: it carries the transition it observed and the operations below
// it decline.
func TestStaleReconciliationIntentIsRefusedBelowTheWatcher(t *testing.T) {
	inner := newChoreography(t)
	released, err := BeginHandoff(inner.predecessorPorts(), inner.record)
	if err != nil {
		t.Fatal(err)
	}
	service := startServiceAs(t, inner.state, inner.root, inner.store, inner.self)
	activated, err := service.ActivateSuccessor(mustExpect(t, released), nil)
	if err != nil {
		t.Fatal(err)
	}

	// Classified while this transition governs.
	status, err := DescribeControllerStatus(inner.store, inner.root, func() (LiveControllerSnapshot, error) {
		return service.DescribeLiveController(activated.ID, time.Unix(1700000103, 0).UTC()), nil
	}, time.Unix(1700000103, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	intent := ClassifyReconciliation(WatcherObservation{Status: status, Self: inner.self})
	if intent.Action != ReconcileResumeActivatedService {
		t.Fatalf("intent = %q, want resume", intent.Action)
	}

	// Another transition activates before the intent is executed.
	superseding := activated
	superseding.ID = "handoff-superseding"
	superseding.Predecessor = activated.Successor
	superseding.UpdatedAt = time.Unix(1700000104, 0).UTC()
	if wrote, err := inner.store.PutControllerHandoff(superseding, ""); err != nil || !wrote {
		t.Fatalf("record the superseding transition: %v wrote=%v", err, wrote)
	}
	if wrote, err := inner.store.ActivateControllerHandoff(superseding, HandoffActivated); err != nil || !wrote {
		t.Fatalf("activate the superseding transition: %v wrote=%v", err, wrote)
	}

	// The operations the intent names are now refused BY THE RUNTIME. The
	// executor lives above this layer and needs no knowledge of currency: it
	// carries the transition it observed, and these decline.
	//
	// The end-to-end version - classify, supersede, execute the stale intent -
	// belongs with the executor, and is the case that proves it needs no
	// special code to behave correctly here.
	if _, err := service.RecoverActivated(intent.HandoffID); err == nil {
		t.Fatal("a stale transition was resumed")
	} else if !strings.Contains(err.Error(), "governs now") {
		t.Fatalf("error = %v, want one naming the governing activation", err)
	}
	if err := service.EnableWorkAdmission(intent.HandoffID); err == nil {
		t.Fatal("a stale transition opened service")
	}
	if service.AdmittingWork() {
		t.Fatal("a stale transition left the controller serving")
	}
}

// The activation and the pointer move in one commit: a compare-and-set that
// fails writes neither.
func TestActivationAndCurrencyCommitTogether(t *testing.T) {
	inner := newChoreography(t)
	released, err := BeginHandoff(inner.predecessorPorts(), inner.record)
	if err != nil {
		t.Fatal(err)
	}
	acquired, err := released.Advance(HandoffSuccessorAcquired, time.Unix(1700000106, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := inner.store.PutControllerHandoff(acquired, HandoffOwnershipReleased); err != nil || !wrote {
		t.Fatal(err)
	}
	revalidated, err := acquired.Advance(HandoffRevalidated, time.Unix(1700000107, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if wrote, err := inner.store.PutControllerHandoff(revalidated, HandoffSuccessorAcquired); err != nil || !wrote {
		t.Fatal(err)
	}
	activated, err := revalidated.Advance(HandoffActivated, time.Unix(1700000108, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}

	// A stale expectation writes nothing at all, including the pointer.
	if wrote, err := inner.store.ActivateControllerHandoff(activated, HandoffOwnershipReleased); err != nil || wrote {
		t.Fatalf("a stale compare-and-set wrote: %v wrote=%v", err, wrote)
	}
	if _, found, err := inner.store.CurrentControllerActivation(); err != nil || found {
		t.Fatalf("a refused activation still moved the pointer: found=%v err=%v", found, err)
	}

	// The correct one moves both.
	if wrote, err := inner.store.ActivateControllerHandoff(activated, HandoffRevalidated); err != nil || !wrote {
		t.Fatalf("activate: %v wrote=%v", err, wrote)
	}
	current, found, err := inner.store.CurrentControllerActivation()
	if err != nil || !found {
		t.Fatalf("the pointer was not written: %v found=%v", err, found)
	}
	if current.ID != activated.ID {
		t.Fatalf("the pointer names %s, want %s", current.ID, activated.ID)
	}
	stored, _, err := inner.store.ControllerHandoff(activated.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Phase != HandoffActivated {
		t.Fatalf("phase = %q, want activated", stored.Phase)
	}
}

func mustReadProjection(t *testing.T, root string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(root, StableEntrypointName))
	if err != nil {
		return ""
	}
	return target
}
