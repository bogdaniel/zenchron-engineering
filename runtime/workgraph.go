package runtime

// The WorkGraph layer (#472): dependency gating over the existing runtime.
//
// What this file does NOT do is the point of it. It schedules nothing: a unit
// becoming runnable means one ordinary #470 one-issue batch is written, and the
// existing orchestration pass creates its ordinary EngineeringRun, which the
// existing scheduler leases under the existing ceilings. There is no graph
// worker pool, no graph semaphore, no graph lease, no graph retry engine and no
// second run database. A graph cannot raise concurrency, and cannot make a run
// execute sooner than the scheduler grants it.
//
// It also decides no unit state into storage: a revision is immutable, an
// activation is written once, and every unit state is projected on each read.

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// AdoptWorkGraph is the explicit intake for a graph revision: the deterministic
// gate every proposal passes before anything can execute.
//
// A proposal may come from an operator today and from a planner or a model
// later; neither is trusted. The revision is composed under the runtime's own
// repository, agent and clock, validated as a graph, and - when it mutates a
// graph that already exists - checked against every unit this graph has already
// activated. Only then is it adopted, and adoption starts nothing: the next
// supervisor pass activates whatever the frontier then says is runnable.
//
// It is idempotent. The graph identity is a pure function of repository, agent
// and name, and a revision's identity is its contents, so a request whose reply
// was lost finds the revision it already adopted.
func (s *Supervisor) AdoptWorkGraph(ctx context.Context, request ControlRequest) (WorkGraphView, error) {
	repository, governed := s.governedRepository(request.Repository)
	if !governed {
		return WorkGraphView{}, fmt.Errorf("repository %q is not governed by this supervisor; enrolment is operator configuration, not a request", request.Repository)
	}
	if request.WorkGraph == nil {
		return WorkGraphView{}, errors.New("a work graph request carries the proposed revision")
	}
	// The agent is EXPLICIT, for the same reason a batch's is: a graph spends
	// one agent's capacity on every unit it names.
	if strings.TrimSpace(request.Agent) == "" {
		return WorkGraphView{}, errors.New("a work graph names its execution agent explicitly")
	}
	agent, err := s.deps.Agents.Agent(request.Agent)
	if err != nil {
		return WorkGraphView{}, err
	}
	if s.deps.AgentProber != nil {
		if err := RefuseUnlessInvocable(ctx, agent, s.deps.AgentProber(agent)); err != nil {
			return WorkGraphView{}, err
		}
	}
	engine, err := s.engine(repository.String(), agent.ID)
	if err != nil {
		return WorkGraphView{}, err
	}
	// Dependency satisfaction IS the admitted handoff, so a worker that cannot
	// write one could never satisfy anything: nothing downstream of any unit
	// would ever become runnable.
	if !writesTypedResults(engine.deps.Provider) {
		return WorkGraphView{}, fmt.Errorf("agent %q cannot write the runtime-owned typed result directory, so no unit it performs could ever transfer the admitted handoff a dependent unit waits for", agent.ID)
	}
	proposed, err := request.WorkGraph.Compose(repository.String(), agent.ID, BoundedNote(request.Operator), s.deps.Clock.Now())
	if err != nil {
		return WorkGraphView{}, err
	}
	err = s.admission.admit(func() error {
		// Serialized against activation: a frontier computed under this
		// graph's current revision must not be activated after a mutation
		// landed, and a mutation must not land against an activation set that
		// is already being added to.
		s.orchestrationMu.Lock()
		defer s.orchestrationMu.Unlock()
		return s.adoptRevision(proposed)
	})
	if err != nil {
		return WorkGraphView{}, err
	}
	holds, err := s.workUnitHolds(proposed.ID)
	if err != nil {
		return WorkGraphView{}, err
	}
	return WorkGraphStatus(s.deps.Store, s.deps.StateDir, proposed.ID, s.deps.Clock.Now(), holds)
}

