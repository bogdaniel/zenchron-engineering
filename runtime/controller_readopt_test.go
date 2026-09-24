package runtime

// THE COLD RE-ADOPTION, and the state directory it has to leave behind.
//
// Every case here is one clause of the boundary: what it refuses, what it
// records, what it leaves alone, and that ordinary succession continues from it
// afterwards with no special path.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func readoptionFixture(t *testing.T) (*choreography, ReadoptionRequest) {
	t.Helper()
	c := newChoreography(t)
	// The generation being re-adopted is the one the fixture's successor
	// identity measures as, under a DIFFERENT effective configuration - which
	// is the boundary this operation exists to cross.
	binding := ControllerBinding{
		Controller: "zenchron-engineering", Build: &c.self.Build,
		Config: ConfigDigest{Global: "config-after-the-change"},
	}
	trusted := RevisionRecord{Revision: c.self.Build.SourceRevision, Tree: c.self.Build.SourceTree}
	return c, ReadoptionRequest{
		Reason:   "the operator changed the default agent to claude",
		Operator: RecordedOperator{ID: "operator-1", Provenance: ProvenanceLocalUnverified},
		Self:     c.self,
		Provenance: AdoptedBuildProvenance{
			Kind: ControllerAdopted, BinarySHA256: c.self.Measured,
			Source: trusted,
			// The fixture's successor artifact is a real file in a real
			// generation directory, which is what the canonical-artifact proof
			// compares the running executable against.
			OutputPath: c.record.Successor.ArtifactPath,
		},
		Binding:            binding,
		ObserveTrustedMain: func() (RevisionRecord, error) { return trusted, nil },
		Now:                time.Unix(1700001000, 0).UTC(),
	}
}

// settleLiveRuns settles the fixture's run, which belongs to the configuration
// being left behind. It is what an operator does when a re-adoption refuses to
// strand work, and TestAReadoptionRefusesToStrandLiveWork is the test that
// proves the refusal rather than assuming it.
func settleLiveRuns(t *testing.T, c *choreography, now time.Time) {
	t.Helper()
	runs, err := c.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			continue
		}
		if _, err := CancelRun(c.store, c.fixture.runtime.scheduler, now, run.ID, "operator/stop"); err != nil {
			t.Fatal(err)
		}
	}
}

func readoptionLease(t *testing.T, c *choreography) *ControllerRoleLease {
	t.Helper()
	lease, err := AcquireControllerRole(c.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Release() })
	return lease
}

// THE WHOLE BOUNDARY: authority moves, the handoff history does not, and the
// re-adoption is its own record.
func TestAColdReadoptionBecomesTheGoverningAuthority(t *testing.T) {
	c, request := readoptionFixture(t)
	// A completed transition first, so there is real history and a real
	// governing activation to supersede.
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	if _, err := CompleteHandoff(c.successorPorts(), c.record.ID); err != nil {
		t.Fatal(err)
	}
	settleLiveRuns(t, c, request.Now)
	before, found, err := c.store.CurrentControllerAuthority()
	if err != nil || !found || before.Kind != AuthorityHandoffActivation {
		t.Fatalf("HARNESS PRECONDITION: the activation did not become authority: %+v found=%v err=%v", before, found, err)
	}

	readoption, err := ReadoptController(c.store, readoptionLease(t, c), request)
	if err != nil {
		t.Fatalf("the re-adoption was refused: %v", err)
	}

	authority, found, err := c.store.CurrentControllerAuthority()
	if err != nil || !found {
		t.Fatal(err, found)
	}
	if authority.Kind != AuthorityOperatorReadoption || authority.Ref != readoption.ID {
		t.Fatalf("authority = %s %s, want the re-adoption", authority.Kind, authority.Ref)
	}
	governing, err := authority.Binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	wanted, err := request.Binding.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if governing != wanted {
		t.Fatal("authority names a binding that is not the re-adopted one")
	}
	// IT IS NOT AN ACTIVATION, and the handoff it superseded is untouched
	// history.
	stored := c.stored()
	if stored.Phase != HandoffActivated {
		t.Fatalf("the superseded transition is now %q; history was rewritten", stored.Phase)
	}
	if readoption.Previous == nil || readoption.Previous.Ref != before.Ref {
		t.Fatalf("the record does not name what it superseded: %+v", readoption.Previous)
	}
	if readoption.PreviousConfig == readoption.Config {
		t.Fatal("the record does not show the configuration boundary it crossed")
	}
	if readoption.Reason == "" || readoption.Operator.ID == "" {
		t.Fatal("an authority event was recorded without a reason or an operator")
	}
}

