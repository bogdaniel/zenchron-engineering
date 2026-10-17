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
	// Refused before the engine, the planning invocation and the plan/graph
	// write all happen for nothing: an unknown product, or one that never
	// named THIS repository, is certain to make AssociateProductGraph
	// refuse later anyway - checked here, now that the repository the
	// objective actually targets is resolved, rather than after paying for
	// all of that.
	if err := plans.RequireProductIfChosen(request.ProductID, repository); err != nil {
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
		ProductID: request.ProductID,
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
			// This objective was already proposed - a resend of the SAME
			// request, exactly like any other idempotent-resend action in
			// this product. BindObjectivePlan is the canonical, idempotent
			// admission path for the objective's WorkGraph regardless of
			// whether it already exists: replaying it resumes an earlier
			// attempt that crashed between writing the plan and binding its
			// graph, rather than reporting success while a product-scoped
			// (or any) objective is silently left with no graph at all. Its
			// own "already-started" guard refuses outright if the plan has
			// since begun executing as a flat run, rather than retroactively
			// touching work that is already live.
			agentID, err := resolveAgentID(c, flags)
			if err != nil {
				return runtime.PlanView{}, err
			}
			graphID, err := plans.BindObjectivePlan(current.Plan, issue, agentID)
			if err != nil {
				return runtime.PlanView{}, err
			}
			if err := plans.AssociateProductGraph(request.ProductID, graphID); err != nil {
				return runtime.PlanView{}, err
			}
			// current was read before BindObjectivePlan; its WorkGraphID is
			// stale (empty) whenever this resend is the one that actually
			// bound the graph, not merely a replay of an already-bound one.
			return plans.View(planID)
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

// resolveAgentID is the one resolution of the operator's agent preference
// for a plan-to-WorkGraph bind, reused wherever one happens: a first
// proposal (proposePlanAndGraph) and a resumed one (revisePlan's dedup
// path) must record the same agent a request named, not two independent
// readings of it.
func resolveAgentID(c *composition, flags autonomyFlags) (string, error) {
	if flags.Agent == "" {
		return c.agents.Default(), nil
	}
	agent, err := c.agents.Agent(flags.Agent)
	if err != nil {
		return "", err
	}
	return agent.ID, nil
}

func proposePlanAndGraph(ctx context.Context, composed *planComposition, input runtime.ProposeInput, flags autonomyFlags) (domain.EngineeringPlan, error) {
	plan, err := composed.service.Propose(ctx, input)
	if err != nil {
		return plan, err
	}
	agentID, err := resolveAgentID(composed.built, flags)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	graphID, err := composed.service.BindObjectivePlan(plan, input.Issue, agentID)
	if err != nil {
		return domain.EngineeringPlan{}, err
	}
	if err := composed.service.AssociateProductGraph(flags.ProductID, graphID); err != nil {
		return domain.EngineeringPlan{}, err
	}
	return plan, nil
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
