package runtime

// THE CONTROLLER HANDOFF, as durable state rather than as process state.
//
// Two controllers, one scheduler, and a window in which neither is obviously
// the owner. What makes that window survivable is not the order the code runs
// in - a crash does not respect it - but a record on disk that answers, at any
// moment, four questions: who the predecessor was, who the successor is, which
// phase had completed, and WHICH CONTROLLER IS ALLOWED TO RECOVER NEXT.
// Inferring those from whichever processes happen to be alive is how two
// controllers come to believe they own serve.
//
// The phases are ordered and each one is a durable fact:
//
//	prepared            every nonterminal run preflighted compatible under B
//	draining            A stopped admitting new work
//	ownership_released  A let go of the scheduler; nobody owns it
//	successor_acquired  B holds the scheduler and has not yet revalidated
//	revalidated         B re-read the state it preflighted and it still holds
//	activated           B proved itself and is scheduling; A may exit
//	failed              the transition stopped; the record names who recovers
//
// THE GATE IS ALL-OR-NOTHING, and that is an architectural choice rather than
// a cautious one. serve has exactly one scheduler owner, so a handoff that
// admitted some runs and not others would leave the refused ones executable
// only by a controller that is about to exit - stranding them, or requiring
// two active schedulers, or forcing the successor to take work it just proved
// it could not safely interpret. Partitioned ownership is a materially larger
// scheduler model; #89 or a dedicated follow-up can want it, and #234 does not.
//
// ADMISSIONS ARE NOT WRITTEN AT PREFLIGHT. A preflight produces decisions and
// nothing else. If it recorded them, a handoff that then failed would leave
// durable state saying the successor was admitted while the predecessor is
// still the active controller, and the journal would be disagreeing with
// reality about who may append. Admission belongs to the ownership transition,
// after the successor holds the scheduler and has revalidated.
//
// THE FAILURE LAW THIS FILE EXISTS TO KEEP: if the successor acquires ownership
// and then fails revalidation, it relinquishes ownership WITHOUT admitting any
// succession and WITHOUT scheduling, and recovery names the predecessor - or a
// restarted generation of it - as the only eligible controller to resume.
//
// ACTIVATION IS THE POINT OF NO SILENT RETURN, and the asymmetry is #122's law
// rather than caution. Before it, nothing has been admitted and the predecessor
// is still the controller of record, so handing control back is returning state
// to the process that wrote it. After it, the successor is appending - possibly
// in a vocabulary the predecessor does not implement - and silently returning
// scheduling authority to an older controller would be letting it execute from
// governance state it cannot completely understand. A later successor failure
// is therefore NOT a rollback to the predecessor: it is a new recovery event
// that has to choose a controller proven compatible with the durable head as it
// stands then. #234 builds no machinery for that, and this file refuses to
// pretend otherwise: there is no edge out of activated.

import (
	"database/sql"
	"fmt"
	"time"
)

// HandoffPhase is how far a transition got. It is durable and ordered.
type HandoffPhase string

const (
	HandoffPrepared          HandoffPhase = "prepared"
	HandoffDraining          HandoffPhase = "draining"
	HandoffOwnershipReleased HandoffPhase = "ownership_released"
	HandoffSuccessorAcquired HandoffPhase = "successor_acquired"
	HandoffRevalidated       HandoffPhase = "revalidated"
	HandoffActivated         HandoffPhase = "activated"
	HandoffFailed            HandoffPhase = "failed"
)

// handoffOrder is the only permitted path. A transition that is not an edge
// here is refused rather than recorded, so a record can never claim a phase
// nothing walked to; failure is reachable from every live phase and from none
// of the settled ones.
var handoffOrder = map[HandoffPhase][]HandoffPhase{
	HandoffPrepared:          {HandoffDraining, HandoffFailed},
	HandoffDraining:          {HandoffOwnershipReleased, HandoffFailed},
	HandoffOwnershipReleased: {HandoffSuccessorAcquired, HandoffFailed},
	HandoffSuccessorAcquired: {HandoffRevalidated, HandoffFailed},
	HandoffRevalidated:       {HandoffActivated, HandoffFailed},
	HandoffActivated:         nil,
	HandoffFailed:            nil,
}

// HandoffParty is one side of the transition: the binding a run is measured
// against, and the artifact that binding runs from.
//
// ArtifactPath is recorded because rollback is a PATH question. A record that
// named only digests could say the predecessor should recover without saying
// what to start, which is the same as not saying it.
type HandoffParty struct {
	Binding      ControllerBinding `json:"binding"`
	ArtifactPath string            `json:"artifact_path,omitempty"`
}

