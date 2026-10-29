package runtime

// Stage 7 (#516, ADR-0007) changes WHICH pull-request checks the
// trusted-main-adoption ruleset requires, and nothing else. The two payloads in
// ci/rulesets are the exact before and after an operator applies. These tests hold
// them to the property the migration depends on: both are valid adoption trust
// roots, and they differ in the required status checks alone.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

func stage7Payload(t *testing.T, name string) (string, map[string]any) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "ci", "rulesets", name))
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	return string(raw), generic
}

func observeStage7Payload(t *testing.T, name string) TrustedMainRuleset {
	t.Helper()
	raw, _ := stage7Payload(t, name)
	doer := &fakeGitHubDoer{responses: map[string]string{
		"GET /repos/bogdaniel/zenchron-engineering/rulesets":          `[{"id":22043609,"name":"trusted-main-adoption"}]`,
		"GET /repos/bogdaniel/zenchron-engineering/rulesets/22043609": raw,
	}}
	observed, err := GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("s")}.Rulesets(context.Background(), governedRepo())
	if err != nil || len(observed) != 1 {
		t.Fatalf("%s: %v (%d rulesets)", name, err, len(observed))
	}
	return observed[0]
}

func requiredContexts(ruleset TrustedMainRuleset) []string {
	var contexts []string
	if ruleset.RequiredChecks != nil {
		for _, check := range ruleset.RequiredChecks.Checks {
			contexts = append(contexts, check.Context)
		}
	}
	slices.Sort(contexts)
	return contexts
}

func TestBothSidesOfTheStage7RulesetMigrationAreValidAdoptionTrustRoots(t *testing.T) {
	for _, name := range []string{"trusted-main-adoption.pre-stage7.json", "trusted-main-adoption.stage7.json"} {
		ruleset := observeStage7Payload(t, name)
		if err := VerifyTrustRoot(ruleset, DefaultBranchIntegrityPolicy()); err != nil {
			t.Errorf("%s is not a valid adoption trust root: %v", name, err)
		}
		if ruleset.RequiredChecks == nil || !ruleset.RequiredChecks.Strict {
			t.Errorf("%s dropped strict up-to-date enforcement", name)
		}
		for _, check := range ruleset.RequiredChecks.Checks {
			if check.IntegrationID != 15368 {
				t.Errorf("%s requires %q from integration %d, not GitHub Actions (15368)", name, check.Context, check.IntegrationID)
			}
		}
	}
}

func TestStage7ChangesTheRequiredChecksAndNothingElse(t *testing.T) {
	before, after := observeStage7Payload(t, "trusted-main-adoption.pre-stage7.json"), observeStage7Payload(t, "trusted-main-adoption.stage7.json")
	if !reflect.DeepEqual(requiredContexts(before), []string{"go"}) {
		t.Fatalf("the before payload requires %v, want exactly the full-suite go check", requiredContexts(before))
	}
	if !reflect.DeepEqual(requiredContexts(after), []string{"evidence", "evidence-race"}) {
		t.Fatalf("the after payload requires %v, want the T0/T1 evidence and its race aggregate", requiredContexts(after))
	}
	if slices.Contains(requiredContexts(after), "go") {
		t.Fatal("the after payload still requires go on pull requests")
	}
	// Everything the branch-integrity proof rests on is identical.
	before.RequiredChecks, after.RequiredChecks = nil, nil
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the migration changes more than the required checks:\nbefore %+v\nafter  %+v", before, after)
	}
	// And byte-for-byte outside the status-check rule, so a field the observer
	// does not model (a merge method, a bypass actor) cannot drift either.
	strip := func(name string) map[string]any {
		_, payload := stage7Payload(t, name)
		rules := payload["rules"].([]any)
		kept := rules[:0:0]
		for _, rule := range rules {
			if rule.(map[string]any)["type"] != "required_status_checks" {
				kept = append(kept, rule)
			}
		}
		payload["rules"] = kept
		return payload
	}
	if !reflect.DeepEqual(strip("trusted-main-adoption.pre-stage7.json"), strip("trusted-main-adoption.stage7.json")) {
		t.Fatal("the payloads differ outside the required status checks")
	}
}

// A required check that no workflow produces would deadlock merging; one that
// names the wrong producer would be satisfiable by anything. Both payloads pin
// the producer, and a ruleset with no required check at all is still a trust
// root (the check is merge policy, not trust - ADR-0007 section 1).
func TestTheTrustRootNeverDependsOnWhichChecksArePullRequestGates(t *testing.T) {
	ruleset := observeStage7Payload(t, "trusted-main-adoption.stage7.json")
	ruleset.RequiredChecks = nil
	if err := VerifyTrustRoot(ruleset, DefaultBranchIntegrityPolicy()); err != nil {
		t.Fatalf("a ruleset with no required check stopped being a trust root: %v", err)
	}
	ruleset.RequiredChecks = &RequiredChecksRule{Strict: true, Checks: []RequiredCheck{{Context: "anything-else", IntegrationID: 1}}}
	if err := VerifyTrustRoot(ruleset, DefaultBranchIntegrityPolicy()); err != nil {
		t.Fatalf("renaming the PR gate broke adoption: %v", err)
	}
	// ...while the integrity rules the proof does rest on are still refused.
	for name, mutate := range map[string]func(*TrustedMainRuleset){
		"a squash merge allowed":   func(r *TrustedMainRuleset) { r.PullRequest.AllowedMergeMethods = []string{"merge", "squash"} },
		"a bypass actor":           func(r *TrustedMainRuleset) { r.BypassActors = 1 },
		"deletion allowed":         func(r *TrustedMainRuleset) { r.Deletion = false },
		"non-fast-forward allowed": func(r *TrustedMainRuleset) { r.NonFastForward = false },
	} {
		weakened := observeStage7Payload(t, "trusted-main-adoption.stage7.json")
		mutate(&weakened)
		if err := VerifyTrustRoot(weakened, DefaultBranchIntegrityPolicy()); err == nil {
			t.Errorf("the Stage 7 ruleset with %s was accepted as a trust root", name)
		}
	}
}
