package runtime

// Integration dispatch (#475): the production wiring that connects the
// merged deterministic Git composition core (integration.Contract,
// runtime.IntegrateInputs) to the governed WorkGraph (#472) and
// EngineeringRun (#63) lifecycle.
//
// composeIntegration is OpIntegrationCompose's handler. It runs in place of
// an ordinary execution.invoke for exactly one kind of unit: one explicitly
// marked orchestration.ExecutionKindIntegrationCompose, decided once at
// adoption by the WorkGraph author/compiler and read here from the run's own
// frozen batch origin - never inferred from Role, purpose, issue title or
// anything a provider wrote. (Role identifies who is qualified to perform a
// unit; ExecutionKind identifies which algorithm its run uses - an ordinary
// RoleIntegrator unit with no ExecutionKind set still gets execution.invoke.)
// It grants that unit no implementation, publication, merge or acceptance
// authority: a clean result is a new candidate that still needs its own
// fresh admission, assurance and review, exactly like any other run's
// commit, through the SAME downstream pipeline every other operation in this
// file already drives.
import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/integration"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// composeIntegration performs one integration.Contract's composition inside
// this run's own candidate workspace, and journals its outcome exactly as the
// operation it stands in for would: a clean result becomes a candidate.
// committed commit and an admissible handoff, in one operation, because the
// commit already exists in Git when composition succeeds; a blocked or
// invalidated result fails the operation under a typed, non-provider-routed
// failure class instead.
func (r *EngineeringRuntime) composeIntegration(_ context.Context, state *runState, op RunOperation) effect {
	origin := state.origin
	if origin == nil || origin.ExecutionKind != orchestration.ExecutionKindIntegrationCompose {
		// bindIntegrationCompose never wants this operation for anything
		// else; a handler reached for the wrong unit is a planner defect,
		// not a recoverable condition.
		return failed(errors.New("integration.compose dispatched for a run whose origin is not an explicit integration_compose unit"))
	}
	contract, err := integration.NewContract(origin.GraphID, origin.UnitID, state.baseRevision(), origin.Inputs)
	if err != nil {
		return failed(err)
	}
	// The workspace is forced to the contract's exact verified base before
	// anything else runs, on every attempt including a retry after a crash.
	// This is deliberately NOT r.workspace(state)'s journal-cross-check: the
	// only operation that ever touches this run's candidate before a commit
	// exists is this one, so a prior attempt's abandoned partial merge is
	// never legitimate material to preserve, and resetting it is exactly the
	// recovery runtime/integration.go's own docs name - RestoreTrusted,
	// through the existing FailureWorkspaceIntegrity route - just performed
	// up front rather than reached for after a refusal.
	ws := &CandidateWorkspace{
		Dir: candidateDir(r.deps.StateDir, state.run.ID), BaseRevision: contract.BaseRevision,
		Remote: r.deps.Remote.URL, Credentials: r.deps.Credentials,
	}
	if err := ws.RestoreTrusted(); err != nil {
		return failed(err)
	}
	sources, readErr := r.integrationSources(origin.GraphID)
	result, err := IntegrateInputs(ws, contract, sources)
	if *readErr != nil {
		// A store read failed inside the sources callback, which has no way
		// to say so through IntegrationSources' own (IntegrationSource, bool)
		// shape. That is never "not found": it is an ordinary operation
		// failure, retried exactly like any other.
		return failed(*readErr)
	}
	if err != nil {
		var integrity *WorkspaceIntegrityError
		if errors.As(err, &integrity) {
			return r.restoreCandidate(ws, err)
		}
		return failed(err)
	}
	switch result.Status {
	case integration.StatusIntegrated:
		return r.admitIntegratedCandidate(state, op, ws, contract, *result.Candidate)
	case integration.StatusBlocked:
		return r.refuseIntegrationConflict(state, op, *result.Conflict)
	case integration.StatusInvalidated:
		return effect{state: OperationFailed, result: integrationFailureResult{FailureClass: FailureIntegrationInvalidated, Reason: boundedDetail(result.Reason)}}
	default:
		return failed(errors.New("integration composition returned an unrecognized status " + string(result.Status)))
	}
}

