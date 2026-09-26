package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// choreography is both halves of the protocol over one store, with every effect
// observable and every step interruptible. Two real processes cannot be crashed
// deterministically between two durable writes; this can.
type choreography struct {
	t *testing.T
	// fixture is retained so a test can move the durable head the preflight
	// decided about, which is what revalidation exists to catch.
	fixture    *phase8Fixture
	store      *SQLiteOperationStore
	state      string
	root       string
	record     ControllerHandoff
	self       ControllerSelfRecord
	predecesor ControllerSelfRecord

	owner     string // "predecessor", "successor" or ""
	drained   bool
	admitted  []string
	crashAt   string
	crashKind error
}

func (c *choreography) fail(step string) error {
	if c.crashAt == step {
		if c.crashKind != nil {
			return c.crashKind
		}
		return fmt.Errorf("simulated crash at %s", step)
	}
	return nil
}

func (c *choreography) predecessorPorts() HandoffPorts {
	return HandoffPorts{
		Store: c.store, Self: c.predecesor, Now: func() time.Time { return time.Unix(1700000000, 0).UTC() },
		DrainWorkAdmission: func() error {
			if err := c.fail("drain"); err != nil {
				return err
			}
			c.drained = true
			return nil
		},
		ReleaseOwnership: func() error {
			if err := c.fail("release"); err != nil {
				return err
			}
			c.owner = ""
			return nil
		},
		ControllerRoot: c.root,
	}
}

func (c *choreography) successorPorts() HandoffPorts {
	return HandoffPorts{
		Store: c.store, Self: c.self, Now: func() time.Time { return time.Unix(1700000001, 0).UTC() },
		AcquireOwnership: func() error {
			if err := c.fail("acquire"); err != nil {
				return err
			}
			if c.owner != "" {
				return fmt.Errorf("ownership is held by the %s", c.owner)
			}
			c.owner = "successor"
			return nil
		},
		ReleaseOwnership: func() error { c.owner = ""; return nil },
		AdmitSuccession: func(runID string, decision ControllerSuccessionDecision) error {
			if err := c.fail("admit-succession"); err != nil {
				return err
			}
			c.admitted = append(c.admitted, runID)
			return nil
		},
		ProveControlEndpoint: func() error { return c.fail("prove") },
		ControllerRoot:       c.root,
	}
}

