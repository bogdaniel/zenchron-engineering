package runtime

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
		projection.RunID = epochs[len(epochs)-1].RunID
		snapshot.Stages[unit.ID] = projection
	}
	return snapshot, nil
}
