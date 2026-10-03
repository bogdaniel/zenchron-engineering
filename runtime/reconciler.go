package runtime

// reconciler.go is the Phase 8 reconcile loop:
//
//	load/replay run -> validate invariants -> evaluate conditions
//	  -> plan desired operation -> validate operation
//	  -> scheduler acquires ONE operation -> operation.before
//	  -> bounded side effect (operations.go) -> operation.after
//	  -> replay/reconcile again
//
// The single most important structural rule here is what is NOT in this file:
// there is no `switch phase { case "execute": ... }`. `phase` is an operator
// projection computed for a status report and never read to decide anything.
// What to do next is decided by replaying the journal (Reduce + Project) and
// asking, for each operation the runtime knows how to perform, two pure
// questions:
//
//	bind(state) -> (idempotency key, is this operation wanted at all?)
//	satisfied(kind, key) -> has exactly this operation already succeeded?
//
// The idempotency key binds the exact state the operation would act on: the
// pinned source digest, the exact candidate commit and tree, the contract
// revision, the exact pull request head. A satisfied operation is therefore
// never repeated, and an operation whose state moved underneath it is
// automatically wanted again because its key changed. That is the whole
// planner, and it is a pure function of the journal.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// Operation kinds
// ---------------------------------------------------------------------------

const (
	OpSourceObserve     = "source.observe"
	OpContractCompile   = "contract.compile"
	OpCandidateCreate   = "candidate.create"
	OpExecutionInvoke   = "execution.invoke"
	OpRemediationGofmt  = "remediation.gofmt"
	OpCandidateCommit   = "candidate.commit"
	OpAssuranceGo       = "assurance.go"
	OpAssuranceSemantic = "assurance.semantic"
	OpAuthorityEvaluate = "authority.evaluate"
	OpBaseIntegrate     = "base.integrate"
	OpCandidatePush     = "candidate.push"
	OpPullRequestCreate = "pull_request.create"
	OpPullRequestUpdate = "pull_request.update"
	OpGitHubObserve     = "github.observe"
)

// publicationKinds are the operations that change protected remote state.
// Every one of them is gated on a current, authorized #7 decision.
var publicationKinds = map[string]bool{OpCandidatePush: true, OpPullRequestCreate: true, OpPullRequestUpdate: true}

// PublicationActionType is the protected action the runtime asks #7 about
// before it pushes or opens a pull request. Push is not a separate authority:
// pushing the run-owned branch exists only to publish, so one decision gates
// the whole publication, evaluated against the exact candidate commit.
const PublicationActionType = "git.pull_request.create"

// HumanEvidenceClass is the class a person records through the operator
// authority boundary. No provider produces it, and nothing may produce it on a
// person's behalf.
const HumanEvidenceClass domain.EvidenceClass = "human_approval"

// SemanticEvidenceClass is the class an INDEPENDENT semantic acceptance
// producer answers: whether the candidate actually discharges the acceptance
// criterion it was asked to, as opposed to whether it compiles and its tests
// pass. It is deliberately distinct from AssuranceEvidenceClass - an automated
// test suite does not answer it - and from a security review, and from a human
// approval. Nothing aliases one to another.
const SemanticEvidenceClass domain.EvidenceClass = "semantic_acceptance"

// AssuranceEvidenceClass is the evidence class the runtime's verifier produces.
// A required claim of any other class (a human approval, an external audit) is
// simply not satisfied by a verifier run, which is what keeps #7 in charge.
const AssuranceEvidenceClass domain.EvidenceClass = "automated_test"

// maxReconcilePasses bounds one Reconcile call. It is not a daemon: it drives
// one run until the run reaches a stop condition or stops making progress.
const maxReconcilePasses = 64

// ---------------------------------------------------------------------------
// Replayed state
// ---------------------------------------------------------------------------

// sourceRecord is the pinned source snapshot: repository identity, issue
// number, URL, title/body/label digests, updated_at, open/closed state, and the
// initiating operator. The untrusted title and body are deliberately absent -
// they live in a local-only file the record references - so no durable event
// row ever carries third-party text. Digest is what decides whether the pinned
// source moved.
type sourceRecord struct {
	Repository   string `json:"repository"`
	Issue        int    `json:"issue"`
	URL          string `json:"url"`
	TitleSHA256  string `json:"title_sha256"`
	BodySHA256   string `json:"body_sha256"`
	LabelsSHA256 string `json:"labels_sha256"`
	UpdatedAt    string `json:"updated_at"`
	State        string `json:"state"`
	Operator     string `json:"operator"`
	Digest       string `json:"digest"`
	BaseRevision string `json:"base_revision"`
	// SnapshotPath references the local-only file holding the untrusted issue
	// title and body. The text itself is NEVER in a durable event row: the
	// journal carries identity, digests and a reference, exactly as it does for
	// a provider transcript.
	SnapshotPath string `json:"snapshot_path"`
}

// mutationResult is what a producing operation records about the change it
// left in the candidate workspace. Mutated is established by inspecting the
// workspace, never by trusting a provider's self-report.
type mutationResult struct {
	Mutated      bool         `json:"mutated"`
	PathCount    int          `json:"path_count"`
	FailureClass FailureClass `json:"failure_class,omitempty"`
	ProviderID   string       `json:"provider_id,omitempty"`
	// ProviderExecuted records that this attempt actually reached a worker, as
	// opposed to being refused before any execution began. It is the difference
	// between an external condition that cost nothing and one that cost twenty
	// minutes of provider work before it appeared, and only the first may be
	// refunded to the run's execution budget.
	ProviderExecuted bool `json:"provider_executed,omitempty"`
	// DiscardRefusals is how many destructive Git operations the runtime
	// refused during this invocation, and DiscardRefused is the bounded shape
	// of the most recent one.
	//
	// They are OBSERVATION and not failure: a provider that reached for a
	// destructive recovery, was refused, and then did the work properly
	// succeeded - and the refusal is still the most interesting thing that
	// happened, because it is where expensive reasoning was nearly lost. That
	// is why it is recorded here rather than becoming a FailureClass: the
	// runtime knows exactly what it refused, so #241's rule against reporting
	// it as quota, unavailability, a stall or an unknown is satisfied by it
	// not being a failure at all.
	DiscardRefusals int    `json:"discard_refusals,omitempty"`
	DiscardRefused  string `json:"discard_refused,omitempty"`
	// ContentDigest identifies the uncommitted change this operation left in
	// the workspace (workspaceContentDigest), taken when it returned. It is
	// what binds held material to exact content when a budget ends the run
	// before the change is committed (#203). Empty means unknown.
	ContentDigest string `json:"content_digest,omitempty"`
	// ResolvedFeedback is the set of admitted feedback keys this attempt
	// explicitly and typedly resolved as requiring no change, admitted by
	// AdmitFeedbackResolution against exactly what this attempt was
	// delivered and exactly the subject it was invoked against (#376).
	//
	// It is populated ONLY through that admission. It is never set from
	// "the provider returned success and the workspace is unchanged" alone -
	// that observation is indistinguishable from a provider that deferred
	// unfinished work and simply exited, which is the exact defect #376
	// exists to close. Discharge reads this field and nothing else.
	ResolvedFeedback []string `json:"resolved_feedback,omitempty"`
	// CheckpointResolved records that AdmitCheckpointCompletion admitted a
	// FeedbackResolutionCheckpointComplete claim for this attempt, independent
	// of how many feedback keys (if any) it named and independent of Mutated.
	// ResolvedFeedback alone cannot say this: an admitted resolution naming
	// zero keys - the shape a continuation that inherits a checkpoint with no
	// feedback obligation writes to state "this checkpoint is complete" -
	// leaves ResolvedFeedback empty exactly like no resolution was ever
	// admitted at all. This is #379's generalization of #376 from feedback
	// discharge to checkpoint continuation: a continuation that returns
	// without setting this is not evidence the checkpoint it inherited is
	// complete, whether or not it mutated the candidate further.
	CheckpointResolved bool `json:"checkpoint_resolved,omitempty"`
}

// pushResult records how a push settled: landed by this attempt, or already
// present on the remote and merely confirmed.
type pushResult struct {
	Ref       string `json:"ref"`
	Revision  string `json:"revision"`
	Confirmed bool   `json:"confirmed"`
}

// runState is one replayed view of one run. Everything a planner may read is
// here, and nothing here comes from wall time, the filesystem, or the network.
type runState struct {
	feedbackCached     *FeedbackState
	feedbackEventCount int
	rt                 *EngineeringRuntime
	run                EngineeringRun
	snapshot           RunSnapshot
	events             []EngineeringEvent
	projection         RunProjection
	sources            []sourceRecord
	source             *sourceRecord
	controllerChanged  bool
	// External-wait accounting, folded from the journal once; see externalWait.
	waitExcluded  time.Duration
	waitOpenSince time.Time
	waitOpenWork  time.Duration
	waitComputed  bool
}

func (r *EngineeringRuntime) load(runID string) (*runState, error) {
	run, ok, err := r.deps.Store.Run(runID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("unknown run %q", runID)
	}
	events, err := r.deps.Store.Events(runID)
	if err != nil {
		return nil, err
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		return nil, err
	}
	projection, err := Project(events)
	if err != nil {
		return nil, err
	}
	state := &runState{
		rt: r, run: run, snapshot: snapshot, events: events, projection: projection,
		// A DIFFERENT CONTROLLER IS STILL THE DEFAULT REFUSAL. What changed
		// with #234 is that one specific transition can be converted from
		// drift into an admitted succession by evidence in this run's own
		// journal; everything without that evidence parks exactly as before.
		controllerChanged: !ControllerSuccessionContinues(run, events, r.controller, r.wasTransitionActivated()),
	}
	for _, op := range state.succeeded(OpSourceObserve) {
		var record sourceRecord
		if len(op.Result) == 0 || json.Unmarshal(op.Result, &record) != nil {
			continue
		}
		state.sources = append(state.sources, record)
	}
	if n := len(state.sources); n > 0 {
		state.source = &state.sources[n-1]
	}
	return state, nil
}

// succeeded returns the run's succeeded operations of one kind in durable
// queue order. It reads the journal's folded operation documents, not the
// scheduler's rows, so it reports what the run can prove it did.
func (s *runState) succeeded(kind string) []RunOperation {
	var out []RunOperation
	for _, op := range s.snapshot.Operations {
		if op.Kind == kind && op.State == Succeeded {
			out = append(out, op)
		}
	}
	return sortOperations(out)
}

// operationKey qualifies a binding with its operation kind. The durable store
// holds ONE unique idempotency key per run across every kind, and two different
// operations legitimately bind to the same state - assurance and authority both
// bind to the exact commit, tree and contract revision - so the kind has to be
// part of the key or the second one would be refused as the first one's twin.
func operationKey(kind, binding string) string { return kind + "#" + binding }

// bindingOf recovers the state binding from a durable operation's key.
func bindingOf(op RunOperation) string {
	return strings.TrimPrefix(op.IdempotencyKey, op.Kind+"#")
}

