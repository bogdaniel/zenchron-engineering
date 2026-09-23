package runtime

// THE WORK-ADMISSION GATE: where "may I serve" stops being a fact anyone reads
// and becomes a fact nobody can act against.
//
// Every policy in the handoff protocol can be correct and the protocol still
// lose, because a check and a commit are two moments:
//
//	T1  admission check       -> permitted
//	T2  drain closes and RETURNS
//	T1  the run is created
//
// Nothing was wrong at either moment, and the statement the protocol depends on
// - "after the predecessor drains, no new work is admitted" - is false. The
// supervisor had exactly this shape: Submit read the draining flag under a
// mutex, released it, and created the run afterwards.
//
// So the gate is not a flag. It is a lock the DECISION AND THE COMMIT are both
// held under, and closing it takes the same lock exclusively. That gives the
// property the protocol actually needs:
//
//	After close returns, no admission that had not already committed can
//	become visible.
//
// A close therefore waits for admissions already in progress, which is correct:
// draining means "stop taking new work", not "abandon the work being written
// down right now". The wait is bounded by one run creation.
//
// CLOSING IS TERMINAL. A drained controller does not resume; the successor is a
// different process with its own gate. Reopening would make "drained" a
// statement about the present rather than about the transition, and the whole
// protocol reasons about it as the latter.
//
// WHAT THIS GATE IS NOT. It is not ownership - a controller can hold the
// scheduler lock and be forbidden to serve - and it is not activation, which is
// durable truth about which generation is active. This is the present
// permission to take on work, derived from those two and collapsible into
// neither.

import (
	"fmt"
	"sync"
)

// workAdmissionState is the gate's lifecycle. Pending exists because a
// successor's supervisor is constructed before it is allowed to serve: it holds
// the scheduler while it revalidates and proves itself, and opening at
// construction would make ownership imply service.
type workAdmissionState int

const (
	admissionPending workAdmissionState = iota
	admissionOpen
	admissionClosed
)

type workAdmissionGate struct {
	// mu is held for READING across an entire admission - decision and commit
	// together - and exclusively to close. That is the whole mechanism.
	mu     sync.RWMutex
	state  workAdmissionState
	reason string
}

func newWorkAdmissionGate(open bool) *workAdmissionGate {
	if open {
		return &workAdmissionGate{state: admissionOpen}
	}
	return &workAdmissionGate{
		state:  admissionPending,
		reason: "this controller has not been activated, so it does not admit work yet",
	}
}

// admit runs commit with the gate held open, or refuses without running it.
//
// The commit is called INSIDE the lock deliberately. Returning a token for the
// caller to commit with later would reintroduce the two-moment problem this
// type exists to remove.
func (g *workAdmissionGate) admit(commit func() error) error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.state != admissionOpen {
		return &WorkAdmissionRefusedError{Reason: g.reason}
	}
	return commit()
}

// section holds the gate open across a longer intake, such as a whole plan
// reconciliation that may create several runs. The release must be called.
func (g *workAdmissionGate) section() (release func(), err error) {
	g.mu.RLock()
	if g.state != admissionOpen {
		reason := g.reason
		g.mu.RUnlock()
		return nil, &WorkAdmissionRefusedError{Reason: reason}
	}
	return g.mu.RUnlock, nil
}

// close is terminal and waits for admissions already in progress.
func (g *workAdmissionGate) close(reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == admissionClosed {
		return
	}
	g.state, g.reason = admissionClosed, reason
}

// open permits work. It refuses to reopen a closed gate, because a drained
// controller resuming is not a state this protocol has a meaning for.
func (g *workAdmissionGate) open() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.state == admissionClosed {
		return fmt.Errorf("work admission was closed (%s) and does not reopen", g.reason)
	}
	g.state, g.reason = admissionOpen, ""
	return nil
}

func (g *workAdmissionGate) permitted() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state == admissionOpen
}

func (g *workAdmissionGate) closed() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.state == admissionClosed
}

// WorkAdmissionRefusedError is a refusal to take on work, distinct from any
// failure of the work itself.
type WorkAdmissionRefusedError struct{ Reason string }

func (e *WorkAdmissionRefusedError) Error() string {
	if e.Reason == "" {
		return "this controller is not admitting work"
	}
	return "this controller is not admitting work: " + e.Reason
}

// EnableWorkAdmission opens the gate, and only on durable authority.
//
// It takes the ANSWER rather than the record so the supervisor cannot become a
// second place that decides what activation means: WorkAdmissionFor reads the
// durable record and this either honours it or refuses.
func (s *Supervisor) EnableWorkAdmission(admission WorkAdmission) error {
	if !admission.Permitted {
		return &WorkAdmissionRefusedError{Reason: admission.Reason}
	}
	return s.admission.open()
}

// AdmittingWork reports whether this supervisor may currently take on work.
//
// It is deliberately separate from the historical question the succession chain
// asks. "Did transition H activate" is a fact about the past that stays true
// forever; "may this generation serve now" is a fact about the present that a
// later transition changes. Merging them would let a years-old activation argue
// that a superseded generation may serve.
func (s *Supervisor) AdmittingWork() bool { return s.admission.permitted() }
