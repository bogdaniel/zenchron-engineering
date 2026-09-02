package runtime

import (
	"errors"
	"strings"
	"testing"
)

// goodRuleset is the trust root as M1-B requires it. Every refusal case below
// is this value with exactly one thing wrong, so each test names one property
// and nothing else can be blamed for the outcome.
func goodRuleset() TrustedMainRuleset {
	return TrustedMainRuleset{
		ID: 22043609, Name: "trusted-main-adoption", Enforcement: "active",
		Targets: []string{"refs/heads/main"}, BypassActors: 0, BypassActorsKnown: true,
		TargetType:     "branch",
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
		// An OMITTED bypass list has told us nothing about bypasses, and
		// reading that silence as "there are none" is the difference between a
		// gate and the belief in a gate.
		"undisclosed bypass actors": {
			func(r *TrustedMainRuleset) { r.BypassActorsKnown = false }, "does not disclose its bypass actors",
		},
		"excluded ref": {
			func(r *TrustedMainRuleset) { r.Excluded = []string{"refs/heads/main"} }, "explicitly excluded",
		},
		"pattern ref condition": {
			func(r *TrustedMainRuleset) { r.Targets = []string{"refs/heads/*"} }, "cannot prove",
		},
		"non-branch target": {
			func(r *TrustedMainRuleset) { r.TargetType = "tag" }, "not branches",
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

// TestTrustRootSelectionRequiresExactlyOne proves the three answers stay
// apart: "there is no gate", "the gate is wrong", and "several rulesets govern
// this branch and their combined effect is not something this builder can
// prove". The last one is a refusal rather than a guess, because guessing how
// overlapping rulesets compose is how a gate gets reported that GitHub is not
// actually enforcing.
func TestTrustRootSelectionRequiresExactlyOne(t *testing.T) {
	if _, err := selectTrustRoot(nil, "refs/heads/main"); !errors.Is(err, ErrNoTrustRoot) {
		t.Fatalf("no ruleset should be ErrNoTrustRoot, got %v", err)
	}
	other := goodRuleset()
	other.Targets = []string{"refs/heads/other"}
	if _, err := selectTrustRoot([]TrustedMainRuleset{other}, "refs/heads/main"); !errors.Is(err, ErrNoTrustRoot) {
		t.Fatalf("a ruleset for another branch should be ErrNoTrustRoot, got %v", err)
	}
	// An excluded ref is not governed, however many includes name it.
	excluded := goodRuleset()
	excluded.Excluded = []string{"refs/heads/main"}
	if _, err := selectTrustRoot([]TrustedMainRuleset{excluded}, "refs/heads/main"); !errors.Is(err, ErrNoTrustRoot) {
		t.Fatalf("an excluded ref should be ErrNoTrustRoot, got %v", err)
	}
	second := goodRuleset()
	second.ID, second.Name = 999, "another-one"
	if _, err := selectTrustRoot([]TrustedMainRuleset{goodRuleset(), second}, "refs/heads/main"); err == nil ||
		!strings.Contains(err.Error(), "not something this builder can prove") {
		t.Fatalf("two applicable rulesets should be an ambiguity refusal, got %v", err)
	}
	// Exactly one, and one that does not apply alongside it, is fine.
	if got, err := selectTrustRoot([]TrustedMainRuleset{other, goodRuleset()}, "refs/heads/main"); err != nil ||
		got.Name != "trusted-main-adoption" {
		t.Fatalf("the governing ruleset was not selected: %+v / %v", got, err)
	}
}

// TestTrustRootDigestIsCanonical proves the digest goes through the
// repository's JCS path, so two observations that are the same trust root
// produce the same digest regardless of field order in the wire response.
func TestTrustRootDigestIsCanonical(t *testing.T) {
	first, err := trustRootDigest(goodRuleset())
	if err != nil {
		t.Fatal(err)
	}
	second, err := trustRootDigest(goodRuleset())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("digest is not a stable canonical digest: %q / %q", first, second)
	}
	weakened := goodRuleset()
	weakened.PullRequest.AllowedMergeMethods = []string{"merge", "squash"}
	changed, err := trustRootDigest(weakened)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("a weakened trust root produced the same digest")
	}
}
