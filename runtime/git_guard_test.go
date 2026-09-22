package runtime

// The boundary, as opposed to the law.
//
// git_authority_test.go proves what the runtime decides. This file proves that
// a provider's Git invocation actually arrives at that decision - and, where it
// does not, says so out loud instead of asserting something weaker and calling
// it a boundary. #241 asks for exactly that distinction.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func guardFixture(t *testing.T) (*GitGuard, string) {
	t.Helper()
	requireGitFixture(t)
	dir, _ := gitAuthorityFixture(t)
	return guardFor(t, dir), dir
}

// guardFor prepares a guard whose broker is a real executable that reports
// success and does nothing. The broker's DECISION is proved in
// git_authority_test.go against real Git; what these tests need is a shim that
// genuinely runs, so that "this spelling reached the shim" and "this spelling
// reached the repository" are distinguishable outcomes rather than both being
// an exec failure.
func guardFor(t *testing.T, candidateDir string) *GitGuard {
	t.Helper()
	broker, err := exec.LookPath("true")
	if err != nil {
		t.Skip("no true(1) to stand in for the broker")
	}
	// THE SHIM HAS TO RUN, so the guard directory has to be somewhere this
	// environment will execute from. Under the assurance sandbox's noexec /tmp
	// it is not, and the shim then loses PATH resolution to the system git -
	// which is #247, and which made these tests fail on a guard that was
	// working correctly.
	guard, err := PrepareGitGuard(execCapableTempDir(t), ExecutionAttemptRef{
		RunID: "run-guard", OperationID: "run-guard:execution.invoke:initial|1|base", Attempt: 1,
	}, candidateDir, "", []string{broker})
	if err != nil {
		t.Fatal(err)
	}
	return guard
}

// workerEnvironment is the environment a native CLI actually receives for Git:
// the guard first on the search path, and the brokered sentinel.
func workerEnvironment(guard *GitGuard) []string {
	env := append(os.Environ(), "PATH="+guard.SearchPath(os.Getenv("PATH")))
	return append(env, guard.Env()...)
}

// TestTheGuardPutsItsOwnGitFirstOnTheWorkerPath is the first mechanism: a bare
// `git`, which is what a model types, resolves to the runtime's broker.
func TestTheGuardPutsItsOwnGitFirstOnTheWorkerPath(t *testing.T) {
	guard, _ := guardFixture(t)

	shim := filepath.Join(guard.BinDir, "git")
	info, err := os.Stat(shim)
	if err != nil {
		t.Fatalf("the guard prepared no git: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("the shim is not executable: %v", info.Mode())
	}
	// It is FIRST, so it wins over the system git rather than sitting behind it.
	path := guard.SearchPath("/usr/bin:/bin")
	if !strings.HasPrefix(path, guard.BinDir+string(os.PathListSeparator)) {
		t.Fatalf("the guard is not first on the worker path: %s", path)
	}
	// And a resolution against that path actually finds the shim.
	resolved, err := exec.LookPath("git")
	if err == nil {
		t.Setenv("PATH", path)
		if found, err := exec.LookPath("git"); err != nil || found != shim {
			t.Fatalf("a bare git resolved to %q (%v), want the shim %q; system git is %q",
				found, err, shim, resolved)
		}
	}
	// The shim decides nothing: it forwards to the broker with the runtime's
	// own arguments. Anything it decided would be law living in a file no test
	// reads.
	script, err := os.ReadFile(shim)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"#!/bin/sh", "exec ", "--candidate", "--refusal-log", `"$@"`} {
		if !strings.Contains(string(script), required) {
			t.Fatalf("the shim is missing %q:\n%s", required, script)
		}
	}
	for _, forbidden := range []string{"reset", "checkout", "clean", "if ", "case "} {
		if strings.Contains(string(script), forbidden) {
			t.Fatalf("the shim contains a decision (%q):\n%s", forbidden, script)
		}
	}
}

