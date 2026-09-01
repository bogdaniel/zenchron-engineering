package runtime

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// projectionEvent builds one unlinked event. Project folds an ordered slice; it
// does not verify the hash chain, which is Reduce's job, so the fixtures here
// carry only the fields the projection reads.
func projectionEvent(t *testing.T, sequence int64, eventType string, payload any) EngineeringEvent {
	t.Helper()
	e := EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e", RunID: "r", Sequence: sequence,
		Type: eventType, OccurredAt: time.Unix(100+sequence, 0).UTC()}
	if payload != nil {
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		e.Payload = raw
	}
	return e
}

// projectionRun is one run's worth of Phase 8 history: a contract, a producer
// change committed to a candidate, a rebase onto a moved base, assurance, a
// pull request, CI, a review, a reassessment, and an authority decision.
func projectionRun(t *testing.T) []EngineeringEvent {
	t.Helper()
	return []EngineeringEvent{
		projectionEvent(t, 1, EventRunCreated, nil),
		projectionEvent(t, 2, EventContractCompiled, ContractCompiledPayload{
			Contract: Ref{"contract-1", "rev-1"},
			Subject:  domain.Subject{Repository: "o/r", Revision: "base-1"}}),
		projectionEvent(t, 3, EventCandidateChanged, CandidateChangedPayload{
			ProducerID: "codex", Purpose: InvocationInitial, Outcome: Succeeded}),
		projectionEvent(t, 4, EventCandidateCommitted, CandidateCommittedPayload{
			Commit: "commit-1", Tree: "tree-1", PathCount: 2, PathsDigest: "paths-1"}),
		projectionEvent(t, 5, EventCandidateBaseIntegrated, CandidateBaseIntegratedPayload{
			Strategy: "rebase", BaseRevision: "base-2", Commit: "commit-2", Tree: "tree-2"}),
		projectionEvent(t, 6, EventReassessmentCompleted, ReassessmentCompletedPayload{
			Material: true, Contract: Ref{"contract-1", "rev-2"},
			DeviationKinds: []string{"scope_expansion"}, RequestedPrivilegeCount: 1}),
		projectionEvent(t, 7, EventAssuranceObserved, AssuranceObservedPayload{
			ProviderID: "go", VerifierDefinition: "verifier-1", Passed: true,
			Commit: "commit-2", Tree: "tree-2", Bundle: Ref{"bundle-1", "rev-1"}}),
		projectionEvent(t, 8, EventGitHubPRObserved, GitHubPRObservedPayload{
			Number: 7, HeadRevision: "commit-2", BaseRevision: "base-2", State: "open"}),
		projectionEvent(t, 9, EventGitHubCIObserved, GitHubCIObservedPayload{
			HeadRevision: "commit-2", Conclusion: "failure", CheckCount: 3, FailingChecks: []string{"vet"}}),
		projectionEvent(t, 10, EventGitHubReviewObserved, GitHubReviewObservedPayload{
			HeadRevision: "commit-2", State: "changes_requested", FindingCount: 2}),
		projectionEvent(t, 11, EventAuthorityEvaluated, AuthorityEvaluatedPayload{
			Decision: Ref{"decision-1", "rev-1"},
			Action:   domain.Action{Type: "publish_pull_request", Target: "o/r"},
			Status:   domain.AuthorityAwaitingAuthority}),
		projectionEvent(t, 12, EventOperationBefore, RunOperation{SchemaVersion: SchemaVersion,
			ID: "op-1", RunID: "r", Kind: "provider", IdempotencyKey: "a", State: Running, MaxAttempts: 3}),
		projectionEvent(t, 13, EventOperationBefore, RunOperation{SchemaVersion: SchemaVersion,
			ID: "op-2", RunID: "r", Kind: "provider", IdempotencyKey: "b", State: Running, MaxAttempts: 3}),
		projectionEvent(t, 14, EventOperationBefore, RunOperation{SchemaVersion: SchemaVersion,
			ID: "op-3", RunID: "r", Kind: "assurance", IdempotencyKey: "c", State: Running, MaxAttempts: 3}),
	}
}

