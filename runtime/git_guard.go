package runtime

// How the #241 decision reaches a provider process.
//
// The law is in git_authority.go. This file is the boundary that makes a
// provider's Git invocation arrive at it, and it is worth being exact about
// what that boundary does and does not establish, because #241 asks for
// precisely that honesty.
//
// TWO MECHANISMS, and the second is the one that matters:
//
//  1. A runtime-owned directory is placed FIRST on the worker's search path,
//     holding a `git` that is the broker. A bare `git ...` - which is what a
//     model types - resolves to it.
//
//  2. The worker's environment carries a BROKERED GIT_DIR pointing at a path
//     that is not a repository. With GIT_DIR set, Git performs no repository
//     discovery at all: it uses that value. So `/usr/bin/git reset --hard`,
//     `env git clean -fd`, `command git checkout -- f` and
//     `sh -c '/usr/bin/git restore .'` do not reach the candidate repository -
//     they fail, naming the brokered path in Git's own error. The shim is the
//     one caller that clears the sentinel, which is why the shim is the only
//     Git that works.
//
// (1) alone would be a convenience, not a boundary: shadowing a name on PATH
// is defeated by an absolute path, and #241 says not to claim otherwise. (2) is
// what turns the bypass into a failure rather than a bypass, because it does
// not depend on which binary runs or how it was spelled.
//
// WHAT IS STILL NOT CLAIMED. These workers are `operator_trusted`: they run
// under the operator's own account, so they can read what that account reads
// and write what it writes. A worker that deliberately sets out to defeat the
// runtime can `unset GIT_DIR`, or point it back at the candidate's own .git.
// No in-process mechanism closes that while the same account owns both sides -
// it is the residual this adapter already NAMES rather than relabels as
// proven, and it is why RequireProtectedIsolation refuses these adapters for
// protected work. What is closed here is the whole of the observed failure:
// a model reaching for habitual destructive recovery is refused, every
// spelling of it that does not deliberately dismantle the runtime's own
// environment fails closed, and the refusal becomes durable evidence.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// brokeredGitDirEnv is the variable the sentinel occupies. It is Git's own
// "use exactly this repository" variable, which is what makes discovery stop.
const brokeredGitDirEnv = "GIT_DIR"

// brokeredGitDirName is the sentinel's basename, and it is written to be read.
// An operator or a model that hits the boundary sees this path in Git's own
// error text, so the path itself says who refused and why.
const brokeredGitDirName = "zenchron-owns-candidate-git-use-git-on-PATH"

// gitGuardDirName is the guard directory inside the attempt's runtime-owned
// area. It is OUTSIDE the candidate workspace, like every other runtime-owned
// slot, so repository content can neither predict nor pre-seed it.
const gitGuardDirName = "git-guard"

// GitGuard is one invocation's brokered Git boundary.
type GitGuard struct {
	// BinDir holds the shim and is prepended to the worker's search path.
	BinDir string
	// RefusalLog is the runtime-owned record the broker appends to.
	RefusalLog string
	// SentinelGitDir is the non-repository path the worker's GIT_DIR names.
	SentinelGitDir string
}

// GitGuardDir is the runtime-owned guard directory for one attempt. It is
// derived from the attempt identity, so it is addressable after a crash and
// never shared between two physical invocations.
func GitGuardDir(stateDir string, attempt ExecutionAttemptRef) (string, error) {
	if err := attempt.Validate(); err != nil {
		return "", err
	}
	return filepath.Join(stateDir, gitGuardDirName, "runs",
		encodePathComponent(attempt.RunID),
		encodePathComponent(attempt.OperationID),
		fmt.Sprintf("attempt-%d", attempt.Attempt)), nil
}

