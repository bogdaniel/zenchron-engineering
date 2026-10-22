package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// recordingForge is the forge boundary: it records what issue-create asked it
// to open. The embedded nil adapter makes any other forge call fail loudly.
type recordingForge struct {
	runtime.GitHubAdapter
	created []struct{ repo, title, body string }
}

func (f *recordingForge) CreateIssue(_ context.Context, repo runtime.GitHubRepo, title, body string) (runtime.GitHubIssue, error) {
	f.created = append(f.created, struct{ repo, title, body string }{repo.String(), title, body})
	return runtime.GitHubIssue{Number: 41, URL: "https://github.com/acme/repo/issues/41"}, nil
}

func issueCreateRequest(repository, title, body, opID, digest string) runtime.ControlRequest {
	return runtime.ControlRequest{
		Command: runtime.ControlIssueCreate, Repository: repository, OperationID: opID, RequestDigest: digest,
		ExpectedController: "test-binding", NewIssue: &runtime.NewIssue{Title: title, Body: body},
	}
}

// TestIssueCreateOpensOneIssueWithProvenanceAndAReceiptTheResendCannotRepeat is
// the whole contract: the issue is opened once on a governed repository, its
// body names the operator, the receipt records the exact issue number, and a
// resend of the same operation reference does not open a second issue.
func TestIssueCreateOpensOneIssueWithProvenanceAndAReceiptTheResendCannotRepeat(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	forge := &recordingForge{}
	c.forge = forge
	c.config.Operator.ID = "alice"
	supervisor := newTestSupervisor(t, c)
	const opID = "01234567-89ab-4cde-8fab-0123456789ab"
	const digest = "4444444444444444444444444444444444444444444444444444444444444444"

	first := c.dispatchArtifactWithReceipt(context.Background(), supervisor, func() {}, issueCreateRequest("acme/repo", "Add CSV export", "Export orders.", opID, digest))
	if !first.OK {
		t.Fatalf("first attempt: %+v", first)
	}
	var created createdIssue
	if err := json.Unmarshal(first.Payload, &created); err != nil || created.Number != 41 {
		t.Fatalf("payload: %s %v", first.Payload, err)
	}
	if len(forge.created) != 1 || forge.created[0].repo != "acme/repo" || forge.created[0].title != "Add CSV export" ||
		!strings.HasPrefix(forge.created[0].body, "Export orders.") || !strings.Contains(forge.created[0].body, "Created from the Zenchron dashboard by operator `alice`") {
		t.Fatalf("forge was asked for %+v", forge.created)
	}

	resend := c.dispatchArtifactWithReceipt(context.Background(), supervisor, func() {}, issueCreateRequest("acme/repo", "Add CSV export", "Export orders.", opID, digest))
	if resend.OK || resend.Code != runtime.ControlCodeOutcomeUnknown || len(forge.created) != 1 {
		t.Fatalf("a resent reference must not open a second issue: %+v created=%d", resend, len(forge.created))
	}
	receipt, err := c.store.ArtifactActionReceipt(opID)
	if err != nil || receipt == nil || receipt.Repository != "acme/repo" || receipt.SourceIssue != 41 {
		t.Fatalf("receipt must name the exact issue: %+v %v", receipt, err)
	}
}

// TestIssueCreateRefusesWhatItMustNotOpen: an ungoverned repository, a blank
// draft and a missing forge capability each open nothing, and the refusal
// releases the claim so a corrected retry under the same reference can succeed.
func TestIssueCreateRefusesWhatItMustNotOpen(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	forge := &recordingForge{}
	c.forge = forge
	c.config.Operator.ID = "alice"
	supervisor := newTestSupervisor(t, c)
	const opID = "01234567-89ab-4cde-8fab-0123456789ac"
	const digest = "5555555555555555555555555555555555555555555555555555555555555555"
	dispatch := func(request runtime.ControlRequest) runtime.ControlResponse {
		return c.dispatchArtifactWithReceipt(context.Background(), supervisor, func() {}, request)
	}

	for name, request := range map[string]runtime.ControlRequest{
		"ungoverned repository": issueCreateRequest("other/repo", "t", "b", opID, digest),
		"blank description":     issueCreateRequest("acme/repo", "t", " ", opID, digest),
	} {
		if response := dispatch(request); response.OK {
			t.Fatalf("%s was not refused: %+v", name, response)
		}
	}
	if len(forge.created) != 0 {
		t.Fatalf("a refused request opened %+v", forge.created)
	}
	if retry := dispatch(issueCreateRequest("acme/repo", "t", "b", opID, digest)); !retry.OK {
		t.Fatalf("a corrected retry under the same reference must succeed: %+v", retry)
	}

	c.forge = runtime.NewFakeGitHubAdapter() // no CreateIssue
	if response := dispatch(issueCreateRequest("acme/repo", "t", "b", "01234567-89ab-4cde-8fab-0123456789ad", digest)); response.OK {
		t.Fatalf("a forge that cannot create issues must be refused: %+v", response)
	}
}

// TestIssueCreateIsRefusedOutsideTheGovernedPath: the local socket never
// carries the receipt that makes a lost reply recoverable, so it never opens one.
func TestIssueCreateIsRefusedOutsideTheGovernedPath(t *testing.T) {
	c := newArtifactReceiptComposition(t)
	forge := &recordingForge{}
	c.forge = forge
	supervisor := newTestSupervisor(t, c)
	response := c.createIssue(context.Background(), supervisor, issueCreateRequest("acme/repo", "t", "b", "", ""), false)
	if response.OK || len(forge.created) != 0 {
		t.Fatalf("ungoverned issue-create: %+v created=%d", response, len(forge.created))
	}
}
