package integration

import (
	"fmt"
	"strings"
)

// Status is the outcome of one integration attempt.
type Status string

const (
	// StatusIntegrated is a clean, deterministic composition: a new exact
	// commit and tree that no human or model edited by hand.
	StatusIntegrated Status = "integrated"
	// StatusBlocked is a conflict composition could not resolve on its own -
	// textual at merge time, or semantic/uncertain after a clean merge (see
	// ClassifyAssuranceFailure). The candidate workspace is left exactly at
	// its base revision; nothing partially merged is ever left standing.
	StatusBlocked Status = "blocked"
	// StatusInvalidated is an upstream input that no longer names a
	// readable, current subject: replaced, unreadable, or not a descendant
	// of the verified base. This fails closed rather than guessing which
	// half of a stale pair to trust.
	StatusInvalidated Status = "invalidated"
)

// ConflictKind distinguishes what composition detected, where the evidence
// available to this package can tell the difference - and says so explicitly
// when it cannot.
type ConflictKind string

const (
	// ConflictTextual is a Git three-way merge conflict: deterministic, and
	// detected without running anything either upstream wrote.
	ConflictTextual ConflictKind = "textual"
	// ConflictSemantic is a clean, conflict-free merge that independent
	// evidence - a fresh assurance pass against the integrated subject
	// itself, never a restatement of either input's own evidence - shows
	// breaks a contract the parts satisfied alone.
	ConflictSemantic ConflictKind = "semantic"
	// ConflictUncertain is a clean merge whose combination this build cannot
	// yet attribute to the integration itself rather than to an input that
	// already failed alone. Composition never upgrades this to "correct":
	// it still requires the same fresh, independent assurance either way.
	ConflictUncertain ConflictKind = "uncertain"
)

const (
	maxConflictDetailBytes = 2 << 10
	maxConflictPaths       = 64
	maxConflictPathBytes   = 4 << 10
)

// Conflict is one blocked attempt's bounded, readable detail: what kind, and
// which paths, so a producer directed to remediate is told exactly what is
// in scope and is not left to guess at the rest of the tree.
type Conflict struct {
	Kind   ConflictKind `json:"kind"`
	Detail string       `json:"detail"`
	Paths  []string     `json:"paths,omitempty"`
}

// NewConflict builds a bounded Conflict. Oversized input is truncated at a
// fixed bound rather than refused outright: a conflict report is the
// runtime's own diagnostic text, not untrusted worker input, and a diagnostic
// too long to render fully is still worth showing in part.
func NewConflict(kind ConflictKind, detail string, paths []string) Conflict {
	detail = strings.TrimSpace(detail)
	if len(detail) > maxConflictDetailBytes {
		detail = detail[:maxConflictDetailBytes]
	}
	if len(paths) > maxConflictPaths {
		paths = paths[:maxConflictPaths]
	}
	bounded := make([]string, len(paths))
	for i, p := range paths {
		if len(p) > maxConflictPathBytes {
			p = p[:maxConflictPathBytes]
		}
		bounded[i] = p
	}
	return Conflict{Kind: kind, Detail: detail, Paths: bounded}
}

// IntegratedCandidate is a new exact subject. It is never one of the inputs'
// own commits, and it carries no inherited evidence, review or acceptance
// verdict: those are what this subject needs next, through its own fresh
// admission, assurance and review, never something it is born already
// holding (requirement: evidence on an input never authorizes the composed
// output).
type IntegratedCandidate struct {
	Revision     string `json:"revision"`
	Tree         string `json:"tree"`
	InputsDigest string `json:"inputs_digest"`
}

// Result is one integration attempt's outcome. Candidate is set only on
// StatusIntegrated or a semantic/uncertain StatusBlocked (a clean merge that
// a later assurance pass then failed); Conflict only on StatusBlocked; Reason
// only on StatusInvalidated.
type Result struct {
	Status    Status               `json:"status"`
	Contract  Contract             `json:"contract"`
	Candidate *IntegratedCandidate `json:"candidate,omitempty"`
	Conflict  *Conflict            `json:"conflict,omitempty"`
	Reason    string               `json:"reason,omitempty"`
}

// Integrated reports a clean composition.
func Integrated(contract Contract, candidate IntegratedCandidate) Result {
	return Result{Status: StatusIntegrated, Contract: contract, Candidate: &candidate}
}

// Blocked reports a textual merge conflict composition could not resolve.
func Blocked(contract Contract, conflict Conflict) Result {
	return Result{Status: StatusBlocked, Contract: contract, Conflict: &conflict}
}

// Invalidated reports an upstream input that no longer names a readable,
// current subject.
func Invalidated(contract Contract, reason string) Result {
	reason = strings.TrimSpace(reason)
	if len(reason) > maxConflictDetailBytes {
		reason = reason[:maxConflictDetailBytes]
	}
	return Result{Status: StatusInvalidated, Contract: contract, Reason: reason}
}

// ClassifyAssuranceFailure turns a clean merge whose independent assurance
// pass then failed into a typed blocked result. It never runs assurance
// itself and never decides correctness on its own: #475 reuses whichever
// existing assurance/reassessment owner already answers that question for an
// ordinary EngineeringRun, against this integrated subject, same as any
// other. This is only the typed vocabulary a caller - the run's existing
// reassessment path - translates that answer into.
//
// kind must be ConflictSemantic or ConflictUncertain: an assurance failure is
// never classified ConflictTextual, which this package reserves for a Git
// merge conflict it detected deterministically and without running anything.
func ClassifyAssuranceFailure(contract Contract, candidate IntegratedCandidate, kind ConflictKind, detail string) (Result, error) {
	if kind != ConflictSemantic && kind != ConflictUncertain {
		return Result{}, fmt.Errorf("an assurance failure on an integrated candidate classifies as %q or %q, not %q",
			ConflictSemantic, ConflictUncertain, kind)
	}
	if strings.TrimSpace(detail) == "" {
		detail = "independent assurance failed against the integrated candidate"
	}
	conflict := NewConflict(kind, detail, nil)
	return Result{Status: StatusBlocked, Contract: contract, Candidate: &candidate, Conflict: &conflict}, nil
}

// VerifyRemediationScope refuses a remediation attempt that touches material
// outside what a textual conflict named. A worker directed to resolve a
// textual conflict may change only the paths Git itself reported as
// conflicted; anything else is a producer reaching past its own contract
// (requirement: conflict remediation modifies only material admitted under
// the integration contract), refused here rather than silently admitted.
func VerifyRemediationScope(conflict Conflict, changedPaths []string) error {
	if conflict.Kind != ConflictTextual {
		return fmt.Errorf("remediation scope is bounded by conflicted paths only for a %q conflict, not %q", ConflictTextual, conflict.Kind)
	}
	inScope := make(map[string]bool, len(conflict.Paths))
	for _, p := range conflict.Paths {
		inScope[p] = true
	}
	for _, p := range changedPaths {
		if !inScope[p] {
			return fmt.Errorf("remediation changed %q, which the integration contract's conflict did not name", p)
		}
	}
	return nil
}
