package runtime

// The console's per-run projection.
//
// FleetStatus answers "what is every run doing" with RunSummary, which is
// deliberately light: it is built for a list of possibly many rows and a
// terminal-width line each. A console asking "what is THIS run doing, why,
// and what is it waiting for" needs more of what the journal already knows -
// held material, assurance and authority milestones, and which of its two
// progress sources (the live operation row, or the durable journal) is
// speaking - without inventing a second replay of the same events to get it.
//
// summarizeRunState (fleet.go) is the one place a run's events become
// runState; everything below reads fields already sitting on the runState
// summarizeRun built, or calls the same package-private helpers
// EngineeringRuntime.Status calls, never a parallel re-derivation.

import (
	"sort"
	"strings"
	"time"
)

// ConsoleOperation is the current bounded operation's live/journal overlay:
// the same row-then-journal choice EngineeringRuntime.Status makes for
// `autonomy status`, so a console marks exactly the same operation "live"
// that the CLI would.
type ConsoleOperation struct {
	ID             string         `json:"id"`
	Kind           string         `json:"kind"`
	State          OperationState `json:"state"`
	Attempt        int            `json:"attempt"`
	MaxAttempts    int            `json:"max_attempts"`
	StartedAt      *time.Time     `json:"started_at,omitempty"`
	LastProgressAt *time.Time     `json:"last_progress_at,omitempty"`
	SilentFor      time.Duration  `json:"silent_for"`
	// ProgressSource is "row" while a live SQLite operation row for this exact
	// attempt is readable, and "journal" otherwise - the same split
	// docs/supervisor.md documents for `autonomy status`/`logs`.
	ProgressSource string       `json:"progress_source"`
	HeartbeatAt    *time.Time   `json:"heartbeat_at,omitempty"`
	Deadline       *time.Time   `json:"deadline,omitempty"`
	DeadlineBound  AttemptBound `json:"deadline_bound,omitempty"`
}

// RunDetail is one run's console view: the RunSummary every fleet row
// already carries, plus the facts a console's run detail page needs beside
// it - held material, assurance/publication/authority milestones, the
// candidate's completeness, and the live operation overlay.
type RunDetail struct {
	Summary RunSummary `json:"summary"`

	HeldMaterial         *HeldMaterial                  `json:"held_material,omitempty"`
	Assurance            *AssuranceObservation          `json:"assurance,omitempty"`
	SemanticAssurance    *AssuranceObservation          `json:"semantic_assurance,omitempty"`
	PublicationAuthority *PublicationAuthority          `json:"publication_authority,omitempty"`
	AuthorityDecisions   map[string]AuthorityEvaluation `json:"authority_decisions,omitempty"`
	ExecutionDiagnostic  *ExecutionDiagnostic           `json:"execution_diagnostic,omitempty"`

	// CandidateComplete and Checkpoints are RunProjection's distinction
	// between a candidate a producer finished against and one left mid-work: a
	// run holding a checkpoint is not holding a result.
	CandidateComplete bool `json:"candidate_complete"`
	Checkpoints       int  `json:"checkpoints,omitempty"`

	// Budgets are the bounds THIS RUN was created under (EngineeringRun.Budgets),
	// frozen at genesis. Nil means a run that predates the field, reported as
	// unknown rather than as an invented zero bound.
	Budgets *RunBudgets `json:"budgets,omitempty"`

	Operation *ConsoleOperation `json:"operation,omitempty"`
}

// ConsoleFleetView is ConsoleFleet's answer: FleetStatus's own counts and
// plan summaries, with RunDetail in place of RunSummary for every run.
type ConsoleFleetView struct {
	At                time.Time     `json:"at"`
	Capacity          int           `json:"capacity"`
	Executing         int           `json:"executing"`
	Active            int           `json:"active"`
	Runs              []RunDetail   `json:"runs"`
	Plans             []PlanSummary `json:"plans,omitempty"`
	SupervisorRunning bool          `json:"supervisor_running"`
	ControlEndpoint   string        `json:"control_endpoint,omitempty"`
}

