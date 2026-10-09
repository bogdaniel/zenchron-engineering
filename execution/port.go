package execution

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Port runs one bounded execution. Its writes are confined to locations the
// host supplies in the Request: the candidate directory, plus the
// runtime-owned paths outside it (ScratchDir and, when set, the result slots
// ReviewerResultPath, FeedbackResolutionPath, HandoffPath and MessagePath).
// Writing a result slot states a claim the host decodes and admits; it grants
// nothing by itself. A Port has no authority and receives no publication
// credentials.
type Port interface {
	Execute(context.Context, Request) (Result, error)
}
type Request struct {
	RunID string
	// OperationID is the runtime operation that authorized this invocation.
	// It is OPERATIONAL IDENTITY, not provider metadata: a brokered candidate
	// command is a runtime-owned side effect, so its Docker lifecycle has to be
	// bound to the exact operation that caused it. Without it a crashed
	// controller has no durable name it alone may reconcile, and recovery would
	// have to guess by prefix or label.
	OperationID string
	// Attempt is the PHYSICAL invocation number for OperationID: which try of
	// this operation this particular provider call is. The runtime derives it
	// from its own monotonic invocation count and the evidence already stored,
	// so it is a fact the runtime owns; a provider must never invent, default,
	// or carry it over.
	//
	// It is not the operation's budget attempt. That one is refunded when a
	// provider condition routes to an external wait, so it can move backwards -
	// and an identity that moves backwards points at a transcript that already
	// exists.
	//
	// Together with RunID and OperationID it is the complete identity of ONE
	// invocation, and therefore of the forensic transcript that invocation
	// produces. Before this existed a transcript was keyed by run alone, so a
	// retry silently overwrote the evidence of the attempt before it and the
	// history of a bounded retry could not be read back at all.
	Attempt int
	// PriorAttemptFailure is the runtime's own typed classification of the
	// PREVIOUS attempt of this exact operation, or empty on attempt 1. It comes
	// from durable scheduler state - the same provenance the reattemptability
	// rule reads - never from a diagnostic string, and it is what decides
	// whether this retry may inherit that attempt's observations. See
	// runtime.PriorAttemptContextEligible.
	PriorAttemptFailure   FailureClass
	SourceSnapshot        Ref
	ControllerID          string
	Base                  Ref
	Candidate             Candidate
	CandidateDir          string
	Contract              Ref
	Objective             string
	AcceptanceObligations []string
	Constraints           []string
	Prohibitions          []string
	Permissions           []string
	TrustedInstructions   string
	// Instructions are the operator-owned InstructionPack lines an AgentProfile
	// contributes. They are TRUSTED text and reach the worker alongside the
	// runtime's own instructions, which is exactly why they may come only from
	// the operator-owned planning directory. Candidate content never arrives
	// here; it arrives as delimited untrusted data.
	Instructions []string
	Purpose      Purpose
	// Mode is what this invocation requires of the provider. The zero value is
	// the ordinary mutating invocation, so every existing caller keeps its
	// exact behaviour; a planner-role stage sets the non-mutating mode and an
	// adapter that cannot prove one refuses.
	Mode domain.InvocationMode
	// ModelPreference is the AgentProfile's model preference for this
	// invocation. Empty means the agent's own configured default.
	ModelPreference string
	// DenyPermissionBypass is an AgentProfile's refusal of the provider's
	// unsafe permission mode for this stage, even where the agent has standing
	// operator permission for it. It is a NARROWING and there is no member
	// beside it that permits one: a profile can refuse the bypass and can never
	// grant it.
	DenyPermissionBypass bool
	Findings             []Finding
	// Feedback is the admitted, applicable, undelivered reviewer feedback this
	// invocation is being given. It is UNTRUSTED DATA: it reaches the worker
	// inside explicit delimiters, framed by the runtime-owned trusted
	// instructions as third-party description of desired behaviour, and it
	// expands no permission. Every item in it has already passed the actor
	// admission gate; nothing that failed that gate is ever placed here.
	Feedback []FeedbackContext
	// Upstream is the accepted output of the plan stages this one depends on:
	// the exact commit and tree, and the DIFF itself.
	//
	// The diff is candidate content, so it reaches the worker the way every
	// other piece of candidate-derived text does - as delimited untrusted data
	// framed by the runtime-owned instructions - and it is never stored in a
	// durable payload. It exists because a reviewer that cannot see the change
	// is not reviewing it: an independent review stage runs in its own
	// workspace at the trusted base, and this is what makes its work real.
	Upstream []UpstreamContext
	// ReviewerResultPath is the runtime-owned file this invocation must write
	// its structured verdict to, set only for a reviewer stage.
	//
	// It is supplied BY the runtime and lives outside the candidate workspace,
	// which is what makes the channel unspoofable: repository content is not on
	// this path and cannot predict it, and the runtime empties the slot before
	// the invocation so no earlier attempt's answer can be inherited. Empty for
	// every other stage, and a provider that is given none emits no verdict.
	ReviewerResultPath string
	// FeedbackResolutionPath is the runtime-owned file a producer invocation
	// may write an explicit no-change resolution to, set only when this
	// invocation was given admitted feedback to address. It is the same kind
	// of unspoofable, runtime-cleared channel ReviewerResultPath is, and it
	// exists so a producer that decides no change is required can STATE that
	// rather than have it inferred from an unmodified workspace (#376).
	FeedbackResolutionPath string
	// HandoffPath is the runtime-owned file an ORCHESTRATED invocation must
	// write its typed handoff to (#470), set only for a run an orchestration
	// batch created. It is the same kind of unspoofable, runtime-cleared
	// channel the two paths above are, and the runtime - not the provider -
	// reads it after a completed invocation; see runtime/handoff_slot.go.
	HandoffPath string
	// MessagePath and Communication: the #473 message slot and inbox; see runtime/communication_slot.go.
	MessagePath, Communication string
	// RequiredTools are the executables THIS invocation's contract obliges the
	// worker to run, derived from the contract's own frozen acceptance
	// obligations.
	//
	// It is the invocation's requirement, never the operator's global toolchain
	// declaration. That list is a readiness ceiling - which executables the
	// brokered environment must resolve at all - and granting from it would
	// give every stage every command family the operator ever declared.
	// Customization may narrow privilege; it may never silently widen it.
	RequiredTools []string
	// ScratchDir is the runtime-owned, EXEC-CAPABLE build scratch brokered to
	// this invocation as GOTMPDIR and GOCACHE.
	//
	// A Go toolchain does not only compile: `go test` links a binary and then
	// EXECUTES it. Brokering a module cache while leaving the build directory
	// to the process default meant that binary landed wherever the surrounding
	// environment put temporary files, and the runtime's own verifier sandbox
	// mounts that location noexec. The candidate then failed a check no
	// candidate could pass.
	ScratchDir string
	// Deadline is the ABSOLUTE instant this invocation's authority ends, taken
	// from the operation's durable remaining execution budget.
	//
	// It is carried as an instant rather than left to be re-derived from a
	// duration so that what bounds the process and what is recorded as its
	// authority are ONE fact. Two arithmetic results computed at different
	// moments can disagree, and a provenance record that disagrees with the
	// bound is worse than none: it reads as forensic truth.
	Deadline *time.Time
	Budgets  Budget
}