// TestUnshimmedGitFailsClosedAgainstTheCandidateRepository is the second
// mechanism, and it is the one that makes the first a boundary rather than a
// convenience.
//
// This is #241's bypass question asked directly, with real processes: absolute
// path, `env`, and a shell. None of them may reach the candidate repository.
// The mechanism is not the shim - it is that the worker's GIT_DIR names
// something that is not a repository, so Git performs no discovery at all.
func TestUnshimmedGitFailsClosedAgainstTheCandidateRepository(t *testing.T) {
	guard, dir := guardFixture(t)
	// THE REAL GIT, RESOLVED THE WAY THE RUNTIME RESOLVES IT - not by a PATH
	// lookup. This test's whole premise is invoking Git by a spelling that goes
	// AROUND the shim, and exec.LookPath finds whatever is first on the search
	// path, which inside a brokered environment is a shim. A provider running
	// this suite under the guard would therefore have been "bypassing" the
	// boundary with the boundary, and the test would report on something it was
	// not written to measure.
	systemGit, err := gitBinary()
	if err != nil {
		t.Skip("no trusted git to attempt a bypass with")
	}
	const work = "package candidate\n\n// work a bypass must not reach\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	writeCandidateFile(t, dir, "added.go", "package candidate\n")

	workerEnv := workerEnvironment(guard)

	// EVERY SPELLING THAT RESOLVES BY NAME REACHES THE SHIM, because the guard
	// is first on the search path - `env git`, `command git` and a bare `git`
	// in a shell are all PATH lookups. They are proved separately, in
	// TestNameResolvedGitReachesTheShim; the table here is the spellings that
	// deliberately go around the name, which are the ones the sentinel has to
	// answer for.
	for name, argv := range map[string][]string{
		"absolute path":    {systemGit, "reset", "--hard"},
		"absolute clean":   {systemGit, "clean", "-fd"},
		"absolute restore": {systemGit, "checkout", "--", "implementation.go"},
		"shell absolute":   {"/bin/sh", "-c", systemGit + " reset --hard"},
		"shell command":    {"/bin/sh", "-c", "command " + systemGit + " clean -fdx"},
		"subshell":         {"/bin/sh", "-c", "(cd . && " + systemGit + " restore .)"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir, cmd.Env = dir, workerEnv
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("an unshimmed %v SUCCEEDED against the candidate repository:\n%s", argv, out)
			}
			// It fails for the right reason, and Git's own error names the
			// brokered path - so a model that hits the boundary is told where
			// it is rather than left guessing.
			if !strings.Contains(string(out), brokeredGitDirName) {
				t.Fatalf("%v failed for an unrelated reason:\n%s", argv, out)
			}
		})
	}

	// NOTHING WAS TOUCHED by any of them.
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("a bypass reached the tracked modification:\n%q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "added.go")); err != nil {
		t.Fatalf("a bypass deleted the untracked candidate file: %v", err)
	}
}

// TestTheBoundaryDoesNotClaimToSurviveDismantlingItsOwnEnvironment is the
// limitation, asserted rather than written only in prose.
//
// These workers are `operator_trusted`: they run under the operator's own
// account, so a worker that deliberately sets out to defeat the runtime can
// clear the variable the runtime set and reach the repository again. No
// in-process mechanism closes that while one account owns both sides - it is
// the residual this adapter already NAMES, and it is why
// RequireProtectedIsolation refuses these adapters for protected work.
//
// The test exists so the claim in git_guard.go cannot silently become false,
// and so a reader of this package finds the boundary's edge stated as a fact
// instead of discovering it in production. What #241 asked for is that the
// limitation be explained rather than hidden behind a green test; this is that
// explanation, executable.
func TestTheBoundaryDoesNotClaimToSurviveDismantlingItsOwnEnvironment(t *testing.T) {
	guard, dir := guardFixture(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no system git")
	}
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// work\n")

	systemGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no system git")
	}
	workerEnv := workerEnvironment(guard)

	// The dismantling needs BOTH halves: go around the name AND clear the
	// variable. Either alone is answered - a bare name reaches the shim, and an
	// absolute path meets the sentinel. This is NOT a defect in the guard; it is
	// the boundary of what an in-process mechanism can assert about a process
	// running as the same user, and asserting it here keeps the documentation
	// honest about where that boundary is.
	cmd := exec.Command("/bin/sh", "-c", "unset GIT_DIR; "+systemGit+" status --porcelain")
	cmd.Dir, cmd.Env = dir, workerEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		// If this ever starts failing, the boundary became STRONGER than
		// documented. That is good news and the documentation must be updated
		// rather than the test deleted.
		t.Skipf("deliberate dismantling no longer reaches the repository (%v); git_guard.go understates the boundary:\n%s", err, out)
	}
	// What survives even then: the runtime still holds the authority that
	// matters, because the candidate commit is the runtime's and the refusal
	// record is outside the workspace.
	if strings.Contains(string(out), brokeredGitDirName) {
		t.Fatalf("the bypass did not actually reach the repository:\n%s", out)
	}
	t.Logf("documented residual: `unset GIT_DIR` plus an absolute git path reaches the repository; operator_trusted owns this gap")
}

