package runtime

// Remediation epoch activation (#581 Batch 2): claiming one unit's Nth
// attempt, once a durable RemediationAuthorization exists for it.
//
// Mirrors workgraph.go's activateWorkUnit for everything EXCEPT the one
// place independent review (PR #585) found it could not: #472's own first
// activation gets away with two decoupled, idempotent steps - write the
// batch, then record the claim - because nothing downstream ever
// materializes a run from a batch #472 itself did not just write in this
// same call. reconcileOrchestration is not that narrow: it materializes a
// run for EVERY batch its own unconditional OrchestrationBatches() read
// returns, on every pass, regardless of who wrote it or when. Decoupling
// this batch's write from its epoch claim would leave exactly the window
// that guard exists to prevent - a batch visible, and therefore
// materializable and schedulable, with no governing claim on it at all, if
// a crash lands between the two writes. AdmitRemediationEpochBatch
// (remediation_store.go) is the fix: one transaction, both rows, so a batch
// is never visible without its claim already alongside it. BatchOrigin.Epoch
// (orchestration/batch.go) remains the one fact that makes a remediation
// epoch's batch/run identity genuinely distinct from epoch 1's.

import (
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// pendingRemediationEpoch reports the one authorization, if any, whose
// proposed epoch is exactly this unit's next attempt and has not yet been
// activated - the single fact that decides whether activateRemediationEpoch
// has anything to do for this unit this pass.
func pendingRemediationEpoch(store *SQLiteOperationStore, graphID, unitID string) (RemediationAuthorization, bool, error) {
	epochs, err := store.WorkUnitRemediationEpochs(graphID, unitID)
	if err != nil {
		return RemediationAuthorization{}, false, err
	}
	next := latestAttemptEpoch(epochs) + 1
	authorizations, err := store.RemediationAuthorizationsFor(graphID, unitID)
	if err != nil {
		return RemediationAuthorization{}, false, err
	}
	for _, authorization := range authorizations {
		if authorization.ProposedEpoch == next {
			return authorization, true, nil
		}
	}
	return RemediationAuthorization{}, false, nil
}

// activateRemediationEpoch claims one unit's remediation attempt, exactly
// once - the epoch-aware sibling of activateWorkUnit. The caller holds
// s.orchestrationMu, for the identical reason activateWorkUnit's own doc
// comment states: deciding a child run identity must not race another
// writer deciding the same one.
func (s *Supervisor) activateRemediationEpoch(graph WorkGraphView, unit WorkGraphUnitView, authorization RemediationAuthorization) error {
	if unit.UnitID == "" {
		return errors.New("the frontier names a unit this projection does not contain")
	}
	if unit.InputsDigest == "" {
		return fmt.Errorf("unit %s has no consumed-inputs digest to activate epoch %d against", unit.UnitID, authorization.ProposedEpoch)
	}
	engine, err := s.engine(graph.Repository, graph.AgentID)
	if err != nil {
		return err
	}
	// BatchOrigin.Epoch is the one fact that makes this batch's identity
	// genuinely distinct from epoch 1's (and from any other epoch of the
	// same unit): same graph, same unit, same exact input set, DIFFERENT
	// epoch, DIFFERENT WorkUnitBatchID - never a second claim on epoch 1's
	// own identity, and never collapsed onto it.
	origin := orchestration.BatchOrigin{GraphID: graph.GraphID, UnitID: unit.UnitID, Inputs: unit.Inputs,
		ExecutionKind: unit.ExecutionKind, Epoch: authorization.ProposedEpoch}
	batchID, err := orchestration.WorkUnitBatchID(graph.Repository, graph.AgentID, unit.Issue, origin)
	if err != nil {
		return err
	}
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
		// NOT engine.planOrchestrationBatch: that function's own liveness
		// guard (planOrchestrationBatch, runtime/orchestration.go) treats
		// any run whose Disposition is Waiting as still live - correct for
		// #470's own ordinary "may I submit a second batch for this issue"
		// question, but WRONG here. A producer unit that finished its goal
		// settles into Waiting/ReasonGoalStateReached PERMANENTLY
		// (disposition.go, orchestration_lifecycle.go, plan_reconciler.go
		// all treat that pair the same way this one does) - it is
		// functionally done, not "still doing something", and reusing the
		// ordinary guard unchanged would refuse to ever create a
		// remediation epoch for ANY unit, every time (confirmed by running
		// this exact scenario before this fix: "issue N already has live
		// run ...; an orchestration batch does not adopt or race work it
		// did not create"). planRemediationEpochBatch below is the
		// narrowly-scoped sibling that recognizes goal-reached as settled,
		// without touching #470's own Orchestrate/planOrchestrationBatch
		// behavior at all.
		planned, err := engine.planRemediationEpochBatch(batchID, unit.Issue, graph.RequestedBy, reserved, origin)
		if err != nil {
			return err
		}
		// Independent review finding (PR #585): writing this batch and
		// claiming its epoch row as two separate calls leaves a window - a
		// crash between them - where the batch is already visible to
		// reconcileOrchestration's own unconditional materialization of
		// every stored batch, with no governing epoch claim to say it was
		// ever authorized. AdmitRemediationEpochBatch closes that window by
		// construction: both rows commit in ONE transaction, so this batch
		// is NEVER visible to anything reading orchestration_batches
		// without its epoch claim already alongside it, and a crash before
		// that commit leaves nothing - not a stray batch, not a stray run -
		// for a retry to find except the identical durable state it started
		// from.
		epoch := RemediationEpoch{
			GraphID: graph.GraphID, UnitID: unit.UnitID, Epoch: authorization.ProposedEpoch,
			BatchID: batchID, RunID: planned.Items[0].RunID, InputsDigest: unit.InputsDigest,
			AuthorizationID: authorization.ID, ActivatedAt: s.deps.Clock.Now(),
		}
		if stored, _, err = s.deps.Store.AdmitRemediationEpochBatch(planned, epoch); err != nil {
			return err
		}
		return validateRemediationEpochBatch(stored, graph, unit, authorization)
	}
	// Replay: the batch already exists, which - by AdmitRemediationEpochBatch's
	// own atomicity - means its epoch claim already does too. Nothing left
	// to do but confirm the shape, exactly as a first attempt's own result
	// would.
	return validateRemediationEpochBatch(stored, graph, unit, authorization)
}

