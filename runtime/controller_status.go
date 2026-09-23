package runtime

// WHAT THE OPERATOR IS TOLD, and how carefully.
//
// Every dimension this stack spent a dozen changes separating - durable
// activation, role ownership, work admission, generation identity, the stable
// entrypoint - can be flattened again in one status line. "controller: G2,
// status: healthy" throws away precisely the distinctions that make the
// protocol correct, and worse, it invites the reader to infer the ones it does
// not have.
//
// SO NOTHING IS INFERRED FROM THE PROJECTION except the projection's own state,
// and NOTHING IS INFERRED FROM THE ENDPOINT'S EXISTENCE at all. The role lock
// carries no metadata on purpose (see controller_role.go), so there is no file
// to read that says who holds it. A live process can report that it holds the
// role, because it can exercise the capability to find out; an absent process
// reports nothing, and the honest word for that is UNKNOWN rather than "none".
//
// UNKNOWN IS FIRST CLASS. An unreachable endpoint means role ownership and work
// admission are unobserved, not absent. Inventing "none" would let a status
// command manufacture the very fact - nobody is serving - that an operator is
// asking about.
//
// A TORN READ IS NOT EVIDENCE. Durable state is read before and after the live
// observations, and a status that spans a change reports SNAPSHOT_UNSTABLE
// instead of classifying a combination that never existed at any instant. The
// comparison is over the exact transition - id, phase, active generation - and
// not merely over the generation pair, because #270 already established that
// the same pair can belong to two different transitions.
//
// THREE CONCLUSIONS, NOT ONE. Durable consistency, serving state and projection
// state are independent, and a perfectly legitimate answer is "consistent, not
// serving, projection drifted". A single red/green light cannot say that, and
// an operator upgrading a controller needs exactly that sentence.

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ControllerStatusSchema is the machine-readable contract. It is versioned from
// the first release rather than later: inspect-self already taught this
// repository what an accidentally-changed JSON shape costs, and the watcher
// will consume this document rather than scrape prose.
const ControllerStatusSchema = "zenchron.controller-status/v1"

// Provenance names WHERE a fact came from. It is carried in the data rather
// than in a comment so that future code cannot quietly treat the projection's
// target as though it were the durable active generation.
type Provenance string

const (
	FromActivationRecord   Provenance = "activation_record"
	FromStableEntrypoint   Provenance = "stable_entrypoint"
	FromLiveEndpoint       Provenance = "live_endpoint"
	FromLiveRoleCapability Provenance = "live_role_capability_snapshot"
)

// DurableConsistency is the verdict about state that is written down.
type DurableConsistency string

const (
	DurableConsistent DurableConsistency = "consistent"
	DurableViolation  DurableConsistency = "invariant_violation"
	DurableUnstable   DurableConsistency = "snapshot_unstable"
	DurableUnrecorded DurableConsistency = "no_activation_recorded"
)

// ServingState is the verdict about live service.
type ServingState string

const (
	Serving        ServingState = "serving"
	NotServing     ServingState = "not_serving"
	ServingUnknown ServingState = "unknown"
)

// ProjectionState is the verdict about the operator's stable path.
type ProjectionState string

const (
	ProjectionCurrent ProjectionState = "current"
	ProjectionDrift   ProjectionState = "drift"
	ProjectionMissing ProjectionState = "missing"
	ProjectionInvalid ProjectionState = "invalid"
)

// WorkAdmissionState is what a live controller says about its own gate.
//
// THE ZERO VALUE IS UNKNOWN, deliberately. A partially populated observation
// must not read as "closed": not observed is not the same as observed to be
// false, and the difference between them is the difference between "nobody is
// serving" and "nobody looked".
type WorkAdmissionState string

const (
	AdmissionUnknown  WorkAdmissionState = ""
	AdmissionOpen     WorkAdmissionState = "open"
	AdmissionWithheld WorkAdmissionState = "withheld"
	AdmissionClosed   WorkAdmissionState = "closed"
)

// RoleObservation is what a live controller says about its own role, and the
// zero value is unknown for the same reason.
//
// It is not a bool. A bool has no third state, so an observation that could not
// establish role ownership would serialize as false - a positive claim nobody
// made, about the one fact this architecture is most careful never to infer.
type RoleObservation string

const (
	RoleUnknown RoleObservation = ""
	RoleHeld    RoleObservation = "held"
	RoleNotHeld RoleObservation = "not_held"
)