// PrepareGitGuard creates the guard for one attempt.
//
// broker is the argv that runs the decision - the controller's own executable
// and its broker subcommand. It is a PARAMETER rather than something this
// function discovers, for the same reason the operator home is: the
// composition root chose which binary is the controller, and a boundary that
// resolved its own enforcer from the environment would be enforcing with
// whatever the environment supplied.
func PrepareGitGuard(stateDir string, attempt ExecutionAttemptRef, candidateDir string, broker []string) (*GitGuard, error) {
	if len(broker) == 0 || strings.TrimSpace(broker[0]) == "" {
		return nil, fmt.Errorf("a brokered Git guard requires the controller's own broker command")
	}
	if !filepath.IsAbs(candidateDir) {
		return nil, fmt.Errorf("a brokered Git guard requires an absolute candidate workspace")
	}
	dir, err := GitGuardDir(stateDir, attempt)
	if err != nil {
		return nil, err
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		return nil, err
	}
	guard := &GitGuard{
		BinDir:         bin,
		RefusalLog:     filepath.Join(dir, gitRefusalLogName),
		SentinelGitDir: filepath.Join(dir, brokeredGitDirName),
	}
	// THE RECORD STARTS EMPTY for this physical attempt. It is append-only
	// within the invocation and truncated between them, so a refusal read back
	// afterwards belongs to the attempt that is being explained and not to its
	// predecessor. The attempt identity is already in the path; this is what
	// makes a re-run of the SAME identity - a crash before settlement -
	// self-consistent rather than cumulative.
	if err := os.Remove(guard.RefusalLog); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	// The sentinel must NOT exist. Its absence is the mechanism: Git resolves
	// GIT_DIR without discovery and fails when it is not a repository.
	if err := os.RemoveAll(guard.SentinelGitDir); err != nil {
		return nil, err
	}
	if err := writeGitShim(filepath.Join(bin, "git"), candidateDir, guard.RefusalLog, broker); err != nil {
		return nil, err
	}
	return guard, nil
}

// writeGitShim writes the executable a bare `git` resolves to.
//
// It is a two-line shell script and nothing more, because everything it could
// decide is already decided in Go: it forwards the argv to the broker with the
// candidate workspace and the record it must write. Putting any classification
// here would put part of the law in a file no test reads.
func writeGitShim(path, candidateDir, refusalLog string, broker []string) error {
	quoted := make([]string, 0, len(broker)+4)
	for _, part := range broker {
		quoted = append(quoted, shellSingleQuoted(part))
	}
	quoted = append(quoted,
		"--candidate", shellSingleQuoted(candidateDir),
		"--refusal-log", shellSingleQuoted(refusalLog), "--")
	script := "#!/bin/sh\n" +
		"# Runtime-owned. Zenchron brokers candidate Git; see issue #241.\n" +
		"exec " + strings.Join(quoted, " ") + " \"$@\"\n"
	return os.WriteFile(path, []byte(script), 0o700)
}

