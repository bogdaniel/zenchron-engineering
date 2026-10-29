package runtime

import (
	"strings"
	"testing"
	"time"
)

// activeGeneration is the durably active generation in these fixtures.
// watching is the default case: the watcher IS the durably active generation.
// A test that cares about the difference supplies its own self.
func watching(status ControllerStatus) WatcherObservation {
	generation := activeGeneration()
	return WatcherObservation{
		Status: status,
		Self:   ControllerSelfRecord{Build: generation, Measured: generation.BinarySHA256},
	}
}

func activeGeneration() ControllerBuild {
	return attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
}

// observedStatus is a fully known, fully agreeing observation. Each test
// degrades exactly the facts it is about.
func observedStatus() ControllerStatus {
	generation := activeGeneration()
	return ControllerStatus{
		Schema: ControllerStatusSchema, ObservedAt: time.Unix(1700000009, 0).UTC(),
		Durable: DurableActive{
			Generation: &generation, HandoffID: "handoff-1", Phase: HandoffActivated,
			ArtifactDir: "/controller/main-bbbbbbbb", Source: FromActivationRecord,
		},
		Live: LiveObservation{Reachable: true, Source: FromLiveEndpoint, Snapshot: &LiveControllerSnapshot{
			Identity: ControllerSelfRecord{Build: generation, Measured: generation.BinarySHA256},
			Role:     RoleHeld, WorkAdmission: AdmissionOpen, HandoffID: "handoff-1",
		}},
		Projection: ProjectionObservation{
			State: ProjectionCurrent, Target: "/controller/main-bbbbbbbb", Source: FromStableEntrypoint,
		},
		DurableConsistency: DurableConsistent, Serving: Serving,
	}
}

// THE DECISION TABLE. It is the whole review surface of this slice: an
// observation in, an intent out, and no side effect in between.
func TestReconciliationDecisionTable(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ControllerStatus)
		want   ReconciliationAction
		reason string
	}{
		{"everything agrees", nil, ReconcileNone, "agree"},
		{"a torn observation", func(s *ControllerStatus) {
			s.DurableConsistency = DurableUnstable
		}, ReconcileNone, "torn observation"},
		// The category that matters most: the watcher does not pick a winner.
		{"two authorities", func(s *ControllerStatus) {
			s.DurableConsistency = DurableViolation
		}, ReconcileRefuse, "not this component's decision"},
		{"no activation recorded", func(s *ControllerStatus) {
			s.DurableConsistency, s.Durable.Generation = DurableUnrecorded, nil
		}, ReconcileNone, "no durable target"},
		{"the record names no generation", func(s *ControllerStatus) {
			s.Durable.Generation = nil
		}, ReconcileUnknown, "names no active generation"},
		{"a stale pointer", func(s *ControllerStatus) {
			s.Projection.State = ProjectionDrift
		}, ReconcileRepairProjection, "durably active"},
		{"a missing pointer", func(s *ControllerStatus) {
			s.Projection.State = ProjectionMissing
		}, ReconcileRepairProjection, "durably active"},
		{"this process is the activated generation and is not serving", func(s *ControllerStatus) {
			s.Live.Snapshot.WorkAdmission = AdmissionClosed
			s.Serving = NotServing
		}, ReconcileResumeActivatedService, "not admitting work"},
		{"the observed controller did not report its admission", func(s *ControllerStatus) {
			s.Live.Snapshot.WorkAdmission = AdmissionUnknown
			s.Serving = ServingUnknown
		}, ReconcileUnknown, "did not report"},
		// AN UNREACHABLE ENDPOINT IS NOT A DEAD CONTROLLER, and a dead
		// controller would not be a free role either.
		{"nothing observable", func(s *ControllerStatus) {
			s.Live = LiveObservation{Reachable: false, Detail: "no control endpoint", Source: FromLiveEndpoint}
			s.Serving = ServingUnknown
		}, ReconcileNone, "not an actionable one"},
		// A live process of another generation, owning nothing, is ordinary.
		{"a drained predecessor is visible", func(s *ControllerStatus) {
			other := attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
			s.Live.Snapshot.Identity = ControllerSelfRecord{Build: other, Measured: other.BinarySHA256}
			s.Live.Snapshot.Role, s.Live.Snapshot.WorkAdmission = RoleNotHeld, AdmissionClosed
			s.Serving = NotServing
		}, ReconcileNone, "agree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := observedStatus()
			if test.mutate != nil {
				test.mutate(&status)
			}
			intent := ClassifyReconciliation(watching(status))
			if intent.Action != test.want {
				t.Fatalf("action = %q, want %q (reason %q)", intent.Action, test.want, intent.Reason)
			}
			if !strings.Contains(intent.Reason, test.reason) {
				t.Fatalf("reason %q does not name %q", intent.Reason, test.reason)
			}
		})
	}
}

