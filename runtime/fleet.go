package runtime

// The control-room view.
//
// `status RUN` answers everything about one run and is deliberately unchanged.
// What was missing is the other question an operator with several workers has:
// what is ALL of my work doing right now, and where do I look next. Answering
// it by opening runtime.db, or by knowing where an artifact path is built, is
// the product failure #63 exists to fix.
//
// This is a PROJECTION over the same durable state every other read uses. It
// makes no network call, takes no lease and holds no lock, so it is safe to run
// against runs another process is driving - which is the normal case once a
// supervisor owns them.

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// RunSummary is one run as an operator sees it in a list. Every field answers a
// question someone standing in front of a terminal actually asks, and the two
// that used to require a database or a path convention - where is the workspace,
// where are the logs - are answered here.
type RunSummary struct {
	RunID      string `json:"run_id"`
	Repository string `json:"repository"`
	// Issue is the source issue number, or zero for a run whose goal is not an
	// issue.
	Issue int `json:"issue,omitempty"`
	// Agent is the named execution agent, empty for a run created before the
	// registry existed. TrustMode and ProviderKind come from the journalled
	// binding, so they describe the run rather than today's configuration.
	Agent        string      `json:"agent,omitempty"`
	ProviderKind string      `json:"provider_kind,omitempty"`
	TrustMode    TrustMode   `json:"trust_mode,omitempty"`
	Model        string      `json:"model,omitempty"`
	Phase        Phase       `json:"phase"`
	Disposition  Disposition `json:"disposition"`
	// Reason is the waiting or terminal reason. For a waiting run this is the
	// answer to "what is it blocked on".
	Reason string `json:"reason,omitempty"`
	// Operation is the current bounded operation's kind and attempt, which is
	// what "implementing" or "testing" actually means underneath.
	Operation string `json:"operation,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	// Elapsed is wall time since the run was created.
	Elapsed time.Duration `json:"elapsed"`
	// Candidate identifies the work: the branch, the exact revision and tree,
	// and the workspace directory on disk.
	Branch            string `json:"branch,omitempty"`
	CandidateRevision string `json:"candidate_revision,omitempty"`
	CandidateTree     string `json:"candidate_tree,omitempty"`
	Workspace         string `json:"workspace,omitempty"`
	// PullRequest, CI and Review are the forge-side state.
	PullRequest int    `json:"pull_request,omitempty"`
	PRState     string `json:"pull_request_state,omitempty"`
	CI          string `json:"ci,omitempty"`
	Review      string `json:"review,omitempty"`
	// Feedback counts admitted items and how many are still undelivered, which
	// is what makes "a review arrived and the worker has not seen it yet"
	// visible without reading a journal.
	FeedbackAdmitted int `json:"feedback_admitted,omitempty"`
	FeedbackPending  int `json:"feedback_pending,omitempty"`
	// Attempts is the per-operation-kind attempt tally.
	Attempts map[string]int `json:"attempts,omitempty"`
	// Error is set when this run's state could not be replayed. One unreadable
	// run must not hide the rest of the fleet.
	Error string `json:"error,omitempty"`
}

// Fleet is the whole operator view.
type Fleet struct {
	At time.Time `json:"at"`
	// Capacity is the operator-authorized concurrency ceiling, and Active is
	// how many runs are currently non-terminal. "3 / 4 active" is a fact about
	// the operator's configuration, not about this process.
	Capacity int          `json:"capacity"`
	Active   int          `json:"active"`
	Runs     []RunSummary `json:"runs"`
	// Plans is the plan-level view beside the runs. An operator with a plan
	// awaiting their approval is being waited ON, and that has to be visible in
	// the same place they look to see whether anything is happening.
	Plans []PlanSummary `json:"plans,omitempty"`
	// SupervisorRunning reports whether a persistent supervisor currently owns
	// the control endpoint for this state directory.
	SupervisorRunning bool `json:"supervisor_running"`
	// ControlEndpoint is the mechanism and path, so an operator can see the
	// authority boundary they are relying on.
	ControlEndpoint string `json:"control_endpoint,omitempty"`
}

// PlanSummary is one plan as the control room shows it: what it is, which
// revision governs, whether it is waiting on a person, and what it has spent.
type PlanSummary struct {
	PlanID   string `json:"plan_id"`
	Revision int    `json:"revision"`
	// ApprovedRevision is the revision actually executing, which is not always
	// the newest one: a proposed revision waits for approval while the approved
	// one keeps governing.
	ApprovedRevision int `json:"approved_revision,omitempty"`
	// State is the plan-level answer: proposed, awaiting_approval, executing,
	// blocked, completed or rejected.
	State string `json:"state"`
	// Stages counts stage states, so "2 running, 1 satisfied, 1 pending" is
	// visible without opening the plan.
	Stages map[string]int `json:"stages,omitempty"`
	// Runs are the child EngineeringRuns this plan created, which are the same
	// runs listed above.
	Runs     []string               `json:"runs,omitempty"`
	Consumed domain.PlanConsumption `json:"consumed"`
	Error    string                 `json:"error,omitempty"`
}

// Plan lifecycle states as the control room names them.
const (
	PlanStateAwaitingApproval = "awaiting_approval"
	PlanStateRejected         = "rejected"
	PlanStateExecuting        = "executing"
	PlanStateCompleted        = "completed"
	PlanStateBlocked          = "blocked"
)

// summarizePlans projects every plan in the store, one entry each.
func summarizePlans(store *SQLiteOperationStore) []PlanSummary {
	plans, err := store.Plans()
	if err != nil {
		return []PlanSummary{{Error: boundedDetail(err.Error())}}
	}
	summaries := make([]PlanSummary, 0, len(plans))
	for _, plan := range plans {
		summary := PlanSummary{PlanID: plan.ID, Revision: plan.Revision}
		snapshot, err := store.ReplayPlan(plan.ID)
		if err != nil {
			summary.Error = boundedDetail(err.Error())
			summaries = append(summaries, summary)
			continue
		}
		summary.Consumed = snapshot.Consumed
		summary.Runs = snapshot.ChildRuns()
		summary.Stages = map[string]int{}
		for _, projection := range snapshot.Stages {
			summary.Stages[string(projection.State)]++
		}
		summary.State = planState(snapshot)
		if approved, ok := snapshot.ApprovedRevision(); ok {
			summary.ApprovedRevision = approved
		}
		summaries = append(summaries, summary)
	}
	return summaries
}

// planState is the plan-level answer, derived from the same replayed state
// everything else reads.
func planState(snapshot PlanSnapshot) string {
	approved, ok := snapshot.ApprovedRevision()
	switch {
	case !ok && snapshot.Approval.Status == domain.ApprovalRejected:
		return PlanStateRejected
	case !ok:
		return PlanStateAwaitingApproval
	case approved < snapshot.Approval.Revision && snapshot.Approval.Status == domain.ApprovalPending:
		// A newer revision is proposed and unapproved: the plan is executing
		// what was approved AND waiting on a person for what was proposed.
		return PlanStateAwaitingApproval
	}
	running, pending, failed := 0, 0, 0
	for _, projection := range snapshot.Stages {
		switch projection.State {
		case PlanStageRunning:
			running++
		case PlanStagePending:
			pending++
		case PlanStageFailed:
			failed++
		}
	}
	switch {
	case running > 0:
		return PlanStateExecuting
	case failed > 0 && pending == 0:
		return PlanStateBlocked
	case pending > 0:
		return PlanStateExecuting
	default:
		return PlanStateCompleted
	}
}

// FleetStatus projects every run in the store. It never fails because one run
// is unreadable: that run reports its own error and the rest are still
// answered, because a fleet view whose whole value is "show me everything" must
// not be lost to one bad row.
func FleetStatus(store *SQLiteOperationStore, stateDir string, capacity int, now time.Time) (Fleet, error) {
	runs, err := store.Runs()
	if err != nil {
		return Fleet{}, err
	}
	fleet := Fleet{
		At: now, Capacity: capacity,
		SupervisorRunning: SupervisorRunning(stateDir),
		ControlEndpoint:   ControlSocketPath(stateDir) + " (" + ControlEndpointMechanism + ")",
	}
	fleet.Plans = summarizePlans(store)
	for _, run := range runs {
		summary := summarizeRun(store, stateDir, run, now)
		if !terminalDisposition(run.Disposition) {
			fleet.Active++
		}
		fleet.Runs = append(fleet.Runs, summary)
	}
	// Active work first, then most recent: the runs an operator can still act
	// on are the ones they are looking for.
	sort.SliceStable(fleet.Runs, func(i, j int) bool {
		left, right := fleet.Runs[i], fleet.Runs[j]
		leftActive := !terminalDisposition(left.Disposition)
		rightActive := !terminalDisposition(right.Disposition)
		if leftActive != rightActive {
			return leftActive
		}
		return left.Elapsed < right.Elapsed
	})
	return fleet, nil
}

func summarizeRun(store *SQLiteOperationStore, stateDir string, run EngineeringRun, now time.Time) RunSummary {
	summary := RunSummary{
		RunID: run.ID, Repository: run.Repository, Agent: run.AgentID,
		Phase: run.Phase, Disposition: run.Disposition, Reason: run.Reason,
		Branch: run.Candidate.Branch, Elapsed: now.Sub(run.CreatedAt),
	}
	if issue, err := issueNumberOf(run.Goal); err == nil {
		summary.Issue = issue
	}
	if dir := candidateDir(stateDir, run.ID); dirExists(dir) {
		summary.Workspace = dir
	}
	events, err := store.Events(run.ID)
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	snapshot, err := Reduce(run, events)
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	projection, err := Project(events)
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	// The journal is the authority for the agent binding; the row is only a
	// projection of it, so a row that somehow disagrees loses.
	state := &runState{run: run, snapshot: snapshot, events: events, projection: projection}
	agent, err := state.recordedAgent()
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	if agent.AgentID != "" {
		summary.Agent, summary.ProviderKind = agent.AgentID, agent.Kind
		summary.TrustMode, summary.Model = agent.TrustMode, agent.Model
	}
	summary.Disposition, summary.Reason = snapshot.Disposition, snapshot.Reason
	summary.CandidateRevision, summary.CandidateTree = projection.CandidateRevision, projection.CandidateTree
	summary.Attempts = projection.Attempts
	if operation, ok := state.currentOperation(); ok {
		summary.Operation, summary.Attempt = operation.Kind, operation.Attempt
	}
	if pr := projection.PullRequest; pr != nil {
		summary.PullRequest, summary.PRState = pr.Number, pr.State
	}
	if ci := projection.CI; ci != nil && !ci.Stale {
		summary.CI = ci.Conclusion
	}
	if review := projection.Review; review != nil && !review.Stale {
		summary.Review = review.State
	}
	feedback := state.feedbackState()
	for _, decision := range feedback.Admitted {
		if decision.Admitted {
			summary.FeedbackAdmitted++
		}
	}
	summary.FeedbackPending = len(feedback.Pending(projection.Head()))
	return summary
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ---------------------------------------------------------------------------
// Logs
// ---------------------------------------------------------------------------

// AttemptLog is one provider invocation's SANITIZED output, with the identity
// that makes it addressable. It is an operator UX over the artifacts #55
// already writes - not a second event store, and not a second copy of the
// journal.
type AttemptLog struct {
	AgentID     string `json:"agent_id"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
	Path        string `json:"path"`
	// Text is the sanitized transcript. The RAW transcript beside it is
	// local-only forensic material and is deliberately never read here: `logs`
	// is a surface an operator points at a screen, and the sanitized artifact
	// is the one that has been through redaction.
	Text string `json:"text,omitempty"`
	// Size is the sanitized transcript's size on disk, so a caller can render a
	// listing without loading every attempt.
	Size int64 `json:"size"`
}

