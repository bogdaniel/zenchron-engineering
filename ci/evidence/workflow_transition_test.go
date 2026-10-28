package main

// The Stage 7 migration changes two independent things - the checks the ruleset
// requires and the checks the pull-request workflow produces. GitHub counts a job
// that its `if:` skips as a successful required check, so a required check that
// the workflow skips is satisfied without anything having run. These tests derive
// the pull-request outcomes from the real ci.yml and the required checks from the
// real ruleset payloads, and keep two questions apart: is the requirement
// satisfied (can the pull request merge), and did every required check execute
// (was anything verified before the merge). The ordered path in
// docs/ci-stage7-migration.md must satisfy both; the one state that satisfies the
// first and not the second is named and rejected.

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// pullRequestChecks are the status checks a pull request receives from a workflow:
// every job that can run on the event.
func pullRequestChecks(workflow workflowFile) []string {
	var checks []string
	for name, job := range workflow.Jobs {
		if job.runsOnPullRequest() {
			checks = append(checks, name)
		}
	}
	slices.Sort(checks)
	return checks
}

// requiredByRuleset reads the required status check contexts of a payload under
// ci/rulesets.
func requiredByRuleset(t *testing.T, name string) []string {
	t.Helper()
	raw, err := os.ReadFile("../rulesets/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				Checks []struct {
					Context       string `json:"context"`
					IntegrationID int64  `json:"integration_id"`
				} `json:"required_status_checks"`
			} `json:"parameters"`
		} `json:"rules"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	var contexts []string
	for _, rule := range payload.Rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		for _, check := range rule.Parameters.Checks {
			contexts = append(contexts, check.Context)
		}
	}
	slices.Sort(contexts)
	return contexts
}

// A required check is satisfied by an executed check and by a skipped one; only a
// check no job reports stays pending forever.
func satisfied(required []string, outcomes map[string]string) bool {
	for _, check := range required {
		if outcomes[check] == "" {
			return false
		}
	}
	return true
}

// verified is stronger: every required check actually ran on the pull request.
func verified(required []string, outcomes map[string]string) bool {
	for _, check := range required {
		if outcomes[check] != "executed" {
			return false
		}
	}
	return true
}

// pullRequestOutcomes is what a pull request reports for each job in the workflow:
// executed when the job's condition admits the event, skipped when it does not.
func pullRequestOutcomes(workflow workflowFile) map[string]string {
	outcomes := map[string]string{}
	for name, job := range workflow.Jobs {
		outcomes[name] = "skipped"
		if job.runsOnPullRequest() {
			outcomes[name] = "executed"
		}
	}
	return outcomes
}

// beforeStage7Outcomes is what the pull-request workflow reported before this
// change: the full suite executed beside T0/T1 and the race aggregate.
var beforeStage7Outcomes = map[string]string{
	"evidence": "executed", "evidence-race": "executed", "evidence-race-parts": "executed", "go": "executed",
}

func TestAPullRequestIsGatedByTheEvidenceJobsAndNotTheFullSuite(t *testing.T) {
	outcomes := pullRequestOutcomes(loadWorkflow(t))
	if outcomes["go"] != "skipped" {
		t.Fatalf("the full suite must not run on a pull request, outcomes: %v", outcomes)
	}
	required := requiredByRuleset(t, "trusted-main-adoption.stage7.json")
	if !verified(required, outcomes) {
		t.Fatalf("the Stage 7 ruleset requires %v and a pull request reports %v: it must require checks that execute", required, outcomes)
	}
	// The push still produces the exact T2 identity (workflow_test.go), and a push
	// is not a pull-request gate.
	if workflow := loadWorkflow(t); !workflow.Jobs["go"].runsOnPush() || workflow.Jobs["go"].runsOnPullRequest() {
		t.Fatalf("go must run on push and not on pull requests (if: %q)", workflow.Jobs["go"].If)
	}
}

func TestTheMigrationPathIsAlwaysSatisfiedAndAlwaysVerified(t *testing.T) {
	final := pullRequestOutcomes(loadWorkflow(t))
	before := requiredByRuleset(t, "trusted-main-adoption.pre-stage7.json")
	after := requiredByRuleset(t, "trusted-main-adoption.stage7.json")

	type state struct {
		name     string
		required []string
		outcomes map[string]string
	}
	beforeCurrent := state{"ruleset before / workflow before", before, beforeStage7Outcomes}
	afterCurrent := state{"ruleset after / workflow before", after, beforeStage7Outcomes}
	afterFinal := state{"ruleset after / workflow after", after, final}

	for _, path := range [][]state{
		{beforeCurrent, afterCurrent, afterFinal}, // activation: ruleset first, then the workflow
		{afterFinal, afterCurrent, beforeCurrent}, // rollback: the workflow first, then the ruleset
	} {
		for _, s := range path {
			if !satisfied(s.required, s.outcomes) {
				t.Errorf("%s: requires %v but a pull request reports %v - no merge path", s.name, s.required, s.outcomes)
			}
			if !verified(s.required, s.outcomes) {
				t.Errorf("%s: requires %v but not all of them execute (%v) - a merge would be unverified", s.name, s.required, s.outcomes)
			}
		}
	}
}

// The old ruleset requires go. Once go is push-only it reports skipped, which
// GitHub accepts, so the pull request is mergeable with no full suite executed
// and no evidence check required: the state the ruleset-first order avoids.
func TestTheOldRulesetDoesNotPreserveFullVerificationOncePushOnlyGo(t *testing.T) {
	final := pullRequestOutcomes(loadWorkflow(t))
	before := requiredByRuleset(t, "trusted-main-adoption.pre-stage7.json")
	if !slices.Equal(before, []string{"go"}) {
		t.Fatalf("the pre-Stage-7 ruleset requires %v, want exactly go", before)
	}
	if !satisfied(before, final) {
		t.Fatalf("a skipped go must satisfy the old requirement (GitHub accepts a skipped job); outcomes %v", final)
	}
	if verified(before, final) {
		t.Fatal("the old go-required ruleset must never be treated as preserving pre-merge verification when go is push-only")
	}
	if slices.Contains(before, "evidence") || slices.Contains(before, "evidence-race") {
		t.Fatal("the old ruleset requires an evidence check, so it is no longer the unsafe state docs/ci-stage7-migration.md describes")
	}
}

func TestACheckNoJobReportsStaysUnsatisfied(t *testing.T) {
	if satisfied([]string{"missing"}, pullRequestOutcomes(loadWorkflow(t))) {
		t.Fatal("a required check that no job reports must stay unsatisfied")
	}
}