// AND ORDINARY SUCCESSION CONTINUES FROM IT. This is the clause that decides
// whether the primitive is finished: the next transition must succeed from a
// re-adopted binding exactly as it would from an activated one.
func TestSuccessionContinuesNormallyFromAReadoptedAnchor(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	if _, err := ReadoptController(c.store, readoptionLease(t, c), request); err != nil {
		t.Fatal(err)
	}
	// A transition whose PREDECESSOR is the re-adopted binding. Nothing about
	// it knows a re-adoption happened.
	successorBuild := c.self.Build
	successorBuild.Version, successorBuild.SourceRevision = "main-cccccccc", strings.Repeat("c", 40)
	successorBuild.BinarySHA256 = strings.Repeat("cd", 32)
	next := c.record
	next.ID = "handoff-after-readoption"
	next.Phase = HandoffPrepared
	next.Runs = nil
	next.Predecessor = HandoffParty{Binding: request.Binding, ArtifactPath: c.record.Predecessor.ArtifactPath}
	next.Successor = HandoffParty{
		Binding:      ControllerBinding{Controller: request.Binding.Controller, Build: &successorBuild, Config: request.Binding.Config},
		ArtifactPath: c.record.Successor.ArtifactPath,
	}

	ports := c.predecessorPorts()
	if _, err := BeginHandoff(ports, next); err != nil {
		t.Fatalf("a transition from the re-adopted anchor could not begin: %v", err)
	}
	successor := c.successorPorts()
	successor.Self = ControllerSelfRecord{
		Build: successorBuild, ExecutablePath: c.record.Successor.ArtifactPath, Measured: successorBuild.BinarySHA256,
	}
	activated, err := CompleteHandoff(successor, next.ID)
	if err != nil {
		t.Fatalf("a successor of the re-adopted anchor was refused: %v", err)
	}
	if activated.Phase != HandoffActivated {
		t.Fatalf("phase = %q, want activated", activated.Phase)
	}
	authority, _, err := c.store.CurrentControllerAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if authority.Kind != AuthorityHandoffActivation || authority.Ref != next.ID {
		t.Fatalf("authority = %s %s, want the new activation", authority.Kind, authority.Ref)
	}
}

