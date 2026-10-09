package runtime

// #508 review P2: DecisionResolution admission is linearizable, proved with
// REAL concurrent goroutines and a deterministic start barrier - never a
// timing-dependent sleep - racing SQLiteOperationStore.ResolveDecisionRequest
// against the OTHER authoritative writers it must be serialized against:
// message admission (supersession) and handoff admission (subject drift),
// and against itself (two simultaneous resolutions).
//
// Every test asserts an INVARIANT that holds regardless of which goroutine's
// transaction the database schedules first - BEGIN IMMEDIATE (sqlite_store.go)
// decides that, not this test - rather than asserting a specific winner.
//
// #508 review P4a (mandatory Phase 1 follow-up, round 2): co-starting two
// goroutines at a single barrier, as the two tests above do, exercises REAL
// concurrency but does not FORCE the dangerous read-to-insert window open -
// the database is free to schedule the second transaction entirely before or
// entirely after the first, and a fast machine routinely does exactly that,
// never actually interleaving a write between a resolve's own read and its
// own insert. A first attempt at strengthening this held only the READ
// (findDecisionRequestTx) and then launched a SECOND, distinct writer after
// release without awaiting the first one's own outcome - proving the read
// transaction's own locking, never the actual resolution insert, and never
// confirming the originally-attempted contender did anything at all.
//
// The four tests below fix this: resolveUnderHeldTx replicates
// ResolveDecisionRequest's own four steps - find, check existing, validate,
// insert - using its own production helpers (decision_store.go), inside a
// transaction the TEST opens and controls the commit of. Holding that
// transaction open past its insert, but before its commit, is the actual
// window #508 means to prove nothing can interleave: a SINGLE contender is
// given a ready-to-attempt signal, proven blocked for a bound, then released
// by the SAME transaction's commit, and its own completion is awaited on the
// SAME channel afterward - never a second, duplicate writer, never a
// fire-and-forget drain. Each scenario is proved in BOTH serial orderings:
// resolution-first (this forced-interleaving test) and contender-first (a
// plain sequential companion test calling the REAL, unmodified
// ResolveDecisionRequest after the conflicting write has already fully
// landed, asserting the stale refusal).
//
// What this proves, precisely: SQLite's own write-lock promotion - acquired
// by BEGIN IMMEDIATE at BEGIN, or by the ordinary deferred rule at the first
// write statement otherwise, either way before resolveUnderHeldTx returns -
// blocks a second connection's conflicting write for as long as the holding
// transaction stays open, uncommitted. ResolveDecisionRequest relies on
// exactly that: its own read, validate and insert run inside ONE
// transaction, using these SAME helpers. A regression that split
// ResolveDecisionRequest's read from its insert across two SEPARATE
// transactions would not itself be caught by these tests, which hold their
// OWN transaction rather than instrumenting the production method directly
// (the narrower of the two options this review named as acceptable); it
// would be caught by TestResolveDecisionRequestLinearizesAgainstSupersession
// and ...SubjectDrift above, which race the REAL method and assert the
// outcome invariant regardless of ordering.

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// newLinearizabilityFixture anchors one REAL run (the ordinary #470
// orchestration path - no ticking needed, only the row must exist) so
// handoffs and messages can be admitted directly against it.
func newLinearizabilityFixture(t *testing.T) (*fleetFixture, orchestration.Batch, string) {
	t.Helper()
	fixture := newFleetFixture(t, 4)
	view := fixture.orchestrate(fixture.supervisor(), "claude", []int{fleetFirstIssue})
	batch, found, err := fixture.store.OrchestrationBatch(view.BatchID)
	if err != nil || !found {
		t.Fatalf("batch unreadable: found=%t err=%v", found, err)
	}
	return fixture, batch, view.Items[0].RunID
}

