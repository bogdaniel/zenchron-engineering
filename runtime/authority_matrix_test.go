package runtime

// Phase 10 §17 - the human-authority adversarial matrix.
//
// Most of §17 is already proven by authority_request_test.go and
// reconciler_test.go, and those tests are not restated here. This file holds
// only the three rows nothing else covers:
//
//	G  a WELL-FORMED but wrong action is named alongside a valid request
//	H  a fabricated request identity is named against a run that really does
//	   have a pending request
//	M  a restart: the store is genuinely closed and reopened, and the evidence
//	   and the reason it produced have to come back off disk unchanged
//
// Everything is asserted on the persisted journal - the events the store hands
// back, their bytes and their hash chain - never on the value Authorize
// happened to return. It runs offline on the same Phase 8 fixture, with the
// same fakes and the same injected clock, and passes under
// `docker run --network none` and `-race`.

import (
	"context"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// refuseWithoutRecording is the shape G and H share: the call is refused with
// an exact code, and - the part that matters - the journal is left with no
// evidence at all. A refusal that still appended would be the real failure.
func refuseWithoutRecording(t *testing.T, fixture *phase8Fixture, runID string, in AuthorizeInput, wantCode string) {
	t.Helper()
	_, err := fixture.runtime.Authorize(context.Background(), in)
	if code := refusalCode(t, err); code != wantCode {
		t.Fatalf("code = %q, want %q (%v)", code, wantCode, err)
	}
	if events := humanAuthorityEvents(t, fixture, runID); len(events) != 0 {
		t.Fatalf("a refused authorization recorded %d evidence events in the journal", len(events))
	}
}

// ---------------------------------------------------------------------------
// G - the wrong action
// ---------------------------------------------------------------------------

// TestG_AuthorizingAWrongActionIsRefused covers the gap between the two
// existing action tests. TestAuthorizeRefusesAnUnsupportedAdoptionAction proves
// an action OUTSIDE the allowlist is refused as unsupported; this proves an
// action that is perfectly authorizable in general - pushing the candidate,
// updating a pull request - is still refused when it is not THIS request's
// action, and so is the request's own action pointed at a different branch.
//
// The distinction is the whole point of §17 G: the allowlist is not the
// binding. An operator answering "yes, push the candidate" has not answered
// "yes, open a pull request against main", and the two must not be
// interchangeable just because both are governable.
func TestG_AuthorizingAWrongActionIsRefused(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)

	if request.Action != (domain.Action{Type: PublicationActionType, Target: fixture.branch}) {
		t.Fatalf("fixture request action = %+v, want the publication action", request.Action)
	}

	for name, action := range map[string]domain.Action{
		// Allowlisted, and not this request's action.
		"a different authorizable action": {Type: "candidate.push", Target: fixture.branch},
		"a pull request update":           {Type: "git.pull_request.update", Target: fixture.branch},
		// The right action type aimed at a branch the request does not name.
		// The target is part of the action and part of the request id.
		"the right action, wrong target": {Type: PublicationActionType, Target: "some-other-branch"},
	} {
		in := approval(runID, request)
		in.Action = action
		t.Run(name, func(t *testing.T) {
			refuseWithoutRecording(t, fixture, runID, in, RefusedStaleRequest)
		})
	}

	// And the control: the request's own exact action is accepted, so the three
	// refusals above are about the binding and not about naming an action at
	// all. Asserted on the journal.
	in := approval(runID, request)
	in.Action = request.Action
	if result := authorize(t, fixture, in); !result.Recorded {
		t.Fatal("naming the request's own action recorded nothing")
	}
	events := humanAuthorityEvents(t, fixture, runID)
	if len(events) != 1 {
		t.Fatalf("journal holds %d evidence events, want 1", len(events))
	}
	payload, err := decodePayload[HumanAuthorityRecordedPayload](events[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Action != request.Action {
		t.Fatalf("recorded action = %+v, want the request's action %+v", payload.Action, request.Action)
	}
}

// ---------------------------------------------------------------------------
// H - a fabricated request identity
// ---------------------------------------------------------------------------

// TestH_AFabricatedRequestIdentityIsRefused is the "an id is not a password"
// case. TestNoAuthorityRequestWhenNoHumanApprovalIsRequired already shows an
// invented id against a run with NOTHING pending; that path returns
// no_authority_request before any comparison happens, so it proves nothing
// about the comparison itself.
//
// Here the run genuinely IS awaiting human authority. A fabricated id, an
// omitted id, a truncated id, and a real id carried with a fabricated digest
// must each be refused as stale, and none of them may append.
func TestH_AFabricatedRequestIdentityIsRefused(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)

	for name, mutate := range map[string]func(AuthorizeInput) AuthorizeInput{
		"a fabricated id": func(in AuthorizeInput) AuthorizeInput {
			in.RequestID = "authreq-" + strings.Repeat("0", 32)
			return in
		},
		"no id at all": func(in AuthorizeInput) AuthorizeInput {
			in.RequestID = ""
			return in
		},
		// A prefix of the real id. Guarding with a prefix or contains test
		// rather than equality would let this through.
		"a truncated id": func(in AuthorizeInput) AuthorizeInput {
			in.RequestID = in.RequestID[:len(in.RequestID)-1]
			return in
		},
		// The id is genuine; the pinned digest is not. The digest is a
		// redundancy check and it has to actually check.
		"a fabricated digest": func(in AuthorizeInput) AuthorizeInput {
			in.Digest = strings.Repeat("a", 64)
			return in
		},
	} {
		t.Run(name, func(t *testing.T) {
			refuseWithoutRecording(t, fixture, runID, mutate(approval(runID, request)), RefusedStaleRequest)
		})
	}

	// The request the run really produces is unchanged by any of it: none of
	// the fabrications moved the run's state, so the honest operator's request
	// is still answerable.
	if after := currentRequest(t, fixture, runID); after.ID != request.ID || after.Digest != request.Digest {
		t.Fatalf("a fabricated authorization moved the run's request: %q -> %q", request.ID, after.ID)
	}
}

// ---------------------------------------------------------------------------
// M - restart
// ---------------------------------------------------------------------------

// reopen genuinely restarts the process's view of the state directory: the
// fixture's store is CLOSED, so nothing below can be answered out of its
// connection pool or its page cache, and a new store is opened over the same
// directory. Everything the reopened runtime reports has to come off disk.
func reopen(t *testing.T, fixture *phase8Fixture) (*SQLiteOperationStore, *EngineeringRuntime) {
	t.Helper()
	if err := fixture.store.Close(); err != nil {
		t.Fatalf("closing the store: %v", err)
	}
	store, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatalf("reopening the store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	deps := fixture.deps
	deps.Store = store
	fixture.store, fixture.deps = store, deps
	fixture.runtime = fixture.newRuntime(deps)
	return store, fixture.runtime
}

// TestM_RestartPreservesTheExactEvidenceAndItsReason restarts after a
// REJECTION, which is the dangerous direction: approval evidence that failed to
// survive a restart would leave a run blocked, but rejection evidence that
// failed to survive would silently unblock a run a human refused. Nothing else
// in the suite closes the store, so nothing else can tell the difference
// between durable state and a warm handle.
//
// Three things have to come back: the event's bytes, the reason the human gave,
// and the reason the RUN reports as a result.
func TestM_RestartPreservesTheExactEvidenceAndItsReason(t *testing.T) {
	fixture := newAuthorityFixture(t)
	runID, request := awaitAuthority(t, fixture)

	rejection := approval(runID, request)
	rejection.Decision = "reject"
	rejection.Note = "the change touches the payments boundary"
	result := authorize(t, fixture, rejection)
	if !result.Recorded {
		t.Fatal("the rejection recorded nothing")
	}
	// Reconcile so the kernel's blocked decision is journalled too: the reason
	// under test is one the run derives, not one the test asserts.
	fixture.reconcile(runID)

	before, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	beforeStatus, err := fixture.runtime.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if beforeStatus.Disposition != Waiting || beforeStatus.Reason != "authority_blocked" {
		t.Fatalf("before restart: %s/%s, want waiting/authority_blocked", beforeStatus.Disposition, beforeStatus.Reason)
	}

	// ---- restart ----
	store, restarted := reopen(t, fixture)

	// Replay re-verifies the hash chain and every event hash from disk. If the
	// journal did not survive intact this fails before any assertion below.
	if _, err := store.Replay(runID); err != nil {
		t.Fatalf("Replay after restart: %v", err)
	}
	after, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("restart changed the journal length: %d then %d", len(before), len(after))
	}
	// Byte-for-byte, event by event, including the chain links and the hashes.
	for i := range before {
		a, b := before[i], after[i]
		if a.ID != b.ID || a.Sequence != b.Sequence || a.Type != b.Type {
			t.Fatalf("event %d changed identity across the restart: %+v vs %+v", i, a, b)
		}
		if !a.OccurredAt.Equal(b.OccurredAt) {
			t.Fatalf("event %s: occurred_at %v became %v", a.ID, a.OccurredAt, b.OccurredAt)
		}
		if a.OperationID != b.OperationID {
			t.Fatalf("event %s: operation %q became %q", a.ID, a.OperationID, b.OperationID)
		}
		if a.EventHash != b.EventHash || a.PreviousEventID != b.PreviousEventID || a.PreviousEventHash != b.PreviousEventHash {
			t.Fatalf("event %s: the hash chain did not survive the restart", a.ID)
		}
		if string(a.Payload) != string(b.Payload) {
			t.Fatalf("event %s payload is not byte-identical after the restart:\n before %s\n after  %s", a.ID, a.Payload, b.Payload)
		}
	}

	// The evidence itself, decoded from the reopened store. The operator's
	// reason, the decision, the identity and the exact subject it was given
	// against all have to be the same facts.
	human := humanAuthorityEvents(t, fixture, runID)
	if len(human) != 1 {
		t.Fatalf("journal holds %d evidence events after the restart, want 1", len(human))
	}
	if human[0].ID != result.EvidenceID {
		t.Fatalf("evidence id %q became %q across the restart", result.EvidenceID, human[0].ID)
	}
	payload, err := decodePayload[HumanAuthorityRecordedPayload](human[0].Payload)
	if err != nil {
		t.Fatal(err)
	}
	if payload.Decision != "reject" {
		t.Fatalf("recorded decision = %q, want reject: the refusal did not survive the restart", payload.Decision)
	}
	if payload.Note != rejection.Note {
		t.Fatalf("the operator's reason did not survive: %q, want %q", payload.Note, rejection.Note)
	}
	if payload.Operator != operator() {
		t.Fatalf("the operator identity did not survive: %+v", payload.Operator)
	}
	if payload.Request != (Ref{ID: request.ID, Revision: request.Digest}) {
		t.Fatalf("the answered request reference did not survive: %+v", payload.Request)
	}
	if payload.Candidate != request.Candidate || payload.Contract != request.Contract {
		t.Fatalf("the subject binding did not survive: %+v / %+v", payload.Candidate, payload.Contract)
	}

	// The reason the RUN reports, recomputed by a runtime that has only ever
	// seen this state directory. Including the state digest: the whole replayed
	// state is the same state.
	afterStatus, err := restarted.Status(runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterStatus.Disposition != beforeStatus.Disposition || afterStatus.Reason != beforeStatus.Reason {
		t.Fatalf("the run's reason did not survive: %s/%s became %s/%s",
			beforeStatus.Disposition, beforeStatus.Reason, afterStatus.Disposition, afterStatus.Reason)
	}
	if afterStatus.StateSHA256 != beforeStatus.StateSHA256 {
		t.Fatalf("state digest %q became %q across the restart", beforeStatus.StateSHA256, afterStatus.StateSHA256)
	}
	if afterStatus.PublicationAuthority == nil || afterStatus.PublicationAuthority.Status != domain.AuthorityBlocked {
		t.Fatalf("the journalled decision after restart = %+v, want blocked", afterStatus.PublicationAuthority)
	}
	// A rejected run is still not authorized and still has not published.
	if fixture.state(runID).published() || countMethod(fixture.forge.Calls, "CreatePullRequest") != 0 {
		t.Fatal("the run published across a restart despite a recorded rejection")
	}

	// Finally the operator retries the same command after the restart, which is
	// exactly what a human does when a crash ate the output. It adopts the
	// record that is on disk rather than writing a second one, and reports the
	// same conclusion.
	retry := authorize(t, fixture, rejection)
	if retry.Recorded {
		t.Fatal("the post-restart retry wrote a second evidence record")
	}
	if retry.EvidenceID != result.EvidenceID {
		t.Fatalf("the idempotency key moved across the restart: %q then %q", result.EvidenceID, retry.EvidenceID)
	}
	if retry.Status != domain.AuthorityBlocked {
		t.Fatalf("the post-restart retry reported %q, want blocked", retry.Status)
	}
	if got := len(humanAuthorityEvents(t, fixture, runID)); got != 1 {
		t.Fatalf("journal holds %d evidence events after the retry, want 1", got)
	}
}
