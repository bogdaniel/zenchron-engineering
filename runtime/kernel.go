package runtime

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/authority"
	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/policy"
	"github.com/bogdaniel/zenchron-engineering/reassessment"
)

// SourceSnapshot is immutable untrusted source data after it is captured. The
// runtime never silently replaces it with a later issue edit.
type SourceSnapshot struct {
	ID, Objective                    string
	AcceptanceIntent, PredictedPaths []string
	PathsKnown                       bool
}
type KernelFlow struct{ Analyzer analysis.Analyzer }
type KernelState struct {
	Source       SourceSnapshot
	Contract     domain.EngineeringWorkContract
	Evidence     map[string]domain.EvidenceBundle
	Observed     analysis.ObservedChange
	Reassessment reassessment.Result
	Decision     domain.AuthorityDecision
}

// Compile turns the pinned source snapshot into the initial, predicted work
// contract. ProjectModel and policy are explicit inputs, not runtime policy.
func (f KernelFlow) Compile(source SourceSnapshot, model domain.ProjectModel, p domain.EngineeringPolicy, contractID, revision string) (KernelState, error) {
	if f.Analyzer.IsZero() {
		f.Analyzer = analysis.NewAnalyzer()
	}
	facts, err := f.Analyzer.Predict(model, model.Subject, analysis.Intent{Objective: source.Objective, AcceptanceIntent: source.AcceptanceIntent, AffectedPaths: source.PredictedPaths, PathsKnown: source.PathsKnown})
	if err != nil {
		return KernelState{}, err
	}
	contract, err := policy.Compile(policy.CompileInput{ContractID: contractID, ContractRevision: revision, Objective: source.Objective, AcceptanceIntent: source.AcceptanceIntent, Subject: model.Subject, Scope: domain.ContractScope{Stage: domain.StagePredicted, AllowedPaths: source.PredictedPaths}, ProjectModel: model, Policy: p, Facts: facts.Sorted()})
	if err != nil {
		return KernelState{}, err
	}
	return KernelState{Source: source, Contract: contract, Evidence: map[string]domain.EvidenceBundle{}}, nil
}

// ObserveCandidate is the single runtime bridge into #8. It never copies old
// evidence to the moved candidate binding; #8 marks it stale first.
func (f KernelFlow) ObserveCandidate(state KernelState, model domain.ProjectModel, p domain.EngineeringPolicy, candidate domain.Subject, paths []string) (KernelState, error) {
	observed, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: paths, PathsKnown: true})
	if err != nil {
		return state, err
	}
	nextEvidence := map[string]string{}
	for id, b := range state.Evidence {
		nextEvidence[id] = nextRevision(b.Revision)
	}
	result, err := reassessment.Reassess(reassessment.Input{CurrentContract: state.Contract, Compile: policy.CompileInput{ContractID: state.Contract.ID, ContractRevision: nextRevision(state.Contract.Revision), Objective: state.Contract.Objective, AcceptanceIntent: state.Contract.AcceptanceIntent, Subject: candidate, ProjectModel: model, Policy: p}, ObservedChange: observed, Analyzer: f.Analyzer, EvidenceBundles: state.Evidence, EvidenceRevisions: nextEvidence})
	if err != nil {
		return state, err
	}
	state.Observed = observed
	state.Reassessment = result
	if result.Material {
		state.Contract = *result.Contract
		state.Evidence = result.StaleEvidence
	}
	return state, nil
}

// ObserveCommit binds reassessment to the exact runtime-owned commit and tree
// result rather than a mutable provider directory.
func (f KernelFlow) ObserveCommit(state KernelState, model domain.ProjectModel, p domain.EngineeringPolicy, repository string, result CommitResult) (KernelState, error) {
	observed, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: result.Paths, PathsKnown: true})
	if err != nil {
		return state, err
	}
	return f.ObserveCandidate(state, model, p, domain.Subject{Repository: repository, Revision: result.Commit}, observed.Paths)
}

// Decide delegates final action status to #7. Awaiting authority and requested
// privilege expansion are represented as waits, never remediation work.
func (f KernelFlow) Decide(state KernelState, action domain.Action, changeProducer domain.EvidenceProducer) (KernelState, error) {
	d, err := authority.Evaluate(authority.Input{DecisionID: "decision-" + state.Contract.ID + "-" + state.Contract.Revision, DecisionRevision: "1", Contract: state.Contract, Action: action, Capability: domain.CapabilityAvailable, ChangeProducer: changeProducer, EvidenceBundles: state.Evidence})
	if err != nil {
		return state, err
	}
	state.Decision = d
	return state, nil
}
func NextDisposition(state KernelState) (Disposition, string) {
	if len(state.Reassessment.RequestedPrivilegeExpansion) > 0 {
		return Waiting, "requested_privilege_expansion"
	}
	disposition, reason, _ := AuthorityDisposition(state.Decision.Status)
	if reason == "" {
		// Authorized is the one status that names no outstanding condition.
		return Active, "authorized"
	}
	return disposition, reason
}

// AuthorityDisposition is THE mapping from an AuthorityStatus to what it means
// for a run. It is one function because it was previously two: this kernel
// helper knew the whole matrix, and the live reconciler's conditions() knew two
// entries of it. A current INCOMPLETE decision therefore matched nothing there,
// no operation was wanted, and the run settled waiting/goal_state_reached over
// an unauthorized protected action - which is what
// run-0943e257539346f8763db04505cbf322 did with a passing exact-tree assurance
// and an incomplete decision.
//
// The third result is the invariant: outstanding reports whether this status
// leaves a protected action UNAUTHORIZED. While it is true the run has not
// reached its goal, whatever else is or is not wanted, so goal_state_reached is
// unreachable.
//
// Authorized is the only status that returns no reason: it names no outstanding
// condition, so it must not displace the terminal reason a finished run has.
// An unrecognized status fails closed - a wait naming the status - rather than
// falling through to a generic success or a generic wait.
func AuthorityDisposition(status domain.AuthorityStatus) (disposition Disposition, reason string, outstanding bool) {
	switch status {
	case domain.AuthorityAuthorized:
		return Active, "", false
	case domain.AuthorityAwaitingAuthority:
		// The only human-authority state. It is reached exactly when every
		// missing requirement is a human_approval claim; a machine claim that
		// nothing produced is INCOMPLETE, never this.
		return Waiting, "awaiting_authority", true
	case domain.AuthorityBlocked:
		return Waiting, "authority_blocked", true
	case domain.AuthorityIncomplete:
		// Evidence is missing that is not a human approval. The run may still
		// be able to produce it, so the disposition stays Active; if nothing is
		// wanted, the reason is what the run settles on.
		return Active, "evidence_required", true
	case domain.AuthorityStale:
		return Active, "fresh_evidence_required", true
	}
	return Waiting, "authority_unknown", true
}
func nextRevision(v string) string {
	if v == "" {
		return "1"
	}
	return v + "-next"
}
func RequireNoRemediationForAuthority(state KernelState) error {
	d, r := NextDisposition(state)
	if d == Waiting && (r == "awaiting_authority" || r == "requested_privilege_expansion") {
		return nil
	}
	return fmt.Errorf("state is not an authority wait")
}
