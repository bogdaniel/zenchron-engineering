package runtime

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// initTargetRepo makes one ordinary repository with a commit in it.
func initTargetRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit("", "init", "-q", dir); err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][]string{{"config", "user.name", "lab"}, {"config", "user.email", "lab@example.invalid"}} {
		if _, err := runGit(dir, pair...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("content\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "--no-gpg-sign", "-m", "base"); err != nil {
		t.Fatal(err)
	}
}

// classifyContext is the decision under test, asked the way the broker will ask
// it: resolve the repository an execution context reaches, then compare it to
// anchors the runtime established.
func classifyContext(t *testing.T, cwd, candidateDir, scratchDir string) GitTargetClass {
	t.Helper()
	identity, err := resolveRepoIdentity(cwd)
	if err != nil {
		return GitTargetExternalOrUnknown
	}
	return ClassifyGitTarget(identity, EstablishGitAuthorityAnchors(candidateDir, scratchDir))
}

// TestTheResourceMatrix is #257's claim, stated as a table.
//
// Authority follows the protected RESOURCE, so every row names an execution
// context and the resource it must resolve to. The classification is asserted
// DIRECTLY rather than inferred from whether some command happened to pass -
// inferring it is what let the original defect stay invisible through a whole
// production run.
//
// Nothing in this slice changes what is permitted. These are the answers the
// authority matrix will later be built on.
func TestTheResourceMatrix(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, candidate)

	// Inside the candidate: a subdirectory, a symlink onto it, a nested
	// repository, and a linked worktree sharing the common git directory.
	if err := os.MkdirAll(filepath.Join(candidate, "subdir"), 0o700); err != nil {
		t.Fatal(err)
	}
	symlinked := filepath.Join(root, "link-to-candidate")
	if err := os.Symlink(candidate, symlinked); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(candidate, "nested")
	initTargetRepo(t, nested)
	linkedWorktree := filepath.Join(root, "linked-worktree")
	if _, err := runGit(candidate, "worktree", "add", "-q", linkedWorktree, "-b", "wt"); err != nil {
		t.Fatalf("linked worktree fixture: %v", err)
	}

	// Under the attempt's scratch root: what a test's t.TempDir() produces
	// once TMPDIR names that root.
	scratchRepo := filepath.Join(scratch, "TestSomething1234567", "001")
	initTargetRepo(t, scratchRepo)

	// Outside both, on the same filesystem: an operator's unrelated repository.
	unrelated := filepath.Join(t.TempDir(), "unrelated")
	initTargetRepo(t, unrelated)

	for name, tc := range map[string]struct {
		cwd  string
		want GitTargetClass
	}{
		"candidate exact worktree":            {candidate, GitTargetCandidate},
		"candidate subdirectory":              {filepath.Join(candidate, "subdir"), GitTargetCandidate},
		"candidate through a symlink":         {symlinked, GitTargetCandidate},
		"nested repo physically under it":     {nested, GitTargetCandidate},
		"linked worktree, shared common dir":  {linkedWorktree, GitTargetCandidate},
		"repo under the attempt scratch root": {scratchRepo, GitTargetRuntimeScratch},
		"unrelated operator repository":       {unrelated, GitTargetExternalOrUnknown},
		"a directory that is no repository":   {scratch, GitTargetExternalOrUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			if got := classifyContext(t, tc.cwd, candidate, scratch); got != tc.want {
				t.Fatalf("target=%s, want %s", got, tc.want)
			}
		})
	}
}

