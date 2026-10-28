package main

// The Stage 7 migration changes two independent things - the checks the ruleset
// requires and the checks the pull-request workflow produces - and a required
// check that nothing produces can never pass, so no pull request could merge
// (there is no bypass actor). These tests derive the pull-request checks from the
// real ci.yml and the required checks from the real ruleset payloads, and hold the
// ordered path in docs/ci-stage7-migration.md to "every state on it is
// satisfiable", with the one state that is not named and rejected.

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

func satisfiable(required, produced []string) bool {
	for _, check := range required {
		if !slices.Contains(produced, check) {
			return false
		}
	}
	return true
}

// beforeStage7Produced is what the pull-request workflow produced before this
// change: the full suite beside T0/T1 and the race aggregate.
var beforeStage7Produced = []string{"evidence", "evidence-race", "evidence-race-parts", "go"}

func TestAPullRequestIsGatedByTheEvidenceJobsAndNotTheFullSuite(t *testing.T) {
	produced := pullRequestChecks(loadWorkflow(t))
	want := []string{"evidence", "evidence-race", "evidence-race-parts"}
	if !slices.Equal(produced, want) {
		t.Fatalf("a pull request produces %v, want exactly %v: the full suite must not run on a pull request", produced, want)
	}
	required := requiredByRuleset(t, "trusted-main-adoption.stage7.json")
	if !satisfiable(required, produced) {
		t.Fatalf("the Stage 7 ruleset requires %v and a pull request produces %v: nothing could merge", required, produced)
	}
	// The push still produces the exact T2 identity (workflow_test.go), and a push
	// is not a pull-request gate.
	if workflow := loadWorkflow(t); !workflow.Jobs["go"].runsOnPush() || workflow.Jobs["go"].runsOnPullRequest() {
		t.Fatalf("go must run on push and not on pull requests (if: %q)", workflow.Jobs["go"].If)
	}
}

func TestTheMigrationPathNeverPassesThroughAnUnsatisfiableRuleset(t *testing.T) {
	finalProduced := pullRequestChecks(loadWorkflow(t))
	before := requiredByRuleset(t, "trusted-main-adoption.pre-stage7.json")
	after := requiredByRuleset(t, "trusted-main-adoption.stage7.json")

	type state struct {
		name     string
		required []string
		produced []string
	}
	beforeCurrent := state{"ruleset before / workflow before", before, beforeStage7Produced}
	afterCurrent := state{"ruleset after / workflow before", after, beforeStage7Produced}
	afterFinal := state{"ruleset after / workflow after", after, finalProduced}
	deadlock := state{"ruleset before / workflow after", before, finalProduced}

	for _, path := range [][]state{
		{beforeCurrent, afterCurrent, afterFinal}, // activation: ruleset first, then the workflow
		{afterFinal, afterCurrent, beforeCurrent}, // rollback: the workflow first, then the ruleset
	} {
		for _, s := range path {
			if !satisfiable(s.required, s.produced) {
				t.Errorf("%s: requires %v but a pull request produces %v - no merge path", s.name, s.required, s.produced)
			}
		}
	}
	// The order is load-bearing: changing the workflow while the ruleset still
	// requires go is the state the procedure exists to avoid.
	if satisfiable(deadlock.required, deadlock.produced) {
		t.Errorf("%s is satisfiable, so the documented order is no longer load-bearing; update docs/ci-stage7-migration.md", deadlock.name)
	}
}
