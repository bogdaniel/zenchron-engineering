package runtime

// A provider may mutate candidate work. It may not destructively discard it.
//
// #241 is the economics of that distinction. During live dogfood a coding CLI
// twice reached for habitual recovery - `git checkout -- <path>` against a
// dirty runtime-owned candidate workspace - and erased its own uncommitted
// implementation edits. No governed candidate commit was lost, because the
// runtime owns those; what was lost was the expensive part, the reasoning that
// had already been performed, and the provider then spent more subscription
// capacity reconstructing work it had already done.
//
// The law this file states:
//
//	a provider may create and modify candidate work,
//	and it has no authority to destructively discard it.
//
// Provider convenience is not discard authority. This is the same shape as
// every other boundary in this runtime - capability is not permission, and
// permission is not authority - applied to the one operation whose whole
// purpose is to throw uncommitted work away.
//
// WHAT THIS FILE IS, AND WHAT IT IS NOT. It is the classification and the
// decision: given an argv a provider asked to run, is this an operation whose
// purpose is to discard uncommitted worktree state? It is deliberately NOT a
// general Git allowlist and NOT a shell policy. A provider that wants to read,
// diff, search, log, add or apply is doing ordinary engineering and is passed
// through untouched. Only the discard family is refused, because only the
// discard family destroys reasoning that has already been paid for.
//
// The delivery of this decision to a provider process - the runtime-owned
// search path, the shim, the brokered environment - is git_guard.go. The two
// are separate on purpose: the law is testable without a process, and the
// boundary is testable without a model.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// GitOperationClass is what the runtime decided about one provider Git argv.
type GitOperationClass string

const (
	// GitOperationPermitted is ordinary engineering Git. It is executed.
	//
	// It is the DEFAULT, and that is a deliberate choice rather than an
	// oversight. This guard exists to stop destruction, not to enumerate every
	// legitimate Git command a worker might need; a closed allowlist would
	// have made the runtime the author of the provider's whole engineering
	// vocabulary, which is the "generic shell command policy" #241 excludes.
	GitOperationPermitted GitOperationClass = "permitted"
	// GitOperationDiscard is an operation whose purpose or effect is to
	// discard uncommitted candidate state. It is refused.
	GitOperationDiscard GitOperationClass = "discard"
	// GitOperationRuntimeOwned is a command that would take over something the
	// RUNTIME owns. It destroys nothing by itself; it is refused because the
	// runtime cannot let it succeed. See the commit arm of ClassifyGitCommand.
	GitOperationRuntimeOwned GitOperationClass = "runtime_owned"
)

