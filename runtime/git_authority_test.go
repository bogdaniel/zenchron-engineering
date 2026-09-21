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

		// COMMITTING IS THE RUNTIME'S. It reached this table as "permitted"
		// only because it was unreachable in practice: every provider commit
		// arrived behind a `-c` prefix and died on the blanket config refusal.
		// Accepting inert `-c` keys makes it reachable, and a provider commit
		// moves HEAD, which the next AssertIntegrity reads as tampering and
		// answers with `reset --hard` plus `clean -fdx`. It is its own class
		// because nothing is being discarded and saying so would be false.
		"commit":            {[]string{"commit", "-m", "x"}, GitOperationRuntimeOwned},
		"commit-tree":       {[]string{"commit-tree", "HEAD^{tree}"}, GitOperationRuntimeOwned},
		"commit behind -c":  {[]string{"-c", "commit.gpgsign=false", "commit", "-m", "x"}, GitOperationRuntimeOwned},
		"status is not one": {[]string{"status", "--porcelain"}, GitOperationPermitted},
	} {
		t.Run(name, func(t *testing.T) {
			got, reason := ClassifyGitCommand(tc.args)
			if got != tc.want {
				t.Fatalf("classified %v as %q, want %q", tc.args, got, tc.want)
			}
			if got != GitOperationPermitted && strings.TrimSpace(reason) == "" {
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
		explanation  string
	}{
		"checkout by alias": {"co", "checkout", []string{"co", "--", "implementation.go"}, "destructive Git refused"},
		"reset by alias":    {"nuke", "reset --hard", []string{"nuke"}, "destructive Git refused"},
		"clean by alias":    {"wipe", "clean -fd", []string{"wipe"}, "destructive Git refused"},
		"restore by alias":  {"undo", "restore .", []string{"undo"}, "destructive Git refused"},
		// The flags may live on either side of the expansion, so both have to
		// be assembled before classification.
		"flag from the caller": {"c", "checkout", []string{"c", "-f"}, "destructive Git refused"},
		"quoted value":         {"q", `checkout "--"`, []string{"q", "implementation.go"}, "destructive Git refused"},
		// A global option must not hide the alias either.
		"alias behind -C": {"co2", "reset --hard", []string{"-C", ".", "co2"}, "destructive Git refused"},
		// `-c` defines the alias in the same breath as using it, and it is
		// refused one step earlier than the others: `alias.*` is not on the
		// runtime's inert key list, so the override never reaches the verb.
		// Since #248 the diagnostic NAMES that key rather than describing the
		// command as a destructive discard it is not - a provider that is told
		// which override was objected to can send the command without it, and
		// the one that was told only "could not be resolved" sent it again 58
		// times.
		"inline -c defines": {"", "", []string{"-c", "alias.zap=reset --hard", "zap"},
			"configuration override `-c alias.zap` is not permitted"},
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
			if !strings.Contains(diagnostic, tc.explanation) {
				t.Fatalf("the refusal was not explained as %q: %s", tc.explanation, diagnostic)
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

// TestBenignConfigOverridesReachTheirVerb is #248.
//
// Claude Code prefixes its Git argv with `-c`, and the blanket refusal of that
// option turned a safety boundary into a productivity trap: run
// run-ca6aecf437c10bc6d2983fe978c5c00a met it 67 times in one attempt, for
// commits, logs, ls-files and remote reads, none of which can discard anything.
// The worker could not read its own repository and retried until its inactivity
// window closed.
//
// The override is now classified by KEY. An inert key reaches its verb, where
// the ordinary taxonomy decides; anything else is refused and NAMED.
func TestBenignConfigOverridesReachTheirVerb(t *testing.T) {
	// The real Claude forms, from the refusal log of that run.
	for name, argv := range map[string][]string{
		"log behind a formatting key": {"-c", "log.date=iso", "log", "-1", "--format=%H:%ct"},
		"ls-files behind quotepath":   {"-c", "core.quotepath=false", "ls-files", "--error-unmatch", "--", "go.mod"},
		// Both spellings of the option, and the keyless `-c key` shorthand.
		"separated key and value": {"-c", "core.abbrev=12", "log", "-1"},
		"config-env joined":       {"--config-env=color.ui=ZC_COLOR", "status"},
		"config-env separated":    {"--config-env", "core.quotepath=ZC_QUOTE", "status"},
		"bare key means true":     {"-c", "core.quotepath", "status"},
		"several overrides":       {"-c", "core.abbrev=12", "-c", "core.quotepath=false", "status"},
		// Case-insensitive, as Git's own key matching is.
		"mixed case key": {"-c", "Core.QuotePath=false", "status"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, reason := ClassifyGitCommand(argv); class != GitOperationPermitted {
				t.Fatalf("%v was refused as %q: %s", argv, class, reason)
			}
			if _, err := ResolveGitCommand(t.TempDir(), argv); err != nil {
				t.Fatalf("%v did not survive resolution: %v", argv, err)
			}
		})
	}
}

// TestConfigOverridesThatRedirectAreStillRefused is the other half, and it is
// the half that keeps this from being "allow Claude's Git flags".
//
// The list is an ALLOWLIST, so an unknown key fails closed, and every refusal
// names the key it objected to.
func TestConfigOverridesThatRedirectAreStillRefused(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		key  string
	}{
		// Programs Git executes.
		"pager":             {[]string{"-c", "core.pager=/tmp/evil", "log"}, "core.pager"},
		"editor":            {[]string{"-c", "core.editor=/tmp/evil", "commit"}, "core.editor"},
		"hooks path":        {[]string{"-c", "core.hooksPath=/tmp/hooks", "status"}, "core.hooksPath"},
		"ssh command":       {[]string{"-c", "core.sshCommand=/tmp/evil", "fetch"}, "core.sshCommand"},
		"askpass":           {[]string{"-c", "core.askPass=/tmp/evil", "fetch"}, "core.askPass"},
		"fsmonitor":         {[]string{"-c", "core.fsmonitor=/tmp/evil", "status"}, "core.fsmonitor"},
		"credential helper": {[]string{"-c", "credential.helper=/tmp/evil", "fetch"}, "credential.helper"},
		"signing program":   {[]string{"-c", "gpg.program=/tmp/evil", "commit"}, "gpg.program"},
		"clean filter":      {[]string{"-c", "filter.x.clean=/tmp/evil", "add", "-A"}, "filter.x.clean"},
		"external diff":     {[]string{"-c", "diff.external=/tmp/evil", "diff"}, "diff.external"},
		// Authority, location and transport.
		"alias defined inline": {[]string{"-c", "alias.zap=reset --hard", "zap"}, "alias.zap"},
		"included config":      {[]string{"-c", "include.path=/tmp/evil", "status"}, "include.path"},
		"conditional include":  {[]string{"-c", "includeIf.gitdir:/.path=/tmp/evil", "status"}, "includeIf.gitdir:/.path"},
		"safe directory":       {[]string{"-c", "safe.directory=*", "status"}, "safe.directory"},
		"bare repository":      {[]string{"-c", "safe.bareRepository=all", "status"}, "safe.bareRepository"},
		"file protocol":        {[]string{"-c", "protocol.file.allow=always", "fetch"}, "protocol.file.allow"},
		"url rewrite":          {[]string{"-c", "url.https://evil/.insteadOf=https://github.com/", "fetch"}, "url.https://evil/.insteadOf"},
		"tls off":              {[]string{"-c", "http.sslVerify=false", "fetch"}, "http.sslVerify"},
		"identity":             {[]string{"-c", "user.email=someone@else", "commit"}, "user.email"},
		// An unknown key is refused because it is unknown, not because it is
		// recognized as dangerous. That is the direction the list has to fail.
		"a key nobody has reasoned about": {[]string{"-c", "zenchron.invented=1", "status"}, "zenchron.invented"},
		// REMOVED FROM THE INERT LIST AFTER REVIEW. `gc.auto=0` reads as
		// turning background work off, but the key is classified for every
		// value and `gc.auto=1` asks Git to repack objects and expire reflogs
		// inside a workspace whose metadata the runtime holds a digest of.
		"automatic gc":          {[]string{"-c", "gc.auto=0", "status"}, "gc.auto"},
		"automatic maintenance": {[]string{"-c", "maintenance.auto=0", "status"}, "maintenance.auto"},
		// A SECTION IS NOT AN ALLOWLIST. Permitting `advice.` by prefix would
		// admit an advice key a future Git adds, without anybody having looked
		// at it - and no advice key appears in the observed provider argv.
		"an advice key":        {[]string{"-c", "advice.detachedHead=false", "status"}, "advice.detachedHead"},
		"an unseen advice key": {[]string{"-c", "advice.somethingNew=false", "status"}, "advice.somethingNew"},
		// SIGNING KEYS CAME OFF THE LIST after review. They choose WHETHER to
		// sign or verify and gpg.program chooses what to execute - but that
		// separation only holds while gpg.program cannot be set, and #251 is
		// open: persisted into .git/config it is beyond the inline allowlist's
		// reach. log.showSignature=true then makes an ordinary `git log` run
		// it, and commit.gpgsign is honoured by cherry-pick, revert, merge,
		// rebase and am, none of which is runtime-owned.
		"verify signatures on log": {[]string{"-c", "log.showSignature=true", "log"}, "log.showSignature"},
		"sign commits":             {[]string{"-c", "commit.gpgsign=true", "cherry-pick", "HEAD"}, "commit.gpgsign"},
		"sign tags":                {[]string{"-c", "tag.gpgsign=true", "status"}, "tag.gpgsign"},
		// The same key arriving by the other spelling.
		"config-env redirect": {[]string{"--config-env=core.pager=EVIL", "log"}, "core.pager"},
		// An override with nothing to override cannot be resolved either.
		"dangling override": {[]string{"-c"}, "-c"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ResolveGitCommand(t.TempDir(), tc.argv)
			if err == nil {
				t.Fatalf("%v was permitted", tc.argv)
			}
			refusal, ok := err.(*GitConfigOverrideRefusedError)
			if !ok {
				t.Fatalf("%v was refused by something other than the key classifier: %T %v", tc.argv, err, err)
			}
			if refusal.Key != tc.key {
				t.Fatalf("%v was refused for %q, want the key %q", tc.argv, refusal.Key, tc.key)
			}
			// NAMING THE KEY IS THE POINT: a provider told only that `-c` is
			// refused cannot tell which part of its invocation to drop.
			if !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("the refusal does not name %q: %v", tc.key, err)
			}
		})
	}
}

// TestDestructiveCommandsBehindABenignOverrideAreStillRefused is the
// composition, and the one that would make this change a regression if it
// failed: accepting the override must not accept the verb behind it.
//
// EVERY KEY HERE IS ON THE INERT LIST, and that is load-bearing rather than
// incidental. A fixture built on a key the classifier refuses would be refused
// at the key and never reach the verb - so it would pass while proving nothing
// about the composition it is named for, which is what an earlier round of this
// test did with `advice.detachedHead` after that key stopped being inert.
//
// So the override is asserted to SURVIVE resolution first, and the verb behind
// it to be refused second. Two assertions, because one of them passing for the
// wrong reason is the failure mode this test exists to have.
func TestDestructiveCommandsBehindABenignOverrideAreStillRefused(t *testing.T) {
	for name, argv := range map[string][]string{
		"reset":    {"-c", "color.ui=never", "reset", "--hard"},
		"clean":    {"-c", "core.quotepath=false", "clean", "-fd"},
		"clean x":  {"-c", "core.quotepath=false", "clean", "-fdx"},
		"checkout": {"-c", "core.quotepath=false", "checkout", "--", "implementation.go"},
		"restore":  {"-c", "log.date=iso", "restore", "."},
		"stash":    {"-c", "core.abbrev=12", "stash", "push"},
	} {
		t.Run(name, func(t *testing.T) {
			// The key is accepted, so the refusal below is about the VERB.
			if _, err := ResolveGitCommand(t.TempDir(), argv); err != nil {
				t.Fatalf("the fixture's override was itself refused, so this proves nothing about %v: %v", argv, err)
			}
			if class, _ := ClassifyGitCommand(argv); class != GitOperationDiscard {
				t.Fatalf("%v classified as %q, want a discard", argv, class)
			}
		})
	}
}

// TestARuntimeOwnedRefusalNamesThePermittedNextAction is the efficiency half of
// #248, and it is a correctness requirement rather than a nicety.
//
// The worker in that run sent the same commit 58 times because every answer it
// got described a problem with its own invocation - a problem a model will keep
// trying to solve. A refusal that names what to do instead preserves exactly
// the same authority and ends the loop on the first reply.
func TestARuntimeOwnedRefusalNamesThePermittedNextAction(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// uncommitted work the provider must keep\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	// THE OVERRIDE MUST BE ONE THE CLASSIFIER ACCEPTS, or the commit is refused
	// at the key and never reaches the arm this test is named for - the same
	// way the composition test above could pass without proving anything.
	argv := []string{"-c", "core.quotepath=false", "commit", "-am", "provider commit"}
	if _, err := ResolveGitCommand(dir, argv); err != nil {
		t.Fatalf("the fixture's override was itself refused, so this proves nothing: %v", err)
	}
	code, diagnostic := brokerGit(t, dir, refusalLog, argv...)
	if code == 0 {
		t.Fatal("a provider commit was permitted")
	}
	// It must not be described as a destructive discard, because it is not one
	// and a false reason is a reason a model will argue with.
	if strings.Contains(diagnostic, "destructive Git refused") {
		t.Fatalf("a commit was explained as a discard: %s", diagnostic)
	}
	for _, want := range []string{
		"runtime-owned Git refused",
		"Do not retry this command",
		"Zenchron commits the candidate itself",
		"continue with the rest of the task",
	} {
		if !strings.Contains(diagnostic, want) {
			t.Fatalf("the refusal does not say %q: %s", want, diagnostic)
		}
	}
	// The work is untouched, and HEAD did not move - which is the whole reason
	// the refusal exists, since a moved HEAD is read as tampering and answered
	// with reset --hard.
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the refused commit changed the candidate:\n%q", got)
	}
	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil || len(refusals) != 1 {
		t.Fatalf("the refusal was not recorded: %v %#v", err, refusals)
	}
}