// LiveControllerSnapshot is ONE coherent observation a serving process makes of
// itself.
//
// It is one operation rather than five queries because the five answers move
// independently: sampling admission, then the role, then the handoff would let
// a status report a combination that never existed - admission open and the
// role gone, when in truth the drain closed one before releasing the other.
type LiveControllerSnapshot struct {
	Identity ControllerSelfRecord `json:"identity"`
	// Role is derived by EXERCISING the capability, not by consulting a flag.
	// There is no Held() to read; the only way to learn this is to do something
	// under the lease and see whether it was allowed.
	Role          RoleObservation    `json:"role,omitempty"`
	WorkAdmission WorkAdmissionState `json:"work_admission,omitempty"`
	HandoffID     string             `json:"handoff_id,omitempty"`
	ObservedAt    time.Time          `json:"observed_at"`
}

// DurableActive is the generation the durable record says is active.
type DurableActive struct {
	Generation *ControllerBuild `json:"generation,omitempty"`
	HandoffID  string           `json:"handoff_id,omitempty"`
	Phase      HandoffPhase     `json:"phase,omitempty"`
	// ArtifactDir is where the record says the active generation lives, and it
	// is what the projection is compared against. Matching on the directory's
	// NAME instead would be comparing a convention - build-adopted happens to
	// name generation directories after their version - and a convention is not
	// a fact the record asserts.
	ArtifactDir string     `json:"artifact_dir,omitempty"`
	Source      Provenance `json:"source"`
}

// ProjectionObservation is the stable entrypoint, and only that.
type ProjectionObservation struct {
	State  ProjectionState `json:"state"`
	Path   string          `json:"path"`
	Target string          `json:"target,omitempty"`
	Source Provenance      `json:"source"`
}

// LiveObservation is what a live endpoint reported, or why nothing was.
type LiveObservation struct {
	Reachable bool                    `json:"reachable"`
	Detail    string                  `json:"detail,omitempty"`
	Snapshot  *LiveControllerSnapshot `json:"snapshot,omitempty"`
	Source    Provenance              `json:"source"`
}

// ControllerStatus is the whole answer, kept dimensional.
type ControllerStatus struct {
	Schema     string                `json:"schema"`
	ObservedAt time.Time             `json:"observed_at"`
	Durable    DurableActive         `json:"durable_active"`
	Live       LiveObservation       `json:"live"`
	Projection ProjectionObservation `json:"projection"`

	// The three verdicts. They are separate fields because they are separate
	// questions, and because the most useful answer this product can give -
	// "written state is consistent, nobody is serving, the pointer is stale" -
	// is unrepresentable in one.
	DurableConsistency DurableConsistency `json:"durable_consistency"`
	Serving            ServingState       `json:"serving"`
	Findings           []string           `json:"findings,omitempty"`
}

// DescribeControllerStatus collects the status. IT MUTATES NOTHING: no
// projection repair, no lock taken, no record written. An operator asking what
// is happening must not change what is happening.
//
// observe is the live half, supplied by the caller because reaching a serving
// process is a transport concern. It returns false when nothing could be
// observed, which becomes UNKNOWN rather than an assumption.
func DescribeControllerStatus(store handoffStore, controllerRoot string, observe func() (LiveControllerSnapshot, error), now time.Time) (ControllerStatus, error) {
	// DURABLE STATE ANCHORS THE SNAPSHOT. It is read before and after, and a
	// change between them means the live observations describe a world that has
	// already moved.
	before, err := describeDurableActive(store)
	if err != nil {
		return ControllerStatus{}, err
	}
	status := ControllerStatus{
		Schema: ControllerStatusSchema, ObservedAt: now,
		Durable: before, Live: LiveObservation{Source: FromLiveEndpoint},
	}
	if observe != nil {
		snapshot, observeErr := observe()
		if observeErr != nil {
			status.Live.Detail = observeErr.Error()
		} else {
			status.Live.Reachable = true
			status.Live.Snapshot = &snapshot
		}
	} else {
		status.Live.Detail = "no live observation was attempted"
	}
	status.Projection = describeProjection(controllerRoot, before)

	after, err := describeDurableActive(store)
	if err != nil {
		return ControllerStatus{}, err
	}
	if !sameTransition(before, after) {
		// A TORN READ IS NOT EVIDENCE. Classifying it would produce a
		// combination that never existed at any instant, and labelling that an
		// invariant violation would be manufacturing a defect.
		status.Durable = after
		status.DurableConsistency = DurableUnstable
		status.Serving = ServingUnknown
		status.Findings = append(status.Findings,
			"the durable transition changed while this status was being collected")
		return status, nil
	}
	status.classify()
	return status, nil
}