// A RETRY AFTER A CRASH CONVERGES rather than recording a second authority
// event for the same boundary.
func TestARepeatedReadoptionConvergesOnItsOwnRecord(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	lease := readoptionLease(t, c)
	first, err := ReadoptController(c.store, lease, request)
	if err != nil {
		t.Fatal(err)
	}
	request.Now = request.Now.Add(time.Minute) // a later retry, same boundary
	second, err := ReadoptController(c.store, lease, request)
	if err != nil {
		t.Fatalf("a retry after the authority commit was refused: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("the retry recorded a second authority event: %s then %s", first.ID, second.ID)
	}
	readoptions, err := c.store.ControllerReadoptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(readoptions) != 1 {
		t.Fatalf("%d re-adoption records for one boundary", len(readoptions))
	}
}

// EVERYTHING IT REFUSES.
func TestAReadoptionRefusesWhatItCannotSanction(t *testing.T) {
	for _, test := range []struct {
		name   string
		break_ func(*choreography, *ReadoptionRequest)
		want   string
	}{
		{"no operator reason", func(_ *choreography, r *ReadoptionRequest) {
			r.Reason = ""
		}, "reason is required"},
		{"no resolvable operator", func(_ *choreography, r *ReadoptionRequest) {
			r.Operator = RecordedOperator{}
		}, "operator identity could not be resolved"},
		{"an unattested build", func(_ *choreography, r *ReadoptionRequest) {
			r.Self.Unattested = true
		}, "unattested build"},
		{"a process that is not the generation it would re-adopt", func(_ *choreography, r *ReadoptionRequest) {
			other := *r.Binding.Build
			other.BinarySHA256 = strings.Repeat("ef", 32)
			r.Binding.Build = &other
		}, "not the generation it would re-adopt"},
		{"a published provenance that is not adopted", func(_ *choreography, r *ReadoptionRequest) {
			r.Provenance.Kind = ControllerUnattested
		}, "not an adopted build"},
		{"a binary that does not measure what was published", func(_ *choreography, r *ReadoptionRequest) {
			r.Provenance.BinarySHA256 = strings.Repeat("99", 32)
		}, "published provenance records"},
		{"a generation trusted main has moved past", func(_ *choreography, r *ReadoptionRequest) {
			r.ObserveTrustedMain = func() (RevisionRecord, error) {
				return RevisionRecord{Revision: strings.Repeat("9", 40), Tree: strings.Repeat("8", 40)}, nil
			}
		}, "re-adopt the generation trusted main names"},
		{"trusted main that cannot be observed at all", func(_ *choreography, r *ReadoptionRequest) {
			r.ObserveTrustedMain = func() (RevisionRecord, error) {
				return RevisionRecord{}, fmt.Errorf("the forge did not answer")
			}
		}, "trusted main could not be observed"},
		{"a binary that is not the published artifact", func(c *choreography, r *ReadoptionRequest) {
			// Byte-identical, same build, same provenance - a copy.
			copied := filepath.Join(t.TempDir(), "zenchron-engineering")
			body, err := os.ReadFile(r.Provenance.OutputPath)
			if err != nil {
				c.t.Fatal(err)
			}
			if err := os.WriteFile(copied, body, 0o700); err != nil {
				c.t.Fatal(err)
			}
			r.Self.ExecutablePath = copied
		}, "not the published artifact"},
		{"a transition still in flight", func(c *choreography, _ *ReadoptionRequest) {
			if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
				c.t.Fatal(err)
			}
		}, "is in flight"},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, request := readoptionFixture(t)
			settleLiveRuns(t, c, request.Now)
			test.break_(c, &request)

			_, err := ReadoptController(c.store, readoptionLease(t, c), request)
			if err == nil {
				t.Fatal("the re-adoption was performed")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want one naming %q", err, test.want)
			}
			if _, found, _ := c.store.CurrentControllerAuthority(); found {
				t.Fatal("a refused re-adoption established authority")
			}
		})
	}
}