// adoptRevision admits one proposed revision against durable state.
func (s *Supervisor) adoptRevision(proposed orchestration.WorkGraph) error {
	current, found, err := s.deps.Store.WorkGraph(proposed.ID)
	if err != nil {
		return err
	}
	switch {
	case !found && proposed.Revision != 1:
		return fmt.Errorf("no work graph %s exists, so its first revision is 1, not %d", proposed.ID, proposed.Revision)
	case found && proposed.Revision <= current.Revision:
		// A resubmission of a revision already adopted is answered from
		// durable state; one that differs is refused by the store rather than
		// overwriting a revision units may already have been activated under.
		if _, _, err := s.deps.Store.AdoptWorkGraphRevision(proposed); err != nil {
			return err
		}
		return nil
	case found:
		if current.ObjectivePlan != nil {
			return errors.New("an objective work graph is compiled through its plan, not an operator graph mutation")
		}
		activations, err := s.deps.Store.WorkUnitActivations(proposed.ID)
		if err != nil {
			return err
		}
		activated := make(map[string]bool, len(activations))
		for unitID := range activations {
			activated[unitID] = true
		}
		if err := orchestration.ValidateMutation(current, proposed, activated); err != nil {
			return err
		}
	}
	_, _, err = s.deps.Store.AdoptWorkGraphRevision(proposed)
	return err
}

// reconcileWorkGraphs is the supervisor pass's graph step: it activates every
// unit the frontier says is runnable, and nothing else. It returns one bounded
// line per problem; a problem with one graph or unit never stops the others.
//
// Activation is the ONLY thing it does. It writes the one-issue #470 batch that
// owns the unit's child run and records the activation; the existing
// orchestration pass creates that run, and the existing scheduler decides when
// it executes.
func (s *Supervisor) reconcileWorkGraphs() []string {
	graphs, err := s.deps.Store.WorkGraphs()
	if err != nil {
		return []string{boundedDetail(err.Error())}
	}
	var problems []string
	for _, graph := range graphs {
		// The plan owns assignments, gates and aggregate budget.
		if graph.ObjectivePlan != nil {
			continue
		}
		problems = append(problems, s.activateGraphFrontier(graph.ID)...)
	}
	return problems
}

// activateGraphFrontier projects one graph and activates what it says is
// runnable, holding the orchestration lock across BOTH.
//
// The lock spans them deliberately. Adopting a revision validates a proposal
// against the units already activated, so a frontier computed under revision N
// must not be activated after N+1 landed: the unit would be claimed against
// inputs the revision that froze it never named. The projection is re-read
// inside the lock for the same reason, so every unit activated here comes from
// the revision the frontier was computed from. Taken per graph, so one graph's
// pass does not hold up adoption for another.
func (s *Supervisor) activateGraphFrontier(graphID string) []string {
	s.orchestrationMu.Lock()
	defer s.orchestrationMu.Unlock()
	// FAILS CLOSED. A hold source that cannot answer is not "no holds": the
	// pass reports it and activates nothing for this graph.
	holds, err := s.workUnitHolds(graphID)
	if err != nil {
		return []string{boundedDetail(graphID + ": " + err.Error())}
	}
	view, err := WorkGraphStatus(s.deps.Store, s.deps.StateDir, graphID, s.deps.Clock.Now(), holds)
	if err != nil {
		return []string{boundedDetail(graphID + ": " + err.Error())}
	}
	units := make(map[string]WorkGraphUnitView, len(view.Units))
	for _, unit := range view.Units {
		units[unit.UnitID] = unit
	}
	var problems []string
	for _, unitID := range view.Frontier {
		if err := s.activateWorkUnit(view, units[unitID]); err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s unit %s: %v", view.GraphID, unitID, err)))
		}
	}
	return problems
}