func describeDurableActive(store handoffStore) (DurableActive, error) {
	active := DurableActive{Source: FromActivationRecord}
	// THE POINTER, not the newest activated row. Scanning for the most
	// recently written activation answered "which transition activated last",
	// which is a question about wall clocks and about which process wrote
	// second. Which activation GOVERNS is a durable subject of its own.
	current, found, err := store.CurrentControllerActivation()
	if err != nil {
		return DurableActive{}, err
	}
	if found {
		active.Generation = current.Successor.Binding.Build
		active.HandoffID = current.ID
		active.Phase = current.Phase
		if current.Successor.ArtifactPath != "" {
			active.ArtifactDir = filepath.Dir(current.Successor.ArtifactPath)
		}
		return active, nil
	}
	records, err := store.ControllerHandoffs()
	if err != nil {
		return DurableActive{}, err
	}
	// An unsettled transition is still worth naming: an operator whose upgrade
	// is stuck needs to see which one.
	for _, record := range records {
		if record.InFlight() {
			active.HandoffID = record.ID
			active.Phase = record.Phase
			break
		}
	}
	return active, nil
}

// sameTransition compares the exact transition rather than the generation.
// #270's lesson applies here too: the same pair of generations can belong to
// two different transitions, so a comparison on generation alone would call a
// genuinely moved world stable.
func sameTransition(before, after DurableActive) bool {
	if before.HandoffID != after.HandoffID || before.Phase != after.Phase {
		return false
	}
	if before.ArtifactDir != after.ArtifactDir {
		return false
	}
	if (before.Generation == nil) != (after.Generation == nil) {
		return false
	}
	return before.Generation == nil || *before.Generation == *after.Generation
}

// describeProjection reads the stable entrypoint and says what it is. It is the
// ONLY thing derived from the pointer, and nothing else is derived from it.
func describeProjection(controllerRoot string, durable DurableActive) ProjectionObservation {
	observation := ProjectionObservation{
		Path: filepath.Join(controllerRoot, StableEntrypointName), Source: FromStableEntrypoint,
	}
	info, err := os.Lstat(observation.Path)
	switch {
	case err != nil:
		observation.State = ProjectionMissing
		return observation
	case info.Mode()&os.ModeSymlink == 0:
		observation.State = ProjectionInvalid
		return observation
	}
	target, err := os.Readlink(observation.Path)
	if err != nil {
		observation.State = ProjectionInvalid
		return observation
	}
	observation.Target = target
	if durable.Generation == nil || durable.ArtifactDir == "" {
		// Nothing durably active, or a record that does not say where the
		// active artifact lives: there is nothing for the pointer to disagree
		// with, and reporting drift would invent a comparison.
		observation.State = ProjectionCurrent
		return observation
	}
	if target == durable.ArtifactDir {
		observation.State = ProjectionCurrent
		return observation
	}
	observation.State = ProjectionDrift
	return observation
}

// classify turns observations into the three verdicts, refusing to conclude
// anything the observations do not support.
func (s *ControllerStatus) classify() {
	s.DurableConsistency = DurableConsistent
	if s.Durable.Generation == nil {
		s.DurableConsistency = DurableUnrecorded
	}
	if !s.Live.Reachable || s.Live.Snapshot == nil {
		// UNOBSERVED IS NOT ABSENT. Role ownership and work admission are
		// simply unknown, and the status says so instead of reporting that
		// nobody holds either.
		s.Serving = ServingUnknown
		s.Findings = append(s.Findings, "no live controller was observed, so role ownership and work admission are unknown")
		return
	}
	live := s.Live.Snapshot
	switch live.WorkAdmission {
	case AdmissionOpen:
		s.Serving = Serving
	case AdmissionUnknown:
		// REACHING THE RIGHT PROCESS IS NOT OBSERVING ITS SERVICE. An endpoint
		// that answered without saying whether it admits work leaves the
		// question open, and identity does not close it.
		s.Serving = ServingUnknown
		s.Findings = append(s.Findings, "the observed controller did not report its work admission")
	default:
		s.Serving = NotServing
	}
	if live.Role == RoleUnknown {
		s.Findings = append(s.Findings, "the observed controller did not report its role ownership")
	}
	if s.Durable.Generation == nil {
		return
	}
	activeGeneration := *s.Durable.Generation
	// A LIVE PROCESS OF ANOTHER GENERATION IS NOT ITSELF WRONG. A drained
	// predecessor that has not exited yet is an ordinary sight during an
	// upgrade. It becomes a violation when it OWNS something it may not.
	sameGeneration := live.Identity.Build == activeGeneration
	if sameGeneration {
		return
	}
	s.Findings = append(s.Findings, fmt.Sprintf(
		"the observed controller is generation %q while %q is durably active",
		live.Identity.Build.Version, activeGeneration.Version))
	// ONLY POSITIVE OBSERVATIONS CONVICT. An unknown role or an unreported gate
	// is not evidence of a violation; overlapping LIVENESS is ordinary during an
	// upgrade, and what the protocol forbids is overlapping AUTHORITY.
	if live.WorkAdmission == AdmissionOpen {
		s.DurableConsistency = DurableViolation
		s.Findings = append(s.Findings, "a controller that is not the durably active generation is admitting work")
	}
	if live.Role == RoleHeld {
		s.DurableConsistency = DurableViolation
		s.Findings = append(s.Findings, "a controller that is not the durably active generation holds the controller role")
	}
}