// LIVE WORK IS NOT CARRIED ACROSS THE BOUNDARY. The fixture's run was created
// by the predecessor generation under the previous configuration, so this
// generation cannot prove it may continue it - and one such run refuses the
// whole operation.
func TestAReadoptionRefusesToStrandLiveWork(t *testing.T) {
	c, request := readoptionFixture(t)
	runs, err := c.store.Runs()
	if err != nil || len(runs) == 0 {
		t.Fatalf("HARNESS PRECONDITION: no live run: %v", err)
	}

	lease := readoptionLease(t, c)
	_, err = ReadoptController(c.store, lease, request)
	if err == nil {
		t.Fatal("a re-adoption carried a run this generation cannot continue")
	}
	if !strings.Contains(err.Error(), "settle it or restore the previous configuration") {
		t.Fatalf("err = %v, want the operator's two options", err)
	}

	// Settled deliberately, exactly as an operator would: now it proceeds.
	if _, err := CancelRun(c.store, c.fixture.runtime.scheduler, request.Now, runs[0].ID, "operator/stop"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadoptController(c.store, lease, request); err != nil {
		t.Fatalf("the re-adoption was still refused after the run settled: %v", err)
	}
}

// A CRASH BETWEEN THE AUTHORITY COMMIT AND THE PROJECTION CONVERGES.
//
// Authority is committed first and the stable entrypoint follows it, so the
// window exists by design. What must not exist is a state that a retry cannot
// resolve: re-running the same re-adoption repairs the pointer and records
// nothing new.
func TestAReadoptionWhoseProjectionDidNotLandIsRepairedByRetrying(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	lease := readoptionLease(t, c)
	if _, err := ReadoptController(c.store, lease, request); err != nil {
		t.Fatal(err)
	}
	// The process died here: authority is durable, the pointer was never
	// written. Nothing repaired it in between.
	pointer := filepath.Join(c.root, StableEntrypointName)
	if _, err := os.Lstat(pointer); err == nil {
		t.Fatal("HARNESS PRECONDITION: the projection already exists")
	}

	resolved, err := ActivateReadoptedGeneration(c.store, c.self, c.root)
	if err != nil {
		t.Fatalf("the projection could not be repaired after the commit: %v", err)
	}
	target, err := os.Readlink(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Dir(c.record.Successor.ArtifactPath) {
		t.Fatalf("the stable entrypoint points at %s, not the re-adopted generation", target)
	}
	if !strings.HasPrefix(resolved, pointer) {
		t.Fatalf("the resolved path %q is not under the stable entrypoint", resolved)
	}
	// AND IT IS IDEMPOTENT: repairing twice is the same pointer, not a second
	// authority event.
	if _, err := ActivateReadoptedGeneration(c.store, c.self, c.root); err != nil {
		t.Fatalf("repairing an already-correct projection failed: %v", err)
	}
	readoptions, err := c.store.ControllerReadoptions()
	if err != nil || len(readoptions) != 1 {
		t.Fatalf("%d re-adoption records after repair: %v", len(readoptions), err)
	}
}

// AND THE STATE DIRECTORY READS AS CONSISTENT AFTERWARDS. This is the operator
// -visible acceptance: the incident left INVARIANT_VIOLATION, and re-adoption
// is what resolves it.
func TestAfterReadoptionTheControllerStatusIsConsistent(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	if _, err := ReadoptController(c.store, readoptionLease(t, c), request); err != nil {
		t.Fatal(err)
	}
	if _, err := ActivateReadoptedGeneration(c.store, c.self, c.root); err != nil {
		t.Fatal(err)
	}

	status, err := DescribeControllerStatus(c.store, c.root, func() (LiveControllerSnapshot, error) {
		return LiveControllerSnapshot{
			Identity: c.self, Role: RoleHeld, WorkAdmission: AdmissionOpen,
			ObservedAt: request.Now,
		}, nil
	}, request.Now)
	if err != nil {
		t.Fatal(err)
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent", status.DurableConsistency)
	}
	if status.Durable.Authority != AuthorityOperatorReadoption {
		t.Fatalf("authority = %q, want the re-adoption to be visible", status.Durable.Authority)
	}
	if status.Durable.Generation == nil || status.Durable.Generation.Version != c.self.Build.Version {
		t.Fatalf("the durable generation is not the re-adopted one: %+v", status.Durable.Generation)
	}
	if status.Projection.State != ProjectionCurrent {
		t.Fatalf("projection = %q, want the entrypoint following the new authority", status.Projection.State)
	}
}

// THE STABLE PATH IS A LEGITIMATE WAY TO INVOKE THE CANONICAL BINARY.
//
// The artifact proof is about file identity, not about the string an operator
// typed: current/zenchron-engineering resolves to the published artifact and is
// exactly how they are told to run the controller.
func TestAReadoptionThroughTheStableEntrypointIsAccepted(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	pointer := filepath.Join(c.root, StableEntrypointName)
	if err := os.Symlink(filepath.Dir(request.Provenance.OutputPath), pointer); err != nil {
		t.Fatal(err)
	}
	request.Self.ExecutablePath = filepath.Join(pointer, filepath.Base(request.Provenance.OutputPath))

	readoption, err := ReadoptController(c.store, readoptionLease(t, c), request)
	if err != nil {
		t.Fatalf("an invocation through the stable entrypoint was refused: %v", err)
	}
	// AND THE CANONICAL PATH IS WHAT GOVERNS. The authority must not name the
	// pointer that was used to reach it: a symlink is a projection, and one
	// that named itself would be a governing root defined in terms of a
	// repairable convenience.
	authority, _, err := c.store.CurrentControllerAuthority()
	if err != nil {
		t.Fatal(err)
	}
	if authority.Artifact != request.Provenance.OutputPath {
		t.Fatalf("authority names %s, want the published artifact %s", authority.Artifact, request.Provenance.OutputPath)
	}
	if readoption.Provenance.OutputPath != request.Provenance.OutputPath {
		t.Fatal("the record does not name the published artifact")
	}
}

// AN INHERITED RUN IS NOT A STRANDED ONE.
//
// A run reaches its current controller through succession admissions in its own
// journal, and each means something only if its transition activated. Without
// that oracle every legitimately inherited run reads as incompatible and
// refuses a re-adoption that should proceed - #282's lesson in a new place.
func TestAReadoptionAcceptsARunInheritedThroughAnActivatedSuccession(t *testing.T) {
	c, request := readoptionFixture(t)
	lease := readoptionLease(t, c)

	// THE HANDOVER IS DRIVEN THROUGH THE REAL SERVICE, because the admissions
	// are the point: the choreography fixture's AdmitSuccession port is a fake
	// that records a run id, and a run with no admissions in its journal
	// cannot demonstrate that the oracle is consulted.
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	service := BindControllerService(c.state, c.root, c.store, c.self, lease, newFakeAdmission())
	expect, err := Expect(c.stored())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ActivateSuccessor(expect, nil); err != nil {
		t.Fatalf("HARNESS PRECONDITION: the handover failed: %v", err)
	}

	runs, err := c.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	inherited := 0
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			continue
		}
		events, err := c.store.Events(run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == EventControllerSuccessionAdmitted {
				inherited++
				break
			}
		}
	}
	if inherited == 0 {
		t.Fatal("HARNESS PRECONDITION: no live run was inherited through an admission")
	}

	// THE BINDING BEING RE-ADOPTED IS EXACTLY THE ONE THAT INHERITED THE RUN.
	//
	// That is the other legitimate use of this operation: establishing a new
	// adopted root with the configuration unchanged, after a chain that cannot
	// continue itself. A CHANGED configuration is a different question, and
	// such a run is stranded by it - correctly, because the configuration is
	// part of the binding an admission names.
	request.Binding = c.record.Successor.Binding
	if _, err := ReadoptController(c.store, lease, request); err != nil {
		t.Fatalf("a run inherited through an activated succession blocked the re-adoption: %v", err)
	}
}