// satisfied is the planner's whole memory: has exactly this operation, bound
// to exactly this state, already succeeded?
func (s *runState) satisfied(kind, binding string) bool {
	op, ok := s.operationByKey(kind, binding)
	return ok && op.State == Succeeded
}

func (s *runState) operationByKey(kind, binding string) (RunOperation, bool) {
	key := operationKey(kind, binding)
	for _, op := range s.snapshot.Operations {
		if op.Kind == kind && op.IdempotencyKey == key {
			return op, true
		}
	}
	return RunOperation{}, false
}

// lastFailure is the failure class an operation durably recorded the last time
// it ran. It reads the journal's folded operation document - the same record
// `satisfied` reads - so it reports what the run can prove, not what a
// scheduler row happens to hold.
//
// It answers false in exactly two cases, and they are different absences.
//
// An operation whose last journalled outcome is not a failure recorded no
// failure at all: a crash between operation.before and operation.after leaves
// the operation mid-flight, and there is no failure to route for work nobody
// watched finish. Such an operation is retry eligible exactly as before.
//
// A failure whose handler determined no class is a failure this boundary
// cannot route. It is left under the attempt budget alone, which is the
// behaviour it already had. Note that this is NOT the `unknown` class: a
// handler that classified its failure as FailureUnknown said something durable,
// and RouteFailure stops it.
//
// ponytail: a handler that records no class therefore keeps budget-only
// retries. That is deliberate - withholding a retry needs a route to withhold
// it by, and inventing one here would be this boundary redefining a
// classification that belongs to the handler. Each handler that starts
// recording a class comes under the route rule with no change here.
func (s *runState) lastFailure(id string) (FailureClass, bool) {
	op, ok := s.snapshot.Operations[id]
	if !ok || op.State != OperationFailed {
		return "", false
	}
	var result mutationResult
	if decodeJSON(op.Result, &result) != nil || result.FailureClass == "" {
		return "", false
	}
	return result.FailureClass, true
}

// lastReviewRefusal reads the exact reviewer-protocol refusal THIS operation's
// most recent failed attempt recorded, so a bounded retry of the same
// reviewer invocation (#374) can be told why rather than being re-dispatched
// blind. It is scoped to FailureReviewerProtocolIncomplete deliberately: that
// is the one class a retry of this exact operation, not a different one,
// answers - unlike FailureVerification's admission refusals, which only ever
// surface once a candidate exists for the run to remediate (see findings()).
func (s *runState) lastReviewRefusal(id string) (*ReviewerResultRefusedError, bool) {
	op, ok := s.snapshot.Operations[id]
	if !ok || op.State != OperationFailed {
		return nil, false
	}
	var record executionRecord
	if decodeJSON(op.Result, &record) != nil ||
		record.FailureClass != FailureReviewerProtocolIncomplete || record.ReviewRefusal == nil {
		return nil, false
	}
	return record.ReviewRefusal, true
}

// reviewerProtocolCorrectionExhausted reports whether this operation's two
// most recent CONSECUTIVE attempts both failed to cross the reviewer-result
// protocol (#374). The runtime grants exactly one corrective reviewer
// invocation after a malformed or missing result; it is independent of
// max_execution_attempts, which bounds a different question (how many tries
// this operation gets, for any reason) and must not be read as also answering
// this one. A second FailureReviewerProtocolIncomplete in a row means the
// correction offered through lastReviewRefusal was not taken up.
//
// It reads the full event history, not the folded operation, because the
// folded operation (lastFailure, lastReviewRefusal) only ever holds the most
// recent attempt - exactly one attempt short of what "two in a row" needs. A
// non-reviewer-protocol attempt anywhere in the streak - success, a different
// failure class, an external wait - resets the count: only a run of
// uninterrupted protocol failures, starting from the most recent attempt,
// counts against the one-correction grant.
func (s *runState) reviewerProtocolCorrectionExhausted(id string) bool {
	streak := 0
	for _, e := range s.events {
		if e.Type != EventOperationAfter || e.OperationID != id {
			continue
		}
		var op RunOperation
		if decodeJSON(e.Payload, &op) != nil || op.State != OperationFailed {
			streak = 0
			continue
		}
		var record executionRecord
		if decodeJSON(op.Result, &record) == nil && record.FailureClass == FailureReviewerProtocolIncomplete {
			streak++
		} else {
			streak = 0
		}
	}
	return streak >= 2
}

// currentOperation is the most recently started operation, for the status
// report only. It is never consulted to decide what to do next.
func (s *runState) currentOperation() (RunOperation, bool) {
	var latest RunOperation
	var found bool
	for _, op := range sortOperations(mapValues(s.snapshot.Operations)) {
		if op.StartedAt != nil {
			latest, found = op, true
		}
	}
	return latest, found
}

