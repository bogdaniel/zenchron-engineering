package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// dispatchArtifactWithReceipt is dispatchPlanOrRunWithReceipt's sibling for
// planning-artifact-propose and product-propose: the two governed commands
// whose exact subject is an artifact or a product, not a run or a plan, and
// so has no home in the event journal's reduced run/plan streams - see
// artifact_action_receipt.go (runtime) for why that store exists at all.
func (c *composition) dispatchArtifactWithReceipt(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	existing, err := c.store.ArtifactActionReceipt(request.OperationID)
	if err != nil {
		return controlError(err)
	}
	if existing != nil {
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "operation reference already recorded; read the original exact receipt without resending"}
	}
	response := c.dispatchControl(ctx, supervisor, shutdown, request, true)
	if !response.OK {
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
		extractErr = c.store.PutArtifactActionReceipt(receipt, time.Now().UTC())
	}
	if extractErr != nil {
		// The command may already have effected a durable change. Never call this a refusal.
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "action result could not be journalled; outcome remains unknown"}
	}
	return response
}
