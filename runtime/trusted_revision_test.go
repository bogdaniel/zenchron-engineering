package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// ADR-0007 deliberate breaks for the trusted_main resolver. Each test names the
// break it catches.

func shaOf(c byte) string { return strings.Repeat(string(c), 40) }

// chainLineage is a first-parent chain (newest first) and the attempts the
// forge reports per revision.
func chainLineage(chain []string, attempts map[string][]T2Attempt) TrustedMainLineage {
	return TrustedMainLineage{
		FirstParent: func(head string, max int) ([]string, error) {
			if len(chain) > max {
				return chain[:max], nil
			}
			return chain, nil
		},
		Evidence:   func(revision string) ([]T2Attempt, error) { return attempts[revision], nil },
		ObservedBy: CredentialProvenance{Role: CredentialRoleGovernance, Method: "test"},
		Now:        func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
}

func pending(revision string) T2Attempt {
	a := t2Attempt(revision, 9, 1, "")
	a.Status = "in_progress"
	return a
}

// Breaks 3 and 4: a failed T2 holds trust back at the newest accepted
// revision, a pending one is skipped, and a newer green advances trust.
func TestTrustedMainHoldsBackOnFailureAndAdvancesOnGreen(t *testing.T) {
	a, b, c, d := shaOf('a'), shaOf('b'), shaOf('c'), shaOf('d')
	chain := []string{d, c, b, a}
	attempts := map[string][]T2Attempt{
		a: {t2Attempt(a, 1, 1, "success")},
		b: {t2Attempt(b, 2, 1, "success")},
		c: {t2Attempt(c, 3, 1, "failure")},
		d: {pending(d)},
	}
	got, err := ResolveTrustedMain(DefaultTrustedRevisionPolicy(), d, chainLineage(chain, attempts))
	if err != nil || got.MainHead != d || got.TrustedMain != b {
		t.Fatalf("resolution = %+v, %v; want main_head d, trusted_main b", got, err)
	}
	if !slices.Equal(got.Skipped, []SkippedRevision{{d, "T2 pending"}, {c, "T2 failure (run 3 attempt 1)"}}) {
		t.Fatalf("skipped = %+v", got.Skipped)
	}
	if !got.Evidence.Eligible || got.Evidence.Subject != b || got.Evidence.ObservedBy.Role != CredentialRoleGovernance {
		t.Fatalf("evidence = %+v", got.Evidence)
	}

	attempts[d] = []T2Attempt{t2Attempt(d, 4, 1, "success")}
	if got, err := ResolveTrustedMain(DefaultTrustedRevisionPolicy(), d, chainLineage(chain, attempts)); err != nil || got.TrustedMain != d {
		t.Fatalf("a green main_head did not advance trust: %+v, %v", got, err)
	}
}

// Break 6: nothing accepted within the bound is no trusted main, never
// main_head.
func TestNoAcceptedRevisionWithinTheBoundIsNoTrustedMain(t *testing.T) {
	policy := DefaultTrustedRevisionPolicy()
	policy.MaxFirstParent = 2
	a, b, c := shaOf('a'), shaOf('b'), shaOf('c')
	attempts := map[string][]T2Attempt{a: {t2Attempt(a, 1, 1, "success")}, b: {t2Attempt(b, 2, 1, "failure")}}
	got, err := ResolveTrustedMain(policy, c, chainLineage([]string{c, b, a}, attempts))
	if !errors.Is(err, ErrNoTrustedMain) || got.TrustedMain != "" {
		t.Fatalf("resolution = %+v, %v; want ErrNoTrustedMain and no trusted main", got, err)
	}
}

// Breaks 1 and 7: evidence must be about exactly this revision, from exactly
// the pinned producer.
func TestT2EvidenceIsExactSubjectAndPinnedProducer(t *testing.T) {
	r := shaOf('r')
	for name, mutate := range map[string]func(*T2Attempt){
		"another head sha":     func(a *T2Attempt) { a.HeadSHA = shaOf('x') },
		"another app":          func(a *T2Attempt) { a.IntegrationID = 99999 },
		"an undisclosed app":   func(a *T2Attempt) { a.IntegrationID = 0 },
		"another workflow":     func(a *T2Attempt) { a.Workflow = ".github/workflows/assurance.yml" },
		"a pull request event": func(a *T2Attempt) { a.Event = "pull_request" },
		"another branch":       func(a *T2Attempt) { a.Branch = "claude/feature" },
		"another job":          func(a *T2Attempt) { a.Job = "evidence" },
	} {
		attempt := t2Attempt(r, 1, 1, "success")
		mutate(&attempt)
		if got := EvaluateT2Evidence(DefaultTrustedRevisionPolicy(), r, []T2Attempt{attempt}); got.Eligible || len(got.Attempts) != 0 {
			t.Errorf("%s was accepted as T2 evidence: %+v", name, got)
		}
	}
}

// Breaks 10 and 11: the latest completed attempt decides, a pending re-run
// overrides nothing, and disagreement is marked without changing eligibility.
func TestTheLatestCompletedAttemptDecidesAndDisagreementIsVisible(t *testing.T) {
	r := shaOf('r')
	rerunning := pending(r)
	rerunning.RunID, rerunning.Attempt = 1, 3
	for name, tc := range map[string]struct {
		attempts     []T2Attempt
		eligible     bool
		inconsistent bool
	}{
		"green then red":           {[]T2Attempt{t2Attempt(r, 1, 1, "success"), t2Attempt(r, 1, 2, "failure")}, false, true},
		"red then green":           {[]T2Attempt{t2Attempt(r, 1, 2, "success"), t2Attempt(r, 1, 1, "failure")}, true, true},
		"green, re-run pending":    {[]T2Attempt{t2Attempt(r, 1, 1, "success"), rerunning}, true, false},
		"cancelled is not success": {[]T2Attempt{t2Attempt(r, 1, 1, "cancelled")}, false, false},
	} {
		got := EvaluateT2Evidence(DefaultTrustedRevisionPolicy(), r, tc.attempts)
		if got.Eligible != tc.eligible || got.Inconsistent != tc.inconsistent || len(got.Attempts) != len(tc.attempts) {
			t.Errorf("%s: eligible=%t inconsistent=%t attempts=%d", name, got.Eligible, got.Inconsistent, len(got.Attempts))
		}
	}
}

// Break 2: a green check on a commit reachable only through a merge's second
// parent is never a candidate, even though it is an ancestor of main_head.
func TestOnlyTheFirstParentChainIsACandidate(t *testing.T) {
	dir := t.TempDir()
	base := initFixtureRepo(t, dir, "README.md", "base\n")
	commit := func(file string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, file), []byte(file), 0600); err != nil {
			t.Fatal(err)
		}
		adoptedGit(t, dir, "add", "-A")
		adoptedGit(t, dir, "commit", "-q", "-m", file)
		return adoptedGit(t, dir, "rev-parse", "HEAD")
	}
	mainBranch := adoptedGit(t, dir, "rev-parse", "--abbrev-ref", "HEAD")
	adoptedGit(t, dir, "checkout", "-q", "-b", "side")
	side := commit("side.txt")
	adoptedGit(t, dir, "checkout", "-q", mainBranch)
	adoptedGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge side", "side")
	merge := adoptedGit(t, dir, "rev-parse", "HEAD")

	attempts := map[string][]T2Attempt{
		merge: {t2Attempt(merge, 3, 1, "failure")},
		side:  {t2Attempt(side, 2, 1, "success")},
		base:  {t2Attempt(base, 1, 1, "success")},
	}
	lineage := chainLineage(nil, attempts)
	lineage.FirstParent = func(head string, max int) ([]string, error) { return firstParentChain(gitOutput, dir, head, max) }
	if chain, err := firstParentChain(gitOutput, dir, merge, 256); err != nil || !slices.Equal(chain, []string{merge, base}) {
		t.Fatalf("main's history = %v, %v; want only the first-parent chain [merge base]", chain, err)
	}
	got, err := ResolveTrustedMain(DefaultTrustedRevisionPolicy(), merge, lineage)
	if err != nil || got.TrustedMain != base {
		t.Fatalf("trusted main = %s, %v; want the first-parent base, never the side commit", got.TrustedMain, err)
	}
}

