package orchestration

import (
	"strings"
	"testing"
	"time"
)

var decisionNow = time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)

func operatorAuthority(actor string) DecisionResolutionAuthority {
	return DecisionResolutionAuthority{Actor: actor, AuthorityKind: AuthorityKindOperator, Provenance: "local_control_endpoint"}
}

func allowOutcome() DecisionOutcome {
	return DecisionOutcome{Kind: DecisionAllowDeny, Value: DecisionAllow}
}

func TestDecisionOutcomeValidate(t *testing.T) {
	cases := []struct {
		name    string
		outcome DecisionOutcome
		wantErr string
	}{
		{"allow is legal", DecisionOutcome{Kind: DecisionAllowDeny, Value: DecisionAllow}, ""},
		{"deny is legal", DecisionOutcome{Kind: DecisionAllowDeny, Value: DecisionDeny}, ""},
		{"allow_deny refuses a third value", DecisionOutcome{Kind: DecisionAllowDeny, Value: "maybe"}, "allow_deny"},
		{"selected_option within bound", DecisionOutcome{Kind: DecisionSelectedOption, Value: "preserve_v1"}, ""},
		{"selected_option over bound", DecisionOutcome{Kind: DecisionSelectedOption, Value: strings.Repeat("x", 65)}, "above the 64 byte bound"},
		{"text within bound", DecisionOutcome{Kind: DecisionText, Value: "proceed with the migration"}, ""},
		{"text over bound", DecisionOutcome{Kind: DecisionText, Value: strings.Repeat("x", 2001)}, "above the 2000 byte bound"},
		{"empty text refused", DecisionOutcome{Kind: DecisionText, Value: ""}, "required"},
		{"unknown kind refused", DecisionOutcome{Kind: "workflow", Value: "anything"}, "not"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.outcome.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, c.wantErr)
			}
		})
	}
}

// TestDecisionResolutionAuthorityRefusesIdentityAlone proves architectural law
// #2 ("identity is not authority") and the self-resolution law (#1): the only
// authority kind this build recognizes is the operator one, established
// through the existing control-plane identity mechanism. Nothing a worker's
// message report could ever produce - there is no such member in MessageDraft
// - and nothing merely naming a plausible actor can pass this gate.
func TestDecisionResolutionAuthorityRefusesIdentityAlone(t *testing.T) {
	cases := []struct {
		name string
		auth DecisionResolutionAuthority
		ok   bool
	}{
		{"operator authority is recognized", operatorAuthority("operator-1"), true},
		{"a worker claiming its own agent id is refused", DecisionResolutionAuthority{Actor: "issue-7", AuthorityKind: "worker", Provenance: "message_report"}, false},
		{"an empty authority kind is refused", DecisionResolutionAuthority{Actor: "someone", AuthorityKind: "", Provenance: "p"}, false},
		{"an actor name alone with no kind is refused", DecisionResolutionAuthority{Actor: "operator-1"}, false},
		{"a kind with no actor is refused", DecisionResolutionAuthority{AuthorityKind: AuthorityKindOperator, Provenance: "p"}, false},
		{"a kind with no provenance is refused", DecisionResolutionAuthority{Actor: "operator-1", AuthorityKind: AuthorityKindOperator}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.auth.Validate()
			if c.ok && err != nil {
				t.Fatalf("expected authority to validate, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("expected authority to be refused, it validated")
			}
		})
	}
}

func TestDecisionResolutionIDIsDeterministicFromRequestAlone(t *testing.T) {
	id1, err := DecisionResolutionID("message-abc")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := DecisionResolutionID("message-abc")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("resolution id was not deterministic: %s vs %s", id1, id2)
	}
	id3, err := DecisionResolutionID("message-def")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id3 {
		t.Fatal("different requests produced the same resolution id")
	}
	if _, err := DecisionResolutionID(""); err == nil {
		t.Fatal("expected an empty request id to be refused")
	}
}

func TestResolveDecisionNormalAuthorizedResolution(t *testing.T) {
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: true}
	resolution, err := ResolveDecision(ref, nil, allowOutcome(), "ship it", operatorAuthority("operator-1"), nil, decisionNow)
	if err != nil {
		t.Fatalf("expected an authorized resolution to succeed, got %v", err)
	}
	wantID, _ := DecisionResolutionID(ref.ID)
	if resolution.ID != wantID || resolution.RequestID != ref.ID || resolution.Scope != ref.Scope {
		t.Fatalf("resolution %+v does not describe request %s", resolution, ref.ID)
	}
	if err := resolution.Validate(); err != nil {
		t.Fatalf("built resolution does not validate: %v", err)
	}
}

func TestResolveDecisionRefusesUnknownRequest(t *testing.T) {
	_, err := ResolveDecision(DecisionRequestRef{}, nil, allowOutcome(), "", operatorAuthority("operator-1"), nil, decisionNow)
	if err == nil {
		t.Fatal("expected an unknown (empty) request to be refused")
	}
}

func TestResolveDecisionRefusesASupersededRequest(t *testing.T) {
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: false}
	_, err := ResolveDecision(ref, nil, allowOutcome(), "", operatorAuthority("operator-1"), nil, decisionNow)
	if err == nil || !strings.Contains(err.Error(), "no longer live") {
		t.Fatalf("expected a superseded/withdrawn request to be refused, got %v", err)
	}
}

