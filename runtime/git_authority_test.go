package runtime

// #241: a provider may mutate candidate work and may not destructively discard it.
//
// Every destructive case here is proved TWICE, against a real Git repository:
// once through the broker, which must refuse and leave the bytes untouched, and
// once by executing the identical argv directly, which must actually destroy
// the fixture. The second half is the part that makes the first half mean
// something - a guard test that never demonstrates the damage it prevents is
// asserting that a command it refused would have mattered, which is exactly the
// vacuity this repository has been bitten by before.

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// gitAuthorityFixture is a real repository with one committed file, shaped like
// a runtime-owned candidate workspace: a baseline commit the provider then
// works on top of.
func gitAuthorityFixture(t *testing.T) (dir, refusalLog string) {
	t.Helper()
	requireGitFixture(t)
	root := t.TempDir()
	dir = filepath.Join(root, "candidate")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// baseline\n")
	runner := GitRunner{Dir: dir}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "runtime@zenchron.local"},
		{"config", "user.name", "Zenchron Runtime"},
		{"add", "-A"},
		{"commit", "--no-gpg-sign", "--quiet", "-m", "baseline"},
	} {
		if _, err := runner.run(args...); err != nil {
			t.Fatalf("git %s: %v", strings.Join(args, " "), err)
		}
	}
	return dir, filepath.Join(root, "guard", gitRefusalLogName)
}

func requireGitFixture(t *testing.T) {
	t.Helper()
	if _, err := gitBinary(); err != nil {
		t.Skip("git unavailable on the trusted search path")
	}
}

func writeCandidateFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func candidateFileBody(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// brokerGit runs one provider argv through the enforcement point and returns
// the exit status plus exactly what the provider saw on stderr - which is the
// diagnostic #241 acceptance 5 is about.
func brokerGit(t *testing.T, dir, refusalLog string, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code, err := BrokerGitCommand(dir, refusalLog, args, &stdout, &stderr)
	if err != nil {
		t.Fatalf("broker %v: %v", args, err)
	}
	return code, stderr.String()
}

// unguardedGit is the MUTATION: the identical argv with the guard removed. It
// is what the provider would have run before #241, and what it still runs if
// the enforcement point is bypassed or deleted.
func unguardedGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	if _, err := execRealGit(dir, args, io.Discard, io.Discard); err != nil {
		t.Fatalf("unguarded git %v: %v", args, err)
	}
}

// ---------------------------------------------------------------------------
// A. A modified tracked file
// ---------------------------------------------------------------------------

// TestADestructiveCheckoutCannotDiscardATrackedModification is #241 acceptance
// 1-5, and it is the exact command observed in the dogfood.
func TestADestructiveCheckoutCannotDiscardATrackedModification(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// the expensive uncommitted implementation\nfunc Added() {}\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	code, diagnostic := brokerGit(t, dir, refusalLog, "checkout", "--", "implementation.go")
	if code == 0 {
		t.Fatal("the broker reported success for a destructive checkout")
	}
	// THE BYTES ARE UNTOUCHED. Not restored, not re-derived: never written.
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the provider's uncommitted work was modified:\n%q", got)
	}

	// The refusal is DURABLE EVIDENCE and it names what was at stake.
	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 1 {
		t.Fatalf("refusals recorded = %d, want exactly 1: %#v", len(refusals), refusals)
	}
	if !strings.Contains(refusals[0].Operation, "checkout") {
		t.Fatalf("the record does not name the operation: %q", refusals[0].Operation)
	}
	if refusals[0].DirtyCount != 1 || len(refusals[0].DirtyPaths) != 1 ||
		refusals[0].DirtyPaths[0] != "implementation.go" {
		t.Fatalf("the record does not name the dirty candidate work: %#v", refusals[0])
	}
	// And the diagnostic THE PROVIDER ACTUALLY RECEIVED is actionable rather
	// than a bare refusal: it says what was refused, what would have been lost,
	// and what to do instead. This is asserted on the real stderr bytes rather
	// than on a reconstruction, because a reconstruction would pass even if
	// nothing reached the worker.
	for _, required := range []string{
		"destructive Git refused", "git checkout", "implementation.go",
		"Zenchron owns candidate commits", "Read-only Git",
	} {
		if !strings.Contains(diagnostic, required) {
			t.Fatalf("the provider diagnostic omits %q: %s", required, diagnostic)
		}
	}

	// MUTATION: the same argv with no guard in front of it. If this does not
	// destroy the fixture, the assertion above proves nothing.
	unguardedGit(t, dir, "checkout", "--", "implementation.go")
	if got := candidateFileBody(t, dir, "implementation.go"); got == work {
		t.Fatal("unguarded git checkout did NOT discard the work, so this test cannot prove the guard does anything")
	}
}