func mapValues(m map[string]RunOperation) []RunOperation {
	out := make([]RunOperation, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// epoch is the sequence of the last event that is not an operation lifecycle
// point. Observation operations are keyed on it, which makes them idempotent
// within one attempt - a crash mid-operation replays to the same epoch and so
// to the same key - while still allowing a later pass to look again once
// something actually changed.
func (s *runState) epoch() int64 {
	var epoch int64
	for _, e := range s.events {
		switch e.Type {
		case EventOperationPlanned, EventOperationBefore, EventOperationAfter:
		default:
			epoch = e.Sequence
		}
	}
	return epoch
}

func (s *runState) epochKey() string { return "epoch-" + strconv.FormatInt(s.epoch(), 10) }

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

var externalWaitReasons = map[string]bool{
	// Waiting for a person: review, merge authority, a policy decision only an
	// operator can make.
	ReasonGoalStateReached:          true,
	ReasonReviewBudgetExhausted:     true,
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
	// The operator has to free disk before anything can proceed; the run is not
	// working while it waits for them.
	"state_storage_exhausted": true,
	// The controller could not install the brokered candidate-Git boundary, so
	// it performed no execution at all. Nothing is running and an operator has
	// to repair the installation.
	"candidate_guard_unavailable": true,
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

// activeElapsed is the time this run has been the SYSTEM'S turn, derived from
// the durable journal rather than from a stopwatch: every interval it excludes
// is bounded by two recorded events, so a restart, a crash, or a second
// supervisor reaches the same number from the same rows. An in-memory timer
// would reset on restart and quietly hand a run a fresh budget.
//
// An external wait runs from the run.waiting event that declared it until the
// next event that is not that same wait. Observation the runtime performs while
// waiting - polling the pull request, re-reading the issue - is real work and is
// counted; only the idle gap between ticks is excluded.
func (s *runState) activeElapsed(now time.Time) time.Duration {
	// The MEMOIZED fold - a loaded run state's events never change and
	// conditions() asks several times per pass - through the same arithmetic
	// the plan's ceiling uses.
	excluded, openSince, openWork := s.externalWait()
	return activeFrom(s.run.CreatedAt, excluded, openSince, openWork, now)
}

// externalWait folds the journal once: the total of the CLOSED external-wait
// intervals, the start of an open one, and the work performed inside it.
//
// An external wait is a STATE, not the gap between two adjacent events. It opens
// at the run.waiting that declared an external reason and closes only at the
// next DISPOSITION event - a wait for a different reason, or a terminal event.
// It deliberately does not close on operation events, because recordDisposition
// appends run.waiting only when the disposition or reason CHANGES:
//
//	t0  run.waiting(goal_state_reached)     <- the human's turn begins
//	t1  operation.planned/before/after      <- a poll; still goal_state_reached,
//	                                           so NO second run.waiting is written
//	t2  hours later, still waiting
//
// An earlier version closed the interval at t1 and, finding no new wait event,
// charged t1..t2 to the execution budget. That reproduced the original defect
// after the first polling tick, and the unit tests missed it because they
// synthesized a fresh run.waiting before every observation - a journal
// production never writes.
//
// Work performed WHILE waiting is still work: operation.before/after pairs
// inside the state are added back, so a poll costs its own duration and no more.
//
// Memoized because a loaded runState's events never change, while conditions()
// is called several times per pass and a long-lived run accumulates thousands of
// events.
func (s *runState) externalWait() (excluded time.Duration, openSince time.Time, openWork time.Duration) {
	if s.waitComputed {
		return s.waitExcluded, s.waitOpenSince, s.waitOpenWork
	}
	excluded, waitingSince, work := foldExternalWait(s.events)
	s.waitExcluded, s.waitOpenSince, s.waitOpenWork, s.waitComputed = excluded, waitingSince, work, true
	return excluded, waitingSince, work
}

// foldExternalWait is the fold itself, over events alone.
//
// It is a package-level function rather than a method because the PLAN needs
// the same answer for a stage's child run and must not have a second definition
// of what counts as active time: a plan wall ceiling judged by different
// arithmetic from the run wall ceiling would be two budgets wearing one name.
func foldExternalWait(events []EngineeringEvent) (excluded time.Duration, openSince time.Time, openWork time.Duration) {
	var waitingSince time.Time
	var work time.Duration
	started := map[string]time.Time{}
	closeWait := func(at time.Time) {
		if waitingSince.IsZero() {
			return
		}
		if idle := at.Sub(waitingSince) - work; idle > 0 {
			excluded += idle
		}
		waitingSince, work = time.Time{}, 0
		started = map[string]time.Time{}
	}
	for _, event := range events {
		switch event.Type {
		case EventRunWaiting:
			if externalWaitReasons[payloadReason(event.Payload)] {
				if waitingSince.IsZero() {
					waitingSince = event.OccurredAt
				}
				continue
			}
			closeWait(event.OccurredAt)
		case EventRunCompleted, EventRunFailed, EventRunCancelled:
			closeWait(event.OccurredAt)
		case EventOperationBefore:
			if !waitingSince.IsZero() && event.OperationID != "" {
				started[event.OperationID] = event.OccurredAt
			}
		case EventOperationAfter:
			// The after record is durable before run.waiting. A crash in that
			// gap must preserve external-wait accounting as well as the deadline.
			if waitingSince.IsZero() {
				var op RunOperation
				if decodeJSON(event.Payload, &op) == nil {
					if s, ok := awaitsRetry(op); ok && !s.SpendsActiveWork {
						waitingSince = event.OccurredAt
					}
				}
			}
			if waitingSince.IsZero() || event.OperationID == "" {
				continue
			}
			if at, ok := started[event.OperationID]; ok {
				if spent := event.OccurredAt.Sub(at); spent > 0 {
					work += spent
				}
				delete(started, event.OperationID)
			}
		}
	}
	return excluded, waitingSince, work
}

// ActiveElapsed is how long a run has been WORKING, from its own durable
// record: total elapsed time less the intervals it spent waiting on something
// external, plus back the work it performed inside those intervals.
//
// The plan's aggregate wall ceiling is attributed with this, so "active time"
// means exactly what it means for a run's own wall budget.
func ActiveElapsed(run EngineeringRun, events []EngineeringEvent, now time.Time) time.Duration {
	excluded, openSince, openWork := foldExternalWait(events)
	return activeFrom(run.CreatedAt, excluded, openSince, openWork, now)
}

// activeFrom is the arithmetic itself, over a fold either caller supplies. It
// is one function because the run's wall budget and the plan's wall ceiling
// must mean the same thing by construction: two copies of this could drift, and
// two ceilings that disagree about what "active" means would be two budgets
// wearing one name.
func activeFrom(createdAt time.Time, excluded time.Duration, openSince time.Time, openWork time.Duration, now time.Time) time.Duration {
	elapsed := now.Sub(createdAt)
	// An open wait runs to now, less the work already performed inside it. This
	// is the only part of the answer that depends on the clock.
	if !openSince.IsZero() {
		if idle := now.Sub(openSince) - openWork; idle > 0 {
			excluded += idle
		}
	}
	if excluded > elapsed {
		return 0
	}
	return elapsed - excluded
}

// pinnedBase is the base revision the run was compiled and cloned against. It
// is the FIRST observation's base: later base movement is handled by
// base.integrate and reassessment, never by silently recompiling the contract
// against a base the candidate was never built on.
func (s *runState) pinnedBase() string {
	// A PLAN STAGE that continues upstream work is based on that work, not on
	// the branch the plan started from. Without this a reviewer or an
	// integrator gets a workspace at the trusted base and has nothing to review
	// or integrate - which is exactly what the first live dogfood review
	// reported as a blocking finding about its own workspace.
	//
	// The override is the upstream candidate commit the plan reconciler
	// recorded when it created this run, and it is only ever recorded for an
	// upstream candidate that was PUBLISHED: a commit that exists only in
	// another run's local workspace is not something the governed remote can
	// clone.
	if s.run.Plan != nil && s.run.Plan.BaseRevision != "" {
		return s.run.Plan.BaseRevision
	}
	// An UNPUBLISHED upstream candidate is not reachable by cloning, so the
	// clone starts at the trusted base and the exact commit is transferred into
	// the workspace afterwards. The pinned base stays the trusted one here on
	// purpose: it is what the remote is cloned at, and claiming a base the
	// remote does not have would fail the clone rather than the transfer.
	if len(s.sources) == 0 {
		return ""
	}
	return s.sources[0].BaseRevision
}

// upstreamCandidate is the exact upstream candidate this run must be moved onto
// after cloning, when its plan stage consumes work that was never published.
func (s *runState) upstreamCandidate() *CandidateRef {
	if s.run.Plan == nil || s.run.Plan.UpstreamCandidate == nil {
		return nil
	}
	if !s.run.Plan.UpstreamCandidate.Materializable() {
		return nil
	}
	return s.run.Plan.UpstreamCandidate
}

// pristineCandidateHead is the commit a freshly cloned workspace must be at
// before this run has made any commit of its own: ordinarily the trusted
// base it was cloned at, but the exact upstream candidate when this stage
// consumes one that was never published and had to be transferred into the
// workspace after the clone - see createCandidate.
func (s *runState) pristineCandidateHead() string {
	if ref := s.upstreamCandidate(); ref != nil {
		return ref.Revision
	}
	return s.pinnedBase()
}

// baseRevision is the base the candidate currently sits on: the recorded base
// once a base.integrate has moved it, otherwise the pristine head the
// workspace started from - which, for a stage consuming an unpublished
// upstream candidate, is the transferred candidate rather than the trusted
// base the clone itself resolved. Falling back to pinnedBase here would claim
// the workspace sits on a commit createCandidate never checked it out to.
func (s *runState) baseRevision() string {
	if s.projection.BaseRevision != "" {
		return s.projection.BaseRevision
	}
	return s.pristineCandidateHead()
}

func (s *runState) contractRevision() string { return s.projection.Contract.Revision }

func (s *runState) published() bool { return s.projection.PullRequest != nil }

func (s *runState) merged() bool {
	return s.projection.PullRequest != nil && s.projection.PullRequest.Merged
}

func (s *runState) issueClosed() bool {
	return s.source != nil && s.source.State == string(GitHubClosed)
}

// decidedPublication is the latest #7 decision for the publication action,
// together with the journal position it was written at - which is what makes
// "has this decision seen everything the run can prove" answerable.
func (s *runState) decidedPublication() (AuthorityEvaluation, bool) {
	key := PublicationActionType + "\x00" + s.rt.deps.Repository.DefaultBranch
	decision, ok := s.projection.AuthorityDecisions[key]
	return decision, ok
}

// publicationDecision is that decision without its journal position.
func (s *runState) publicationDecision() (AuthorityEvaluatedPayload, bool) {
	decision, ok := s.decidedPublication()
	return decision.AuthorityEvaluatedPayload, ok
}

// unansweredPublicationDecision is the publication decision a run may still
// WAIT on. A human answer recorded after the decision was journalled is exactly
// what that decision did not see, so it is no longer a reason to keep waiting:
// the run has to re-evaluate before it settles again. Without this the run
// deadlocks - a waiting run performs observation only, so it could never plan
// the authority.evaluate that would clear its own wait.
//
// Only a LATER human answer supersedes a decision. A decision written after the
// answer has already seen it, so an approval that did not satisfy the evaluator
// still settles the run as waiting, and a rejection still settles it as
// blocked. Both terminate.
func (s *runState) unansweredPublicationDecision() (AuthorityEvaluatedPayload, bool) {
	decision, ok := s.decidedPublication()
	if !ok {
		return AuthorityEvaluatedPayload{}, false
	}
	if answer, answered := s.humanAuthorityAnswer(); answered && answer.Sequence > decision.Sequence {
		return AuthorityEvaluatedPayload{}, false
	}
	return decision.AuthorityEvaluatedPayload, true
}

// currentHeadFailure is the only failure a reconciler may act on: one observed
// against the CURRENT head. A finding for a superseded head is recorded in the
// journal and deliberately ignored here - Project's Stale flag is what makes
// that distinction, and remediation is driven from this function alone.
func (s *runState) currentHeadFailure() (FailureClass, bool) {
	if a := s.projection.Assurance; a != nil && !a.Stale && !a.Passed {
		class := a.FailureClass
		if class == "" {
			class = FailureVerification
		}
		return class, true
	}
	// CI annotations and review comments are untrusted external data. They are
	// normalized into a typed failure class and routed; their text never
	// becomes an instruction.
	if ci := s.projection.CI; ci != nil && !ci.Stale && ci.Conclusion == string(GitHubCheckFailure) {
		return FailureCompileTest, true
	}
	if review := s.projection.Review; review != nil && !review.Stale && review.State == string(GitHubReviewChangesRequested) {
		return FailureCompileTest, true
	}
	// An INTERNAL plan reviewer's block is a verdict about the candidate, so it
	// routes exactly like the verifier's: to the producer, bounded by the same
	// remediation budget. Without this the one review a failed candidate can
	// actually receive would be the one that could not reach its producer.
	if blocked := s.projection.StageReview; blocked != nil && !blocked.Stale {
		return FailureVerification, true
	}
	return "", false
}

// failureFingerprint is the no-progress identity of the run's current state.
// It is deliberately built from durable identifiers only, so two attempts that
// differ merely in provider wording are the same lack of progress.
func (s *runState) failureFingerprint() (FailureFingerprint, bool) {
	class, failing := s.currentHeadFailure()
	if !failing {
		return FailureFingerprint{}, false
	}
	fingerprint := FailureFingerprint{
		CandidateTree:    s.projection.CandidateTree,
		ContractRevision: s.contractRevision(),
		FailureSignature: string(class),
		ProviderIdentity: providerIdentity(s),
	}
	if a := s.projection.Assurance; a != nil && !a.Stale {
		fingerprint.VerifierIdentity = a.VerifierDefinition
	}
	if key, wanted := bindExecutionInvoke(s); wanted {
		fingerprint.RemediationIdentity = key
	}
	return fingerprint, true
}

// phase is an OPERATOR PROJECTION. It exists for status output. Nothing in the
// planner, the validator, or a handler reads it.
func (s *runState) phase() Phase {
	switch {
	case s.published():
		return Publish
	case func() bool { _, ok := s.publicationDecision(); return ok }():
		return Authorize
	case s.projection.Assurance != nil:
		return Assure
	case s.projection.CandidateRevision != "":
		return Observe
	case s.projection.Contract != (Ref{}):
		return Execute
	default:
		return Contract
	}
}

// ---------------------------------------------------------------------------
// Invariants
// ---------------------------------------------------------------------------

// invariants are the structural facts a replayed run must satisfy before any
// planning happens. A violation is a fail-closed stop, not a repair: durable
// state that contradicts itself is an operator problem.
func (s *runState) invariants() error {
	if s.snapshot.ID != s.run.ID {
		return fmt.Errorf("replayed snapshot identity does not match the run")
	}
	if s.projection.CandidateRevision != "" && s.projection.Contract == (Ref{}) {
		return fmt.Errorf("a candidate commit exists with no governing contract")
	}
	if s.projection.CandidateRevision != "" && s.pinnedBase() == "" {
		return fmt.Errorf("a candidate commit exists with no pinned base revision")
	}
	if len(s.sources) > 1 {
		first := s.sources[0]
		for _, record := range s.sources[1:] {
			if record.Repository != first.Repository || record.Issue != first.Issue {
				return fmt.Errorf("run observed two different sources")
			}
		}
	}
	if pr := s.projection.PullRequest; pr != nil && s.projection.CandidateRevision != "" {
		if pr.BaseRevision == "" {
			return fmt.Errorf("bound pull request has no base binding")
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Conditions
// ---------------------------------------------------------------------------

// conditions evaluates the run's live disposition from replayed state. It is
// pure and ordered, and the order is the policy:
//
//  1. A merged pull request completes the run. This is passive, observation
//     only, and it wins over EVERYTHING below - including a changed
//     controller, a closed source issue, and an authority wait - because
//     GitHub auto-closes an issue when `Closes #N` merges, and reading that
//     auto-close as a cancellation would be wrong.
//  2. A journalled cancellation is terminal. `autonomy stop` is explicit
//     operator intent, so no later pass may re-settle the run as waiting and
//     silently un-cancel it. It sits below merge precedence for the same
//     reason (1) does: a merge that already landed is a fact about the world,
//     not something a cancellation retracts.
//  3. Only then do the waiting conditions apply.
func (s *runState) conditions() (Disposition, string) {
	if disposition, reason := MergePrecedence(s.merged(), false); disposition == Completed {
		return disposition, reason
	}
	if s.snapshot.Disposition == Cancelled {
		return Cancelled, s.snapshot.Reason
	}
	now := s.rt.deps.Clock.Now()
	// The execution budget bounds ACTIVE work; see activeElapsed. A separate,
	// optional lifecycle deadline is what bounds total calendar time, for an
	// operator who genuinely wants a run to stop existing after a while. They
	// are different questions and overloading one to answer both is what made a
	// pull request awaiting review look like a runaway run.
	reviewDelivered := false
	if runBudgetSpent(s.budgets().WallLimit, s.activeElapsed(now)) {
		if len(s.outstandingReviewKeys()) > 0 {
			if s.reviewContinuationRemaining(now) <= 0 {
				return Waiting, ReasonReviewBudgetExhausted
			}
		} else if s.reviewContinuationDelivered() {
			reviewDelivered = true
		} else {
			return Failed, ReasonRunWallBudgetExhausted
		}
	}
	if deadline := s.budgets().LifecycleDeadline; deadline > 0 && now.Sub(s.run.CreatedAt) > deadline {
		return Failed, ReasonLifecycleDeadlineExhausted
	}
	if s.controllerChanged {
		return Waiting, "controller_changed"
	}
	if s.projection.ObservedExternalHead != "" {
		return Waiting, "candidate_external_changed"
	}
	if s.projection.SourceIntentChanged {
		return Waiting, "source_intent_changed"
	}
	// A closed issue with an open, unmerged pull request is source
	// cancellation semantics; MergePrecedence is the one place that ordering
	// lives, and it has already ruled out the merged case above.
	if disposition, reason := MergePrecedence(false, s.issueClosed()); disposition == Waiting {
		return disposition, reason
	}
	if pr := s.projection.PullRequest; pr != nil && pr.State == string(GitHubClosed) && !pr.Merged {
		return Waiting, "pull_request_closed_unmerged"
	}
	// Continuation depth is its own finite resource, spent by STARTING a new
	// distinct continuation binding and by nothing else.
	//
	// This used to read projection.Checkpoints > MaxExecutionAttempts, which
	// was wrong twice over. It spent the retry budget on productive work, so a
	// run that did four different things in a row was stopped by a ceiling
	// meant for four failures of the same thing; and it counted CHECKPOINTS,
	// which are not continuation bindings. Run
	// run-1b876b78f20d83195e6b503831fcc9c7 is the proof: 4 checkpoints, 3
	// distinct continuation bindings, terminated while still productive.
	if s.continuationCeilingReached() {
		return Failed, ReasonContinuationsExhausted
	}
	// The RUN TOTAL of provider invocations, across every binding. This is the
	// bound a plan's remaining aggregate headroom becomes: continuation depth
	// bounds how many bindings there may be and the attempt budget bounds
	// retries within one, but neither of them bounds the sum, and a plan
	// ceiling is a sum.
	if s.providerInvocationCeilingReached() {
		return Failed, ReasonProviderInvocationsExhausted
	}
	if r := s.projection.Reassessment; r != nil && r.RequestedPrivilegeCount > 0 {
		return Waiting, "requested_privilege_expansion"
	}
	// ONE mapping, shared with the kernel. Every non-authorized status names an
	// outstanding condition, and naming it is what makes goal_state_reached
	// unreachable while a current protected action is unauthorized: the settle
	// path prefers this reason over its own fallback.
	if decision, ok := s.unansweredPublicationDecision(); ok {
		if disposition, reason, outstanding := AuthorityDisposition(decision.Status); outstanding {
			return disposition, reason
		}
	}
	if reviewDelivered {
		return Waiting, ReasonGoalStateReached
	}
	return Active, ""
}

// ---------------------------------------------------------------------------
// Planner
// ---------------------------------------------------------------------------

type desiredOperation struct {
	kind        string
	key         string
	maxAttempts int
}

// operationSpec is one operation the runtime knows how to perform. bind is
// pure: it reads replayed state and answers what exact state this operation
// would act on, or that the operation is not wanted at all.
type operationSpec struct {
	kind string
	bind func(*runState) (string, bool)
}

// operationSpecs is the precedence order. It is a list, not a state machine:
// the planner takes the first entry that is wanted and not already satisfied.
// Observation comes first so a merge is seen before anything else is decided.
var operationSpecs = []operationSpec{
	{OpSourceObserve, bindSourceObserve},
	{OpGitHubObserve, bindGitHubObserve},
	{OpContractCompile, bindContractCompile},
	{OpCandidateCreate, bindCandidateCreate},
	{OpExecutionInvoke, bindExecutionInvoke},
	{OpRemediationGofmt, bindRemediationGofmt},
	{OpCandidateCommit, bindCandidateCommit},
	{OpAssuranceGo, bindAssuranceGo},
	{OpAssuranceSemantic, bindAssuranceSemantic},
	{OpBaseIntegrate, bindBaseIntegrate},
	{OpAuthorityEvaluate, bindAuthorityEvaluate},
	{OpCandidatePush, bindCandidatePush},
	{OpPullRequestCreate, bindPullRequestCreate},
	{OpPullRequestUpdate, bindPullRequestUpdate},
}

// plan returns the next desired operation. It performs no side effect, opens
// no file, makes no network call, and reads no clock beyond the run budget.
func (s *runState) plan() (desiredOperation, bool) {
	for _, spec := range operationSpecs {
		key, wanted := spec.bind(s)
		if !wanted || key == "" {
			continue
		}
		if s.satisfied(spec.kind, key) {
			continue
		}
		return desiredOperation{kind: spec.kind, key: key, maxAttempts: s.attemptsFor(spec.kind)}, true
	}
	return desiredOperation{}, false
}

// runBudgetSpent is the one definition of an exhausted run active-work budget,
// shared by conditions() and grantReviewContinuation so they cannot disagree
// at the boundary. ZERO remaining is spent: no successor may start with no
// authority (#328).
func runBudgetSpent(limit, elapsed time.Duration) bool { return limit > 0 && elapsed >= limit }

// activeWorkRemaining is the run's remaining CUMULATIVE active-work authority,
// from the existing journal-derived counter (#83). Once a review continuation
// has been granted the run budget is already spent and the grant is the
// enclosing envelope, so it is what remains.
func (s *runState) activeWorkRemaining(now time.Time) time.Duration {
	if _, granted := s.reviewContinuationGrant(); granted {
		return s.reviewContinuationRemaining(now)
	}
	return max(s.budgets().WallLimit-s.activeElapsed(now), 0)
}

// attemptLimit is the authority ONE physical attempt starting now receives:
// min(frozen attempt limit, remaining run active work). When the run has less
// left than a full attempt, the attempt is truncated and the bound is the RUN's
// - a stop there is run exhaustion, not an attempt-wall stop. A run frozen
// before the attempt limit existed gets nil: the legacy rule, unchanged.
func (s *runState) attemptLimit(now time.Time) *AttemptLimit {
	limit := s.budgets().AttemptWallLimit
	if limit <= 0 {
		return nil
	}
	if remaining := s.activeWorkRemaining(now); remaining <= limit {
		return &AttemptLimit{Within: remaining, Bound: BoundRunActiveWork}
	}
	return &AttemptLimit{Within: limit, Bound: BoundAttemptWall}
}

// attemptsFor is the attempt ceiling a newly planned operation freezes, read
// from the run's frozen budgets for every kind that has one.
func (s *runState) attemptsFor(kind string) int {
	switch kind {
	case OpExecutionInvoke:
		return s.budgets().MaxExecutionAttempts
	case OpRemediationGofmt:
		return s.budgets().MaxRemediationAttempts
	case OpAssuranceGo, OpAssuranceSemantic:
		return s.budgets().MaxAssuranceAttempts
	default:
		return 3
	}
}

// A retry disposition with finite attempt authority changes status, not the
// observation being retried. Keep its binding so status events cannot mint a
// fresh attempt budget, through to the last attempt.
func observationBinding(s *runState, kind string) string {
	var latest *RunOperation
	for _, op := range s.snapshot.Operations {
		if op.Kind == kind && (latest == nil || op.CreatedAt.After(latest.CreatedAt) || (op.CreatedAt.Equal(latest.CreatedAt) && op.ID > latest.ID)) {
			copy := op
			latest = &copy
		}
	}
	if latest != nil && latest.State == OperationFailed && retryDispositions[latest.RetryDisposition].FiniteAttemptAuthority {
		return bindingOf(*latest)
	}
	return s.epochKey()
}

func bindSourceObserve(s *runState) (string, bool) {
	return observationBinding(s, OpSourceObserve), true
}

func bindGitHubObserve(s *runState) (string, bool) {
	if !s.published() {
		return "", false
	}
	return observationBinding(s, OpGitHubObserve), true
}

func bindContractCompile(s *runState) (string, bool) {
	if s.source == nil || s.pinnedBase() == "" {
		return "", false
	}
	return s.sources[0].Digest + "|" + s.pinnedBase(), true
}

func bindCandidateCreate(s *runState) (string, bool) {
	if s.projection.Contract == (Ref{}) {
		return "", false
	}
	return s.pinnedBase(), true
}

func bindExecutionInvoke(s *runState) (string, bool) {
	if s.projection.Contract == (Ref{}) {
		return "", false
	}
	if key, wanted := bindCandidateCreate(s); !wanted || !s.satisfied(OpCandidateCreate, key) {
		return "", false
	}
	// Initial implementation: no candidate commit exists yet.
	if s.projection.CandidateRevision == "" {
		return "initial|" + s.contractRevision() + "|" + s.pinnedBase(), true
	}
	// Continuation: the head is a runtime-owned checkpoint, so the producer was
	// interrupted rather than finished. One continuation per checkpoint commit,
	// bound to that exact commit, so a continuation can never be confused with
	// a retry of the invocation that produced it. How many DISTINCT bindings of
	// this shape a run may start is bounded in conditions().
	if !s.projection.CandidateComplete {
		return invocationContinuationPrefix + s.projection.CandidateRevision, true
	}
	// Bounded remediation: only a CURRENT-head failure that routes to a
	// producer. An authority wait never reaches this branch, because
	// RouteFailure never routes an authority wait to a provider.
	if class, ok := s.currentHeadFailure(); ok && RouteFailure(class) == RouteProviderRemediation {
		return "remediation|" + s.projection.CandidateRevision + "|" + string(class), true
	}
	// Admitted, applicable, undelivered reviewer feedback. It is the LAST
	// producer branch deliberately: a candidate that does not build is fixed
	// before a reviewer's request is acted on, and the feedback stays pending
	// rather than being consumed by an invocation that was about something
	// else.
	//
	// The binding is the exact set of items, so delivering them satisfies this
	// operation forever and a later comment produces a different binding
	// rather than a retry of this one. That is what makes "the same comment is
	// never sent to the worker twice" a property of the planner rather than of
	// a cursor someone has to remember to advance.
	//
	// An EXISTING unresolved delivery for this head is checked FIRST and takes
	// priority over deriving a fresh binding from newly pending items (#376).
	// pendingFeedbackKeys() empties the instant delivery is journalled - before
	// the attempt that delivered it even succeeds or fails - so re-deriving a
	// binding purely from what is still pending would stop proposing THIS
	// operation again the moment it failed with feedback_unresolved: plan()
	// would find nothing wanted, the run would settle, and the operation would
	// sit failed-but-retryable forever with its feedback still outstanding.
	// Re-finding the EXACT key that existing non-succeeded operation already
	// used keeps it eligible for the scheduler's own bounded retry instead.
	if binding, ok := s.unresolvedFeedbackBinding(s.projection.CandidateRevision); ok {
		return binding, true
	}
	if pending := s.pendingFeedbackKeys(); len(pending) > 0 {
		return "feedback|" + s.projection.CandidateRevision + "|" + digestOfKeys(pending), true
	}
	return "", false
}

// unresolvedFeedbackBinding re-proposes the exact feedback execution binding
// an earlier attempt for this head already started, when that operation has
// not succeeded. See the call site in bindExecutionInvoke for why this is
// necessary rather than merely convenient.
func (s *runState) unresolvedFeedbackBinding(head string) (string, bool) {
	prefix := "feedback|" + head + "|"
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke || op.State == Succeeded {
			continue
		}
		if binding := bindingOf(op); strings.HasPrefix(binding, prefix) {
			return binding, true
		}
	}
	return "", false
}

// pendingFeedbackKeys is the admitted, applicable, undelivered feedback for the
// current head, in stable order.
func (s *runState) pendingFeedbackKeys() []string {
	var keys []string
	for _, decision := range s.feedbackState().Pending(s.projection.Head()) {
		keys = append(keys, decision.Key)
	}
	sort.Strings(keys)
	return keys
}

// digestOfKeys is a stable identity for a SET of feedback items. The keys
// themselves would make an unbounded idempotency key; their digest is fixed
// width and just as exact.
func digestOfKeys(keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])[:32]
}

// invocationContinuationPrefix marks an execution binding as continuing
// interrupted work on an exact checkpoint commit. It is part of the durable
// operation identity, which is what makes continuation depth replayable.
const invocationContinuationPrefix = "continuation|"

// startedContinuationBindings is the set of DISTINCT continuation execution
// bindings durable state shows this run has already started.
//
// It reads operations, not events, because an operation IS the binding: every
// retry of continuation|A reuses one operation with one idempotency key, so
// counting operations counts bindings and counting attempts does not. Nothing
// here looks at checkpoints, commits, provider invocations or reassessments.
func (s *runState) startedContinuationBindings() map[string]bool {
	started := map[string]bool{}
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke {
			continue
		}
		if binding := bindingOf(op); strings.HasPrefix(binding, invocationContinuationPrefix) {
			started[binding] = true
		}
	}
	return started
}

// providerInvocationCeilingReached reports that this run has spent every
// provider invocation it was created with.
//
// It counts ATTEMPTS - one per execution invocation actually begun - because
// that is what a provider account is charged for. The count comes from the
// projection of durable events, so a restart resumes at the same total rather
// than at zero.
func (s *runState) providerInvocationCeilingReached() bool {
	limit := s.providerInvocationLimit()
	if limit <= 0 {
		return false
	}
	// A ceiling refuses the NEXT invocation; it does not retroactively fail a
	// run that spent its last one productively. Without this, a run whose final
	// permitted invocation completed the candidate read as failed the moment it
	// finished - the continuation ceiling has the same exemption, for the same
	// reason.
	if _, wanted := bindExecutionInvoke(s); !wanted {
		return false
	}
	return s.projection.Attempts[OpExecutionInvoke] >= limit
}

// providerInvocationLimit is the run's total, taken from what the run
// persisted. Absent means unbounded, exactly as it does for every run created
// before this bound existed: a run is judged by the budgets it was created
// with, never by whatever is configured now.
func (s *runState) providerInvocationLimit() int {
	if budgets := s.run.Budgets; budgets != nil {
		return budgets.MaxProviderInvocations
	}
	return 0
}

// continuationLimit is the run's continuation bound, taken from durable state.
//
// A run created after #54 persisted an explicit positive budget and is judged
// by it forever, whatever the operator configures later. A run created BEFORE
// #54 persisted nothing, and its absence is not "use the new default": it means
// the run was bounded by the execution-attempt budget, so replaying it has to
// reproduce that. The oldest runs persisted no budgets at all, and for those
// the attempt budget is the configured one, exactly as it was when they ran.
func (s *runState) continuationLimit() int {
	if budgets := s.run.Budgets; budgets != nil {
		if limit := budgets.MaxExecutionContinuations; limit > 0 {
			return limit
		}
		if legacy := budgets.MaxExecutionAttempts; legacy > 0 {
			return legacy
		}
	}
	return s.rt.deps.Budgets.MaxExecutionAttempts
}

// continuationCeilingReached answers the only question the ceiling is about:
// may this run START one more distinct continuation binding?
//
// It asks the planner what binding it wants rather than predicting anything.
// That matters because whether an invocation mutates, completes, or does
// neither is not knowable before it runs - which is exactly why counting
// checkpoints in advance could never express this rule.
//
// Consequences, all of them deliberate:
//
//   - retries of an already-started binding are not refused here at all; they
//     are bounded by that binding's own MaxExecutionAttempts;
//   - a candidate that COMPLETES is never retroactively failed, because a
//     complete head asks for no continuation binding;
//   - the last permitted continuation may finish and go on to assurance;
//   - only a genuinely NEW binding beyond the ceiling is refused, and the
//     checkpoint that asked for it is preserved by not being touched.
func (s *runState) continuationCeilingReached() bool {
	limit := s.continuationLimit()
	if limit <= 0 {
		return false
	}
	binding, wanted := bindExecutionInvoke(s)
	if !wanted || !strings.HasPrefix(binding, invocationContinuationPrefix) {
		return false
	}
	started := s.startedContinuationBindings()
	if started[binding] {
		return false
	}
	return len(started) >= limit
}

func bindRemediationGofmt(s *runState) (string, bool) {
	class, ok := s.currentHeadFailure()
	if !ok || RouteFailure(class) != RouteGofmt || s.projection.CandidateRevision == "" {
		return "", false
	}
	return s.projection.CandidateRevision, true
}

// bindCandidateCommit binds to the producing operation whose mutation has not
// been committed yet. Every producer mutation - a provider invocation or a
// deterministic gofmt - is committed by the runtime, and the binding is the
// operation identity, so one mutation is committed exactly once.
func bindCandidateCommit(s *runState) (string, bool) {
	for _, op := range s.mutations() {
		if !s.satisfied(OpCandidateCommit, op.ID) {
			return op.ID, true
		}
	}
	return "", false
}

// mutations are the succeeded producing operations that actually changed the
// candidate workspace, in durable order.
func (s *runState) mutations() []RunOperation {
	var out []RunOperation
	for _, kind := range []string{OpExecutionInvoke, OpRemediationGofmt} {
		for _, op := range s.succeeded(kind) {
			var result mutationResult
			if len(op.Result) == 0 || json.Unmarshal(op.Result, &result) != nil || !result.Mutated {
				continue
			}
			out = append(out, op)
		}
	}
	return sortOperations(out)
}

// bindAssuranceGo requires an EXECUTION-COMPLETE candidate. Assurance answers
// "is this candidate acceptable"; asking it about work a bounded stop cut off
// mid-invocation answers a question nobody asked, and a pass on preserved
// partial work would carry the whole way to publication eligibility.
func bindAssuranceGo(s *runState) (string, bool) {
	if s.projection.CandidateRevision == "" || s.projection.Contract == (Ref{}) {
		return "", false
	}
	if !s.projection.CandidateComplete {
		return "", false
	}
	return s.projection.CandidateRevision + "|" + s.projection.CandidateTree + "|" + s.contractRevision(), true
}

// bindAssuranceSemantic plans an INDEPENDENT semantic assurance invocation when
// the contract actually requires that class of evidence. It is derived from the
// contract, never from a hardcoded issue: a contract with no semantic claim
// plans no semantic operation, and a configuration with no semantic producer
// plans none either - fulfillability has already refused such a contract before
// any expensive work.
//
// It waits for the automated verifier to have judged the same exact head, so the
// semantic verifier can be told the automated result rather than re-deriving it,
// and so a candidate that does not even build is not sent to a model.
func bindAssuranceSemantic(s *runState) (string, bool) {
	key, wanted := bindAssuranceGo(s)
	if !wanted || !s.satisfied(OpAssuranceGo, key) {
		return "", false
	}
	assurance := s.projection.Assurance
	if assurance == nil || assurance.Stale || !assurance.Passed {
		return "", false
	}
	if s.rt.deps.SemanticAssurance == nil {
		return "", false
	}
	if len(s.semanticClaims()) == 0 {
		return "", false
	}
	return key, true
}

// semanticClaims are the contract's required claims of the semantic class, with
// the material obligations each one discharges. It is the whole question the
// semantic verifier is asked, derived from the exact contract.
func (s *runState) semanticClaims() []SemanticClaimRequest {
	contract, err := s.rt.contractFor(s)
	if err != nil {
		return nil
	}
	var requests []SemanticClaimRequest
	for claimID, claim := range contract.RequiredClaims {
		if claim.EvidenceClass != SemanticEvidenceClass {
			continue
		}
		request := SemanticClaimRequest{ClaimID: claimID}
		for obligationID, obligation := range contract.Obligations {
			if !obligation.Material {
				continue
			}
			for _, discharge := range obligation.RequiredClaims {
				if discharge == claimID {
					request.ObligationIDs = append(request.ObligationIDs, obligationID)
					request.Statements = append(request.Statements, obligation.Statement)
				}
			}
		}
		sort.Strings(request.ObligationIDs)
		sort.Strings(request.Statements)
		requests = append(requests, request)
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i].ClaimID < requests[j].ClaimID })
	return requests
}