// TestRefCreationIsRuntimeOwnedAndRefReadingIsNot is the third review blocker
// on #248.
//
// The runtime-owned diagnostic tells a provider not to create "commits,
// branches or tags", and only the commit half was enforced: `checkout -b` and
// `switch -c` were already refused as worktree-replacing, which left the direct
// spellings as the way around a law the refusal was stating anyway. The trusted
// provider instructions have always said Git metadata belongs to the runtime.
//
// The reading forms must stay permitted. Refusing `git branch --show-current`
// would rebuild, one verb along, exactly the trap #248 exists to remove.
func TestRefCreationIsRuntimeOwnedAndRefReadingIsNot(t *testing.T) {
	for name, tc := range map[string]struct {
		argv []string
		want GitOperationClass
	}{
		// Writing.
		"create a branch":      {[]string{"branch", "feature"}, GitOperationRuntimeOwned},
		"create from a commit": {[]string{"branch", "feature", "HEAD~1"}, GitOperationRuntimeOwned},
		"delete a branch":      {[]string{"branch", "-D", "feature"}, GitOperationRuntimeOwned},
		"rename a branch":      {[]string{"branch", "-m", "old", "new"}, GitOperationRuntimeOwned},
		"set upstream":         {[]string{"branch", "--set-upstream-to", "origin/main"}, GitOperationRuntimeOwned},
		"create a tag":         {[]string{"tag", "v1.0.0"}, GitOperationRuntimeOwned},
		"annotated tag":        {[]string{"tag", "-a", "v1", "-m", "release"}, GitOperationRuntimeOwned},
		"delete a tag":         {[]string{"tag", "-d", "v1"}, GitOperationRuntimeOwned},
		"write a ref":          {[]string{"update-ref", "refs/heads/x", "HEAD"}, GitOperationRuntimeOwned},
		"delete a ref":         {[]string{"update-ref", "-d", "refs/heads/x"}, GitOperationRuntimeOwned},
		"behind a benign -c":   {[]string{"-c", "color.ui=never", "branch", "feature"}, GitOperationRuntimeOwned},
		"after the separator":  {[]string{"branch", "--", "feature"}, GitOperationRuntimeOwned},

		// Reading. Every one of these answers a question a worker legitimately
		// has, and several carry an operand that is a revision or a pattern
		// rather than a name.
		"list branches":        {[]string{"branch"}, GitOperationPermitted},
		"list all branches":    {[]string{"branch", "-a"}, GitOperationPermitted},
		"verbose list":         {[]string{"branch", "-vv"}, GitOperationPermitted},
		"current branch":       {[]string{"branch", "--show-current"}, GitOperationPermitted},
		"branches containing":  {[]string{"branch", "--contains", "HEAD"}, GitOperationPermitted},
		"branches merged":      {[]string{"branch", "--merged", "origin/main"}, GitOperationPermitted},
		"formatted list":       {[]string{"branch", "--format=%(refname)"}, GitOperationPermitted},
		"list tags":            {[]string{"tag"}, GitOperationPermitted},
		"tag pattern":          {[]string{"tag", "-l", "v*"}, GitOperationPermitted},
		"tags pointing at":     {[]string{"tag", "--points-at", "HEAD"}, GitOperationPermitted},
		"for-each-ref is read": {[]string{"for-each-ref", "refs/heads"}, GitOperationPermitted},
		"rev-parse is read":    {[]string{"rev-parse", "HEAD"}, GitOperationPermitted},
	} {
		t.Run(name, func(t *testing.T) {
			got, reason := ClassifyGitCommand(tc.argv)
			if got != tc.want {
				t.Fatalf("classified %v as %q, want %q", tc.argv, got, tc.want)
			}
			if got != GitOperationPermitted && strings.TrimSpace(reason) == "" {
				t.Fatalf("a refusal of %v carries no reason", tc.argv)
			}
		})
	}
}

