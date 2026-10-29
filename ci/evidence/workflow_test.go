package main

// The CI workflow is where three decisions meet: which evidence a pull request
// must produce (T0/T1), which job is the exact-revision T2 the trusted-main
// resolver accepts, and which check names the ruleset requires. These tests read
// .github/workflows/ci.yml as text and hold the parts of it that other code and
// the ruleset depend on, so that editing a job cannot silently break adoption or
// leave a required check that nothing produces.

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type workflowJob struct {
	If     string
	Needs  string
	Runs   []string
	onPush bool
	onPull bool
}

type workflowFile struct {
	// Triggers maps each event under `on:` to its branch filter (nil when it has none).
	Triggers map[string][]string
	Jobs     map[string]*workflowJob
}

// supportedConditions is every job-level `if` ci.yml may use and the events it
// admits. A condition outside this list is refused, not interpreted: a negation
// or a different event name that merely contains "push" must never read as "runs
// on push".
var supportedConditions = map[string]struct{ push, pull bool }{
	"":                                    {true, true},
	"github.event_name == 'push'":         {true, false},
	"github.event_name == 'pull_request'": {false, true},
	"always() && github.event_name == 'pull_request'": {false, true},
}

var branchFilter = regexp.MustCompile(`^branches: \[([^\]]*)\]$`)

// parseWorkflow reads only the shapes ci.yml uses: two event keys under `on:` with
// an optional one-line branch filter, top-level keys, two-space job keys,
// four-space job scalars, and single-line `run:` steps at any depth. It refuses a
// file it cannot read that way - a multi-line run, a step-level `if`,
// continue-on-error, an unknown trigger shape or an unsupported condition -
// rather than guessing.
func parseWorkflow(text string) (workflowFile, error) {
	file := workflowFile{Triggers: map[string][]string{}, Jobs: map[string]*workflowJob{}}
	var section, event string
	var job *workflowJob
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.Contains(trimmed, "continue-on-error") {
			return file, fmt.Errorf("continue-on-error is not a shape this test reads: %q", trimmed)
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		isCondition := strings.HasPrefix(trimmed, "if:") || strings.HasPrefix(trimmed, "- if:")
		switch {
		case indent == 0:
			section, job, event = strings.TrimSuffix(trimmed, ":"), nil, ""
		case section == "on" && indent == 2 && strings.HasSuffix(trimmed, ":"):
			event = strings.TrimSuffix(trimmed, ":")
			file.Triggers[event] = nil
		case section == "on" && indent == 4 && event != "" && branchFilter.MatchString(trimmed):
			for _, branch := range strings.Split(branchFilter.FindStringSubmatch(trimmed)[1], ",") {
				file.Triggers[event] = append(file.Triggers[event], strings.TrimSpace(branch))
			}
		case section == "on":
			return file, fmt.Errorf("an `on:` shape this test does not read: %q", trimmed)
		case section == "jobs" && indent == 2 && strings.HasSuffix(trimmed, ":"):
			job = &workflowJob{}
			file.Jobs[strings.TrimSuffix(trimmed, ":")] = job
		case section == "jobs" && isCondition && indent != 4:
			return file, fmt.Errorf("a step-level condition can silently skip evidence: %q", trimmed)
		case section == "jobs" && job != nil && indent == 4 && strings.HasPrefix(trimmed, "if:"):
			job.If = strings.TrimSpace(strings.TrimPrefix(trimmed, "if:"))
		case section == "jobs" && job != nil && indent == 4 && strings.HasPrefix(trimmed, "needs:"):
			job.Needs = strings.TrimSpace(strings.TrimPrefix(trimmed, "needs:"))
		case section == "jobs" && job != nil && strings.Contains(trimmed, "run:"):
			command := strings.TrimSpace(trimmed[strings.Index(trimmed, "run:")+len("run:"):])
			if command == "|" || command == ">" {
				return file, fmt.Errorf("a multi-line run block is a shape this test does not read: %q", trimmed)
			}
			job.Runs = append(job.Runs, command)
		}
	}
	for name, job := range file.Jobs {
		events, supported := supportedConditions[job.If]
		if !supported {
			return file, fmt.Errorf("job %q has an unsupported condition %q", name, job.If)
		}
		job.onPush, job.onPull = events.push, events.pull
	}
	return file, nil
}