func TestProjectFoldsARunToItsDomainView(t *testing.T) {
	projection, err := Project(projectionRun(t))
	if err != nil {
		t.Fatal(err)
	}
	// The reassessment's resulting contract revision supersedes the compiled one.
	if projection.Contract != (Ref{"contract-1", "rev-2"}) {
		t.Fatalf("contract is %+v", projection.Contract)
	}
	if projection.Subject != (domain.Subject{Repository: "o/r", Revision: "base-1"}) {
		t.Fatalf("subject is %+v", projection.Subject)
	}
	// The rebase, not the first commit, is the candidate the run now carries.
	if projection.CandidateRevision != "commit-2" || projection.CandidateTree != "tree-2" || projection.BaseRevision != "base-2" {
		t.Fatalf("candidate is %q/%q on base %q", projection.CandidateRevision, projection.CandidateTree, projection.BaseRevision)
	}
	if projection.Head() != "commit-2" {
		t.Fatalf("head is %q", projection.Head())
	}
	if projection.SourceIntentChanged || projection.ObservedExternalHead != "" {
		t.Fatalf("nothing changed the source or the external head: %+v", projection)
	}
	if projection.Reassessment == nil || !projection.Reassessment.Material || projection.Reassessment.RequestedPrivilegeCount != 1 {
		t.Fatalf("reassessment is %+v", projection.Reassessment)
	}
	if len(projection.EvidenceBundles) != 1 || projection.EvidenceBundles[0] != (Ref{"bundle-1", "rev-1"}) {
		t.Fatalf("evidence bundles are %+v", projection.EvidenceBundles)
	}
	decision, ok := projection.AuthorityDecisions["publish_pull_request\x00o/r"]
	if !ok || decision.Status != domain.AuthorityAwaitingAuthority || decision.Decision != (Ref{"decision-1", "rev-1"}) {
		t.Fatalf("authority decisions are %+v", projection.AuthorityDecisions)
	}
	if projection.PullRequest == nil || projection.PullRequest.Number != 7 || projection.PullRequest.Merged {
		t.Fatalf("pull request is %+v", projection.PullRequest)
	}
	if projection.Assurance == nil || !projection.Assurance.Passed || projection.Assurance.Commit != "commit-2" {
		t.Fatalf("assurance is %+v", projection.Assurance)
	}
	if projection.CI == nil || projection.CI.Conclusion != "failure" || len(projection.CI.FailingChecks) != 1 {
		t.Fatalf("ci is %+v", projection.CI)
	}
	if projection.Review == nil || projection.Review.FindingCount != 2 {
		t.Fatalf("review is %+v", projection.Review)
	}
	for _, observation := range []Observation{projection.PullRequest.Observation, projection.Assurance.Observation,
		projection.CI.Observation, projection.Review.Observation} {
		if observation.Stale {
			t.Fatalf("an observation for the current head is marked stale: %+v", observation)
		}
	}
	if projection.Attempts["provider"] != 2 || projection.Attempts["assurance"] != 1 {
		t.Fatalf("attempts are %+v", projection.Attempts)
	}
}

// TestProjectIsDeterministic folds the same events repeatedly and compares the
// canonical bytes, so an iteration-order dependency in the maps or a retained
// pointer into a previous fold cannot pass unnoticed.
func TestProjectIsDeterministic(t *testing.T) {
	events := projectionRun(t)
	first, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalJSON(first)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		again, err := Project(events)
		if err != nil {
			t.Fatal(err)
		}
		got, err := CanonicalJSON(again)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Fatalf("fold %d produced different canonical bytes:\n%s\n%s", i, want, got)
		}
	}
}

