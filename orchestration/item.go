package orchestration

import "fmt"

// ItemState is a batch item's state. It is a PROJECTION over the child run and
// its handoff observations, recomputed on every read; it is never stored, so
// it cannot drift from the run that is the authority for everything it says.
type ItemState string

const (
	// ItemNotCreated is an item whose child run does not exist yet: the batch
	// was recorded and the run's creation has not landed. The supervisor
	// creates it on its next pass.
	ItemNotCreated ItemState = "not_created"
	// ItemQueued is a live child run that is not holding a capacity slot and
	// is not waiting on anything but capacity or its turn.
	ItemQueued ItemState = "queued"
	// ItemRunning is a live child run holding a scheduler slot, or one whose
	// worker reported a handoff the runtime has not yet bound to a commit.
	ItemRunning ItemState = "running"
	// ItemWaiting is a live child run in a typed wait or an operator pause.
	ItemWaiting ItemState = "waiting"
	// ItemHandoffPending is a child whose latest finished worker invocation
	// did not transfer an admissible handoff. Provider success is not
	// orchestration completion.
	ItemHandoffPending ItemState = "handoff_pending"
	// ItemCompleted is a child whose latest finished worker invocation
	// transferred a handoff the runtime admitted and bound to the exact
	// candidate it committed. The child run keeps its own governed lifecycle.
	ItemCompleted ItemState = "completed"
	// ItemPartial is a child whose latest finished invocation transferred an
	// ADMITTED handoff that reports unresolved work. Admission proves the
	// transfer is valid and bound; it does not make partial work complete.
	ItemPartial ItemState = "partial"
	// ItemFailed and ItemStopped mirror a terminal child run honestly.
	ItemFailed  ItemState = "failed"
	ItemStopped ItemState = "stopped"
)

// RunTermination is whether, and how, a child run ended.
type RunTermination string

const (
	RunLive      RunTermination = "live"
	RunCompleted RunTermination = "completed"
	RunFailed    RunTermination = "failed"
	RunCancelled RunTermination = "cancelled"
)

// RunActivity is what a live child run is doing with respect to capacity.
type RunActivity string

const (
	ActivityIdle    RunActivity = "idle"
	ActivityWorking RunActivity = "working"
	ActivityWaiting RunActivity = "waiting"
)

// HandoffObservation is the latest thing the runtime recorded about a worker
// handoff for this child.
type HandoffObservation string

const (
	// HandoffNone: no worker invocation of this child has finished yet.
	HandoffNone HandoffObservation = "none"
	// HandoffReported: a finished invocation wrote a valid report, and the
	// runtime has not yet bound it to the commit of that invocation's output.
	HandoffReported HandoffObservation = "reported"
	// HandoffAdmitted: the latest report is bound into a durable handoff.
	HandoffAdmitted HandoffObservation = "admitted"
	// HandoffRefused: the latest finished invocation wrote no report, an
	// invalid one, or one that cannot be bound to an exact output.
	HandoffRefused HandoffObservation = "refused"
)

// ChildFacts are the typed facts one item's state is projected from. The
// runtime supplies them from its own durable state.
type ChildFacts struct {
	Exists      bool
	Termination RunTermination
	Activity    RunActivity
	Handoff     HandoffObservation
	// HandoffOutcome is the admitted report's outcome. It is read only when
	// Handoff is HandoffAdmitted.
	HandoffOutcome string
}

// ProjectItem is the ONE place an item's state is decided. An unrecognized
// fact is an error, never a default: a projection that guessed would show an
// operator a state nothing durable supports.
func ProjectItem(facts ChildFacts) (ItemState, error) {
	if !facts.Exists {
		return ItemNotCreated, nil
	}
	switch facts.Handoff {
	case HandoffNone, HandoffReported, HandoffAdmitted, HandoffRefused:
	default:
		return "", fmt.Errorf("unrecognized handoff observation %q", facts.Handoff)
	}
	switch facts.Termination {
	case RunFailed:
		return ItemFailed, nil
	case RunCancelled:
		return ItemStopped, nil
	case RunCompleted, RunLive:
	default:
		return "", fmt.Errorf("unrecognized run termination %q", facts.Termination)
	}
	switch facts.Handoff {
	case HandoffAdmitted:
		switch facts.HandoffOutcome {
		case OutcomeCompleted:
			return ItemCompleted, nil
		case OutcomePartial:
			return ItemPartial, nil
		}
		return "", fmt.Errorf("unrecognized admitted handoff outcome %q", facts.HandoffOutcome)
	case HandoffRefused:
		return ItemHandoffPending, nil
	}
	// A child that ENDED without an admitted handoff will never produce one.
	if facts.Termination == RunCompleted {
		return ItemHandoffPending, nil
	}
	if facts.Handoff == HandoffReported {
		return ItemRunning, nil
	}
	switch facts.Activity {
	case ActivityWorking:
		return ItemRunning, nil
	case ActivityWaiting:
		return ItemWaiting, nil
	case ActivityIdle:
		return ItemQueued, nil
	}
	return "", fmt.Errorf("unrecognized run activity %q", facts.Activity)
}

// Counts is the aggregate a whole batch is read through.
type Counts struct {
	Total          int `json:"total"`
	NotCreated     int `json:"not_created"`
	Queued         int `json:"queued"`
	Running        int `json:"running"`
	Waiting        int `json:"waiting"`
	HandoffPending int `json:"handoff_pending"`
	Completed      int `json:"completed"`
	Partial        int `json:"partial"`
	Failed         int `json:"failed"`
	Stopped        int `json:"stopped"`
	// Unknown counts items whose facts could not be read or projected. It is
	// counted rather than dropped so the totals always add up.
	Unknown int `json:"unknown"`
}

// Add counts one item.
func (c *Counts) Add(state ItemState) {
	c.Total++
	switch state {
	case ItemNotCreated:
		c.NotCreated++
	case ItemQueued:
		c.Queued++
	case ItemRunning:
		c.Running++
	case ItemWaiting:
		c.Waiting++
	case ItemHandoffPending:
		c.HandoffPending++
	case ItemCompleted:
		c.Completed++
	case ItemPartial:
		c.Partial++
	case ItemFailed:
		c.Failed++
	case ItemStopped:
		c.Stopped++
	default:
		c.Unknown++
	}
}
