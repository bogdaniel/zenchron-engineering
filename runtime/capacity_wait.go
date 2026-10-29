package runtime

import "fmt"

const (
	ReasonWorkCapacity        = "work_capacity_unavailable"
	ReasonObservationCapacity = "observation_capacity_unavailable"
	// CapacityVerification is an additional resource, never a primary operation class.
	CapacityVerification CapacityClass = "verification"
)

// CapacityWait records the scheduler's actual ceiling at refusal, rather than
// reconstructing it from configuration which may change before a status read.
type CapacityWait struct {
	Class   CapacityClass `json:"class"`
	Ceiling int           `json:"ceiling"`
}

func (w CapacityWait) reason() string {
	switch w.Class {
	case CapacityWork:
		return ReasonWorkCapacity
	case CapacityObservation:
		return ReasonObservationCapacity
	case CapacityVerification:
		return ReasonVerificationCapacity
	}
	return ""
}

func (w CapacityWait) validate(reason string) error {
	if w.Ceiling <= 0 || w.reason() == "" || w.reason() != reason {
		return fmt.Errorf("invalid capacity wait class, ceiling or reason")
	}
	return nil
}

// CapacityBlocked explains a refused eligible operation using the same
// durable occupancy counted by atomic acquisition. It grants no capacity.
func (s Scheduler) CapacityBlocked(op RunOperation) (*CapacityWait, error) {
	s = s.defaults()
	// ponytail: scans history like Next; fine below ~10k operations;
	// query indexed active occupancy if the history grows beyond that.
	all, err := s.Store.AllOperations()
	if err != nil {
		return nil, err
	}
	class := OperationCapacityClass(op.Kind)
	ceiling := s.MaxConcurrentRuns
	if class == CapacityObservation {
		ceiling = s.MaxConcurrentObservations
	}
	held := map[string]bool{}
	operations := map[string]RunOperation{}
	for _, current := range all {
		operations[current.ID] = current
		if current.Lease == nil || (current.State != Leased && current.State != Running) {
			continue
		}
		if current.RunID == op.RunID {
			return nil, nil
		}
		if OperationCapacityClass(current.Kind) == class {
			held[current.RunID] = true
		}
	}
	now := s.Clock.Now()
	current, found := operations[op.ID]
	if !found || !leasable(current, operations, now, false) || OperationExpired(current, now) {
		return nil, nil
	}
	if len(held) >= ceiling {
		return &CapacityWait{Class: class, Ceiling: ceiling}, nil
	}
	if consumesVerification(op.Kind) {
		saturated, err := s.VerificationSaturated(op.RunID)
		if err != nil {
			return nil, err
		}
		if saturated {
			return &CapacityWait{Class: CapacityVerification, Ceiling: s.MaxConcurrentVerifications}, nil
		}
	}
	return nil, nil
}
