package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func statusNow() time.Time { return time.Unix(1700000009, 0).UTC() }

// activatedFixture is a state directory with one activated transition and a
// projection pointing at the activated generation.
func activatedFixture(t *testing.T) *serviceFixture {
	t.Helper()
	fixture := newServiceFixture(t)
	service := fixture.start(t)
	activated, err := service.ActivateSuccessor(fixture.expectation(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.record = activated
	t.Cleanup(func() { _ = service.lease.Release() })
	fixture.service = service
	return fixture
}

// AN UNREACHABLE ENDPOINT PRODUCES UNKNOWN, never an invented "none". The role
// lock carries no metadata, so nothing on disk can answer who holds it.
func TestUnobservableControllerIsUnknownAndNotAbsent(t *testing.T) {
	fixture := activatedFixture(t)
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return LiveControllerSnapshot{}, fmt.Errorf("no control endpoint is available")
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.Serving != ServingUnknown {
		t.Fatalf("serving = %q, want unknown", status.Serving)
	}
	if status.Live.Reachable || status.Live.Snapshot != nil {
		t.Fatal("an unreachable endpoint produced a live snapshot")
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q: an unobservable process is not a durable problem", status.DurableConsistency)
	}
	// The whole document must not contain a claim that nobody holds the role.
	encoded, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"role_held_by_this_process"`) {
		t.Fatalf("an unobserved role was reported as a fact: %s", encoded)
	}
	var named bool
	for _, finding := range status.Findings {
		if strings.Contains(finding, "unknown") {
			named = true
		}
	}
	if !named {
		t.Fatalf("the status did not say what it could not observe: %v", status.Findings)
	}
}

// A SOCKET IS NOT A ROLE. An endpoint that answers while reporting that it does
// NOT hold the role must not be read as owning anything.
func TestReachableEndpointWithoutTheRoleIsNotAnOwner(t *testing.T) {
	fixture := activatedFixture(t)
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return LiveControllerSnapshot{
			Identity: fixture.self, RoleHeldByThisProcess: false,
			WorkAdmission: AdmissionWithheld, ObservedAt: statusNow(),
		}, nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.Serving != NotServing {
		t.Fatalf("serving = %q, want not_serving", status.Serving)
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent", status.DurableConsistency)
	}
	if status.Live.Snapshot.RoleHeldByThisProcess {
		t.Fatal("reachability was read as role ownership")
	}
}

// THE LEGITIMATE ZERO-AVAILABILITY STATE: durably active, provably not serving,
// pointer stale. Consistent, and not serving, and drifted - three answers.
func TestActivatedAndNotServingIsConsistent(t *testing.T) {
	fixture := activatedFixture(t)
	// Point the projection at the predecessor, as a crash before the flip does.
	pointer := filepath.Join(fixture.root, StableEntrypointName)
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(fixture.record.Predecessor.ArtifactPath), pointer); err != nil {
		t.Fatal(err)
	}
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return LiveControllerSnapshot{
			Identity: fixture.self, RoleHeldByThisProcess: true,
			WorkAdmission: AdmissionClosed, ObservedAt: statusNow(),
		}, nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent", status.DurableConsistency)
	}
	if status.Serving != NotServing {
		t.Fatalf("serving = %q, want not_serving", status.Serving)
	}
	if status.Projection.State != ProjectionDrift {
		t.Fatalf("projection = %q, want drift", status.Projection.State)
	}
	// PROJECTION DRIFT IS NOT A DURABLE PROBLEM.
	if status.DurableConsistency == DurableViolation {
		t.Fatal("a stale symlink made durable state invalid")
	}
}

// A STALE PREDECESSOR IS NOT A VIOLATION, until it owns something.
func TestDrainedPredecessorIsObservableWithoutBeingAViolation(t *testing.T) {
	fixture := activatedFixture(t)
	predecessorBuild := *fixture.record.Predecessor.Binding.Build
	predecessor := ControllerSelfRecord{Build: predecessorBuild, Measured: predecessorBuild.BinarySHA256}

	for _, test := range []struct {
		name     string
		snapshot LiveControllerSnapshot
		want     DurableConsistency
		mustName string
	}{
		{"drained and holding nothing", LiveControllerSnapshot{
			Identity: predecessor, RoleHeldByThisProcess: false, WorkAdmission: AdmissionClosed,
		}, DurableConsistent, "durably active"},
		{"still admitting work", LiveControllerSnapshot{
			Identity: predecessor, RoleHeldByThisProcess: false, WorkAdmission: AdmissionOpen,
		}, DurableViolation, "admitting work"},
		{"still holding the role", LiveControllerSnapshot{
			Identity: predecessor, RoleHeldByThisProcess: true, WorkAdmission: AdmissionClosed,
		}, DurableViolation, "holds the controller role"},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot := test.snapshot
			snapshot.ObservedAt = statusNow()
			status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
				return snapshot, nil
			}, statusNow())
			if err != nil {
				t.Fatal(err)
			}
			if status.DurableConsistency != test.want {
				t.Fatalf("durable consistency = %q, want %q (findings %v)",
					status.DurableConsistency, test.want, status.Findings)
			}
			var named bool
			for _, finding := range status.Findings {
				if strings.Contains(finding, test.mustName) {
					named = true
				}
			}
			if !named {
				t.Fatalf("findings %v do not name %q", status.Findings, test.mustName)
			}
		})
	}
}

// A TORN READ IS NOT EVIDENCE. Durable state moving during collection reports
// instability rather than a violation nobody observed.
func TestDurableStateMovingDuringCollectionIsNotAViolation(t *testing.T) {
	fixture := activatedFixture(t)
	predecessorBuild := *fixture.record.Predecessor.Binding.Build
	predecessor := ControllerSelfRecord{Build: predecessorBuild, Measured: predecessorBuild.BinarySHA256}

	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		// A new transition settles while the live half is being observed.
		next := ControllerHandoff{
			ID: "handoff-later", Phase: HandoffActivated,
			Predecessor:   fixture.record.Successor,
			Successor:     fixture.record.Predecessor,
			RecoveryOwner: "later", UpdatedAt: statusNow().Add(time.Minute),
		}
		if wrote, err := fixture.store.PutControllerHandoff(next, ""); err != nil || !wrote {
			t.Fatalf("write the later transition: %v wrote=%v", err, wrote)
		}
		// Observed state that WOULD look like a violation against the old read.
		return LiveControllerSnapshot{
			Identity: predecessor, RoleHeldByThisProcess: true,
			WorkAdmission: AdmissionOpen, ObservedAt: statusNow(),
		}, nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.DurableConsistency != DurableUnstable {
		t.Fatalf("durable consistency = %q, want snapshot_unstable (findings %v)",
			status.DurableConsistency, status.Findings)
	}
	if status.Serving != ServingUnknown {
		t.Fatalf("serving = %q, want unknown during an unstable snapshot", status.Serving)
	}
}

// Status changes nothing: no repair, no lock, no record.
func TestStatusMutatesNothing(t *testing.T) {
	fixture := activatedFixture(t)
	pointer := filepath.Join(fixture.root, StableEntrypointName)
	if err := os.Remove(pointer); err != nil {
		t.Fatal(err)
	}
	before, err := fixture.store.ControllerHandoffs()
	if err != nil {
		t.Fatal(err)
	}
	status, err := DescribeControllerStatus(fixture.store, fixture.root, nil, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.Projection.State != ProjectionMissing {
		t.Fatalf("projection = %q, want missing", status.Projection.State)
	}
	if _, err := os.Lstat(pointer); err == nil {
		t.Fatal("collecting status repaired the projection")
	}
	after, err := fixture.store.ControllerHandoffs()
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("collecting status wrote a durable record")
	}
	// And the role is free for anybody to take, so status took no lock.
	lease, err := AcquireControllerRole(fixture.state)
	if err == nil {
		_ = lease.Release()
		t.Fatal("the role was free, so the service fixture is not holding it")
	}
}

// Every fact carries where it came from, so nothing downstream can mistake the
// pointer for the record.
func TestStatusCarriesProvenanceAndAVersionedSchema(t *testing.T) {
	fixture := activatedFixture(t)
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return fixture.service.DescribeLiveController(fixture.record.ID, statusNow()), nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.Schema != ControllerStatusSchema {
		t.Fatalf("schema = %q, want %q", status.Schema, ControllerStatusSchema)
	}
	if status.Durable.Source != FromActivationRecord {
		t.Fatalf("durable source = %q", status.Durable.Source)
	}
	if status.Projection.Source != FromStableEntrypoint {
		t.Fatalf("projection source = %q", status.Projection.Source)
	}
	if status.Live.Source != FromLiveEndpoint {
		t.Fatalf("live source = %q", status.Live.Source)
	}
	// The live half is the service's own coherent snapshot, and it knows it
	// holds the role because it exercised the capability to find out.
	if !status.Live.Snapshot.RoleHeldByThisProcess {
		t.Fatal("the serving process did not observe its own role")
	}
	if status.Live.Snapshot.WorkAdmission != AdmissionClosed {
		t.Fatalf("work admission = %q, want closed before service opens", status.Live.Snapshot.WorkAdmission)
	}
}

// A released lease is observed as not held, because the snapshot exercises the
// capability rather than reading a flag that could be stale.
func TestLiveSnapshotFollowsTheCapability(t *testing.T) {
	fixture := activatedFixture(t)
	snapshot := fixture.service.DescribeLiveController(fixture.record.ID, statusNow())
	if !snapshot.RoleHeldByThisProcess {
		t.Fatal("a live service did not observe its own role")
	}
	if err := fixture.service.lease.Release(); err != nil {
		t.Fatal(err)
	}
	after := fixture.service.DescribeLiveController(fixture.record.ID, statusNow())
	if after.RoleHeldByThisProcess {
		t.Fatal("a released lease was still observed as held")
	}
}
