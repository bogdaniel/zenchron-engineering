package runtime

import (
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// OrchestrationView is one batch read through its children: the aggregate an
// operator watches instead of one terminal per run. It is read-only and
// derived entirely from durable state, so it reads the same whether or not a
// supervisor is running, and the same after a restart.
type OrchestrationView struct {
	BatchID     string                  `json:"batch_id"`
	Repository  string                  `json:"repository"`
	AgentID     string                  `json:"agent_id"`
	RequestedBy string                  `json:"requested_by,omitempty"`
	CreatedAt   time.Time               `json:"created_at"`
	Counts      orchestration.Counts    `json:"counts"`
	Items       []OrchestrationItemView `json:"items"`
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
	Reason            string      `json:"reason,omitempty"`
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
	Handoff   orchestration.HandoffObservation `json:"handoff"`
	HandoffID string                           `json:"handoff_id,omitempty"`
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
	byRun, err := capacityOperations(store)
	if err != nil {
		return OrchestrationView{}, err
	}
	view := OrchestrationView{
		BatchID: batch.ID, Repository: batch.Repository, AgentID: batch.AgentID,
		RequestedBy: batch.RequestedBy, CreatedAt: batch.CreatedAt,
	}
	for _, item := range batch.Items {
		projected := projectOrchestrationItem(store, stateDir, item, byRun[item.RunID], now)
		view.Counts.Add(projected.State)
		view.Items = append(view.Items, projected)
	}
	return view, nil
}

func projectOrchestrationItem(store *SQLiteOperationStore, stateDir string, item orchestration.BatchItem, operations map[string]RunOperation, now time.Time) OrchestrationItemView {
	out := OrchestrationItemView{Issue: item.Issue, RunID: item.RunID, Handoff: orchestration.HandoffNone}
	fail := func(err error) OrchestrationItemView {
		out.State, out.Reason = "", boundedDetail(err.Error())
		return out
	}
	run, found, err := store.Run(item.RunID)
	if err != nil {
		return fail(err)
	}
	if !found {
		out.State = orchestration.ItemNotCreated
		return out
	}
	summary := summarizeRun(store, stateDir, run, now)
	if summary.Error != "" {
		return fail(fmt.Errorf("%s", summary.Error))
	}
	out.AgentID, out.Phase, out.Disposition, out.Reason = summary.Agent, summary.Phase, summary.Disposition, summary.Reason
	out.Executing, out.Branch = summary.Executing, summary.Branch
	out.CandidateRevision, out.CandidateTree = summary.CandidateRevision, summary.CandidateTree
	out.PullRequest, out.PRState = summary.PullRequest, summary.PRState

	events, err := store.Events(run.ID)
	if err != nil {
		return fail(err)
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		return fail(err)
	}
	admitted, err := admittedHandoffs(store, run.ID)
	if err != nil {
		return fail(err)
	}
	finding, err := inspectHandoff(run.ID, events, snapshot.Operations, admitted)
	if err != nil {
		return fail(err)
	}
	out.Handoff = finding.observation
	if finding.observation == orchestration.HandoffAdmitted {
		out.HandoffID = finding.handoffID
	}
	termination, err := runTermination(summary.Disposition)
	if err != nil {
		return fail(err)
	}
	state, err := orchestration.ProjectItem(orchestration.ChildFacts{
		Exists: true, Termination: termination, Handoff: finding.observation,
		Activity: runActivity(summary, operations, now),
	})
	if err != nil {
		return fail(err)
	}
	out.State = state
	// The handoff explains the item only where the handoff is what the item
	// is waiting on; a failed or stopped child keeps its own run's reason.
	if finding.detail != "" && (state == orchestration.ItemHandoffPending || state == orchestration.ItemRunning) {
		out.Reason = boundedDetail(finding.detail)
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
