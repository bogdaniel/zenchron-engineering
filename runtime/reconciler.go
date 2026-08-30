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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

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
	OpAuthorityEvaluate = "authority.evaluate"
	OpBaseIntegrate     = "base.integrate"
	OpCandidatePush     = "candidate.push"
	OpPullRequestCreate = "pull_request.create"
	OpPullRequestUpdate = "pull_request.update"
	OpGitHubObserve     = "github.observe"
)

// observationKinds are the operations that only READ external state. They are
// the only operations a waiting run may perform: a waiting run must still be
// able to notice that its pull request was merged, but it must not execute,
// mutate, verify, authorize, or publish anything while it waits.
var observationKinds = map[string]bool{OpSourceObserve: true, OpGitHubObserve: true}

// publicationKinds are the operations that change protected remote state.
// Every one of them is gated on a current, authorized #7 decision.
var publicationKinds = map[string]bool{OpCandidatePush: true, OpPullRequestCreate: true, OpPullRequestUpdate: true}

// PublicationActionType is the protected action the runtime asks #7 about
// before it pushes or opens a pull request. Push is not a separate authority:
// pushing the run-owned branch exists only to publish, so one decision gates
// the whole publication, evaluated against the exact candidate commit.
const PublicationActionType = "git.pull_request.create"

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
	rt                *EngineeringRuntime
	run               EngineeringRun
	snapshot          RunSnapshot
	events            []EngineeringEvent
	projection        RunProjection
	sources           []sourceRecord
	source            *sourceRecord
	controllerChanged bool
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
		controllerChanged: run.ControllerSHA256 != r.controller,
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

// pinnedBase is the base revision the run was compiled and cloned against. It
// is the FIRST observation's base: later base movement is handled by
// base.integrate and reassessment, never by silently recompiling the contract
// against a base the candidate was never built on.
func (s *runState) pinnedBase() string {
	if len(s.sources) == 0 {
		return ""
	}
	return s.sources[0].BaseRevision
}