// TestAmbiguityNeverResolvesToScratch pins the direction every failure must
// fall in.
//
// runtime_scratch is the only class this model will eventually permit mutation
// in, so it is the one answer that must never be reached by accident. Every
// unresolvable, moved or half-outside case has to land elsewhere.
func TestAmbiguityNeverResolvesToScratch(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	if err := os.MkdirAll(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, candidate)
	scratchRepo := filepath.Join(scratch, "fixture")
	initTargetRepo(t, scratchRepo)

	// NO SCRATCH GRANT IS NOT A DEFAULT. The same repository, with the runtime
	// having granted no scratch root, has no scratch authority.
	if got := classifyContext(t, scratchRepo, candidate, ""); got == GitTargetRuntimeScratch {
		t.Fatal("scratch authority was assumed without a runtime grant")
	}
	// A CONTEXT THAT IS NO REPOSITORY resolves to nothing.
	if got := classifyContext(t, t.TempDir(), candidate, scratch); got != GitTargetExternalOrUnknown {
		t.Fatalf("a non-repository context classified %s", got)
	}
	// AN ANCHOR THAT MOVED cannot answer, and the answer is not scratch.
	moved := filepath.Join(root, "scratch-moved")
	if err := os.Rename(scratch, moved); err != nil {
		t.Fatal(err)
	}
	if got := classifyContext(t, filepath.Join(moved, "fixture"), candidate, scratch); got == GitTargetRuntimeScratch {
		t.Fatal("a scratch root that had been replaced still conferred scratch authority")
	}
	if err := os.Rename(moved, scratch); err != nil {
		t.Fatal(err)
	}
	// A REPLACED ANCHOR at the same path is a different resource. Identity is
	// the inode, so an attacker-supplied directory standing where the grant was
	// does not inherit its authority.
	if err := os.Rename(scratch, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scratch, "fixture"), 0o700); err != nil {
		t.Fatal(err)
	}
	initTargetRepo(t, filepath.Join(scratch, "fixture"))
	anchors := EstablishGitAuthorityAnchors(candidate, moved)
	identity, err := resolveRepoIdentity(filepath.Join(scratch, "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	if got := ClassifyGitTarget(identity, anchors); got == GitTargetRuntimeScratch {
		t.Fatal("a repository outside the granted root inherited scratch authority by standing at its old path")
	}
}

// TestCandidateIdentityIsAPair records why the candidate test looks at the work
// tree as well as the git directory.
//
// The redirection flags that produce a mismatch are refused outright today, so
// this is not reachable through the broker - it is the property that makes the
// model correct if they are ever relaxed, and losing it silently later would be
// hard to notice.
func TestCandidateIdentityIsAPair(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	other := filepath.Join(root, "other")
	initTargetRepo(t, candidate)
	initTargetRepo(t, other)

	candidateIdentity, err := resolveRepoIdentity(candidate)
	if err != nil {
		t.Fatal(err)
	}
	otherIdentity, err := resolveRepoIdentity(other)
	if err != nil {
		t.Fatal(err)
	}
	anchors := EstablishGitAuthorityAnchors(candidate, "")

	// The shape that destroys candidate work: an unrelated git directory, the
	// candidate's work tree.
	mismatched := repoIdentity{
		CommonDir: otherIdentity.CommonDir, CommonID: otherIdentity.CommonID,
		WorkTree: candidateIdentity.WorkTree, WorkID: candidateIdentity.WorkID,
	}
	if got := ClassifyGitTarget(mismatched, anchors); got != GitTargetCandidate {
		t.Fatalf("a candidate work tree behind an unrelated git directory classified %s", got)
	}
	// And the ancestry direction: a work tree that CONTAINS the candidate
	// brings it into scope.
	ancestor, err := canonicalPath(root)
	if err != nil {
		t.Fatal(err)
	}
	ancestorID, err := identify(ancestor)
	if err != nil {
		t.Fatal(err)
	}
	containing := repoIdentity{
		CommonDir: otherIdentity.CommonDir, CommonID: otherIdentity.CommonID,
		WorkTree: ancestor, WorkID: ancestorID,
	}
	if got := ClassifyGitTarget(containing, anchors); got != GitTargetCandidate {
		t.Fatalf("a work tree containing the candidate classified %s", got)
	}
}

// lastRefusalTarget reads the resource the most recent refusal was about.
//
// It is asserted rather than inferred, so a future regression cannot produce
// the right refusal for the wrong reason - refusing a scratch command because
// the target resolution silently failed would otherwise look identical to
// refusing a candidate command correctly.
func lastRefusalTarget(t *testing.T, log string) GitTargetClass {
	t.Helper()
	refusals, err := ReadGitRefusals(log)
	if err != nil || len(refusals) == 0 {
		t.Fatalf("no refusal was recorded: %v", err)
	}
	return refusals[len(refusals)-1].Target
}

