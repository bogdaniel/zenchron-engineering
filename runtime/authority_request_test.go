package runtime

// Phase 10 §1/§2/§5/§6/§7 tests. Everything here runs offline on the Phase 8
// fixture: a temporary Git repository stands in for the remote, the forge, the
// execution provider and the verifier are fakes, the clock is injected, and no
// provider or network call is made by anything under test. The suite passes
// under `docker run --network none`.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

const (
	humanClaim   = "human-approval"
	auditClaim   = "external-audit"
	verifyClaim  = "verification"
	auditClass   = domain.EvidenceClass("external_audit")
	testOperator = "operator-1"
)

// governedBy rebuilds the fixture policy with an explicit claim set, authority
// condition and permission target. Every rule gets the same effect, which is
// what the compiler requires of rules that can match together.
func governedBy(policy domain.EngineeringPolicy, claims map[string]domain.RequiredClaim, required []string, conditionTarget, permissionTarget string) domain.EngineeringPolicy {
	rules := map[string]domain.PolicyRule{}
	for id, rule := range policy.Rules {
		claimSet := claims
		permissions := []domain.Action{{Type: PublicationActionType, Target: permissionTarget}}
		conditions := []domain.AuthorityCondition{{
			Action:         domain.Action{Type: PublicationActionType, Target: conditionTarget},
			RequiredClaims: required,
		}}
		effect := rule.Effect
		effect.RequiredClaims = &claimSet
		effect.Permissions = &permissions
		effect.AuthorityConditions = &conditions
		rules[id] = domain.PolicyRule{When: rule.When, Effect: effect}
	}
	policy.Rules = rules
	return policy
}

// humanApprovalClaims is the ordinary Phase 10 governance: the runtime's own
// verifier satisfies one claim, and a human has to answer the other.
func humanApprovalClaims() map[string]domain.RequiredClaim {
	return map[string]domain.RequiredClaim{
		verifyClaim: {EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true},
		humanClaim:  {EvidenceClass: HumanApprovalEvidenceClass, IndependentFromChangeProducer: true},
	}
}

// newAuthorityFixture is the Phase 8 fixture re-governed so that publication
// requires a human approval as well as a passing verifier.
func newAuthorityFixture(t *testing.T) *phase8Fixture {
	t.Helper()
	return newAuthorityFixtureWith(t, humanApprovalClaims(), []string{humanClaim, verifyClaim}, "")
}

// newAuthorityFixtureWith allows a test to change the claim set or to deny the
// permission by pointing it at a branch the run does not publish to.
func newAuthorityFixtureWith(t *testing.T, claims map[string]domain.RequiredClaim, required []string, permissionTarget string) *phase8Fixture {
	t.Helper()
	fixture := newPhase8Fixture(t)
	if permissionTarget == "" {
		permissionTarget = fixture.branch
	}
	fixture.deps.Policy = governedBy(fixture.deps.Policy, claims, required, fixture.branch, permissionTarget)
	fixture.runtime = fixture.newRuntime(fixture.deps)
	return fixture
}

// awaitAuthority drives the run to its human-authority wait and returns the run
// and the request the runtime's own authority evaluation produced.
func awaitAuthority(t *testing.T, fixture *phase8Fixture) (string, AuthorityRequest) {
	t.Helper()
	runID := fixture.start()
	fixture.reconcile(runID)
	request := currentRequest(t, fixture, runID)
	return runID, request
}

func currentRequest(t *testing.T, fixture *phase8Fixture, runID string) AuthorityRequest {
	t.Helper()
	request, err := fixture.runtime.PendingAuthorityRequest(runID)
	if err != nil {
		t.Fatalf("PendingAuthorityRequest: %v", err)
	}
	if request == nil {
		t.Fatal("the run reports no human authority request")
	}
	return *request
}

func operator() RecordedOperator {
	return RecordedOperator{
		ID: testOperator, AccountName: "local-account", Host: "workstation",
		Provenance: ProvenanceLocalUnverified,
	}
}

func approval(runID string, request AuthorityRequest) AuthorizeInput {
	return AuthorizeInput{
		RunID: runID, RequestID: request.ID, Digest: request.Digest,
		Decision: "approve", Operator: operator(),
	}
}