// newChoreography prepares a real store, two real generation directories, and a
// prepared handoff between two adopted controllers.
func newChoreography(t *testing.T) *choreography {
	t.Helper()
	fixture := newPhase8Fixture(t)
	root := t.TempDir()
	predecessorDir, successorDir := filepath.Join(root, "main-aaaaaaaa"), filepath.Join(root, "main-bbbbbbbb")
	for _, dir := range []string{predecessorDir, successorDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "zenchron-engineering"), []byte(dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// A REAL LIVE RUN, created by the predecessor generation, so the handoff
	// has something to carry: a protocol proven only against an empty store
	// would never exercise the succession admissions it exists to order.
	predecessorBuild := attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	successorBuild := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	deps := fixture.deps
	deps.ControllerBuild = predecessorBuild
	predecessorRuntime, err := NewEngineeringRuntime(deps)
	if err != nil {
		t.Fatal(err)
	}
	fixture.runtime = predecessorRuntime
	fixture.start()

	predecessor := HandoffParty{
		Binding:      ControllerBinding{Controller: deps.ControllerID, Build: &predecessorBuild, Config: deps.ConfigDigest},
		ArtifactPath: filepath.Join(predecessorDir, "zenchron-engineering"),
	}
	successor := HandoffParty{
		Binding:      ControllerBinding{Controller: deps.ControllerID, Build: &successorBuild, Config: deps.ConfigDigest},
		ArtifactPath: filepath.Join(successorDir, "zenchron-engineering"),
	}
	record, err := PreflightControllerHandoff(fixture.store, HandoffPreflightInput{
		Predecessor: predecessor, Successor: successor,
		TrustedMain: RevisionRecord{Revision: successorRevision, Tree: "tree-b"},
		IsAncestor:  ancestorAlways, Now: time.Unix(1700000000, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.Phase != HandoffPrepared {
		t.Fatalf("preflight refused: %v", record.Blockers())
	}
	return &choreography{
		t: t, fixture: fixture, store: fixture.store, state: fixture.stateDir, root: root, record: record, owner: "predecessor",
		self:       ControllerSelfRecord{Build: successorBuild, ExecutablePath: successor.ArtifactPath, Measured: successorBuild.BinarySHA256},
		predecesor: ControllerSelfRecord{Build: predecessorBuild, ExecutablePath: predecessor.ArtifactPath, Measured: predecessorBuild.BinarySHA256},
	}
}

func (c *choreography) digest(party HandoffParty) string {
	c.t.Helper()
	digest, err := party.Binding.Digest()
	if err != nil {
		c.t.Fatal(err)
	}
	return digest
}

func (c *choreography) stored() ControllerHandoff {
	c.t.Helper()
	stored, found, err := c.store.ControllerHandoff(c.record.ID)
	if err != nil || !found {
		c.t.Fatalf("read handoff: %v found=%v", err, found)
	}
	return stored
}

// THE WHOLE PROTOCOL, and the properties that must hold at the end of it.
func TestHandoffRunsEndToEnd(t *testing.T) {
	c := newChoreography(t)
	released, err := BeginHandoff(c.predecessorPorts(), c.record)
	if err != nil {
		t.Fatal(err)
	}
	if released.Phase != HandoffOwnershipReleased || !c.drained || c.owner != "" {
		t.Fatalf("after the predecessor's half: phase=%q drained=%v owner=%q", released.Phase, c.drained, c.owner)
	}

	activated, err := CompleteHandoff(c.successorPorts(), c.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if activated.Phase != HandoffActivated {
		t.Fatalf("phase = %q, want activated", activated.Phase)
	}
	if c.owner != "successor" {
		t.Fatalf("ownership is %q, want the successor", c.owner)
	}
	// The projection followed authority, and the operator path resolves.
	target, err := os.Readlink(filepath.Join(c.root, StableEntrypointName))
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Dir(c.record.Successor.ArtifactPath) {
		t.Fatalf("the pointer names %s", target)
	}
	// Work admission moved exactly once, and in the right direction.
	if admission := WorkAdmissionFor(&activated, c.self, c.digest(c.record.Successor)); !admission.Permitted {
		t.Fatalf("the activated successor may not admit work: %s", admission.Reason)
	}
	if admission := WorkAdmissionFor(&activated, c.predecesor, c.digest(c.record.Predecessor)); admission.Permitted {
		t.Fatal("the drained predecessor may still admit work")
	}
}

// AT MOST ONE PROCESS MAY ADMIT WORK, at every phase of the transition. This is
// the property the whole protocol exists for, so it is asserted phase by phase
// rather than at the end.
func TestExactlyOneControllerMayAdmitWorkAtEveryPhase(t *testing.T) {
	c := newChoreography(t)
	predecessor, successor := c.digest(c.record.Predecessor), c.digest(c.record.Successor)
	record := c.record
	now := time.Unix(1700000000, 0).UTC()

	for _, phase := range []HandoffPhase{
		HandoffPrepared, HandoffDraining, HandoffOwnershipReleased,
		HandoffSuccessorAcquired, HandoffRevalidated, HandoffActivated,
	} {
		if phase != HandoffPrepared {
			advanced, err := record.Advance(phase, now)
			if err != nil {
				t.Fatal(err)
			}
			record = advanced
		}
		predecessorMay := WorkAdmissionFor(&record, c.predecesor, predecessor).Permitted
		successorMay := WorkAdmissionFor(&record, c.self, successor).Permitted
		admitting := 0
		for _, may := range []bool{predecessorMay, successorMay} {
			if may {
				admitting++
			}
		}
		if admitting > 1 {
			t.Fatalf("at %q both controllers may admit work", phase)
		}
		switch phase {
		case HandoffPrepared:
			if !predecessorMay || successorMay {
				t.Fatalf("at %q admission should be the predecessor's alone", phase)
			}
		case HandoffActivated:
			if predecessorMay || !successorMay {
				t.Fatalf("at %q admission should be the successor's alone", phase)
			}
		default:
			// The window between them: NOBODY serves. A gap is the honest
			// answer while the scheduler is changing hands.
			if predecessorMay || successorMay {
				t.Fatalf("at %q somebody admitted work mid-transition", phase)
			}
		}
	}

	// OWNERSHIP IS NOT ACTIVATION. The successor holds the lock at
	// successor_acquired and still may not serve.
	acquired, err := c.record.Advance(HandoffDraining, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []HandoffPhase{HandoffOwnershipReleased, HandoffSuccessorAcquired} {
		if acquired, err = acquired.Advance(phase, now); err != nil {
			t.Fatal(err)
		}
	}
	if WorkAdmissionFor(&acquired, c.self, successor).Permitted {
		t.Fatal("holding ownership was read as being the active generation")
	}
}

// EVERY BOUNDARY, crashed. What matters is not that the step failed but that
// what is left on disk resolves deterministically to exactly one controller.
func TestCrashAtEveryHandoffBoundary(t *testing.T) {
	for _, test := range []struct {
		step       string
		wantPhase  HandoffPhase
		wantAction HandoffAction
		byWhom     string
	}{
		{"drain", HandoffPrepared, HandoffActionResumePredecessor, "predecessor"},
		{"release", HandoffDraining, HandoffActionResumePredecessor, "predecessor"},
		{"acquire", HandoffOwnershipReleased, HandoffActionResumePredecessor, "predecessor"},
		// Past acquisition the successor owns the choreography, and the record
		// still names the predecessor until revalidation completes.
		{"admit-succession", HandoffFailed, HandoffActionNone, ""},
		{"prove", HandoffRevalidated, HandoffActionContinueSuccessor, "successor"},
	} {
		t.Run(test.step, func(t *testing.T) {
			c := newChoreography(t)
			c.crashAt = test.step
			released, err := BeginHandoff(c.predecessorPorts(), c.record)
			if err == nil && (test.step == "drain" || test.step == "release") {
				t.Fatalf("the predecessor's half survived a crash at %s", test.step)
			}
			if err == nil {
				if _, err := CompleteHandoff(c.successorPorts(), released.ID); err == nil {
					t.Fatalf("the successor's half survived a crash at %s", test.step)
				}
			}

			stored := c.stored()
			if stored.Phase != test.wantPhase {
				t.Fatalf("durable phase = %q, want %q", stored.Phase, test.wantPhase)
			}
			// NOBODY IS SERVING on a broken transition until it is resolved.
			if WorkAdmissionFor(&stored, c.self, c.digest(c.record.Successor)).Permitted {
				t.Fatal("the successor admitted work on an unfinished handoff")
			}

			records := []ControllerHandoff{stored}
			predecessorResolution := ResolveControllerHandoff(records, c.predecesor, c.digest(c.record.Predecessor))
			successorResolution := ResolveControllerHandoff(records, c.self, c.digest(c.record.Successor))
			switch test.byWhom {
			case "predecessor":
				if predecessorResolution.Action != test.wantAction {
					t.Fatalf("the predecessor resolves to %q, want %q (%s)",
						predecessorResolution.Action, test.wantAction, predecessorResolution.Detail)
				}
				if successorResolution.Action != HandoffActionRefuse {
					t.Fatalf("the successor resolves to %q, want refusal", successorResolution.Action)
				}
			case "successor":
				if successorResolution.Action != test.wantAction {
					t.Fatalf("the successor resolves to %q, want %q (%s)",
						successorResolution.Action, test.wantAction, successorResolution.Detail)
				}
				if predecessorResolution.Action != HandoffActionRefuse {
					t.Fatalf("the predecessor resolves to %q, want refusal", predecessorResolution.Action)
				}
			default:
				// A settled failure is nobody's to resume: the record named the
				// predecessor as recovery owner and the transition is over.
				if successorResolution.Action != HandoffActionNone || predecessorResolution.Action != HandoffActionNone {
					t.Fatalf("a settled failure is still resolvable: %q / %q",
						predecessorResolution.Action, successorResolution.Action)
				}
				if !stored.MayRecover(c.digest(c.record.Predecessor)) {
					t.Fatal("a failed transition did not hand recovery back to the predecessor")
				}
				if !WorkAdmissionFor(&stored, c.predecesor, c.digest(c.record.Predecessor)).Permitted {
					t.Fatal("the predecessor cannot resume after a failed transition it never left")
				}
			}
		})
	}
}

// ACTIVATION WRITTEN, POINTER STALE. Authority is settled; only the projection
// is wrong, and repairing it reconsiders nothing.
func TestCrashBetweenActivationAndProjection(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	// Point the projection at the predecessor, as an operator would have had it.
	pointer := filepath.Join(c.root, StableEntrypointName)
	if err := os.Symlink(filepath.Dir(c.record.Predecessor.ArtifactPath), pointer); err != nil {
		t.Fatal(err)
	}
	// Make the projection step fail by occupying it with a directory, which is
	// the one thing activation refuses to clear.
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pointer, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteHandoff(c.successorPorts(), c.record.ID); err == nil {
		t.Fatal("the projection failure was not reported")
	}

	stored := c.stored()
	if stored.Phase != HandoffActivated {
		t.Fatalf("phase = %q: authority must be settled even though the projection failed", stored.Phase)
	}
	if !WorkAdmissionFor(&stored, c.self, c.digest(c.record.Successor)).Permitted {
		t.Fatal("the activated successor may not serve because a symlink is wrong")
	}
	resolution := ResolveControllerHandoff([]ControllerHandoff{stored}, c.self, c.digest(c.record.Successor))
	if resolution.Action != HandoffActionRepairProjection {
		t.Fatalf("resolution = %q, want repair_projection", resolution.Action)
	}
	// Repair once the obstruction is gone; authority is never revisited.
	if err := os.RemoveAll(pointer); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateControllerGeneration(c.store, stored.ID, c.self, c.root); err != nil {
		t.Fatalf("repair refused: %v", err)
	}
	if c.stored().Phase != HandoffActivated {
		t.Fatal("repairing the projection changed the authority record")
	}
}

// SPLIT BRAIN: the pointer names the successor and the predecessor process is
// still alive. It stays drained, cannot regain authority, and may only finish
// and exit.
func TestActivatedSuccessorLeavesThePredecessorUnableToServe(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	activated, err := CompleteHandoff(c.successorPorts(), c.record.ID)
	if err != nil {
		t.Fatal(err)
	}
	predecessor := c.digest(c.record.Predecessor)

	if WorkAdmissionFor(&activated, c.predecesor, predecessor).Permitted {
		t.Fatal("the predecessor regained work admission after the successor activated")
	}
	resolution := ResolveControllerHandoff([]ControllerHandoff{activated}, c.predecesor, predecessor)
	if resolution.Action != HandoffActionRefuse {
		t.Fatalf("the predecessor resolves to %q, want refusal", resolution.Action)
	}
	// It cannot re-enter the protocol either: the record is settled, and a
	// second attempt to move it finds the phase it expected is gone.
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err == nil {
		t.Fatal("the predecessor restarted a settled handoff")
	}
	// And the old generation is still on disk, addressable as provenance.
	if _, err := os.Stat(c.record.Predecessor.ArtifactPath); err != nil {
		t.Fatalf("the predecessor artifact stopped being addressable: %v", err)
	}
}

// Revalidation is what makes anything learned before acquisition safe to use.
// A successor whose generation stopped matching the record fails back.
func TestRevalidationAfterAcquisitionFailsBackToThePredecessor(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	ports := c.successorPorts()
	// This process is not the generation the record names.
	ports.Self.Build.BinarySHA256 = strings.Repeat("ef", 32)

	if _, err := CompleteHandoff(ports, c.record.ID); err == nil {
		t.Fatal("a successor that is not the named generation completed the handoff")
	}
	stored := c.stored()
	if stored.Phase != HandoffFailed {
		t.Fatalf("phase = %q, want failed", stored.Phase)
	}
	if !stored.MayRecover(c.digest(c.record.Predecessor)) {
		t.Fatal("recovery was not handed back to the predecessor")
	}
	if len(c.admitted) != 0 {
		t.Fatalf("succession was admitted for %v before revalidation passed", c.admitted)
	}
	if c.owner != "" {
		t.Fatalf("ownership is still held by the %s after failing back", c.owner)
	}
}

// Recovery converges: running the resolution repeatedly reaches the same answer
// and nothing about it depends on reading the stable pointer.
func TestRecoveryConvergesAndNeverReadsTheProjection(t *testing.T) {
	c := newChoreography(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteHandoff(c.successorPorts(), c.record.ID); err != nil {
		t.Fatal(err)
	}
	stored := c.stored()
	successor := c.digest(c.record.Successor)

	first := ResolveControllerHandoff([]ControllerHandoff{stored}, c.self, successor)
	// Destroy the projection entirely. The answer must not move.
	if err := os.RemoveAll(filepath.Join(c.root, StableEntrypointName)); err != nil {
		t.Fatal(err)
	}
	second := ResolveControllerHandoff([]ControllerHandoff{stored}, c.self, successor)
	if first.Action != second.Action || first.Action != HandoffActionRepairProjection {
		t.Fatalf("resolution moved with the projection: %q then %q", first.Action, second.Action)
	}
	if !WorkAdmissionFor(&stored, c.self, successor).Permitted {
		t.Fatal("deleting a symlink removed the successor's authority")
	}
}
