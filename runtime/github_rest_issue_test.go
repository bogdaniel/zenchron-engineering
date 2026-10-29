package runtime

import (
	"context"
	"strings"
	"testing"
)

func TestGitHubRESTAdapterCreateIssueShapesItsRequest(t *testing.T) {
	responses := restResponses()
	responses["POST /repos/zenchron/fixture/issues"] = `{"number":77,"html_url":"https://github.com/zenchron/fixture/issues/77","title":"Add export","body":"b","state":"open","user":{"login":"op","id":1},"labels":[]}`
	doer := &fakeGitHubDoer{responses: responses}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: staticCredential{secret: testToken}}

	created, err := adapter.CreateIssue(context.Background(), testRepo, "Add export", "b")
	if err != nil || created.Number != 77 {
		t.Fatalf("CreateIssue: %+v %v", created, err)
	}
	got := doer.requests[0]
	if got.Method != "POST" || got.URL != "https://api.github.com/repos/zenchron/fixture/issues" || !strings.Contains(got.Body, `"title":"Add export"`) {
		t.Fatalf("request: %+v", got)
	}
}

func TestGitHubRESTAdapterCreateIssueRefusesABlankTitleBeforeAnyRequest(t *testing.T) {
	doer := &fakeGitHubDoer{responses: map[string]string{}}
	adapter := GitHubRESTAdapter{HTTP: doer, Credentials: staticCredential{secret: testToken}}
	if _, err := adapter.CreateIssue(context.Background(), testRepo, "  ", "b"); err == nil || len(doer.requests) != 0 {
		t.Fatalf("blank title: err=%v requests=%d", err, len(doer.requests))
	}
}