func dirtyBody(t *testing.T, dir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func makeDirty(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestTheAuthorityMatrix is slice 3's whole claim: resource scoping alone moves
// mutation authority exactly where intended, and nowhere else.
//
// The proof is SYMMETRIC - the same argv against each of the three resources -
// because a per-case assertion cannot distinguish "scoping works" from "this
// one command happened to be allowed". And it asserts BOTH halves of an
// authority claim:
//
//	permission works where intended  - the scratch command actually mutated
//	refusal preserves work           - the refused ones left their state intact
//
// Returning 0 is not evidence that a command ran, and returning 1 is not
// evidence that anything was protected.
//
// The scratch repositories carry their own committer identity already, so this
// exercises resource scoping only. The `-c user.name` / `-c user.email` question
// is deliberately absent; it belongs to the next slice.
func TestTheAuthorityMatrix(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	scratchForCommit := filepath.Join(scratch, "TestCommit1234567", "001")
	scratchForReset := filepath.Join(scratch, "TestReset7654321", "001")
	external := filepath.Join(t.TempDir(), "unrelated")
	for _, dir := range []string{candidate, scratchForCommit, scratchForReset, external} {
		initTargetRepo(t, dir)
	}
	log := filepath.Join(t.TempDir(), "refused.jsonl")

	const dirty = "uncommitted work that must survive a refusal\n"
	for _, dir := range []string{candidate, external, scratchForCommit, scratchForReset} {
		makeDirty(t, dir, dirty)
	}

	// ----- commit -------------------------------------------------------
	if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log, "commit", "-a", "-m", "x"); code == 0 {
		t.Fatal("a provider committed to the candidate")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetCandidate {
		t.Fatalf("candidate commit recorded target=%q", got)
	}
	if body := dirtyBody(t, candidate); body != dirty {
		t.Fatalf("the refused candidate commit changed the workspace: %q", body)
	}

	if code, _ := brokerGitFrom(t, external, candidate, scratch, log, "commit", "-a", "-m", "x"); code == 0 {
		t.Fatal("a provider committed to an unrelated repository")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetExternalOrUnknown {
		t.Fatalf("external commit recorded target=%q", got)
	}
	if body := dirtyBody(t, external); body != dirty {
		t.Fatalf("the refused external commit changed the repository: %q", body)
	}

	before, err := runGit(scratchForCommit, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := brokerGitFrom(t, scratchForCommit, candidate, scratch, log, "commit", "-a", "-m", "fixture"); code != 0 {
		t.Fatalf("a fixture could not commit to its own repository: %s", out)
	}
	// PERMISSION MUST HAVE DONE SOMETHING. A zero exit status proves the broker
	// did not refuse; only the repository proves the command ran.
	after, err := runGit(scratchForCommit, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(before)) == strings.TrimSpace(string(after)) {
		t.Fatal("the permitted scratch commit did not create a commit")
	}
	if status, err := runGit(scratchForCommit, "status", "--porcelain"); err != nil || strings.TrimSpace(string(status)) != "" {
		t.Fatalf("the permitted scratch commit left the work uncommitted: %q %v", status, err)
	}

	// ----- reset --hard -------------------------------------------------
	if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log, "reset", "--hard"); code == 0 {
		t.Fatal("a provider discarded candidate work")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetCandidate {
		t.Fatalf("candidate reset recorded target=%q", got)
	}
	if body := dirtyBody(t, candidate); body != dirty {
		t.Fatalf("the refused candidate reset discarded the work: %q", body)
	}

	if code, _ := brokerGitFrom(t, external, candidate, scratch, log, "reset", "--hard"); code == 0 {
		t.Fatal("a provider discarded work in an unrelated repository")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetExternalOrUnknown {
		t.Fatalf("external reset recorded target=%q", got)
	}
	if body := dirtyBody(t, external); body != dirty {
		t.Fatalf("the refused external reset discarded the work: %q", body)
	}

	if code, out := brokerGitFrom(t, scratchForReset, candidate, scratch, log, "reset", "--hard"); code != 0 {
		t.Fatalf("a fixture could not reset its own repository: %s", out)
	}
	if body := dirtyBody(t, scratchForReset); body == dirty {
		t.Fatal("the permitted scratch reset did not discard the fixture's work")
	}
}

// TestIdentityOverridesAreScopedToScratch is slice 4's claim, and the contrast
// is what keeps it from being read as "scratch permits configuration".
//
// A fixture that sets its own committer identity is doing ordinary repository
// setup - tests do it precisely so they do not depend on the machine's global
// Git identity - and forcing them to drop it would make the boundary dictate
// how a candidate writes its tests. That is backwards: the boundary should
// accommodate safe fixture behaviour.
//
// The same argv is put to all three resources, and then the NEGATIVE case pins
// that it is identity being scoped and not config authority generally.
func TestIdentityOverridesAreScopedToScratch(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	fixture := filepath.Join(scratch, "TestFixture1234567", "001")
	external := filepath.Join(t.TempDir(), "unrelated")
	for _, dir := range []string{candidate, fixture, external} {
		initTargetRepo(t, dir)
	}
	log := filepath.Join(t.TempDir(), "refused.jsonl")

	const dirty = "uncommitted work that must survive a refusal\n"
	for _, dir := range []string{candidate, external} {
		makeDirty(t, dir, dirty)
	}
	makeDirty(t, fixture, "fixture work\n")

	identity := []string{"-c", "user.email=fixture@example.invalid", "-c", "user.name=fixture",
		"commit", "-a", "-m", "fixture"}

	// SCRATCH: permitted, and it must actually have committed.
	before, err := runGit(fixture, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if code, out := brokerGitFrom(t, fixture, candidate, scratch, log, identity...); code != 0 {
		t.Fatalf("a fixture could not set its own committer identity: %s", out)
	}
	after, err := runGit(fixture, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(before)) == strings.TrimSpace(string(after)) {
		t.Fatal("the permitted identity commit did not create a commit")
	}

	// CANDIDATE: refused, work intact.
	if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log, identity...); code == 0 {
		t.Fatal("a provider chose the identity of candidate history")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetCandidate {
		t.Fatalf("candidate identity commit recorded target=%q", got)
	}
	if body := dirtyBody(t, candidate); body != dirty {
		t.Fatalf("the refused candidate commit changed the workspace: %q", body)
	}

	// EXTERNAL: refused, work intact.
	if code, _ := brokerGitFrom(t, external, candidate, scratch, log, identity...); code == 0 {
		t.Fatal("a provider committed to an unrelated repository")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetExternalOrUnknown {
		t.Fatalf("external identity commit recorded target=%q", got)
	}
	if body := dirtyBody(t, external); body != dirty {
		t.Fatalf("the refused external commit changed the repository: %q", body)
	}

	// THE NEGATIVE CASE. Scratch scope admits an identity, not a capability: a
	// key that can execute a program, name a path or present a credential is
	// refused there exactly as everywhere else. One of each family is enough -
	// the rest are covered by TestConfigOverridesThatRedirectAreStillRefused,
	// which is target-independent by construction.
	for name, key := range map[string]string{
		"a hooks directory":    "core.hooksPath=/tmp/hooks",
		"a pager program":      "core.pager=/tmp/evil",
		"a credential helper":  "credential.helper=/tmp/evil",
		"a signing program":    "gpg.program=/tmp/evil",
		"a filesystem monitor": "core.fsmonitor=/tmp/evil",
	} {
		t.Run(name, func(t *testing.T) {
			code, out := brokerGitFrom(t, fixture, candidate, scratch, log,
				"-c", key, "commit", "--allow-empty", "-m", "x")
			if code == 0 {
				t.Fatalf("%s was admitted by scratch scope", key)
			}
			if !strings.Contains(out, "configuration override") {
				t.Fatalf("%s was refused for the wrong reason: %s", key, out)
			}
		})
	}

	// A READ carrying an identity override is still a question about whose
	// identity, so the candidate refuses it even though the verb is permitted.
	if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log,
		"-c", "user.email=x@example.invalid", "status", "--porcelain"); code == 0 {
		t.Fatal("an identity override reached the candidate behind a permitted verb")
	}
}

// TestTheExecutionContextCannotBeLaundered is the review's three blockers, each
// of which was demonstrated to destroy candidate work before the repair.
//
// They are one defect wearing three costumes: the decision and the execution
// consulted different information about which repository was involved. A
// boundary that classifies against one repository and acts on another has not
// been narrowed, it has been bypassed.
func TestTheExecutionContextCannotBeLaundered(t *testing.T) {
	requireGitFixture(t)
	const precious = "uncommitted candidate work\n"

	newLab := func(t *testing.T) (candidate, scratch, fixture, log string) {
		t.Helper()
		root := t.TempDir()
		candidate = filepath.Join(root, "candidate")
		scratch = filepath.Join(root, "scratch")
		fixture = filepath.Join(scratch, "TestFixture1234567", "001")
		initTargetRepo(t, candidate)
		initTargetRepo(t, fixture)
		makeDirty(t, candidate, precious)
		return candidate, scratch, fixture, filepath.Join(root, "refused.jsonl")
	}

	// A `-C` HIDDEN BEHIND A VALUE-TAKING GLOBAL. The context scanners stopped
	// at `user.email=...` because it does not begin with a dash, so the `-C`
	// was invisible to the pin, survived into the argv, and Git applied it.
	// Classified against scratch, executed against the candidate.
	t.Run("a -C behind another global", func(t *testing.T) {
		candidate, scratch, fixture, log := newLab(t)
		code, _ := brokerGitFrom(t, fixture, candidate, scratch, log,
			"-c", "user.email=x@example.invalid", "-C", candidate, "reset", "--hard")
		if code == 0 {
			t.Fatal("a redirected destructive command was permitted")
		}
		if body := dirtyBody(t, candidate); body != precious {
			t.Fatalf("candidate work was discarded: %q", body)
		}
	})

	// THE SAME SHAPE WITH THE REDIRECT IN THE ENVIRONMENT. This began as a
	// consistency bug - resolution sanitized the environment and could not see
	// GIT_WORK_TREE, execution inherited it and could - and consistency alone
	// was not the right repair. Agreeing about a redirected repository is still
	// agreement about the wrong one, so GIT_WORK_TREE now takes the GIT_DIR
	// route: removed from the canonical environment for both paths.
	//
	// So what is asserted is not refusal. Acting on the fixture is legitimate,
	// and the command must still DO something - a boundary that silently turned
	// every redirected command into a no-op would pass a candidate-untouched
	// check while being useless.
	t.Run("GIT_WORK_TREE from the environment", func(t *testing.T) {
		candidate, scratch, fixture, log := newLab(t)
		makeDirty(t, fixture, "fixture work\n")
		t.Setenv("GIT_WORK_TREE", candidate)
		if code, out := brokerGitFrom(t, fixture, candidate, scratch, log, "checkout", "--", "."); code != 0 {
			t.Fatalf("a scratch checkout was refused: %d %s", code, out)
		}
		if body := dirtyBody(t, fixture); body == "fixture work\n" {
			t.Fatal("the permitted checkout did not run")
		}
		if body := dirtyBody(t, candidate); body != precious {
			t.Fatalf("candidate work was discarded: %q", body)
		}
	})

	// GIT_DIR takes the OTHER route to the same guarantee: it is removed from
	// the canonical environment outright, for the decision and the execution
	// alike, because that variable is the brokered sentinel's own. So it
	// redirects neither, and the command acts on the directory it was run in.
	// What matters here is not that it is refused - acting on the fixture is
	// legitimate - but that it cannot reach the candidate.
	t.Run("GIT_DIR from the environment", func(t *testing.T) {
		candidate, scratch, fixture, log := newLab(t)
		t.Setenv("GIT_DIR", filepath.Join(candidate, ".git"))
		brokerGitFrom(t, fixture, candidate, scratch, log, "checkout", "--", ".")
		if body := dirtyBody(t, candidate); body != precious {
			t.Fatalf("candidate work was discarded: %q", body)
		}
	})

	// AN ALIAS THAT INTRODUCES AN IDENTITY OVERRIDE. Git honours an alias
	// beginning with `-c`, so the override appears during expansion - after a
	// check reading the original argv has already decided there was none.
	t.Run("an alias introducing an identity override", func(t *testing.T) {
		candidate, scratch, _, log := newLab(t)
		// Written with real Git rather than the trusted runner, which refuses
		// to put an [alias] section into a candidate at all. A provider's own
		// `git config` is not so constrained, which is why the resolver has to
		// answer for one.
		if _, err := brokerGitOutput(candidate, "config", "alias.sneak",
			"-c user.email=alias@example.invalid commit --allow-empty -m viaalias"); err != nil {
			t.Fatal(err)
		}
		// Read with real Git for the same reason the alias was written with it:
		// the trusted runner refuses a repository carrying an [alias] section
		// outright, which is its own boundary and not the one under test here.
		before, err := brokerGitOutput(candidate, "log", "-1", "--format=%H")
		if err != nil {
			t.Fatal(err)
		}
		if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log, "sneak"); code == 0 {
			t.Fatal("an alias-introduced commit was permitted against the candidate")
		}
		after, err := brokerGitOutput(candidate, "log", "-1", "--format=%H")
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(before)) != strings.TrimSpace(string(after)) {
			t.Fatal("the alias moved candidate history")
		}
	})

	// AND THE REPAIR DOES NOT COST THE LEGITIMATE FORM: `-C` into the attempt's
	// own scratch still works, with the other globals still in place.
	t.Run("a legitimate -C into scratch still works", func(t *testing.T) {
		candidate, scratch, fixture, log := newLab(t)
		makeDirty(t, fixture, "fixture work\n")
		if code, out := brokerGitFrom(t, candidate, candidate, scratch, log,
			"-c", "user.email=x@example.invalid", "-C", fixture, "reset", "--hard"); code != 0 {
			t.Fatalf("a scratch reset reached through -C was refused: %s", out)
		}
		if body := dirtyBody(t, fixture); body == "fixture work\n" {
			t.Fatal("the permitted scratch reset did not run")
		}
		if body := dirtyBody(t, candidate); body != precious {
			t.Fatalf("the scratch reset reached the candidate: %q", body)
		}
	})
}

// TestTheContextScannersAgree pins the property the review found violated,
// rather than the shape of any one past bug.
//
// Two functions read the same argv for the same option: one decides where the
// command will be classified, the other decides what Git actually receives. If
// they disagree about a single -C, the command is classified against one
// repository and executed against another. That is not a narrower boundary, it
// is a bypass, so the agreement is asserted directly.
func TestTheContextScannersAgree(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no -C at all":                      {[]string{"status"}, "/base"},
		"a bare -C":                         {[]string{"-C", "/x", "status"}, "/x"},
		"-C behind an inert -c":             {[]string{"-c", "core.quotepath=false", "-C", "/x", "status"}, "/x"},
		"-C behind a deferred -c":           {[]string{"-c", "user.email=a@b", "-C", "/x", "reset", "--hard"}, "/x"},
		"-C behind two -c options":          {[]string{"-c", "user.name=n", "-c", "user.email=a@b", "-C", "/x", "status"}, "/x"},
		"two -C options compose":            {[]string{"-C", "/x", "-C", "sub", "status"}, "/x/sub"},
		"-C after a valueless global":       {[]string{"--no-pager", "-C", "/x", "status"}, "/x"},
		"a pathspec that looks like a flag": {[]string{"-C", "/x", "reset", "--", "-C"}, "/x"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := effectiveCwd("/base", tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("the pin resolved %q and the argv says %q", got, tc.want)
			}
			// NOTHING THE PIN ALREADY APPLIED MAY REACH GIT. A surviving -C is
			// applied a second time, landing somewhere nobody classified.
			executed := withoutDirectoryGlobals(tc.args)
			for i, arg := range executed {
				if arg == "--" {
					break
				}
				if arg == "-C" {
					t.Fatalf("a -C survived into the executed argv at %d: %v", i, executed)
				}
			}
			// And the verb the classifier answered for must be the verb Git runs.
			_, pinnedVerb, _ := splitGitCommand(executed)
			_, originalVerb, _ := splitGitCommand(tc.args)
			if pinnedVerb != originalVerb {
				t.Fatalf("the executed verb is %q and the classified verb was %q", pinnedVerb, originalVerb)
			}
		})
	}
}

// TestAnAliasCannotSlipAnIdentityPastAPermittedVerb is the case an earlier
// version of the alias regression missed.
//
// `commit` is runtime-owned whatever identity it carries, so an alias expanding
// to one is refused either way and proves nothing about WHERE the deferred keys
// were read from. An alias expanding to a PERMITTED verb is the sharp case: a
// check reading the original argv finds no override there, and the identity
// reaches the candidate repository unexamined.
func TestAnAliasCannotSlipAnIdentityPastAPermittedVerb(t *testing.T) {
	requireGitFixture(t)
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	scratch := filepath.Join(root, "scratch")
	initTargetRepo(t, candidate)
	log := filepath.Join(root, "refused.jsonl")

	if _, err := brokerGitOutput(candidate, "config", "alias.peek",
		"-c user.email=alias@example.invalid status --porcelain"); err != nil {
		t.Fatal(err)
	}
	if code, _ := brokerGitFrom(t, candidate, candidate, scratch, log, "peek"); code == 0 {
		t.Fatal("an alias slipped an identity override past a permitted verb")
	}
	if got := lastRefusalTarget(t, log); got != GitTargetCandidate {
		t.Fatalf("the refusal recorded target=%q", got)
	}
}

// TestAGrantedAuthorityCannotWriteOutsideItsResource is the law the environment
// was still able to break after resolution and execution were made consistent.
//
// Consistency answered "which repository is this command against?". It did not
// answer "and where will the writes land?", because several Git variables move
// the writable state without moving the repository's identity at all:
// GIT_INDEX_FILE names the index file, GIT_OBJECT_DIRECTORY names the object
// store, GIT_ALTERNATE_OBJECT_DIRECTORIES adds another. A command classified as
// runtime_scratch_repo - and legitimately permitted there - could carry any of
// them pointed at the candidate.
//
// Each case runs a PERMITTED scratch command, so nothing here depends on a
// refusal. The grant is real; what must be true is that it stops at the
// resource it was granted over.
func TestAGrantedAuthorityCannotWriteOutsideItsResource(t *testing.T) {
	requireGitFixture(t)
	const precious = "uncommitted candidate work\n"

	for name, redirect := range map[string]func(candidate string) (string, string){
		"GIT_INDEX_FILE at the candidate index": func(c string) (string, string) {
			return "GIT_INDEX_FILE", filepath.Join(c, ".git", "index")
		},
		"GIT_OBJECT_DIRECTORY at the candidate object store": func(c string) (string, string) {
			return "GIT_OBJECT_DIRECTORY", filepath.Join(c, ".git", "objects")
		},
		"GIT_COMMON_DIR at the candidate": func(c string) (string, string) {
			return "GIT_COMMON_DIR", filepath.Join(c, ".git")
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			candidate := filepath.Join(root, "candidate")
			scratch := filepath.Join(root, "scratch")
			fixture := filepath.Join(scratch, "TestFixture1234567", "001")
			initTargetRepo(t, candidate)
			initTargetRepo(t, fixture)
			makeDirty(t, candidate, precious)
			log := filepath.Join(root, "refused.jsonl")

			candidateIndexBefore := indexDigest(t, candidate)
			candidateHeadBefore := repoHead(t, candidate)
			candidateObjectsBefore := objectCount(t, candidate)

			key, value := redirect(candidate)
			t.Setenv(key, value)

			// A permitted, runtime-owned commit in the attempt's own scratch.
			if code, out := brokerGitFrom(t, fixture, candidate, scratch, log,
				"-c", "user.email=r@runtime", "-c", "user.name=runtime",
				"commit", "--allow-empty", "-m", "scratch work"); code != 0 {
				t.Fatalf("a runtime-owned scratch commit was refused: %d %s", code, out)
			}
			// The grant was real.
			if repoHead(t, fixture) == "" {
				t.Fatal("the permitted scratch commit did not run")
			}
			// And it stopped at the scratch repository.
			if got := indexDigest(t, candidate); got != candidateIndexBefore {
				t.Fatalf("%s moved a write into the candidate index", key)
			}
			if got := repoHead(t, candidate); got != candidateHeadBefore {
				t.Fatalf("%s moved candidate history to %s", key, got)
			}
			// A loose object written into the candidate's store moves neither
			// the index nor HEAD, so it has to be counted to be seen.
			if got := objectCount(t, candidate); got != candidateObjectsBefore {
				t.Fatalf("%s wrote %d objects into the candidate store", key, got-candidateObjectsBefore)
			}
			if body := dirtyBody(t, candidate); body != precious {
				t.Fatalf("%s discarded candidate work: %q", key, body)
			}
		})
	}
}

// TestTheEnvironmentIsNotASecondArgv covers the two powers that bypass argv
// classification entirely rather than redirecting it.
//
// GIT_CONFIG_COUNT with GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n is exactly `-c` in
// environment form, so every rule that reads configuration overrides out of the
// argv - the bounded allow/deny classification, and the narrow
// user.name/user.email scoping - is blind to it. GIT_AUTHOR_* and
// GIT_COMMITTER_* name a committer the same way, and never appear in argv at
// all.
func TestTheEnvironmentIsNotASecondArgv(t *testing.T) {
	requireGitFixture(t)

	setup := func(t *testing.T) (candidate, scratch, fixture, log string) {
		t.Helper()
		root := t.TempDir()
		candidate = filepath.Join(root, "candidate")
		scratch = filepath.Join(root, "scratch")
		fixture = filepath.Join(scratch, "TestFixture1234567", "001")
		initTargetRepo(t, candidate)
		initTargetRepo(t, fixture)
		return candidate, scratch, fixture, filepath.Join(root, "refused.jsonl")
	}

	// `-c user.email=` against the candidate is refused by the identity rule.
	// The environment spelling must not be the way around it.
	t.Run("GIT_CONFIG_COUNT cannot inject an identity", func(t *testing.T) {
		candidate, scratch, fixture, log := setup(t)
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "user.email")
		t.Setenv("GIT_CONFIG_VALUE_0", "injected@example.invalid")
		// Deliberately NO `-c` identity here. Git ranks command-line `-c` above
		// GIT_CONFIG_*, so passing one would mean the injected value could never
		// have won and the case would prove nothing. The fixture's own repo-local
		// identity is what the environment has to be unable to displace.
		if code, out := brokerGitFrom(t, fixture, candidate, scratch, log,
			"commit", "--allow-empty", "-m", "scratch work"); code != 0 {
			t.Fatalf("a runtime-owned scratch commit was refused: %d %s", code, out)
		}
		if got := commitAuthor(t, fixture); got != "lab@example.invalid" {
			t.Fatalf("the environment named the committer: %s", got)
		}
	})

	// core.hooksPath is the sharper form: it does not change what Git writes,
	// it makes Git RUN something of the provider's choosing on a runtime-owned
	// commit.
	t.Run("GIT_CONFIG_COUNT cannot point Git at a hook directory", func(t *testing.T) {
		candidate, scratch, fixture, log := setup(t)
		hooks := filepath.Join(t.TempDir(), "hooks")
		if err := os.MkdirAll(hooks, 0o755); err != nil {
			t.Fatal(err)
		}
		witness := filepath.Join(t.TempDir(), "hook-ran")
		hook := "#!/bin/sh\necho ran > " + witness + "\n"
		if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte(hook), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GIT_CONFIG_COUNT", "1")
		t.Setenv("GIT_CONFIG_KEY_0", "core.hooksPath")
		t.Setenv("GIT_CONFIG_VALUE_0", hooks)

		if code, out := brokerGitFrom(t, fixture, candidate, scratch, log,
			"-c", "user.email=r@runtime", "-c", "user.name=runtime",
			"commit", "--allow-empty", "-m", "scratch work"); code != 0 {
			t.Fatalf("a runtime-owned scratch commit was refused: %d %s", code, out)
		}
		if _, err := os.Stat(witness); err == nil {
			t.Fatal("a provider hook ran on a runtime-owned commit")
		}
	})

	t.Run("GIT_AUTHOR_EMAIL cannot name the author", func(t *testing.T) {
		candidate, scratch, fixture, log := setup(t)
		t.Setenv("GIT_AUTHOR_EMAIL", "injected@example.invalid")
		t.Setenv("GIT_AUTHOR_NAME", "injected")
		t.Setenv("GIT_COMMITTER_EMAIL", "injected@example.invalid")
		t.Setenv("GIT_COMMITTER_NAME", "injected")
		if code, out := brokerGitFrom(t, fixture, candidate, scratch, log,
			"-c", "user.email=r@runtime", "-c", "user.name=runtime",
			"commit", "--allow-empty", "-m", "scratch work"); code != 0 {
			t.Fatalf("a runtime-owned scratch commit was refused: %d %s", code, out)
		}
		if got := commitAuthor(t, fixture); got != "r@runtime" {
			t.Fatalf("the environment named the author: %s", got)
		}
	})
}

// indexDigest fingerprints a repository's index, reporting a missing one as a
// distinct value rather than an error - a redirect that CREATES an index where
// there was none is as much a write outside the resource as one that edits it.
func indexDigest(t *testing.T, dir string) string {
	t.Helper()
	digest, err := fileDigest(filepath.Join(dir, ".git", "index"))
	if err != nil {
		return "absent"
	}
	return digest
}

// objectCount counts loose objects, which is how a redirected object store
// shows up: the write lands, but nothing about the repository's identity,
// index, or HEAD reflects it.
func objectCount(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	root := filepath.Join(dir, ".git", "objects")
	if err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

func repoHead(t *testing.T, dir string) string {
	t.Helper()
	out, err := brokerGitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func commitAuthor(t *testing.T, dir string) string {
	t.Helper()
	out, err := brokerGitOutput(dir, "log", "-1", "--format=%ae")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}