// bindBaseIntegrate is the base drift check. Its binding is the candidate head
// together with the run's publication position, so the fetch happens
// immediately before the first publication of a head AND again immediately
// after it - which is what makes the after-publication rule reachable at all,
// since that one is a merge-from-base rather than a rebase.
//
// It requires passing assurance at the head first, so a moved base always
// produces a new tree that reassessment and assurance must see again before
// anything is published from it.
func bindBaseIntegrate(s *runState) (string, bool) {
	head := s.projection.CandidateRevision
	if head == "" {
		return "", false
	}
	assurance := s.projection.Assurance
	if assurance == nil || assurance.Stale || !assurance.Passed {
		return "", false
	}
	return head + "|" + strconv.FormatBool(s.published()) + "|" + strconv.FormatBool(s.satisfied(OpCandidatePush, head)), true
}

// bindAuthorityEvaluate binds the #7 decision to the exact candidate commit,
// tree, contract revision and assurance outcome, AND to the run's latest
// recorded human answer. Any of those moving makes the previous decision
// inapplicable and forces a fresh evaluation.
//
// The human component is what makes an authority wait exitable at all. A
// human.authority_recorded event moves no commit, no tree and no contract
// revision, so without it a satisfied authority.evaluate would never be
// re-planned and an approved run would wait forever.
func bindAuthorityEvaluate(s *runState) (string, bool) {
	key, wanted := bindBaseIntegrate(s)
	if !wanted || !s.satisfied(OpBaseIntegrate, key) {
		return "", false
	}
	binding := s.projection.CandidateRevision + "|" + s.projection.CandidateTree + "|" + s.contractRevision()
	// Appended only when an answer exists, so a run nobody has answered keeps
	// the exact key it had before this component existed.
	if answer, ok := s.humanAuthorityAnswer(); ok {
		binding += "|" + answer.ID
	}
	return binding, true
}

