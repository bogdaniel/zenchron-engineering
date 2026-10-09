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
	// The workspace is loaded through the SAME r.workspace(state) every other
	// candidate operation uses (#475 R3-3) - never a hand-built struct, and
	// never preemptively reset. r.workspace verifies head against the run's
	// own durably recorded revision and loads TrustedMetadata from the
	// journalled baseline createCandidate established; it performs no
	// destructive action itself. A prior attempt's abandoned partial merge, a
	// crash mid-plan, or genuine tampering surfaces as a real error here -
	// IntegrateInputs' own precondition (assertCleanAtVerifiedBase) additionally
	// proves the workspace is clean - and is OBSERVED and CLASSIFIED before
	// anything restores it: only the runtime's existing FailureWorkspaceIntegrity
	// route restores, with that real cause recorded as the operation's
	// failure. Resetting before observing would erase the one signal that
	// something unexpected happened.
	ws, err := r.workspace(state)
	if err != nil {
		// #475 review N2: r.workspace itself returning *WorkspaceIntegrityError
		// means a PRIOR attempt already advanced HEAD (a real merge, or a
		// partial post-merge preparation failure after one) without
		// journalling it - exactly the same containment this function's own
		// later errors.As branch already gives IntegrateInputs' failures.
		// r.workspace returns no usable workspace on error, so recovery
		// names only Dir/BaseRevision here - exactly what RestoreTrusted
		// needs - never trusting anything else about the observed state.
		var integrity *WorkspaceIntegrityError
		if errors.As(err, &integrity) {
			return r.restoreCandidate(&CandidateWorkspace{
				Dir: candidateDir(r.deps.StateDir, state.run.ID), BaseRevision: contract.BaseRevision,
			}, err)
		}
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
		return r.commitIntegratedCandidate(state, op, ws, contract, *result.Candidate)
	case integration.StatusBlocked:
		return r.refuseIntegrationConflict(state, op, *result.Conflict)
	case integration.StatusInvalidated:
		return integrationFailureEffect(FailureIntegrationInvalidated, integrationInvalidatedCode, result.Reason, nil)
	default:
		return failed(errors.New("integration composition returned an unrecognized status " + string(result.Status)))
	}
}

// classifyIntegrationAssuranceFailure turns a failing assurance verdict
// against an integration_compose unit's own composed candidate into
// integration.ClassifyAssuranceFailure's typed vocabulary, never a bare
// FailureClass string guessed at the call site.
//
// It always classifies ConflictUncertain, never ConflictSemantic (#475
// review B5): both consumed inputs passing their OWN assurance before
// admission is not positive evidence that THIS failure is specific to their
// combination - the verifier could be flaky, the environment could have
// drifted since either input was assured, or the failure could be latent in
// one input alone and merely surfaced now. Attributing it to "the
// combination" without evidence that actually distinguishes that from "one
// input's pre-existing defect" would be exactly the fabricated blame the
// architecture record forbids. ClassifyAssuranceFailure itself still refuses
// to classify this a ConflictTextual either way.
func (r *EngineeringRuntime) classifyIntegrationAssuranceFailure(state *runState, detail string) (FailureClass, error) {
	origin := state.origin
	contract, err := integration.NewContract(origin.GraphID, origin.UnitID, state.baseRevision(), origin.Inputs)
	if err != nil {
		return "", err
	}
	digest, err := origin.Inputs.Digest()
	if err != nil {
		return "", err
	}
	candidate := integration.IntegratedCandidate{
		Revision: state.projection.CandidateRevision, Tree: state.projection.CandidateTree, InputsDigest: digest,
	}
	if _, err := integration.ClassifyAssuranceFailure(contract, candidate, integration.ConflictUncertain, detail); err != nil {
		return "", err
	}
	return FailureIntegrationConflict, nil
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

// commitIntegratedCandidate journals a clean composition exactly as
// candidate.commit would for an ordinary mutation - the same
// EventCandidateCommitted and EventReassessmentCompleted shapes, so every
// existing reader (assurance gating, status projection) needs no change to
// see this candidate.
//
// It does NOT report a handoff (#475 review B2). A successful Git
// composition is not acceptance: reporting here would make
// handoffCommitOf/admitOrchestratedHandoff consider this run's handoff
// admissible before its own fresh assurance has even run, let alone passed.
// That report is deferred to admitIntegrationHandoff, OpIntegrationHandoffAdmit's
// own handler, which runs only once bindIntegrationHandoffAdmit sees a
// passing, non-stale assurance observation against this exact head.
func (r *EngineeringRuntime) commitIntegratedCandidate(state *runState, op RunOperation, ws *CandidateWorkspace, contract integration.Contract, candidate integration.IntegratedCandidate) effect {
	if current, err := r.integrationInputsStillCurrent(contract.GraphID, contract.Inputs); err != nil {
		// #475 review N2: by this line the real Git merge already advanced
		// HEAD. A transient read failure here (not necessarily a
		// *WorkspaceIntegrityError itself) is exactly the same
		// advanced-but-unrecorded state this function's own later steps
		// already route through restoreCandidate for - never a bare
		// failed(err) that would strand it until the NEXT attempt's
		// r.workspace call happens to classify the mismatch on its own.
		return r.restoreCandidate(ws, err)
	} else if !current {
		if err := ws.RestoreTrusted(); err != nil {
			return failed(err)
		}
		return integrationFailureEffect(FailureIntegrationInvalidated, integrationInvalidatedCode,
			"an upstream input was superseded during composition, immediately before admission", nil)
	}
	paths, err := integratedChangedPaths(ws.Dir, contract.BaseRevision, candidate.Revision)
	if err != nil {
		// The merge already committed: HEAD has advanced, but nothing durable
		// records it yet (#475 review B4). r.workspace's own expected-head
		// check will see that mismatch on any later attempt and return a
		// *WorkspaceIntegrityError before this function's own precondition
		// ever runs again - that is the existing recovery route, reached
		// through composeIntegration's own errors.As branch, not reinvented
		// here. This function itself never retries or self-heals.
		return r.restoreCandidate(ws, err)
	}
	next, err := r.buildKernelAt(state, ws.Dir, candidate.Revision)
	if err != nil {
		return r.restoreCandidate(ws, err)
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
		},
	}
}