// RunAttemptLogs lists one run's sanitized provider transcripts in attempt
// order. Text is loaded only when withText is set, so a listing stays cheap.
//
// The traversal is over the runtime's OWN artifact layout, which is derived
// from run, operation and attempt identity. Nothing here accepts a path from a
// caller, so `logs` cannot be pointed at a file the runtime does not own.
func RunAttemptLogs(artifacts ArtifactStore, runID string, withText bool) ([]AttemptLog, error) {
	if artifacts.Root == "" || runID == "" {
		return nil, nil
	}
	providers, err := os.ReadDir(filepath.Join(artifacts.Root, "provider"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var logs []AttemptLog
	encoded := encodePathComponent(runID)
	for _, provider := range providers {
		if !provider.IsDir() {
			continue
		}
		runDir := filepath.Join(artifacts.Root, "provider", provider.Name(), encoded)
		operations, err := os.ReadDir(runDir)
		if err != nil {
			continue
		}
		for _, operation := range operations {
			if !operation.IsDir() {
				continue
			}
			attempts, err := os.ReadDir(filepath.Join(runDir, operation.Name()))
			if err != nil {
				continue
			}
			for _, attempt := range attempts {
				name := attempt.Name()
				if !strings.HasSuffix(name, ".sanitized-candidate.log") {
					continue
				}
				number, ok := attemptNumberOf(strings.TrimSuffix(name, ".sanitized-candidate.log"))
				if !ok {
					continue
				}
				path := filepath.Join(runDir, operation.Name(), name)
				entry := AttemptLog{
					AgentID: provider.Name(), OperationID: operation.Name(), Attempt: number, Path: path,
				}
				if info, err := attempt.Info(); err == nil {
					entry.Size = info.Size()
				}
				if withText {
					if raw, err := os.ReadFile(path); err == nil {
						entry.Text = string(raw)
					}
				}
				logs = append(logs, entry)
			}
		}
	}
	sort.SliceStable(logs, func(i, j int) bool {
		if logs[i].OperationID != logs[j].OperationID {
			return logs[i].OperationID < logs[j].OperationID
		}
		return logs[i].Attempt < logs[j].Attempt
	})
	return logs, nil
}

func attemptNumberOf(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "attempt-")
	if !ok {
		return 0, false
	}
	number, err := strconv.Atoi(rest)
	return number, err == nil && number > 0
}