// TestNameResolvedGitReachesTheShim is the other half of the bypass question,
// and it is the half that covers what a model actually types.
//
// Every spelling here is a PATH lookup, so the guard being first is what
// decides them: a bare `git`, `env git`, `command git`, and a bare `git` inside
// a shell all reach the runtime's own broker. The shim in this fixture reports
// success and does nothing, so "reached the shim" is observable as the absence
// of both a Git error and any change to the candidate.
func TestNameResolvedGitReachesTheShim(t *testing.T) {
	guard, dir := guardFixture(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no system git")
	}
	const work = "package candidate\n\n// work only the broker may be asked about\n"
	writeCandidateFile(t, dir, "implementation.go", work)
	workerEnv := workerEnvironment(guard)

	// Each of these performs its OWN lookup against the worker environment.
	// A bare exec.Command("git", ...) is deliberately not among them: Go
	// resolves that against the TEST process's search path rather than
	// cmd.Env, so it would prove something about this test harness instead of
	// about the worker. Bare-name resolution against the worker path is
	// asserted directly in TestTheGuardPutsItsOwnGitFirstOnTheWorkerPath, and
	// a shell is the faithful stand-in for a provider doing its own lookup.
	for name, argv := range map[string][]string{
		"env git":       {"env", "git", "reset", "--hard"},
		"shell bare":    {"/bin/sh", "-c", "git clean -fd"},
		"shell command": {"/bin/sh", "-c", "command git checkout -- ."},
		"shell env":     {"/bin/sh", "-c", "env git restore ."},
		"shell exec":    {"/bin/sh", "-c", "exec git reset --hard"},
	} {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(argv[0], argv[1:]...)
			cmd.Dir, cmd.Env = dir, workerEnv
			out, err := cmd.CombinedOutput()
			// It reached the SHIM, not the repository: no Git error naming the
			// sentinel, and nothing destroyed.
			if strings.Contains(string(out), brokeredGitDirName) {
				t.Fatalf("%v went around the shim and met the sentinel instead:\n%s", argv, out)
			}
			if err != nil {
				t.Fatalf("%v did not reach the shim: %v\n%s", argv, err, out)
			}
			if got := candidateFileBody(t, dir, "implementation.go"); got != work {
				t.Fatalf("%v changed the candidate:\n%q", argv, got)
			}
		})
	}
}

// TestTheBrokerSeesPastItsOwnSentinel: the shim is the ONE caller that must,
// or every permitted command would fail for the same reason a bypass does.
func TestTheBrokerSeesPastItsOwnSentinel(t *testing.T) {
	guard, dir := guardFixture(t)
	t.Setenv(brokeredGitDirEnv, guard.SentinelGitDir)

	// Asserted against the environment the broker actually builds, not against
	// a helper standing in for it - the sentinel is a GIT_ variable, so what
	// removes it now is the same prefix rule that removes every other one.
	for _, entry := range brokerGitEnv() {
		if strings.HasPrefix(entry, brokeredGitDirEnv+"=") {
			t.Fatalf("the sentinel survived into the broker's own child environment: %s", entry)
		}
	}
	// And an ordinary command really does work through the broker while the
	// sentinel is set in this process.
	if code, out := brokerGit(t, dir, guard.RefusalLog, "status", "--porcelain"); code != 0 {
		t.Fatalf("an ordinary command failed under the sentinel: %d %s", code, out)
	}
}