// AND AN ADMISSION WHOSE TRANSITION NEVER ACTIVATED STILL GRANTS NOTHING.
func TestAReadoptionIgnoresAnAdmissionWhoseTransitionFailed(t *testing.T) {
	c, request := readoptionFixture(t)
	if _, err := BeginHandoff(c.predecessorPorts(), c.record); err != nil {
		t.Fatal(err)
	}
	// The successor acquires, writes its admissions, and then fails back: the
	// evidence exists in the journal and the occasion never happened.
	ports := c.successorPorts()
	ports.ProveControlEndpoint = func() error { return fmt.Errorf("the control endpoint did not answer") }
	if _, err := CompleteHandoff(ports, c.record.ID); err == nil {
		t.Fatal("HARNESS PRECONDITION: the transition completed")
	}
	if stored := c.stored(); stored.Phase == HandoffActivated {
		t.Fatalf("HARNESS PRECONDITION: phase %q", stored.Phase)
	}

	_, err := ReadoptController(c.store, readoptionLease(t, c), request)
	if err == nil {
		t.Fatal("a run was carried on the strength of an admission whose transition never activated")
	}
	if !strings.Contains(err.Error(), "cannot continue it") && !strings.Contains(err.Error(), "in flight") {
		t.Fatalf("err = %v, want the run or the transition to block it", err)
	}
}