// Purpose is deliberately operational rather than a provider role.
type Purpose string

const (
	PurposeInitial     Purpose = "initial_implementation"
	PurposeRemediation Purpose = "remediation"
	// PurposePlanning is REASONING about what work is required. It produces
	// a structured proposal and changes nothing: it is the only purpose that
	// runs in a non-mutating provider mode, and the runtime verifies the
	// workspace afterwards rather than trusting that claim.
	PurposePlanning Purpose = "planning"
	// PurposeContinuation resumes work a bounded stop interrupted. It is
	// exact-bound to the runtime-owned checkpoint commit and tree the previous
	// invocation produced, so the provider sees a clean workspace at a known
	// revision and never unbound dirty state. It carries no findings: nothing
	// judged the work, it was simply cut off.
	PurposeContinuation Purpose = "continuation"
	// PurposeHandoffRepair rewrites ONE handoff document the strict decoder
	// refused, and nothing else (#492). It runs in an empty runtime-owned
	// directory instead of the candidate workspace, is denied the unsafe
	// permission bypass, and is given no tools, findings or feedback: the
	// engineering is already committed and this invocation cannot reach it.
	PurposeHandoffRepair Purpose = "handoff_repair"
	// PurposeIndependentReview is an independent engineering review of an
	// EXTERNALLY supplied subject - a pull request's exact head, not a
	// candidate this runtime produced (#233). Like planning, it is the other
	// purpose that runs in a non-mutating provider mode: a reviewer may read,
	// and may run independent checks/tests that write only to ScratchDir, but
	// the candidate workspace it was given must verify unchanged afterwards.
	// It is never implementation or remediation, and it grants no merge,
	// publication or candidate-mutation authority by itself.
	PurposeIndependentReview Purpose = "independent_review"
)