// TestPreparingTheGuardIsIdempotentAndAttemptScoped covers the crash boundary
// from the guard's side: a re-run of the SAME physical attempt - a controller
// that died before settlement - must find a consistent guard rather than its
// predecessor's leftovers, and a DIFFERENT attempt must never share one.
func TestPreparingTheGuardIsIdempotentAndAttemptScoped(t *testing.T) {
	requireGitFixture(t)
	dir, _ := gitAuthorityFixture(t)
	state := t.TempDir()
	attempt := ExecutionAttemptRef{RunID: "run-x", OperationID: "run-x:execution.invoke:initial|1|base", Attempt: 1}

	first, err := PrepareGitGuard(state, attempt, dir, "", []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := brokerGit(t, dir, first.RefusalLog, "reset", "--hard"); code == 0 {
		t.Fatal("the destructive command was permitted")
	}
	if refusals, _ := ReadGitRefusals(first.RefusalLog); len(refusals) != 1 {
		t.Fatalf("the refusal was not recorded: %#v", refusals)
	}

	// The same attempt prepared again: the record starts empty, so what is read
	// back afterwards explains the invocation being explained rather than
	// accumulating across a crash.
	again, err := PrepareGitGuard(state, attempt, dir, "", []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if again.RefusalLog != first.RefusalLog {
		t.Fatalf("the same attempt got two record paths: %q then %q", first.RefusalLog, again.RefusalLog)
	}
	if refusals, _ := ReadGitRefusals(again.RefusalLog); len(refusals) != 0 {
		t.Fatalf("a re-prepared attempt inherited stale refusals: %#v", refusals)
	}

	// A different attempt identity is a different guard, per #236/#237.
	next := attempt
	next.Attempt = 2
	second, err := PrepareGitGuard(state, next, dir, "", []string{"/bin/true"})
	if err != nil {
		t.Fatal(err)
	}
	if second.RefusalLog == first.RefusalLog || second.BinDir == first.BinDir {
		t.Fatalf("two attempts shared a guard: %#v %#v", first, second)
	}
	// The sentinel must not exist, in either. Its absence IS the mechanism.
	for _, guard := range []*GitGuard{first, second} {
		if _, err := os.Stat(guard.SentinelGitDir); err == nil {
			t.Fatalf("the brokered sentinel exists as a repository: %s", guard.SentinelGitDir)
		}
	}
}

// TestAGuardRefusesToPrepareWithoutRuntimeOwnedInputs keeps the boundary from
// being assembled out of whatever was lying around.
func TestAGuardRefusesToPrepareWithoutRuntimeOwnedInputs(t *testing.T) {
	state := t.TempDir()
	attempt := ExecutionAttemptRef{RunID: "r", OperationID: "r:execution.invoke:initial|1|b", Attempt: 1}
	if _, err := PrepareGitGuard(state, attempt, "/tmp/candidate", "", nil); err == nil {
		t.Fatal("a guard prepared with no broker command")
	}
	if _, err := PrepareGitGuard(state, attempt, "relative/candidate", "", []string{"/bin/true"}); err == nil {
		t.Fatal("a guard prepared against a relative candidate workspace")
	}
	if _, err := PrepareGitGuard(state, ExecutionAttemptRef{}, "/tmp/candidate", "", []string{"/bin/true"}); err == nil {
		t.Fatal("a guard prepared without an attempt identity")
	}
}

// ---------------------------------------------------------------------------
// E. Provider independence
// ---------------------------------------------------------------------------

// TestEveryNativeProviderRunsUnderTheBrokeredGitBoundary is #241 acceptance 12.
//
// The boundary is applied in CLIAgentProvider.Execute, which is the ONE place
// every native CLI invocation passes through, so this is a table over all four
// kinds rather than a Claude branch. A provider-specific guard would have left
// the next adapter unprotected by default, which is the opposite of what the
// issue asks for.
func TestEveryNativeProviderRunsUnderTheBrokeredGitBoundary(t *testing.T) {
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode, AgentKindGeminiCLI, AgentKindQwenCLI} {
		t.Run(kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, kind)
			provider.StateDir = t.TempDir()
			provider.GitBroker = []string{"/bin/true", "__git-broker"}

			result, err := provider.Execute(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if result.Invocation == nil || !result.Invocation.GitGuarded {
				t.Fatalf("%s ran without the brokered Git boundary: %#v", kind, result.Invocation)
			}
			// The WORKER'S OWN ENVIRONMENT carries it: the guard first on PATH
			// and the brokered sentinel. This is asserted on the recorded
			// invocation rather than on the provider struct, so it is the
			// environment the process actually received.
			call := fake.execution(t)
			var path, gitDir string
			for _, entry := range call.env {
				if strings.HasPrefix(entry, "PATH=") {
					path = strings.TrimPrefix(entry, "PATH=")
				}
				if strings.HasPrefix(entry, brokeredGitDirEnv+"=") {
					gitDir = strings.TrimPrefix(entry, brokeredGitDirEnv+"=")
				}
			}
			if !strings.Contains(filepath.ToSlash(path), "/"+gitGuardDirName+"/") {
				t.Fatalf("%s did not receive the guard on its search path: %q", kind, path)
			}
			if !strings.Contains(path, "bin") || strings.Index(path, string(os.PathListSeparator)) == 0 {
				t.Fatalf("%s search path is malformed: %q", kind, path)
			}
			if !strings.Contains(gitDir, brokeredGitDirName) {
				t.Fatalf("%s did not receive the brokered sentinel: %q", kind, gitDir)
			}
			// And no credential rode along with it - acceptance 13.
			for _, entry := range call.env {
				for _, forbidden := range []string{"GITHUB_TOKEN", "GH_TOKEN", "SSH_AUTH_SOCK", "GIT_ASKPASS", "GIT_SSH"} {
					if strings.HasPrefix(entry, forbidden+"=") {
						t.Fatalf("%s received %s", kind, forbidden)
					}
				}
			}
		})
	}
}

// TestAnUnguardedCompositionSaysSoRatherThanPretending: a composition that
// prepared no guard produced a worker that could discard candidate work, and
// that has to be a durable fact rather than an inference.
func TestAnUnguardedCompositionSaysSoRatherThanPretending(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	// No StateDir, no broker: the pre-#241 composition.
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Invocation == nil || result.Invocation.GitGuarded {
		t.Fatalf("an unguarded invocation claimed the boundary: %#v", result.Invocation)
	}
}

// TestTheBoundaryGrantsTheProviderNoNewCommandSurface is the #241 non-goal
// asserted as a law: the guard must not widen what a worker may run.
//
// The first draft added a standing `Bash(git *)` to Claude's allowlist so that
// the only Git it could invoke was the broker. Structurally true, and still
// wrong: this allowlist is derived from the invocation's own obligations, so a
// standing grant would have CREATED the capability the broker then guards - and
// it broke the existing law that a stage needing no tool is given none.
//
// The boundary therefore lives entirely in the search path and the environment,
// which is why it needs no provider's cooperation and why it is identical for
// all four adapters.
func TestTheBoundaryGrantsTheProviderNoNewCommandSurface(t *testing.T) {
	// An invocation with no obligations is granted nothing, exactly as before.
	if allowed := claudeAllowedTools(cliInvocation{}); len(allowed) != 0 {
		t.Fatalf("the guard granted an unobligated invocation a tool: %v", allowed)
	}
	// An obligated one is granted exactly its contract's tools, and no Git.
	allowed := strings.Join(claudeAllowedTools(cliInvocation{RequiredTools: []string{"go", "gofmt"}}), " ")
	for _, required := range []string{"Bash(go *)", "Bash(gofmt *)"} {
		if !strings.Contains(allowed, required) {
			t.Fatalf("a contract-required tool lost its grant: %s", allowed)
		}
	}
	if strings.Contains(allowed, "git") {
		t.Fatalf("the guard added a standing Git grant: %s", allowed)
	}
}