// baseRevision is the base the candidate currently sits on.
func (s *runState) baseRevision() string {
	if s.projection.BaseRevision != "" {
		return s.projection.BaseRevision
	}
	return s.pinnedBase()
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
	if limit := s.rt.deps.Budgets.WallLimit; limit > 0 && now.Sub(s.run.CreatedAt) > limit {
		return Failed, "run_wall_budget_exhausted"
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
	if r := s.projection.Reassessment; r != nil && r.RequestedPrivilegeCount > 0 {
		return Waiting, "requested_privilege_expansion"
	}
	if decision, ok := s.unansweredPublicationDecision(); ok {
		switch decision.Status {
		case domain.AuthorityAwaitingAuthority:
			return Waiting, "awaiting_authority"
		case domain.AuthorityBlocked:
			return Waiting, "authority_blocked"
		}
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
		return desiredOperation{kind: spec.kind, key: key, maxAttempts: s.rt.attemptsFor(spec.kind)}, true
	}
	return desiredOperation{}, false
}

func (r *EngineeringRuntime) attemptsFor(kind string) int {
	switch kind {
	case OpExecutionInvoke:
		return r.deps.Budgets.MaxExecutionAttempts
	case OpRemediationGofmt:
		return r.deps.Budgets.MaxRemediationAttempts
	case OpAssuranceGo:
		return r.deps.Budgets.MaxAssuranceAttempts
	default:
		return 3
	}
}

func bindSourceObserve(s *runState) (string, bool) { return s.epochKey(), true }

func bindGitHubObserve(s *runState) (string, bool) {
	if !s.published() {
		return "", false
	}
	return s.epochKey(), true
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
	// Bounded remediation: only a CURRENT-head failure that routes to a
	// producer. An authority wait never reaches this branch, because
	// RouteFailure never routes an authority wait to a provider.
	class, ok := s.currentHeadFailure()
	if !ok || RouteFailure(class) != RouteProviderRemediation {
		return "", false
	}
	return "remediation|" + s.projection.CandidateRevision + "|" + string(class), true
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

func bindAssuranceGo(s *runState) (string, bool) {
	if s.projection.CandidateRevision == "" || s.projection.Contract == (Ref{}) {
		return "", false
	}
	return s.projection.CandidateRevision + "|" + s.projection.CandidateTree + "|" + s.contractRevision(), true
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
	if live == Waiting && !observationKinds[desired.kind] {
		return &OperationRefusedError{desired.kind, "a waiting run performs observation only"}
	}
	if s.projection.SourceIntentChanged && desired.kind == OpContractCompile {
		return &OperationRefusedError{desired.kind, "the pinned source moved; new intent is never silently compiled"}
	}
	if s.projection.ObservedExternalHead != "" && !observationKinds[desired.kind] {
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
		if err := state.invariants(); err != nil {
			return r.settle(state, Failed, "invariant_violation")
		}
		live, reason := state.conditions()
		if terminalDisposition(live) {
			return r.settle(state, live, reason)
		}
		desired, wanted := state.plan()
		// No progress means PRODUCING the same failure again, not looking at it
		// again. An observation pass changes nothing and must not spend the
		// budget bounded remediation needs: a current-head CI or review finding
		// is recorded BY an observation, which then makes the next two passes
		// re-observe at the new epoch, so counting them would exhaust the
		// budget before the producer could ever be planned.
		if !wanted || !observationKinds[desired.kind] {
			if fingerprint, failing := state.failureFingerprint(); failing && !progress.Allow(fingerprint) {
				return r.settle(state, Failed, "no_progress")
			}
		}
		if !wanted {
			return r.settle(state, waitingOr(live, Waiting), waitingReason(reason, "goal_state_reached"))
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
	for _, journalled := range state.snapshot.Operations {
		if journalled.State != Succeeded && journalled.State != OperationFailed && journalled.State != OperationCancelled {
			continue
		}
		stored, _, ok, err := r.deps.Store.Operation(journalled.ID)
		if err != nil {
			return err
		}
		if !ok || stored.State == journalled.State {
			continue
		}
		if stored.State != Leased && stored.State != Running {
			continue
		}
		if _, err := r.scheduler.Finish(journalled.ID, journalled.State); err != nil {
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
		WallBudget:       r.deps.Budgets.WallLimit,
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
		outcome, err := r.settle(state, Failed, desired.kind+"_attempts_exhausted")
		return false, outcome, err
	}
	leased, err := r.scheduler.Next(state.run.ID)
	if err != nil {
		return false, Outcome{}, err
	}
	if leased == nil {
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
	started, err := r.scheduler.Start(leased.ID)
	if err != nil {
		return false, Outcome{}, err
	}
	if err := r.append(state, EventOperationBefore, started.ID, started, nil); err != nil {
		return false, Outcome{}, err
	}
	produced := r.handle(ctx, state, started)
	for _, entry := range produced.events {
		if err := r.append(state, entry.Type, started.ID, entry.Payload, entry.Artifacts); err != nil {
			return false, Outcome{}, err
		}
	}
	finished := started
	finished.State = produced.state
	finished.Lease = nil
	if produced.result != nil {
		raw, err := marshalPayloadJSON(produced.result)
		if err != nil {
			return false, Outcome{}, err
		}
		finished.Result = raw
	}
	// The journal is written first and is the authority for reconciliation.
	if err := r.append(state, EventOperationAfter, started.ID, finished, nil); err != nil {
		return false, Outcome{}, err
	}
	if _, err := r.scheduler.Finish(started.ID, produced.state); err != nil {
		return false, Outcome{}, err
	}
	return true, Outcome{}, nil
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
	if state.snapshot.Disposition != disposition || state.snapshot.Reason != reason {
		eventType, ok := dispositionEvents[disposition]
		if !ok {
			return fmt.Errorf("no journal event for disposition %q", disposition)
		}
		payload := struct {
			Reason string `json:"reason,omitempty"`
		}{reason}
		if err := r.append(state, eventType, "", payload, nil); err != nil {
			return err
		}
		state.snapshot.Disposition, state.snapshot.Reason = disposition, reason
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
	return r.deps.Store.PutRun(run)
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
	return Outcome{RunID: state.run.ID, Disposition: disposition, Reason: reason}, nil
}