// validateRemediationEpochBatch confirms a stored batch - freshly admitted
// or found on replay - actually describes the exact unit execution this
// call asked for. Shared so both paths through activateRemediationEpoch
// check the identical thing.
func validateRemediationEpochBatch(stored orchestration.Batch, graph WorkGraphView, unit WorkGraphUnitView, authorization RemediationAuthorization) error {
	if len(stored.Items) != 1 || stored.Items[0].Issue != unit.Issue {
		return fmt.Errorf("batch %s does not describe issue %d alone", stored.ID, unit.Issue)
	}
	if stored.Origin == nil || stored.Origin.GraphID != graph.GraphID || stored.Origin.UnitID != unit.UnitID || stored.Origin.Epoch != authorization.ProposedEpoch {
		return fmt.Errorf("batch %s does not describe unit %s of graph %s at epoch %d", stored.ID, unit.UnitID, graph.GraphID, authorization.ProposedEpoch)
	}
	return nil
}

// goalSettled is runGoalSettled's whole-run form: true for any disposition
// terminalDisposition already recognizes, PLUS Waiting/ReasonGoalStateReached
// - the permanent resting state an ordinary unit's run reaches once its own
// goal (an admitted completed handoff) is satisfied, which #470's own
// terminalDisposition deliberately does not count as terminal (it still
// means "live" for #470's own purposes: a run resting there could still be
// cancelled, paused, or have new feedback observed). For deciding whether
// THIS issue's identity is free for a genuinely new remediation attempt,
// goal-reached is exactly as settled as any other terminal disposition.
func goalSettled(run EngineeringRun) bool {
	if !terminalDisposition(run.Disposition) && run.Disposition != Waiting {
		return false
	}
	switch run.Reason {
	case ReasonGoalStateReached:
		// An ordinary producer that reached its goal: its own work is done,
		// permanently - the whole reason this function needed to exist.
		return true
	case ReasonDeterministicFailureUnchanged:
		// #475's own park-on-identical-attempt: an integration_compose unit
		// parked here will NEVER make progress on its own (the same inputs
		// will always reproduce the same conflict) - only a genuinely new
		// input set, i.e. exactly a remediation epoch, can ever move it.
		return true
	}
	return terminalDisposition(run.Disposition)
}

// planRemediationEpochBatch is planOrchestrationBatch's (runtime/orchestration.go)
// epoch-aware sibling: identical shape, one issue, one deterministic batch,
// decided before anything is written - except its liveness guard uses
// goalSettled, not terminalDisposition alone, so a producer resting at its
// own completed goal is correctly free to be superseded by a new epoch.
// #470's own planOrchestrationBatch/Orchestrate are completely untouched.
func (r *EngineeringRuntime) planRemediationEpochBatch(id string, issue int, requestedBy string,
	reserved map[string]string, origin orchestration.BatchOrigin) (orchestration.Batch, error) {
	live, err := r.deps.Store.ActiveRuns()
	if err != nil {
		return orchestration.Batch{}, err
	}
	goal := issueGoal(r.deps.Repository.Identity, issue)
	for _, run := range live {
		if run.Goal == goal && !goalSettled(run) {
			return orchestration.Batch{}, &OrchestrationConflictError{Issue: issue, RunID: run.ID}
		}
	}
	runID, err := r.freeRemediationEpochRunID(issue, goal)
	if err != nil {
		return orchestration.Batch{}, err
	}
	if owner, taken := reserved[runID]; taken {
		return orchestration.Batch{}, &OrchestrationConflictError{Issue: issue, RunID: runID, Batch: owner}
	}
	return orchestration.Batch{
		SchemaVersion: orchestration.BatchSchemaVersion, ID: id,
		Repository: r.deps.Repository.Identity, AgentID: r.deps.Agent.ID,
		RequestedBy: BoundedNote(requestedBy), CreatedAt: r.deps.Clock.Now(), Origin: &origin,
		Items: []orchestration.BatchItem{{Issue: issue, RunID: runID}},
	}, nil
}

// freeRemediationEpochRunID is freeIssueRunID's (runtime/orchestration.go)
// goalSettled-aware sibling: the same generation-probing identity space
// StartIssueRun and an ordinary orchestrated child already share, so a
// remediation epoch's run is an ordinary run under this issue's own
// existing generation sequence - never a parallel identity space - that a
// later `run issue` or restart finds and resumes exactly like any other.
func (r *EngineeringRuntime) freeRemediationEpochRunID(issue int, goal string) (string, error) {
	for generation := 0; generation < maxRunGenerations; generation++ {
		runID, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return "", err
		}
		existing, found, err := r.deps.Store.Run(runID)
		if err != nil {
			return "", err
		}
		if !found {
			return runID, nil
		}
		if existing.Repository != r.deps.Repository.Identity || existing.Goal != goal {
			return "", &RunConflictError{RunID: runID, Detail: "durable run describes different work"}
		}
		if !goalSettled(existing) {
			return "", &OrchestrationConflictError{Issue: issue, RunID: runID}
		}
	}
	return "", fmt.Errorf("issue %d has exhausted %d run generations", issue, maxRunGenerations)
}