// humanAuthorityAnswer is the id of the run's latest recorded human answer for
// the subject that governs NOW - the same applicability rule the human evidence
// bundle is rebuilt under: the contract revision it was given against and the
// candidate revision it was given against, neither of which is ever rebound.
//
// It is an event id, not a counter and not a clock, so it moves exactly once
// per answer and then settles: an approval re-plans authority.evaluate once, a
// rejection re-plans it once, and a pass that records nothing leaves the key
// where it is. Nothing the runtime does writes this event type - only the
// operator boundary does - so the key can never chase itself and reconciliation
// still terminates.
func (s *runState) humanAuthorityAnswer() (EngineeringEvent, bool) {
	var answer EngineeringEvent
	found := false
	for _, event := range s.events {
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		payload, err := decodePayload[HumanAuthorityRecordedPayload](event.Payload)
		if err != nil {
			// A payload that cannot be decoded is not an answer this planner
			// may act on. The evaluation itself refuses it, with the error.
			continue
		}
		if payload.Contract != s.projection.Contract || payload.Candidate.Revision != s.projection.CandidateRevision {
			continue
		}
		answer, found = event, true
	}
	return answer, found
}

// authorizedForPublication reports whether the CURRENT head carries a current,
// authorized publication decision. It is the gate for push and pull request
// operations, and it is deliberately re-derived from state rather than
// remembered.
func (s *runState) authorizedForPublication() bool {
	key, wanted := bindAuthorityEvaluate(s)
	if !wanted || !s.satisfied(OpAuthorityEvaluate, key) {
		return false
	}
	decision, ok := s.publicationDecision()
	return ok && decision.Status == domain.AuthorityAuthorized
}