// TestRefReadingFormsWithOperandsStayPermitted is the review's second and third
// blockers on the ref split.
//
// A read is not distinguished by having no operand - `git branch --list
// "feature/*"`, `git tag --verify v1` and `git symbolic-ref --short HEAD` all
// carry one and all ask questions. Classifying by "is there an operand" alone
// would refuse exactly the inspection a worker is entitled to, which is the
// #248 trap rebuilt one verb along.
func TestRefReadingFormsWithOperandsStayPermitted(t *testing.T) {
	for name, argv := range map[string][]string{
		"branch list by pattern":   {"branch", "--list", "feature/*"},
		"branch list short":        {"branch", "-l", "feature/*"},
		"branch remotes pattern":   {"branch", "-r", "origin/*"},
		"branch all pattern":       {"branch", "--all", "feature/*"},
		"branch remotes long":      {"branch", "--remotes", "origin/*"},
		"tag verify":               {"tag", "--verify", "v1.0.0"},
		"tag verify short":         {"tag", "-v", "v1.0.0"},
		"symbolic-ref read":        {"symbolic-ref", "HEAD"},
		"symbolic-ref short read":  {"symbolic-ref", "--short", "HEAD"},
		"symbolic-ref quiet read":  {"symbolic-ref", "-q", "HEAD"},
		"branch contains a commit": {"branch", "--contains", "HEAD"},
		"branch sorted format":     {"branch", "--sort=-committerdate", "--format=%(refname)"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, reason := ClassifyGitCommand(argv); class != GitOperationPermitted {
				t.Fatalf("%v was refused as %q: %s", argv, class, reason)
			}
		})
	}
}