func authorize(t *testing.T, fixture *phase8Fixture, in AuthorizeInput) AuthorizeResult {
	t.Helper()
	result, err := fixture.runtime.Authorize(context.Background(), in)
	if err != nil {
		t.Fatalf("Authorize: %v", err)
	}
	return result
}

// refusalCode returns the typed refusal code, failing the test when the error
// is not a refusal at all. Tests branch on the code, never on message text.
func refusalCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal, got none")
	}
	var refused *AuthorityRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("expected an *AuthorityRefusedError, got %T: %v", err, err)
	}
	return refused.Code
}

// journalEvent appends one event outside any operation, which is exactly how a
// crash-recovered or concurrently driven run's journal can look.
func journalEvent(t *testing.T, store *SQLiteOperationStore, clock *steppingClock, runID, eventType string, payload any) {
	t.Helper()
	raw, err := marshalPayloadJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: newEventID(runID), RunID: runID,
		Type: eventType, OccurredAt: clock.Now(), Payload: raw,
	}); err != nil {
		t.Fatalf("append %s: %v", eventType, err)
	}
}

// advanceCandidate makes a REAL new commit in the run's own candidate
// workspace. Fabricating a SHA would not exercise staleness; it would exercise
// a broken workspace, which is a different refusal.
func advanceCandidate(t *testing.T, fixture *phase8Fixture, runID, file string) (string, string) {
	t.Helper()
	dir := candidateDir(fixture.stateDir, runID)
	if err := os.WriteFile(filepath.Join(dir, file), []byte("advanced\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The runtime-owned workspace already carries its own commit identity; the
	// trusted Git runner refuses caller-supplied global flags, which is the
	// point of it.
	for _, args := range [][]string{{"add", "-A", "--"}, {"commit", "-m", "advance"}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	commit, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(commit), strings.TrimSpace(tree)
}

func humanAuthorityEvents(t *testing.T, fixture *phase8Fixture, runID string) []EngineeringEvent {
	t.Helper()
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	var out []EngineeringEvent
	for _, event := range events {
		if event.Type == EventHumanAuthorityRecorded {
			out = append(out, event)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// §1 - the request is a projection, and its id digests the exact bindings
// ---------------------------------------------------------------------------

func TestAuthorityRequestProjectsTheRunsOwnAuthorityEvaluation(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	state := fixture.state(runID)

	if request.Status != domain.AuthorityAwaitingAuthority {
		t.Fatalf("status = %q, want awaiting_authority", request.Status)
	}
	if len(request.Requires) != 1 || request.Requires[0] != humanClaim {
		t.Fatalf("requires = %v, want exactly the human approval claim", request.Requires)
	}
	if request.Action != (domain.Action{Type: PublicationActionType, Target: fixture.branch}) {
		t.Fatalf("action = %+v", request.Action)
	}
	if request.RunID != runID || request.Repository != state.run.Repository {
		t.Fatalf("request does not name the run: %+v", request)
	}
	if request.Candidate.Revision != state.projection.CandidateRevision || request.Candidate.Tree != state.projection.CandidateTree {
		t.Fatalf("candidate binding = %+v, want %s/%s", request.Candidate, state.projection.CandidateRevision, state.projection.CandidateTree)
	}
	if request.Contract != state.projection.Contract {
		t.Fatalf("contract binding = %+v, want %+v", request.Contract, state.projection.Contract)
	}
	if request.Controller != (Ref{ID: fixture.deps.ControllerID, Revision: state.run.ControllerSHA256}) {
		t.Fatalf("controller binding = %+v", request.Controller)
	}
	if request.Base.Revision != state.baseRevision() || request.Base.ID != fixture.branch {
		t.Fatalf("base binding = %+v", request.Base)
	}
	if request.Source.Revision == "" || request.Source.ID == "" {
		t.Fatalf("source binding = %+v", request.Source)
	}
	if request.Decision.ID == "" || request.Decision.Revision == "" {
		t.Fatalf("decision binding = %+v", request.Decision)
	}
	if request.StateSHA256 != state.snapshot.StateSHA256 {
		t.Fatalf("state digest = %q, want %q", request.StateSHA256, state.snapshot.StateSHA256)
	}
	if request.ObservedAt.IsZero() || request.Disposition != Waiting || request.Reason != "awaiting_authority" {
		t.Fatalf("observation members = %v/%s/%s", request.ObservedAt, request.Disposition, request.Reason)
	}
	if len(request.Digest) != 64 || request.ID != "authreq-"+request.Digest[:32] {
		t.Fatalf("id %q is not derived from digest %q", request.ID, request.Digest)
	}
	// Reading twice is the same request: the projection is a pure function of
	// the journal, and the observation members are excluded from the identity.
	again := currentRequest(t, fixture, runID)
	if again.ID != request.ID || again.Digest != request.Digest {
		t.Fatalf("request identity is not stable across reads: %q then %q", request.ID, again.ID)
	}
	if !again.ObservedAt.After(request.ObservedAt) {
		t.Fatal("the observed time should advance between reads")
	}
}

// TestAuthorityRequestIdDigestsEveryBinding is the mutation check for the id:
// changing any one binding member has to change the id. A digest that ignored
// the candidate tree - or the contract, controller, source, base, or the
// outstanding requirement - would fail here before it could fail in production.
func TestAuthorityRequestIdDigestsEveryBinding(t *testing.T) {
	fixture := newAuthorityFixture(t)
	_, request := awaitAuthority(t, fixture)

	for name, mutate := range map[string]func(AuthorityRequest) AuthorityRequest{
		"run":            func(a AuthorityRequest) AuthorityRequest { a.RunID += "-x"; return a },
		"repository":     func(a AuthorityRequest) AuthorityRequest { a.Repository += "-x"; return a },
		"action":         func(a AuthorityRequest) AuthorityRequest { a.Action.Target += "-x"; return a },
		"controller":     func(a AuthorityRequest) AuthorityRequest { a.Controller.Revision += "x"; return a },
		"source":         func(a AuthorityRequest) AuthorityRequest { a.Source.Revision += "x"; return a },
		"base":           func(a AuthorityRequest) AuthorityRequest { a.Base.Revision += "x"; return a },
		"candidate head": func(a AuthorityRequest) AuthorityRequest { a.Candidate.Revision += "x"; return a },
		"candidate tree": func(a AuthorityRequest) AuthorityRequest { a.Candidate.Tree += "x"; return a },
		"contract":       func(a AuthorityRequest) AuthorityRequest { a.Contract.Revision += "x"; return a },
		"decision":       func(a AuthorityRequest) AuthorityRequest { a.Decision.Revision += "x"; return a },
		"status":         func(a AuthorityRequest) AuthorityRequest { a.Status = domain.AuthorityIncomplete; return a },
		"requirement":    func(a AuthorityRequest) AuthorityRequest { a.Requires = []string{"other"}; return a },
		"evidence":       func(a AuthorityRequest) AuthorityRequest { a.Evidence = append(a.Evidence, Ref{"e", "1"}); return a },
	} {
		moved, err := mutate(request).identify()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if moved.ID == request.ID {
			t.Fatalf("changing the %s did not change the request id", name)
		}
	}

	// The observation members are deliberately NOT in the identity: an
	// unrelated journal append moves the run's state digest, and expiring an
	// operator's request for that would refuse for reasons that have nothing to
	// do with the subject being authorized.
	for name, mutate := range map[string]func(AuthorityRequest) AuthorityRequest{
		"state digest": func(a AuthorityRequest) AuthorityRequest { a.StateSHA256 += "x"; return a },
		"observed at":  func(a AuthorityRequest) AuthorityRequest { a.ObservedAt = time.Now(); return a },
		"disposition":  func(a AuthorityRequest) AuthorityRequest { a.Disposition = Active; return a },
		"reason":       func(a AuthorityRequest) AuthorityRequest { a.Reason = "other"; return a },
	} {
		same, err := mutate(request).identify()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if same.ID != request.ID {
			t.Fatalf("changing the %s changed the request id", name)
		}
	}
}

func TestNoAuthorityRequestWhenNoHumanApprovalIsRequired(t *testing.T) {
	// The unmodified Phase 8 governance requires a verifier and nothing else.
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	fixture.reconcile(runID)
	request, err := fixture.runtime.PendingAuthorityRequest(runID)
	if err != nil {
		t.Fatalf("PendingAuthorityRequest: %v", err)
	}
	if request != nil {
		t.Fatalf("a run with no human requirement produced a request: %+v", request)
	}
	if _, err := fixture.runtime.Authorize(context.Background(), AuthorizeInput{
		RunID: runID, RequestID: "authreq-invented", Decision: "approve", Operator: operator(),
	}); refusalCode(t, err) != RefusedNoRequest {
		t.Fatalf("code = %q, want %q", refusalCode(t, err), RefusedNoRequest)
	}
}

// ---------------------------------------------------------------------------
// §2 - authorize records evidence; the evaluator decides what it means
// ---------------------------------------------------------------------------

func TestAuthorizeRecordsEvidenceAndNeverAssignsAuthority(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	before, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}

	result := authorize(t, fixture, approval(runID, request))

	if !result.Recorded || result.EvidenceID == "" {
		t.Fatalf("expected recorded evidence, got %+v", result)
	}
	if result.Status != domain.AuthorityAuthorized {
		t.Fatalf("status = %q, want authorized", result.Status)
	}
	after, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	// Exactly one event, and it is the evidence. Authorize writes no authority
	// decision of its own and no disposition: it did not decide anything.
	if len(after) != len(before)+1 {
		t.Fatalf("appended %d events, want 1: %v", len(after)-len(before), journalTypes(after[len(before):]))
	}
	appended := after[len(after)-1]
	if appended.Type != EventHumanAuthorityRecorded {
		t.Fatalf("appended %q, want %q", appended.Type, EventHumanAuthorityRecorded)
	}
	// Recording human authority is not an engineering side effect: it takes no
	// operation lease and belongs to no operation.
	if appended.OperationID != "" {
		t.Fatalf("evidence was attached to operation %q", appended.OperationID)
	}
	if appended.ID != result.EvidenceID {
		t.Fatalf("event id %q is not the evidence id %q", appended.ID, result.EvidenceID)
	}
	payload, err := decodePayload[HumanAuthorityRecordedPayload](appended.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Request != (Ref{ID: request.ID, Revision: request.Digest}) {
		t.Fatalf("evidence does not cite the exact request: %+v", payload.Request)
	}
	if payload.Binding(runID) != request.binding("approve", testOperator) {
		t.Fatalf("recorded binding is not the request's binding: %+v", payload.Binding(runID))
	}
	// The run's own state is unchanged by the recording: nothing became active
	// and no phase moved. The disposition still comes from the run's
	// conditions, not from the approval.
	status, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if status.PublicationAuthority == nil || status.PublicationAuthority.Status != domain.AuthorityAwaitingAuthority {
		t.Fatalf("the journalled #7 decision was rewritten by authorize: %+v", status.PublicationAuthority)
	}
}

func TestRecordedEvidenceCanStillBeBlockedByPolicy(t *testing.T) {
	// The authority condition still names the branch the run publishes to, so
	// the human claim is genuinely required - but the PERMISSION is granted for
	// a different branch. No human answer can widen a work contract.
	fixture := newAuthorityFixtureWith(t, humanApprovalClaims(), []string{humanClaim, verifyClaim}, "some-other-branch")
	runID, request := awaitAuthority(t, fixture)
	if request.Status != domain.AuthorityBlocked {
		t.Fatalf("status = %q, want blocked", request.Status)
	}
	result := authorize(t, fixture, approval(runID, request))
	if result.Status != domain.AuthorityBlocked {
		t.Fatalf("status after approval = %q, want blocked: approval expanded a permission", result.Status)
	}
	if len(result.Blocking) == 0 {
		t.Fatal("expected the denied permission to remain blocking")
	}
}

func TestRecordedEvidenceCanStillBeIncomplete(t *testing.T) {
	// An external audit claim the runtime cannot produce. Human approval is not
	// a substitute for independent assurance.
	claims := humanApprovalClaims()
	claims[auditClaim] = domain.RequiredClaim{EvidenceClass: auditClass, IndependentFromChangeProducer: true}
	fixture := newAuthorityFixtureWith(t, claims, []string{auditClaim, humanClaim, verifyClaim}, "")
	runID, request := awaitAuthority(t, fixture)
	if request.Status != domain.AuthorityIncomplete {
		t.Fatalf("status = %q, want incomplete", request.Status)
	}
	result := authorize(t, fixture, approval(runID, request))
	if result.Status != domain.AuthorityIncomplete {
		t.Fatalf("status after approval = %q, want incomplete: approval replaced independent assurance", result.Status)
	}
	if !contains(result.Missing, auditClaim) {
		t.Fatalf("missing = %v, want the external audit claim still outstanding", result.Missing)
	}
}

func TestRecordedRejectionIsFailedEvidenceAndBlocks(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	in := approval(runID, request)
	in.Decision = "reject"
	result := authorize(t, fixture, in)
	if result.Status != domain.AuthorityBlocked {
		t.Fatalf("status after rejection = %q, want blocked", result.Status)
	}
}

func TestAuthorizeRefusesAnUnresolvedOperatorIdentity(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	for name, in := range map[string]AuthorizeInput{
		"no identity": {RunID: runID, RequestID: request.ID, Decision: "approve"},
		"unknown provenance": {RunID: runID, RequestID: request.ID, Decision: "approve",
			Operator: RecordedOperator{ID: testOperator, Provenance: "verified-somehow"}},
	} {
		_, err := fixture.runtime.Authorize(context.Background(), in)
		var identity *OperatorIdentityError
		if !errors.As(err, &identity) {
			t.Fatalf("%s: expected *OperatorIdentityError, got %v", name, err)
		}
	}
	in := approval(runID, request)
	in.Decision = "maybe"
	if _, err := fixture.runtime.Authorize(context.Background(), in); refusalCode(t, err) != RefusedInvalidDecision {
		t.Fatalf("an unrecognised decision was accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// §5 - staleness, one case at a time
// ---------------------------------------------------------------------------

// refuseStaleAfter is the shape every staleness case shares: hold a request,
// move exactly one thing, and confirm the held request is refused rather than
// retargeted at the new state.
func refuseStaleAfter(t *testing.T, fixture *phase8Fixture, runID string, held AuthorityRequest, wantCode string) {
	t.Helper()
	_, err := fixture.runtime.Authorize(context.Background(), approval(runID, held))
	if code := refusalCode(t, err); code != wantCode {
		t.Fatalf("code = %q, want %q (%v)", code, wantCode, err)
	}
	if len(humanAuthorityEvents(t, fixture, runID)) != 0 {
		t.Fatal("a refused authorization still recorded evidence")
	}
}

func TestStalenessA_ExecutionProviderCreatesANewCandidate(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	commit, tree := advanceCandidate(t, fixture, runID, "provider.go")
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateChanged, CandidateChangedPayload{
		ProducerID: "test-provider", Purpose: InvocationInitial, Outcome: Succeeded,
	})
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateCommitted, CandidateCommittedPayload{
		Commit: commit, Tree: tree, PathCount: 1, PathsDigest: pathsDigest([]string{"provider.go"}),
	})
	fresh := currentRequest(t, fixture, runID)
	if fresh.ID == held.ID {
		t.Fatal("a new candidate did not produce a new request")
	}
	if fresh.Candidate.Revision != commit || fresh.Candidate.Tree != tree {
		t.Fatalf("the fresh request is not bound to the new candidate: %+v", fresh.Candidate)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedStaleRequest)
}

func TestStalenessB_DeterministicRemediationCreatesANewCandidate(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	commit, tree := advanceCandidate(t, fixture, runID, "remediated.go")
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateChanged, CandidateChangedPayload{
		ProducerID: "remediation.gofmt", Purpose: InvocationRemediation, Outcome: Succeeded,
	})
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateCommitted, CandidateCommittedPayload{
		Commit: commit, Tree: tree, PathCount: 1, PathsDigest: pathsDigest([]string{"remediated.go"}),
	})
	refuseStaleAfter(t, fixture, runID, held, RefusedStaleRequest)
}

func TestStalenessC_BaseIntegrationChangesTheCandidate(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	base := fixture.moveBase("moved.md", "moved\n")
	commit, tree := advanceCandidate(t, fixture, runID, "integrated.go")
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateBaseIntegrated, CandidateBaseIntegratedPayload{
		Strategy: "rebase", BaseRevision: base, Commit: commit, Tree: tree,
	})
	fresh := currentRequest(t, fixture, runID)
	if fresh.Base.Revision != base {
		t.Fatalf("base binding = %q, want the integrated base %q", fresh.Base.Revision, base)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedStaleRequest)
}

func TestStalenessD_ReassessmentCreatesANewContractRevision(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	journalEvent(t, fixture.store, fixture.clock, runID, EventReassessmentCompleted, ReassessmentCompletedPayload{
		Material: true,
		Contract: Ref{ID: held.Contract.ID, Revision: held.Contract.Revision + "-next"},
	})
	fresh := currentRequest(t, fixture, runID)
	if fresh.Contract == held.Contract {
		t.Fatal("a new contract revision did not move the request's contract binding")
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedStaleRequest)
}

func TestStalenessE_ControllerChanges(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)

	// A different controller identity, same durable run. This is also the rule
	// that a human approval can never override a controller mismatch.
	moved := fixture.deps
	moved.ControllerID = "controller-b"
	fixture.runtime = fixture.newRuntime(moved)

	if _, err := fixture.runtime.PendingAuthorityRequest(runID); refusalCode(t, err) != RefusedControllerChanged {
		t.Fatalf("reading a request under a changed controller: %v", err)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedControllerChanged)
}

func TestStalenessF_SourceIsRefreshed(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	journalEvent(t, fixture.store, fixture.clock, runID, EventSourceIntentChanged, SourceIntentChangedPayload{
		PreviousDigest: "before", CurrentDigest: "after", Reason: "issue edited",
	})
	if _, err := fixture.runtime.PendingAuthorityRequest(runID); refusalCode(t, err) != RefusedSourceMoved {
		t.Fatalf("reading a request against moved source: %v", err)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedSourceMoved)
}

func TestStalenessG_TheAuthorityRequirementItselfChanges(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)

	// Same candidate, same tree, same contract revision, same controller, same
	// source. Only the required claim changed. The action string, the issue
	// number and the pull request number are all unchanged, and none of them
	// carries the approval forward.
	claims := map[string]domain.RequiredClaim{
		verifyClaim:        {EvidenceClass: AssuranceEvidenceClass, IndependentFromChangeProducer: true},
		humanClaim + "-v2": {EvidenceClass: HumanApprovalEvidenceClass, IndependentFromChangeProducer: true},
	}
	moved := fixture.deps
	moved.Policy = governedBy(moved.Policy, claims, []string{humanClaim + "-v2", verifyClaim}, fixture.branch, fixture.branch)
	fixture.runtime = fixture.newRuntime(moved)

	fresh := currentRequest(t, fixture, runID)
	if fresh.Candidate != held.Candidate || fresh.Contract != held.Contract {
		t.Fatalf("the fixture moved more than the requirement: %+v vs %+v", fresh, held)
	}
	if fresh.ID == held.ID {
		t.Fatal("a changed authority requirement did not change the request id")
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedStaleRequest)
}

// TestApprovalIsNotRetargetedAtANewCandidate is the other half of staleness: an
// approval that WAS recorded does not follow the run onto a new subject.
func TestApprovalIsNotRetargetedAtANewCandidate(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	if result := authorize(t, fixture, approval(runID, request)); result.Status != domain.AuthorityAuthorized {
		t.Fatalf("status = %q, want authorized", result.Status)
	}

	commit, tree := advanceCandidate(t, fixture, runID, "later.go")
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateCommitted, CandidateCommittedPayload{
		Commit: commit, Tree: tree, PathCount: 1, PathsDigest: pathsDigest([]string{"later.go"}),
	})

	fresh := currentRequest(t, fixture, runID)
	if !contains(fresh.Requires, humanClaim) {
		t.Fatalf("the recorded approval was carried onto the new candidate: %+v", fresh)
	}
	if fresh.Status == domain.AuthorityAuthorized {
		t.Fatal("the new candidate is authorized by an approval given for the old one")
	}
	// The evidence is still in the journal. It simply is not evidence about
	// this subject; nothing was deleted and nothing was rebound.
	if len(humanAuthorityEvents(t, fixture, runID)) != 1 {
		t.Fatal("the recorded evidence was mutated or removed")
	}
}

// ---------------------------------------------------------------------------
// What human approval can never do
// ---------------------------------------------------------------------------

func TestApprovalCannotGrantARequestedPrivilegeExpansion(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, _ := awaitAuthority(t, fixture)
	state := fixture.state(runID)
	journalEvent(t, fixture.store, fixture.clock, runID, EventReassessmentCompleted, ReassessmentCompletedPayload{
		Material: true, Contract: state.projection.Contract, RequestedPrivilegeCount: 2,
	})
	request := currentRequest(t, fixture, runID)
	if request.Reason != "requested_privilege_expansion" {
		t.Fatalf("request reason = %q, want requested_privilege_expansion", request.Reason)
	}
	result := authorize(t, fixture, approval(runID, request))
	if result.Disposition != Waiting || result.Reason != "requested_privilege_expansion" {
		t.Fatalf("approval granted a privilege expansion: %s/%s", result.Disposition, result.Reason)
	}
}

func TestApprovalCannotOverrideAWorkspaceIntegrityFailure(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	journalEvent(t, fixture.store, fixture.clock, runID, EventCandidateExternalChanged, CandidateExternalChangedPayload{
		ExpectedRevision: held.Candidate.Revision, ObservedRevision: strings.Repeat("f", 40),
	})
	if _, err := fixture.runtime.PendingAuthorityRequest(runID); refusalCode(t, err) != RefusedExternalHead {
		t.Fatalf("reading a request against an unexpected external head: %v", err)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedExternalHead)
}

func TestApprovalCannotBeRecordedAgainstATerminalRun(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	journalEvent(t, fixture.store, fixture.clock, runID, EventRunCancelled, struct {
		Reason string `json:"reason,omitempty"`
	}{"operator_stop"})
	refuseStaleAfter(t, fixture, runID, held, RefusedRunTerminal)
}

// ---------------------------------------------------------------------------
// §6 - idempotency and crash safety
// ---------------------------------------------------------------------------

// A: validation completed, the process died before the append. Nothing durable
// exists, so the retry validates again and records exactly once.
func TestCrashA_ValidatedButNotAppendedRetriesSafely(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	if len(humanAuthorityEvents(t, fixture, runID)) != 0 {
		t.Fatal("evidence exists before any authorization")
	}
	result := authorize(t, fixture, approval(runID, request))
	if !result.Recorded {
		t.Fatal("the retry after a pre-append crash did not record the evidence")
	}
	if got := len(humanAuthorityEvents(t, fixture, runID)); got != 1 {
		t.Fatalf("recorded %d evidence events, want 1", got)
	}
}

// B: the append committed, the process died before the CLI printed. The retry
// must adopt the existing evidence rather than write a second record.
func TestCrashB_AppendedButUnreportedRetryAdoptsTheEvidence(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	first := authorize(t, fixture, approval(runID, request))

	retry := approval(runID, request)
	retry.Note = "different wording on the retry"
	second := authorize(t, fixture, retry)

	if second.Recorded {
		t.Fatal("the retry wrote a second evidence record")
	}
	if second.EvidenceID != first.EvidenceID {
		t.Fatalf("idempotency key moved: %q then %q", first.EvidenceID, second.EvidenceID)
	}
	if second.Status != first.Status {
		t.Fatalf("the retry reported a different status: %q then %q", first.Status, second.Status)
	}
	if got := len(humanAuthorityEvents(t, fixture, runID)); got != 1 {
		t.Fatalf("recorded %d evidence events, want 1: an annotation minted a second record", got)
	}
}

// secondHandle opens an INDEPENDENT SQLite handle on the same state directory,
// which is what a second operator process or a running controller actually is.
func secondHandle(t *testing.T, fixture *phase8Fixture) *EngineeringRuntime {
	t.Helper()
	store, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	deps := fixture.deps
	deps.Store = store
	return fixture.newRuntime(deps)
}

// C: two operator processes authorize the same exact request at the same time.
// Exactly one logical evidence record may exist.
func TestCrashC_ConcurrentAuthorizationsProduceOneEvidenceRecord(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	other := secondHandle(t, fixture)

	results := make([]AuthorizeResult, 2)
	errs := make([]error, 2)
	runtimes := []*EngineeringRuntime{fixture.runtime, other}
	var wait sync.WaitGroup
	for i := range runtimes {
		wait.Add(1)
		go func(i int) {
			defer wait.Done()
			results[i], errs[i] = runtimes[i].Authorize(context.Background(), approval(runID, request))
		}(i)
	}
	wait.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("process %d: %v", i, err)
		}
	}
	if results[0].EvidenceID != results[1].EvidenceID {
		t.Fatalf("the two processes produced different evidence ids: %q and %q", results[0].EvidenceID, results[1].EvidenceID)
	}
	if results[0].Recorded == results[1].Recorded {
		t.Fatalf("exactly one process must have written the record; both reported recorded=%v", results[0].Recorded)
	}
	if got := len(humanAuthorityEvents(t, fixture, runID)); got != 1 {
		t.Fatalf("recorded %d evidence events, want 1", got)
	}
}