// TestTheRealControllerBinaryRefusesADestructiveProviderCommand is the whole
// chain, end to end, with nothing stood in for.
//
// It builds the actual controller, generates the actual shim against it, and
// runs `git checkout -- <file>` the way a provider would - through a shell,
// resolving by name, in the candidate workspace, under the worker environment.
// Everything in between is production code: the shim script, the hidden broker
// subcommand, the classifier, the refusal record and the diagnostic.
//
// The other tests in this file each prove one link. This proves there is no gap
// between them, which is the only way to know the boundary is real rather than
// well-tested in pieces.
func TestTheRealControllerBinaryRefusesADestructiveProviderCommand(t *testing.T) {
	requireGitFixture(t)
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go toolchain to build the controller with")
	}
	dir, _ := gitAuthorityFixture(t)
	const work = "package candidate\n\n// the expensive uncommitted implementation\nfunc Added() {}\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	// The real controller binary. Both it and the shim that forwards to it are
	// executed, so both live somewhere this environment will execute from.
	execDir := execCapableTempDir(t)
	binary := filepath.Join(execDir, "zenchron-engineering")
	// -buildvcs=false because the assurance sandbox masks /candidate/.git with
	// an empty tmpfs: Go sees a .git, asks Git about it, gets "not a git
	// repository" and refuses to build at all. Stamping the provenance of a
	// throwaway fixture binary was never the point, and without this the one
	// end-to-end proof of the boundary skips in the only environment whose
	// answer matters.
	build := exec.Command(goTool, "build", "-buildvcs=false", "-o", binary, "./cmd/zenchron-engineering")
	build.Dir = repositoryRootForTest(t)
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cannot build the controller here: %v\n%s", err, out)
	}

	guard, err := PrepareGitGuard(execCapableTempDir(t), ExecutionAttemptRef{
		RunID: "run-e2e", OperationID: "run-e2e:execution.invoke:initial|1|base", Attempt: 1,
	}, dir, "", []string{binary, "__git-broker"})
	if err != nil {
		t.Fatal(err)
	}

	// Exactly what a provider does: resolve git by name, in the workspace,
	// under the worker environment.
	cmd := exec.Command("/bin/sh", "-c", "git checkout -- implementation.go")
	cmd.Dir, cmd.Env = dir, workerEnvironment(guard)
	out, runErr := cmd.CombinedOutput()
	if runErr == nil {
		t.Fatalf("the real controller permitted a destructive checkout:\n%s", out)
	}
	// THE BYTES SURVIVED.
	if got := candidateFileBody(t, dir, "implementation.go"); got != work {
		t.Fatalf("the provider's uncommitted work was discarded:\n%q", got)
	}
	// The provider received the runtime's own diagnostic, not Git's.
	for _, required := range []string{"destructive Git refused", "implementation.go", "Zenchron owns candidate commits"} {
		if !strings.Contains(string(out), required) {
			t.Fatalf("the end-to-end diagnostic omits %q:\n%s", required, out)
		}
	}
	// And the refusal is durable evidence the adapter can read back.
	refusals, err := ReadGitRefusals(guard.RefusalLog)
	if err != nil || len(refusals) != 1 {
		t.Fatalf("the real broker recorded no refusal: %v %#v", err, refusals)
	}
	if refusals[0].DirtyCount != 1 || len(refusals[0].DirtyPaths) != 1 {
		t.Fatalf("the record does not name the work at stake: %#v", refusals[0])
	}

	// A READ-ONLY COMMAND STILL WORKS through the same chain, so the boundary
	// is a guard and not a wall.
	status := exec.Command("/bin/sh", "-c", "git status --porcelain")
	status.Dir, status.Env = dir, workerEnvironment(guard)
	statusOut, err := status.CombinedOutput()
	if err != nil {
		t.Fatalf("git status failed through the real broker: %v\n%s", err, statusOut)
	}
	if !strings.Contains(string(statusOut), "implementation.go") {
		t.Fatalf("git status did not report the candidate delta:\n%s", statusOut)
	}
}

