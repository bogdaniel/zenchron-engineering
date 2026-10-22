package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// dispatchArtifactWithReceipt is dispatchPlanOrRunWithReceipt's sibling for
// planning-artifact-propose and product-propose: the two governed commands
// whose exact subject is an artifact or a product, not a run or a plan, and
// so has no home in the event journal's reduced run/plan streams - see
// artifact_action_receipt.go (runtime) for why that store exists at all.
func (c *composition) dispatchArtifactWithReceipt(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	if crossed, err := c.store.OperatorActionReceipt(request.OperationID); err != nil {
		return controlError(err)
	} else if crossed != nil {
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "operation reference already recorded for a different action kind"}
	}
	claimed, err := c.store.ClaimArtifactActionOperation(request.OperationID, time.Now().UTC())
	if err != nil {
		return controlError(err)
	}
	if !claimed {
		// Already finalized (a true duplicate) or still mid-flight from
		// another request sharing this reference - both read the same way
		// to the caller, and neither may dispatch a second time.
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "operation reference already recorded or in flight; read the original exact receipt without resending"}
	}
	response := c.dispatchControl(ctx, supervisor, shutdown, request, true)
	if !response.OK {
		// Nothing durable happened - release the claim so a corrected retry
		// under the same reference is not blocked by this refusal forever.
		if releaseErr := c.store.ReleaseArtifactActionClaim(request.OperationID); releaseErr != nil {
			return controlError(releaseErr)
		}
		return response
	}
	receipt := runtime.ArtifactActionReceipt{
		OperationID: request.OperationID, RequestDigest: request.RequestDigest,
		Action: request.Command, ControllerBinding: request.ExpectedController,
	}
	var extractErr error
	if request.Command == runtime.ControlPlanningArtifactPropose {
		var sealed struct {
			ID     string `json:"id"`
			Digest string `json:"digest"`
		}
		if extractErr = json.Unmarshal(response.Payload, &sealed); extractErr == nil {
			if request.PlanningArtifact == nil || sealed.ID == "" || sealed.ID != request.PlanningArtifact.ID {
				extractErr = fmt.Errorf("planning-artifact result differs from original request")
			}
		}
		receipt.ArtifactKind = string(request.PlanningArtifact.Kind)
		receipt.ArtifactID = sealed.ID
		receipt.ArtifactDigest = sealed.Digest
	} else if request.Command == runtime.ControlIssueCreate {
		var created createdIssue
		if extractErr = json.Unmarshal(response.Payload, &created); extractErr == nil && (created.Number < 1 || !strings.EqualFold(created.Repository, request.Repository)) {
			extractErr = fmt.Errorf("issue-create result differs from original request")
		}
		receipt.Repository, receipt.SourceIssue = created.Repository, created.Number
	} else {
		var adopted struct {
			ID       string `json:"id"`
			Revision int    `json:"revision"`
		}
		if extractErr = json.Unmarshal(response.Payload, &adopted); extractErr == nil {
			if adopted.ID == "" || (request.Product != nil && adopted.Revision != request.Product.Revision) {
				extractErr = fmt.Errorf("product result differs from original request")
			}
		}
		receipt.ProductID = adopted.ID
		receipt.ProductRevision = adopted.Revision
	}
	if extractErr == nil {
		extractErr = c.store.FinalizeArtifactActionReceipt(receipt, time.Now().UTC())
	}
	if extractErr != nil {
		// The command may already have effected a durable change. Never call this a refusal.
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "action result could not be journalled; outcome remains unknown"}
	}
	return response
}
