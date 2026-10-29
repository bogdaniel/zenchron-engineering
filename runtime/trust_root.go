package runtime

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ---------------------------------------------------------------------------
// Trust root
// ---------------------------------------------------------------------------

// The adoption trust root is a GitHub repository ruleset, and this file is the
// only place that decides whether one is strong enough to make "adopted" mean
// anything.
//
// The whole provenance model rests on one property: a runtime candidate that
// reached trusted main is still PROVABLY CONTAINED in it. Ancestry is what
// proves that, and ancestry is exactly what a squash or a rebase destroys - the
// merged commit is a different object with different parents, so the candidate
// the runtime built, verified and published is no longer reachable from the
// branch that supposedly adopted it. Merge-only is therefore not a stylistic
// preference; it is the rule that keeps the proof available.
//
// Everything else here exists so the branch cannot move except through a pull
// request: no bypass actor, no deletion, no non-fast-forward rewrite.
//
// ADR-0007 took one thing OUT of this proof: the required status check. Which
// checks a pull request must pass is merge policy. Which revision on the branch
// is trusted is decided by exact-revision evidence (trusted_revision.go), not
// by the branch having moved under a check. So nothing here requires a check,
// and renaming a CI job can never break adoption.

// TrustedMainRuleset is the subset of a GitHub repository ruleset the adoption
// trust root depends on. Anything not represented here is not something the
// trust decision may rest on.
type TrustedMainRuleset struct {
	ID          int64
	Name        string
	Enforcement string
	// Targets and Excluded are the ref conditions. Both matter: an include that
	// also appears in exclude governs nothing, and reading only the include
	// would report a gate where there is none.
	Targets  []string
	Excluded []string
	// TargetType is GitHub's ruleset target ("branch", "tag", "push"). A
	// ruleset that governs tags does not govern the trusted branch.
	TargetType string
	// BypassActors is a COUNT, deliberately. Which actor could bypass the gate
	// does not change the answer: a trust root with any bypass at all is not a
	// trust root, and naming the actor would invite arguing about exceptions.
	//
	// BypassActorsKnown is separate because an OMITTED field is not an empty
	// one. A response that never mentions bypass_actors has told us nothing
	// about bypasses, and reading that silence as "there are none" is the
	// difference between a gate and the belief in a gate.
	BypassActors      int
	BypassActorsKnown bool
	PullRequest       *PullRequestRule
	RequiredChecks    *RequiredChecksRule
	Deletion          bool
	NonFastForward    bool
}

type PullRequestRule struct {
	AllowedMergeMethods []string
	RequiredApprovals   int
}

type RequiredChecksRule struct {
	Strict bool
	Checks []RequiredCheck
}

type RequiredCheck struct {
	Context       string `json:"context"`
	IntegrationID int64  `json:"integration_id"`
}

// BranchIntegrityPolicy is what an adopted build requires of the ruleset that
// governs the trusted branch (ADR-0007 §1). It is data so the exact expectation
// appears in the provenance artifact rather than only in code. The
// requirements that take no parameter - active, no bypass, no deletion, no
// non-fast-forward, pull requests required - are not fields: they are not
// negotiable, so there is nothing to record about them but the outcome.
type BranchIntegrityPolicy struct {
	Ref                 string   `json:"ref"`
	AllowedMergeMethods []string `json:"allowed_merge_methods"`
}

// DefaultBranchIntegrityPolicy is the frozen branch-integrity policy.
func DefaultBranchIntegrityPolicy() BranchIntegrityPolicy {
	return BranchIntegrityPolicy{Ref: "refs/heads/main", AllowedMergeMethods: []string{"merge"}}
}

// TrustRootPolicyRecord is the policy as an adopted-build record states it.
// An adopted-build/2 record carries only the branch-integrity fields. The
// legacy fields exist so an adopted-build/1 record - verified under M1-B's
// strict required "go" check - still decodes to what it actually said; nothing
// verifies against them.
type TrustRootPolicyRecord struct {
	Ref                 string         `json:"ref"`
	RequiredCheck       *RequiredCheck `json:"required_check,omitempty"`
	AllowedMergeMethods []string       `json:"allowed_merge_methods"`
	RequireStrictChecks bool           `json:"require_strict_checks,omitempty"`
}

