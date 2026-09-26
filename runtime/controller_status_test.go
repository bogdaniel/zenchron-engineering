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
	if strings.Contains(string(encoded), `"role"`) {
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
			Identity: fixture.self, Role: RoleNotHeld,
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
	if status.Live.Snapshot.Role == RoleHeld {
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
			Identity: fixture.self, Role: RoleHeld,
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
			Identity: predecessor, Role: RoleNotHeld, WorkAdmission: AdmissionClosed,
		}, DurableConsistent, "durably active"},
		{"still admitting work", LiveControllerSnapshot{
			Identity: predecessor, Role: RoleNotHeld, WorkAdmission: AdmissionOpen,
		}, DurableViolation, "admitting work"},
		{"still holding the role", LiveControllerSnapshot{
			Identity: predecessor, Role: RoleHeld, WorkAdmission: AdmissionClosed,
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
		// A new transition ACTIVATES while the live half is being observed.
		// It has to activate rather than merely be written: what status reads
		// is the activation that governs, so a row appearing elsewhere is not
		// the durable state moving under the observation.
		next := ControllerHandoff{
			ID: "handoff-later", Phase: HandoffRevalidated,
			Predecessor:   fixture.record.Successor,
			Successor:     fixture.record.Predecessor,
			RecoveryOwner: "later", UpdatedAt: statusNow().Add(time.Minute),
		}
		if wrote, err := fixture.store.PutControllerHandoff(next, ""); err != nil || !wrote {
			t.Fatalf("write the later transition: %v wrote=%v", err, wrote)
		}
		activated := next
		activated.Phase = HandoffActivated
		if wrote, err := fixture.store.ActivateControllerHandoff(activated, HandoffRevalidated); err != nil || !wrote {
			t.Fatalf("activate the later transition: %v wrote=%v", err, wrote)
		}
		// Observed state that WOULD look like a violation against the old read.
		return LiveControllerSnapshot{
			Identity: predecessor, Role: RoleHeld,
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
	if status.Live.Snapshot.Role != RoleHeld {
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
	if snapshot.Role != RoleHeld {
		t.Fatal("a live service did not observe its own role")
	}
	if err := fixture.service.lease.Release(); err != nil {
		t.Fatal(err)
	}
	after := fixture.service.DescribeLiveController(fixture.record.ID, statusNow())
	if after.Role != RoleNotHeld {
		t.Fatalf("a released lease was observed as %q, want not_held", after.Role)
	}
}

// ACTIVATED, ROLE HELD, ADMISSION CLOSED, POINTER CURRENT is a legitimate
// transient - the state #276 leaves between activation and opening service -
// and must not classify as a broken invariant.
func TestActivatedAndHoldingWithoutServingIsValid(t *testing.T) {
	fixture := activatedFixture(t)
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		return fixture.service.DescribeLiveController(fixture.record.ID, statusNow()), nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent (findings %v)", status.DurableConsistency, status.Findings)
	}
	if status.Serving != NotServing {
		t.Fatalf("serving = %q, want not_serving", status.Serving)
	}
	if status.Projection.State != ProjectionCurrent {
		t.Fatalf("projection = %q, want current", status.Projection.State)
	}
	if status.Live.Snapshot.Role != RoleHeld {
		t.Fatal("the activated controller did not observe its own role")
	}
}

// REACHING THE RIGHT PROCESS IS NOT OBSERVING ITS SERVICE. An endpoint that
// answers identity without reporting role or admission leaves both unknown, and
// serving stays unknown with it.
func TestReachableRightGenerationWithUnreportedFactsStaysUnknown(t *testing.T) {
	fixture := activatedFixture(t)
	status, err := DescribeControllerStatus(fixture.store, fixture.root, func() (LiveControllerSnapshot, error) {
		// Identity only: the zero values of the other two mean unknown.
		return LiveControllerSnapshot{Identity: fixture.self, ObservedAt: statusNow()}, nil
	}, statusNow())
	if err != nil {
		t.Fatal(err)
	}
	if status.Serving != ServingUnknown {
		t.Fatalf("serving = %q, want unknown: identity is not service authority", status.Serving)
	}
	if status.DurableConsistency != DurableConsistent {
		t.Fatalf("durable consistency = %q, want consistent", status.DurableConsistency)
	}
	// Both gaps are named rather than silently defaulted.
	var admission, role bool
	for _, finding := range status.Findings {
		if strings.Contains(finding, "work admission") {
			admission = true
		}
		if strings.Contains(finding, "role ownership") {
			role = true
		}
	}
	if !admission || !role {
		t.Fatalf("findings %v do not name both unobserved facts", status.Findings)
	}
	// And neither absence serializes as a positive claim.
	encoded, err := json.Marshal(status.Live.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"role"`) || strings.Contains(string(encoded), `"work_admission"`) {
		t.Fatalf("an unobserved fact was serialized as a value: %s", encoded)
	}
}

// THE UPDATE RESULT STAYS DIMENSIONAL. A projection that did not follow is a
// completed succession with a stale pointer, not a failed one.
func TestUpdateResultKeepsActivationAndProjectionApart(t *testing.T) {
	succeeded := func() ControllerUpdateResult {
		var result ControllerUpdateResult
		result.Schema, result.HandoffID = ControllerUpdateSchema, "handoff-1"
		result.Activation.Outcome, result.Activation.Phase = UpdateSucceeded, HandoffActivated
		result.Projection.Outcome = UpdateSucceeded
		result.WorkAdmission.Outcome = UpdateSucceeded
		result.Predecessor.Outcome = UpdateSucceeded
		return result
	}

	for _, test := range []struct {
		name   string
		mutate func(*ControllerUpdateResult)
		want   UpdateOutcome
	}{
		{"everything worked", nil, UpdateCompleted},
		{"the pointer did not follow", func(r *ControllerUpdateResult) {
			r.Projection.Outcome, r.Projection.Detail = UpdateDrifted, "a directory occupies the entrypoint"
		}, UpdateCompletedWithDrift},
		{"the projection repair failed outright", func(r *ControllerUpdateResult) {
			r.Projection.Outcome = UpdateFailed
		}, UpdateCompletedWithDrift},
		{"the activation failed", func(r *ControllerUpdateResult) {
			r.Activation.Outcome = UpdateFailed
		}, UpdateFailed},
		{"service could not be opened", func(r *ControllerUpdateResult) {
			r.WorkAdmission.Outcome = UpdateFailed
		}, UpdateFailed},
		// The predecessor lingering is worth reporting and is not a failure of
		// the succession: it is drained and holds nothing.
		{"the predecessor has not exited yet", func(r *ControllerUpdateResult) {
			r.Predecessor.Outcome = UpdateSkipped
		}, UpdateCompleted},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := succeeded()
			if test.mutate != nil {
				test.mutate(&result)
			}
			result.settle()
			if result.Overall != test.want {
				t.Fatalf("overall = %q, want %q", result.Overall, test.want)
			}
			// The dimensions survive into the document whatever the overall says.
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{`"activation"`, `"projection"`, `"work_admission"`, `"predecessor"`, `"schema"`} {
				if !strings.Contains(string(encoded), field) {
					t.Fatalf("the result dropped %s: %s", field, encoded)
				}
			}
		})
	}
}