// AN UNREACHABLE ENDPOINT NEVER IMPLIES DEATH, ABSENCE OR A FREE ROLE, and a
// reachable one never implies authority. Both directions, because both
// inferences are tempting and both were removed from the layers below.
func TestReachabilityImpliesNothingAboutAuthority(t *testing.T) {
	unreachable := observedStatus()
	unreachable.Live = LiveObservation{Reachable: false, Detail: "connection refused", Source: FromLiveEndpoint}
	unreachable.Serving = ServingUnknown
	if intent := ClassifyReconciliation(watching(unreachable)); intent.Action.Mutating() {
		t.Fatalf("an unreachable endpoint produced %q", intent.Action)
	}

	// Reachable, right generation, but reporting neither fact: still nothing.
	silent := observedStatus()
	silent.Live.Snapshot.Role = RoleUnknown
	silent.Live.Snapshot.WorkAdmission = AdmissionUnknown
	silent.Serving = ServingUnknown
	if intent := ClassifyReconciliation(watching(silent)); intent.Action.Mutating() {
		t.Fatalf("a silent endpoint produced %q", intent.Action)
	}
}

// AN INVARIANT VIOLATION IS REFUSED RATHER THAN REPAIRED, including when a
// repair looks obvious. Deciding which claimant is right is the authority
// decision this component may not make.
func TestViolationIsRefusedEvenWhenARepairLooksObvious(t *testing.T) {
	status := observedStatus()
	other := attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	status.Live.Snapshot.Identity = ControllerSelfRecord{Build: other, Measured: other.BinarySHA256}
	status.Live.Snapshot.Role, status.Live.Snapshot.WorkAdmission = RoleHeld, AdmissionOpen
	status.Projection.State = ProjectionDrift // a repair the watcher could "helpfully" attempt
	status.DurableConsistency = DurableViolation

	intent := ClassifyReconciliation(watching(status))
	if intent.Action != ReconcileRefuse {
		t.Fatalf("action = %q, want refuse", intent.Action)
	}
	if intent.Action.Mutating() {
		t.Fatal("a violation produced a mutating intent")
	}
	// The evidence survives for whoever handles it.
	if intent.HandoffID == "" || intent.Generation == nil {
		t.Fatal("the refusal discarded the subject it refused about")
	}
}

// THE METAMORPHIC LAW: replacing a known fact with an unknown one may remove an
// action and may never introduce a more powerful one.
//
// This is the property that keeps ignorance from being productive. A watcher
// that reacted to missing information by doing MORE would be treating absence
// as permission, which is the failure mode every layer below this one was
// built to prevent.
func TestUnknownFactsNeverIncreasePower(t *testing.T) {
	bases := map[string]func() ControllerStatus{
		"agreeing":      observedStatus,
		"stale pointer": func() ControllerStatus { s := observedStatus(); s.Projection.State = ProjectionDrift; return s },
		"not serving": func() ControllerStatus {
			s := observedStatus()
			s.Live.Snapshot.WorkAdmission = AdmissionClosed
			return s
		},
		"missing pointer": func() ControllerStatus { s := observedStatus(); s.Projection.State = ProjectionMissing; return s },
	}
	degradations := map[string]func(*ControllerStatus){
		"the role becomes unknown":       func(s *ControllerStatus) { s.Live.Snapshot.Role = RoleUnknown },
		"admission becomes unknown":      func(s *ControllerStatus) { s.Live.Snapshot.WorkAdmission = AdmissionUnknown },
		"the endpoint becomes silent":    func(s *ControllerStatus) { s.Live = LiveObservation{Source: FromLiveEndpoint} },
		"the generation becomes unknown": func(s *ControllerStatus) { s.Durable.Generation = nil },
		"the transition becomes unknown": func(s *ControllerStatus) { s.Durable.HandoffID = "" },
		"the snapshot becomes unstable":  func(s *ControllerStatus) { s.DurableConsistency = DurableUnstable },
		"the projection becomes unknown": func(s *ControllerStatus) { s.Projection = ProjectionObservation{Source: FromStableEntrypoint} },
	}
	for baseName, base := range bases {
		known := ClassifyReconciliation(watching(base()))
		for degradationName, degrade := range degradations {
			t.Run(baseName+", "+degradationName, func(t *testing.T) {
				status := base()
				degrade(&status)
				degraded := ClassifyReconciliation(watching(status))
				if degraded.Action.Power() > known.Action.Power() {
					t.Fatalf("losing a fact raised the action from %q to %q", known.Action, degraded.Action)
				}
			})
		}
	}
}