// TestRefWritingFormsAreRuntimeOwnedHoweverSpelled is the fourth blocker: a
// writing flag must be recognized in the spelling that carries its value with
// an `=`, and inside a short cluster.
//
// The earlier helper skipped a joined flag as "a flag with a value" BEFORE
// asking whether that flag writes, so `--set-upstream-to=origin/main` walked
// past the check `--set-upstream-to origin/main` failed.
func TestRefWritingFormsAreRuntimeOwnedHoweverSpelled(t *testing.T) {
	for name, argv := range map[string][]string{
		"joined set-upstream":       {"branch", "--set-upstream-to=origin/main"},
		"separated set-upstream":    {"branch", "--set-upstream-to", "origin/main"},
		"joined move":               {"branch", "--move=old", "new"},
		"joined delete":             {"branch", "--delete=feature"},
		"short cluster delete":      {"branch", "-aD", "feature"},
		"short cluster force":       {"branch", "-fm", "old", "new"},
		"unset upstream":            {"branch", "--unset-upstream"},
		"edit description":          {"branch", "--edit-description"},
		"joined tag message":        {"tag", "--message=release", "v1"},
		"tag force short cluster":   {"tag", "-af", "v1"},
		"symbolic-ref write":        {"symbolic-ref", "HEAD", "refs/heads/other"},
		"symbolic-ref with reason":  {"symbolic-ref", "-m", "why", "HEAD", "refs/heads/other"},
		"symbolic-ref delete":       {"symbolic-ref", "--delete", "HEAD"},
		"symbolic-ref delete short": {"symbolic-ref", "-d", "HEAD"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, reason := ClassifyGitCommand(argv); class != GitOperationRuntimeOwned {
				t.Fatalf("%v classified as %q, want runtime-owned", argv, class)
			} else if strings.TrimSpace(reason) == "" {
				t.Fatalf("a refusal of %v carries no reason", argv)
			}
		})
	}
}

