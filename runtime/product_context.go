package runtime

// Product context for a WorkGraph unit's execution (#480).
//
// Two moments, one identity. At ADMISSION - the instant a unit's batch is
// written - pinProductContext resolves the graph's product from the runtime's
// own association and freezes the context it compiles into the batch
// (orchestration.Batch.Product). At EXECUTION, unitProductContext reads that
// pin back, cross-checks it against the association, the product's repository
// list and the plan binding, and recompiles through product.ContextPin.Resolve.
//
// Nothing here takes a product id, owner or revision from a model, a provider
// argument or a request: the product comes from product_graph_associations, the
// owner is derived from the batch's graph and unit ids, and the revision comes
// from the pin. Product configuration is delivered as context DATA only - its
// Policy and ContextPolicy references are never consulted, so a configuration
// can neither widen nor replace the work contract the unit was approved under.

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/execution"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/planning"
	"github.com/bogdaniel/zenchron-engineering/product"
)

// unitContextRequest is the only way the runtime asks for a unit's context.
// ScopeProduct is the widest scope a unit may read: its configuration, the
// widely promoted knowledge of its own product, and promotions audienced to
// exactly this unit. FeatureRef stays empty because the runtime has no
// authoritative feature identity yet; an empty one is the safe default.
func unitContextRequest(productID string, origin orchestration.BatchOrigin) product.ContextRequest {
	return product.ContextRequest{
		ProductID: productID, Scope: product.ScopeProduct,
		OwnerRef: product.WorkUnitOwnerRef(origin.GraphID, origin.UnitID),
	}
}

// pinProductContext freezes the product context a unit execution is admitted
// with, onto the batch about to be written. A batch with no origin, or whose
// graph has no product, is returned unchanged - legacy flows are untouched.
//
// A remediation epoch inherits its unit's first pin instead of compiling a new
// one: a unit keeps one context identity for its whole life.
func (s *SQLiteOperationStore) pinProductContext(batch orchestration.Batch) (orchestration.Batch, error) {
	if batch.Origin == nil {
		return batch, nil
	}
	productID, associated, err := s.AssociatedProduct(batch.Origin.GraphID)
	if err != nil || !associated {
		return batch, err
	}
	if batch.Origin.Epoch > 0 {
		return s.inheritProductPin(batch, productID)
	}
	compiled, err := product.CompileContext(s, unitContextRequest(productID, *batch.Origin))
	if err != nil {
		return batch, fmt.Errorf("compile product context for unit %s: %w", batch.Origin.UnitID, err)
	}
	pin, err := product.NewContextPin(compiled)
	if err != nil {
		return batch, err
	}
	batch.Product = &pin
	return batch, nil
}

func (s *SQLiteOperationStore) inheritProductPin(batch orchestration.Batch, productID string) (orchestration.Batch, error) {
	activations, err := s.WorkUnitActivations(batch.Origin.GraphID)
	if err != nil {
		return batch, err
	}
	activation, activated := activations[batch.Origin.UnitID]
	if !activated {
		return batch, fmt.Errorf("unit %s has no activation whose product context a remediation epoch could inherit", batch.Origin.UnitID)
	}
	first, found, err := s.OrchestrationBatch(activation.BatchID)
	if err != nil {
		return batch, err
	}
	if !found || first.Product == nil || first.Product.ProductID != productID {
		return batch, fmt.Errorf("unit %s was not admitted with a product %s context; a remediation epoch cannot invent one", batch.Origin.UnitID, productID)
	}
	batch.Product = first.Product
	return batch, nil
}

// unitProductContext returns the product context a unit's invocation receives,
// or nil when there is none to deliver. Every inconsistency is an error:
// executing a unit WITHOUT the context it was admitted with, or with another
// product's, is never a quiet fallback.
//
// assignment is the unit's frozen plan assignment, when it has one. Its
// ContextPack already records the operator's ContextPolicy decision; a unit
// without one is judged by its role's stated selection.
func (r *EngineeringRuntime) unitProductContext(state *runState, batch orchestration.Batch, assignment *domain.AgentAssignment) (*product.ProductContext, error) {
	origin := batch.Origin
	productID, associated, err := r.deps.Store.AssociatedProduct(origin.GraphID)
	if err != nil {
		return nil, err
	}
	if !associated {
		if batch.Product != nil {
			return nil, fmt.Errorf("unit %s was admitted with a product context pin and its graph %s has no product association", origin.UnitID, origin.GraphID)
		}
		return nil, nil
	}
	if batch.Product == nil {
		return nil, fmt.Errorf("graph %s is associated with product %s and unit %s was admitted without a product context pin", origin.GraphID, productID, origin.UnitID)
	}
	graph, found, err := r.deps.Store.WorkGraph(origin.GraphID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("work graph %s does not exist", origin.GraphID)
	}
	if err := requireGraphPlanBinding(graph, state.run.Plan); err != nil {
		return nil, err
	}
	owner, found, err := r.deps.Store.CurrentProduct(productID)
	if err != nil {
		return nil, err
	}
	if !found || !slices.Contains(owner.Repositories, graph.Repository) {
		return nil, fmt.Errorf("work graph %s's repository %s is not a member of product %s", graph.ID, graph.Repository, productID)
	}
	entitled, err := receivesProductContext(graph, origin.UnitID, assignment)
	if err != nil || !entitled {
		return nil, err
	}
	resolved, err := batch.Product.Resolve(r.deps.Store, unitContextRequest(productID, *origin))
	if err != nil {
		return nil, fmt.Errorf("resolve the product context unit %s was admitted with: %w", origin.UnitID, err)
	}
	return &resolved, nil
}