// buildTestHandoff and admitTestMessage are error-returning, NOT
// t.Fatal-calling: both run inside a racing goroutine in at least one test
// below, and t.Fatal/t.Fatalf must only ever be called from the goroutine
// running the test itself.
func buildTestHandoff(batch orchestration.Batch, runID, operationID, revision string, at time.Time) (orchestration.EngineeringHandoff, error) {
	id, err := orchestration.HandoffID(runID, operationID, 1)
	if err != nil {
		return orchestration.EngineeringHandoff{}, err
	}
	handoff := orchestration.EngineeringHandoff{
		SchemaVersion: orchestration.EngineeringHandoffSchemaVersion, ID: id, BatchID: batch.ID, Issue: fleetFirstIssue, RunID: runID,
		Producer:       orchestration.HandoffProducer{AgentID: "claude", OperationID: operationID, Attempt: 1},
		Subject:        orchestration.HandoffSubject{BaseRevision: "base", CandidateRevision: revision, CandidateTree: "tree-" + revision},
		Governance:     orchestration.HandoffGovernance{ContractID: "contract", ContractRevision: "1"},
		Observed:       orchestration.HandoffObserved{ChangedPathCount: 1, ChangedPathsDigest: "digest-" + revision},
		ReportSHA256:   "sha-" + operationID,
		ProducerReport: orchestration.HandoffReport{SchemaVersion: orchestration.HandoffSchemaVersion, Outcome: orchestration.OutcomeCompleted, Summary: "done"},
		AdmittedAt:     at,
	}
	return handoff, handoff.Validate()
}

func admitTestHandoff(t *testing.T, store *SQLiteOperationStore, batch orchestration.Batch, runID, operationID, revision string, at time.Time) orchestration.EngineeringHandoff {
	t.Helper()
	handoff, err := buildTestHandoff(batch, runID, operationID, revision, at)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err := store.AdmitHandoff(handoff)
	if err != nil || !inserted {
		t.Fatalf("admit handoff: inserted=%t err=%v", inserted, err)
	}
	return handoff
}

// admitTestHandoffNoFatal is admitTestHandoff's goroutine-safe twin.
func admitTestHandoffNoFatal(store *SQLiteOperationStore, batch orchestration.Batch, runID, operationID, revision string, at time.Time) error {
	handoff, err := buildTestHandoff(batch, runID, operationID, revision, at)
	if err != nil {
		return err
	}
	_, err = store.AdmitHandoff(handoff)
	return err
}

// admitTestMessage admits one draft (decision_request, or a supersession of
// one) through the real #473 admission path - orchestration.AdmitMessages,
// then the store - never a hand-built EngineeringMessage, so route, subject
// and provenance are exactly what a real worker invocation would produce.
func admitTestMessage(store *SQLiteOperationStore, batch orchestration.Batch, runID, operationID string, draft orchestration.MessageDraft, at time.Time) (orchestration.EngineeringMessage, error) {
	scope, _, err := batchMessageScope(store, batch)
	if err != nil {
		return orchestration.EngineeringMessage{}, err
	}
	source := orchestration.MessageSource{Unit: orchestration.BatchItemUnit(fleetFirstIssue), RunID: runID, AgentID: "claude", OperationID: operationID, Attempt: 1}
	admitted, err := orchestration.AdmitMessages(
		orchestration.MessageReport{SchemaVersion: orchestration.MessageSchemaVersion, Messages: []orchestration.MessageDraft{draft}},
		"sha-"+operationID, scope, source, at)
	if err != nil {
		return orchestration.EngineeringMessage{}, err
	}
	if err := store.AdmitMessages(admitted); err != nil {
		return orchestration.EngineeringMessage{}, err
	}
	return admitted[0], nil
}