// shellSingleQuoted makes one string a literal shell word. Every value that
// reaches it is runtime-owned - an executable path, the candidate directory,
// the record path - and quoting them anyway is what keeps that true of the
// generated script rather than of the caller's memory.
func shellSingleQuoted(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// Env is the environment the worker receives for Git. It is the sentinel, and
// it is what makes an unshimmed invocation fail instead of succeeding.
func (g *GitGuard) Env() []string {
	if g == nil {
		return nil
	}
	return []string{brokeredGitDirEnv + "=" + g.SentinelGitDir}
}

// SearchPath prepends the guard to a worker search path, so a bare `git`
// resolves to the broker and everything else resolves exactly as before.
func (g *GitGuard) SearchPath(path string) string {
	if g == nil {
		return path
	}
	if strings.TrimSpace(path) == "" {
		return g.BinDir
	}
	return g.BinDir + string(os.PathListSeparator) + path
}

// withoutBrokeredGitDir strips the sentinel from an environment.
//
// The broker is the ONE caller that must see past it: it has already decided
// the command is permitted, and the real Git it then runs has to discover the
// candidate repository normally. Nothing else in the worker's process tree
// clears it.
func withoutBrokeredGitDir(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, brokeredGitDirEnv+"=") {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// ReadGitRefusals reads the refusals one attempt's provider produced.
//
// An absent file is no refusals and not an error: the overwhelmingly common
// case is a provider that never reached for a destructive command, and that
// must not look like a failure to read evidence.
func ReadGitRefusals(path string) ([]GitRefusal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var refusals []GitRefusal
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var refusal GitRefusal
		if decodeJSON([]byte(line), &refusal) != nil {
			// A malformed line is dropped rather than failing the read. The
			// record is an observation about the invocation; an unreadable one
			// is worth less than the rest of them are, and nothing about the
			// candidate depends on it.
			continue
		}
		refusals = append(refusals, refusal)
	}
	return refusals, nil
}

// prepareGitGuard materializes the boundary for one invocation, or reports
// that this composition prepared none.
//
// It returns (nil, nil) when the composition root supplied no state root or no
// broker command. That is an honest absence rather than a silent one: the
// invocation proceeds exactly as it did before #241 and the provenance says
// GitGuarded=false, so an unguarded worker is a durable fact an operator can
// read instead of an assumption they have to make.
func (p CLIAgentProvider) prepareGitGuard(request ExecutionRequest) (*GitGuard, error) {
	if strings.TrimSpace(p.StateDir) == "" || len(p.GitBroker) == 0 {
		return nil, nil
	}
	return PrepareGitGuard(p.StateDir, request.AttemptRef(), request.CandidateDir, p.GitBroker)
}

// providerGitRefusals is the refusals one invocation's provenance recorded.
func providerGitRefusals(result ExecutionResult) []GitRefusal {
	if result.Invocation == nil {
		return nil
	}
	return result.Invocation.GitRefusals
}

// lastProviderGitRefusal renders the most recent refusal for durable operation
// state and for status.
//
// The MOST RECENT rather than the first: a provider that was refused three
// times was most recently doing whatever it decided to do after the second
// refusal, and that is the one an operator is reading the run to understand.
func lastProviderGitRefusal(result ExecutionResult) string {
	refusals := providerGitRefusals(result)
	if len(refusals) == 0 {
		return ""
	}
	last := refusals[len(refusals)-1]
	rendered := last.Operation
	if last.DirtyCount > 0 {
		rendered += fmt.Sprintf(" (%d dirty candidate path(s) preserved)", last.DirtyCount)
	}
	return boundedDetail(rendered)
}

// CandidateGitGuardUnavailableError is the typed refusal for a composition that
// requires the #241 boundary and could not install it.
//
// It names which half was missing, because the two have different repairs: no
// state root is a misconfigured composition, and no broker command is a
// controller that could not resolve its own executable. Neither is a provider
// fault, and the failure class it carries says so.
type CandidateGitGuardUnavailableError struct {
	AgentID  string
	StateDir string
	Broker   bool
}

func (e *CandidateGitGuardUnavailableError) Error() string {
	missing := "the controller could not resolve its own brokered Git executable"
	if strings.TrimSpace(e.StateDir) == "" {
		missing = "no runtime state root was configured for the brokered Git guard"
	}
	return "refusing to dispatch agent " + e.AgentID +
		": candidate Git must be brokered and " + missing +
		". No provider was invoked and the candidate workspace was not touched"
}

// candidateGuardFailureClass maps a pre-dispatch guard refusal onto its typed
// class, and reports whether the error was one.
//
// It exists so the classification lives beside the error rather than in the
// execution handler: the handler asks one question and gets the runtime's own
// answer, instead of a provider refusal falling through to FailureUnknown and
// being reported as though the worker had done something wrong.
func candidateGuardFailureClass(err error) (FailureClass, bool) {
	var guard *CandidateGitGuardUnavailableError
	if errors.As(err, &guard) {
		return FailureCandidateGuardUnavailable, true
	}
	return "", false
}