// Break 5: adoption defaults to trusted_main, not main_head, and records both.
func TestAdoptionStandsOnTrustedMainNotMainHead(t *testing.T) {
	f := newAdoptedFixture(t)
	parent := adoptedGit(t, f.dir, "rev-parse", f.head+"^")
	f.governance.evidence = func(revision string) ([]T2Attempt, error) {
		if revision == f.head {
			return []T2Attempt{t2Attempt(revision, 2, 1, "failure")}, nil
		}
		return []T2Attempt{t2Attempt(revision, 1, 1, "success")}, nil
	}
	got, err := BuildAdoptedController(context.Background(), f.request(t), f.deps, BuilderRecord{Kind: ControllerUnattested})
	if err != nil {
		t.Fatalf("the build was refused: %v", err)
	}
	if got.Source.Revision != parent || got.TrustedMain.Revision != parent || got.MainHead == nil || got.MainHead.Revision != f.head {
		t.Fatalf("source %s trusted %s main_head %+v; want source and trusted at the parent, main_head at head",
			shortSHA(got.Source.Revision), shortSHA(got.TrustedMain.Revision), got.MainHead)
	}
	if got.SchemaVersion != "adopted-build/2" || got.TrustEvidence == nil || got.TrustEvidence.Kind != TrustEvidenceT2 ||
		got.TrustEvidence.Observation.Subject != parent || len(got.Skipped) != 1 || got.Skipped[0].Revision != f.head {
		t.Fatalf("v2 trust fields = %+v %+v", got.TrustEvidence, got.Skipped)
	}
	if _, err := got.Projected(); err != nil {
		t.Fatalf("a v2 record does not project: %v", err)
	}

	// An explicit request for the newer, unassured main_head is refused.
	request := f.request(t)
	request.Revision = f.head
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil {
		t.Fatal("a revision past trusted main was adopted because main_head contains it")
	}
	assertNothingInstalled(t, request.OutputRoot)
}