// TestAnOptionalValueFlagDoesNotSwallowTheRefName is a bypass this classifier
// had, found in review and confirmed against real Git.
//
// `--color` and `--abbrev` take an OPTIONAL value, which Git requires to be
// attached: `--color=always`, `--abbrev=12`. A bare argument after them is
// therefore the branch or tag NAME, not the flag's value. Treating them as
// value-carrying skipped the name, left no operand behind, and classified a ref
// creation as permitted.
//
// Observed on git 2.55.0, in a scratch repository:
//
//	git branch --color probe-ref   -> refs/heads/probe-ref EXISTS
//	git branch --abbrev probe-ref  -> refs/heads/probe-ref EXISTS
//	git tag --color probe-ref      -> refs/tags/probe-ref EXISTS
//
// while every flag still on the listing table was observed NOT to create one.
// That is why membership there is evidence rather than inference: the listing
// table is the permissive direction, and a wrong entry is a bypass.
func TestAnOptionalValueFlagDoesNotSwallowTheRefName(t *testing.T) {
	for name, argv := range map[string][]string{
		"branch color":          {"branch", "--color", "provider-branch"},
		"branch abbrev":         {"branch", "--abbrev", "provider-branch"},
		"tag color":             {"tag", "--color", "provider-tag"},
		"branch color attached": {"branch", "--color=always", "provider-branch"},
		"branch verbose":        {"branch", "-v", "provider-branch"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, _ := ClassifyGitCommand(argv); class != GitOperationRuntimeOwned {
				t.Fatalf("%v classified as %q, want runtime-owned: Git creates the ref", argv, class)
			}
		})
	}
	// And the attached form of the value, which IS how Git takes one, still
	// leaves a listing invocation a listing invocation.
	for name, argv := range map[string][]string{
		"attached color while listing":  {"branch", "--list", "--color=always", "feature/*"},
		"attached abbrev while listing": {"branch", "--list", "--abbrev=12"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, reason := ClassifyGitCommand(argv); class != GitOperationPermitted {
				t.Fatalf("%v was refused as %q: %s", argv, class, reason)
			}
		})
	}
}

