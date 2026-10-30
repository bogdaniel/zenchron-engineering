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
	"strconv"
	"strings"
	"testing"
)

type workflowJob struct {
	If     string
	Needs  string
	Matrix []string
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
			if job.Needs == "" {
				return file, fmt.Errorf("a block-style needs list is not a shape this test reads: %q", trimmed)
			}
		case section == "jobs" && job != nil && strings.HasPrefix(trimmed, "shard:"):
			list := strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "shard:")), "[]")
			for _, shard := range strings.Split(list, ",") {
				job.Matrix = append(job.Matrix, strings.TrimSpace(shard))
			}
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

const fullSuiteCommand = "go test -json -timeout 45m ./..."

// needList reads `needs: a` and `needs: [a, b]`.
func (j *workflowJob) needList() []string {
	var needs []string
	for _, need := range strings.Split(strings.Trim(j.Needs, "[]"), ",") {
		needs = append(needs, strings.TrimSpace(need))
	}
	return needs
}

// evidenceAggregates maps each check a ruleset requires to the jobs whose
// results it aggregates, in the order its command tests them.
var evidenceAggregates = []struct {
	check string
	parts []string
}{
	{"evidence", []string{"evidence-static", "evidence-packages", "evidence-runtime"}},
	{"evidence-race", []string{"evidence-race-parts"}},
}

// aggregateCommand is the only command an aggregate may run: every part must
// have succeeded, so a failed, cancelled or skipped part fails the check.
func aggregateCommand(parts []string) string {
	tests := make([]string, len(parts))
	for i, part := range parts {
		tests[i] = `test "${{ needs.` + part + `.result }}" = success`
	}
	return strings.Join(tests, " && ")
}

// shardedJobs are the matrix jobs whose shards must be exactly 1..N of the N the
// command partitions by. A matrix that drops a shard, or a command whose N
// outgrows the matrix, silently stops running part of the selected evidence.
var shardedJobs = []struct{ job, flag string }{
	{"evidence-runtime", "-shard"},
	{"evidence-race-parts", "-race-shard"},
}

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
	require := func(name string) *workflowJob {
		job, found := w.Jobs[name]
		if !found {
			errs = append(errs, fmt.Errorf("job %q is missing", name))
			return nil
		}
		if !job.runsOnPullRequest() || job.runsOnPush() {
			errs = append(errs, fmt.Errorf("job %q must run on pull requests only (if: %q)", name, job.If))
		}
		return job
	}
	for _, aggregate := range evidenceAggregates {
		job := require(aggregate.check)
		for _, part := range aggregate.parts {
			require(part)
		}
		if job == nil {
			continue
		}
		if !slices.Equal(job.needList(), aggregate.parts) {
			errs = append(errs, fmt.Errorf("the %s aggregate needs %v, not its parts %v", aggregate.check, job.needList(), aggregate.parts))
		}
		if !strings.HasPrefix(job.If, "always()") {
			errs = append(errs, fmt.Errorf("the %s aggregate is skipped when a part fails (if: %q), and a skipped required check reads as passed", aggregate.check, job.If))
		}
		if want := aggregateCommand(aggregate.parts); !slices.Equal(job.Runs, []string{want}) {
			errs = append(errs, fmt.Errorf("the %s aggregate must run exactly %q, got %v", aggregate.check, want, job.Runs))
		}
	}
	if static, found := w.Jobs["evidence-static"]; found && !contains(static.Runs, "ci/static.sh") {
		errs = append(errs, errors.New("the pull request static job dropped T0"))
	}
	if packages, found := w.Jobs["evidence-packages"]; found && !slices.ContainsFunc(packages.Runs, func(run string) bool { return strings.Contains(run, "go run ./ci/evidence -packages ") }) {
		errs = append(errs, errors.New("the pull request packages job no longer runs the non-runtime packages"))
	}
	for _, sharded := range shardedJobs {
		if job, found := w.Jobs[sharded.job]; found {
			errs = append(errs, checkShards(sharded.job, sharded.flag, job))
		}
	}
	return errors.Join(errs...)
}

// checkShards: the job's matrix is exactly 1..N and its one evidence command
// partitions by the same N, so the shards are the whole selection.
func checkShards(name, flag string, job *workflowJob) error {
	pattern := regexp.MustCompile(regexp.QuoteMeta(flag+` "${{ matrix.shard }}/`) + `(\d+)"`)
	var counts []string
	for _, run := range job.Runs {
		if match := pattern.FindStringSubmatch(run); match != nil {
			counts = append(counts, match[1])
		}
	}
	if len(counts) != 1 {
		return fmt.Errorf("job %q must partition by `%s \"${{ matrix.shard }}/N\"` exactly once, found %d", name, flag, len(counts))
	}
	var want []string
	for shard := 1; shard <= atoi(counts[0]); shard++ {
		want = append(want, fmt.Sprint(shard))
	}
	if !slices.Equal(job.Matrix, want) {
		return fmt.Errorf("job %q partitions by %s but its matrix is %v, not %v: part of the selected evidence would not run", name, counts[0], job.Matrix, want)
	}
	return nil
}

func atoi(text string) int {
	n, _ := strconv.Atoi(text)
	return n
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
		"race aggregate ignores failure":                      strings.Replace(text, aggregateCommand([]string{"evidence-race-parts"}), aggregateCommand([]string{"evidence-race-parts"})+" || true", 1),
		"race aggregate continues on error":                   strings.Replace(text, "\n  evidence-race:\n", "\n  evidence-race:\n    continue-on-error: true\n", 1),
		"evidence aggregate ignores a part":                   strings.Replace(text, ` && test "${{ needs.evidence-runtime.result }}" = success`, "", 1),
		"evidence aggregate does not wait for the runtime":    strings.Replace(text, "needs: [evidence-static, evidence-packages, evidence-runtime]", "needs: [evidence-static, evidence-packages]", 1),
		"evidence aggregate is skipped when a part fails":     withJobCondition(text, "evidence", "github.event_name == 'pull_request'"),
		"static evidence dropped":                             strings.Replace(text, "run: ci/static.sh\n\n  evidence-packages", "run: echo skipped\n\n  evidence-packages", 1),
		"the packages are no longer run":                      strings.Replace(text, "-packages -base", "-plan -base", 1),
		"a runtime shard is dropped from the matrix":          strings.Replace(text, "shard: [1, 2, 3, 4]\n    steps:\n      - name: Checkout\n        uses: actions/checkout@v4\n        with:\n          fetch-depth: 0\n\n      - name: Set up Go\n        uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n          cache: true\n\n      - name: Selected runtime", "shard: [1, 2, 3]\n    steps:\n      - name: Checkout\n        uses: actions/checkout@v4\n        with:\n          fetch-depth: 0\n\n      - name: Set up Go\n        uses: actions/setup-go@v5\n        with:\n          go-version-file: go.mod\n          cache: true\n\n      - name: Selected runtime", 1),
		"a race shard is dropped from the matrix":             strings.Replace(text, "shard: [1, 2, 3, 4, 5, 6, 7, 8]", "shard: [1, 2, 3, 4, 5, 6, 7]", 1),
		"the runtime partition outgrows its matrix":           strings.Replace(text, `-shard "${{ matrix.shard }}/4"`, `-shard "${{ matrix.shard }}/5"`, 1),
		"full suite waits on a block-style dependency":        strings.Replace(text, "\n  go:\n", "\n  go:\n    needs:\n      - evidence\n", 1),
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
