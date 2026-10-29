package runtime

import (
	"encoding/json"
	"fmt"
	"strings"
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
	// admitted is the durable handoff, when the latest report was admitted.
	admitted *orchestration.EngineeringHandoff
}

// handoffRecords are the orchestration layer's own durable decisions about one
// run's reports: the admitted handoffs and the refused identities.
type handoffRecords struct {
	admitted map[string]orchestration.EngineeringHandoff
	refused  map[string]string
}

// orchestrationHandoffStore is the durable admission boundary. Tests interleave
// an independent journal writer at the conditional insert, after binding proof.
type orchestrationHandoffStore interface {
	Run(string) (EngineeringRun, bool, error)
	Events(string) ([]EngineeringEvent, error)
	RunHandoffs(string) ([]orchestration.EngineeringHandoff, error)
	RunHandoffRefusals(string) (map[string]string, error)
	RefuseHandoff(string, string, string, string, time.Time) error
	AdmitHandoff(orchestration.EngineeringHandoff) (bool, error)
	AdmitCheckpointHandoff(orchestration.EngineeringHandoff, int64) (bool, error)
	AdmitIntegratedHandoff(orchestration.EngineeringHandoff, orchestration.WorkUnitInputs) (bool, error)
}

func loadHandoffRecords(store orchestrationHandoffStore, runID string) (handoffRecords, error) {
	handoffs, err := store.RunHandoffs(runID)
	if err != nil {
		return handoffRecords{}, err
	}
	refused, err := store.RunHandoffRefusals(runID)
	if err != nil {
		return handoffRecords{}, err
	}
	records := handoffRecords{admitted: make(map[string]orchestration.EngineeringHandoff, len(handoffs)), refused: refused}
	for _, handoff := range handoffs {
		records.admitted[handoff.ID] = handoff
	}
	return records, nil
}

// inspectHandoff reads the latest handoff observation out of a run's journal.
// It is the one place both the admission pass and the status projection ask
// "where is this run's handoff", so they cannot disagree.
func inspectHandoff(runID string, events []EngineeringEvent, operations map[string]RunOperation, records handoffRecords) (handoffFinding, error) {
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
	if handoff, ok := records.admitted[id]; ok {
		finding.observation, finding.admitted = orchestration.HandoffAdmitted, &handoff
		return finding, nil
	}
	if reason, ok := records.refused[id]; ok {
		finding.observation, finding.detail = orchestration.HandoffRefused, "unadmittable: "+reason
		return finding, nil
	}
	// A report from an invocation that changed nothing will never be bound:
	// only a mutation is committed, and a handoff binds to a commit. Saying
	// so is better than reporting it as awaiting a commit forever. The one
	// exception is a continuation that completed its checkpoint (#489): that
	// checkpoint IS a runtime-owned commit, and admission decides it.
	var result mutationResult
	if producing, ok := operations[reported.OperationID]; ok && producing.State == Succeeded &&
		len(producing.Result) > 0 && json.Unmarshal(producing.Result, &result) == nil && !result.Mutated {
		if _, continued := completedCheckpoint(producing); !continued {
			finding.observation = orchestration.HandoffRefused
			finding.detail = "unbindable: the invocation that reported this handoff changed nothing, so there is no committed candidate to bind it to"
			return finding, nil
		}
	}
	finding.detail = "awaiting the commit of the reporting invocation's output and admission by the supervisor"
	return finding, nil
}