// ---------------------------------------------------------------------------
// B. reset --hard
// ---------------------------------------------------------------------------

// TestResetHardCannotDiscardDirtyCandidateWork is acceptance 7, across the
// spellings a worker actually types - including the ones that prefix a global
// option after a bare invocation has just been refused.
func TestResetHardCannotDiscardDirtyCandidateWork(t *testing.T) {
	for name, args := range map[string][]string{
		"bare":             {"reset", "--hard"},
		"explicit head":    {"reset", "--hard", "HEAD"},
		"with -C":          {"-C", ".", "reset", "--hard"},
		"with --git-dir":   {"--git-dir=.git", "reset", "--hard"},
		"merge form":       {"reset", "--merge"},
		"restore":          {"restore", "."},
		"restore worktree": {"restore", "--worktree", "implementation.go"},
		"switch force":     {"switch", "--discard-changes", "-"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, refusalLog := gitAuthorityFixture(t)
			const work = "package candidate\n\n// work that must survive\n"
			writeCandidateFile(t, dir, "implementation.go", work)

			if code, _ := brokerGit(t, dir, refusalLog, args...); code == 0 {
				t.Fatalf("the broker reported success for %v", args)
			}
			if got := candidateFileBody(t, dir, "implementation.go"); got != work {
				t.Fatalf("%v discarded the provider's work:\n%q", args, got)
			}
			if refusals, err := ReadGitRefusals(refusalLog); err != nil || len(refusals) != 1 {
				t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
			}
		})
	}

	// MUTATION, once, on its own fixture: reset --hard really does erase it.
	dir, _ := gitAuthorityFixture(t)
	const work = "package candidate\n\n// work that must survive\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	unguardedGit(t, dir, "reset", "--hard")
	if got := candidateFileBody(t, dir, "implementation.go"); got == work {
		t.Fatal("unguarded git reset --hard did NOT discard the work")
	}
}

// TestASoftResetIsOrdinaryAndStillRuns keeps the taxonomy honest in the other
// direction. --soft and --mixed do not touch the working tree, so refusing them
// would be the runtime inventing a restriction #241 did not ask for.
func TestASoftResetIsOrdinaryAndStillRuns(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// dirty\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	for _, args := range [][]string{{"reset", "--soft", "HEAD"}, {"reset", "HEAD"}} {
		if code, _ := brokerGit(t, dir, refusalLog, args...); code != 0 {
			t.Fatalf("%v was refused; it does not touch the working tree", args)
		}
	}
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("an index-only reset changed the working tree:\n%q", got)
	}
	if refusals, _ := ReadGitRefusals(refusalLog); len(refusals) != 0 {
		t.Fatalf("an ordinary command was recorded as refused: %#v", refusals)
	}
}

// ---------------------------------------------------------------------------
// C. Untracked candidate files
// ---------------------------------------------------------------------------

// TestGitCleanCannotDeleteUntrackedCandidateWork is acceptance 8.
//
// An untracked file is the MOST expensive state in the workspace, not the least:
// nothing has committed it, so it exists only because the provider just reasoned
// it into existence. `git clean -fd` is the command that deletes exactly those.
func TestGitCleanCannotDeleteUntrackedCandidateWork(t *testing.T) {
	for name, args := range map[string][]string{
		"clean -fd":               {"clean", "-fd"},
		"clean -f":                {"clean", "-f"},
		"clean -fdx":              {"clean", "-fdx"},
		"clean --force with dirs": {"clean", "--force", "-d"},
	} {
		t.Run(name, func(t *testing.T) {
			dir, refusalLog := gitAuthorityFixture(t)
			const added = "package candidate\n\n// a brand new file nothing has committed\n"
			writeCandidateFile(t, dir, "added.go", added)
			if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o700); err != nil {
				t.Fatal(err)
			}
			writeCandidateFile(t, dir, filepath.Join("pkg", "nested.go"), "package pkg\n")

			if code, _ := brokerGit(t, dir, refusalLog, args...); code == 0 {
				t.Fatalf("the broker reported success for %v", args)
			}
			if got := candidateFileBody(t, dir, "added.go"); got != added {
				t.Fatalf("%v discarded the new file:\n%q", args, got)
			}
			if _, err := os.Stat(filepath.Join(dir, "pkg", "nested.go")); err != nil {
				t.Fatalf("%v removed a nested untracked file: %v", args, err)
			}
			// The record names BOTH untracked paths: --untracked-files=all is
			// what makes a whole new directory visible as candidate work rather
			// than as one directory entry.
			refusals, err := ReadGitRefusals(refusalLog)
			if err != nil || len(refusals) != 1 {
				t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
			}
			if refusals[0].DirtyCount != 2 {
				t.Fatalf("dirty count = %d, want both untracked candidate files: %#v",
					refusals[0].DirtyCount, refusals[0].DirtyPaths)
			}
		})
	}

	// MUTATION: clean -fd really does delete them.
	dir, _ := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "added.go", "package candidate\n")
	unguardedGit(t, dir, "clean", "-fd")
	if _, err := os.Stat(filepath.Join(dir, "added.go")); err == nil {
		t.Fatal("unguarded git clean -fd did NOT delete the untracked file")
	}
}