func bindCandidatePush(s *runState) (string, bool) {
	if !s.authorizedForPublication() {
		return "", false
	}
	return s.projection.CandidateRevision, true
}

func bindPullRequestCreate(s *runState) (string, bool) {
	if s.published() {
		return "", false
	}
	if key, wanted := bindCandidatePush(s); !wanted || !s.satisfied(OpCandidatePush, key) {
		return "", false
	}
	return candidateBranch(s.run.ID) + "|" + s.rt.deps.Repository.DefaultBranch, true
}

func bindPullRequestUpdate(s *runState) (string, bool) {
	pr := s.projection.PullRequest
	if pr == nil {
		return "", false
	}
	key, wanted := bindCandidatePush(s)
	if !wanted || !s.satisfied(OpCandidatePush, key) {
		return "", false
	}
	if pr.HeadRevision == s.projection.CandidateRevision {
		return "", false
	}
	return strconv.Itoa(pr.Number) + "|" + s.projection.CandidateRevision, true
}

// ---------------------------------------------------------------------------
// Validator
// ---------------------------------------------------------------------------

// OperationRefusedError is the typed refusal the validator produces. It never
// becomes a retry: a refused operation is a state problem, not a flake.
type OperationRefusedError struct{ Kind, Reason string }

func (e *OperationRefusedError) Error() string {
	return "operation_refused: " + e.Kind + ": " + e.Reason
}

// validate is the second gate. The planner says what the state wants; the
// validator says whether the run is currently allowed to do it. Splitting them
// is what makes "waiting on authority never invokes the provider" a property
// of the runtime rather than of one call site.
func (s *runState) validate(desired desiredOperation, live Disposition) error {
	if terminalDisposition(s.snapshot.Disposition) {
		return &OperationRefusedError{desired.kind, "run is terminal"}
	}
	if terminalDisposition(live) {
		return &OperationRefusedError{desired.kind, "run reached a terminal condition"}
	}
	// Observation-class operations only READ external state, so they are the
	// only ones a waiting run may perform: it must still notice that its pull
	// request was merged, but it must not execute, mutate, verify, authorize
	// or publish anything while it waits.
	if live == Waiting && OperationCapacityClass(desired.kind) != CapacityObservation {
		return &OperationRefusedError{desired.kind, "a waiting run performs observation only"}
	}
	if s.projection.SourceIntentChanged && desired.kind == OpContractCompile {
		return &OperationRefusedError{desired.kind, "the pinned source moved; new intent is never silently compiled"}
	}
	if s.projection.ObservedExternalHead != "" && OperationCapacityClass(desired.kind) != CapacityObservation {
		return &OperationRefusedError{desired.kind, "an unexpected external head is never overwritten"}
	}
	if publicationKinds[desired.kind] && !s.authorizedForPublication() {
		return &OperationRefusedError{desired.kind, "publication requires a current authorized decision"}
	}
	// The binding must still be the one the current state wants. This is what
	// stops an operation that was leased against state that has since moved.
	spec, ok := specFor(desired.kind)
	if !ok {
		return &OperationRefusedError{desired.kind, "unknown operation kind"}
	}
	key, wanted := spec.bind(s)
	if !wanted {
		return &OperationRefusedError{desired.kind, "the state no longer wants this operation"}
	}
	if key != desired.key {
		return &OperationRefusedError{desired.kind, "operation is bound to superseded state"}
	}
	return nil
}

func specFor(kind string) (operationSpec, bool) {
	for _, spec := range operationSpecs {
		if spec.kind == kind {
			return spec, true
		}
	}
	return operationSpec{}, false
}

// ---------------------------------------------------------------------------
// The loop
// ---------------------------------------------------------------------------

// Reconcile drives one run until it reaches a stop condition: goal reached,
// waiting on real external input, failed, cancelled, merged, or out of run
// wall budget. It is not a daemon, it discovers no repositories, and it
// schedules no future work. It always persists before returning, so a later
// resume continues from durable state rather than from anything held here.
func (r *EngineeringRuntime) Reconcile(ctx context.Context, runID string) (Outcome, error) {
	// Forward progress is tracked by the existing deterministic fingerprint -
	// candidate tree, contract revision, failure signature, verifier, provider,
	// remediation identity - not by transcript text and not by a pass counter.
	// A run that keeps producing the same failure against the same tree is not
	// making progress, however many operations it completes.
	progress := &NoProgressTracker{Limit: 2}
	for pass := 0; pass < maxReconcilePasses; pass++ {
		if err := ctx.Err(); err != nil {
			return Outcome{}, err
		}
		state, err := r.load(runID)
		if err != nil {
			return Outcome{}, err
		}
		// A crash between the journal write and the scheduler write leaves the
		// store believing an operation is still active. The journal is the
		// authority; reconcile the store to it before planning.
		if err := r.reconcileStoreLag(state); err != nil {
			return Outcome{}, err
		}
		// A PAUSED run is returned unchanged (#86): nothing is planned,
		// settled or journalled, and the Outcome carries the run's own
		// disposition and reason - a pause is never one. This exit is only the
		// no-journal guarantee; the gate is AcquireOperation, which refuses a
		// paused run's lease whatever this pass read.
		if state.snapshot.Paused != nil {
			return Outcome{RunID: runID, Disposition: state.run.Disposition, Reason: state.run.Reason}, nil
		}
		// Another live driver is operating this run. The store would refuse
		// every acquisition anyway (a run holds at most one active operation),
		// so this pass journals NOTHING - no planned operation, no run.waiting
		// over a run someone else is driving. The reason is not journalled; it
		// tells the caller why this pass did nothing.
		if elsewhere, err := r.drivenElsewhere(runID); err != nil || elsewhere {
			return Outcome{RunID: runID, Disposition: state.run.Disposition, Reason: ReasonDrivenElsewhere}, err
		}
		if err := state.invariants(); err != nil {
			return r.settle(state, Failed, "invariant_violation")
		}
		if err := r.grantReviewContinuation(state); err != nil {
			return Outcome{}, err
		}
		live, reason := state.conditions()
		if terminalDisposition(live) || reason == ReasonReviewBudgetExhausted {
			return r.settle(state, live, reason)
		}
		desired, wanted := state.plan()
		// No progress means PRODUCING the same failure again, not looking at it
		// again. An observation pass changes nothing and must not spend the
		// budget bounded remediation needs: a current-head CI or review finding
		// is recorded BY an observation, which then makes the next two passes
		// re-observe at the new epoch, so counting them would exhaust the
		// budget before the producer could ever be planned.
		if !wanted || OperationCapacityClass(desired.kind) != CapacityObservation {
			if fingerprint, failing := state.failureFingerprint(); failing && !progress.Allow(fingerprint) {
				return r.settle(state, Failed, "no_progress")
			}
		}
		if !wanted {
			return r.settle(state, waitingOr(live, Waiting), waitingReason(reason, ReasonGoalStateReached))
		}
		if err := state.validate(desired, live); err != nil {
			return r.settle(state, waitingOr(live, Waiting), waitingReason(reason, "operation_refused"))
		}
		if live == Waiting {
			// Record the wait durably before observing, so a crash mid-pass
			// resumes into the same wait rather than into fresh work.
			if err := r.recordDisposition(state, Waiting, reason); err != nil {
				return Outcome{}, err
			}
		}
		progressed, outcome, err := r.runOperation(ctx, state, desired, live)
		if err != nil {
			return Outcome{}, err
		}
		if !progressed {
			return outcome, nil
		}
	}
	state, err := r.load(runID)
	if err != nil {
		return Outcome{}, err
	}
	return r.settle(state, Waiting, "reconcile_pass_limit")
}

// ReasonDrivenElsewhere is the Outcome reason of a Reconcile pass that found
// another live driver operating the run. It is never journalled.
const ReasonDrivenElsewhere = "driven_elsewhere"

// drivenElsewhere reports whether a DIFFERENT owner holds a lease on one of the
// run's operations that this driver may not take over: the owner is alive, or
// its lease has not expired. The driver's own leftover lease is not
// "elsewhere"; it is recovered by the ordinary takeover in Scheduler.Next.
func (r *EngineeringRuntime) drivenElsewhere(runID string) (bool, error) {
	s := r.scheduler.defaults()
	operations, err := s.Store.Operations(runID)
	if err != nil {
		return false, err
	}
	now := s.Clock.Now()
	for _, op := range operations {
		if op.Lease == nil || (op.State != Leased && op.State != Running) || op.Lease.Owner == s.Owner {
			continue
		}
		if !CanAcquire(op, now, s.Liveness.Alive(op.Lease.Owner)) {
			return true, nil
		}
	}
	return false, nil
}

func waitingOr(live, fallback Disposition) Disposition {
	if live == Waiting {
		return Waiting
	}
	return fallback
}

func waitingReason(conditionReason, fallback string) string {
	if conditionReason != "" {
		return conditionReason
	}
	return fallback
}