func admitTestDecisionRequest(t *testing.T, store *SQLiteOperationStore, batch orchestration.Batch, runID, operationID string, subjectHandoff *orchestration.EngineeringHandoff, at time.Time) orchestration.EngineeringMessage {
	t.Helper()
	draft := orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "which way?", Body: "pick one"}
	if subjectHandoff != nil {
		draft.SubjectHandoff = subjectHandoff.ID
	}
	message, err := admitTestMessage(store, batch, runID, operationID, draft, at)
	if err != nil {
		t.Fatal(err)
	}
	return message
}

// TestResolveDecisionRequestLinearizesAgainstSupersession is #508 review P2
// scenario 1: a concurrent writer superseding the SAME request A is
// resolving must never let A commit a resolution for a request that was no
// longer live, and must never leave a half-written row behind when A
// correctly refuses.
func TestResolveDecisionRequestLinearizesAgainstSupersession(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	authority := testHoldAuthority("operator-1")

	var wg sync.WaitGroup
	start := make(chan struct{})
	var resolveErr, supersedeErr error
	var resolved orchestration.DecisionResolution
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		resolved, resolveErr = store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go ahead", authority, now)
	}()
	go func() {
		defer wg.Done()
		<-start
		_, supersedeErr = admitTestMessage(store, batch, runID, "op-2",
			orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "which way? (revised)", Body: "pick one, revised", Supersedes: d1.ID}, now)
	}()
	close(start)
	wg.Wait()
	if supersedeErr != nil {
		t.Fatal(supersedeErr)
	}

	stored, found, err := store.DecisionResolutionByRequestID(d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case resolveErr == nil:
		// A's transaction committed before B's supersession could land (or
		// observed it had not yet landed within A's own held snapshot): the
		// durable row must be exactly what A proposed.
		if !found || stored != resolved {
			t.Fatalf("resolve reported success but the durable row disagrees: found=%t stored=%+v resolved=%+v", found, stored, resolved)
		}
	case strings.Contains(resolveErr.Error(), "no longer live"):
		// B's supersession was the one A's own transaction observed: A
		// correctly refused, and left NOTHING durable - not a partial
		// resolution, not a wrong one.
		if found {
			t.Fatalf("a refused (superseded) resolve left a durable row anyway: %+v", stored)
		}
	default:
		t.Fatalf("resolve failed for an unexpected reason: %v", resolveErr)
	}
}

// TestResolveDecisionRequestLinearizesAgainstSubjectDrift is #508 review P2
// scenario 2: a concurrent SECOND handoff moving the owner's current subject
// must never let a subject-bound resolution commit against a subject that
// had already stopped being current.
func TestResolveDecisionRequestLinearizesAgainstSubjectDrift(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	h1 := admitTestHandoff(t, store, batch, runID, "op-1", "commit-1", now)
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-2", &h1, now)
	authority := testHoldAuthority("operator-1")

	var wg sync.WaitGroup
	start := make(chan struct{})
	var resolveErr, handoffErr error
	var resolved orchestration.DecisionResolution
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		resolved, resolveErr = store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go ahead", authority, now)
	}()
	go func() {
		defer wg.Done()
		<-start
		// A LATER admission time than h1's: queryRunHandoffs orders by it,
		// so "current" is unambiguous whichever transaction the database
		// happens to schedule first - this is a fact about admission order,
		// not about which goroutine's lock-acquisition wins.
		handoffErr = admitTestHandoffNoFatal(store, batch, runID, "op-3", "commit-2", now.Add(time.Second))
	}()
	close(start)
	wg.Wait()
	if handoffErr != nil {
		t.Fatal(handoffErr)
	}

	stored, found, err := store.DecisionResolutionByRequestID(d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch {
	case resolveErr == nil:
		// A committed against the subject that was current WITHIN its own
		// held transaction - which can only be commit-1, since commit-2 (if
		// it exists at all yet) was written by a transaction that could not
		// have started until A's finished.
		//
		// Compared field by field, never by struct equality: Subject is a
		// pointer, and two independently decoded copies of the identical
		// content are never == to one another.
		if !found || stored.RequestID != resolved.RequestID || stored.Outcome != resolved.Outcome ||
			stored.Subject == nil || stored.Subject.Revision.CandidateRevision != "commit-1" {
			t.Fatalf("resolve succeeded but bound a subject other than the one it validated against: found=%t stored=%+v", found, stored)
		}
	case strings.Contains(resolveErr.Error(), "no longer current"):
		if found {
			t.Fatalf("a refused stale-subject resolve left a durable row anyway: %+v", stored)
		}
	default:
		t.Fatalf("resolve failed for an unexpected reason: %v", resolveErr)
	}
}

