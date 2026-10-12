package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func (c *composition) revisePlan(ctx context.Context, supervisor *runtime.Supervisor, request runtime.ControlRequest) (runtime.PlanView, error) {
	plans, err := c.planService()
	if err != nil {
		return runtime.PlanView{}, err
	}
	repository, issue, defaultBranch, err := c.planSubject(supervisor, request)
	if err != nil {
		return runtime.PlanView{}, err
	}
	repo, err := runtime.ParseGitHubRepo(repository)
	if err != nil {
		return runtime.PlanView{}, err
	}
	// Derived from the plan's own repository IDENTITY, exactly as the local
	// command derives it when a repository is named explicitly.
	//
	// An earlier attempt at this ran `git remote get-url origin` in the STATE
	// DIRECTORY, which is not a checkout of anything: usually it errors and the
	// fallback silently assumed a default branch, and where the state directory
	// happens to sit inside some unrelated git repository it succeeded and bound
	// a governed-remote system to that repository's origin. A supervisor governs
	// several repositories; the plan says which one, and nothing about the
	// process's own working directory does.
	target := runtime.RepositoryTarget{
		Identity: repo.String(), Remote: repo.CloneURL(), DefaultBranch: defaultBranch,
	}
	agent := c.agent
	if request.Agent != "" {
		agent, err = c.agents.Agent(request.Agent)
		if err != nil {
			return runtime.PlanView{}, err
		}
	}
	engine, err := c.engineFor(target, agent)
	if err != nil {
		return runtime.PlanView{}, err
	}
	composed := &planComposition{
		built: c, engine: engine, service: plans, target: target, release: func() {},
	}
	flags := autonomyFlags{
		Template: request.Template, Profile: request.Profile, Deterministic: request.Deterministic,
		Agent:           request.Agent,
		SubstituteHuman: request.SubstituteHuman, Note: request.Note,
	}
	// A first proposal has no id yet. It is derived exactly as the local path
	// derives it - from the issue, deterministically - so the view returned
	// afterwards is of the plan this call created rather than of nothing.
	planID := request.PlanID
	if planID == "" {
		if planID, err = engine.PlanID(issue); err != nil {
			return runtime.PlanView{}, err
		}
	}
	if request.ExpectedController != "" {
		release, err := c.admitBrowserPlanning(planID)
		if err != nil {
			return runtime.PlanView{}, err
		}
		defer release()
		current, err := plans.View(planID)
		var absent *runtime.PlanRefusedError
		if request.PlanID == "" && err == nil {
			return current, nil
		}
		if request.PlanID == "" && !errors.As(err, &absent) {
			return runtime.PlanView{}, err
		}
		if request.PlanID != "" {
			if err != nil {
				return runtime.PlanView{}, err
			}
			if current.Plan.Revision != request.Revision || current.Plan.Digest != request.Digest {
				return runtime.PlanView{}, fmt.Errorf("plan revision does not match the revision the operator read")
			}
		}
	}
	serialize := supervisor.WithPlanLock
	if request.ExpectedController != "" {
		serialize = func(write func() error) error {
			return supervisor.WithPlanLock(func() error {
				current, err := plans.View(planID)
				if request.PlanID == "" {
					var absent *runtime.PlanRefusedError
					if errors.As(err, &absent) {
						return write()
					}
					if err != nil {
						return err
					}
					return fmt.Errorf("objective already has plan %s at revision %d; refresh before requesting a revision", planID, current.Plan.Revision)
				}
				if err != nil {
					return err
				}
				if current.Plan.Revision != request.Revision || current.Plan.Digest != request.Digest {
					return fmt.Errorf("plan revision changed during planning; proposal refused")
				}
				return write()
			})
		}
	}
	if flags.SubstituteHuman != "" {
		if request.PlanID == "" {
			return runtime.PlanView{}, errors.New("substituting a human names the plan whose stage is being substituted")
		}
		// The substitution compiles deterministically - no provider call - so
		// the whole of it is short enough to serialize.
		if err := serialize(func() error {
			_, err := substituteHumanWithComposition(ctx, composed, flags, request.PlanID, io.Discard)
			return err
		}); err != nil {
			return runtime.PlanView{}, err
		}
		return plans.View(request.PlanID)
	}
	if _, err := proposeSerialized(ctx, composed, flags, issue, planID, io.Discard, serialize); err != nil {
		return runtime.PlanView{}, err
	}
	return plans.View(planID)
}

func proposePlanAndGraph(ctx context.Context, composed *planComposition, input runtime.ProposeInput, flags autonomyFlags) (domain.EngineeringPlan, error) {
	plan, err := composed.service.Propose(ctx, input)
	if err != nil {
		return plan, err
	}
	agentID := composed.built.agents.Default()
	if flags.Agent != "" {
		agent, resolveErr := composed.built.agents.Agent(flags.Agent)
		err = resolveErr
		if err != nil {
			return domain.EngineeringPlan{}, err
		}
		agentID = agent.ID
	}
	_, err = composed.service.BindObjectivePlan(plan, input.Issue, agentID)
	return plan, err
}

func renderObjectivePlan(flags autonomyFlags, view runtime.PlanView, stdout io.Writer, action string) (int, error) {
	code, err := planOutput(flags, view, stdout, action)
	if err != nil || !flags.Text || view.WorkGraphID == "" {
		return code, err
	}
	_, err = fmt.Fprintf(stdout, "work graph: %s (activation awaits plan approval)\n", terminalSafe(view.WorkGraphID))
	if err != nil {
		return runtime.ExitFailed, err
	}
	return code, nil
}
