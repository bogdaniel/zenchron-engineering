package runtime

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// A plan stage built on an objective WorkGraph unit follows that unit's
// CURRENT run (#480).
//
// The stage records the run it was started with. When the unit is remediated
// through a governed epoch (#581), the work continues in the epoch's run and the
// first run stays parked at whatever it stopped on - a conflict, a finding. A
// reader that kept judging the first run would never settle the stage, would
// judge a downstream gate on superseded evidence, and would hold the
// candidate's publication behind a gate that can no longer open.
//
// The epoch table is the authority for which run is current, so the plan
// journal is not rewritten and no second record of the run is kept: the
// original run stays in the journal, and the snapshot is read through this
// overlay by every consumer.

// followRemediationEpochs points each remediated unit's stage at the unit's
// latest epoch run.
func (s *SQLiteOperationStore) followRemediationEpochs(snapshot PlanSnapshot) (PlanSnapshot, error) {
	graph, err := objectiveGraphFor(s, snapshot.PlanID)
	if err != nil || graph == nil {
		return snapshot, err
	}
	for _, unit := range graph.Units {
		projection, ok := snapshot.Stages[unit.ID]
		if !ok || projection.RunID == "" {
			continue
		}
		epochs, err := s.WorkUnitRemediationEpochs(graph.ID, unit.ID)
		if err != nil {
			return snapshot, err
		}
		if len(epochs) == 0 {
			continue
		}
		current := epochs[len(epochs)-1]
		adopt, err := s.refuseForeignEpoch(*graph.ObjectivePlan, graph.ID, unit.ID, current)
		if err != nil {
			return snapshot, err
		}
		if !adopt {
			continue
		}
		projection.RunID = current.RunID
		snapshot.Stages[unit.ID] = projection
	}
	return snapshot, nil
}

// refuseForeignEpoch makes the overlay adopt an epoch run only when everything
// that names it agrees: the graph and unit, the epoch's batch (which must be that
// graph's epoch of that unit, and name this run), and - once the run exists - its
// binding to the approved plan revision and stage. Anything else would let a run
// the plan never granted stand in for the stage.
//
// adopt is false for a run created before epochs were plan-bound: it is not the
// stage's run, so the stage keeps the run it was started with.
func (s *SQLiteOperationStore) refuseForeignEpoch(ref domain.PlanRef, graphID, unitID string, epoch RemediationEpoch) (adopt bool, err error) {
	if epoch.GraphID != graphID || epoch.UnitID != unitID {
		return false, fmt.Errorf("remediation epoch %d of %s/%s does not belong to graph %s unit %s", epoch.Epoch, epoch.GraphID, epoch.UnitID, graphID, unitID)
	}
	batch, found, err := s.OrchestrationBatch(epoch.BatchID)
	if err != nil {
		return false, err
	}
	if !found || batch.Origin == nil || batch.Origin.GraphID != graphID || batch.Origin.UnitID != unitID ||
		batch.Origin.Epoch != epoch.Epoch || len(batch.Items) != 1 || batch.Items[0].RunID != epoch.RunID {
		return false, fmt.Errorf("epoch %d of %s/%s names batch %s, which is not that epoch's batch", epoch.Epoch, graphID, unitID, epoch.BatchID)
	}
	run, found, err := s.Run(epoch.RunID)
	if err != nil {
		return false, err
	}
	if !found {
		return true, nil
	}
	if run.Orchestration == nil || run.Orchestration.BatchID != epoch.BatchID {
		return false, fmt.Errorf("epoch run %s is not the run of batch %s", epoch.RunID, epoch.BatchID)
	}
	if run.Plan == nil {
		return false, nil
	}
	if run.Plan.PlanID != ref.ID || run.Plan.StageID != unitID || run.Plan.Revision != ref.Revision || run.Plan.PlanDigest != ref.Digest {
		return false, fmt.Errorf("epoch run %s is not bound to plan %s revision %d stage %s", epoch.RunID, ref.ID, ref.Revision, unitID)
	}
	return true, nil
}