// TestAnIgnoredPathIsNeitherCandidateWorkNorGroundsToDestroy is the .gitignore
// rule #241 states: being ignorable is not authority to destroy, and it is not
// a claim on candidate work either.
func TestAnIgnoredPathIsNeitherCandidateWorkNorGroundsToDestroy(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, ".gitignore", "build/\n")
	if err := os.MkdirAll(filepath.Join(dir, "build"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCandidateFile(t, dir, filepath.Join("build", "artifact.bin"), "generated\n")

	// The ignored artifact is not reported as candidate work.
	dirty, err := CandidateDirtyPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range dirty {
		if strings.HasPrefix(path, "build/") {
			t.Fatalf("an ignored path was reported as candidate work: %v", dirty)
		}
	}
	// And the destructive command is refused anyway, so "git would have ignored
	// it" never becomes a reason the file was destroyed.
	if code, _ := brokerGit(t, dir, refusalLog, "clean", "-fdx"); code == 0 {
		t.Fatal("clean -fdx was permitted because the target was ignorable")
	}
	if _, err := os.Stat(filepath.Join(dir, "build", "artifact.bin")); err != nil {
		t.Fatalf("an ignored runtime-attributable file was destroyed: %v", err)
	}
}

// ---------------------------------------------------------------------------
// D. Safe commands keep working
// ---------------------------------------------------------------------------

// TestOrdinaryGitStillWorksThroughTheBroker is acceptance 9. A guard that broke
// the provider's ability to read the repository would have replaced one
// expensive failure mode with another.
func TestOrdinaryGitStillWorksThroughTheBroker(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	writeCandidateFile(t, dir, "added.go", "package candidate\n")

	for _, args := range [][]string{
		{"status", "--porcelain"},
		{"diff"},
		{"diff", "--stat"},
		{"log", "--oneline", "-1"},
		{"show", "--stat", "HEAD"},
		{"rev-parse", "HEAD"},
		{"ls-files"},
		{"add", "-A"},
		{"stash", "list"},
		{"branch", "--show-current"},
	} {
		if code, _ := brokerGit(t, dir, refusalLog, args...); code != 0 {
			t.Fatalf("ordinary git %v exited %d through the broker", args, code)
		}
	}
	if refusals, _ := ReadGitRefusals(refusalLog); len(refusals) != 0 {
		t.Fatalf("ordinary commands were recorded as refused: %#v", refusals)
	}
}

// ---------------------------------------------------------------------------
// The taxonomy, stated
// ---------------------------------------------------------------------------

// TestGitDiscardTaxonomy is the table a reviewer can check without running a
// repository. It is where the conservative choices are visible.
func TestGitDiscardTaxonomy(t *testing.T) {
	for name, tc := range map[string]struct {
		args []string
		want GitOperationClass
	}{
		"checkout pathspec":   {[]string{"checkout", "--", "a.go"}, GitOperationDiscard},
		"checkout dot":        {[]string{"checkout", "."}, GitOperationDiscard},
		"checkout force":      {[]string{"checkout", "-f"}, GitOperationDiscard},
		"checkout branch":     {[]string{"checkout", "main"}, GitOperationDiscard},
		"checkout create":     {[]string{"checkout", "-b", "work"}, GitOperationDiscard},
		"switch":              {[]string{"switch", "main"}, GitOperationDiscard},
		"restore":             {[]string{"restore", "a.go"}, GitOperationDiscard},
		"restore staged only": {[]string{"restore", "--staged", "a.go"}, GitOperationDiscard},
		"reset hard":          {[]string{"reset", "--hard"}, GitOperationDiscard},
		"reset merge":         {[]string{"reset", "--merge"}, GitOperationDiscard},
		"reset soft":          {[]string{"reset", "--soft", "HEAD~1"}, GitOperationPermitted},
		"reset mixed":         {[]string{"reset"}, GitOperationPermitted},
		"reset keep":          {[]string{"reset", "--keep", "HEAD"}, GitOperationPermitted},
		"clean":               {[]string{"clean", "-fd"}, GitOperationDiscard},
		"clean dry run":       {[]string{"clean", "-n"}, GitOperationDiscard},
		"stash bare":          {[]string{"stash"}, GitOperationDiscard},
		"stash push":          {[]string{"stash", "push"}, GitOperationDiscard},
		"stash save":          {[]string{"stash", "save", "wip"}, GitOperationDiscard},
		"stash clear":         {[]string{"stash", "clear"}, GitOperationDiscard},
		"stash drop":          {[]string{"stash", "drop"}, GitOperationDiscard},
		"stash list":          {[]string{"stash", "list"}, GitOperationPermitted},
		"stash pop":           {[]string{"stash", "pop"}, GitOperationPermitted},
		"stash apply":         {[]string{"stash", "apply"}, GitOperationPermitted},
		"rm forced":           {[]string{"rm", "-f", "a.go"}, GitOperationDiscard},
		"rm recursive forced": {[]string{"rm", "-rf", "pkg"}, GitOperationDiscard},
		"rm cached":           {[]string{"rm", "--cached", "-f", "a.go"}, GitOperationPermitted},
		"rm plain":            {[]string{"rm", "a.go"}, GitOperationPermitted},
		"read-tree update":    {[]string{"read-tree", "-u", "--reset", "HEAD"}, GitOperationDiscard},
		"read-tree plain":     {[]string{"read-tree", "HEAD"}, GitOperationPermitted},
		"checkout-index":      {[]string{"checkout-index", "-a", "-f"}, GitOperationDiscard},
		"sparse-checkout":     {[]string{"sparse-checkout", "set", "pkg"}, GitOperationDiscard},
		"worktree remove":     {[]string{"worktree", "remove", "--force", "w"}, GitOperationDiscard},
		"submodule deinit":    {[]string{"submodule", "deinit", "-f", "."}, GitOperationDiscard},
		"submodule status":    {[]string{"submodule", "status"}, GitOperationPermitted},
		"status":              {[]string{"status"}, GitOperationPermitted},
		"diff":                {[]string{"diff", "--cached"}, GitOperationPermitted},
		"log":                 {[]string{"log"}, GitOperationPermitted},
		"show":                {[]string{"show", "HEAD"}, GitOperationPermitted},
		"add":                 {[]string{"add", "-A"}, GitOperationPermitted},
		"apply":               {[]string{"apply", "patch.diff"}, GitOperationPermitted},
		"commit":              {[]string{"commit", "-m", "x"}, GitOperationPermitted},
		"empty":               {nil, GitOperationPermitted},

		// GLOBAL OPTIONS MUST NOT HIDE THE VERB. Prefixing one is exactly what
		// a worker does after a bare invocation was refused, so a classifier
		// that read args[0] would be defeated by the second attempt.
		"dash C then reset":   {[]string{"-C", "/tmp/x", "reset", "--hard"}, GitOperationDiscard},
		"git-dir equals":      {[]string{"--git-dir=.git", "clean", "-fd"}, GitOperationDiscard},
		"git-dir separate":    {[]string{"--git-dir", ".git", "checkout", "--", "a"}, GitOperationDiscard},
		"work-tree separate":  {[]string{"--work-tree", ".", "restore", "."}, GitOperationDiscard},
		"config then reset":   {[]string{"-c", "core.pager=cat", "reset", "--hard"}, GitOperationDiscard},
		"no-pager then clean": {[]string{"--no-pager", "clean", "-f"}, GitOperationDiscard},
		"literal then reset":  {[]string{"--literal-pathspecs", "reset", "--hard"}, GitOperationDiscard},
		"globals then status": {[]string{"-C", "/tmp/x", "--no-pager", "status"}, GitOperationPermitted},

		// A FILE NAMED LIKE A FLAG is an operand, not a flag: the pathspec
		// separator ends flag parsing, exactly as it does for git.
		"file called --hard": {[]string{"reset", "--", "--hard"}, GitOperationPermitted},
	} {
		t.Run(name, func(t *testing.T) {
			got, reason := ClassifyGitCommand(tc.args)
			if got != tc.want {
				t.Fatalf("classified %v as %q, want %q", tc.args, got, tc.want)
			}
			if got == GitOperationDiscard && strings.TrimSpace(reason) == "" {
				t.Fatalf("a refusal of %v carries no reason", tc.args)
			}
		})
	}
}

// TestARefusalRecordCarriesNoProviderChosenOperand keeps untrusted text out of
// durable rows. The verb and its flags are runtime vocabulary; a path or a
// revision is something the provider wrote.
func TestARefusalRecordCarriesNoProviderChosenOperand(t *testing.T) {
	rendered := boundedGitArgv([]string{"checkout", "--", "secrets/../../etc/passwd", "other.go"})
	if strings.Contains(rendered, "passwd") || strings.Contains(rendered, "other.go") {
		t.Fatalf("a provider-chosen operand reached the durable record: %q", rendered)
	}
	if !strings.Contains(rendered, "checkout") || !strings.Contains(rendered, "2 operand") {
		t.Fatalf("the record does not describe the command shape: %q", rendered)
	}
}

// TestTheRefusalRecordIsAppendOnlyWithinAnAttempt: a provider that tried three
// destructive recoveries is a different fact from one that tried once, and the
// record has to be able to say so.
func TestTheRefusalRecordIsAppendOnlyWithinAnAttempt(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	for _, args := range [][]string{{"checkout", "--", "."}, {"reset", "--hard"}, {"clean", "-fd"}} {
		if code, _ := brokerGit(t, dir, refusalLog, args...); code == 0 {
			t.Fatalf("%v was permitted", args)
		}
	}
	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(refusals) != 3 {
		t.Fatalf("recorded %d refusals, want all 3: %#v", len(refusals), refusals)
	}
}

// ---------------------------------------------------------------------------
// The clean-workspace law
// ---------------------------------------------------------------------------

// TestADestructiveCommandIsRefusedEvenOnACleanWorkspace states the chosen
// clean-workspace policy - refuse uniformly - and states WHY it is the smallest
// coherent one.
//
// The alternative, permitting the destructive command when the workspace is
// observably clean, requires observing dirtiness and then executing. That is
// the TOCTOU #241 warns about and it is not closable while the provider's own
// process is free to write between the two: observe clean, provider writes a
// file, the destructive command runs, the work is gone. Re-checking only
// narrows the window.
//
// Refusing uniformly removes the window by removing the question. The decision
// is made from the argv, which cannot change, so there is no state to race and
// no way for the clean case to leak into the dirty one. It costs the provider
// nothing it needed: a destructive command against a clean tree is a no-op.
func TestADestructiveCommandIsRefusedEvenOnACleanWorkspace(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	dirty, err := CandidateDirtyPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 0 {
		t.Fatalf("the fixture is not clean: %v", dirty)
	}

	code, diagnostic := brokerGit(t, dir, refusalLog, "reset", "--hard")
	if code == 0 {
		t.Fatal("a destructive command was permitted because the workspace looked clean")
	}
	// And the diagnostic does not invent work that was never at stake.
	if strings.Contains(diagnostic, "path(s) would have been lost") {
		t.Fatalf("the clean-workspace diagnostic invents lost work: %s", diagnostic)
	}
	if !strings.Contains(diagnostic, "destructive Git refused") {
		t.Fatalf("the clean-workspace refusal was not explained: %s", diagnostic)
	}
	// The record is written, and it truthfully names NO dirty paths - the
	// refusal did not depend on there being any.
	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil || len(refusals) != 1 {
		t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
	}
	if refusals[0].DirtyCount != 0 || len(refusals[0].DirtyPaths) != 0 {
		t.Fatalf("a clean workspace was recorded as holding dirty work: %#v", refusals[0])
	}
}

// ---------------------------------------------------------------------------
// F. The runtime's own Git is untouched
// ---------------------------------------------------------------------------

// TestTheRuntimesOwnCandidateGitIsUnaffected is acceptance 14: the restriction
// is on the PROVIDER's authority, not on Zenchron's candidate machinery.
//
// The runtime's own path is RepositoryGitRunner/GitRunner, which builds its
// environment from scratch and resolves git from its own trusted search path.
// It therefore never sees the guard's search path or its sentinel, and this
// test is what keeps that true: it performs the whole runtime-owned sequence -
// inspect, add, commit, re-inspect - against a workspace a provider has dirtied.
func TestTheRuntimesOwnCandidateGitIsUnaffected(t *testing.T) {
	dir, _ := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// provider work\n")
	writeCandidateFile(t, dir, "added.go", "package candidate\n")

	// The runtime can still see the delta the provider produced.
	dirty, err := CandidateDirtyPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 2 {
		t.Fatalf("the runtime cannot observe the candidate delta: %v", dirty)
	}
	// And it can still commit it, which is the authority it alone holds.
	runtimeGit := GitRunner{Dir: dir}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "--no-gpg-sign", "--quiet", "-m", "runtime-owned candidate commit"},
	} {
		if _, err := runtimeGit.run(args...); err != nil {
			t.Fatalf("the runtime's own git %v failed: %v", args, err)
		}
	}
	after, err := CandidateDirtyPaths(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Fatalf("the runtime-owned commit did not settle the candidate: %v", after)
	}
	head, err := runtimeGit.run("rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(string(head)) == "" {
		t.Fatalf("the runtime cannot read its own candidate head: %v", err)
	}
}