// workUnitHolds asks the readiness owner outside the graph what it is holding.
func (s *Supervisor) workUnitHolds(graphID string) (map[string]orchestration.DecisionWait, error) {
	if s.deps.WorkUnitHolds == nil {
		return nil, nil
	}
	return s.deps.WorkUnitHolds(graphID)
}

// activateWorkUnit claims one unit's child run, exactly once.
//
// The child is an ordinary #470 batch over one issue whose identity is a pure
// function of repository, agent, issue, THIS GRAPH, THIS UNIT and the exact
// input set. So a crash between writing the batch and recording the activation
// replays onto the same batch and the same run instead of creating a second, and
// nothing else in the system can land on that identity. The activation's primary
// key makes the record itself write-once.
//
// The caller holds s.orchestrationMu: deciding a child run identity must not
// race another writer deciding the same one.
func (s *Supervisor) activateWorkUnit(graph WorkGraphView, unit WorkGraphUnitView) error {
	if unit.UnitID == "" {
		return errors.New("the frontier names a unit this projection does not contain")
	}
	if unit.InputsDigest == "" {
		return fmt.Errorf("unit %s is runnable but its consumed inputs have no digest", unit.UnitID)
	}
	engine, err := s.engine(graph.Repository, graph.AgentID)
	if err != nil {
		return err
	}
	// THE BATCH IS THIS UNIT EXECUTION'S, not the issue's. Its identity binds
	// the graph, the unit and the exact input set, so replaying a crashed
	// activation finds the run it already created and nothing else can: a
	// completed run from an earlier direct orchestration of the same issue, or
	// from another graph, or from this unit against other inputs, is a
	// different batch and therefore never satisfies this unit.
	origin := orchestration.BatchOrigin{GraphID: graph.GraphID, UnitID: unit.UnitID, Inputs: unit.Inputs, ExecutionKind: unit.ExecutionKind}
	batchID, err := orchestration.WorkUnitBatchID(graph.Repository, graph.AgentID, unit.Issue, origin)
	if err != nil {
		return err
	}
	// The digest the projection computed and the digest of what is about to be
	// recorded must be ONE answer. They are computed by the same function over
	// the same set; a disagreement means this pass is acting on a projection it
	// no longer describes, and it refuses rather than activating.
	digest, err := unit.Inputs.Digest()
	if err != nil {
		return err
	}
	if digest != unit.InputsDigest {
		return fmt.Errorf("unit %s projects inputs digest %s and its recorded input set digests %s", unit.UnitID, unit.InputsDigest, digest)
	}
	stored, found, err := s.deps.Store.OrchestrationBatch(batchID)
	if err != nil {
		return err
	}
	if !found {
		reserved, err := reservedRunIDs(s.deps.Store)
		if err != nil {
			return err
		}
		planned, err := engine.planOrchestrationBatch(batchID, []int{unit.Issue}, graph.RequestedBy, reserved, &origin)
		if err != nil {
			return err
		}
		if stored, _, err = s.deps.Store.CreateOrchestrationBatch(planned); err != nil {
			return err
		}
	}
	// Checked rather than assumed: the batch this activation points at must be
	// THIS unit execution's, over this issue alone.
	if len(stored.Items) != 1 || stored.Items[0].Issue != unit.Issue {
		return fmt.Errorf("batch %s does not describe issue %d alone", batchID, unit.Issue)
	}
	if stored.Origin == nil || stored.Origin.GraphID != graph.GraphID || stored.Origin.UnitID != unit.UnitID {
		return fmt.Errorf("batch %s does not describe unit %s of graph %s", batchID, unit.UnitID, graph.GraphID)
	}
	_, err = s.deps.Store.ActivateWorkUnit(WorkUnitActivation{
		GraphID: graph.GraphID, UnitID: unit.UnitID, BatchID: batchID, RunID: stored.Items[0].RunID,
		InputsDigest: unit.InputsDigest, ActivatedAt: s.deps.Clock.Now(),
	})
	return err
}