func TestResolveDecisionRefusesAStaleSubject(t *testing.T) {
	subject := &MessageSubject{Handoff: "handoff-1", Owner: "issue-2", Revision: HandoffSubject{CandidateRevision: "c1", CandidateTree: "tree-c1"}}
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: true, Subject: subject}

	// The owner's current subject has moved on to c2: resolving against the
	// request's original c1 binding must be refused as stale.
	moved := HandoffSubject{CandidateRevision: "c2", CandidateTree: "tree-c2"}
	if _, err := ResolveDecision(ref, &moved, allowOutcome(), "", operatorAuthority("operator-1"), nil, decisionNow); err == nil {
		t.Fatal("expected a resolution against a replaced subject to be refused as stale")
	}
	// No current subject at all - the owner produced nothing yet, or the
	// handoff was never admitted - is refused the same way.
	if _, err := ResolveDecision(ref, nil, allowOutcome(), "", operatorAuthority("operator-1"), nil, decisionNow); err == nil {
		t.Fatal("expected a resolution with no current subject to be refused as stale")
	}
	// The EXACT current subject is accepted.
	current := subject.Revision
	if _, err := ResolveDecision(ref, &current, allowOutcome(), "", operatorAuthority("operator-1"), nil, decisionNow); err != nil {
		t.Fatalf("expected a resolution against the exact current subject to succeed, got %v", err)
	}
}

func TestResolveDecisionRefusesUnauthorizedAuthority(t *testing.T) {
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: true}
	unauthorized := DecisionResolutionAuthority{Actor: "issue-2", AuthorityKind: "worker", Provenance: "message_report"}
	if _, err := ResolveDecision(ref, nil, allowOutcome(), "", unauthorized, nil, decisionNow); err == nil {
		t.Fatal("expected an unauthorized actor to be refused")
	}
}

// TestResolveDecisionIdempotentRetry proves acceptance #8: an identical retry
// after a lost reply yields the same resolution rather than a second write or
// a refusal.
func TestResolveDecisionIdempotentRetry(t *testing.T) {
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: true}
	first, err := ResolveDecision(ref, nil, allowOutcome(), "ship it", operatorAuthority("operator-1"), nil, decisionNow)
	if err != nil {
		t.Fatal(err)
	}
	// A later clock reading must not change the identity or the content of an
	// idempotent replay: the ANSWER, not the retry's own timestamp, decides it.
	retry, err := ResolveDecision(ref, nil, allowOutcome(), "ship it", operatorAuthority("operator-1"), &first, decisionNow.Add(time.Hour))
	if err != nil {
		t.Fatalf("expected an identical retry to be idempotent, got %v", err)
	}
	if retry != first {
		t.Fatalf("identical retry produced a different resolution: %+v vs %+v", retry, first)
	}
}

// TestResolveDecisionRefusesAConflictingSecondAnswer proves acceptance #9.
func TestResolveDecisionRefusesAConflictingSecondAnswer(t *testing.T) {
	ref := DecisionRequestRef{ID: "message-1", Scope: "batch-1", Live: true}
	first, err := ResolveDecision(ref, nil, allowOutcome(), "ship it", operatorAuthority("operator-1"), nil, decisionNow)
	if err != nil {
		t.Fatal(err)
	}
	denyOutcome := DecisionOutcome{Kind: DecisionAllowDeny, Value: DecisionDeny}
	if _, err := ResolveDecision(ref, nil, denyOutcome, "changed my mind", operatorAuthority("operator-1"), &first, decisionNow); err == nil {
		t.Fatal("expected a conflicting second answer to be refused")
	}
}

func TestWorkUnitHoldIdentityAndRendering(t *testing.T) {
	id, err := WorkUnitHoldID("graph-1", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	again, err := WorkUnitHoldID("graph-1", "deploy")
	if err != nil || id != again {
		t.Fatalf("hold id was not deterministic from graph and unit alone: %v / %s vs %s", err, id, again)
	}
	other, err := WorkUnitHoldID("graph-1", "build")
	if err != nil || other == id {
		t.Fatal("different units produced the same hold id")
	}
	hold := WorkUnitHold{
		SchemaVersion: WorkUnitHoldSchemaVersion, ID: id, GraphID: "graph-1", UnitID: "deploy",
		Purpose: "human sign-off before production deploy", RequestedBy: operatorAuthority("operator-1"), RequestedAt: decisionNow,
	}
	if err := hold.Validate(); err != nil {
		t.Fatalf("a well-formed hold should validate, got %v", err)
	}
	ref := hold.Ref()
	if ref.ID != id || !ref.Live || ref.Subject != nil {
		t.Fatalf("hold.Ref() is not the expected unconditionally-live, subject-free request: %+v", ref)
	}
	wait := hold.DecisionWait()
	if wait.Reference != id || wait.Detail != hold.Purpose {
		t.Fatalf("hold.DecisionWait() does not describe the hold opaquely: %+v", wait)
	}
	// A hold placed by anything other than an authorized operator is refused,
	// the same law a resolution's authority already enforces.
	bad := hold
	bad.RequestedBy = DecisionResolutionAuthority{Actor: "issue-7", AuthorityKind: "worker", Provenance: "message_report"}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected a hold requested by a non-operator authority to be refused")
	}
}