// integrationSources resolves one WorkGraph unit id to its LIVE
// IntegrationSource, read fresh from durable state on every call - never
// memoized across the contract's whole plan - so a commit that remains
// fetchable is never mistaken for one that is still the graph's current
// admitted output. The returned error pointer is set only if a durable read
// itself failed, which IntegrationSources' (IntegrationSource, bool) shape
// cannot otherwise report; the caller checks it before trusting a "not
// found" as a real invalidation.
func (r *EngineeringRuntime) integrationSources(graphID string) (sources IntegrationSources, readErr *error) {
	var captured error
	return func(unitID string) (IntegrationSource, bool) {
		view, err := WorkGraphStatus(r.deps.Store, r.deps.StateDir, graphID, r.deps.Clock.Now(), nil)
		if err != nil {
			captured = err
			return IntegrationSource{}, false
		}
		for _, unit := range view.Units {
			if unit.UnitID == unitID && unit.Output != nil {
				return IntegrationSource{Dir: candidateDir(r.deps.StateDir, unit.Output.RunID), HandoffID: unit.Output.HandoffID}, true
			}
		}
		return IntegrationSource{}, false
	}, &captured
}

// integrationInputsStillCurrent re-reads every consumed input's LIVE admitted
// handoff once more, immediately before the integrated candidate is admitted
// - not only incrementally, as each step merged. A supersession racing the
// tail of composition is caught here rather than let through on the strength
// of checks that are already one step stale by the time the last input
// merged.
func (r *EngineeringRuntime) integrationInputsStillCurrent(graphID string, inputs orchestration.WorkUnitInputs) (bool, error) {
	view, err := WorkGraphStatus(r.deps.Store, r.deps.StateDir, graphID, r.deps.Clock.Now(), nil)
	if err != nil {
		return false, err
	}
	current := make(map[string]string, len(view.Units))
	for _, unit := range view.Units {
		if unit.Output != nil {
			current[unit.UnitID] = unit.Output.HandoffID
		}
	}
	for _, input := range inputs {
		if current[input.UnitID] != input.HandoffID {
			return false, nil
		}
	}
	return true, nil
}

// admitIntegratedCandidate journals a clean composition exactly as
// candidate.commit would for an ordinary mutation - the same
// EventCandidateCommitted and EventReassessmentCompleted shapes, so every
// existing reader (assurance gating, handoff admission, status projection)
// needs no change to see this candidate - and reports the same runtime-
// authored handoff an ordinary worker's completed invocation would.
func (r *EngineeringRuntime) admitIntegratedCandidate(state *runState, op RunOperation, ws *CandidateWorkspace, contract integration.Contract, candidate integration.IntegratedCandidate) effect {
	if current, err := r.integrationInputsStillCurrent(contract.GraphID, contract.Inputs); err != nil {
		return failed(err)
	} else if !current {
		if err := ws.RestoreTrusted(); err != nil {
			return failed(err)
		}
		return effect{state: OperationFailed, result: integrationFailureResult{FailureClass: FailureIntegrationInvalidated,
			Reason: "an upstream input was superseded during composition, immediately before admission"}}
	}
	paths, err := integratedChangedPaths(ws.Dir, contract.BaseRevision, candidate.Revision)
	if err != nil {
		return failed(err)
	}
	next, err := r.buildKernelAt(state, ws.Dir, candidate.Revision)
	if err != nil {
		return failed(err)
	}
	attempt := attemptIdentityOf(op)
	handoffPath, err := r.prepareHandoffSlot(state, op.ID, attempt)
	if err != nil {
		return failed(err)
	}
	report := orchestration.HandoffReport{
		SchemaVersion: orchestration.HandoffSchemaVersion, Outcome: orchestration.OutcomeCompleted,
		Summary: integrationSummary(contract),
	}
	if err := writeHandoffReport(handoffPath, report); err != nil {
		return failed(err)
	}
	return effect{
		state:  Succeeded,
		result: mutationResult{Mutated: true, PathCount: len(paths), ProviderID: "integration.compose"},
		events: []journalEntry{
			{Type: EventCandidateCommitted, Payload: CandidateCommittedPayload{
				Commit: candidate.Revision, Tree: candidate.Tree, PathCount: len(paths), PathsDigest: pathsDigest(paths),
			}},
			{Type: EventReassessmentCompleted, Payload: ReassessmentCompletedPayload{
				Material:                next.Reassessment.Material,
				Contract:                Ref{ID: next.Contract.ID, Revision: next.Contract.Revision},
				DeviationKinds:          deviationKinds(next),
				RequestedPrivilegeCount: len(next.Reassessment.RequestedPrivilegeExpansion),
			}},
			handoffObservation(handoffPath, op.ID, attempt),
		},
	}
}