// Break 6, at the builder: no accepted revision refuses and installs nothing.
func TestAdoptionWithNoTrustedMainRefuses(t *testing.T) {
	f := newAdoptedFixture(t)
	f.governance.evidence = func(revision string) ([]T2Attempt, error) {
		return []T2Attempt{t2Attempt(revision, 1, 1, "failure")}, nil
	}
	request := f.request(t)
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); !errors.Is(err, ErrNoTrustedMain) {
		t.Fatalf("err = %v, want ErrNoTrustedMain", err)
	}
	assertNothingInstalled(t, request.OutputRoot)
}

// Break 12: a v1 record projects to main_head == trusted_main with legacy
// trust evidence, synthesizes no T2 observation, and re-encodes byte for byte.
func TestAV1RecordProjectsToTheLegacyModel(t *testing.T) {
	const v1 = `{"schema_version":"adopted-build/1","repository":"acme/widgets","trust_root":{"ruleset_id":1,"name":"n","digest":"d",` +
		`"policy":{"ref":"refs/heads/main","required_check":{"context":"go","integration_id":15368},"allowed_merge_methods":["merge"],"require_strict_checks":true},` +
		`"enforcement":"active","observed_by":{"role":"governance-observation","method":"gh"},"bypass":{"observed":true,"count":0,"detail":"x"}},` +
		`"trusted_main":{"revision":"` + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `","tree":"t"},"source":{"revision":"s","tree":"t"},"containment_proof":"c",` +
		`"controller_kind":"adopted","version":"v","goos":"linux","goarch":"amd64","build_flags":null,"build_environment":{"kind":"","image":"","toolchain":"",` +
		`"network":"","source_mount":"","cache_mount":"","environment":null,"digest":""},"binary_sha256":"b","built_at":"t","builder":{"version":"",` +
		`"source_revision":"","kind":""},"output_path":"o","self_probe":{"kind":"","version":"","source_revision":"","source_tree":"","binary_sha256":"","matched":false}}`
	var record AdoptedBuildProvenance
	if err := json.Unmarshal([]byte(v1), &record); err != nil {
		t.Fatal(err)
	}
	if again, _ := json.Marshal(record); string(again) != v1 {
		t.Fatalf("a v1 record does not re-encode byte for byte:\n%s\n%s", again, v1)
	}
	projected, err := record.Projected()
	if err != nil {
		t.Fatal(err)
	}
	if projected.MainHead == nil || *projected.MainHead != record.TrustedMain {
		t.Fatalf("main_head = %+v, want trusted_main %+v", projected.MainHead, record.TrustedMain)
	}
	if projected.TrustEvidence.Kind != TrustEvidenceLegacy || projected.TrustEvidence.Observation != nil {
		t.Fatalf("v1 trust evidence = %+v, want legacy with no observation", projected.TrustEvidence)
	}
	if projected.TrustRoot.Policy.RequiredCheck == nil || !projected.TrustRoot.Policy.RequireStrictChecks {
		t.Fatalf("the v1 policy lost what it was verified under: %+v", projected.TrustRoot.Policy)
	}
}