// DescribeLiveController is the serving process's coherent self-observation.
//
// The role fact is established by EXERCISING the lease rather than by reading a
// flag: there is deliberately no Held(), so the only honest way to learn
// whether this process still owns the role is to attempt something under it.
// The result is a point-in-time diagnostic and never a capability - nobody gets
// a boolean they can authorize later work with.
func (s *ControllerService) DescribeLiveController(handoffID string, now time.Time) LiveControllerSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snapshot := LiveControllerSnapshot{Identity: s.self, HandoffID: handoffID, ObservedAt: now}
	snapshot.Role = RoleNotHeld
	if err := s.lease.WithAuthority(func() error { return nil }); err == nil {
		snapshot.Role = RoleHeld
	}
	switch {
	case s.admission == nil:
		snapshot.WorkAdmission = AdmissionWithheld
	case s.admission.AdmittingWork():
		snapshot.WorkAdmission = AdmissionOpen
	default:
		snapshot.WorkAdmission = AdmissionClosed
	}
	return snapshot
}

// ---------------------------------------------------------------------------
// The update result
// ---------------------------------------------------------------------------

// ControllerUpdateSchema versions the machine-readable succession result, for
// the same reason the status document is versioned.
const ControllerUpdateSchema = "zenchron.controller-update/v1"

// Update outcomes, per dimension. They are separate because the protocol has
// already established that they fail separately: an activation that succeeded
// with a projection that did not is a COMPLETED succession with a stale
// pointer, and one boolean cannot say that.
type UpdateOutcome string

const (
	UpdateSucceeded          UpdateOutcome = "succeeded"
	UpdateFailed             UpdateOutcome = "failed"
	UpdateSkipped            UpdateOutcome = "skipped"
	UpdateDrifted            UpdateOutcome = "drift"
	UpdateCompleted          UpdateOutcome = "completed"
	UpdateCompletedWithDrift UpdateOutcome = "completed_with_drift"
)

// ControllerUpdateResult is what a succession reports. Every dimension the
// protocol treats as independent is reported independently, and the overall
// line is derived from them rather than replacing them.
type ControllerUpdateResult struct {
	Schema    string    `json:"schema"`
	HandoffID string    `json:"handoff_id"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`

	Activation struct {
		Outcome UpdateOutcome `json:"outcome"`
		Phase   HandoffPhase  `json:"phase,omitempty"`
		Detail  string        `json:"detail,omitempty"`
	} `json:"activation"`
	Successor struct {
		Generation *ControllerBuild `json:"generation,omitempty"`
		Role       RoleObservation  `json:"role,omitempty"`
	} `json:"successor"`
	Projection struct {
		Outcome UpdateOutcome `json:"outcome"`
		Detail  string        `json:"detail,omitempty"`
	} `json:"projection"`
	WorkAdmission struct {
		Outcome UpdateOutcome `json:"outcome"`
		Detail  string        `json:"detail,omitempty"`
	} `json:"work_admission"`
	Predecessor struct {
		Outcome UpdateOutcome `json:"outcome"`
		Detail  string        `json:"detail,omitempty"`
	} `json:"predecessor"`

	Overall UpdateOutcome `json:"overall"`
}

// settle derives the overall line from the dimensions.
//
// A FAILED PROJECTION DOES NOT FAIL THE SUCCESSION, which is the whole reason
// this result is dimensional: #276 established that authority is durable and
// the pointer is a repairable projection of it, and collapsing the two here
// would undo that at the last moment, in the place an operator actually reads.
func (r *ControllerUpdateResult) settle() {
	switch {
	case r.Activation.Outcome != UpdateSucceeded:
		r.Overall = UpdateFailed
	case r.WorkAdmission.Outcome == UpdateFailed:
		r.Overall = UpdateFailed
	case r.Projection.Outcome == UpdateDrifted || r.Projection.Outcome == UpdateFailed:
		r.Overall = UpdateCompletedWithDrift
	default:
		r.Overall = UpdateCompleted
	}
}