// CURRENCY MOVING DURING THE PREFLIGHT IS CAUGHT, AND NOTHING IS WRITTEN.
//
// The preflight replays every live run's journal, which takes as long as it
// takes. A revision observed before that is a fact about a branch as it was.
func TestTrustedMainMovingDuringThePreflightWritesNothing(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	request.ObserveTrustedMain = func() (RevisionRecord, error) {
		// By the time currency is proven, somebody has merged.
		return RevisionRecord{Revision: strings.Repeat("7", 40), Tree: strings.Repeat("6", 40)}, nil
	}

	if _, err := ReadoptController(c.store, readoptionLease(t, c), request); err == nil {
		t.Fatal("a generation trusted main had moved past became the governing root")
	}
	if _, found, _ := c.store.CurrentControllerAuthority(); found {
		t.Fatal("authority moved for a re-adoption that was refused")
	}
	readoptions, err := c.store.ControllerReadoptions()
	if err != nil {
		t.Fatal(err)
	}
	if len(readoptions) != 0 {
		t.Fatalf("%d re-adoption records were written by a refused operation", len(readoptions))
	}
}

// THE CRASH GUARANTEE, WITHOUT THE FORGE.
//
// Authority is committed and the process dies before the projection is
// repaired. Trusted main then moves and governance goes away. Re-running the
// same binary must still repair the pointer, because there is no adoption
// decision left to make - it was made, and it is durable.
func TestAnAlreadyCommittedReadoptionRepairsWithoutObservingTrustedMain(t *testing.T) {
	c, request := readoptionFixture(t)
	settleLiveRuns(t, c, request.Now)
	lease := readoptionLease(t, c)
	first, err := ReadoptController(c.store, lease, request)
	if err != nil {
		t.Fatal(err)
	}
	// The process died here: authority durable, no projection.
	pointer := filepath.Join(c.root, StableEntrypointName)
	if _, err := os.Lstat(pointer); err == nil {
		t.Fatal("HARNESS PRECONDITION: the projection already exists")
	}

	// Main has moved and the forge is unreachable. Neither is asked.
	observed := 0
	request.ObserveTrustedMain = func() (RevisionRecord, error) {
		observed++
		return RevisionRecord{}, fmt.Errorf("the governance credential is unavailable")
	}
	request.Now = request.Now.Add(time.Hour)

	again, err := ReadoptController(c.store, lease, request)
	if err != nil {
		t.Fatalf("the committed re-adoption could not be recovered: %v", err)
	}
	if observed != 0 {
		t.Fatal("the recovery path asked the forge, which is exactly what it must not depend on")
	}
	if again.ID != first.ID {
		t.Fatalf("recovery recorded a second authority event: %s then %s", first.ID, again.ID)
	}
	if _, err := ActivateReadoptedGeneration(c.store, c.self, c.root); err != nil {
		t.Fatalf("the projection could not be repaired: %v", err)
	}
	target, err := os.Readlink(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Dir(request.Provenance.OutputPath) {
		t.Fatalf("the stable entrypoint points at %s", target)
	}
	readoptions, err := c.store.ControllerReadoptions()
	if err != nil || len(readoptions) != 1 {
		t.Fatalf("%d re-adoption records after recovery: %v", len(readoptions), err)
	}
}
