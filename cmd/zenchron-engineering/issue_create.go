package main

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// createdIssue is the control payload and what the receipt records.
type createdIssue struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	URL        string `json:"url"`
}

// createIssue opens one issue on a repository this supervisor governs, with
// the credential the runtime already uses for the forge. It only ever runs
// for a governed (browser) request, because the receipt that makes a lost
// reply recoverable exists only on that path.
func (c *composition) createIssue(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest, governed bool) runtime.ControlResponse {
	if !governed {
		return controlError(fmt.Errorf("issue-create is a governed dashboard action"))
	}
	if request.NewIssue == nil {
		return controlError(fmt.Errorf("an issue-create request carries the issue text"))
	}
	if err := request.NewIssue.Validate(); err != nil {
		return controlError(err)
	}
	repository, ok := supervisor.GovernedRepository(request.Repository)
	if !ok {
		return controlError(fmt.Errorf("repository %q is not governed by this supervisor", request.Repository))
	}
	creator, ok := c.forge.(runtime.NewIssueCreator)
	if !ok {
		return controlError(fmt.Errorf("the configured forge cannot create issues"))
	}
	operator, err := c.config.ResolveOperator()
	if err != nil {
		return controlError(err)
	}
	issue, err := creator.CreateIssue(ctx, repository, request.NewIssue.Title, request.NewIssue.ForgeBody(operator.ID))
	if err != nil {
		return controlError(err)
	}
	return controlOK(createdIssue{Repository: repository.String(), Number: issue.Number, URL: issue.URL})
}
