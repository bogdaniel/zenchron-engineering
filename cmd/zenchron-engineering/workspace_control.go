package main

import (
	"context"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// Browser commands are a closed subset of the existing operator lifecycle.
func browserGovernedCommand(command string) bool {
	switch command {
	case runtime.ControlStop, runtime.ControlPlanReject, runtime.ControlPlanApprove,
		runtime.ControlResolveDecision, runtime.ControlPause, runtime.ControlUnpause, runtime.ControlPlanRevise, runtime.ControlAuthorize, runtime.ControlPlanView, runtime.ControlWorkspaceOptions, runtime.ControlAuthorityView, runtime.ControlResume, runtime.ControlRefresh,
		runtime.ControlPlanningArtifactPropose, runtime.ControlPlanningReload:
		return true
	}
	return false
}

func (c *composition) workspaceRead(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest) runtime.ControlResponse {
	if request.Command == runtime.ControlAuthorityView {
		authority, err := supervisor.OperatorAuthorityRequest(request.RunID)
		if err != nil {
			return controlError(err)
		}
		return controlOK(authority)
	}
	if request.Command == runtime.ControlPlanView {
		plans, err := c.planService()
		if err != nil {
			return controlError(err)
		}
		view, err := plans.View(request.PlanID)
		if err != nil {
			return controlError(err)
		}
		preview := controlplane.PlanPreviewProjection(view)
		preview.ControllerBinding = request.ExpectedController
		return controlOK(preview)
	}
	repositories := supervisor.GovernedRepositories()
	customization := c.currentPlanning()
	options := controlplane.WorkspaceOptions{Repositories: []string{}, Agents: []controlplane.WorkspaceAgent{}, Profiles: customization.ProfileIDs(), Templates: customization.TemplateIDs(), Budget: c.config.PlanEnvelope()}
	for _, repo := range repositories {
		options.Repositories = append(options.Repositories, repo.String())
	}
	for _, agent := range c.describeAgents(ctx) {
		options.Agents = append(options.Agents, controlplane.WorkspaceAgent{ID: agent.ID, Kind: agent.Kind, TrustMode: agent.TrustMode, Model: agent.Model, Default: agent.Default, Eligible: agent.Eligible})
	}
	return controlOK(options)
}

func (c *composition) recordBrowserAuthority(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest) runtime.ControlResponse {
	operator, err := c.config.ResolveOperator()
	if err != nil {
		return controlError(err)
	}
	result, err := supervisor.RecordOperatorAuthority(ctx, runtime.AuthorizeInput{RunID: request.RunID, RequestID: request.AuthorityRequestID, Digest: request.AuthorityDigest, Decision: request.AuthorityDecision, Note: runtime.BoundedNote(request.Note), Operator: operator})
	if err != nil {
		return controlError(err)
	}
	return controlOK(result)
}

func (c *composition) controlPause(request runtime.ControlRequest, governed bool) runtime.ControlResponse {
	if governed {
		operator, err := c.config.ResolveOperator()
		if err != nil {
			return controlError(err)
		}
		request.Operator = operator.ID
	}
	// Applied under the controller role (#86): a generation that has been
	// superseded - or a process holding no role at all (WithAuthority is
	// nil-safe and refuses) - cannot accept a pause or an unpause.
	var view runtime.PauseView
	apply := func() (err error) { view, err = applyPause(c.store, request); return err }
	var pauseErr error
	if governed {
		pauseErr = apply()
	} else {
		pauseErr = c.role.WithAuthority(apply)
	}
	if err := pauseErr; err != nil {
		return controlError(err)
	}
	return controlOK(view)
}

// This is admission of an active invocation, not a queue: duplicate work is
// refused immediately and the entry disappears when its owner returns.
func (c *composition) admitBrowserPlanning(planID string) (func(), error) {
	ceiling, err := c.maxConcurrentRuns()
	if err != nil {
		return nil, err
	}
	c.planInvocationsMu.Lock()
	defer c.planInvocationsMu.Unlock()
	if _, active := c.planInvocations[planID]; active {
		return nil, fmt.Errorf("planning_in_progress: plan %s already has an active invocation", planID)
	}
	if len(c.planInvocations) >= ceiling {
		return nil, fmt.Errorf("planning_capacity: the operator concurrency ceiling is occupied")
	}
	if c.planInvocations == nil {
		c.planInvocations = map[string]struct{}{}
	}
	c.planInvocations[planID] = struct{}{}
	return func() { c.planInvocationsMu.Lock(); delete(c.planInvocations, planID); c.planInvocationsMu.Unlock() }, nil
}

func (c *composition) controlRunLifecycle(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest) runtime.ControlResponse {
	run, err := requireRun(c, request.RunID)
	if err != nil {
		return controlError(err)
	}
	if request.Command == runtime.ControlRefresh {
		if _, err := c.recordPolicyGeneration(); err != nil {
			return controlError(err)
		}
		outcome, err := supervisor.RefreshIssueRun(ctx, request.RunID)
		if err != nil {
			return controlError(err)
		}
		return controlOK(outcome)
	}
	events, err := c.store.Events(request.RunID)
	if err != nil {
		return controlError(err)
	}
	if _, detail, refused := resumeRefusal(run, request.RunID, events); refused {
		return controlError(fmt.Errorf("resume_refused: %s", detail))
	}
	if err := supervisor.RequestRunReconciliation(request.RunID); err != nil {
		return controlError(err)
	}
	return controlOK(map[string]string{"run_id": request.RunID, "state": "reconciliation_requested"})
}
