package runtime

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// handoffFinding is what one orchestrated run's durable record says about its
// LATEST finished worker invocation's handoff.
type handoffFinding struct {
	observation orchestration.HandoffObservation
	// detail says why a refused handoff is refused, or what a reported one is
	// waiting for.
	detail string
	// reported is set when the latest observation is a valid report.
	reported *HandoffReportedPayload
	// handoffID is the identity that report is (or will be) admitted under.
	handoffID string
}

// inspectHandoff reads the latest handoff observation out of a run's journal.
// It is the one place both the admission pass and the status projection ask
// "where is this run's handoff", so they cannot disagree.
func inspectHandoff(runID string, events []EngineeringEvent, operations map[string]RunOperation, admitted map[string]bool) (handoffFinding, error) {
	var latest *EngineeringEvent
	for i := range events {
		if events[i].Type == EventHandoffReported || events[i].Type == EventHandoffRefused {
			latest = &events[i]
		}
	}
	if latest == nil {
		return handoffFinding{observation: orchestration.HandoffNone}, nil
	}
	if latest.Type == EventHandoffRefused {
		refused, err := decodePayload[HandoffRefusedPayload](latest.Payload)
		if err != nil {
			return handoffFinding{}, err
		}
		return handoffFinding{observation: orchestration.HandoffRefused, detail: refused.Kind + ": " + refused.Detail}, nil
	}
	reported, err := decodePayload[HandoffReportedPayload](latest.Payload)
	if err != nil {
		return handoffFinding{}, err
	}
	id, err := orchestration.HandoffID(runID, reported.OperationID, reported.Attempt)
	if err != nil {
		return handoffFinding{}, err
	}
	finding := handoffFinding{observation: orchestration.HandoffReported, reported: &reported, handoffID: id}
	if admitted[id] {
		finding.observation = orchestration.HandoffAdmitted
		return finding, nil
	}
	// A report from an invocation that changed nothing will never be bound:
	// only a mutation is committed, and a handoff binds to a commit. Saying
	// so is better than reporting it as awaiting a commit forever.
	var result mutationResult
	if producing, ok := operations[reported.OperationID]; ok && producing.State == Succeeded &&
		len(producing.Result) > 0 && json.Unmarshal(producing.Result, &result) == nil && !result.Mutated {
		finding.observation = orchestration.HandoffRefused
		finding.detail = "unbindable: the invocation that reported this handoff changed nothing, so there is no committed candidate to bind it to"
		return finding, nil
	}
	finding.detail = "awaiting the commit of the reporting invocation's output and admission by the supervisor"
	return finding, nil
}

// admitOrchestratedHandoff binds the latest reported handoff of one batch item
// to the exact candidate the runtime committed from that invocation's output,
// and persists it immutably. It does nothing until that commit exists, and
// nothing for a handoff already admitted.
//
// Every binding comes from the runtime's own records: the commit operation
// whose idempotency key IS the reporting operation, that commit's own
// journalled event, and the journal projected up to it. The report is the only
// worker-authored part, and it is admitted only if the slot still holds the
// exact document whose digest was journalled when the invocation completed.
func admitOrchestratedHandoff(store *SQLiteOperationStore, stateDir string, batch orchestration.Batch, item orchestration.BatchItem, now time.Time) error {
	run, found, err := store.Run(item.RunID)
	if err != nil || !found {
		return err
	}
	events, err := store.Events(run.ID)
	if err != nil {
		return err
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		return err
	}
	admitted, err := admittedHandoffs(store, run.ID)
	if err != nil {
		return err
	}
	finding, err := inspectHandoff(run.ID, events, snapshot.Operations, admitted)
	if err != nil || finding.observation != orchestration.HandoffReported {
		return err
	}
	reported := *finding.reported
	commit, committed := succeededOperation(snapshot.Operations, OpCandidateCommit, operationKey(OpCandidateCommit, reported.OperationID))
	if !committed {
		return nil
	}
	upTo, payload := -1, CandidateCommittedPayload{}
	for i, event := range events {
		if event.OperationID != commit.ID {
			continue
		}
		switch event.Type {
		case EventCandidateCommitted:
			if payload, err = decodePayload[CandidateCommittedPayload](event.Payload); err != nil {
				return err
			}
			upTo = i
		case EventReassessmentCompleted:
			upTo = max(upTo, i)
		}
	}
	if payload.Commit == "" {
		return fmt.Errorf("handoff not admitted: the commit of the reporting invocation journalled no completed candidate (run %s attempt %d)", run.ID, reported.Attempt)
	}
	// The run AS IT STOOD when that commit landed, so the base and contract
	// bound are the ones the commit was made and reassessed against, read
	// through the runtime's own accessors rather than re-derived here.
	prefix := events[:upTo+1]
	prior, err := Reduce(run, prefix)
	if err != nil {
		return err
	}
	projected, err := Project(prefix)
	if err != nil {
		return err
	}
	atCommit := &runState{run: run, snapshot: prior, events: prefix, projection: projected}
	atCommit.collectSources()
	path, err := HandoffReportPath(stateDir, ExecutionAttemptRef{RunID: run.ID, OperationID: reported.OperationID, Attempt: reported.Attempt})
	if err != nil {
		return err
	}
	report, digest, present, err := readHandoffReport(path)
	if !present || err != nil || digest != reported.ReportSHA256 {
		return fmt.Errorf("handoff not admitted: the report is no longer the document journalled when the invocation completed (run %s attempt %d, present=%t, error=%v)",
			run.ID, reported.Attempt, present, err)
	}
	_, err = store.AdmitHandoff(orchestration.EngineeringHandoff{
		SchemaVersion: orchestration.EngineeringHandoffSchemaVersion,
		ID:            finding.handoffID, BatchID: batch.ID, Issue: item.Issue, RunID: run.ID,
		Producer: orchestration.HandoffProducer{AgentID: run.AgentID, OperationID: reported.OperationID, Attempt: reported.Attempt},
		Subject: orchestration.HandoffSubject{
			BaseRevision: atCommit.baseRevision(), CandidateRevision: payload.Commit, CandidateTree: payload.Tree,
		},
		Governance:   orchestration.HandoffGovernance{ContractID: projected.Contract.ID, ContractRevision: projected.Contract.Revision},
		Observed:     orchestration.HandoffObserved{ChangedPathCount: payload.PathCount, ChangedPathsDigest: payload.PathsDigest},
		ReportSHA256: digest, ProducerReport: report, AdmittedAt: now,
	})
	return err
}

func admittedHandoffs(store *SQLiteOperationStore, runID string) (map[string]bool, error) {
	handoffs, err := store.RunHandoffs(runID)
	if err != nil {
		return nil, err
	}
	admitted := make(map[string]bool, len(handoffs))
	for _, handoff := range handoffs {
		admitted[handoff.ID] = true
	}
	return admitted, nil
}

func succeededOperation(operations map[string]RunOperation, kind, key string) (RunOperation, bool) {
	for _, op := range operations {
		if op.Kind == kind && op.IdempotencyKey == key && op.State == Succeeded {
			return op, true
		}
	}
	return RunOperation{}, false
}