// TestProjectMarksSupersededHeadObservationsStale is the reconciler's guarantee:
// a finding recorded against a head the run has moved past is representable as
// stale, and it never displaces the finding for the head the run is on.
func TestProjectMarksSupersededHeadObservationsStale(t *testing.T) {
	base := projectionRun(t)
	t.Run("an observation the candidate moved past is stale", func(t *testing.T) {
		events := append(append([]EngineeringEvent{}, base...),
			projectionEvent(t, 15, EventCandidateCommitted, CandidateCommittedPayload{
				Commit: "commit-3", Tree: "tree-3", PathCount: 1, PathsDigest: "paths-3"}))
		projection, err := Project(events)
		if err != nil {
			t.Fatal(err)
		}
		if projection.Head() != "commit-3" {
			t.Fatalf("head is %q", projection.Head())
		}
		for name, stale := range map[string]bool{
			"pull request": projection.PullRequest.Stale,
			"assurance":    projection.Assurance.Stale,
			"ci":           projection.CI.Stale,
			"review":       projection.Review.Stale,
		} {
			if !stale {
				t.Fatalf("the %s observation is for commit-2 but is not marked stale", name)
			}
		}
	})
	t.Run("a stale-head observation does not overwrite the current head", func(t *testing.T) {
		// A late CI result for the pre-rebase commit arrives after the current-head
		// result. Believing it would report a green run on a commit that no longer
		// exists, so the current-head observation must survive it.
		events := append(append([]EngineeringEvent{}, base...),
			projectionEvent(t, 15, EventGitHubCIObserved, GitHubCIObservedPayload{
				HeadRevision: "commit-1", Conclusion: "success", CheckCount: 3}),
			projectionEvent(t, 16, EventGitHubReviewObserved, GitHubReviewObservedPayload{
				HeadRevision: "commit-1", State: "approved", FindingCount: 0}),
			projectionEvent(t, 17, EventAssuranceObserved, AssuranceObservedPayload{
				ProviderID: "go", VerifierDefinition: "verifier-1", Passed: false,
				FailureClass: FailureCompileTest, Commit: "commit-1", Tree: "tree-1"}),
			projectionEvent(t, 18, EventGitHubPRObserved, GitHubPRObservedPayload{
				Number: 7, HeadRevision: "commit-1", BaseRevision: "base-2", State: "open", Merged: true}))
		projection, err := Project(events)
		if err != nil {
			t.Fatal(err)
		}
		if projection.CI.HeadRevision != "commit-2" || projection.CI.Conclusion != "failure" || projection.CI.Stale {
			t.Fatalf("a stale CI result overwrote the current head: %+v", projection.CI)
		}
		if projection.Review.HeadRevision != "commit-2" || projection.Review.State != "changes_requested" || projection.Review.Stale {
			t.Fatalf("a stale review overwrote the current head: %+v", projection.Review)
		}
		if projection.Assurance.Commit != "commit-2" || !projection.Assurance.Passed || projection.Assurance.Stale {
			t.Fatalf("a stale assurance result overwrote the current head: %+v", projection.Assurance)
		}
		if projection.PullRequest.HeadRevision != "commit-2" || projection.PullRequest.Merged || projection.PullRequest.Stale {
			t.Fatalf("a stale pull request observation overwrote the current head: %+v", projection.PullRequest)
		}
	})
	t.Run("a current-head observation still wins latest", func(t *testing.T) {
		events := append(append([]EngineeringEvent{}, base...),
			projectionEvent(t, 15, EventGitHubCIObserved, GitHubCIObservedPayload{
				HeadRevision: "commit-2", Conclusion: "success", CheckCount: 3}))
		projection, err := Project(events)
		if err != nil {
			t.Fatal(err)
		}
		if projection.CI.Conclusion != "success" || projection.CI.Sequence != 15 || projection.CI.Stale {
			t.Fatalf("latest-wins did not apply to a current-head observation: %+v", projection.CI)
		}
	})
}

// TestProjectFromReplayEqualsInMemory is the single-source-of-truth check: the
// projection is a read of the journal, so persisting the events and replaying
// them must produce exactly the projection the same events produced in memory.
func TestProjectFromReplayEqualsInMemory(t *testing.T) {
	_, store := openJournal(t)
	appended := []EngineeringEvent{}
	for i, e := range projectionRun(t) {
		e.ID, e.Sequence = eventID(i), 0
		stored, err := store.AppendEvent(e)
		if err != nil {
			t.Fatalf("event %d (%s): %v", i, e.Type, err)
		}
		appended = append(appended, stored)
	}
	inMemory, err := Project(appended)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := Project(persisted)
	if err != nil {
		t.Fatal(err)
	}
	want, err := CanonicalJSON(inMemory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalJSON(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("the replayed projection differs from the in-memory one:\n%s\n%s", want, got)
	}
	// Reduce still owns the state hash; the projection neither produces nor
	// contradicts one.
	if _, err := store.Replay("r"); err != nil {
		t.Fatal(err)
	}
}

func eventID(i int) string { return "e-" + string(rune('a'+i)) }