// admitOrchestratedHandoff binds the latest reported handoff of one batch item
// to the exact candidate the runtime committed for that invocation, and
// persists it immutably. It does nothing until that commit exists, and nothing
// for a handoff already admitted.
//
// Every binding comes from the runtime's own records: the commit operation
// handoffCommitOf names, that commit's own journalled event, and the journal
// projected up to it. The report is the only worker-authored part, and it is
// admitted only if the slot still holds the exact document whose digest was
// journalled when the invocation completed.
func admitOrchestratedHandoff(store orchestrationHandoffStore, stateDir string, batch orchestration.Batch, item orchestration.BatchItem, now time.Time) error {
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
	records, err := loadHandoffRecords(store, run.ID)
	if err != nil {
		return err
	}
	finding, err := inspectHandoff(run.ID, events, snapshot.Operations, records)
	if err != nil || finding.observation != orchestration.HandoffReported {
		return err
	}
	reported := *finding.reported
	// From here on, a report that cannot be admitted is a DECISION, recorded
	// once, not an error repeated every pass: the item settles to
	// handoff_pending with the reason instead of reading as running forever.
	refuse := func(reason string) error {
		return store.RefuseHandoff(finding.handoffID, batch.ID, run.ID, reason, now)
	}
	commit, refusal, err := handoffCommitOf(snapshot.Operations, events, reported.OperationID)
	if err != nil {
		return err
	}
	if refusal != "" {
		return refuse(refusal)
	}
	if commit.operationID == "" {
		return nil
	}
	upTo, payload := -1, CandidateCommittedPayload{}
	for i, event := range events {
		if event.OperationID != commit.operationID {
			continue
		}
		switch event.Type {
		case commit.event:
			if payload, err = decodePayload[CandidateCommittedPayload](event.Payload); err != nil {
				return err
			}
			upTo = i
		case EventReassessmentCompleted:
			upTo = max(upTo, i)
		}
	}
	if payload.Commit == "" {
		return refuse("the commit this handoff binds to journalled no candidate")
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
	path, err := handoffReportPathOf(stateDir, run.ID, reported)
	if err != nil {
		return err
	}
	report, digest, present, err := readHandoffReport(path)
	if !present || err != nil || digest != reported.ReportSHA256 {
		return refuse(fmt.Sprintf("the report is no longer the document journalled when the invocation completed (present=%t, error=%v)", present, err))
	}
	handoff := orchestration.EngineeringHandoff{
		SchemaVersion: orchestration.EngineeringHandoffSchemaVersion,
		ID:            finding.handoffID, BatchID: batch.ID, Issue: item.Issue, RunID: run.ID,
		Producer: orchestration.HandoffProducer{AgentID: run.AgentID, OperationID: reported.OperationID, Attempt: reported.Attempt},
		Subject: orchestration.HandoffSubject{
			BaseRevision: atCommit.baseRevision(), CandidateRevision: payload.Commit, CandidateTree: payload.Tree,
		},
		Governance:   orchestration.HandoffGovernance{ContractID: projected.Contract.ID, ContractRevision: projected.Contract.Revision},
		Observed:     orchestration.HandoffObserved{ChangedPathCount: payload.PathCount, ChangedPathsDigest: payload.PathsDigest},
		ReportSHA256: digest, ProducerReport: report, AdmittedAt: now,
	}
	if reported.RepairOperationID != "" {
		handoff.ProtocolRepair = &orchestration.HandoffProducer{
			AgentID: run.AgentID, OperationID: reported.RepairOperationID, Attempt: reported.RepairAttempt,
		}
	}
	if batch.Origin != nil && batch.Origin.ExecutionKind == orchestration.ExecutionKindIntegrationCompose {
		// The exact upstream handoff identities this unit consumed are bound
		// INTO the same conditional INSERT as the admission itself (#475
		// review B3): SQLite evaluates the WHERE clause and the row write as
		// one statement, so there is no separate read-then-write window a
		// concurrent supersession could land in. A lost insert here is a
		// real refusal, never silently retried forever: the one recorded
		// decision is what later passes find.
		inserted, err := store.AdmitIntegratedHandoff(handoff, batch.Origin.Inputs)
		if err != nil {
			return err
		}
		if !inserted {
			return refuse("a consumed input was superseded between the live freshness check and the atomic handoff admission")
		}
		return nil
	}
	if commit.event == EventCandidateCheckpointed {
		// The snapshot proves the binding; SQLite must keep that proof current
		// through insertion despite journal appends by other run drivers.
		// A lost insert leaves no handoff; the next pass replays and refuses
		// the superseded checkpoint (or finds an already-admitted handoff).
		_, err = store.AdmitCheckpointHandoff(handoff, events[len(events)-1].Sequence)
		return err
	}
	_, err = store.AdmitHandoff(handoff)
	return err
}

// handoffCommit is the runtime-owned commit a reported handoff binds to: the
// commit operation, and the event type that journalled its candidate.
type handoffCommit struct {
	operationID string
	event       string
}

// handoffCommitOf decides which runtime-owned commit the reporting invocation's
// handoff binds to. A zero commit with no refusal means admission waits.
//
//  1. An invocation whose output the runtime committed binds to that commit.
//  2. A continuation that changed nothing and completed its checkpoint binds
//     to that checkpoint, under checkpointCommitOf's proof (#489).
//  3. Anything else has no commit to bind to (inspectHandoff says so).
func handoffCommitOf(operations map[string]RunOperation, events []EngineeringEvent, reportingID string) (handoffCommit, string, error) {
	if commit, ok := succeededOperation(operations, OpCandidateCommit, operationKey(OpCandidateCommit, reportingID)); ok {
		return handoffCommit{commit.ID, EventCandidateCommitted}, "", nil
	}
	// A WORK GRAPH INTEGRATION UNIT (#475) commits its own merge result
	// directly - the commit already exists in Git when composition succeeds,
	// so there is no separate uncommitted diff for a candidate.commit
	// operation to turn into one - but its handoff report is journalled by a
	// SEPARATE, LATER operation (OpIntegrationHandoffAdmit, review B2),
	// reached only once its own fresh assurance has passed. The commit this
	// binds to is therefore this run's own succeeded OpIntegrationCompose,
	// never the reporting operation itself.
	if op, ok := operations[reportingID]; ok && op.Kind == OpIntegrationHandoffAdmit && op.State == Succeeded {
		for _, candidate := range operations {
			if candidate.Kind == OpIntegrationCompose && candidate.State == Succeeded {
				return handoffCommit{candidate.ID, EventCandidateCommitted}, "", nil
			}
		}
	}
	checkpoint, ok := completedCheckpoint(operations[reportingID])
	if !ok {
		return handoffCommit{}, "", nil
	}
	return checkpointCommitOf(events, reportingID, checkpoint)
}

// completedCheckpoint names the checkpoint commit a succeeded, zero-delta
// continuation was created to complete, from the operation's own durable
// binding (continuation|<commit>) and the runtime's admission of its
// checkpoint-completion claim - never from anything the provider wrote.
func completedCheckpoint(op RunOperation) (string, bool) {
	var result mutationResult
	if op.Kind != OpExecutionInvoke || op.State != Succeeded || len(op.Result) == 0 ||
		json.Unmarshal(op.Result, &result) != nil || result.Mutated || !result.CheckpointResolved {
		return "", false
	}
	checkpoint, continued := strings.CutPrefix(bindingOf(op), invocationContinuationPrefix)
	return checkpoint, continued && checkpoint != ""
}

// checkpointCommitOf is the specific binding proof for a continuation that
// completed checkpoint C without a newer mutation (#489). It is NOT "there is
// a current candidate, so bind to it": each answer comes from the journal,
// and any answer that is not exactly one is a refusal.
//
//   - C was journalled as a checkpoint exactly once, by a runtime commit;
//   - nothing after that checkpoint moved the candidate head, so C is still
//     the run's current candidate and nothing superseded it;
//   - the reporting continuation's own execution.completed names C's exact
//     commit and tree as the subject it completed.
func checkpointCommitOf(events []EngineeringEvent, reportingID, checkpoint string) (handoffCommit, string, error) {
	at, found := -1, 0
	var tree string
	for i, event := range events {
		if event.Type != EventCandidateCheckpointed {
			continue
		}
		payload, err := decodePayload[CandidateCommittedPayload](event.Payload)
		if err != nil {
			return handoffCommit{}, "", err
		}
		if payload.Commit == checkpoint {
			at, tree, found = i, payload.Tree, found+1
		}
	}
	if found != 1 {
		return handoffCommit{}, fmt.Sprintf("unbindable: the continuation's checkpoint %s is journalled %d times, not exactly once", checkpoint, found), nil
	}
	completed := false
	for _, event := range events[at+1:] {
		switch event.Type {
		case EventCandidateCommitted, EventCandidateCheckpointed, EventCandidateBaseIntegrated:
			return handoffCommit{}, fmt.Sprintf("unbindable: checkpoint %s was superseded as the run's candidate before its continuation's handoff was admitted", checkpoint), nil
		case EventExecutionCompleted:
			if event.OperationID != reportingID {
				continue
			}
			payload, err := decodePayload[ExecutionCompletedPayload](event.Payload)
			if err != nil {
				return handoffCommit{}, "", err
			}
			completed = payload.SubjectCommit == checkpoint && payload.SubjectTree == tree
		}
	}
	if !completed {
		return handoffCommit{}, fmt.Sprintf("unbindable: the continuation's completion is not journalled against checkpoint %s and its exact tree", checkpoint), nil
	}
	return handoffCommit{events[at].OperationID, EventCandidateCheckpointed}, "", nil
}

func succeededOperation(operations map[string]RunOperation, kind, key string) (RunOperation, bool) {
	for _, op := range operations {
		if op.Kind == kind && op.IdempotencyKey == key && op.State == Succeeded {
			return op, true
		}
	}
	return RunOperation{}, false
}