// The classification is deterministic and reads nothing but its argument: the
// same observation always yields the same intent.
func TestClassificationIsDeterministicAndPure(t *testing.T) {
	status := observedStatus()
	status.Projection.State = ProjectionDrift
	first := ClassifyReconciliation(watching(status))
	for i := 0; i < 32; i++ {
		if again := ClassifyReconciliation(watching(status)); again != first {
			t.Fatalf("classification %d differed: %+v vs %+v", i, again, first)
		}
	}
	// And it takes its subject from the record rather than constructing one.
	if first.HandoffID != status.Durable.HandoffID || first.Generation != status.Durable.Generation {
		t.Fatal("the intent named a subject the record did not")
	}
}

// A VALUE THIS BUILD DOES NOT RECOGNISE IS NOT A SAFE ONE. The zero value and
// any future member must fail closed rather than fall through the default arm
// of a switch into whatever the last case happened to be.
func TestUnrecognisedEnumValuesCannotAuthorizeAnything(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*ControllerStatus)
	}{
		{"a durable verdict from a future build", func(s *ControllerStatus) {
			s.DurableConsistency = DurableConsistency("reconciling_with_quorum")
			s.Projection.State = ProjectionDrift
		}},
		{"an empty durable verdict", func(s *ControllerStatus) {
			s.DurableConsistency = DurableConsistency("")
			s.Projection.State = ProjectionDrift
		}},
		{"an admission state from a future build", func(s *ControllerStatus) {
			s.Live.Snapshot.WorkAdmission = WorkAdmissionState("quiescing")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			status := observedStatus()
			test.mutate(&status)
			intent := ClassifyReconciliation(watching(status))
			if intent.Action != ReconcileUnknown {
				t.Fatalf("action = %q, want unknown", intent.Action)
			}
			if intent.Action.Mutating() {
				t.Fatal("an unrecognised value authorized a mutation")
			}
			if !strings.Contains(intent.Reason, "recognises") {
				t.Fatalf("reason %q does not say the value was unrecognised", intent.Reason)
			}
		})
	}
}

// THE ENDPOINT'S IDENTITY IS NOT THIS PROCESS'S. A remote controller of the
// right generation is not grounds for THIS controller to resume service.
func TestAnotherProcessBeingTheActiveGenerationIsNotSelfObservation(t *testing.T) {
	status := observedStatus()
	status.Live.Snapshot.WorkAdmission = AdmissionClosed
	status.Serving = NotServing

	// The endpoint is the activated generation; the watcher is not.
	stranger := attestedBuild(ControllerAdopted, predecessorRevision, "tree-a", strings.Repeat("ab", 32))
	observation := WatcherObservation{
		Status: status,
		Self:   ControllerSelfRecord{Build: stranger, Measured: stranger.BinarySHA256},
	}
	if intent := ClassifyReconciliation(observation); intent.Action == ReconcileResumeActivatedService {
		t.Fatal("a watcher of another generation asked to resume somebody else's service")
	}

	// And the reverse: this process IS the activated generation, but the
	// endpoint observed is somebody else. Still not a self-observation.
	elsewhere := observedStatus()
	elsewhere.Live.Snapshot.Identity = ControllerSelfRecord{Build: stranger, Measured: stranger.BinarySHA256}
	elsewhere.Live.Snapshot.WorkAdmission = AdmissionClosed
	if intent := ClassifyReconciliation(watching(elsewhere)); intent.Action == ReconcileResumeActivatedService {
		t.Fatal("an observation of another process was read as this process not serving")
	}

	// An unattested watcher can never match a generation either.
	unattested := WatcherObservation{Status: status, Self: ControllerSelfRecord{Unattested: true}}
	if intent := ClassifyReconciliation(unattested); intent.Action == ReconcileResumeActivatedService {
		t.Fatal("an unattested process asked to resume an adopted generation's service")
	}
}

// Self is the active generation, the endpoint is this process, and it did not
// report its gate: unknown, not resume.
func TestSelfActiveWithUnreportedAdmissionIsUnknown(t *testing.T) {
	status := observedStatus()
	status.Live.Snapshot.WorkAdmission = AdmissionUnknown
	status.Serving = ServingUnknown
	intent := ClassifyReconciliation(watching(status))
	if intent.Action != ReconcileUnknown {
		t.Fatalf("action = %q, want unknown", intent.Action)
	}
}