// resolveUnderHeldTx replicates ResolveDecisionRequest's own four steps -
// find the live request, read any existing resolution, validate, insert -
// using its own production helpers (decision_store.go), inside a
// transaction the CALLER opened and controls the commit of. It exists so a
// test can hold the exact window between a resolve's own insert and its own
// commit open for an externally observed duration.
func resolveUnderHeldTx(tx *sql.Tx, decisionID string, outcome orchestration.DecisionOutcome, reason string,
	authority orchestration.DecisionResolutionAuthority, now time.Time) (orchestration.DecisionResolution, error) {
	ref, current, err := findDecisionRequestTx(tx, decisionID)
	if err != nil {
		return orchestration.DecisionResolution{}, err
	}
	existing, found, err := decisionResolutionByRequestID(tx, ref.ID)
	if err != nil {
		return orchestration.DecisionResolution{}, err
	}
	var existingPtr *orchestration.DecisionResolution
	if found {
		existingPtr = &existing
	}
	proposed, err := orchestration.ResolveDecision(ref, current, outcome, reason, authority, existingPtr, now)
	if err != nil {
		return orchestration.DecisionResolution{}, err
	}
	stored, _, err := insertDecisionResolution(tx, proposed)
	return stored, err
}

// TestResolveDecisionRequestResolutionCommitsBeforeAConcurrentSupersession is
// #508 review P4a's T1 fix, the RESOLUTION-FIRST ordering: the resolution's
// own read-validate-insert is held open (resolveUnderHeldTx) past its
// insert, a SINGLE contender's supersession is given a ready-to-attempt
// signal and proven blocked for a bound, the transaction then commits, and
// the SAME contender's own completion is awaited afterward on the SAME
// channel - never a second, duplicate writer, never a fire-and-forget drain.
// The supersession can only ever land AFTER the resolution has already
// committed, so it has no intervening effect on it.
func TestResolveDecisionRequestResolutionCommitsBeforeAConcurrentSupersession(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	second, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	authority := testHoldAuthority("operator-1")

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveUnderHeldTx(tx, d1.ID, allowOutcomeForStore(), "go ahead", authority, now)
	if err != nil {
		t.Fatal(err)
	}
	// tx now holds the write lock BEGIN IMMEDIATE acquires, with the
	// resolution's own INSERT already executed but not yet committed -
	// exactly the window between ResolveDecisionRequest's own insert and its
	// commit.
	ready := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		close(ready) // READY-TO-ATTEMPT: about to issue the conflicting write, now.
		_, err := admitTestMessage(second, batch, runID, "op-2",
			orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "pick one, revised", Supersedes: d1.ID}, now)
		completed <- err
	}()
	<-ready
	select {
	case err := <-completed:
		t.Fatalf("the ONE contender's supersession landed while the resolution's own transaction was still open (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
		// still blocked, as required.
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select { // awaited AFTER release, on the SAME channel.
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the contender's supersession never completed after the resolution's transaction committed")
	}

	stored, found, err := store.DecisionResolutionByRequestID(d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || stored != resolved {
		t.Fatalf("the resolution did not stand exactly as committed: found=%t stored=%+v resolved=%+v", found, stored, resolved)
	}
}

// TestResolveDecisionRequestRefusesAfterASupersessionAlreadyLanded is the
// opposite, CONTENDER-FIRST ordering: a supersession that has already fully
// landed before ResolveDecisionRequest is ever called makes the real,
// unmodified method refuse the request as no longer live, durably writing
// nothing. No concurrency is needed to prove this ordering; it is the
// method's own validation, exercised after the fact.
func TestResolveDecisionRequestRefusesAfterASupersessionAlreadyLanded(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	if _, err := admitTestMessage(store, batch, runID, "op-2",
		orchestration.MessageDraft{Kind: orchestration.KindDecisionRequest, Purpose: "revised", Body: "pick one, revised", Supersedes: d1.ID}, now); err != nil {
		t.Fatal(err)
	}

	_, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go ahead", testHoldAuthority("operator-1"), now)
	if err == nil || !strings.Contains(err.Error(), "no longer live") {
		t.Fatalf("resolving a request superseded before the call must refuse as no longer live, got: %v", err)
	}
	if _, found, err := store.DecisionResolutionByRequestID(d1.ID); err != nil || found {
		t.Fatalf("a refused (superseded) resolve must leave no durable row: found=%t err=%v", found, err)
	}
}

// TestResolveDecisionRequestResolutionCommitsBeforeAConcurrentSubjectDrift is
// the subject-drift companion of the supersession test above: the same
// forced, held-transaction proof that a second handoff attempted during the
// resolution's own transaction cannot land until it commits.
func TestResolveDecisionRequestResolutionCommitsBeforeAConcurrentSubjectDrift(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	second, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	now := fixture.clock.Now()
	h1 := admitTestHandoff(t, store, batch, runID, "op-1", "commit-1", now)
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-2", &h1, now)
	authority := testHoldAuthority("operator-1")

	tx, err := store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveUnderHeldTx(tx, d1.ID, allowOutcomeForStore(), "go ahead", authority, now)
	if err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	completed := make(chan error, 1)
	go func() {
		close(ready)
		completed <- admitTestHandoffNoFatal(second, batch, runID, "op-3", "commit-2", now.Add(time.Second))
	}()
	<-ready
	select {
	case err := <-completed:
		t.Fatalf("the ONE contender's second handoff landed while the resolution's own transaction was still open (err=%v)", err)
	case <-time.After(150 * time.Millisecond):
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second handoff never completed after the resolution's transaction committed")
	}

	stored, found, err := store.DecisionResolutionByRequestID(d1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found || stored.RequestID != resolved.RequestID || stored.Outcome != resolved.Outcome ||
		stored.Subject == nil || stored.Subject.Revision.CandidateRevision != "commit-1" {
		t.Fatalf("the resolution did not stand bound to the subject it validated against: found=%t stored=%+v resolved=%+v", found, stored, resolved)
	}
}

// TestResolveDecisionRequestRefusesAfterSubjectDriftAlreadyLanded is the
// opposite, CONTENDER-FIRST ordering: a second handoff that has already
// moved the owner's subject before ResolveDecisionRequest is ever called
// makes the real, unmodified method refuse as no longer current.
func TestResolveDecisionRequestRefusesAfterSubjectDriftAlreadyLanded(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	h1 := admitTestHandoff(t, store, batch, runID, "op-1", "commit-1", now)
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-2", &h1, now)
	admitTestHandoff(t, store, batch, runID, "op-3", "commit-2", now.Add(time.Second))

	_, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go ahead", testHoldAuthority("operator-1"), now)
	if err == nil || !strings.Contains(err.Error(), "no longer current") {
		t.Fatalf("resolving a request whose subject already moved must refuse as no longer current, got: %v", err)
	}
	if _, found, err := store.DecisionResolutionByRequestID(d1.ID); err != nil || found {
		t.Fatalf("a refused (stale-subject) resolve must leave no durable row: found=%t err=%v", found, err)
	}
}

// TestResolveDecisionRequestConcurrentIdenticalAnswersAreIdempotent is #508
// review P2 scenario 3: two authorized operators racing with the identical
// answer both succeed, and exactly one durable resolution exists.
func TestResolveDecisionRequestConcurrentIdenticalAnswersAreIdempotent(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	outcome := allowOutcomeForStore()

	var wg sync.WaitGroup
	start := make(chan struct{})
	var results [2]orchestration.DecisionResolution
	var errs [2]error
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.ResolveDecisionRequest(d1.ID, outcome, "go ahead", testHoldAuthority("operator-"+strconv.Itoa(i+1)), now)
		}(i)
	}
	close(start)
	wg.Wait()

	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("two identical concurrent resolutions must both succeed: %v / %v", errs[0], errs[1])
	}
	if results[0] != results[1] {
		t.Fatalf("two identical concurrent resolutions produced different durable results: %+v vs %+v", results[0], results[1])
	}
	rows := countDecisionResolutionRows(t, store, d1.ID)
	if rows != 1 {
		t.Fatalf("expected exactly one durable resolution row, got %d", rows)
	}
}