// ClassifyGitCommand decides what a provider's Git argv is, from the argv
// alone. args is the vector AFTER the program name.
//
// It is pure, so the taxonomy is a table a reader can check rather than
// behaviour that has to be reproduced. The taxonomy is conservative by
// construction: where a verb has both destructive and harmless forms and
// telling them apart needs to know what is in the repository, the verb is
// classified as discard and the provider is told to ask the runtime. Refusing
// a harmless `git checkout -b` costs a provider one sentence of diagnostic;
// admitting one destructive form costs the operator the reasoning it erases.
func ClassifyGitCommand(args []string) (GitOperationClass, string) {
	verb, rest := gitVerb(args)
	switch verb {
	// THE WORKTREE-REPLACING FAMILY. Every one of these exists to make the
	// working tree match something else, and "something else" is by definition
	// not the uncommitted work. `checkout` and `switch` are included in full,
	// including their branch-creating forms: the runtime owns candidate
	// branches and commits, so a provider has nothing to gain from moving HEAD
	// and everything to lose from a form that turned out to carry a pathspec.
	case "checkout", "switch", "restore", "checkout-index", "sparse-checkout", "worktree":
		return GitOperationDiscard, "would replace working-tree state from another revision"
	// `clean` has exactly one purpose.
	case "clean":
		return GitOperationDiscard, "would delete untracked candidate files"
	case "reset":
		// --soft and the default --mixed move the index and HEAD and leave the
		// working tree alone, so they are ordinary. --hard overwrites it and
		// --merge may discard from it.
		if hasAnyFlag(rest, "--hard", "--merge") {
			return GitOperationDiscard, "would overwrite working-tree state from another revision"
		}
	case "stash":
		// Stashing REMOVES the work from the working tree. It is recoverable
		// in principle and routinely unrecoverable in practice: a model that
		// stashes to "clean up" and then reasons on from a clean tree has lost
		// the work from its own view just as completely as if it had deleted
		// it, which is the #241 failure mode exactly. Restoring forms are
		// ordinary.
		switch stashSubcommand(rest) {
		case "push", "save", "clear", "drop", "create", "store":
			return GitOperationDiscard, "would remove uncommitted candidate work from the working tree"
		}
	case "rm":
		// --cached only rewrites the index. A forced removal deletes the file,
		// including its uncommitted modifications; git refuses the unforced
		// form on a modified file by itself.
		if hasAnyFlag(rest, "-f", "--force") && !hasAnyFlag(rest, "--cached") {
			return GitOperationDiscard, "would force-delete candidate files"
		}
	case "read-tree":
		if hasAnyFlag(rest, "-u", "--reset") {
			return GitOperationDiscard, "would overwrite working-tree state from another tree"
		}
	case "submodule":
		if len(rest) > 0 && rest[0] == "deinit" {
			return GitOperationDiscard, "would remove a submodule working tree"
		}
	// COMMITTING IS THE RUNTIME'S, and until #248 nothing said so here.
	//
	// It was unreachable rather than permitted: every `git commit` a provider
	// sent arrived with a `-c` prefix and died on the blanket config refusal,
	// which is why 58 of one attempt's 73 refusals were commits. Accepting
	// inert `-c` keys makes it reachable for the first time, so the arm that
	// was always missing has to exist before that lands.
	//
	// A provider commit is not harmless. It moves HEAD, gitMetadataDigest
	// covers HEAD, so the next AssertIntegrity reads the provider's own commit
	// as tampering - and workspace_integrity_violation routes to RouteRestore,
	// which is `reset --hard` plus `clean -fdx`. The provider would destroy the
	// candidate by committing it, which is the #241 outcome reached through the
	// one verb #241 does not classify.
	//
	// It is deliberately NOT GitOperationDiscard. Nothing is being discarded
	// and saying so would be false; what is true is that the runtime owns this
	// and the work is safe where it is.
	case "commit", "commit-tree":
		return GitOperationRuntimeOwned, "creating a candidate commit is the runtime's, not the provider's"
	}
	return GitOperationPermitted, ""
}

// gitVerb finds the subcommand, skipping Git's global options.
//
// It has to skip them properly rather than reading args[0]: `git -C dir
// checkout -- f` and `git --git-dir=x reset --hard` are the same operations as
// their bare forms, and a classifier that only looked at the first element
// would pass both straight through. That is not a hypothetical - prefixing a
// global option is exactly what a worker does when a bare invocation has just
// failed.
func gitVerb(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return arg, args[i+1:]
		}
		// Options that consume the NEXT element. A self-contained `--x=y` form
		// needs no skip, and neither do the boolean globals.
		switch arg {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path",
			"--super-prefix", "--config-env", "--attr-source":
			i++
		}
	}
	return "", nil
}

// hasAnyFlag reports whether any of the named flags appears, stopping at the
// pathspec separator so a FILE called "--hard" cannot be read as a flag.
//
// Short flags are also matched inside a combined cluster, because `-fd` and
// `-f -d` are the same request and `git clean -fdx` is the spelling that
// actually appears.
func hasAnyFlag(args []string, flags ...string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		for _, flag := range flags {
			if arg == flag {
				return true
			}
			if len(flag) == 2 && flag[0] == '-' && flag[1] != '-' &&
				strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") &&
				strings.ContainsRune(arg[1:], rune(flag[1])) {
				return true
			}
		}
	}
	return false
}

// stashSubcommand resolves which stash operation this is. A bare `git stash`
// is `push`, which is the destructive default and the one a worker types.
func stashSubcommand(rest []string) string {
	for _, arg := range rest {
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			return arg
		}
	}
	return "push"
}

