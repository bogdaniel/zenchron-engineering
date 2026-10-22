package controlplane

import (
	"net/http"
	"strings"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

// createIssue serves POST /v1/issues: open one issue on a governed repository
// from the operator's own title and description. The text is validated here
// with the controller's own rules so an invalid draft never reaches the socket;
// the controller validates again and checks the repository is governed.
//
// There is no durable state to re-read afterwards: the issue lives on the
// forge, and a lost reply is reconciled through the operator action receipt.
func (a *API) createIssue(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ControllerBinding string `json:"controller_binding"`
		Repository        string `json:"repository"`
		Title             string `json:"title"`
		Description       string `json:"description"`
	}
	if !decodeAction(w, r, &body) {
		return
	}
	issue := rt.NewIssue{Title: body.Title, Body: body.Description}
	if strings.TrimSpace(body.Repository) == "" || issue.Validate() != nil {
		fail(w, 400, "invalid_request")
		return
	}
	a.actResponse(w, r, ActionResult{Action: rt.ControlIssueCreate}, body.ControllerBinding,
		rt.ControlRequest{Command: rt.ControlIssueCreate, Repository: body.Repository, NewIssue: &issue},
		func() (bool, any, error) { return false, nil, nil },
		func(any) (int, string) { return 0, "" },
		nil)
}
