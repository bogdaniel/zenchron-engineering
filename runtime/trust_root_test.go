package runtime

import (
	"strings"
	"testing"
)

// goodRuleset is the trust root as M1-B requires it. Every refusal case below
// is this value with exactly one thing wrong, so each test names one property
// and nothing else can be blamed for the outcome.
func goodRuleset() TrustedMainRuleset {
	return TrustedMainRuleset{
		ID: 22043609, Name: "trusted-main-adoption", Enforcement: "active",
		Targets: []string{"refs/heads/main"}, BypassActors: 0,
		PullRequest:    &PullRequestRule{AllowedMergeMethods: []string{"merge"}},
		RequiredChecks: &RequiredChecksRule{Strict: true, Checks: []RequiredCheck{{Context: "go", IntegrationID: 15368}}},
		Deletion:       true, NonFastForward: true,
	}
}

func TestTrustRootAcceptsOnlyAGateWorthTrusting(t *testing.T) {
	if err := VerifyTrustRoot(goodRuleset(), DefaultTrustPolicy()); err != nil {
		t.Fatalf("the intended trust root was refused: %v", err)
	}

	for name, tc := range map[string]struct {
		mutate func(*TrustedMainRuleset)
		says   string
	}{
		"inactive": {
			func(r *TrustedMainRuleset) { r.Enforcement = "evaluate" }, "not active",
		},
		"wrong target": {
			func(r *TrustedMainRuleset) { r.Targets = []string{"refs/heads/develop"} }, "does not include refs/heads/main",
		},
		"no target": {
			func(r *TrustedMainRuleset) { r.Targets = nil }, "does not include refs/heads/main",
		},
		"a bypass actor": {
			func(r *TrustedMainRuleset) { r.BypassActors = 1 }, "gates nothing",
		},
		"deletion allowed": {
			func(r *TrustedMainRuleset) { r.Deletion = false }, "deletion is not prohibited",
		},
		"non-fast-forward allowed": {
			func(r *TrustedMainRuleset) { r.NonFastForward = false }, "history could be rewritten",
		},
		"pull requests not required": {
			func(r *TrustedMainRuleset) { r.PullRequest = nil }, "pull requests are not required",
		},
		// Squash and rebase are the two that matter most: each replaces the
		// candidate with a different commit, so the runtime-owned object the
		// evidence is bound to is no longer reachable from the branch that
		// supposedly adopted it.
		"squash allowed": {
			func(r *TrustedMainRuleset) {
				r.PullRequest.AllowedMergeMethods = []string{"merge", "squash"}
			}, `merge method "squash" is allowed`,
		},
		"rebase allowed": {
			func(r *TrustedMainRuleset) {
				r.PullRequest.AllowedMergeMethods = []string{"merge", "rebase"}
			}, `merge method "rebase" is allowed`,
		},
		"no merge method stated": {
			func(r *TrustedMainRuleset) { r.PullRequest.AllowedMergeMethods = nil }, "no merge method is stated",
		},
		"no required check": {
			func(r *TrustedMainRuleset) { r.RequiredChecks = nil }, "no status check is required",
		},
		"required check absent": {
			func(r *TrustedMainRuleset) { r.RequiredChecks.Checks = nil }, "is absent or bound to a different app",
		},
		"required check from another app": {
			func(r *TrustedMainRuleset) {
				r.RequiredChecks.Checks = []RequiredCheck{{Context: "go", IntegrationID: 99999}}
			}, "bound to a different app",
		},
		"checks not strict": {
			func(r *TrustedMainRuleset) { r.RequiredChecks.Strict = false }, "not strict",
		},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			r := goodRuleset()
			tc.mutate(&r)
			err := VerifyTrustRoot(r, DefaultTrustPolicy())
			if err == nil {
				t.Fatal("a ruleset that is not a trust root was accepted")
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("refusal does not explain %q: %v", tc.says, err)
			}
		})
	}
}

// TestTrustRootReportsEveryGapAtOnce: an operator repairing a ruleset should
// see the whole gap, not discover it one round trip at a time.
func TestTrustRootReportsEveryGapAtOnce(t *testing.T) {
	r := goodRuleset()
	r.Enforcement = "disabled"
	r.BypassActors = 2
	r.PullRequest.AllowedMergeMethods = []string{"squash"}
	r.RequiredChecks.Strict = false

	var refusal *TrustRootError
	err := VerifyTrustRoot(r, DefaultTrustPolicy())
	if err == nil {
		t.Fatal("a thoroughly broken ruleset was accepted")
	}
	if !asTrustRootError(err, &refusal) || len(refusal.Reasons) < 4 {
		t.Fatalf("expected every gap at once, got %v", err)
	}
}

func asTrustRootError(err error, target **TrustRootError) bool {
	if e, ok := err.(*TrustRootError); ok {
		*target = e
		return true
	}
	return false
}

// TestNoTrustRootIsDistinctFromAWeakOne proves the two failures stay apart:
// "there is no gate" and "the gate is wrong" are different things to fix.
func TestNoTrustRootIsDistinctFromAWeakOne(t *testing.T) {
	if _, found := selectTrustRoot(nil, "refs/heads/main"); found {
		t.Fatal("a trust root was found where none exists")
	}
	if _, found := selectTrustRoot([]TrustedMainRuleset{{Targets: []string{"refs/heads/other"}}}, "refs/heads/main"); found {
		t.Fatal("a ruleset for another branch was accepted as the trust root")
	}
	if _, found := selectTrustRoot([]TrustedMainRuleset{goodRuleset()}, "refs/heads/main"); !found {
		t.Fatal("the governing ruleset was not selected")
	}
}