// TestResolveDecisionRequestConcurrentConflictingAnswersRefuseOne is #508
// review P2 scenario 4: two operators racing with CONFLICTING answers leave
// exactly one durable resolution and refuse the other, whichever loses.
func TestResolveDecisionRequestConcurrentConflictingAnswersRefuseOne(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	outcomes := [2]orchestration.DecisionOutcome{allowOutcomeForStore(), denyOutcomeForStore()}

	var wg sync.WaitGroup
	start := make(chan struct{})
	var results [2]orchestration.DecisionResolution
	var errs [2]error
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.ResolveDecisionRequest(d1.ID, outcomes[i], "", testHoldAuthority("operator-1"), now)
		}(i)
	}
	close(start)
	wg.Wait()

	oneEach := (errs[0] == nil) != (errs[1] == nil)
	if !oneEach {
		t.Fatalf("expected exactly one of two conflicting resolutions to succeed: err0=%v err1=%v", errs[0], errs[1])
	}
	winner := results[0]
	if errs[0] != nil {
		winner = results[1]
	}
	stored, found, err := store.DecisionResolutionByRequestID(d1.ID)
	if err != nil || !found {
		t.Fatalf("no durable resolution exists after the race: found=%t err=%v", found, err)
	}
	if stored != winner {
		t.Fatalf("durable row does not match the winning resolution: stored=%+v winner=%+v", stored, winner)
	}
	if rows := countDecisionResolutionRows(t, store, d1.ID); rows != 1 {
		t.Fatalf("expected exactly one durable resolution row, got %d", rows)
	}
}

// TestResolveDecisionRequestAtomicPathSurvivesRestart is #508 review P2
// scenario 5, through the governed atomic path specifically (not the
// standalone InsertDecisionResolution already proved by
// TestDecisionResolutionSurvivesRestart).
func TestResolveDecisionRequestAtomicPathSurvivesRestart(t *testing.T) {
	fixture, batch, runID := newLinearizabilityFixture(t)
	store := fixture.store
	now := fixture.clock.Now()
	d1 := admitTestDecisionRequest(t, store, batch, runID, "op-1", nil, now)
	resolved, err := store.ResolveDecisionRequest(d1.ID, allowOutcomeForStore(), "go ahead", testHoldAuthority("operator-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	stored, found, err := reopened.DecisionResolutionByRequestID(d1.ID)
	if err != nil || !found || stored != resolved {
		t.Fatalf("resolution did not survive restart: found=%t err=%v stored=%+v resolved=%+v", found, err, stored, resolved)
	}
}

func countDecisionResolutionRows(t *testing.T, store *SQLiteOperationStore, requestID string) int {
	t.Helper()
	row := store.db.QueryRow(`SELECT COUNT(*) FROM decision_resolutions WHERE request_id = ?`, requestID)
	var n int
	if err := row.Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