// admitIntegrationHandoff is OpIntegrationHandoffAdmit's handler (#475
// review B2): the deferred handoff report for a WorkGraph integration_compose
// unit, reached only once its own composed head has a passing, non-stale
// assurance observation (bindIntegrationHandoffAdmit). It re-verifies
// freshness one more time, immediately before writing the report that makes
// handoffCommitOf/admitOrchestratedHandoff consider this admissible - the
// generic admission path's own AdmitIntegratedHandoff additionally enforces
// this atomically against the SAME exact inputs at the actual SQL INSERT
// (#475 review B3), so this check is a fast, clear failure path, not the
// sole guarantee.
func (r *EngineeringRuntime) admitIntegrationHandoff(_ context.Context, state *runState, op RunOperation) effect {
	origin := state.origin
	if origin == nil || origin.ExecutionKind != orchestration.ExecutionKindIntegrationCompose {
		return failed(errors.New("integration.handoff_admit dispatched for a run whose origin is not an explicit integration_compose unit"))
	}
	current, err := r.integrationInputsStillCurrent(origin.GraphID, origin.Inputs)
	if err != nil {
		return failed(err)
	}
	if !current {
		// The commit this run made already exists and stays exactly as
		// committed - it is not undone merely because an unrelated upstream
		// unit moved on. Only the handoff that would make it admissible is
		// refused.
		return integrationFailureEffect(FailureIntegrationInvalidated, integrationInvalidatedCode,
			"an upstream input was superseded before the integration handoff could be admitted", nil)
	}
	contract, err := integration.NewContract(origin.GraphID, origin.UnitID, state.baseRevision(), origin.Inputs)
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
		state: Succeeded,
		// Mutated: true is what inspectHandoff reads to tell a real commit to
		// bind from a no-op report (runtime/orchestration_handoff.go) - true
		// here because there genuinely IS a committed candidate to bind to,
		// from this run's own earlier OpIntegrationCompose. Returning the
		// zero value would make inspectHandoff read this exactly like an
		// invocation that reported a handoff after changing nothing, and
		// refuse it as unbindable even though the commit is real.
		result: mutationResult{Mutated: true, ProviderID: "integration.handoff_admit"},
		events: []journalEntry{handoffObservation(handoffPath, op.ID, attempt)},
	}
}

// refuseIntegrationConflict fails the operation under a typed, non-provider-
// routed failure class and, for a textual conflict, admits a bounded #473
// Finding bound to the exact consumed input Git reported as conflicting -
// never a forged worker invocation: the message is sourced from this run's
// own operation, exactly as any runtime-derived observation is.
//
// Review B1: this Finding is admitted into the INTEGRATION UNIT'S OWN
// single-item batch, never the producer's. #473's per-batch isolation means
// it is durable, typed, correctly sourced and visible to anyone reading
// THIS batch's scope (the run, its operator, its own status) - it is not
// yet delivered into the producer's own inbox, which lives in a separate
// batch this one has no read access to. Route.Unit names the producer as
// the intended audience without granting it: routeOf's own Audience enum
// has no "another batch" case, only AudienceUnit/AudienceAuthority/
// AudienceScope within THIS scope. Building that cross-batch delivery
// (or an explicit decision for it) is the still-open #475 follow-up; do
// not resurrect reverted #543's graph-wide mailbox to get there.
// integrationTextualConflictCode and integrationInvalidatedCode are
// deterministicCode identities for the two ways a WorkGraph
// integration_compose unit's attempt can be blocked - never provider
// diagnoses, see deterministicError.
const (
	integrationTextualConflictCode = "integration.textual_conflict"
	integrationInvalidatedCode     = "integration.invalidated"
)

// integrationFailureEffect builds a typed, non-provider-routed failure that
// ALSO participates in the runtime's existing identicalDeterministicFailure
// park-not-retry mechanism (#475 review's retry-policy follow-up): a
// deterministic conflict or invalidation against this exact, frozen
// consumed input set can never fix itself via an identical re-merge, so a
// repeated attempt of the SAME operation parks (settles Waiting,
// deterministic_failure_unchanged) rather than spending its whole attempt
// budget on an unchanging pair. A genuinely different input set is a
// DIFFERENT operation entirely - bindIntegrationCompose's own binding IS
// the consumed input digest - so it is never compared against this one at
// all, and reaches a fresh attempt exactly as #472's own activation
// identity already guarantees.
func integrationFailureEffect(class FailureClass, code, detail string, events []journalEntry) effect {
	cause := deterministicRefusal(code, errors.New(detail))
	return effect{
		state: OperationFailed, events: events, failure: deterministicIdentity(cause),
		result: integrationFailureResult{FailureClass: class, Reason: boundedDetail(detail)},
	}
}

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
	return integrationFailureEffect(FailureIntegrationConflict, integrationTextualConflictCode, conflict.Detail, events)
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
