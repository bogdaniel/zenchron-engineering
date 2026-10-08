package execution

// FailureClass is the typed classification of a failed execution. The type is
// shared with the host, which declares its own host-only classes (verification,
// scope, authority, candidate and workspace conditions) and routes every class
// with RouteFailure. Only the classes an execution implementation itself
// emits are declared here.
type FailureClass string

const (
	FailureTransientProvider FailureClass = "transient_execution_provider"
	// FailureProviderAccountUnavailable is a recoverable EXTERNAL ACCOUNT
	// prerequisite of the execution provider: the provider refused at its own
	// account boundary before any reasoning happened. It is not transient (a
	// retry cannot clear it), not a producer or code failure (no work was
	// attempted), not an authority failure (nothing about permission changed),
	// and not terminal (an operator restores the account and the same run
	// continues). It is deliberately NOT FailureAuthorityWait: conflating an
	// external billing prerequisite with the human-authority boundary would
	// make status tell an operator to resolve an authority condition that does
	// not exist.
	FailureProviderAccountUnavailable FailureClass = "provider_account_unavailable"
	// FailureProviderQuota is an execution worker refused by its own plan or
	// account QUOTA: the allowance is spent and returns on that provider's own
	// schedule. It is deliberately distinct from
	// FailureProviderAccountUnavailable, which is an account prerequisite an
	// operator must repair, and from FailureTransientProvider, which is
	// capacity that clears in seconds.
	//
	// It is a CAPACITY WAIT, not an engineering attempt: no reasoning happened,
	// no candidate moved, no evidence or authority changed. Routing it to a
	// wait is what stops several concurrent workers sharing one subscription
	// from burning a run's remediation budget on an allowance that will simply
	// come back.
	FailureProviderQuota FailureClass = "provider_quota"
	// FailureControllerShutdown is the CONTROLLER stopping, observed from
	// inside an invocation as a cancelled context.
	//
	// It is not the work failing and it is not the operator cancelling the run.
	// Those are different acts with different records: `stop RUN` journals
	// run.cancelled and the Cancelled disposition takes precedence over
	// everything below it, while a shutdown is supposed to leave every run
	// exactly as resumable as its journal says.
	//
	// Classifying it as FailureUnknown routed it to RouteStop, so a supervisor
	// shutting down terminalized whatever was mid-flight - the controller
	// lifecycle failure shape #57 documents, and the exact opposite of what
	// docs/supervisor.md promises about shutdown.
	FailureControllerShutdown FailureClass = "controller_shutdown"
	// FailureProviderRateLimited is the same shape at a shorter timescale: the
	// provider asked to be called less often. It is kept separate from quota
	// because the operator action differs - one waits, the other means the
	// configured concurrency is above what that account tolerates - and an
	// operator cannot see that difference through one merged class.
	FailureProviderRateLimited FailureClass = "provider_rate_limited"
	// FailureProviderNoProgress is a provider invocation the runtime ended
	// because it produced no output for the whole configured inactivity
	// window.
	//
	// It is not a quota, not an account condition, and not a verdict about the
	// work: nothing external refused anything, and the process was alive the
	// entire time. That liveness is exactly what made it invisible - the run
	// that exposed it charged 8h55m of an active-work budget to a coding CLI
	// that had lost its network, and the run wall budget was what eventually
	// noticed.
	//
	// It routes to a bounded RETRY rather than a wait. A stall is a runtime
	// bound reached, like FailureExecutionIncomplete, and the condition may
	// well be gone by the next attempt - but it is bounded by the execution
	// attempt ceiling and the silent interval is still charged to the wall
	// budget, so a provider that always stalls exhausts its attempts in
	// minutes instead of consuming the day. It is deliberately NOT
	// continuation-eligible: silence is not interrupted work waiting to be
	// resumed, and a retry inherits no observations from it.
	FailureProviderNoProgress FailureClass = "provider_no_progress"
	// FailureProviderBackgroundWorkUnresolved means a main-thread Bash call
	// detached explicitly or automatically (#384, #388). This stays unresolved
	// throughout the invocation, regardless of later polls or kill requests.
	//

	// Provider return is not proof of semantic completion, the same finding
	// #376 and #379 made about a provider's own self-report: a process that
	// backgrounded its verification and then wrote a final answer without
	// ever checking back is indistinguishable, from the model's own prose
	// alone, from one that genuinely finished - and the typed stream gives
	// no field that could tell the two apart. The invocation's process tree
	// ends with it, so whatever that shell was running - a test suite, most
	// often - is lost with it: a valid final result here is proof only that
	// this invocation stopped talking, not that the work it describes
	// actually happened.
	//
	// It routes to a bounded RETRY of the SAME execution.invoke operation, the
	// shape FailureFeedbackUnresolved and FailureCheckpointContinuationUnresolved
	// already use: no budget is minted or reset, and an invocation that keeps
	// abandoning its own background work exhausts its attempts and stops
	// truthfully instead of looping forever on a checkpoint it can never
	// admit as resolved. It is deliberately NOT continuation-eligible: the
	// provider was not cut short by a runtime bound, it walked away from work
	// it started on its own, so the retry gets a fresh invocation rather than
	// observations from one that said nothing worth keeping.
	FailureProviderBackgroundWorkUnresolved FailureClass = "provider_background_work_unresolved"
	// FailureProviderUnavailable is the provider's ENDPOINT saying it cannot
	// serve: overloaded, or a gateway status (502/503/504). The host reached
	// it; losing the transport itself is FailureConnectivity.
	//
	// It is a recognized statement, never an inference from silence. Silence
	// is FailureProviderNoProgress; only an explicit diagnostic reaches here,
	// because guessing "offline" from an arbitrary substring is how a provider
	// bug becomes a permanent wait. It is separate from
	// FailureProviderAccountUnavailable - the credential is fine and there is
	// nothing for an operator to repair in their account - and separate from
	// FailureTransientProvider, which is that provider's own capacity rather
	// than the host's ability to reach it at all.
	//
	// It routes to a bounded external WAIT under the existing #83 accounting:
	// a host with no network is not performing engineering work, so the
	// interval must not be charged to the active-work budget, and the same run
	// continues once connectivity returns.
	FailureProviderUnavailable FailureClass = "provider_unavailable"
	// FailureConnectivity is the host's TRANSPORT being gone (#380), named by a
	// typed transport error or the provider's own diagnostic: DNS did not
	// resolve, or the connection was refused, reset or unreachable. It routes
	// to an attempt-CONSUMING retry that runs only after a durable bounded
	// backoff (RetryNotBefore), so persistent loss exhausts finite authority
	// rather than waiting forever on a refunded attempt.
	FailureConnectivity FailureClass = "connectivity_unavailable"
	// FailureExecutionIncomplete is a producer invocation that produced real
	// work and then ran out of one of the runtime's own bounds. The work is
	// preserved as a checkpoint; the OPERATION did not complete, which is why
	// it is a failure at all. It routes to a retry because continuing is
	// exactly what it needs, and routing it that way is what puts continuations
	// under the ordinary execution attempt budget instead of a counter of their
	// own.
	FailureExecutionIncomplete FailureClass = "execution_incomplete"

	// FailureRunCancelled is the SAME revocation as the host's deadline class arriving from the
	// other direction: a provider that returned after the operator stopped the run.
	//
	// It sits beside the deadline class deliberately, and routes the same way,
	// because it is the same statement - the authority to turn this result into
	// a candidate had already ended - and not a statement about the work, which
	// may be perfect. What differs is only the cause and when it is knowable: a
	// deadline can be recognised only once the attempt ends, while cancellation
	// exists before it begins and is therefore ALSO refused at acquisition,
	// where it prevents the invocation rather than discarding its result. This
	// class is what remains for an attempt that was legitimately acquired and
	// then outlived the stop.
	//
	// It is also the class a provider reports when its executor committed
	// the operator stop as the owner of its termination (#213): the stop was
	// the first event that initiated ending a still-running execution. That
	// is the operator's act, never FailureControllerShutdown, which would
	// leave a stopped run looking resumable.
	FailureRunCancelled FailureClass = "run_cancelled"
	FailureUnknown      FailureClass = "unknown"
)