// The observer reads every attempt of every run, joins each job to the app
// its check run names, and refuses anything that is not an exact commit
// before it reaches a URL.
func TestRevisionEvidenceReadsEveryAttempt(t *testing.T) {
	r := shaOf('a')
	doer := &fakeGitHubDoer{responses: map[string]string{
		"GET /repos/bogdaniel/zenchron-engineering/commits/" + r + "/check-runs": `{"total_count":2,"check_runs":[{"id":71,"app":{"id":15368}},{"id":72,"app":{"id":15368}}]}`,
		"GET /repos/bogdaniel/zenchron-engineering/actions/runs": `{"total_count":1,"workflow_runs":[{"id":7,"path":".github/workflows/ci.yml","event":"push",` +
			`"head_branch":"main","head_sha":"` + r + `","run_attempt":2}]}`,
		"GET /repos/bogdaniel/zenchron-engineering/actions/runs/7/attempts/1/jobs": `{"total_count":1,"jobs":[{"id":71,"name":"go","head_sha":"` + r + `","status":"completed","conclusion":"failure"}]}`,
		"GET /repos/bogdaniel/zenchron-engineering/actions/runs/7/attempts/2/jobs": `{"total_count":1,"jobs":[{"id":72,"name":"go","head_sha":"` + r + `","status":"completed","conclusion":"success"}]}`,
	}}
	observer := GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("s")}
	attempts, err := observer.RevisionEvidence(context.Background(), governedRepo(), r)
	if err != nil {
		t.Fatal(err)
	}
	got := EvaluateT2Evidence(DefaultTrustedRevisionPolicy(), r, attempts)
	if !got.Eligible || !got.Inconsistent || len(got.Attempts) != 2 {
		t.Fatalf("observation = %+v", got)
	}
	if _, err := observer.RevisionEvidence(context.Background(), governedRepo(), "main&x=1"); err == nil {
		t.Fatal("a non-commit revision reached the forge")
	}
}

// evidenceResponses is a complete, valid forge answer for revision r: one
// push run of ci.yml whose go job succeeded, produced by app.
func evidenceResponses(r string, app int) map[string]string {
	return map[string]string{
		"GET /repos/bogdaniel/zenchron-engineering/commits/" + r + "/check-runs": fmt.Sprintf(`{"total_count":1,"check_runs":[{"id":71,"app":{"id":%d}}]}`, app),
		"GET /repos/bogdaniel/zenchron-engineering/actions/runs": `{"total_count":1,"workflow_runs":[{"id":7,"path":".github/workflows/ci.yml","event":"push",` +
			`"head_branch":"main","head_sha":"` + r + `","run_attempt":1}]}`,
		"GET /repos/bogdaniel/zenchron-engineering/actions/runs/7/attempts/1/jobs": `{"total_count":1,"jobs":[{"id":71,"name":"go","head_sha":"` + r + `","status":"completed","conclusion":"success"}]}`,
	}
}

// The producing app comes from the job's own check run: a go job whose check
// run names another app is not T2 evidence.
func TestRevisionEvidenceTakesTheProducerFromTheCheckRun(t *testing.T) {
	r := shaOf('a')
	for app, eligible := range map[int]bool{15368: true, 99999: false} {
		observer := GitHubGovernanceObserver{HTTP: &fakeGitHubDoer{responses: evidenceResponses(r, app)}, Credential: testGovernanceCredential("s")}
		attempts, err := observer.RevisionEvidence(context.Background(), governedRepo(), r)
		if err != nil {
			t.Fatal(err)
		}
		if got := EvaluateT2Evidence(DefaultTrustedRevisionPolicy(), r, attempts); got.Eligible != eligible {
			t.Errorf("app %d: eligible = %t", app, got.Eligible)
		}
	}
}

