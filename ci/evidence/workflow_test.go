package main

// The CI workflow is where three decisions meet: which evidence a pull request
// must produce (T0/T1), which job is the exact-revision T2 the trusted-main
// resolver accepts, and which check names the ruleset requires. These tests read
// .github/workflows/ci.yml as text and hold the parts of it that other code and
// the ruleset depend on, so that editing a job cannot silently break adoption or
// leave a required check that nothing produces.

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

type workflowJob struct {
	If    string
	Needs string
	Runs  []string
}

type workflowFile struct {
	On   string
	Jobs map[string]*workflowJob
}

// parseWorkflow reads only the shapes ci.yml uses: top-level keys, two-space job
// keys, four-space job scalars, and single-line `run:` steps at any depth. It
// refuses a file it cannot read that way rather than guessing.
func parseWorkflow(t *testing.T, text string) workflowFile {
	t.Helper()
	file := workflowFile{Jobs: map[string]*workflowJob{}}
	var section string
	var job *workflowJob
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		switch {
		case indent == 0:
			section, job = strings.TrimSuffix(trimmed, ":"), nil
		case section == "on":
			file.On += trimmed + "\n"
		case section == "jobs" && indent == 2 && strings.HasSuffix(trimmed, ":"):
			job = &workflowJob{}
			file.Jobs[strings.TrimSuffix(trimmed, ":")] = job
		case section == "jobs" && job != nil && indent == 4 && strings.HasPrefix(trimmed, "if:"):
			job.If = strings.TrimSpace(strings.TrimPrefix(trimmed, "if:"))
		case section == "jobs" && job != nil && indent == 4 && strings.HasPrefix(trimmed, "needs:"):
			job.Needs = strings.TrimSpace(strings.TrimPrefix(trimmed, "needs:"))
		case section == "jobs" && job != nil && strings.Contains(trimmed, "run:"):
			command := strings.TrimSpace(trimmed[strings.Index(trimmed, "run:")+len("run:"):])
			if command == "|" || command == ">" {
				t.Fatalf("a multi-line run block is a shape this test does not read: %q", trimmed)
			}
			job.Runs = append(job.Runs, command)
		}
	}
	return file
}

func loadWorkflow(t *testing.T) workflowFile {
	t.Helper()
	text, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	return parseWorkflow(t, string(text))
}

// runsOnPullRequest and runsOnPush say whether a job's `if` can admit the event.
// A job with no condition runs on both.
func (j *workflowJob) runsOnPullRequest() bool {
	return j.If == "" || strings.Contains(j.If, "pull_request")
}
func (j *workflowJob) runsOnPush() bool { return j.If == "" || strings.Contains(j.If, "'push'") }

const fullSuiteCommand = "go test -json -timeout 45m ./..."

// The trusted-main resolver accepts one producer: GitHub Actions' job `go`, from
// ci.yml, on a push to main, for the exact commit (runtime/trusted_revision.go).
// Whatever else changes, that identity and that command must not.
func TestTheFullSuiteKeepsTheIdentityTheTrustedRevisionResolverAccepts(t *testing.T) {
	workflow := loadWorkflow(t)
	if !strings.Contains(workflow.On, "push:") || !strings.Contains(workflow.On, "branches: [main]") {
		t.Fatalf("the workflow no longer runs on a push to main:\n%s", workflow.On)
	}
	full, found := workflow.Jobs["go"]
	if !found {
		t.Fatal("there is no job named go: the exact-revision T2 evidence identity is gone")
	}
	if !full.runsOnPush() {
		t.Fatalf("the go job no longer runs on push (if: %q), so no revision on main can ever produce T2", full.If)
	}
	if full.Needs != "" {
		t.Fatalf("the go job needs %q: a skipped dependency would silently skip T2", full.Needs)
	}
	if !contains(full.Runs, fullSuiteCommand) {
		t.Fatalf("the go job no longer runs %q: %v", fullSuiteCommand, full.Runs)
	}
	if !contains(full.Runs, "ci/static.sh") {
		t.Fatalf("the go job dropped the static evidence it shares with the pull request jobs: %v", full.Runs)
	}
	// The unpartitioned full suite lives in exactly one job.
	for name, job := range workflow.Jobs {
		if name != "go" && contains(job.Runs, fullSuiteCommand) {
			t.Errorf("job %q also runs the full suite; T2 identity must be unambiguous", name)
		}
	}
}

// T1 produces no status on a push (the full suite is the push evidence), and its
// race aggregate fails closed: a failed, cancelled or skipped part can never pass
// the check name the ruleset requires.
func TestTheEvidenceJobsAreFailClosedAndPullRequestOnly(t *testing.T) {
	workflow := loadWorkflow(t)
	for _, name := range []string{"evidence", "evidence-race-parts", "evidence-race"} {
		job, found := workflow.Jobs[name]
		if !found {
			t.Fatalf("job %q is missing", name)
		}
		if !job.runsOnPullRequest() || job.runsOnPush() {
			t.Errorf("job %q must run on pull requests only (if: %q)", name, job.If)
		}
	}
	aggregate := workflow.Jobs["evidence-race"]
	if aggregate.Needs != "evidence-race-parts" {
		t.Errorf("the race aggregate needs %q, not its parts", aggregate.Needs)
	}
	if !strings.HasPrefix(aggregate.If, "always()") {
		t.Errorf("the race aggregate is skipped when a part fails (if: %q), and a skipped required check reads as passed", aggregate.If)
	}
	if len(aggregate.Runs) != 1 || !regexp.MustCompile(`test "\$\{\{ needs\.evidence-race-parts\.result \}\}" = success`).MatchString(aggregate.Runs[0]) {
		t.Errorf("the race aggregate does not require every part to have succeeded: %v", aggregate.Runs)
	}
	if !contains(workflow.Jobs["evidence"].Runs, "ci/static.sh") {
		t.Error("the pull request evidence job dropped T0")
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
