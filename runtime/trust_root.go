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
// Everything else here exists so the branch cannot move except through the
// gate: pull requests required, the deterministic check required, no bypass
// actor, no deletion, no non-fast-forward rewrite.

// TrustedMainRuleset is the subset of a GitHub repository ruleset the adoption
// trust root depends on. Anything not represented here is not something the
// trust decision may rest on.
type TrustedMainRuleset struct {
	ID          int64
	Name        string
	Enforcement string
	Targets     []string
	// BypassActors is a COUNT, deliberately. Which actor could bypass the gate
	// does not change the answer: a trust root with any bypass at all is not a
	// trust root, and naming the actor would invite arguing about exceptions.
	BypassActors   int
	PullRequest    *PullRequestRule
	RequiredChecks *RequiredChecksRule
	Deletion       bool
	NonFastForward bool
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

// TrustPolicy is what an adopted build requires of the trust root. It is data
// so a test can state a different policy without editing the rule that reads
// it, and so the exact expectation appears in the provenance artifact rather
// than only in code.
type TrustPolicy struct {
	Ref                 string        `json:"ref"`
	RequiredCheck       RequiredCheck `json:"required_check"`
	AllowedMergeMethods []string      `json:"allowed_merge_methods"`
	RequireStrictChecks bool          `json:"require_strict_checks"`
}

// DefaultTrustPolicy is the frozen M1-B trust root.
func DefaultTrustPolicy() TrustPolicy {
	return TrustPolicy{
		Ref:                 "refs/heads/main",
		RequiredCheck:       RequiredCheck{Context: "go", IntegrationID: 15368},
		AllowedMergeMethods: []string{"merge"},
		RequireStrictChecks: true,
	}
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
func VerifyTrustRoot(ruleset TrustedMainRuleset, policy TrustPolicy) error {
	var reasons []string
	add := func(format string, args ...any) { reasons = append(reasons, fmt.Sprintf(format, args...)) }

	if !strings.EqualFold(ruleset.Enforcement, "active") {
		add("enforcement is %q, not active", ruleset.Enforcement)
	}
	if !trustRootContains(ruleset.Targets, policy.Ref) {
		add("it targets %v, which does not include %s", ruleset.Targets, policy.Ref)
	}
	if ruleset.BypassActors > 0 {
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

	switch {
	case ruleset.RequiredChecks == nil:
		add("no status check is required, so nothing deterministic gates the branch")
	default:
		if policy.RequireStrictChecks && !ruleset.RequiredChecks.Strict {
			add("required checks are not strict, so a check can pass against a base the branch has since left behind")
		}
		if !hasCheck(ruleset.RequiredChecks.Checks, policy.RequiredCheck) {
			add("the required check %s (app %d) is absent or bound to a different app",
				policy.RequiredCheck.Context, policy.RequiredCheck.IntegrationID)
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

func hasCheck(checks []RequiredCheck, want RequiredCheck) bool {
	for _, c := range checks {
		if c.Context == want.Context && c.IntegrationID == want.IntegrationID {
			return true
		}
	}
	return false
}

// ErrNoTrustRoot is returned when the repository has no ruleset at all. It is
// separate from a weak one: "there is no gate" and "the gate is wrong" are
// different things for an operator to fix.
var ErrNoTrustRoot = errors.New("the repository has no ruleset governing the trusted branch, so no source in it can be called adopted")