// A listing is read completely or not at all: one that does not state its
// size, ends short of it, or exceeds the page ceiling fails the observation.
// Every other listing is valid, so only the guard under test can refuse.
func TestEvidenceListingsAreNeverTruncated(t *testing.T) {
	r := shaOf('a')
	checks := "GET /repos/bogdaniel/zenchron-engineering/commits/" + r + "/check-runs"
	for name, payload := range map[string]string{
		"no total_count":          `{"check_runs":[{"id":71,"app":{"id":15368}}]}`,
		"ends short of its total": `{"total_count":3,"check_runs":[]}`,
		"exceeds the ceiling":     `{"total_count":5000,"check_runs":[{"id":71,"app":{"id":15368}}]}`,
	} {
		responses := evidenceResponses(r, 15368)
		responses[checks] = payload
		observer := GitHubGovernanceObserver{HTTP: &fakeGitHubDoer{responses: responses}, Credential: testGovernanceCredential("s")}
		if _, err := observer.RevisionEvidence(context.Background(), governedRepo(), r); err == nil {
			t.Errorf("%s: a truncated listing was read as evidence", name)
		}
	}
}

// pagedDoer answers by path AND page, so a listing can be split across pages.
type pagedDoer struct{ pages map[string]string }

func (d pagedDoer) Do(r *http.Request) (*http.Response, error) {
	page := r.URL.Query().Get("page")
	body, ok := d.pages[r.URL.Path+"#"+page]
	if !ok {
		body, ok = d.pages[r.URL.Path]
	}
	if !ok {
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

// Page 2 is consumed, not merely checked for: the red re-run of the go job
// lives on page 2 of its attempt's jobs and the green one on page 1 of the
// next attempt, so eligibility and the inconsistency both depend on it.
func TestEvidenceOnALaterPageParticipates(t *testing.T) {
	r := shaOf('a')
	repo := "/repos/bogdaniel/zenchron-engineering"
	filler := make([]string, 100)
	for i := range filler {
		filler[i] = fmt.Sprintf(`{"id":%d,"name":"lint-%d","head_sha":"%s","status":"completed","conclusion":"success"}`, 1000+i, i, r)
	}
	doer := pagedDoer{pages: map[string]string{
		repo + "/commits/" + r + "/check-runs": `{"total_count":2,"check_runs":[{"id":71,"app":{"id":15368}},{"id":72,"app":{"id":15368}}]}`,
		repo + "/actions/runs": `{"total_count":1,"workflow_runs":[{"id":7,"path":".github/workflows/ci.yml","event":"push",` +
			`"head_branch":"main","head_sha":"` + r + `","run_attempt":2}]}`,
		repo + "/actions/runs/7/attempts/1/jobs#1": `{"total_count":101,"jobs":[` + strings.Join(filler, ",") + `]}`,
		repo + "/actions/runs/7/attempts/1/jobs#2": `{"total_count":101,"jobs":[{"id":71,"name":"go","head_sha":"` + r + `","status":"completed","conclusion":"failure"}]}`,
		repo + "/actions/runs/7/attempts/2/jobs#1": `{"total_count":1,"jobs":[{"id":72,"name":"go","head_sha":"` + r + `","status":"completed","conclusion":"success"}]}`,
	}}
	observer := GitHubGovernanceObserver{HTTP: doer, Credential: testGovernanceCredential("s")}
	attempts, err := observer.RevisionEvidence(context.Background(), governedRepo(), r)
	if err != nil {
		t.Fatal(err)
	}
	got := EvaluateT2Evidence(DefaultTrustedRevisionPolicy(), r, attempts)
	if len(got.Attempts) != 2 || !got.Inconsistent || !got.Eligible {
		t.Fatalf("observation = %d attempts, inconsistent=%t eligible=%t; the page-2 red attempt was not consumed",
			len(got.Attempts), got.Inconsistent, got.Eligible)
	}
}

// A persisted adopted-build/2 record must prove its own trusted_main: the
// monotonic floor will be read from it.
func TestAV2RecordMustProveItsTrustedMain(t *testing.T) {
	f := newAdoptedFixture(t)
	good, err := BuildAdoptedController(context.Background(), f.request(t), f.deps, BuilderRecord{Kind: ControllerUnattested})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*AdoptedBuildProvenance){
		"no policy": func(p *AdoptedBuildProvenance) { p.TrustedRevisionPolicy = nil },
		"another subject": func(p *AdoptedBuildProvenance) {
			// Consistent all the way down - evidence about another commit.
			p.TrustEvidence.Observation.Subject = shaOf('x')
			p.TrustEvidence.Observation.Attempts[0].HeadSHA = shaOf('x')
			p.TrustEvidence.Observation.Deciding.HeadSHA = shaOf('x')
		},
		"not eligible":      func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Eligible = false },
		"another tier":      func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Tier = "T1" },
		"failed deciding":   func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.Conclusion = "failure" },
		"no main_head":      func(p *AdoptedBuildProvenance) { p.MainHead = nil },
		"legacy kind in v2": func(p *AdoptedBuildProvenance) { p.TrustEvidence.Kind = TrustEvidenceLegacy },
		"another producer":  func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.IntegrationID = 99999 },
		// The cache claims success; the raw attempt it was derived from failed.
		"cached success over a failed attempt": func(p *AdoptedBuildProvenance) {
			p.TrustEvidence.Observation.Attempts[0].Conclusion = "failure"
		},
		// Policy and deciding attempt changed together: internally consistent,
		// and still not the frozen policy.
		"a policy weakened with its evidence": func(p *AdoptedBuildProvenance) {
			p.TrustedRevisionPolicy.IntegrationID = 99999
			p.TrustEvidence.Observation.Attempts[0].IntegrationID = 99999
			p.TrustEvidence.Observation.Deciding.IntegrationID = 99999
		},
		// Every cached field agrees with the raw attempt; the record is
		// internally honest and simply does not prove eligibility.
		"an honest record of a failed run": func(p *AdoptedBuildProvenance) {
			p.TrustEvidence.Observation.Attempts[0].Conclusion = "failure"
			p.TrustEvidence.Observation.Deciding.Conclusion = "failure"
			p.TrustEvidence.Observation.Eligible = false
		},
		// The raw attempts stay valid; only the cached deciding attempt lies.
		"a cached deciding workflow": func(p *AdoptedBuildProvenance) {
			p.TrustEvidence.Observation.Deciding.Workflow = ".github/workflows/assurance.yml"
		},
		"a cached deciding event":  func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.Event = "pull_request" },
		"a cached deciding branch": func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.Branch = "claude/feature" },
		"a cached deciding job":    func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.Job = "evidence" },
		"a cached deciding run":    func(p *AdoptedBuildProvenance) { p.TrustEvidence.Observation.Deciding.RunID++ },
		"an unpinned attempt smuggled in": func(p *AdoptedBuildProvenance) {
			extra := p.TrustEvidence.Observation.Attempts[0]
			extra.Workflow = ".github/workflows/assurance.yml"
			p.TrustEvidence.Observation.Attempts = append(p.TrustEvidence.Observation.Attempts, extra)
		},
		"a hidden inconsistency": func(p *AdoptedBuildProvenance) {
			red := p.TrustEvidence.Observation.Attempts[0]
			red.Attempt, red.Conclusion = 0, "failure"
			p.TrustEvidence.Observation.Attempts = append([]T2Attempt{red}, p.TrustEvidence.Observation.Attempts...)
		},
	} {
		var record AdoptedBuildProvenance
		raw, _ := json.Marshal(good)
		if err := json.Unmarshal(raw, &record); err != nil {
			t.Fatal(err)
		}
		mutate(&record)
		if _, err := record.Projected(); err == nil {
			t.Errorf("%s: an invalid adopted-build/2 record projected", name)
		}
	}
	if _, err := good.Projected(); err != nil {
		t.Fatalf("a valid record was refused: %v", err)
	}
}
