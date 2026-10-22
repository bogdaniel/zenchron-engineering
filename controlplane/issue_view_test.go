package controlplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func issueBody(repository, title, description string) string {
	return fmt.Sprintf(`{"controller_binding":%q,"repository":%q,"title":%q,"description":%q}`, testBinding, repository, title, description)
}

// TestIssueCreateSendsTheOperatorsTextToTheController pins that the browser
// action carries exactly the typed title and description, and nothing else, as
// one governed issue-create command.
func TestIssueCreateSendsTheOperatorsTextToTheController(t *testing.T) {
	f := newProductActionFixture(t)
	var sent []rt.ControlRequest
	f.api.Send = func(request rt.ControlRequest) (rt.ControlResponse, error) {
		sent = append(sent, request)
		payload, _ := json.Marshal(map[string]any{"repository": "acme/repo", "number": 12, "url": "https://github.com/acme/repo/issues/12"})
		return rt.ControlResponse{OK: true, Payload: payload}, nil
	}
	result := post(t, f.api, validRequest("/v1/issues", issueBody("acme/repo", "Add CSV export", "Export orders as CSV.")), 200)
	if outcome(result) != "applied" || result["action"] != "issue-create" {
		t.Fatalf("result: %+v", result)
	}
	if len(sent) != 1 || sent[0].Command != rt.ControlIssueCreate || sent[0].Repository != "acme/repo" ||
		sent[0].NewIssue == nil || sent[0].NewIssue.Title != "Add CSV export" || sent[0].NewIssue.Body != "Export orders as CSV." {
		t.Fatalf("controller received %+v", sent)
	}
}

// TestIssueCreateValidatesBeforeTheSocket: a draft the controller would refuse
// never reaches the socket.
func TestIssueCreateValidatesBeforeTheSocket(t *testing.T) {
	f := newProductActionFixture(t)
	f.api.Send = func(rt.ControlRequest) (rt.ControlResponse, error) {
		t.Fatal("an invalid draft reached the controller")
		return rt.ControlResponse{}, nil
	}
	for name, body := range map[string]string{
		"blank title":        issueBody("acme/repo", "  ", "text"),
		"blank description":  issueBody("acme/repo", "title", ""),
		"no repository":      issueBody("", "title", "text"),
		"multi-line title":   issueBody("acme/repo", "a\nb", "text"),
		"oversized title":    issueBody("acme/repo", strings.Repeat("t", rt.MaxNewIssueTitleBytes+1), "text"),
		"oversized document": issueBody("acme/repo", "title", strings.Repeat("d", rt.MaxNewIssueBodyBytes+1)),
	} {
		t.Run(name, func(t *testing.T) { post(t, f.api, validRequest("/v1/issues", body), 400) })
	}
}