// TestTagNumericListingIsARead covers `git tag -n[<num>]`, which Git documents
// as implying --list, so the operand beside it is a pattern.
func TestTagNumericListingIsARead(t *testing.T) {
	for name, argv := range map[string][]string{
		"bare":             {"tag", "-n", "release-*"},
		"with a count":     {"tag", "-n5", "release-*"},
		"count no pattern": {"tag", "-n3"},
	} {
		t.Run(name, func(t *testing.T) {
			if class, reason := ClassifyGitCommand(argv); class != GitOperationPermitted {
				t.Fatalf("%v was refused as %q: %s", argv, class, reason)
			}
		})
	}
	// -n is not a blanket escape: a writing flag beside it still writes.
	if class, _ := ClassifyGitCommand([]string{"tag", "-n", "-d", "v1"}); class != GitOperationRuntimeOwned {
		t.Fatalf("a delete behind -n was classified as %q", class)
	}
}

// smokeRunBrokerArgv is the Git argv Claude Code actually emitted during run
// run-9bd736a983ab21ea6be069da6f24e364 — the first governed self-hosted run a
// Claude worker carried through candidate, assurance, authority and
// publication (PR #252, candidate 72c178bd, assurance passed on attempt 1).
//
// It is kept as a FIXTURE rather than a note, because its numbers are the
// benchmark the boundary is tuned against:
//
//	broker refusals   54
//	  config-key      48   user.email 43, core.hooksPath 3, core.fsmonitor 1, log.showSignature 1
//	  destructive      6   reset --hard x2, clean -fd, clean -fdx, checkout -- <path>, restore <path>
//
// The six destructive refusals are NOT a target for optimization. They are
// evidence that #241 protected paid-for candidate work, and a change that
// reduced them would be a regression wearing an improvement's clothes. The
// optimization target is only the repeated configuration and authority
// friction: 43 identical commits, answered 43 times with a complaint about the
// provider's own invocation.
var smokeRunBrokerArgv = []struct {
	name  string
	argv  []string
	want  GitOperationClass
	count int
}{
	// The loop. One provider intent, refused forty-three times.
	{"commit behind an identity override", []string{"-c", "user.email=agent@example.invalid", "commit", "-m", "wip"}, GitOperationRuntimeOwned, 43},
	// Reads, each refused once on a key that must stay refused.
	{"ls-files behind a hooks path", []string{"-c", "core.hooksPath=/tmp/hooks", "ls-files", "--error-unmatch", "--", "go.mod"}, GitOperationPermitted, 1},
	{"remote behind a hooks path", []string{"-c", "core.hooksPath=/tmp/hooks", "remote"}, GitOperationPermitted, 1},
	{"remote -v behind a hooks path", []string{"-c", "core.hooksPath=/tmp/hooks", "remote", "-v"}, GitOperationPermitted, 1},
	{"log behind a monitor", []string{"-c", "core.fsmonitor=/tmp/watch", "log", "-1", "--format=%H:%ct"}, GitOperationPermitted, 1},
	{"log behind a signature toggle", []string{"-c", "log.showSignature=true", "log", "--since=7.days", "--name-only"}, GitOperationPermitted, 1},
	// The six that must stay refused, and stay DESTRUCTIVE. Two of them were
	// sent behind the same identity override the commits carried, which is the
	// case that proves the precedence rule is about CLASS and not about one
	// favoured arm.
	{"reset behind an identity override", []string{"-c", "user.email=agent@example.invalid", "reset", "--hard"}, GitOperationDiscard, 1},
	{"reset", []string{"reset", "--hard"}, GitOperationDiscard, 1},
	{"clean", []string{"clean", "-fd"}, GitOperationDiscard, 1},
	{"clean x", []string{"clean", "-fdx"}, GitOperationDiscard, 1},
	{"checkout a path", []string{"checkout", "--", "implementation.go"}, GitOperationDiscard, 1},
	{"restore a path", []string{"restore", "implementation.go"}, GitOperationDiscard, 1},
}

