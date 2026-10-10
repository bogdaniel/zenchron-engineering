package runtime

// #474 B2: reviewRemediationLiveHeadCheck is the one point before dispatch
// that asks GitHub itself rather than the run's own last-observed
// projection. These tests move the FakeGitHubAdapter's stored PR directly -
// simulating an external push - without touching the run's journalled
// projection or performing any observation tick, exactly the gap B2 closes.

import (
	"context"
	"strings"
	"testing"
)

// newReviewRemediationLiveHeadFixture folds admissionFixture's own seeded
// run into a *runState the same way TestConditionsSurfacesAnUnreadableReview-
// RemediationTable does: a direct construction that bypasses
// (*EngineeringRuntime).load's unrelated controller-succession checks, with
// a runtime whose GitHub dependency is the fixture's own FakeGitHubAdapter
// (admissionFixture.runtime() leaves GitHub unset, since most of that
// fixture's tests never need a live read).
func newReviewRemediationLiveHeadFixture(t *testing.T) (*admissionFixture, *runState) {
	t.Helper()
	f := newAdmissionFixture(t)
	run, found, err := f.store.Run(f.runID)
	if err != nil || !found {
		t.Fatal(err)
	}
	events, err := f.store.Events(f.runID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Project(events)
	if err != nil {
		t.Fatal(err)
	}
	rt := &EngineeringRuntime{deps: Dependencies{Store: f.store, Clock: f.clock, GitHub: f.fake}}
	return f, &runState{rt: rt, run: run, snapshot: snapshot, events: events, projection: projection}
}

func TestReviewRemediationLiveHeadCheckAgreesWithTheRunsOwnProjection(t *testing.T) {
	_, state := newReviewRemediationLiveHeadFixture(t)
	findings := []Finding{{Signature: "review-remediation:f1"}}
	if err := state.rt.reviewRemediationLiveHeadCheck(context.Background(), state, findings); err != nil {
		t.Fatalf("expected the live head to agree with the run's own just-observed PR: %v", err)
	}
}

func TestReviewRemediationLiveHeadCheckSkipsWithNoFindingsDelivered(t *testing.T) {
	f, state := newReviewRemediationLiveHeadFixture(t)
	// Would disagree with state.projection.Head() if the live head were
	// consulted at all - proving the empty-findings guard genuinely skips
	// the call rather than happening to agree.
	f.fake.PullRequests[admissionTestPRNumber] = GitHubPullRequest{}
	if err := state.rt.reviewRemediationLiveHeadCheck(context.Background(), state, nil); err != nil {
		t.Fatalf("expected no findings to skip the live check entirely, got %v", err)
	}
}

// TestReviewRemediationLiveHeadCheckCatchesAnExternalMoveWithoutAnObservationTick
// is #474 B2's required regression: the pull request's forge-side head
// moves AFTER this run's own journalled projection was built, and BEFORE any
// observation tick folds that move into the projection bindExecutionInvoke
// and reviewRemediationFindings both read. Only a live read, taken at this
// exact point before dispatch, can see it.
func TestReviewRemediationLiveHeadCheckCatchesAnExternalMoveWithoutAnObservationTick(t *testing.T) {
	f, state := newReviewRemediationLiveHeadFixture(t)
	findings := []Finding{{Signature: "review-remediation:f1"}}

	external := strings.Repeat("e", 40)
	pr := f.fake.PullRequests[admissionTestPRNumber]
	pr.HeadSHA = external
	f.fake.PullRequests[admissionTestPRNumber] = pr

	if state.projection.Head() == external {
		t.Fatal("test invariant broken: the run's own journalled projection must stay exactly as it was - this is the no-observation-tick scenario")
	}
	if err := state.rt.reviewRemediationLiveHeadCheck(context.Background(), state, findings); err == nil {
		t.Fatal("expected the live head check to refuse once the PR moved externally without an observation tick")
	}
}

func TestReviewRemediationLiveHeadCheckRefusesWhenGitHubIsUnavailable(t *testing.T) {
	f, state := newReviewRemediationLiveHeadFixture(t)
	findings := []Finding{{Signature: "review-remediation:f1"}}
	f.fake.Fail = func(GitHubCall) error { return context.DeadlineExceeded }

	if err := state.rt.reviewRemediationLiveHeadCheck(context.Background(), state, findings); err == nil {
		t.Fatal("expected unavailable external truth to refuse rather than silently proceed")
	}
}