// HandoffRunDecision is one run's preflight answer, kept per run so a blocked
// upgrade can name the exact run and the exact dimension instead of reporting
// that something somewhere was incompatible.
//
// EventCount and StateSHA256 are the durable head the decision was made
// against. They are what revalidation compares: a run whose journal moved
// between preflight and ownership transfer was decided about in a state that no
// longer exists.
type HandoffRunDecision struct {
	RunID       string                       `json:"run_id"`
	Result      ControllerSuccessionResult   `json:"result"`
	Refusals    []string                     `json:"refusals,omitempty"`
	EventCount  int                          `json:"event_count"`
	StateSHA256 string                       `json:"state_sha256,omitempty"`
	Decision    ControllerSuccessionDecision `json:"decision"`
}

// ControllerHandoff is the durable record of one transition.
type ControllerHandoff struct {
	ID          string               `json:"id"`
	Predecessor HandoffParty         `json:"predecessor"`
	Successor   HandoffParty         `json:"successor"`
	TrustedMain RevisionRecord       `json:"trusted_main"`
	Phase       HandoffPhase         `json:"phase"`
	Runs        []HandoffRunDecision `json:"runs,omitempty"`
	// RecoveryOwner is the controller digest permitted to resume after a crash
	// in this phase. It is STORED rather than derived at read time so a
	// recovering process reads the answer the transition recorded, not one
	// computed by whichever binary happened to open the database.
	//
	// IT IS AN ELIGIBILITY STATEMENT, NOT A PREFERRED BINARY. Naming a
	// controller here says that generation has proven compatibility with the
	// durable head as it stood at that phase. The other party's artifact
	// remaining on disk is provenance and recovery material; it is not evidence
	// that it may resume scheduling, and "a rollback target exists" must never
	// be read as "rollback is safe".
	RecoveryOwner string    `json:"recovery_owner"`
	Detail        string    `json:"detail,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Blockers is the per-run reason a handoff may not proceed.
func (h ControllerHandoff) Blockers() []string {
	var blockers []string
	for _, run := range h.Runs {
		if run.Result == SuccessionCompatible {
			continue
		}
		for _, refusal := range run.Refusals {
			blockers = append(blockers, run.RunID+" "+refusal)
		}
	}
	return blockers
}

// Compatible reports whether every preflighted run may be continued by the
// successor. An empty set is compatible: a controller with no live work has
// nothing that could be stranded.
func (h ControllerHandoff) Compatible() bool {
	for _, run := range h.Runs {
		if run.Result != SuccessionCompatible {
			return false
		}
	}
	return true
}

// HandoffPreflightInput is the read-only classification the SUCCESSOR performs
// before anything moves. The successor runs it because the vocabulary and
// replay questions are only meaningful when asked of the code that would do the
// reading.
type HandoffPreflightInput struct {
	Predecessor HandoffParty
	Successor   HandoffParty
	TrustedMain RevisionRecord
	IsAncestor  func(ancestor, descendant string) (bool, error)
	// Now stamps the record. It is a parameter because a durable record with a
	// time nobody controls is a record a test cannot pin.
	Now time.Time
}

// runReader is the read-only slice of the store a preflight needs. It is an
// interface so the preflight cannot write even by accident, and so a test can
// drive journals that no fixture can produce.
type runReader interface {
	Runs() ([]EngineeringRun, error)
	Events(runID string) ([]EngineeringEvent, error)
}

// PreflightControllerHandoff classifies EVERY nonterminal run under the
// successor and returns the resulting record, unpersisted.
//
// Nonterminal is terminalDisposition inverted rather than a second notion of
// "finished" invented here: a waiting run is exactly the kind that looks idle
// and still needs a controller able to resume it, and having two answers to
// "is this run over" is how one of them drifts.
//
// Nothing is written. A preflight that fails leaves no trace precisely because
// the predecessor is still the active controller and its state must not record
// a transition that never began.
func PreflightControllerHandoff(store runReader, in HandoffPreflightInput) (ControllerHandoff, error) {
	predecessor, err := in.Predecessor.Binding.Digest()
	if err != nil {
		return ControllerHandoff{}, err
	}
	successor, err := in.Successor.Binding.Digest()
	if err != nil {
		return ControllerHandoff{}, err
	}
	if predecessor == successor {
		return ControllerHandoff{}, fmt.Errorf("a controller does not hand off to itself")
	}
	runs, err := store.Runs()
	if err != nil {
		return ControllerHandoff{}, err
	}
	record := ControllerHandoff{
		ID:          handoffID(predecessor, successor),
		Predecessor: in.Predecessor, Successor: in.Successor,
		TrustedMain: in.TrustedMain,
		Phase:       HandoffPrepared,
		// Until the successor owns the scheduler, the predecessor is who
		// recovers. The record says so from the first moment it exists.
		RecoveryOwner: predecessor,
		UpdatedAt:     in.Now,
	}
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			continue
		}
		events, err := store.Events(run.ID)
		if err != nil {
			// A journal that cannot be read is a run whose compatibility is
			// unknown, and unknown blocks the handoff exactly as refused does.
			record.Runs = append(record.Runs, HandoffRunDecision{
				RunID: run.ID, Result: SuccessionRefused,
				Refusals: []string{"durable_replay: the journal could not be read: " + err.Error()},
			})
			continue
		}
		decision := EvaluateControllerSuccession(ControllerSuccessionInput{
			Run: run, Events: events,
			Predecessor: in.Predecessor.Binding, Successor: in.Successor.Binding,
			TrustedMain: in.TrustedMain, IsAncestor: in.IsAncestor,
		})
		// The decision is evidence FOR THIS TRANSITION and for no other, so it
		// carries the transition's identity from the moment it is made.
		decision.HandoffID = record.ID
		entry := HandoffRunDecision{
			RunID: run.ID, Result: decision.Result, Refusals: decision.Refusals(),
			EventCount: len(events), Decision: decision,
		}
		// The head the decision was made against. A replay that already failed
		// has no state to record, and the refusal above is what carries that.
		if snapshot, err := Reduce(run, events); err == nil {
			entry.StateSHA256 = snapshot.StateSHA256
		}
		record.Runs = append(record.Runs, entry)
	}
	if !record.Compatible() {
		record.Phase = HandoffFailed
		record.Detail = "one or more live runs cannot be continued by the successor"
	}
	return record, nil
}

// RevalidateControllerHandoff re-reads every run the preflight decided about,
// AFTER the successor holds the scheduler, and reports whether the decisions
// still describe the state on disk.
//
// This exists because the preflight and the transfer are separated in time by
// everything the predecessor does while draining. A run whose journal grew in
// between was classified against a state that is gone, and continuing on that
// classification would be admitting a succession nobody proved.
func RevalidateControllerHandoff(store runReader, record ControllerHandoff) error {
	runs, err := store.Runs()
	if err != nil {
		return err
	}
	decided := map[string]HandoffRunDecision{}
	for _, run := range record.Runs {
		decided[run.RunID] = run
	}
	for _, run := range runs {
		if terminalDisposition(run.Disposition) {
			// A run that FINISHED while draining is not a problem: nothing
			// needs to continue it. The reverse - a run that became live - is
			// the case below.
			continue
		}
		decision, preflighted := decided[run.ID]
		if !preflighted {
			return fmt.Errorf("run %s became live after the preflight and was never classified", run.ID)
		}
		if decision.Result != SuccessionCompatible {
			return fmt.Errorf("run %s was not compatible at preflight", run.ID)
		}
		events, err := store.Events(run.ID)
		if err != nil {
			return fmt.Errorf("run %s could not be re-read: %w", run.ID, err)
		}
		if len(events) != decision.EventCount {
			return fmt.Errorf("run %s moved between the preflight and the transfer: %d event(s), preflighted at %d",
				run.ID, len(events), decision.EventCount)
		}
		snapshot, err := Reduce(run, events)
		if err != nil {
			return fmt.Errorf("run %s no longer replays under this controller: %w", run.ID, err)
		}
		if snapshot.StateSHA256 != decision.StateSHA256 {
			return fmt.Errorf("run %s is at state %s, preflighted at %s",
				run.ID, shortSHA(snapshot.StateSHA256), shortSHA(decision.StateSHA256))
		}
	}
	return nil
}

// Advance moves the record one phase, or refuses.
//
// The recovery owner moves with the phase, and the one transition that changes
// it is acquisition: before the successor holds the scheduler the predecessor
// recovers, and from revalidation onward the successor does - because that is
// where the admissions are written, and a journal carrying events the
// predecessor may not understand is not a journal it can be handed back.
func (h ControllerHandoff) Advance(to HandoffPhase, now time.Time) (ControllerHandoff, error) {
	permitted := false
	for _, next := range handoffOrder[h.Phase] {
		if next == to {
			permitted = true
		}
	}
	if !permitted {
		return h, fmt.Errorf("a handoff does not move from %q to %q", h.Phase, to)
	}
	predecessor, err := h.Predecessor.Binding.Digest()
	if err != nil {
		return h, err
	}
	successor, err := h.Successor.Binding.Digest()
	if err != nil {
		return h, err
	}
	h.Phase, h.UpdatedAt = to, now
	switch to {
	case HandoffRevalidated, HandoffActivated:
		h.RecoveryOwner = successor
	case HandoffFailed:
		// UNCHANGED ON PURPOSE. Fail is reached from both sides of the
		// ownership boundary, and who recovers depends on which side: the
		// caller states it through Fail rather than this switch guessing.
	default:
		h.RecoveryOwner = predecessor
	}
	return h, nil
}

// Fail settles the record, naming why and who recovers.
//
// A successor that acquired ownership and could not revalidate fails to the
// PREDECESSOR: it has admitted nothing and scheduled nothing, so the state is
// exactly what the predecessor left, and the predecessor - or a restarted
// generation of it - is the only controller that should resume.
func (h ControllerHandoff) Fail(detail string, recoverAs HandoffParty, now time.Time) (ControllerHandoff, error) {
	owner, err := recoverAs.Binding.Digest()
	if err != nil {
		return h, err
	}
	failed, err := h.Advance(HandoffFailed, now)
	if err != nil {
		return h, err
	}
	failed.RecoveryOwner, failed.Detail = owner, detail
	return failed, nil
}

// MayRecover reports whether this controller is the one permitted to resume a
// handoff found on disk.
//
// A settled record permits the owner it names. An ACTIVATED one permits the
// successor and nobody else: the successor is scheduling and has begun writing,
// so a predecessor that came back up would be both a second owner and an older
// reader of state it may not understand.
func (h ControllerHandoff) MayRecover(controller string) bool {
	return h.RecoveryOwner == controller
}

// InFlight reports whether the record describes a transition that neither
// completed nor settled. A process starting on a state directory holding one of
// these has to resolve it before scheduling anything.
func (h ControllerHandoff) InFlight() bool {
	return h.Phase != HandoffActivated && h.Phase != HandoffFailed
}

// handoffID is deterministic in the transition, so a retried handoff between
// the same two controllers addresses the same record instead of accumulating
// one per attempt.
func handoffID(predecessor, successor string) string {
	return "handoff-" + shortSHA(predecessor) + "-" + shortSHA(successor)
}

// ---------------------------------------------------------------------------
// Durable storage
// ---------------------------------------------------------------------------

// ControllerHandoffs returns every recorded transition, newest first. It is how
// a starting controller finds a handoff it has to resolve before scheduling.
func (s *SQLiteOperationStore) ControllerHandoffs() ([]ControllerHandoff, error) {
	rows, err := s.db.Query(`SELECT document FROM controller_handoffs ORDER BY updated_unix_nano DESC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var handoffs []ControllerHandoff
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var handoff ControllerHandoff
		if err := decodeJSON([]byte(document), &handoff); err != nil {
			return nil, err
		}
		handoffs = append(handoffs, handoff)
	}
	return handoffs, rows.Err()
}