func workflowText(t *testing.T) string {
	t.Helper()
	text, err := os.ReadFile("../../.github/workflows/ci.yml")
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func loadWorkflow(t *testing.T) workflowFile {
	t.Helper()
	file, err := parseWorkflow(workflowText(t))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// runsOnPullRequest and runsOnPush say which events a job's supported condition
// admits. A job with no condition runs on both.
func (j *workflowJob) runsOnPullRequest() bool { return j.onPull }
func (j *workflowJob) runsOnPush() bool        { return j.onPush }

const (
	fullSuiteCommand = "go test -json -timeout 45m ./..."
	aggregateCommand = `test "${{ needs.evidence-race-parts.result }}" = success`
)

// checkTriggers: the workflow runs on a push to main (the T2 event) and on every
// pull request, and on nothing else. The branch filter belongs to its own event.
func checkTriggers(w workflowFile) error {
	var errs []error
	if push, found := w.Triggers["push"]; !found || !slices.Equal(push, []string{"main"}) {
		errs = append(errs, fmt.Errorf("push must trigger on exactly [main], got %v (present: %t)", push, found))
	}
	if pull, found := w.Triggers["pull_request"]; !found || len(pull) != 0 {
		errs = append(errs, fmt.Errorf("pull_request must trigger on every branch, got %v (present: %t)", pull, found))
	}
	if len(w.Triggers) != 2 {
		errs = append(errs, fmt.Errorf("unexpected triggers %v", w.Triggers))
	}
	return errors.Join(errs...)
}

// checkFullSuiteIdentity: the trusted-main resolver accepts one producer, GitHub
// Actions' job `go`, from ci.yml, on a push to main, for the exact commit
// (runtime/trusted_revision.go). Whatever else changes, that identity and that
// command must not.
func checkFullSuiteIdentity(w workflowFile) error {
	full, found := w.Jobs["go"]
	if !found {
		return errors.New("there is no job named go: the exact-revision T2 evidence identity is gone")
	}
	var errs []error
	if !full.runsOnPush() {
		errs = append(errs, fmt.Errorf("the go job no longer runs on push (if: %q), so no revision on main can ever produce T2", full.If))
	}
	if full.Needs != "" {
		errs = append(errs, fmt.Errorf("the go job needs %q: a skipped dependency would silently skip T2", full.Needs))
	}
	if !contains(full.Runs, fullSuiteCommand) {
		errs = append(errs, fmt.Errorf("the go job no longer runs %q: %v", fullSuiteCommand, full.Runs))
	}
	if !contains(full.Runs, "ci/static.sh") {
		errs = append(errs, fmt.Errorf("the go job dropped the static evidence it shares with the pull request jobs: %v", full.Runs))
	}
	// The unpartitioned full suite lives in exactly one job.
	for name, job := range w.Jobs {
		if name != "go" && contains(job.Runs, fullSuiteCommand) {
			errs = append(errs, fmt.Errorf("job %q also runs the full suite; T2 identity must be unambiguous", name))
		}
	}
	return errors.Join(errs...)
}

// checkEvidenceJobs: T1 produces no status on a push (the full suite is the push
// evidence), and its race aggregate fails closed: a failed, cancelled or skipped
// part can never pass the check name the ruleset requires. The aggregate command
// must be exactly the comparison, so a trailing `|| true` cannot absorb a failure.
func checkEvidenceJobs(w workflowFile) error {
	var errs []error
	for _, name := range []string{"evidence", "evidence-race-parts", "evidence-race"} {
		job, found := w.Jobs[name]
		if !found {
			errs = append(errs, fmt.Errorf("job %q is missing", name))
			continue
		}
		if !job.runsOnPullRequest() || job.runsOnPush() {
			errs = append(errs, fmt.Errorf("job %q must run on pull requests only (if: %q)", name, job.If))
		}
	}
	aggregate, found := w.Jobs["evidence-race"]
	if !found {
		return errors.Join(errs...)
	}
	if aggregate.Needs != "evidence-race-parts" {
		errs = append(errs, fmt.Errorf("the race aggregate needs %q, not its parts", aggregate.Needs))
	}
	if !strings.HasPrefix(aggregate.If, "always()") {
		errs = append(errs, fmt.Errorf("the race aggregate is skipped when a part fails (if: %q), and a skipped required check reads as passed", aggregate.If))
	}
	if !slices.Equal(aggregate.Runs, []string{aggregateCommand}) {
		errs = append(errs, fmt.Errorf("the race aggregate must run exactly %q, got %v", aggregateCommand, aggregate.Runs))
	}
	if evidence, found := w.Jobs["evidence"]; found && !contains(evidence.Runs, "ci/static.sh") {
		errs = append(errs, errors.New("the pull request evidence job dropped T0"))
	}
	return errors.Join(errs...)
}

func TestTheWorkflowTriggersAreTheEventsTheEvidenceModelNeeds(t *testing.T) {
	if err := checkTriggers(loadWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTheFullSuiteKeepsTheIdentityTheTrustedRevisionResolverAccepts(t *testing.T) {
	if err := checkFullSuiteIdentity(loadWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

func TestTheEvidenceJobsAreFailClosedAndPullRequestOnly(t *testing.T) {
	if err := checkEvidenceJobs(loadWorkflow(t)); err != nil {
		t.Fatal(err)
	}
}

// withJobCondition replaces (or adds) the job-level `if` of one job.
func withJobCondition(text, job, condition string) string {
	var out []string
	inJob := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "  "+job+":") {
			inJob = true
			out = append(out, line, "    if: "+condition)
			continue
		}
		if inJob && strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") {
			inJob = false
		}
		if inJob && strings.HasPrefix(line, "    if:") {
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// Each mutation is a way to leave the checks green while losing T2, the pull
// request gate, or the fail-closed aggregate. The checks must reject every one.
func TestTheWorkflowChecksRejectTheirNegativeMutations(t *testing.T) {
	text := workflowText(t)
	mutations := map[string]string{
		"go runs everywhere except a push":                    withJobCondition(text, "go", "github.event_name != 'push'"),
		"go runs only on pull requests":                       withJobCondition(text, "go", "github.event_name == 'pull_request'"),
		"evidence runs everywhere except a pull request":      withJobCondition(text, "evidence", "github.event_name != 'pull_request'"),
		"push filtered to dev, pull_request filtered to main": strings.Replace(strings.Replace(text, "branches: [main]", "branches: [dev]", 1), "\n  pull_request:\n", "\n  pull_request:\n    branches: [main]\n", 1),
		"no pull_request trigger":                             strings.Replace(text, "\n  pull_request:\n", "\n", 1),
		"race aggregate ignores failure":                      strings.Replace(text, aggregateCommand, aggregateCommand+" || true", 1),
		"race aggregate continues on error":                   strings.Replace(text, "\n  evidence-race:\n", "\n  evidence-race:\n    continue-on-error: true\n", 1),
		"full suite step skipped":                             strings.Replace(text, "      - name: Test\n", "      - name: Test\n        if: false\n", 1),
	}
	for name, mutated := range mutations {
		if mutated == text {
			t.Fatalf("mutation %q did not change the workflow; the test no longer exercises it", name)
		}
		file, err := parseWorkflow(mutated)
		if err == nil {
			err = errors.Join(checkTriggers(file), checkFullSuiteIdentity(file), checkEvidenceJobs(file))
		}
		if err == nil {
			t.Errorf("mutation %q was accepted", name)
		}
	}
}

func contains(values []string, want string) bool {
	return slices.Contains(values, want)
}
