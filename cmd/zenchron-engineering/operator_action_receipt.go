package main

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func (c *composition) dispatchWithReceipt(ctx context.Context, supervisor *runtime.Supervisor, shutdown func(), request runtime.ControlRequest) runtime.ControlResponse {
	if request.OperationID == "" && request.RequestDigest == "" {
		return c.dispatchControl(ctx, supervisor, shutdown, request, true)
	}
	if !runtime.ValidActionReference(request.OperationID, request.RequestDigest) ||
		(request.Command != runtime.ControlPlanRevise && request.Command != runtime.ControlResume && request.Command != runtime.ControlRefresh) {
		return controlError(fmt.Errorf("invalid action recovery reference"))
	}
	existing, err := c.store.OperatorActionReceipt(request.OperationID)
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