// reconcileStoreLag finishes scheduler rows the journal already recorded as
// terminal. Without it, an operation whose operation.after landed but whose
// scheduler write did not would stay leased forever and no later pass could
// acquire anything.
func (r *EngineeringRuntime) reconcileStoreLag(state *runState) error {
	active, err := r.deps.Store.ActiveOperations(state.run.ID)
	if err != nil {
		return err
	}
	for _, stored := range active {
		journalled, ok := state.snapshot.Operations[stored.ID]
		if !ok || (journalled.State != Succeeded && journalled.State != OperationFailed && journalled.State != OperationCancelled) {
			continue
		}
		// The attempt ended when its after record was journalled; the
		// controller's downtime since then is not execution. Only an after
		// record of the SAME attempt the store holds says when that ended.
		var ended time.Time
		if journalled.Attempt == stored.Attempt {
			for _, event := range state.events {
				if event.Type == EventOperationAfter && event.OperationID == stored.ID {
					ended = event.OccurredAt
				}
			}
		}
		if _, err := r.scheduler.finishAt(journalled.ID, journalled.State, journalled.RetryNotBefore, journalled.RetryDisposition, ended); err != nil {
			return err
		}
	}
	return nil
}

// runOperation acquires exactly one operation through the scheduler, records
// operation.before, performs the bounded side effect, records the effect's
// typed events, and records operation.after.
//
// The order is deliberate and is what the crash matrix depends on:
//
//	planned -> (crash here: no side effect happened)
//	before  -> (crash here: the handler's own probe reconciles the effect)
//	effect
//	events
//	after   -> (crash here: journal is authoritative, store is reconciled)
func (r *EngineeringRuntime) runOperation(ctx context.Context, state *runState, desired desiredOperation, live Disposition) (bool, Outcome, error) {
	planned, created, err := r.scheduler.Plan(RunOperation{
		RunID:            state.run.ID,
		Kind:             desired.kind,
		IdempotencyKey:   operationKey(desired.kind, desired.key),
		MaxAttempts:      desired.maxAttempts,
		InputStateSHA256: state.snapshot.StateSHA256,
		// The RUN's effective budget, not the raw operator default. A plan
		// stage that tightened its run's wall budget was previously ignored
		// here, so the operation was planned against a ceiling the run itself
		// had already narrowed.
		WallBudget: state.budgets().WallLimit,
	})
	if err != nil {
		return false, Outcome{}, err
	}
	if created {
		if err := r.append(state, EventOperationPlanned, planned.ID, planned, nil); err != nil {
			return false, Outcome{}, err
		}
	}
	if planned.Attempt >= planned.MaxAttempts {
		outcome, err := r.settle(state, Failed, desired.kind+attemptsExhaustedSuffix)
		return false, outcome, err
	}
	if r.deps.Clock.Now().Before(planned.RetryNotBefore) {
		// The timestamp only says "not yet"; the journalled record says why.
		outcome, err := r.settle(state, Waiting, waitReasonOf(state.snapshot.Operations[planned.ID]))
		return false, outcome, err
	}
	leased, err := r.scheduler.Next(state.run.ID)
	if err != nil {
		return false, Outcome{}, err
	}
	if leased == nil {
		// A pause that committed after this pass's check is why the store
		// refused (#86). The run is left as it is rather than settled on the
		// stale view, so nothing is journalled after run.paused.
		if paused, err := r.deps.Store.RunPaused(state.run.ID); err != nil || paused {
			return false, Outcome{RunID: state.run.ID, Disposition: state.run.Disposition, Reason: state.run.Reason}, err
		}
		outcome, err := r.settle(state, waitingOr(live, Waiting), "operation_unavailable")
		return false, outcome, err
	}
	if leased.ID != planned.ID {
		// The scheduler handed back a different eligible operation. It is only
		// legitimate if the current state still wants exactly that binding.
		if err := state.validate(desiredOperation{kind: leased.Kind, key: bindingOf(*leased)}, live); err != nil {
			if _, err := r.scheduler.Finish(leased.ID, OperationCancelled); err != nil {
				return false, Outcome{}, err
			}
			return true, Outcome{}, nil
		}
	}
	// Re-attempting an operation requires BOTH conditions: the class of the
	// failure its last attempt recorded must route to RouteRetry, AND the
	// attempt budget must have room. Remaining budget on its own is NOT a
	// reason to repeat a failure. That was the #29 defect: a deterministic
	// provider failure recorded as `unknown` ran three identical attempts in
	// under a hundred milliseconds, because `attempt < max_attempts` was the
	// only question anyone asked and RouteFailure(FailureUnknown) -> RouteStop
	// was never consulted.
	//
	// The rule is applied HERE, to the operation the scheduler actually handed
	// back, because this is the one place every attempt of every kind passes
	// through. It is deliberately not applied to the planned operation instead:
	// Scheduler.Next scans every operation of the run and `attempt <
	// max_attempts` is its whole eligibility rule, so a failed operation gets
	// re-leased on passes that planned something else entirely - which is
	// exactly how the failed run reached three attempts. It is equally
	// deliberately not an execution.invoke special case: a failure that routes
	// to neither a retry nor a wait stops the run whatever kind produced it.
	//
	// RouteFailure's remaining routes name a DIFFERENT operation - gofmt,
	// provider remediation, reassessment, restore. None of them means "run this
	// same operation again", and the planner reaches them by binding that other
	// operation, never by re-attempting this one.
	//
	// RouteWait is the one other route that leaves this operation attemptable.
	// A wait does not mean "run it again now" - the pass that produced it ends
	// immediately, below - but it does mean the condition is EXTERNAL and
	// recoverable, so a later pass, after an operator has corrected it, must be
	// able to find out. There is no free way to observe an execution provider's
	// account state, so asking it again is the only honest re-derivation; the
	// attempt that only re-observes the wait is given back by RestoreAttempt,
	// which is what keeps repeated passes from spending the run's budget.
	// A REVIEWER PROTOCOL FAILURE gets exactly ONE corrective re-invocation
	// (#374), never a budget's worth. RouteFailure(FailureReviewerProtocolIncomplete)
	// is RouteRetry - a malformed or missing result is a correctable mistake,
	// not a verdict on the candidate - so without this check the ordinary rule
	// below would keep re-dispatching the SAME reviewer for as many attempts as
	// the operator's execution-attempt budget happens to allow, which answers a
	// question #374 does not ask: how many tries does this invocation get, not
	// how many corrections does a reviewer that keeps failing the protocol
	// deserve. One correction is the whole grant. A reviewer handed its own
	// exact refusal reason (lastReviewRefusal, consulted in invokeExecution)
	// and still failing to cross the protocol a second time in a row has shown
	// the correction was not taken up, and no further authority accrues to
	// this operation from retrying it again - whatever budget remains.
	if state.reviewerProtocolCorrectionExhausted(leased.ID) {
		if _, err := r.scheduler.Finish(leased.ID, OperationFailed); err != nil {
			return false, Outcome{}, err
		}
		outcome, err := r.settle(state, Failed, leased.Kind+"_reviewer_protocol_correction_exhausted")
		return false, outcome, err
	}
	if class, recorded := state.lastFailure(leased.ID); recorded && !reattemptable(RouteFailure(class)) {
		// The journal already records this operation as failed; releasing the
		// lease keeps the scheduler row saying the same thing.
		if _, err := r.scheduler.Finish(leased.ID, OperationFailed); err != nil {
			return false, Outcome{}, err
		}
		outcome, err := r.settle(state, Failed, leased.Kind+"_failure_not_retryable")
		return false, outcome, err
	}
	started, err := r.scheduler.StartWithin(leased.ID, state.attemptLimit(r.deps.Clock.Now()))
	if err != nil {
		return false, Outcome{}, err
	}
	if err := r.append(state, EventOperationBefore, started.ID, started, nil); err != nil {
		return false, Outcome{}, err
	}
	produced := r.handle(ctx, state, started)
	// Only invokeExecution ever sets interrupted: a running provider is the one
	// started attempt a stop reaches (#213); every other kind is unchanged.
	interrupted := produced.interrupted
	for _, entry := range produced.events {
		if err := r.append(state, entry.Type, started.ID, entry.Payload, entry.Artifacts); err != nil {
			return false, Outcome{}, err
		}
	}
	// An interrupted execution ends OperationCancelled - the state the stop
	// itself writes to the scheduler row - so journal and store agree, and the
	// handler's run_cancelled diagnostic is the terminal record.
	finished := started
	finished.RetryNotBefore, finished.RetryDisposition = time.Time{}, ""
	finished.State = produced.state
	if interrupted {
		finished.State = OperationCancelled
	}
	finished.Lease = nil
	if produced.result != nil {
		raw, err := marshalPayloadJSON(produced.result)
		if err != nil {
			return false, Outcome{}, err
		}
		finished.Result = raw
	}
	// The disposition is recorded from the class; the wait and its timing only
	// while a successor attempt exists, so the last attempt stops truthfully.
	if finished.State == OperationFailed {
		finished.RetryDisposition = retryDispositionFor(failureClassOf(finished.Result))
	}
	disposition, awaiting := awaitsRetry(finished)
	if awaiting && disposition.Delay != nil {
		finished.RetryNotBefore = r.deps.Clock.Now().Add(disposition.Delay(started.Attempt))
	}
	// The journal is written first and is the authority for reconciliation.
	if err := r.append(state, EventOperationAfter, started.ID, finished, nil); err != nil {
		return false, Outcome{}, err
	}
	if _, err := r.scheduler.finishAt(started.ID, finished.State, finished.RetryNotBefore, finished.RetryDisposition, time.Time{}); err != nil {
		// The stop may already have finished the row. That is accepted only for
		// an interrupted execution and only when the row durably reads
		// OperationCancelled - the one state CancelRun writes. Every other
		// Finish failure is reported as it always was.
		if !interrupted {
			return false, Outcome{}, err
		}
		stored, _, found, readErr := r.deps.Store.Operation(started.ID)
		if readErr != nil || !found || stored.State != OperationCancelled {
			return false, Outcome{}, err
		}
	}
	// The next pass reloads, finds run.cancelled in replay and settles the run
	// cancelled through the ordinary terminal path.
	if interrupted {
		return true, Outcome{}, nil
	}
	// A wait-routed failure settles the RUN, not just the operation: the
	// external world refused, nothing here can change that, and the pass must
	// end rather than immediately try again. The run stays waiting - it is not
	// failed, it has lost no binding, and it owns everything it owned before -
	// and the attempt is given back, because observing an external refusal is
	// not work the run's budget should pay for.
	// A checkpoint ends the PASS. The work is preserved and committed, #8 has
	// reassessed it, and the continuation is planned - but running it in the
	// same call would let one reconciliation spend the entire execution budget
	// on a provider that keeps stalling, and would leave no durable point at
	// which an operator, a restart, or a watch tick sees the preserved work.
	// The next reconciliation continues from the exact checkpoint.
	if journalled(produced.events, EventCandidateCheckpointed) {
		outcome, err := r.settle(state, Waiting, "execution_checkpointed")
		return false, outcome, err
	}
	// AN UNRESOLVED CONTINUATION ALSO ENDS THE PASS (#379), for the same
	// reason a checkpoint does: the checkpoint it inherited is unresolved work,
	// not a flake worth hammering same-call like an ordinary retryable
	// failure. feedback_unresolved and reviewer-protocol failures are
	// deliberately NOT given this treatment - they retry within the same pass,
	// spending their whole attempt budget at once, because that failure is
	// about a candidate already known to exist and already being iterated on.
	// A checkpoint continuation is different: it is the one invocation that
	// may have just spent real wall-clock time deferring to work it never
	// finished, and retrying it immediately, in the same call, is exactly how
	// the fourth dogfood's checkpoint got hammered through its whole budget
	// and promoted on the attempt that happened to return clean. Ending the
	// pass here means each continuation attempt is observed at a durable
	// point - the checkpoint stays exactly where it was, the attempt it spent
	// is not given back, and a later reconciliation (not this same call)
	// decides whether to spend the next one.
	if failureClassOf(finished.Result) == FailureCheckpointContinuationUnresolved {
		outcome, err := r.settle(state, Waiting, "execution_continuation_unresolved")
		return false, outcome, err
	}
	if awaiting {
		if !disposition.SpendsAttempt {
			if _, err := r.scheduler.RestoreAttempt(started.ID, !providerExecuted(finished.Result)); err != nil {
				return false, Outcome{}, err
			}
		}
		outcome, err := r.settle(state, Waiting, disposition.Reason)
		return false, outcome, err
	}
	if class, waiting := waitRoutedFailure(finished.Result); waiting {
		// The ATTEMPT is always given back - observing an external refusal is
		// not work. The execution TIME is given back only when no execution
		// happened: a provider that reasoned for twenty minutes and only then
		// met a rate limit did that work, and refunding it would let a run
		// exceed a budget the operator set by repeatedly hitting the same wall.
		if _, err := r.scheduler.RestoreAttempt(started.ID, !providerExecuted(finished.Result)); err != nil {
			return false, Outcome{}, err
		}
		outcome, err := r.settle(state, Waiting, waitReason(class))
		return false, outcome, err
	}
	return true, Outcome{}, nil
}

