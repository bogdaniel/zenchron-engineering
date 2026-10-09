package runtime

// externalWaitReasons is the CLOSED set of waits that are somebody else's turn.
//
// An execution wall budget bounds the work this system does. It must not be
// spent waiting for a human to read a pull request, for an operator to sign a
// provider back in, or for a rate limit to lift: none of that is the runtime
// working, and a budget that burns through it forces an operator to size their
// engineering budget around how fast people answer email. A run that reached
// its goal and sat overnight awaiting review used to die of
// run_wall_budget_exhausted, which made the #63 review loop unusable at any
// budget that still bounded runaway work.
//
// The set is closed and fail-closed: a reason that is not listed here SPENDS
// the budget. A new wait pauses the clock only when somebody decides it should,
// which is the safe direction for a bound whose whole job is to end things.
// ReasonGoalStateReached is the wait a run settles into when it has done
// everything it can: the candidate is produced, assurance has judged it, and
// what remains is a person in the forge. It is named because a PLAN reads it -
// a stage whose run reached its goal state has produced its output, and the
// stages that depend on it can proceed while the run itself waits for review.
const ReasonGoalStateReached = "goal_state_reached"

// ReasonReviewBudgetExhausted retains accepted review work after its finite
// continuation allowance is spent. Delivery does not imply remediation was published.
const ReasonReviewBudgetExhausted = "review_wall_budget_exhausted"

// ReasonDecisionPending is the wait a run settles into when it has emitted a
// live #473 decision_request this build has not yet admitted a
// DecisionResolution for (#508). It is the one case where "somebody else's
// turn" is a human or designated authority deciding a question the run itself
// asked, never a worker answering its own question: Reconcile holds the run
// here, planning no further operation, until the request resolves.
const ReasonDecisionPending = "decision_pending"

var externalWaitReasons = map[string]bool{
	ReasonDeterministicFailureUnchanged: true,
	// Waiting for a person: review, merge authority, a policy decision only an
	// operator can make.
	ReasonGoalStateReached:          true,
	ReasonReviewBudgetExhausted:     true,
	ReasonDecisionPending:           true,
	"awaiting_authority":            true,
	"authority_blocked":             true,
	"authority_unknown":             true,
	"requested_privilege_expansion": true,
	// Waiting for the operator's own accounts and tools.
	"execution_provider_account_unavailable": true,
	"execution_provider_quota":               true,
	// Rate limiting is the other capacity wait. It is the provider declining to
	// be asked yet, not the runtime working, and leaving it out charged an
	// operator for their provider's backoff.
	"execution_provider_rate_limited": true,
	// The host cannot reach the provider at all. A machine with no network is
	// not performing engineering work, and #238's whole defect was charging
	// exactly this interval to the active-work budget - so leaving it out here
	// would fix the detection and keep the accounting lie.
	"execution_provider_unavailable":   true,
	"assurance_dependency_unavailable": true,
	// Every host verification slot is held by another run (#490). The run
	// executes nothing while it waits, and charging it would turn the host's
	// verification capacity into this run's run_wall_budget_exhausted - the
	// exact conversion of capacity exhaustion into failure #490 forbids. The
	// wait is bounded by the verifications holding the slots, each of which
	// runs under its own physical deadline.
	ReasonVerificationCapacity: true,
	ReasonWorkCapacity:         true,
	ReasonObservationCapacity:  true,
	// The operator has to free disk before anything can proceed; the run is not
	// working while it waits for them.
	"state_storage_exhausted": true,
	// The controller could not install the brokered candidate-Git boundary, so
	// it performed no execution at all. Nothing is running and an operator has
	// to repair the installation.
	"candidate_guard_unavailable": true,
	// A dead owner's process still holds the candidate (#168); nothing was
	// dispatched and an operator has to stop it.
	"candidate_writer_alive": true,
	// The controller stopped. The run is not working, and it is waiting for a
	// supervisor to exist again rather than for anything it can do itself.
	"controller_shutdown":  true,
	WatchWaitingGitHubAuth: true,
	// Waiting for a human decision about the source or the pull request.
	WatchWaitingOptInRemoved:       true,
	"source_intent_changed":        true,
	"source_closed":                true,
	"pull_request_closed_unmerged": true,
	"candidate_external_changed":   true,
}
