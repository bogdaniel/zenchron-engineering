package main

import (
	"context"
	"errors"

	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// proposePlanningArtifact validates and durably writes one planning registry
// artifact (ADR-0008). planning.Write applies the identical seal function
// LoadRegistry uses on read, so a document the loader would refuse is
// refused here too, before anything reaches disk. It does not reload the
// held registry; a caller that wants the write to take effect without a
// restart sends ControlPlanningReload next.
//
// An AgentProfile is also checked against the CURRENT configured workforce
// here, at the write, rather than left to the whole-registry Registry.Bind
// check planService() runs for every plan action: a profile that would
// escalate its agent must refuse only itself, not the plan service for
// every other profile already installed.
func (c *composition) proposePlanningArtifact(request runtime.ControlRequest) ([]byte, error) {
	if request.PlanningArtifact == nil {
		return nil, errors.New("a planning artifact request carries its kind, id and document")
	}
	agents := runtime.DescribeExecutionAgents(context.Background(), c.agents, func(agent runtime.ResolvedAgent) runtime.AgentProber {
		return runtime.AgentProberFor(agent, c.artifacts, c.config.StateDir)
	})
	return planning.Write(c.config.PlanningDir, *request.PlanningArtifact, agents)
}

// currentPlanning returns the registry snapshot every reader uses. Copying
// the value under the lock is cheap (a handful of map headers); the maps
// themselves are never mutated after LoadRegistry builds them, so a reader
// that already has a snapshot needs no lock to use it.
func (c *composition) currentPlanning() planning.Registry {
	c.planningMu.RLock()
	defer c.planningMu.RUnlock()
	return c.planning
}

// reloadPlanning re-reads the planning registry from its own, unchanged
// directory (ADR-0008) and atomically swaps the held snapshot. It never
// changes planning_dir itself, which per ADR-0003 is fixed at controller
// start; it only re-resolves what that unchanged path currently contains.
func (c *composition) reloadPlanning() (planning.Registry, error) {
	registry, err := planning.LoadRegistry(c.config.PlanningDir)
	if err != nil {
		return planning.Registry{}, err
	}
	c.planningMu.Lock()
	c.planning = registry
	c.planningMu.Unlock()
	return registry, nil
}