// refuseIntegrationConflict fails the operation under a typed, non-provider-
// routed failure class and, for a textual conflict, admits a bounded #473
// Finding bound to the exact consumed input Git reported as conflicting -
// never graph-wide, never a forged worker invocation: the message is sourced
// from this run's own operation, exactly as any runtime-derived observation
// is.
func (r *EngineeringRuntime) refuseIntegrationConflict(state *runState, op RunOperation, conflict integration.Conflict) effect {
	events := []journalEntry(nil)
	if conflict.Kind == integration.ConflictTextual && conflict.UnitID != "" {
		attempt := attemptIdentityOf(op)
		path, err := MessageReportPath(r.deps.StateDir, ExecutionAttemptRef{RunID: state.run.ID, OperationID: op.ID, Attempt: attempt})
		if err != nil {
			return failed(err)
		}
		if err := clearResultSlot(path); err != nil {
			return failed(err)
		}
		report := orchestration.MessageReport{SchemaVersion: orchestration.MessageSchemaVersion, Messages: []orchestration.MessageDraft{
			{Kind: orchestration.KindFinding, SubjectHandoff: handoffIDOf(state.origin.Inputs, conflict.UnitID),
				Category: orchestration.FindingDefect, Body: conflict.Detail},
		}}
		if err := writeMessageReport(path, report); err != nil {
			return failed(err)
		}
		if entry, wrote := messageObservation(path, op.ID, attempt, false); wrote {
			events = append(events, entry)
		}
	}
	return effect{state: OperationFailed, events: events,
		result: integrationFailureResult{FailureClass: FailureIntegrationConflict, Reason: boundedDetail(conflict.Detail)}}
}

// integrationFailureResult is a blocked or invalidated attempt's durable
// result: the typed failure class RouteFailure reads (never provider
// remediation - see adapters.go) and a bounded, readable reason. It carries
// no deterministic identity: an integration failure is retried through the
// operation's ordinary bounded attempt budget like any other, exactly as
// base.integrate's own conflict result does, never parked waiting as if
// retrying could never possibly change its answer.
type integrationFailureResult struct {
	FailureClass FailureClass `json:"failure_class"`
	Reason       string       `json:"reason,omitempty"`
}

// writeHandoffReport writes the runtime-authored handoff document into this
// operation's own slot - the same shape and the same strict decoder a
// worker's report crosses, so admission reads it exactly as it reads any
// other invocation's report; nothing downstream can tell the two apart.
func writeHandoffReport(path string, report orchestration.HandoffReport) error {
	document, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(path, document, 0600)
}

// writeMessageReport writes the runtime-authored message document into this
// operation's own slot, read back by messageObservation exactly as a
// worker's would be.
func writeMessageReport(path string, report orchestration.MessageReport) error {
	document, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return os.WriteFile(path, document, 0600)
}

// handoffIDOf is the consumed input's recorded handoff id, from the exact
// contract this attempt composed - never re-read live, because a blocked
// Finding is bound to what was actually attempted.
func handoffIDOf(inputs orchestration.WorkUnitInputs, unitID string) string {
	for _, input := range inputs {
		if input.UnitID == unitID {
			return input.HandoffID
		}
	}
	return ""
}

// attemptIdentityOf is the physical attempt identity this operation's result
// and messages are keyed under. There is no scheduler-assigned provider
// attempt for a run that never reaches a provider, so the operation's own
// frozen identity is used - never zero, matching every other handler's floor.
func attemptIdentityOf(op RunOperation) int {
	if op.AttemptIdentity < 1 {
		return 1
	}
	return op.AttemptIdentity
}

// integrationSummary is the runtime-authored handoff summary for a clean
// composition: which exact upstream units it composed, in the contract's own
// canonical (and so deterministic, restart-stable) order.
func integrationSummary(contract integration.Contract) string {
	units := make([]string, len(contract.Plan()))
	for i, step := range contract.Plan() {
		units[i] = step.UnitID
	}
	return "deterministic integration composition of " + strings.Join(units, ", ")
}

// integratedChangedPaths reads the committed paths a new integration commit
// changed relative to its verified base, from the verified subject store
// (#437) - never a `git diff` against the live candidate workspace, which
// that guard refuses outside the subject store.
func integratedChangedPaths(dir, base, commit string) ([]string, error) {
	store, err := subjectStore(dir, commit)
	if err != nil {
		return nil, err
	}
	out, err := gitOutput(store, "diff", "--name-only", "-z", base+".."+commit)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(out, "\x00") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