// TestARefusalReachesStatusWithoutBecomingAFailure is #241 acceptance 6 and the
// observability requirement, asserted through a whole real run.
//
// The provider here does what the dogfood provider did: it writes candidate
// work, then reaches for a destructive recovery. It is refused, and then it
// finishes the work properly - so the invocation SUCCEEDS. That is the case
// that matters, because a refusal that only showed up on failures would be
// invisible exactly when the boundary did its job.
//
// The refusal must therefore appear in status as an observation, and must NOT
// appear as a provider quota, an unavailability, a stall, an assurance failure
// or an unknown - which #241 names explicitly.
func TestARefusalReachesStatusWithoutBecomingAFailure(t *testing.T) {
	fixture := newPhase8Fixture(t)
	deps := fixture.deps
	deps.Provider = &discardAttemptingProvider{base: fixture.provider}
	engine := fixture.newRuntime(deps)

	runID, err := engine.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 12; pass++ {
		if _, err := engine.Reconcile(context.Background(), runID); err != nil {
			t.Fatal(err)
		}
		report, err := engine.Status(runID)
		if err != nil {
			t.Fatal(err)
		}
		if report.CandidateDiscardRefusals == 0 {
			continue
		}
		// THE REFUSAL IS VISIBLE, and it says what it was.
		if !strings.Contains(report.CandidateDiscardRefused, "git") {
			t.Fatalf("status does not name the refused operation: %q", report.CandidateDiscardRefused)
		}
		if !strings.Contains(report.CandidateDiscardRefused, "preserved") {
			t.Fatalf("status does not say the work was preserved: %q", report.CandidateDiscardRefused)
		}
		// AND IT IS NOT A FAILURE. #241 names the classes it must never be
		// reported as; the runtime knew exactly what it refused, so none of
		// them apply.
		if d := report.ExecutionDiagnostic; d != nil {
			switch d.FailureClass {
			case FailureProviderQuota, FailureProviderRateLimited, FailureProviderUnavailable,
				FailureProviderAccountUnavailable, FailureProviderNoProgress, FailureUnknown:
				t.Fatalf("a runtime refusal was reported as %q", d.FailureClass)
			}
		}
		return
	}
	t.Fatal("the refusal never reached status")
}

// discardAttemptingProvider is the dogfood provider: it produces candidate
// work, reaches for a destructive recovery through the brokered boundary, is
// refused, and then completes normally.
type discardAttemptingProvider struct {
	base *isolatedProvider
}

func (p *discardAttemptingProvider) Isolation() ProviderIsolation {
	return p.base.Isolation()
}

func (p *discardAttemptingProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	result, err := p.base.Execute(ctx, request)
	if err != nil {
		return result, err
	}
	// The provider asks the runtime to discard its own dirty work. The guard is
	// the real one: the same BrokerGitCommand a shim would have reached.
	guard, prepareErr := PrepareGitGuard(p.stateDir(request), request.AttemptRef(), request.CandidateDir, request.ScratchDir, []string{"/unused"})
	if prepareErr != nil {
		return result, prepareErr
	}
	// FROM THE CANDIDATE, because that is where a worker stands when its shim
	// runs. Called in-process from wherever the test binary happens to be, the
	// broker classifies the TEST'S directory and records external_or_unknown -
	// still a refusal, so the provenance assertion downstream passes either
	// way, and passes just as well if candidate classification broke entirely.
	//
	// The broker moves its own working directory and does not move it back,
	// which is correct for the one-shot process it normally is and is not
	// correct for an in-process caller, so the restore is this caller's job.
	previous, wdErr := os.Getwd()
	if wdErr != nil {
		return result, wdErr
	}
	if chErr := os.Chdir(request.CandidateDir); chErr != nil {
		return result, chErr
	}
	_, brokerErr := BrokerGitCommand(request.CandidateDir, request.ScratchDir, guard.RefusalLog,
		[]string{"checkout", "--", "."}, io.Discard, io.Discard)
	if restoreErr := os.Chdir(previous); restoreErr != nil {
		return result, restoreErr
	}
	if brokerErr != nil {
		return result, brokerErr
	}
	refusals, readErr := ReadGitRefusals(guard.RefusalLog)
	if readErr != nil {
		return result, readErr
	}
	if len(refusals) == 0 {
		return result, fmt.Errorf("the brokered discard recorded no refusal")
	}
	// AND IT IS THE REFUSAL WE MEANT. Carrying "some refusal" into provenance
	// would be satisfied by one recorded against an unrelated directory.
	if refusals[0].Target != GitTargetCandidate {
		return result, fmt.Errorf("the discard was refused against %q, not the candidate", refusals[0].Target)
	}
	// The adapter's own job: carry what the boundary refused into provenance.
	if result.Invocation == nil {
		result.Invocation = &InvocationProvenance{AgentID: result.ProviderID}
	}
	result.Invocation.GitGuarded = true
	result.Invocation.GitRefusals = refusals
	return result, err
}

// stateDir keeps the guard outside the candidate workspace, as production does.
func (p *discardAttemptingProvider) stateDir(request ExecutionRequest) string {
	return filepath.Join(filepath.Dir(request.CandidateDir), "guard-state")
}