// ControllerHandoff reads one transition by id.
func (s *SQLiteOperationStore) ControllerHandoff(id string) (ControllerHandoff, bool, error) {
	var document string
	switch err := s.db.QueryRow(`SELECT document FROM controller_handoffs WHERE id = ?`, id).Scan(&document); err {
	case nil:
	case sql.ErrNoRows:
		return ControllerHandoff{}, false, nil
	default:
		return ControllerHandoff{}, false, err
	}
	var handoff ControllerHandoff
	err := decodeJSON([]byte(document), &handoff)
	return handoff, err == nil, err
}

// PutControllerHandoff writes a transition, conditional on the phase the caller
// last read.
//
// THE COMPARE-AND-SET IS THE WHOLE POINT. Two processes are alive during a
// handoff and both can write; an unconditional update would let a successor
// that revalidated and a predecessor that timed out both record their view,
// and the last writer would decide who owns the scheduler. expected is the
// empty string only for the first write, which inserts.
func (s *SQLiteOperationStore) PutControllerHandoff(handoff ControllerHandoff, expected HandoffPhase) (bool, error) {
	if handoff.ID == "" {
		return false, fmt.Errorf("a controller handoff requires an id")
	}
	if handoff.Phase == "" || handoff.RecoveryOwner == "" {
		return false, fmt.Errorf("a controller handoff records a phase and the controller permitted to recover it")
	}
	document, err := CanonicalJSON(handoff)
	if err != nil {
		return false, err
	}
	stamp := handoff.UpdatedAt.UnixNano()
	if expected == "" {
		result, err := s.db.Exec(
			`INSERT INTO controller_handoffs (id, phase, updated_unix_nano, document) VALUES (?, ?, ?, ?)
			 ON CONFLICT(id) DO NOTHING`,
			handoff.ID, string(handoff.Phase), stamp, document)
		if err != nil {
			return false, err
		}
		affected, err := result.RowsAffected()
		return affected == 1, err
	}
	result, err := s.db.Exec(
		`UPDATE controller_handoffs SET phase = ?, updated_unix_nano = ?, document = ? WHERE id = ? AND phase = ?`,
		string(handoff.Phase), stamp, document, handoff.ID, string(expected))
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}