// D: an operator authorizes while a controller records a GitHub observation.
// Both events must survive with a valid ordering and an intact hash chain.
func TestCrashD_AuthorizationAndObservationBothSurvive(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	controller := secondHandle(t, fixture)

	var wait sync.WaitGroup
	var authorizeErr, observeErr error
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, authorizeErr = fixture.runtime.Authorize(context.Background(), approval(runID, request))
	}()
	go func() {
		defer wait.Done()
		raw, err := marshalPayloadJSON(GitHubPRObservedPayload{
			Number: 7, HeadRevision: request.Candidate.Revision, BaseRevision: request.Base.Revision, State: "open",
		})
		if err != nil {
			observeErr = err
			return
		}
		_, observeErr = controller.deps.Store.AppendEvent(EngineeringEvent{
			SchemaVersion: SchemaVersion, ID: newEventID(runID), RunID: runID,
			Type: EventGitHubPRObserved, OccurredAt: fixture.clock.Now(), Payload: raw,
		})
	}()
	wait.Wait()

	if authorizeErr != nil || observeErr != nil {
		t.Fatalf("authorize: %v; observe: %v", authorizeErr, observeErr)
	}
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if countType(events, EventHumanAuthorityRecorded) != 1 || countType(events, EventGitHubPRObserved) != 1 {
		t.Fatalf("both events must survive: %v", journalTypes(events))
	}
	for i, event := range events {
		if event.Sequence != int64(i+1) {
			t.Fatalf("event %d has sequence %d", i, event.Sequence)
		}
		if i > 0 && (event.PreviousEventID != events[i-1].ID || event.PreviousEventHash != events[i-1].EventHash) {
			t.Fatalf("hash chain broken at sequence %d", event.Sequence)
		}
	}
	// Replay re-verifies the chain and every event hash.
	if _, err := fixture.store.Replay(runID); err != nil {
		t.Fatalf("Replay after concurrent appends: %v", err)
	}
}

