package runtime

import (
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// workUnitContext is the upstream a WorkGraph unit's child run consumes (#472).
//
// It is read from the run's OWN durable binding: the run names the batch that
// created it, and that batch's origin names the unit and the exact admitted
// handoffs the unit was activated against. So the invocation receives precisely
// the inputs the activation recorded, and an invocation of a run bound to some
// other batch receives none of them.
//
// Composing upstream CODE into this candidate is #475's; this carries the exact
// handoff and subject into the invocation, which is what makes the activation's
// recorded input set a fact about the execution rather than bookkeeping.
type workUnitContext struct {
	upstream []UpstreamContext
	// product is the unit's frozen product context (#480), nil for a graph with
	// no product.
	product *execution.ProductContext
}

// apply gives the invocation its upstream and product context. It widens
// nothing: permissions, prohibitions and acceptance still come from the
// compiled contract.
func (w workUnitContext) apply(request ExecutionRequest) ExecutionRequest {
	if len(w.upstream) > 0 {
		request.Upstream = w.upstream
	}
	request.Product = w.product
	return request
}

// workUnit resolves everything a unit's invocation is given beyond its issue.
// Its error is returned BEFORE a provider is reached, so a unit whose product
// context is missing or inconsistent never spends a model invocation.
func (r *EngineeringRuntime) workUnit(state *runState, assignment *domain.AgentAssignment) (workUnitContext, error) {
	if state.run.Orchestration == nil || state.run.Orchestration.BatchID == "" {
		return r.planStageProductContext(state, assignment)
	}
	batch, found, err := r.deps.Store.OrchestrationBatch(state.run.Orchestration.BatchID)
	if err != nil {
		return workUnitContext{}, err
	}
	if !found || batch.Origin == nil {
		return workUnitContext{}, nil
	}
	compiled, err := r.unitProductContext(state, *batch.Origin, batch.Product, assignment)
	if err != nil {
		return workUnitContext{}, err
	}
	if len(batch.Origin.Inputs) == 0 {
		return workUnitContext{product: executionProductContext(compiled)}, nil
	}
	upstream := make([]UpstreamContext, 0, len(batch.Origin.Inputs))
	for _, input := range batch.Origin.Inputs {
		context, err := r.withUpstreamDiff(UpstreamContext{
			StageID: input.UnitID, RunID: input.RunID,
			Commit: input.CandidateRevision, Tree: input.CandidateTree,
			Handoff: &UpstreamHandoff{
				ID: input.HandoffID, Outcome: input.Outcome, Summary: input.Summary,
				Unresolved: input.Unresolved, RecommendedNext: input.RecommendedNext,
			},
		})
		if err != nil {
			return workUnitContext{}, err
		}
		upstream = append(upstream, context)
	}
	return workUnitContext{upstream: upstream, product: executionProductContext(compiled)}, nil
}

// planStageProductContext is workUnit's counterpart for a plan stage run with
// no batch: its product context was frozen on the run's own plan binding.
func (r *EngineeringRuntime) planStageProductContext(state *runState, assignment *domain.AgentAssignment) (workUnitContext, error) {
	binding := state.run.Plan
	if binding == nil || binding.Product == nil {
		return workUnitContext{}, nil
	}
	origin := orchestration.BatchOrigin{GraphID: binding.Product.GraphID, UnitID: binding.StageID}
	compiled, err := r.unitProductContext(state, origin, &binding.Product.Pin, assignment)
	if err != nil {
		return workUnitContext{}, err
	}
	return workUnitContext{product: executionProductContext(compiled)}, nil
}