// ---------------------------------------------------------------------------
// G. Crash and restart
// ---------------------------------------------------------------------------

// TestDirtyCandidateStateSurvivesRestartAndStaysProtected is acceptance 11.
//
// A controller that died mid-invocation leaves the candidate dirty. The next
// provider invocation is a NEW physical attempt with its own guard directory -
// and the thing it must not be able to do is erase the previous attempt's work
// just because it would prefer a clean checkout.
func TestDirtyCandidateStateSurvivesRestartAndStaysProtected(t *testing.T) {
	dir, firstLog := gitAuthorityFixture(t)
	const inherited = "package candidate\n\n// work the dead attempt left behind\n"
	writeCandidateFile(t, dir, "implementation.go", inherited)
	writeCandidateFile(t, dir, "added.go", "package candidate\n")

	// The controller dies here. Nothing settles, nothing commits, and the
	// dirty candidate state is simply still on disk.
	if code, _ := brokerGit(t, dir, firstLog, "checkout", "--", "."); code == 0 {
		t.Fatal("the first attempt's destructive command was permitted")
	}

	// A new attempt, a new guard record - the attempt identity is part of the
	// path, per #236/#237, so the successor cannot address its predecessor's.
	root := filepath.Dir(filepath.Dir(firstLog))
	secondLog := filepath.Join(root, "attempt-2", gitRefusalLogName)
	if secondLog == firstLog {
		t.Fatal("the successor attempt reused the dead attempt's record path")
	}
	for _, args := range [][]string{{"reset", "--hard"}, {"clean", "-fd"}, {"restore", "."}} {
		if code, _ := brokerGit(t, dir, secondLog, args...); code == 0 {
			t.Fatalf("the successor attempt was permitted to erase inherited work with %v", args)
		}
	}
	if got := candidateFileBody(t, dir, "implementation.go"); got != inherited {
		t.Fatalf("inherited dirty work did not survive the restart:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "added.go")); err != nil {
		t.Fatalf("an inherited untracked candidate file was destroyed: %v", err)
	}
	// Each attempt's record explains its own attempt.
	if first, _ := ReadGitRefusals(firstLog); len(first) != 1 {
		t.Fatalf("the dead attempt's record changed: %#v", first)
	}
	if second, _ := ReadGitRefusals(secondLog); len(second) != 3 {
		t.Fatalf("the successor's record is not its own: %#v", second)
	}
}

// ---------------------------------------------------------------------------
// Aliases: the classifier must see what real Git will run
// ---------------------------------------------------------------------------

// setGitAlias writes one alias into the candidate repository's own
// configuration - which is exactly how a provider or a checked-out tree would,
// since `git config` is an ordinary permitted command.
func setGitAlias(t *testing.T, dir, name, value string) {
	t.Helper()
	if _, err := (GitRunner{Dir: dir}).run("config", "alias."+name, value); err != nil {
		t.Fatalf("setting alias.%s: %v", name, err)
	}
}

// TestAnAliasCannotSmuggleADestructiveCommandPastTheBroker is the hole review
// 5249945424 found, closed and proved.
//
// The classifier used to read the literal verb and then hand the original argv
// to real Git, which expands aliases itself - after the broker had already
// authorized what it saw. A provider could author the bypass out of two
// permitted commands, and a repository could ship it in .git/config.
func TestAnAliasCannotSmuggleADestructiveCommandPastTheBroker(t *testing.T) {
	for name, tc := range map[string]struct {
		alias, value string
		argv         []string
	}{
		"checkout by alias": {"co", "checkout", []string{"co", "--", "implementation.go"}},
		"reset by alias":    {"nuke", "reset --hard", []string{"nuke"}},
		"clean by alias":    {"wipe", "clean -fd", []string{"wipe"}},
		"restore by alias":  {"undo", "restore .", []string{"undo"}},
		// The flags may live on either side of the expansion, so both have to
		// be assembled before classification.
		"flag from the caller": {"c", "checkout", []string{"c", "-f"}},
		"quoted value":         {"q", `checkout "--"`, []string{"q", "implementation.go"}},
		// A global option must not hide the alias either, and -c is the form
		// that defines the alias in the same breath as using it.
		"alias behind -C":   {"co2", "reset --hard", []string{"-C", ".", "co2"}},
		"inline -c defines": {"", "", []string{"-c", "alias.zap=reset --hard", "zap"}},
	} {
		t.Run(name, func(t *testing.T) {
			dir, refusalLog := gitAuthorityFixture(t)
			const work = "package candidate\n\n// work an alias must not reach\n"
			writeCandidateFile(t, dir, "implementation.go", work)
			writeCandidateFile(t, dir, "added.go", "package candidate\n")
			if tc.alias != "" {
				setGitAlias(t, dir, tc.alias, tc.value)
			}

			code, diagnostic := brokerGit(t, dir, refusalLog, tc.argv...)
			if code == 0 {
				t.Fatalf("%v was permitted through its alias", tc.argv)
			}
			if got := candidateFileBody(t, dir, "implementation.go"); got != work {
				t.Fatalf("%v discarded the tracked modification:\n%q", tc.argv, got)
			}
			if _, err := os.Stat(filepath.Join(dir, "added.go")); err != nil {
				t.Fatalf("%v deleted the untracked candidate file: %v", tc.argv, err)
			}
			if !strings.Contains(diagnostic, "destructive Git refused") {
				t.Fatalf("the refusal was not explained: %s", diagnostic)
			}
			// The record names the EFFECTIVE operation, so an operator reading
			// `git co` is told it was a checkout.
			refusals, err := ReadGitRefusals(refusalLog)
			if err != nil || len(refusals) != 1 {
				t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
			}
			t.Logf("recorded: %s | %s", refusals[0].Operation, refusals[0].Reason)
		})
	}

	// MUTATION, in the test rather than only in a patch: the identical alias
	// invocation with no resolution in front of it really does erase the
	// fixture. Without this the refusals above assert that something harmless
	// was refused.
	dir, _ := gitAuthorityFixture(t)
	const work = "package candidate\n\n// work an alias must not reach\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	setGitAlias(t, dir, "co", "checkout")
	// Real Git expands the alias itself, which is the whole defect: the argv
	// the classifier used to see is not the operation that runs.
	unguardedGit(t, dir, "co", "--", "implementation.go")
	if got := candidateFileBody(t, dir, "implementation.go"); got == work {
		t.Fatal("an unresolved alias did NOT discard the work, so these refusals prove nothing")
	}
}

// TestAnAliasChainIsFollowedAndACycleFailsClosed covers the two shapes review
// 5249945424 named explicitly.
func TestAnAliasChainIsFollowedAndACycleFailsClosed(t *testing.T) {
	t.Run("chain", func(t *testing.T) {
		dir, refusalLog := gitAuthorityFixture(t)
		const work = "package candidate\n\n// work a chain must not reach\n"
		writeCandidateFile(t, dir, "implementation.go", work)
		// a -> b -> checkout
		setGitAlias(t, dir, "a", "b")
		setGitAlias(t, dir, "b", "checkout")

		if code, _ := brokerGit(t, dir, refusalLog, "a", "--", "implementation.go"); code == 0 {
			t.Fatal("a two-hop alias chain was permitted")
		}
		if got := candidateFileBody(t, dir, "implementation.go"); got != work {
			t.Fatalf("the chain discarded the work:\n%q", got)
		}
		// And real Git agrees the chain resolves that way, so the resolver is
		// mirroring Git rather than inventing its own rule.
		resolved, err := ResolveGitCommand(dir, []string{"a", "--", "implementation.go"})
		if err != nil {
			t.Fatal(err)
		}
		if verb, _ := gitVerb(resolved); verb != "checkout" {
			t.Fatalf("the chain resolved to %q, want checkout: %v", verb, resolved)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		dir, refusalLog := gitAuthorityFixture(t)
		const work = "package candidate\n\n// work a cycle must not reach\n"
		writeCandidateFile(t, dir, "implementation.go", work)
		setGitAlias(t, dir, "a", "b")
		setGitAlias(t, dir, "b", "a")

		// FAIL CLOSED, and without running real Git: the resolver cannot say
		// what this would do, and "I cannot tell" is not "this is safe".
		if _, err := ResolveGitCommand(dir, []string{"a"}); err == nil {
			t.Fatal("a cycle resolved to something")
		}
		code, diagnostic := brokerGit(t, dir, refusalLog, "a")
		if code == 0 {
			t.Fatalf("a cyclic alias was permitted: %s", diagnostic)
		}
		if got := candidateFileBody(t, dir, "implementation.go"); got != work {
			t.Fatalf("a cyclic alias changed the candidate:\n%q", got)
		}
		refusals, err := ReadGitRefusals(refusalLog)
		if err != nil || len(refusals) != 1 {
			t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
		}
		if !strings.Contains(refusals[0].Reason, aliasResolutionReason) {
			t.Fatalf("a cycle was recorded as something other than unresolvable: %q", refusals[0].Reason)
		}
	})
}

// TestAShellAliasIsRefusedWithoutBeingExecuted is the rule that a boundary must
// not run the thing it is deciding about.
func TestAShellAliasIsRefusedWithoutBeingExecuted(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// work a shell alias must not reach\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	// The alias would erase the work AND leave a marker, so executing it to
	// find out what it means is observable.
	marker := filepath.Join(dir, "shell-alias-ran")
	setGitAlias(t, dir, "sneaky", "!touch "+marker+" && git reset --hard")

	if code, _ := brokerGit(t, dir, refusalLog, "sneaky"); code == 0 {
		t.Fatal("a shell alias was permitted")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the boundary EXECUTED the shell alias to decide what it meant")
	}
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the shell alias discarded the work:\n%q", got)
	}
	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil || len(refusals) != 1 {
		t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
	}
	if !strings.Contains(refusals[0].Reason, "shell alias") {
		t.Fatalf("the record does not say it was a shell alias: %q", refusals[0].Reason)
	}
}

// TestOrdinaryAliasesAndVerbsStillWork keeps the resolver from becoming a
// second refusal surface. A guard that broke every alias would have replaced
// one expensive failure with another.
func TestOrdinaryAliasesAndVerbsStillWork(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	setGitAlias(t, dir, "st", "status --porcelain")
	setGitAlias(t, dir, "ll", "log --oneline")
	setGitAlias(t, dir, "d", "diff")

	for _, argv := range [][]string{
		{"st"}, {"ll", "-1"}, {"d"},
		{"status"}, {"diff"}, {"log", "-1"}, {"rev-parse", "HEAD"},
	} {
		if code, out := brokerGit(t, dir, refusalLog, argv...); code != 0 {
			t.Fatalf("ordinary %v exited %d: %s", argv, code, out)
		}
	}
	if refusals, _ := ReadGitRefusals(refusalLog); len(refusals) != 0 {
		t.Fatalf("ordinary commands were refused: %#v", refusals)
	}
	// AND AN ALIAS NAMED AFTER A COMMAND IS IGNORED, exactly as Git ignores it.
	// Refusing here would mean refusing `git status` because a config file once
	// mentioned it.
	setGitAlias(t, dir, "status", "reset --hard")
	if code, out := brokerGit(t, dir, refusalLog, "status", "--porcelain"); code != 0 {
		t.Fatalf("git status was refused because an ignored alias shadowed it: %s", out)
	}
}

// TestAnAliasValueThatCannotBeParsedFailsClosed: a value nobody can split the
// same way twice is not something to authorize a decision on.
func TestAnAliasValueThatCannotBeParsedFailsClosed(t *testing.T) {
	if _, err := splitGitAliasValue(`checkout "--`); err == nil {
		t.Fatal("an unbalanced quote parsed successfully")
	}
	if _, err := splitGitAliasValue(`checkout '`); err == nil {
		t.Fatal("an unbalanced single quote parsed successfully")
	}
	// And the ordinary shapes split the way Git splits them.
	for value, want := range map[string][]string{
		"checkout":            {"checkout"},
		"reset --hard":        {"reset", "--hard"},
		`checkout "--" a.go`:  {"checkout", "--", "a.go"},
		`commit -m 'a b'`:     {"commit", "-m", "a b"},
		`commit -m "a \"b\""`: {"commit", "-m", `a "b"`},
		"  clean   -fd  ":     {"clean", "-fd"},
	} {
		got, err := splitGitAliasValue(value)
		if err != nil {
			t.Fatalf("%q: %v", value, err)
		}
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("%q split to %#v, want %#v", value, got, want)
		}
	}
	// An empty alias resolves to nothing, which is unresolvable rather than
	// permitted.
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	setGitAlias(t, dir, "empty", "   ")
	if code, _ := brokerGit(t, dir, refusalLog, "empty"); code == 0 {
		t.Fatal("an alias expanding to nothing was permitted")
	}
}

// TestTheResolvedFormIsWhatExecutes closes the last gap: the broker executes
// the expansion it classified, so a provider cannot rewrite the alias between
// the lookup and the execution and make the two disagree.
func TestTheResolvedFormIsWhatExecutes(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	setGitAlias(t, dir, "st", "status --porcelain")

	resolved, err := ResolveGitCommand(dir, []string{"st"})
	if err != nil {
		t.Fatal(err)
	}
	if verb, _ := gitVerb(resolved); verb != "status" {
		t.Fatalf("the alias resolved to %q: %v", verb, resolved)
	}
	// The permitted path runs that form, so its output is the expansion's.
	code, _ := brokerGit(t, dir, refusalLog, "st")
	if code != 0 {
		t.Fatalf("the resolved alias did not execute: %d", code)
	}
	// A non-alias argv is returned byte-identical, which is what keeps the
	// overwhelming majority of invocations behaving exactly as before.
	original := []string{"-C", ".", "diff", "--stat", "--", "implementation.go"}
	same, err := ResolveGitCommand(dir, original)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(same, "\x00") != strings.Join(original, "\x00") {
		t.Fatalf("a non-alias argv was rewritten: %#v", same)
	}
}
