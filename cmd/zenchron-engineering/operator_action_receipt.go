package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// dispatchWithReceipt routes a recovery-capable request (one carrying an
// operation id and request digest) to the receipt store that matches its
// command: the event journal for plan/run lifecycle commands, or
// artifact_action_receipts (artifact_action_receipt.go) for the two command
// kinds that are not journaled lifecycle state. A request naming neither
// skips recovery entirely - exactly today's behavior.
func (c *composition) dispatchWithReceipt(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	if request.OperationID == "" && request.RequestDigest == "" {
		return c.dispatchControl(ctx, supervisor, shutdown, request, true)
	}
	if !runtime.ValidActionReference(request.OperationID, request.RequestDigest) {
		return controlError(fmt.Errorf("invalid action recovery reference"))
	}
	switch request.Command {
	case runtime.ControlPlanRevise, runtime.ControlResume, runtime.ControlRefresh:
		return c.dispatchPlanOrRunWithReceipt(ctx, supervisor, shutdown, request)
	case runtime.ControlPlanningArtifactPropose, runtime.ControlProductPropose:
		return c.dispatchArtifactWithReceipt(ctx, supervisor, shutdown, request)
	default:
		return controlError(fmt.Errorf("invalid action recovery reference"))
	}
}

// dispatchPlanOrRunWithReceipt is dispatchWithReceipt's original body,
// unchanged, for the three command kinds the plan/run event journal already
// carries.
func (c *composition) dispatchPlanOrRunWithReceipt(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	existing, err := c.store.OperatorActionReceipt(request.OperationID)
	if err != nil {
		return controlError(err)
	}
	if existing != nil {
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "operation reference already recorded; read the original exact receipt without resending"}
	}
	if crossed, err := c.store.ArtifactActionReceipt(request.OperationID); err != nil {
		return controlError(err)
	} else if crossed != nil {
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "operation reference already recorded for a different action kind"}
	}
	response := c.dispatchControl(ctx, supervisor, shutdown, request, true)
	if !response.OK {
		return response
	}
	receipt := runtime.OperatorActionReceipt{OperationID: request.OperationID, RequestDigest: request.RequestDigest, Action: request.Command, ControllerBinding: request.ExpectedController}
	event := runtime.EngineeringEvent{SchemaVersion: runtime.SchemaVersion, ID: "operator-action-" + request.OperationID, OperationID: request.OperationID, OccurredAt: time.Now().UTC()}
	if request.Command == runtime.ControlPlanRevise {
		var view runtime.PlanView
		err = json.Unmarshal(response.Payload, &view)
		if err == nil && (view.Plan.ID == "" || (request.PlanID != "" && (view.Plan.ID != request.PlanID || view.Plan.Revision <= request.Revision))) {
			err = fmt.Errorf("planning result differs from original request")
		}
		receipt.PlanID, receipt.Revision, receipt.PlanDigest = view.Plan.ID, view.Plan.Revision, view.Plan.Digest
		if err == nil {
			var found bool
			receipt.Repository, receipt.SourceIssue, found, err = c.store.PlanSource(view.Plan.ID)
			if err == nil && (!found || (request.PlanID == "" && (receipt.Repository != request.Repository || receipt.SourceIssue != request.Issue))) {
				err = fmt.Errorf("planning source differs from original request")
			}
		}
		event.Type, event.PlanID = runtime.EventPlanOperatorActionReceipt, view.Plan.ID
	} else {
		event.Type, event.RunID = runtime.EventRunOperatorActionReceipt, request.RunID
		receipt.RunID = request.RunID
		receipt.SourceRunID = request.RunID
		if request.Command == runtime.ControlRefresh {
			var result runtime.StartOutcome
			err = json.Unmarshal(response.Payload, &result)
			receipt.RunID = result.RunID
		}
	}
	if err == nil {
		event.Payload, err = json.Marshal(receipt)
	}
	if err == nil {
		if event.PlanID != "" {
			_, err = c.store.AppendPlanEvent(event)
		} else {
			_, err = c.store.AppendEvent(event)
		}
	}
	if err != nil {
		// The command may already have effected a durable change. Never call this a refusal.
		return runtime.ControlResponse{Code: runtime.ControlCodeOutcomeUnknown, Error: "action result could not be journalled; outcome remains unknown"}
	}
	return response
}