// TestTheSmokeRunArgvGetsTheAnswerThatEndsTheLoop replays that run through the
// broker and asserts the answer each shape now receives.
//
// The measure is not "were there refusals". One runtime-owned refusal is
// healthy and expected. The measure is whether the FIRST answer terminates the
// provider's decision tree: a commit must be told that committing is not its
// to do, not that its override is invalid.
func TestTheSmokeRunArgvGetsTheAnswerThatEndsTheLoop(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// the work this run produced\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	for _, tc := range smokeRunBrokerArgv {
		t.Run(tc.name, func(t *testing.T) {
			code, diagnostic := brokerGit(t, dir, refusalLog, tc.argv...)
			switch tc.want {
			case GitOperationRuntimeOwned:
				if code == 0 {
					t.Fatalf("%v was permitted", tc.argv)
				}
				// THE TERMINAL FACT LEADS. This is the whole of #253: the
				// provider must read the authority decision, not a fixable
				// complaint about its own invocation.
				for _, want := range []string{
					"runtime-owned Git refused",
					"Do not retry this command",
					"Zenchron commits the candidate itself",
				} {
					if !strings.Contains(diagnostic, want) {
						t.Fatalf("the answer does not say %q, so it does not end the loop: %s", want, diagnostic)
					}
				}
				// The configuration is still refused - it is simply not the
				// reason, and the provider is told that removing it would not
				// help.
				if !strings.Contains(diagnostic, "user.email") {
					t.Fatalf("the refused override is no longer recorded in the answer: %s", diagnostic)
				}
				if strings.Index(diagnostic, "runtime-owned Git refused") > strings.Index(diagnostic, "user.email") {
					t.Fatalf("the incidental objection leads the answer: %s", diagnostic)
				}
			case GitOperationDiscard:
				// NOT A TARGET FOR OPTIMIZATION. These are #241 protecting
				// paid-for candidate work.
				if code == 0 {
					t.Fatalf("%v was permitted", tc.argv)
				}
				if !strings.Contains(diagnostic, "destructive Git refused") {
					t.Fatalf("%v is no longer answered as a discard: %s", tc.argv, diagnostic)
				}
			case GitOperationPermitted:
				// Refused today on the key, and #254 is where that changes.
				// What must hold now is that the answer NAMES the key, so the
				// provider can act on it rather than guess.
				if code != 0 && !strings.Contains(diagnostic, "configuration override") {
					t.Fatalf("%v was refused without naming the objection: %s", tc.argv, diagnostic)
				}
			}
			if got := candidateFileBody(t, dir, "implementation.go"); got != work {
				t.Fatalf("%v reached the candidate:\n%q", tc.argv, got)
			}
		})
	}
}

