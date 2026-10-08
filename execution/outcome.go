package execution

// Outcome is how one execution ended, as the implementation reports it. It is
// deliberately not the host's OperationState: an execution has no pending,
// leased or running state the host could confuse with its own scheduling.
// The values are the same strings the host's operation states use, so nothing
// recorded from a Result changes on the wire.
type Outcome string

const (
	Succeeded Outcome = "succeeded"
	Failed    Outcome = "failed"
	Cancelled Outcome = "cancelled"
)