// GitRefusal is the durable record of one refused provider Git operation.
//
// It is written by the broker, inside the provider's process tree, into a
// runtime-owned file the provider is not told the purpose of - the same
// unspoofable-slot pattern the reviewer verdict uses. The adapter reads it
// after the invocation so the refusal becomes attempt evidence rather than a
// line of stderr the provider could have paraphrased.
type GitRefusal struct {
	// Operation is the bounded, redacted argv the provider asked for.
	Operation string `json:"operation"`
	// Reason is the runtime's own words for what it refused.
	Reason string `json:"reason"`
	// DirtyPaths is the bounded set of candidate paths that would have been
	// discarded, or empty when the workspace held no delta. It is OBSERVATION
	// for the operator and for the provider's next decision; it is not what
	// the refusal was based on. See BrokerGitCommand.
	DirtyPaths []string `json:"dirty_paths,omitempty"`
	// DirtyCount is the true total, which DirtyPaths may have been bounded
	// below. A record that showed three paths without saying there were forty
	// would understate what was at stake.
	DirtyCount int `json:"dirty_count,omitempty"`
}

// maxRefusalDirtyPaths bounds what one refusal record names. The count is
// always exact; the list is a sample, because a provider that dirtied a
// thousand files must not be able to grow a durable record by that much.
const maxRefusalDirtyPaths = 16

