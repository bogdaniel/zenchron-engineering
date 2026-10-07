package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// OrchestrationView is one batch read through its children: the aggregate an
// operator watches instead of one terminal per run. It is read-only and
// derived entirely from durable state, so it reads the same whether or not a
// supervisor is running, and the same after a restart.
type OrchestrationView struct {
	BatchID     string               `json:"batch_id"`
	Repository  string               `json:"repository"`
	AgentID     string               `json:"agent_id"`
	RequestedBy string               `json:"requested_by,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	Counts      orchestration.Counts `json:"counts"`
	// HandoffRepairs counts the batch's one-per-invocation handoff protocol
	// repairs (#492) that were started, and how many of those were repaired.
	HandoffRepairs HandoffRepairCounts     `json:"handoff_repairs"`
	Items          []OrchestrationItemView `json:"items"`
}

// HandoffRepairCounts is the batch aggregate of handoff protocol repairs.
type HandoffRepairCounts struct {
	Started  int `json:"started"`
	Repaired int `json:"repaired"`
}

// OrchestrationItemView is one item: its projected state beside the child
// run facts it was projected from. Run-level `status` and `logs` remain the
// drill-down for anything more.
type OrchestrationItemView struct {
	Issue   int                     `json:"issue"`
	RunID   string                  `json:"run_id"`
	AgentID string                  `json:"agent_id,omitempty"`
	State   orchestration.ItemState `json:"state"`
	// Reason is the waiting, blocking, refusal or read-failure explanation
	// for this item, when there is one.
	Reason   string        `json:"reason,omitempty"`
	Capacity *CapacityWait `json:"capacity,omitempty"`
	Paused   *RunPause     `json:"paused,omitempty"`
	// Observation marks a durable transition between journal and row writes.
	Observation       string      `json:"observation,omitempty"`
	Phase             Phase       `json:"phase,omitempty"`
	Disposition       Disposition `json:"disposition,omitempty"`
	Executing         bool        `json:"executing,omitempty"`
	Branch            string      `json:"branch,omitempty"`
	CandidateRevision string      `json:"candidate_revision,omitempty"`
	CandidateTree     string      `json:"candidate_tree,omitempty"`
	PullRequest       int         `json:"pull_request,omitempty"`
	PRState           string      `json:"pull_request_state,omitempty"`
	// Handoff is the latest handoff observation, and HandoffID the latest
	// admitted handoff's identity, when one exists.
	Handoff       orchestration.HandoffObservation `json:"handoff"`
	HandoffID     string                           `json:"handoff_id,omitempty"`
	HandoffReason string                           `json:"handoff_reason,omitempty"`
	// HandoffRepair is where the latest handoff's one protocol repair stands
	// (#492): pending, ineligible, running, interrupted, repaired, refused or
	// failed; empty when no repair applies.
	HandoffRepair       string               `json:"handoff_repair,omitempty"`
	HandoffRepairDetail string               `json:"handoff_repair_detail,omitempty"`
	Operation           string               `json:"operation,omitempty"`
	VerificationTools   []VerificationPermit `json:"verification_tools,omitempty"`
}

// OrchestrationStatus projects one batch. A child that cannot be read is
// reported on its own item and counted as unknown; it never hides its
// siblings.
func OrchestrationStatus(store *SQLiteOperationStore, stateDir, batchID string, now time.Time) (OrchestrationView, error) {
	batch, found, err := store.OrchestrationBatch(batchID)
	if err != nil {
		return OrchestrationView{}, err
	}
	if !found {
		return OrchestrationView{}, fmt.Errorf("unknown orchestration batch %q", batchID)
	}
	tx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OrchestrationView{}, err
	}
	defer tx.Rollback()
	view := OrchestrationView{
		BatchID: batch.ID, Repository: batch.Repository, AgentID: batch.AgentID,
		RequestedBy: batch.RequestedBy, CreatedAt: batch.CreatedAt,
	}
	for _, item := range batch.Items {
		projected := projectOrchestrationItem(tx, stateDir, item, now)
		view.Counts.Add(projected.State)
		switch projected.HandoffRepair {
		case HandoffRepairRepaired:
			view.HandoffRepairs.Repaired++
			view.HandoffRepairs.Started++
		case HandoffRepairRunning, HandoffRepairInterrupted, HandoffRepairRefused, HandoffRepairFailed:
			view.HandoffRepairs.Started++
		}
		view.Items = append(view.Items, projected)
	}
	if err := tx.Commit(); err != nil {
		return OrchestrationView{}, err
	}
	return view, nil
}

func projectOrchestrationItem(tx *sql.Tx, stateDir string, item orchestration.BatchItem, now time.Time) OrchestrationItemView {
	out := OrchestrationItemView{Issue: item.Issue, RunID: item.RunID, Handoff: orchestration.HandoffNone}
	fail := func(err error) OrchestrationItemView {
		out.State, out.Reason = "", boundedDetail(err.Error())
		return out
	}
	var document string
	err := tx.QueryRow(`SELECT document FROM runs WHERE id = ?`, item.RunID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		out.State = orchestration.ItemNotCreated
		return out
	}
	if err != nil {
		return fail(err)
	}
	run, err := decodeRun(document)
	if err != nil {
		return fail(err)
	}
	events, err := queryEvents(tx, run.ID)
	if err != nil {
		return fail(err)
	}
	rows, err := tx.Query(`SELECT document FROM run_operations WHERE run_id = ? ORDER BY created_unix_nano ASC, id ASC`, run.ID)
	if err != nil {
		return fail(err)
	}
	owned, err := scanOperations(rows)
	if err != nil {
		return fail(err)
	}
	operations := make(map[string]RunOperation, len(owned))
	for _, op := range owned {
		operations[op.ID] = op
	}
	summary := summarizeRunEvents(stateDir, run, events, now)
	if summary.Error != "" {
		return fail(fmt.Errorf("%s", summary.Error))
	}
	out.AgentID, out.Phase, out.Disposition, out.Reason = summary.Agent, summary.Phase, summary.Disposition, summary.Reason
	out.Paused = summary.Paused
	out.Branch = summary.Branch
	out.CandidateRevision, out.CandidateTree = summary.CandidateRevision, summary.CandidateTree
	out.PullRequest, out.PRState = summary.PullRequest, summary.PRState
	for _, op := range owned {
		if op.Lease != nil && (op.State == Leased || op.State == Running) {
			out.Executing, out.Operation = true, op.Kind
		}
	}
	toolRows, err := tx.Query(`SELECT id, document FROM verification_permits
		WHERE json_extract(document, '$.parent.RunID') = ?
		AND COALESCE(json_extract(document, '$.state'), '') <> 'released' ORDER BY id`, run.ID)
	if err != nil {
		return fail(err)
	}
	out.VerificationTools, err = scanVerificationPermits(toolRows)
	if err != nil {
		return fail(err)
	}
	for _, permit := range out.VerificationTools {
		if permit.State == VerificationGranted {
			out.Executing = true
		}
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		return fail(err)
	}
	admitted, err := queryRunHandoffs(tx, run.ID)
	if err != nil {
		return fail(err)
	}
	refused, err := queryRunHandoffRefusals(tx, run.ID)
	records := handoffRecords{admitted: map[string]orchestration.EngineeringHandoff{}, refused: refused}
	for _, handoff := range admitted {
		records.admitted[handoff.ID] = handoff
	}
	if err != nil {
		return fail(err)
	}
	finding, err := inspectHandoff(run.ID, events, snapshot.Operations, records)
	if err != nil {
		return fail(err)
	}
	out.Handoff, out.HandoffReason = finding.observation, boundedDetail(finding.detail)
	repair, err := inspectHandoffRepair(events, snapshot.Operations)
	if err != nil {
		return fail(err)
	}
	out.HandoffRepair, out.HandoffRepairDetail = repair.state, boundedDetail(repair.detail)
	if finding.observation == orchestration.HandoffAdmitted {
		out.HandoffID = finding.handoffID
	}
	if err := orchestrationLifecycleCoherent(run, snapshot, operations); err != nil {
		out.Observation = "transitioning"
		return fail(err)
	}
	var capacitySuperseded bool
	out.Capacity, capacitySuperseded, err = capacityWaitFor(events, summary.Reason)
	if err != nil {
		return fail(err)
	}
	activity := runActivity(summary, operations, now)
	if out.Executing {
		activity = orchestration.ActivityWorking
		out.Capacity = nil
		if summary.Disposition != Active || summary.Paused != nil {
			out.Observation = "transitioning"
		}
	} else if summary.Paused != nil {
		activity = orchestration.ActivityWaiting
		out.Capacity = nil
		if summary.Disposition != Waiting {
			out.Observation = "transitioning"
		}
	} else if capacitySuperseded && summary.Disposition == Waiting {
		out.Observation = "transitioning"
		return fail(fmt.Errorf("transitioning: capacity was reacquired; awaiting fresh run disposition"))
	} else if out.Capacity != nil && summary.Disposition == Waiting {
		activity = orchestration.ActivityIdle
	} else if summary.producerFinished {
		activity = orchestration.ActivityFinished
	} else if summary.Disposition == Waiting && summary.Reason == ReasonGoalStateReached && summary.Paused == nil {
		activity = orchestration.ActivityIdle
		out.Observation = "transitioning"
	}
	termination, err := runTermination(summary.Disposition)
	if err != nil {
		return fail(err)
	}
	facts := orchestration.ChildFacts{
		Exists: true, Termination: termination, Handoff: finding.observation,
		Activity: activity,
	}
	if finding.admitted != nil {
		facts.HandoffOutcome = finding.admitted.ProducerReport.Outcome
	}
	state, err := orchestration.ProjectItem(facts)
	if err != nil {
		return fail(err)
	}
	out.State = state
	// The handoff explains the item only where the handoff is what the item
	// is waiting on; a failed or stopped child keeps its own run's reason.
	if finding.detail != "" && state == orchestration.ItemHandoffPending {
		out.Reason = boundedDetail(finding.detail)
	}
	if state == orchestration.ItemPartial {
		unresolved := finding.admitted.ProducerReport.Unresolved
		out.Reason = boundedDetail(fmt.Sprintf("the worker reports %d unresolved item(s): %s", len(unresolved), strings.Join(unresolved, "; ")))
	}
	return out
}

// runTermination maps the child's disposition onto the closed vocabulary the
// projection accepts. An unrecognized disposition is an error.
func runTermination(disposition Disposition) (orchestration.RunTermination, error) {
	switch disposition {
	case Active, Waiting:
		return orchestration.RunLive, nil
	case Completed:
		return orchestration.RunCompleted, nil
	case Failed:
		return orchestration.RunFailed, nil
	case Cancelled:
		return orchestration.RunCancelled, nil
	}
	return "", fmt.Errorf("unrecognized run disposition %q", disposition)
}

// runActivity reads a live child's relation to capacity from the same
// predicate the fleet view uses: holding a scheduler slot is working, a typed
// wait or an operator pause is waiting, anything else is queued.
func runActivity(summary RunSummary, operations map[string]RunOperation, now time.Time) orchestration.RunActivity {
	switch capacityState(operations, now) {
	case CapacityWork, CapacityObservation:
		return orchestration.ActivityWorking
	}
	if summary.Disposition == Waiting || summary.Paused != nil {
		return orchestration.ActivityWaiting
	}
	return orchestration.ActivityIdle
}