type Budget struct {
	MaxTokens     *int64
	MaxCostMicros *int64
	WallLimit     time.Duration
	// InactivityLimit is how long this ONE invocation may go without producing
	// observable output before the runtime terminates it.
	//
	// It sits beside WallLimit because it is the same kind of statement - a
	// bound this invocation runs under - and because every caller that states
	// one has to state the other in the same place. It is not a share of
	// WallLimit and does not reduce it: a provider that keeps talking is still
	// stopped by the total bound, and a provider that goes quiet is stopped by
	// this one long before the total bound would notice. Zero means no
	// inactivity bound, which is what a caller predating this budget gets.
	InactivityLimit time.Duration
	// InactivityWindow is the CONFIGURED per-attempt window InactivityLimit was
	// derived from. The two differ when a byte_output successor inherits only
	// the remainder. A provider control derived from the window - Claude's
	// background-wait ceiling (#322) - reads this one, so a shrunken remainder
	// can never silently shrink it. Zero means the caller stated no separate
	// window, and InactivityLimit is then the configured window itself: the
	// planner (runtime/supervisor.go) and any caller passing the configured limit
	// directly rely on that, so zero must stay legal.
	InactivityWindow time.Duration
}
type Result struct {
	ProviderID, Model, AuthMode string
	Attempt                     int
	Outcome                     Outcome
	Tokens, CostMicros          *int64
	Artifacts                   []Artifact
	ChangeSummary               string
	ChangedPaths                []string
	Failure                     *Failure
	// PriorContext is the runtime's account of the prior-attempt observations
	// this invocation was actually given, or nil when it was given none. It is
	// an observation about the handoff, not about the work, and it exists so a
	// replayed run can explain a retry rather than leaving an operator to infer
	// what the model saw.
	PriorContext *PriorAttemptObservations
	// Invocation is the non-secret record of HOW this attempt was invoked:
	// which executable, which version, which permission and sandbox mode, and
	// which security-relevant arguments. It is a POINTER because a provider
	// that cannot state it truthfully must state nothing rather than a zero
	// value that would read as "no sandbox, no bypass, unknown auth" - three
	// claims it did not make.
	Invocation *InvocationProvenance
	// Executed is a provider's own report that this attempt reached its model
	// (at least one completed exchange), for a provider with no process
	// provenance to say so. A provider that failed after real work sets it,
	// so that work stays charged even when the failure routes to a wait.
	Executed bool
	// Answer is the invocation's SEMANTIC final answer text, exposed by an
	// adapter that can state one directly rather than leaving a consumer to
	// locate it inside the forensic transcript. It is empty whenever the
	// adapter has no such shape to offer - every provider but Claude's
	// structured stream, and even that one on a failed or malformed
	// invocation - and emptiness is exactly the signal that a consumer must
	// fall back to reading the transcript itself. Provider-specific transport
	// framing (stream-json event shapes, escaping, and the like) is decoded by
	// the adapter that owns it and never described here.
	Answer string
}

// AttemptRef is the runtime-owned identity of one provider
// invocation: which run, which authorizing operation, which attempt of it.
//
// It is deliberately built only from durable scheduler facts. A timestamp, a
// model response id, a random provider id or anything derived from candidate
// text would all be unusable here for the same reason: replay has to arrive at
// the same identity from the journal alone, and none of those are in it.
type AttemptRef struct {
	RunID       string
	OperationID string
	Attempt     int
}

// AttemptRef is the identity of the invocation this request authorizes.
func (r Request) AttemptRef() AttemptRef {
	return AttemptRef{RunID: r.RunID, OperationID: r.OperationID, Attempt: r.Attempt}
}

// Validate refuses an identity a provider cannot honestly write evidence
// under. It is a PLUMBING check, not a policy one: reaching it with a zero
// attempt means a request producer was never wired to the scheduler, which is
// a defect in this runtime rather than a condition of the run.
func (a AttemptRef) Validate() error {
	// Whitespace is not an identity. ToolBroker already refuses a blank
	// operation id as unbound, and a namespace of encoded spaces would be a
	// durable place to file evidence that no operation authorized.
	if strings.TrimSpace(a.RunID) == "" {
		return fmt.Errorf("execution attempt identity requires a run")
	}
	if strings.TrimSpace(a.OperationID) == "" {
		return fmt.Errorf("execution attempt identity requires an authorizing operation")
	}
	if a.Attempt < 1 {
		return fmt.Errorf("execution attempt identity requires the scheduler attempt for operation %q, got %d", a.OperationID, a.Attempt)
	}
	return nil
}

type Failure struct {
	Classification   FailureClass
	RawDiagnosticRef string
}