// CandidateDirtyPaths reports the candidate paths that carry uncommitted work:
// tracked modifications and untracked files alike.
//
// UNTRACKED IS NOT THE SAME AS WORTHLESS, and the distinction #241 draws is
// attribution rather than tracking. A file the provider has just written is the
// most expensive state in the workspace precisely because nothing has committed
// it yet, and `git clean -fd` is the command that deletes exactly those. So
// --untracked-files=all is used, and .gitignore is NOT read as authority to
// destroy: --ignored is deliberately absent, so ignored paths are not reported
// as candidate work, but nor is an ignored path ever an argument FOR a discard.
//
// Runtime scratch is outside the candidate workspace by construction - see
// ExecutionScratchDir - so it cannot appear here and the existing separation
// between candidate work and runtime scratch is preserved rather than
// re-derived.
func CandidateDirtyPaths(dir string) ([]string, error) {
	out, err := GitRunner{Dir: dir}.run("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		// Porcelain v1: two status columns, a space, then the path. A rename
		// carries "old -> new"; the NEW path is the one that exists.
		path := strings.TrimSpace(line[3:])
		if arrow := strings.Index(path, " -> "); arrow >= 0 {
			path = path[arrow+4:]
		}
		if path = strings.Trim(strings.TrimSpace(path), `"`); path != "" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// GitDiscardRefusedError is the typed refusal a provider receives.
type GitDiscardRefusedError struct {
	Operation  string
	Reason     string
	Dirty      []string
	DirtyTotal int
}

func (e *GitDiscardRefusedError) Error() string {
	// The message is written FOR THE MODEL, because the model is the only
	// reader who can act on it, and a refusal a worker cannot act on just
	// becomes a retry of the same command. So it says what was refused, why,
	// what would have been lost, and what to do instead.
	message := "destructive Git refused: " + e.Operation +
		" would discard dirty runtime-owned candidate work (" + e.Reason + ")."
	if e.DirtyTotal > 0 {
		message += fmt.Sprintf(" %d uncommitted candidate path(s) would have been lost", e.DirtyTotal)
		if len(e.Dirty) > 0 {
			message += ": " + strings.Join(e.Dirty, ", ")
		}
		message += "."
	}
	return message + " Zenchron owns candidate commits and candidate discard;" +
		" keep working from the current files, or edit them back yourself." +
		" Read-only Git (status, diff, log, show) is unaffected."
}

// GitRuntimeOwnedRefusedError is the refusal for a command the runtime owns.
//
// It exists so the provider is told the one thing that ends the loop: the work
// is already safe and the runtime will commit it. #248's worker sent the same
// commit 58 times because every answer it got described a problem with its own
// invocation, which is a problem a model will keep trying to solve. A refusal
// that names the permitted next action preserves exactly the same authority and
// costs the run one command instead of its whole budget.
type GitRuntimeOwnedRefusedError struct{ Operation, Reason string }

func (e *GitRuntimeOwnedRefusedError) Error() string {
	return "runtime-owned Git refused: " + e.Operation + " (" + e.Reason + ")." +
		" Do not retry this command and do not create commits, branches or tags:" +
		" Zenchron commits the candidate itself from your working tree." +
		" Leave your changes as edited files and continue with the rest of the task." +
		" Read-only Git (status, diff, log, show) is unaffected."
}

// BrokerGitCommand is the decision, and it is the whole enforcement point.
//
// It classifies, records a refusal durably where one is needed, and otherwise
// executes the real Git command. It returns the command's exit status so the
// caller can be a faithful stand-in for git.
//
// THE REFUSAL DOES NOT DEPEND ON THE WORKSPACE BEING DIRTY, and that is the
// deliberate answer to #241's TOCTOU question. An implementation that checked
// dirtiness and then executed would have exactly the window the issue warns
// about - observe clean, provider writes a file, destructive command runs, work
// gone - and no amount of re-checking closes it while the provider's own
// process is free to write between the two. So the decision is made from the
// argv alone, which is immutable, and the answer for a clean workspace is the
// same as for a dirty one: refused.
//
// That is the smallest coherent law, and it is stronger than the alternative
// rather than weaker. A destructive command against a genuinely clean
// workspace is a no-op, so refusing it takes nothing from the provider; and
// because the answer never depends on mutable state, there is no race to win,
// no check to defeat, and nothing for the clean case to leak into the dirty
// one. Dirtiness is still OBSERVED, because the diagnostic and the durable
// record are worth more when they say what was at stake - but it is observed
// after the decision and it cannot change it.
func BrokerGitCommand(candidateDir, refusalLog string, args []string, stdout, stderr io.Writer) (int, error) {
	// THE EFFECTIVE COMMAND FIRST, because the classifier has to be looking at
	// what real Git will run. An alias expands inside Git, after the broker
	// would otherwise have authorized the verb it was spelled with - and both a
	// provider and a checked-out .git/config can define one, since `config` is
	// an ordinary command. See git_alias.go.
	effective, resolveErr := ResolveGitCommand(candidateDir, args)
	if resolveErr != nil {
		// FAIL CLOSED. "I could not tell what this would do" is not "this is
		// safe", and the boundary must never answer the second when it means
		// the first.
		// The underlying refusal already says what to do about itself - a
		// configuration override names its key and how to proceed without it -
		// so the provider is given THAT, while the durable record keeps the
		// resolution prefix a reviewer reads it by.
		return refuseGitCommand(candidateDir, refusalLog, args,
			"the effective operation could not be resolved: "+resolveErr.Error(),
			GitOperationClass(""), stderr)
	}
	class, reason := ClassifyGitCommand(effective)
	if class == GitOperationPermitted {
		// The RESOLVED form is executed. Where no alias was involved it is the
		// original argv unchanged, which is almost every invocation; where one
		// was, running the expansion the broker actually classified is what
		// stops the lookup and the execution from being able to disagree.
		return execRealGit(candidateDir, effective, stdout, stderr)
	}
	if aliased := boundedGitArgv(args); aliased != boundedGitArgv(effective) {
		// The record names BOTH: what the provider asked for and what it
		// resolved to. An operator reading `git co` needs to be told it was a
		// checkout, and a reader of `git checkout` needs to know nobody typed
		// that.
		reason += " (requested as " + aliased + ")"
	}
	return refuseGitCommand(candidateDir, refusalLog, effective, reason, class, stderr)
}

// refuseGitCommand records the refusal durably and tells the provider why.
//
// It is one function because every refusal has to do all of it: a refusal that
// executed nothing but recorded nothing would be invisible, and one that
// recorded without explaining would leave the worker to guess.
func refuseGitCommand(candidateDir, refusalLog string, args []string, reason string, class GitOperationClass, stderr io.Writer) (int, error) {
	refusal := GitRefusal{Operation: boundedGitArgv(args), Reason: reason}
	// Observation, after the decision. A workspace whose status cannot be read
	// does not soften the refusal; it just means the record names no paths.
	if dirty, err := CandidateDirtyPaths(candidateDir); err == nil {
		refusal.DirtyCount = len(dirty)
		if len(dirty) > maxRefusalDirtyPaths {
			dirty = dirty[:maxRefusalDirtyPaths]
		}
		refusal.DirtyPaths = dirty
	}
	if err := appendGitRefusal(refusalLog, refusal); err != nil {
		// The record is evidence, and failing to write it must not turn a
		// refusal into an execution. The provider is still refused.
		fmt.Fprintln(stderr, "zenchron: recording the refusal failed:", err)
	}
	// THE MESSAGE MATCHES THE REASON. Every refusal used to be described as a
	// destructive discard, so a provider refused an inert `-c log` was told its
	// read would lose uncommitted work - which is false, unactionable, and
	// exactly the shape that produced #248's retry loop.
	var diagnostic string
	switch class {
	case GitOperationRuntimeOwned:
		diagnostic = (&GitRuntimeOwnedRefusedError{Operation: refusal.Operation, Reason: reason}).Error()
	case GitOperationDiscard:
		diagnostic = (&GitDiscardRefusedError{
			Operation: refusal.Operation, Reason: reason,
			Dirty: refusal.DirtyPaths, DirtyTotal: refusal.DirtyCount,
		}).Error()
	default:
		// A command whose meaning could not be established. The reason carries
		// the underlying refusal's own words, which already name what to do.
		diagnostic = "Git refused: " + refusal.Operation + ": " + reason
	}
	fmt.Fprintln(stderr, diagnostic)
	// Git's own exit status for a command it would not perform.
	return 1, nil
}

// aliasResolutionReason is recognizable in a refusal record, so a reviewer can
// tell a classified discard from a command whose meaning could not be
// established. Both are refusals; they are not the same fact.
const aliasResolutionReason = "the effective operation could not be resolved"

// execRealGit runs a permitted command through the real binary.
//
// It resolves Git from the runtime's own trusted search path rather than from
// whatever PATH the provider is running under - the shim is FIRST on that path,
// so resolving by name here would make the broker invoke itself forever.
func execRealGit(candidateDir string, args []string, stdout, stderr io.Writer) (int, error) {
	binary, err := gitBinary()
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = candidateDir
	// The provider's own environment is inherited so that a permitted command
	// behaves exactly as it would have without the guard, MINUS the brokered
	// sentinel: the sentinel exists to make unshimmed Git fail closed, and the
	// shim is the one caller that must see past it. See git_guard.go.
	cmd.Env = withoutBrokeredGitDir(os.Environ())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// boundedGitArgv renders the argv for a durable record.
//
// Only the SHAPE of the command is recorded: the verb and its option flags,
// with operands replaced by a count. An operand is a path or a revision the
// provider chose, so it is untrusted text, and a durable row is not the place
// for it - the classification is what the refusal was about.
func boundedGitArgv(args []string) string {
	// The VERB is resolved the same way the classifier resolves it, so a
	// command prefixed with a global option renders as the operation it is
	// rather than as its first flag. A record that called `-C . reset --hard`
	// a "-C" would describe the spelling and not the decision.
	verb, rest := gitVerb(args)
	rendered := make([]string, 0, len(rest)+2)
	if verb != "" {
		rendered = append(rendered, boundedDetail(verb))
	}
	operands := 0
	for _, arg := range rest {
		if arg == "--" || (strings.HasPrefix(arg, "-") && arg != "-") {
			rendered = append(rendered, boundedDetail(arg))
			continue
		}
		operands++
	}
	out := "git " + strings.Join(rendered, " ")
	if operands > 0 {
		out += fmt.Sprintf(" <%d operand(s)>", operands)
	}
	return strings.TrimSpace(out)
}

// gitRefusalLogName is the runtime-owned record inside the guard directory.
const gitRefusalLogName = "refused.jsonl"

// appendGitRefusal appends one record. It is append-only: a second refusal in
// the same invocation must not overwrite the first, because a provider that
// tried three destructive recoveries is a different fact from one that tried
// one.
func appendGitRefusal(path string, refusal GitRefusal) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	line, err := marshalPayloadJSON(refusal)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(append(line, '\n'))
	return err
}