// ConsoleFleet walks every run exactly as FleetStatus does - same replay, same
// Active/Executing counters, same sort (active work first, then most recent)
// - and additionally carries each run's RunDetail. It is FleetStatus's
// sibling, not a competing fleet view: a caller wanting the terminal-width
// list still has FleetStatus; a console wanting the richer per-run facts
// without a second pass over the store has this.
func ConsoleFleet(store *SQLiteOperationStore, stateDir string, capacity int, now time.Time) (ConsoleFleetView, error) {
	runs, err := store.Runs()
	if err != nil {
		return ConsoleFleetView{}, err
	}
	view := ConsoleFleetView{
		At: now, Capacity: capacity,
		SupervisorRunning: SupervisorRunning(stateDir),
		ControlEndpoint:   ControlSocketPath(stateDir) + " (" + ControlEndpointMechanism + ")",
	}
	view.Plans = summarizePlans(store)
	for _, run := range runs {
		summary, state, ok := summarizeRunState(store, stateDir, run, now)
		detail := RunDetail{Summary: summary}
		if ok {
			detail = enrichRunDetail(store, detail, state, now)
		}
		if !terminalDisposition(summary.Disposition) {
			view.Active++
		}
		if summary.Executing {
			view.Executing++
		}
		view.Runs = append(view.Runs, detail)
	}
	sort.SliceStable(view.Runs, func(i, j int) bool {
		left, right := view.Runs[i].Summary, view.Runs[j].Summary
		leftActive := !terminalDisposition(left.Disposition)
		rightActive := !terminalDisposition(right.Disposition)
		if leftActive != rightActive {
			return leftActive
		}
		return left.Elapsed < right.Elapsed
	})
	return view, nil
}

// ConsoleRunDetail answers one run's RunDetail. found is false when no run
// with this id exists; a run that exists but could not be fully replayed is
// still found, with summary.Error set and every console-only field absent.
func ConsoleRunDetail(store *SQLiteOperationStore, stateDir, runID string, now time.Time) (detail RunDetail, found bool, err error) {
	run, found, err := store.Run(runID)
	if err != nil || !found {
		return RunDetail{}, found, err
	}
	summary, state, ok := summarizeRunState(store, stateDir, run, now)
	detail = RunDetail{Summary: summary}
	if ok {
		detail = enrichRunDetail(store, detail, state, now)
	}
	return detail, true, nil
}

// enrichRunDetail layers the console-only fields onto a RunDetail whose
// Summary summarizeRunState already built, reading them off the SAME
// replayed state rather than replaying again.
func enrichRunDetail(store *SQLiteOperationStore, detail RunDetail, state *runState, now time.Time) RunDetail {
	detail.HeldMaterial = state.snapshot.HeldMaterial
	detail.Assurance = state.projection.Assurance
	detail.SemanticAssurance = state.projection.SemanticAssurance
	detail.AuthorityDecisions = state.projection.AuthorityDecisions
	detail.PublicationAuthority = publicationAuthorityFromDecisions(state.projection.AuthorityDecisions)
	detail.ExecutionDiagnostic = state.projection.ExecutionDiagnostic
	detail.CandidateComplete = state.projection.CandidateComplete
	detail.Checkpoints = state.projection.Checkpoints
	detail.Budgets = state.run.Budgets
	if operation, ok := state.currentOperation(); ok {
		detail.Operation = consoleOperationOverlay(store, operation, now)
	}
	return detail
}

// publicationAuthorityFromDecisions finds the #7 publication decision inside
// a run's projected authority decisions without needing the run's governed
// repository target the way EngineeringRuntime.decidedPublication does: the
// decision key is PublicationActionType + "\x00" + the target branch, and a
// run answers exactly one base branch, so the publication action type alone
// identifies it among whatever other actions the run also evaluated.
func publicationAuthorityFromDecisions(decisions map[string]AuthorityEvaluation) *PublicationAuthority {
	prefix := PublicationActionType + "\x00"
	for key, decision := range decisions {
		if strings.HasPrefix(key, prefix) {
			return &PublicationAuthority{Decision: decision.Decision, Action: decision.Action, Status: decision.Status}
		}
	}
	return nil
}

// consoleOperationOverlay is EngineeringRuntime.Status's row-then-journal
// choice, as a free function over just a store and the journal's own
// operation: the console builds no EngineeringRuntime (it would need
// adapters this read-only process has none of), so it reimplements this one
// small overlay rather than depend on Status's receiver.
func consoleOperationOverlay(store *SQLiteOperationStore, op RunOperation, now time.Time) *ConsoleOperation {
	overlay := &ConsoleOperation{
		ID: op.ID, Kind: op.Kind, State: op.State,
		Attempt: op.Attempt, MaxAttempts: op.MaxAttempts,
		StartedAt: op.StartedAt, LastProgressAt: op.LastProgressAt,
		SilentFor:      ProviderSilence(op, now),
		ProgressSource: "journal",
		Deadline:       op.Deadline, DeadlineBound: op.DeadlineBound,
	}
	if op.State != Leased && op.State != Running {
		return overlay
	}
	row, _, found, err := store.Operation(op.ID)
	if err != nil || !found || row.AttemptIdentity != op.AttemptIdentity || (row.State != Leased && row.State != Running) {
		return overlay
	}
	overlay.ProgressSource = "row"
	overlay.LastProgressAt = row.LastProgressAt
	overlay.SilentFor = ProviderSilence(row, now)
	if row.Lease != nil {
		heartbeat := row.Lease.HeartbeatAt
		overlay.HeartbeatAt = &heartbeat
	}
	return overlay
}