// failureClassOf reads the same one shared mutationResult field lastFailure
// does, without requiring the caller to care whether the class routes
// anywhere in particular - unlike waitRoutedFailure, which only answers for
// RouteWait.
func failureClassOf(raw json.RawMessage) FailureClass {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil {
		return ""
	}
	return result.FailureClass
}

// journalled reports whether an effect appended one particular event type.
func journalled(entries []journalEntry, eventType string) bool {
	for _, entry := range entries {
		if entry.Type == eventType {
			return true
		}
	}
	return false
}

// reattemptable reports whether a route leaves the SAME operation eligible to
// run again. Retry means run it again now; wait means run it again once the
// external condition it named has been corrected. Every other route names a
// different operation, or none.
func reattemptable(route FailureRoute) bool {
	return route == RouteRetry || route == RouteWait
}

// waitRoutedFailure reads the class an operation result recorded and reports it
// only when it routes to a wait. It reads the same one shared field lastFailure
// does, so this is not an execution.invoke special case: any handler that
// records a wait-routed class settles the run into that wait.
// providerExecuted reports whether the attempt recorded in this result actually
// reached a worker. An unreadable or absent result is treated as HAVING
// executed: refunding budget is the generous direction, and guessing generously
// about an unknown is how a bounded budget stops being one.
func providerExecuted(raw json.RawMessage) bool {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil {
		return true
	}
	return result.ProviderExecuted
}

func waitRoutedFailure(raw json.RawMessage) (FailureClass, bool) {
	var result mutationResult
	if len(raw) == 0 || decodeJSON(raw, &result) != nil || result.FailureClass == "" {
		return "", false
	}
	if RouteFailure(result.FailureClass) != RouteWait {
		return "", false
	}
	return result.FailureClass, true
}

// waitReasons names the durable run reason each wait-routed class settles into.
// It is a stated table and not a formatted class name so that the identifier an
// operator reads, and a later reader replays, cannot drift when a class is
// renamed.
var waitReasons = map[FailureClass]string{
	FailureProviderAccountUnavailable: "execution_provider_account_unavailable",
	FailureAssurancePrerequisite:      "assurance_dependency_unavailable",
	// The two capacity waits are reported separately because the operator
	// action differs: a quota comes back on the provider's own schedule, while
	// repeated rate limiting means the configured concurrency is above what
	// that account tolerates.
	FailureProviderQuota:       "execution_provider_quota",
	FailureProviderRateLimited: "execution_provider_rate_limited",
	// The host cannot REACH the provider. It is reported separately from the
	// two capacity waits and from the account prerequisite because the
	// operator action is different again: nothing is spent, nothing is
	// revoked, and what has to change is connectivity.
	FailureProviderUnavailable:   "execution_provider_unavailable",
	FailureStateStorageExhausted: "state_storage_exhausted",
	// The controller cannot install its own candidate-Git boundary. An
	// operator repairs the installation; nothing about the work is wrong.
	FailureCandidateGuardUnavailable: "candidate_guard_unavailable",
	FailureControllerShutdown:        "controller_shutdown",
}

func waitReason(class FailureClass) string {
	if reason, ok := waitReasons[class]; ok {
		return reason
	}
	return string(class) + "_wait"
}

// append records one typed event through the existing journal. Every Phase 8
// event goes through here, so the payload registry and the 8 KiB canonical
// ceiling apply uniformly.
func (r *EngineeringRuntime) append(state *runState, eventType, operationID string, payload any, artifacts []Artifact) error {
	raw, err := marshalPayloadJSON(payload)
	if err != nil {
		return err
	}
	now := r.deps.Clock.Now()
	event := EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            newEventID(state.run.ID),
		RunID:         state.run.ID,
		Type:          eventType,
		OccurredAt:    now,
		OperationID:   operationID,
		Payload:       raw,
		Artifacts:     artifacts,
	}
	appended, err := r.deps.Store.AppendEvent(event)
	if err != nil {
		return err
	}
	state.events = append(state.events, appended)
	return nil
}

// newEventID mints a random per-event identity. Uniqueness must not depend on
// the clock or on an in-memory sequence: Phase 10 has a watch process and an
// operator CLI appending to one run at the same instant, and recording human
// authority is not an engineering side effect, so it must not have to take the
// run's operation lease to get a distinct id. crypto/rand.Text is 130 bits of
// base32, so two independent writers colliding is not a case worth designing
// for, and the journal's UNIQUE constraints still refuse it if it ever happens.
//
// Replay stays deterministic despite the non-deterministic id: Reduce is a pure
// function of the run and the events it is handed, and an id is recorded once
// and then only ever read back. Replay never re-mints one.
func newEventID(runID string) string { return runID + "-" + rand.Text() }

// recordDisposition persists the run's disposition. The event is appended only
// when the disposition or its reason actually changes, so a repeated wait does
// not grow the journal; the run document is always refreshed, so a later
// resume sees the current identity bindings without replaying.
func (r *EngineeringRuntime) recordDisposition(state *runState, disposition Disposition, reason string) error {
	// A run the operator has already STOPPED is never settled onto anything
	// else. Every disposition this pass could record was derived from a
	// snapshot read at the start of the pass, and CancelRun writes from another
	// goroutine entirely - the control endpoint's stop-all runs concurrently
	// with the tick that is driving this run. Recording the stale answer
	// appended run.waiting after run.cancelled and wrote the run document back
	// to waiting, which returned the run to the supervisor's active set and
	// handed the work the operator stopped straight back to the next tick.
	//
	// The re-read is not the guarantee - PutRun's own condition is, and it
	// refuses to replace a cancelled row whatever this pass decided. What the
	// re-read buys is the COMMON case: a stop that has already landed stops
	// this pass from appending a junk run.waiting or run.failed to the hash
	// chain at all, which the write below cannot do anything about because the
	// append comes first. A stop that lands between this read and that write
	// is caught by the condition, and adopted straight afterwards.
	if disposition != Cancelled {
		live, found, err := r.deps.Store.Run(state.run.ID)
		if err != nil {
			return err
		}
		if found && live.Disposition == Cancelled {
			state.run = live
			state.snapshot.Disposition, state.snapshot.Reason = live.Disposition, live.Reason
			return nil
		}
	}
	if state.snapshot.Disposition != disposition || state.snapshot.Reason != reason {
		eventType, ok := dispositionEvents[disposition]
		if !ok {
			return fmt.Errorf("no journal event for disposition %q", disposition)
		}
		// A budget boundary names what the run is holding (#203) in the SAME
		// event that ends it, so the terminal fact and the held material can
		// never be journalled apart, and replay reads the record back rather
		// than re-deriving it.
		// A record already journalled is reused, never re-derived: a later
		// re-settle carries the SAME identity rather than an opinion of it
		// from moved state. Only run.failed may carry one.
		var held *HeldMaterial
		if disposition == Failed {
			held = state.snapshot.HeldMaterial
			if held == nil && BudgetBoundary(disposition, reason) {
				held = state.heldMaterial(reason)
			}
		}
		payload := dispositionRecord{Reason: reason, HeldMaterial: held}
		if err := r.append(state, eventType, "", payload, nil); err != nil {
			return err
		}
		state.snapshot.Disposition, state.snapshot.Reason = disposition, reason
		if held != nil {
			state.snapshot.HeldMaterial = held
		}
	}
	run := state.run
	run.Phase = state.phase()
	run.Disposition = disposition
	run.Reason = reason
	run.Base = Ref{ID: r.deps.Repository.DefaultBranch, Revision: state.baseRevision()}
	run.Candidate = Candidate{Branch: candidateBranch(run.ID), Revision: state.projection.CandidateRevision, Tree: state.projection.CandidateTree}
	run.Contract = state.projection.Contract
	run.UpdatedAt = r.deps.Clock.Now()
	state.run = run
	if err := r.deps.Store.PutRun(run); err != nil {
		return err
	}
	// ADOPT WHAT THE ROW ACTUALLY SAYS. The write above is conditional and
	// refuses silently, so a stop that won the race leaves this pass holding a
	// disposition the database never accepted. Nothing durable is wrong at that
	// point - the row and replay both say cancelled, and the acquisition
	// statement reads the row - but the pass would go on to REPORT `waiting`
	// for a run the operator stopped, which is the one thing its caller acts
	// on. One read is cheaper than an operator who believes their stop is still
	// pending.
	if disposition != Cancelled {
		live, found, err := r.deps.Store.Run(run.ID)
		if err != nil {
			return err
		}
		if found && live.Disposition == Cancelled {
			state.run = live
			state.snapshot.Disposition, state.snapshot.Reason = live.Disposition, live.Reason
		}
	}
	return nil
}

var dispositionEvents = map[Disposition]string{
	Waiting:   EventRunWaiting,
	Completed: EventRunCompleted,
	Failed:    EventRunFailed,
	Cancelled: EventRunCancelled,
}

func (r *EngineeringRuntime) settle(state *runState, disposition Disposition, reason string) (Outcome, error) {
	if err := r.recordDisposition(state, disposition, reason); err != nil {
		return Outcome{}, err
	}
	// Reported from what was RECORDED, not from what was asked for: a pass
	// settling on stale state over a stopped run records the stop instead, and
	// the operator's caller has to be told the run is cancelled.
	return Outcome{RunID: state.run.ID, Disposition: state.run.Disposition, Reason: state.run.Reason}, nil
}