// TestTheRuntimeOwnedAnswerIsUnchangedWithoutAnOverride keeps the precedence
// change from being the only path that produces the actionable answer.
func TestTheRuntimeOwnedAnswerIsUnchangedWithoutAnOverride(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n")
	code, diagnostic := brokerGit(t, dir, refusalLog, "commit", "-m", "wip")
	if code == 0 {
		t.Fatal("a bare provider commit was permitted")
	}
	if !strings.Contains(diagnostic, "runtime-owned Git refused") {
		t.Fatalf("a commit with no override lost its answer: %s", diagnostic)
	}
	// With nothing incidental to report, nothing incidental is reported.
	if strings.Contains(diagnostic, "also carried") {
		t.Fatalf("an incidental note appeared with no incidental objection: %s", diagnostic)
	}
}

// TestAnAliasCannotBecomeARuntimeOwnedAnswer is the anti-regression for the
// precedence change: classifying the UNRESOLVED argv must not let an alias
// present itself as a runtime-owned verb and collect the gentler answer.
func TestAnAliasCannotBecomeARuntimeOwnedAnswer(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// work an alias must not reach\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	// An alias NAMED like a runtime-owned verb cannot exist - Git ignores an
	// alias whose name collides with a builtin - so the hostile shape is an
	// alias with its own name, defined inline, expanding to a discard.
	code, diagnostic := brokerGit(t, dir, refusalLog, "-c", "alias.ci=reset --hard", "ci")
	if code == 0 {
		t.Fatal("an inline alias was permitted")
	}
	if strings.Contains(diagnostic, "runtime-owned Git refused") {
		t.Fatalf("an alias collected the runtime-owned answer: %s", diagnostic)
	}
	if !strings.Contains(diagnostic, "alias.ci") {
		t.Fatalf("the refusal does not name the override: %s", diagnostic)
	}
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the alias reached the candidate:\n%q", got)
	}
}

// TestTheSmokeRunReplaysToTheExpectedTaxonomy replays the run's FULL call
// sequence - all 54 invocations, at their production multiplicity - and asserts
// what the refusal record accumulates to.
//
// The row-wise test above asks what each shape is answered with. This one asks
// what an operator reading the durable evidence afterwards would see, which is
// a different question and the one the benchmark is for: the taxonomy is how
// the next run is compared to this one.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. It proves the ANSWER is stable across
// 43 identical commits. It cannot prove the provider stops after the first one
// - that is behaviour, not classification, and only a live run can measure it.
// The target #253 records ("no repeated equivalent refusal") is therefore
// validated by the next Claude smoke, not here. What here can catch is the
// answer regressing back to the sentence that caused the loop.
func TestTheSmokeRunReplaysToTheExpectedTaxonomy(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// the work this run produced\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	calls := 0
	for _, tc := range smokeRunBrokerArgv {
		for i := 0; i < tc.count; i++ {
			if code, _ := brokerGit(t, dir, refusalLog, tc.argv...); code == 0 && tc.want != GitOperationPermitted {
				t.Fatalf("%v was permitted", tc.argv)
			}
			calls++
		}
	}
	if calls != 54 {
		t.Fatalf("the fixture replayed %d calls, and the run made 54", calls)
	}

	refusals, err := ReadGitRefusals(refusalLog)
	if err != nil {
		t.Fatal(err)
	}
	taxonomy := map[string]int{}
	for _, refusal := range refusals {
		switch {
		case strings.Contains(refusal.Reason, "not the provider's"):
			taxonomy["runtime-owned"]++
		case strings.Contains(refusal.Reason, "would discard"), strings.Contains(refusal.Reason, "would delete"),
			strings.Contains(refusal.Reason, "would replace"), strings.Contains(refusal.Reason, "would overwrite"),
			strings.Contains(refusal.Reason, "would remove"):
			taxonomy["destructive"]++
		case strings.Contains(refusal.Reason, "configuration override"):
			taxonomy["config-key"]++
		default:
			taxonomy["other"]++
		}
	}
	// The production run recorded 48 config-key and 6 destructive. The 43
	// commits move to runtime-owned, and the two destructive commands that
	// carried the same override move to destructive - which is the whole of
	// #253 expressed as a count.
	for class, want := range map[string]int{
		"runtime-owned": 43,
		"destructive":   6,
		"config-key":    5,
		"other":         0,
	} {
		if taxonomy[class] != want {
			t.Errorf("%s refusals: got %d, want %d (full taxonomy %v)", class, taxonomy[class], want, taxonomy)
		}
	}
	if total := len(refusals); total != 54 {
		t.Fatalf("recorded %d refusals, want the run's 54", total)
	}
	// The candidate is untouched after all 54, which is the point of the six.
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the replay reached the candidate:\n%q", got)
	}
}