// requireGraphPlanBinding refuses a run bound to one plan revision executing a
// graph that binds another, which would deliver the context of work the
// operator approved to a run that was approved for something else.
func requireGraphPlanBinding(graph orchestration.WorkGraph, binding *RunPlanBinding) error {
	if binding == nil {
		return nil
	}
	if graph.ObjectivePlan == nil {
		return fmt.Errorf("run is bound to plan %s and work graph %s binds no plan", binding.PlanID, graph.ID)
	}
	bound := graph.ObjectivePlan
	if bound.ID != binding.PlanID || bound.Revision != binding.Revision || bound.Digest != binding.PlanDigest {
		return errors.New("run's plan binding differs from the plan revision its work graph binds")
	}
	return nil
}

// receivesProductContext decides whether the unit is entitled to product
// context at all. Product configuration and knowledge travel as
// project_model_facts - the closed ContextClass vocabulary has no class of its
// own for them, and a ContextPolicy that excludes the class excludes them too.
// The required governance classes are never the carrier, so a narrowing policy
// can drop this context and can never drop the work contract.
func receivesProductContext(graph orchestration.WorkGraph, unitID string, assignment *domain.AgentAssignment) (bool, error) {
	if assignment != nil {
		return slices.Contains(assignment.Context.Included, domain.ContextProjectFacts), nil
	}
	for _, unit := range graph.Units {
		if unit.ID != unitID {
			continue
		}
		classes, err := planning.RoleContextClasses(unit.Role, nil)
		return classes[domain.ContextProjectFacts], err
	}
	return false, fmt.Errorf("work graph %s has no unit %s", graph.ID, unitID)
}

// executionProductContext narrows a compiled ProductContext to what the worker
// is given. It is the point where configuration stops being a record the
// runtime owns and becomes plain data: Policy and ContextPolicy references do
// not cross it.
func executionProductContext(compiled *product.ProductContext) *execution.ProductContext {
	if compiled == nil {
		return nil
	}
	out := &execution.ProductContext{ProductID: compiled.ProductID, Repositories: compiled.Repositories}
	if compiled.Configuration != nil {
		out.ConfigurationRevision, out.Settings = compiled.Configuration.Revision, compiled.Configuration.Settings
	}
	for _, entry := range compiled.Knowledge {
		out.Knowledge = append(out.Knowledge, execution.ProductKnowledge{
			ID: entry.ID, Scope: string(entry.Scope), Provenance: string(entry.Provenance.Type), Statement: entry.Statement})
	}
	return out
}

// requireBatchProductBinding is the write-time half of the pin: inside the same
// transaction that inserts a unit's batch, the graph's association must agree
// with the batch's pin. An associated graph's batch without its pin, or a pin
// for another product, is refused, so a batch can never be admitted unpinned
// for an associated graph. AssociateWorkGraph's own check reads the batches
// inside ITS transaction; SQLite serializes the two, so neither order leaves an
// associated graph holding an unpinned admitted unit.
func requireBatchProductBinding(q sqlExecutor, batch orchestration.Batch) error {
	if batch.Origin == nil {
		return nil
	}
	var associated string
	err := q.QueryRow(`SELECT product_id FROM product_graph_associations WHERE graph_id = ?`, batch.Origin.GraphID).Scan(&associated)
	if errors.Is(err, sql.ErrNoRows) {
		associated = ""
	} else if err != nil {
		return err
	}
	pinned := ""
	if batch.Product != nil {
		pinned = batch.Product.ProductID
	}
	if pinned != associated {
		return fmt.Errorf("batch %s pins product %q and its graph %s is associated with %q", batch.ID, pinned, batch.Origin.GraphID, associated)
	}
	return nil
}