// ---------------------------------------------------------------------------
// §7 - the self-adoption boundary
// ---------------------------------------------------------------------------

func TestAdoptionIsNotAHumanAuthorizableAction(t *testing.T) {
	// Publication may legitimately sit behind a human. Adoption may not, and it
	// is refused as unsupported rather than given a governed shape here.
	for _, action := range []string{"git.pull_request.create", "git.pull_request.update", "candidate.push"} {
		if !authorizableActions[action] {
			t.Fatalf("%s should be human-authorizable", action)
		}
	}
	for _, action := range []string{"git.merge", "git.pull_request.merge", "candidate.adopt", "autonomy.merge"} {
		if authorizableActions[action] {
			t.Fatalf("%s must not be human-authorizable: a candidate cannot authorize its own adoption", action)
		}
	}
}

func TestAuthorizeRefusesAnUnsupportedAdoptionAction(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)
	for _, action := range []domain.Action{
		{Type: "git.merge", Target: fixture.branch},
		{Type: "git.pull_request.merge", Target: fixture.branch},
		{Type: "candidate.adopt", Target: fixture.branch},
	} {
		in := approval(runID, request)
		in.Action = action
		_, err := fixture.runtime.Authorize(context.Background(), in)
		if code := refusalCode(t, err); code != RefusedUnsupportedAction {
			t.Fatalf("%s: code = %q, want %q", action.Type, code, RefusedUnsupportedAction)
		}
	}
	if len(humanAuthorityEvents(t, fixture, runID)) != 0 {
		t.Fatal("a refused adoption still recorded evidence")
	}
}

func TestAJournalledAdoptionDecisionRefusesTheWholeBoundary(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, held := awaitAuthority(t, fixture)
	journalEvent(t, fixture.store, fixture.clock, runID, EventAuthorityEvaluated, AuthorityEvaluatedPayload{
		Decision: Ref{ID: "decision-merge", Revision: "1"},
		Action:   domain.Action{Type: "git.merge", Target: fixture.branch},
		Status:   domain.AuthorityAwaitingAuthority,
	})
	if _, err := fixture.runtime.PendingAuthorityRequest(runID); refusalCode(t, err) != RefusedUnsupportedAction {
		t.Fatalf("reading a request for a run holding a merge decision: %v", err)
	}
	refuseStaleAfter(t, fixture, runID, held, RefusedUnsupportedAction)
}