// TestALaterAttemptWithNoRefusalsClearsTheStaleObservation is the finding from
// review 5249945424, closed and proved through the REAL projection.
//
// The projection assigned the refusal fields only when there WERE refusals, so
// a second attempt that behaved perfectly left the first attempt's count on
// display. An operator would read "a destructive command was refused" about an
// attempt that never attempted one - worse than silence, because it is the
// runtime being confidently wrong about its own history.
//
// The projection is a view of the LATEST attempt, and "this attempt refused
// nothing" is as much a fact about it as any other. This drives Project over
// two real operation.after events, which is the fold production uses, so
// restoring the `if refusals > 0` guard fails it.
func TestALaterAttemptWithNoRefusalsClearsTheStaleObservation(t *testing.T) {
	refused, err := marshalPayloadJSON(executionRecord{
		mutationResult: mutationResult{
			Mutated: true, PathCount: 1, ProviderID: "codex", ProviderExecuted: true,
			DiscardRefusals: 1,
			DiscardRefused:  "git reset --hard (2 dirty candidate path(s) preserved)",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	clean, err := marshalPayloadJSON(executionRecord{
		mutationResult: mutationResult{
			Mutated: true, PathCount: 3, ProviderID: "codex", ProviderExecuted: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// ATTEMPT 1 REFUSED ONE COMMAND, and the projection says so.
	afterOne := executionAfterEvent(t, 1, Succeeded, refused)
	projection, err := Project([]EngineeringEvent{afterOne})
	if err != nil {
		t.Fatal(err)
	}
	if projection.CandidateDiscardRefusals != 1 ||
		!strings.Contains(projection.CandidateDiscardRefused, "reset --hard") {
		t.Fatalf("the first attempt's refusal was not projected: %d %q",
			projection.CandidateDiscardRefusals, projection.CandidateDiscardRefused)
	}

	// ATTEMPT 2 REFUSED NOTHING, and the projection must say THAT.
	afterTwo := executionAfterEvent(t, 2, Succeeded, clean)
	projection, err = Project([]EngineeringEvent{afterOne, afterTwo})
	if err != nil {
		t.Fatal(err)
	}
	if projection.CandidateDiscardRefusals != 0 || projection.CandidateDiscardRefused != "" {
		t.Fatalf("the second attempt left the first attempt's observation on display: %d %q",
			projection.CandidateDiscardRefusals, projection.CandidateDiscardRefused)
	}

	// And the order is the fold's, not the values': a refusal AFTER a clean
	// attempt is shown, so clearing is not the same as ignoring.
	projection, err = Project([]EngineeringEvent{afterTwo, executionAfterEvent(t, 3, Succeeded, refused)})
	if err != nil {
		t.Fatal(err)
	}
	if projection.CandidateDiscardRefusals != 1 {
		t.Fatalf("a later refusal was not projected: %d", projection.CandidateDiscardRefusals)
	}
}

// executionAfterEvent is one operation.after for an execution.invoke attempt,
// which is the event the projection folds.
func executionAfterEvent(t *testing.T, attempt int, state OperationState, result json.RawMessage) EngineeringEvent {
	t.Helper()
	payload, err := marshalPayloadJSON(RunOperation{
		SchemaVersion: SchemaVersion, ID: "op-invoke", RunID: "run-1",
		Kind: OpExecutionInvoke, IdempotencyKey: "invoke-1",
		State: state, Attempt: attempt, AttemptIdentity: attempt, MaxAttempts: 3,
		Result: result,
	})
	if err != nil {
		t.Fatal(err)
	}
	return EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: fmt.Sprintf("event-%d", attempt), RunID: "run-1",
		Sequence: int64(attempt), Type: EventOperationAfter, OperationID: "op-invoke",
		Payload: payload,
	}
}

// TestAnExecPathRedirectCannotReachTheCandidateRepository is CodeRabbit's
// finding on git_alias.go:84, closed and proved.
//
// ClassifyGitCommand permits verbs it does not recognize - deliberately, since
// this is a destruction guard and not a Git allowlist - and Git resolves an
// unknown verb by looking for `git-<verb>` on its exec path. So
// `git --exec-path=<provider dir> anything` was a straight escape: an unlisted
// verb, permitted, then executed with the brokered sentinel removed. The verb
// looking harmless is the point.
func TestAnExecPathRedirectCannotReachTheCandidateRepository(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	const work = "package candidate\n\n// work an exec-path redirect must not reach\n"
	writeCandidateFile(t, dir, "implementation.go", work)

	// A helper the provider controls, which does what the provider wanted all
	// along: discard the candidate work. The MUTATION at the end of this test
	// runs it, so it has to live somewhere executable - otherwise the mutation
	// cannot fire and the test says so, correctly, by failing.
	helpers := execCapableTempDir(t)
	helper := filepath.Join(helpers, "git-zap")
	script := "#!/bin/sh\nrm -f " + filepath.Join(dir, "implementation.go") + "\n"
	if err := os.WriteFile(helper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	// EACH ROW NAMES WHAT THE REFUSAL MUST OBJECT TO, rather than asserting one
	// sentence for all of them. Every refusal here used to be described as a
	// destructive discard, which for `git --exec-path=<dir> zap` is simply
	// false - it discards nothing, it redirects what Git runs - and a false
	// reason is one a provider will argue with. Since #248 the diagnostic says
	// which option or key was the objection; see git_authority.go.
	for name, tc := range map[string]struct {
		argv      []string
		objection string
	}{
		"exec-path equals":   {[]string{"--exec-path=" + helpers, "zap"}, "--exec-path"},
		"exec-path separate": {[]string{"--exec-path", helpers, "zap"}, "--exec-path"},
		// The same shape one layer along: configuration keys whose values are
		// programs Git executes. `core.pager` is not on the inert list and
		// never will be, and the refusal names the key.
		"inline config":    {[]string{"-c", "core.pager=" + helper, "log"}, "core.pager"},
		"config env":       {[]string{"--config-env", "core.pager=EVIL", "log"}, "core.pager"},
		"other repository": {[]string{"--git-dir=" + filepath.Join(dir, ".git"), "reset", "--hard"}, "--git-dir"},
		"other work tree":  {[]string{"--work-tree=/tmp", "checkout", "--", "."}, "--work-tree"},
		"namespace":        {[]string{"--namespace=x", "zap"}, "--namespace"},
	} {
		argv := tc.argv
		t.Run(name, func(t *testing.T) {
			code, diagnostic := brokerGit(t, dir, refusalLog, argv...)
			if code == 0 {
				t.Fatalf("%v was permitted", argv)
			}
			if !strings.Contains(diagnostic, "refused") || !strings.Contains(diagnostic, tc.objection) {
				t.Fatalf("%v was not refused by the boundary naming %q: %s", argv, tc.objection, diagnostic)
			}
			if got := candidateFileBody(t, dir, "implementation.go"); got != work {
				t.Fatalf("%v reached the candidate:\n%q", argv, got)
			}
		})
	}

	// MUTATION, inline: with the redirect permitted, the helper really does
	// run and really does erase the file.
	if _, err := execRealGit(dir, []string{"--exec-path=" + helpers, "zap"}, io.Discard, io.Discard); err != nil {
		t.Skipf("this git does not honour --exec-path for an unknown verb here: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "implementation.go")); err == nil {
		t.Fatal("an unguarded --exec-path redirect did NOT run the helper, so these refusals prove nothing")
	}
}

// TestOrdinaryGlobalOptionsStillWork keeps the refusal list from becoming a
// second wall. -C is deliberately not refused: it cannot change which program
// Git runs, and a destructive verb under it is still classified as one.
func TestOrdinaryGlobalOptionsStillWork(t *testing.T) {
	dir, refusalLog := gitAuthorityFixture(t)
	writeCandidateFile(t, dir, "implementation.go", "package candidate\n\n// dirty\n")
	for _, argv := range [][]string{
		{"-C", ".", "status", "--porcelain"},
		{"--no-pager", "log", "--oneline", "-1"},
		{"--literal-pathspecs", "diff", "--stat"},
		{"-C", ".", "--no-pager", "diff"},
	} {
		if code, out := brokerGit(t, dir, refusalLog, argv...); code != 0 {
			t.Fatalf("ordinary %v exited %d: %s", argv, code, out)
		}
	}
	if refusals, _ := ReadGitRefusals(refusalLog); len(refusals) != 0 {
		t.Fatalf("ordinary global options were refused: %#v", refusals)
	}
	// And -C with a destructive verb is still caught, so permitting -C is not
	// a hole.
	if code, _ := brokerGit(t, dir, refusalLog, "-C", ".", "reset", "--hard"); code == 0 {
		t.Fatal("-C hid a destructive verb")
	}
}

// TestAGuardThatCannotBeInstalledWaitsRatherThanStoppingTheRun is CodeRabbit's
// finding on git_guard.go:310, closed.
//
// A configured guard can fail to MATERIALIZE - a full disk, a read-only state
// directory, a shim that cannot be written. That error used to be returned raw,
// so it classified as FailureUnknown and routed to RouteStop: a repairable
// local condition terminalizing a run and destroying the work it was
// protecting, which is the opposite of what fail-closed is for.
func TestAGuardThatCannotBeInstalledWaitsRatherThanStoppingTheRun(t *testing.T) {
	candidate := t.TempDir()
	// A state root that cannot be written into, which is what a full or
	// read-only state directory looks like from here.
	state := filepath.Join(t.TempDir(), "unwritable")
	if err := os.WriteFile(state, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	provider := CLIAgentProvider{
		Agent:           ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted},
		StateDir:        state,
		GitBroker:       []string{"/unused", "__git-broker"},
		RequireGitGuard: true,
	}
	_, err := provider.prepareGitGuard(ExecutionRequest{
		RunID: "run-1", OperationID: "run-1:execution.invoke:initial|1|base", Attempt: 1,
		CandidateDir: candidate,
	})
	if err == nil {
		t.Fatal("an uninstallable guard reported success")
	}
	// IT IS THE RUNTIME'S OWN TYPED REFUSAL, so it routes as a repairable wait
	// rather than stopping the run.
	var refusal *CandidateGitGuardUnavailableError
	if !errors.As(err, &refusal) {
		t.Fatalf("a guard setup failure is not classified: %T %v", err, err)
	}
	if class, ok := candidateGuardFailureClass(err); !ok || class != FailureCandidateGuardUnavailable {
		t.Fatalf("the failure class is %q (%v)", class, ok)
	}
	if got := RouteFailure(FailureCandidateGuardUnavailable); got != RouteWait {
		t.Fatalf("a repairable controller setup failure routes to %q", got)
	}
	// The original failure is still reachable, because the repair depends on it.
	if refusal.Cause == nil {
		t.Fatal("the underlying failure was flattened away")
	}
	if !strings.Contains(err.Error(), "installing the brokered Git guard failed") {
		t.Fatalf("the refusal does not say what happened: %v", err)
	}
	// And it still fails closed: no guard was returned.
	if refusal.Broker != true {
		t.Fatalf("the refusal misreports which half was missing: %#v", refusal)
	}
}