func (p BranchIntegrityPolicy) record() TrustRootPolicyRecord {
	return TrustRootPolicyRecord{Ref: p.Ref, AllowedMergeMethods: p.AllowedMergeMethods}
}

// TrustRootError is a refusal to treat a ruleset as an adoption trust root. It
// carries every reason at once rather than the first: an operator fixing a
// ruleset should see the whole gap, not discover it one round trip at a time.
type TrustRootError struct{ Reasons []string }

func (e *TrustRootError) Error() string {
	return "the trusted-main ruleset is not a valid adoption trust root: " + strings.Join(e.Reasons, "; ")
}

// VerifyTrustRoot answers whether this ruleset makes "adopted" mean anything.
// It is pure: everything it needs is in its arguments, so every refusal below
// is reachable in a test without touching a real repository.
func VerifyTrustRoot(ruleset TrustedMainRuleset, policy BranchIntegrityPolicy) error {
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }

	if !strings.EqualFold(ruleset.Enforcement, "active") {
		add("enforcement is %q, not active", ruleset.Enforcement)
	}
	if ruleset.TargetType != "" && ruleset.TargetType != "branch" {
		add("it targets %q, not branches, so it does not govern %s", ruleset.TargetType, policy.Ref)
	}
	if !trustRootContains(ruleset.Targets, policy.Ref) {
		add("it targets %v, which does not include %s", ruleset.Targets, policy.Ref)
	}
	// An exclusion beats an inclusion. Reading only the include list is how a
	// ruleset that governs nothing gets reported as a gate.
	if trustRootContains(ruleset.Excluded, policy.Ref) {
		add("%s is explicitly excluded from it, so it does not govern the trusted branch", policy.Ref)
	}
	// Zenchron matches ref conditions EXACTLY and refuses anything else. A
	// half-written pattern matcher that quietly disagrees with GitHub's is
	// worse than no matcher: it would report a gate GitHub is not enforcing.
	for _, condition := range append(append([]string{}, ruleset.Targets...), ruleset.Excluded...) {
		if !strings.HasPrefix(condition, "refs/heads/") || strings.ContainsAny(condition, "*?[]") {
			add("ref condition %q is a pattern this trust root cannot prove; only exact refs/heads/ names are accepted", condition)
		}
	}
	switch {
	case !ruleset.BypassActorsKnown:
		add("it does not disclose its bypass actors, and an undisclosed bypass is not the same as no bypass")
	case ruleset.BypassActors > 0:
		add("%d bypass actor(s) can evade it, so it gates nothing", ruleset.BypassActors)
	}
	if !ruleset.Deletion {
		add("branch deletion is not prohibited")
	}
	if !ruleset.NonFastForward {
		add("non-fast-forward updates are not prohibited, so history could be rewritten under an adopted commit")
	}

	if ruleset.PullRequest == nil {
		add("pull requests are not required, so the branch can move without passing the gate")
	} else {
		for _, method := range ruleset.PullRequest.AllowedMergeMethods {
			if !trustRootContains(policy.AllowedMergeMethods, method) {
				add("merge method %q is allowed; it rewrites the candidate into a new commit and destroys the ancestry the provenance model proves containment with", method)
			}
		}
		if len(ruleset.PullRequest.AllowedMergeMethods) == 0 {
			add("no merge method is stated, so ancestry preservation is not guaranteed")
		}
	}

	if len(reasons) == 0 {
		return nil
	}
	sort.Strings(reasons)
	return &TrustRootError{Reasons: reasons}
}

func trustRootContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// ErrNoTrustRoot is returned when the repository has no ruleset at all. It is
// separate from a weak one: "there is no gate" and "the gate is wrong" are
// different things for an operator to fix.
var ErrNoTrustRoot = errors.New("the repository has no ruleset governing the trusted branch, so no source in it can be called adopted")
