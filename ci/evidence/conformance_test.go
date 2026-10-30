package main

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// ADR-0007 section 6: until domain-aware evidence reuse exists, GitHub's strict
// up-to-date requirement is the explicit, conservative invalidation rule: the
// evidence of a pull request head never authorizes a head that contains a newer
// main, so a base move forces a new head and a new T0/T1 run. That rule lives in
// the ruleset payload, so the payload is where it is held.
func TestTheRulesetInvalidatesPullRequestEvidenceWhenMainMoves(t *testing.T) {
	raw, err := os.ReadFile("../rulesets/trusted-main-adoption.stage7.json")
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Target       string `json:"target"`
		Enforcement  string `json:"enforcement"`
		BypassActors []any  `json:"bypass_actors"`
		Conditions   struct {
			RefName struct {
				Include []string `json:"include"`
				Exclude []string `json:"exclude"`
			} `json:"ref_name"`
		} `json:"conditions"`
		Rules []struct {
			Type       string `json:"type"`
			Parameters struct {
				Strict bool `json:"strict_required_status_checks_policy"`
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
	if payload.Enforcement != "active" || len(payload.BypassActors) != 0 {
		t.Errorf("the ruleset must be active with no bypass actors: %q, %d", payload.Enforcement, len(payload.BypassActors))
	}
	ref := payload.Conditions.RefName
	if payload.Target != "branch" || !slices.Contains(ref.Include, "refs/heads/main") || slices.Contains(ref.Exclude, "refs/heads/main") {
		t.Errorf("the ruleset must target the branch refs/heads/main: target %q, include %v, exclude %v", payload.Target, ref.Include, ref.Exclude)
	}
	var contexts []string
	strict := false
	for _, rule := range payload.Rules {
		if rule.Type != "required_status_checks" {
			continue
		}
		strict = rule.Parameters.Strict
		for _, check := range rule.Parameters.Checks {
			contexts = append(contexts, check.Context)
			if check.IntegrationID != 15368 {
				t.Errorf("required check %q is not pinned to GitHub Actions (app 15368): %d", check.Context, check.IntegrationID)
			}
		}
	}
	slices.Sort(contexts)
	if !strict {
		t.Error("strict up-to-date is off: evidence for an older base would authorize a head that never ran against the current main")
	}
	if !slices.Equal(contexts, []string{"evidence", "evidence-race"}) {
		t.Errorf("required checks are %v, want evidence and evidence-race", contexts)
	}
}

// AGENTS.md: Windows and non-Unix coverage lives in T0. A build-tagged file no
// job compiles rots unseen (control_endpoint_other.go did), so the static script
// must keep compiling and vetting for a non-Unix target, tests included.
func TestStaticEvidenceKeepsCompilingAndVettingForANonUnixTarget(t *testing.T) {
	raw, err := os.ReadFile("../static.sh")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "\nset -euo pipefail\n") {
		t.Error("ci/static.sh must fail on the first failing command (set -euo pipefail)")
	}
	for _, command := range []string{
		"GOOS=windows GOARCH=amd64 go build ./...",
		"GOOS=windows GOARCH=amd64 go vet ./...",
		"gofmt -l .",
		"go vet ./...",
	} {
		lines := 0
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), command) || strings.Contains(line, "$("+command) {
				lines++
				if strings.Contains(line, "||") || strings.Contains(line, ";") {
					t.Errorf("ci/static.sh suppresses or chains the failure of %q: %s", command, line)
				}
			}
		}
		if lines != 1 {
			t.Errorf("ci/static.sh runs %q %d times, want exactly once", command, lines)
		}
	}
}
