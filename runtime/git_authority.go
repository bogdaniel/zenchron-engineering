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
	"bytes"
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
	// REFS ARE THE RUNTIME'S TOO. The trusted provider instructions already say
	// so - "Do not create commits, branches, tags, remotes, or any other Git
	// metadata" - and until now only the commit half was enforced, so the
	// refusal was promising something the classifier did not deliver.
	//
	// `checkout -b` and `switch -c` are already refused as worktree-replacing,
	// which left the direct spellings as the way around a law that was being
	// stated anyway.
	//
	// The READING forms stay permitted, and that distinction is the whole
	// point of #248: `git branch --show-current` and `git tag -l` answer
	// questions a worker legitimately has, and refusing them would rebuild the
	// trap this change exists to remove.
	case "branch":
		if namesANewRef(rest, branchRefSpec) {
			return GitOperationRuntimeOwned, "candidate branches are the runtime's, not the provider's"
		}
	case "tag":
		if namesANewRef(rest, tagRefSpec) {
			return GitOperationRuntimeOwned, "candidate tags are the runtime's, not the provider's"
		}
	// update-ref has no reading form at all: every invocation writes or deletes
	// a ref, so there is no distinction to draw.
	case "update-ref":
		return GitOperationRuntimeOwned, "candidate refs are the runtime's, not the provider's"
	// symbolic-ref has both. One operand ASKS what a symbolic ref points at -
	// `git symbolic-ref --short HEAD` is how a worker finds its branch without
	// `git branch`. Two operands REPOINT it, and --delete removes it.
	case "symbolic-ref":
		if hasAnyFlag(rest, "-d", "--delete") || refOperands(rest, symbolicRefReadValue) >= 2 {
			return GitOperationRuntimeOwned, "candidate refs are the runtime's, not the provider's"
		}
	}
	return GitOperationPermitted, ""
}

// refVerbSpec is one ref verb's flag vocabulary, split by what a flag MEANS
// for this question. It is a table rather than a chain of conditions because
// the distinction it draws - between an operand that is a ref's name and an
// operand that is a pattern or a revision - is exactly where a guess becomes a
// refused read.
type refVerbSpec struct {
	// writing flags settle the question on their own.
	writing map[string]bool
	// writingShort are the same flags inside a short cluster: `-aD` is a delete.
	writingShort string
	// listing flags turn the invocation into a query, so whatever operands
	// follow are patterns, revisions or sort keys rather than names.
	//
	// EVERY MEMBER IS THERE BECAUSE GIT WAS ASKED. `git branch <flag> name` was
	// run against real Git for each one and observed not to create
	// refs/heads/name; the flags that DID create it are deliberately absent.
	// Membership here is permissive - it stops operands being read as names -
	// so it is the direction that has to be evidence and not inference.
	listing map[string]bool
	// readValue flags take a SEPARATE mandatory value that must not be read as
	// a name, for a verb that has no listing mode to fall back on.
	readValue map[string]bool
	// numericListing marks a verb whose `-n[<num>]` implies --list, which is
	// `git tag` and only `git tag`.
	numericListing bool
}

// namesANewRef reports whether a ref verb would CREATE or CHANGE a ref rather
// than report on one.
//
// It is argv-only, like everything else here. A writing flag settles it; so
// does a bare operand, once the flags that legitimately carry a value have been
// consumed and once a listing flag has been given the chance to say that the
// operands are patterns.
//
// THE JOINED SPELLING IS THE SAME FLAG. `--set-upstream-to=origin/main` and
// `--set-upstream-to origin/main` are one option, and an earlier version of
// this function skipped the joined form as "a flag with a value" before asking
// whether that flag writes - so the spelling with an `=` in it walked past the
// check the spelling without one failed.
func namesANewRef(rest []string, spec refVerbSpec) bool {
	listing, operands := false, 0
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg == "--" {
			// Everything after the separator is an operand.
			operands += len(rest) - i - 1
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			operands++
			continue
		}
		name, joined := arg, false
		if before, _, found := strings.Cut(arg, "="); found {
			name, joined = before, true
		}
		if spec.writing[name] {
			return true
		}
		if isShortCluster(arg) && strings.ContainsAny(arg[1:], spec.writingShort) {
			return true
		}
		if spec.listing[name] || (spec.numericListing && isNumericListing(name)) {
			listing = true
			continue
		}
		if !joined && spec.readValue[name] && i+1 < len(rest) {
			i++
		}
	}
	// A LISTING FORM'S OPERANDS ARE NOT NAMES. `git branch --list "feature/*"`
	// and `git tag --verify v1` are questions, and refusing them would rebuild
	// the #248 trap one verb along.
	if listing {
		return false
	}
	return operands > 0
}

// isShortCluster reports whether an argument is Git's `-abc` form, where every
// character after the dash is its own single-letter flag.
func isShortCluster(arg string) bool {
	return len(arg) > 1 && arg[0] == '-' && arg[1] != '-'
}

// isNumericListing recognizes `git tag`'s `-n[<num>]`, which Git documents as
// implying --list. The count is attached rather than separate, so `-n5` is one
// argument and `git tag -n5 "v*"` is a query.
func isNumericListing(arg string) bool {
	if !strings.HasPrefix(arg, "-n") {
		return false
	}
	for _, r := range arg[2:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// refOperands counts the operands of a verb whose flags carry no separate
// value except the ones named.
func refOperands(rest []string, readValue map[string]bool) int {
	operands := 0
	for i := 0; i < len(rest); i++ {
		arg := rest[i]
		if arg == "--" {
			return operands + len(rest) - i - 1
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			operands++
			continue
		}
		if _, _, joined := strings.Cut(arg, "="); !joined && readValue[arg] && i+1 < len(rest) {
			i++
		}
	}
	return operands
}

var (
	branchRefSpec = refVerbSpec{
		// Creating, renaming, copying, deleting, or repointing a branch.
		writing: map[string]bool{
			"-d": true, "-D": true, "--delete": true,
			"-m": true, "-M": true, "--move": true,
			"-c": true, "-C": true, "--copy": true,
			"-f": true, "--force": true,
			"-u": true, "--set-upstream-to": true, "--unset-upstream": true,
			"--edit-description": true,
		},
		writingShort: "dDmMcCfu",
		// Each of these was observed NOT to create a branch when followed by a
		// bare name. --color and --abbrev are absent because they DO: their
		// value is optional and must be attached, so `git branch --color foo`
		// creates foo. Listing them as value-carrying skipped the name and
		// classified the creation as permitted, which is the bypass this list
		// is shaped to avoid.
		listing: map[string]bool{
			"-l": true, "--list": true, "-a": true, "--all": true,
			"-r": true, "--remotes": true,
			"--contains": true, "--no-contains": true, "--merged": true,
			"--no-merged": true, "--points-at": true,
			"--format": true, "--sort": true,
		},
	}
	tagRefSpec = refVerbSpec{
		// Creating, signing, annotating, replacing, or deleting a tag.
		writing: map[string]bool{
			"-a": true, "--annotate": true, "-s": true, "--sign": true,
			"-u": true, "--local-user": true,
			"-m": true, "--message": true, "-F": true, "--file": true,
			"-d": true, "--delete": true, "-f": true, "--force": true,
			"-e": true, "--edit": true,
		},
		writingShort: "asumFdfe",
		// `--verify` reads a tag's signature, `-l` filters by pattern, and
		// `-n[<num>]` implies --list. --color is absent for the same reason it
		// is absent above: `git tag --color foo` creates foo.
		listing: map[string]bool{
			"-l": true, "--list": true, "-v": true, "--verify": true,
			"--contains": true, "--no-contains": true, "--merged": true,
			"--no-merged": true, "--points-at": true,
			"--format": true, "--sort": true,
		},
		numericListing: true,
	}
	// symbolic-ref takes a reason with -m; nothing else it accepts carries a
	// separate value.
	symbolicRefReadValue = map[string]bool{"-m": true}
)

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
	// Target is the RESOURCE the invocation resolved to. It is recorded because
	// a refusal is not interpretable without it: the same argv is a protected
	// operation against the candidate and an ordinary one against a test's own
	// fixture, and #257 exists because the boundary could not tell them apart.
	Target GitTargetClass `json:"target,omitempty"`
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
	//
	// An incidental objection the same invocation also carried comes last, for
	// the reason it does in the runtime-owned message: the terminal fact has to
	// lead, or the provider acts on whatever it reads first.
	headline, incidental, _ := strings.Cut(e.Reason, "; ")
	message := "destructive Git refused: " + e.Operation +
		" would discard dirty runtime-owned candidate work (" + headline + ")."
	if e.DirtyTotal > 0 {
		message += fmt.Sprintf(" %d uncommitted candidate path(s) would have been lost", e.DirtyTotal)
		if len(e.Dirty) > 0 {
			message += ": " + strings.Join(e.Dirty, ", ")
		}
		message += "."
	}
	message += " Zenchron owns candidate commits and candidate discard;" +
		" keep working from the current files, or edit them back yourself." +
		" Read-only Git (status, diff, log, show) is unaffected."
	if incidental != "" {
		message += " (This invocation also carried something the boundary refuses" +
			" independently - " + incidental + " - which is not why this command was refused," +
			" and removing it would not make the command permitted.)"
	}
	return message
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
	// THE TERMINAL FACT LEADS. Anything else the invocation was also wrong
	// about comes after the instruction, because a provider reads the first
	// actionable-looking sentence and acts on it - which is how forty-three
	// identical commits got sent.
	headline, incidental, _ := strings.Cut(e.Reason, "; ")
	message := "runtime-owned Git refused: " + e.Operation + " (" + headline + ")." +
		" Do not retry this command and do not create commits, branches or tags:" +
		" Zenchron commits the candidate itself from your working tree." +
		" Leave your changes as edited files and continue with the rest of the task." +
		" Read-only Git (status, diff, log, show) is unaffected."
	if incidental != "" {
		message += " (This invocation also carried something the boundary refuses" +
			" independently - " + incidental + " - which is not why this command was refused," +
			" and removing it would not make the command permitted.)"
	}
	return message
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
func BrokerGitCommand(candidateDir, scratchDir, refusalLog string, args []string, stdout, stderr io.Writer) (int, error) {
	// THE EXECUTION CONTEXT IS THE COMMAND'S OWN, established before anything
	// is classified or resolved.
	//
	// It used to be the candidate's: every permitted command ran with the
	// candidate as its working directory whatever the caller's was, so
	// `cd fixture && git remote add origin <url>` added a remote to the
	// CANDIDATE. Resource scoping cannot be built on that - classifying a
	// target from the caller's directory while executing in the candidate's is
	// a laundering path, not a smaller defect.
	//
	// `-C` is applied here rather than left in the argv, and the broker's own
	// working directory is moved to the result, which pins the context to a
	// DIRECTORY HANDLE: the handle follows the inode, so renaming or replacing
	// the path underneath cannot redirect what the command reaches. The `-C`
	// options are then dropped from what executes, because applying them twice
	// would land somewhere nobody asked for.
	base, pinErr := os.Getwd()
	pinned := base
	if pinErr == nil {
		pinned, pinErr = effectiveCwd(base, args)
	}
	if pinErr == nil {
		pinErr = os.Chdir(pinned)
	}
	if pinErr != nil {
		return refuseGitCommand(candidateDir, refusalLog, args,
			"the execution context could not be established: "+pinErr.Error(),
			GitOperationClass(""), GitTargetExternalOrUnknown, stderr)
	}
	args = withoutDirectoryGlobals(args)
	// THE EFFECTIVE COMMAND FIRST, because the classifier has to be looking at
	// what real Git will run. An alias expands inside Git, after the broker
	// would otherwise have authorized the verb it was spelled with - and both a
	// provider and a checked-out .git/config can define one, since `config` is
	// an ordinary command. See git_alias.go.
	effective, resolveErr := ResolveGitCommand(pinned, args)
	if resolveErr != nil {
		// AUTHORITY OUTRANKS AN INCIDENTAL OBJECTION.
		//
		// Both answers refuse. But when the argv also names something the
		// runtime categorically owns, the configuration complaint is the wrong
		// one to give: it describes a fixable problem with the provider's own
		// invocation, so the provider fixes it and sends the command again.
		// Production did exactly that - run
		// run-9bd736a983ab21ea6be069da6f24e364 refused `git -c user.email=...
		// commit -m ...` FORTY-THREE times in one invocation, each time saying
		// "run the same command without that override", and the worker never
		// once saw that committing was not its to do. It spent the invocation's
		// whole inactivity budget on it.
		//
		// The general law the case taught: when several boundaries object,
		// answer with the one that most accurately terminates the provider's
		// decision tree.
		//
		// EVERY classified answer outranks the incidental one, not just the
		// runtime-owned arm. `git -c user.email=... reset --hard` is a
		// destructive discard that happens to carry a refused key, and telling
		// its sender to "run the same command without that override" invites
		// exactly one thing: running the same destructive command without the
		// override. The law is about which answer TERMINATES the decision
		// tree, and a discard refusal terminates it for the same reason a
		// runtime-owned one does - it says the operation is not available,
		// rather than that the invocation is malformed.
		//
		// CLASSIFYING THE UNRESOLVED ARGV IS SOUND HERE, and only here. Every
		// verb this classifier names is a Git BUILTIN, and Git ignores an alias
		// whose name collides with a command - so for these the literal verb IS
		// the effective verb and no expansion can change it. An alias that
		// expands TO one of them is not recognized, which costs a better
		// message and nothing else: the command is still refused, by the arm
		// below.
		//
		// Nothing is executed on this path either way. Only the class, and the
		// sentence the provider reads, differ.
		if class, reason := ClassifyGitCommand(args); class != GitOperationPermitted {
			return refuseGitCommand(candidateDir, refusalLog, args,
				reason+"; "+resolveErr.Error(), class, targetOf(pinned, candidateDir, scratchDir), stderr)
		}
		// FAIL CLOSED. "I could not tell what this would do" is not "this is
		// safe", and the boundary must never answer the second when it means
		// the first.
		// The underlying refusal already says what to do about itself - a
		// configuration override names its key and how to proceed without it -
		// so the provider is given THAT, while the durable record keeps the
		// resolution prefix a reviewer reads it by.
		return refuseGitCommand(candidateDir, refusalLog, args,
			"the effective operation could not be resolved: "+resolveErr.Error(),
			GitOperationClass(""), targetOf(pinned, candidateDir, scratchDir), stderr)
	}
	class, reason := ClassifyGitCommand(effective)
	// DEFERRED AUTHORITY, resolved here and nowhere earlier. `-c user.email`
	// and `-c user.name` carry no intrinsic capability, so parse time did not
	// refuse them; what they DO carry is whose history a commit is written
	// into, and only the target answers that.
	// FROM THE EFFECTIVE COMMAND, not the one that was typed. Git honours an
	// alias that begins with `-c`, so `alias.sneak = -c user.email=... commit`
	// introduces an identity override during expansion - after a check reading
	// the original argv has already decided there was none. Verified: such an
	// alias commits as the address it names.
	deferred := DeferredGitConfigOverrides(effective)
	// WHICH RESOURCE - asked only when the answer can change the decision. A
	// permitted verb carrying no deferred override is permitted everywhere it
	// was permitted before, so reads are untouched and pay no resolution cost.
	target := GitTargetClass("")
	if class != GitOperationPermitted || len(deferred) > 0 {
		target = targetOf(pinned, candidateDir, scratchDir)
	}
	// THE VERB IS ANSWERED FIRST, and that ordering is #253's law applied one
	// layer along. `-c user.email=... reset --hard` against the candidate is a
	// destructive command that happens to carry an identity override, and
	// telling its sender the identity was the objection invites them to send
	// the same destructive command without it. Removing the override would not
	// make it permitted, so the override is not the answer that terminates the
	// decision.
	if class != GitOperationPermitted {
		// THE ONLY THING THIS PERMITS THAT WAS REFUSED BEFORE. A destructive or
		// runtime-owned verb against the attempt's OWN temp root is a test
		// operating on a repository it created, and governing that was the
		// defect. The candidate, an operator's unrelated repositories, and
		// every context that could not be resolved are refused exactly as
		// before.
		if target != GitTargetRuntimeScratch {
			return refuseGitCommand(candidateDir, refusalLog, effective, reason, class, target, stderr)
		}
		class = GitOperationPermitted
	}
	// ONLY NOW is the identity question the terminal one: the verb would be
	// permitted, so what remains to decide is whose history it writes. Identity
	// may be chosen for a repository the attempt itself created; on the
	// candidate a provider must not decide whose commit it is, and outside both
	// there is nothing this runtime is entitled to write at all.
	if len(deferred) > 0 && target != GitTargetRuntimeScratch {
		return refuseGitCommand(candidateDir, refusalLog, effective,
			"setting the committer identity ("+strings.Join(deferred, ", ")+
				") is not the provider's outside a repository this attempt created; the runtime owns candidate history",
			class, target, stderr)
	}
	if class == GitOperationPermitted {
		// The RESOLVED form is executed. Where no alias was involved it is the
		// original argv unchanged, which is almost every invocation; where one
		// was, running the expansion the broker actually classified is what
		// stops the lookup and the execution from being able to disagree.
		return execRealGit("", effective, stdout, stderr)
	}
	if aliased := boundedGitArgv(args); aliased != boundedGitArgv(effective) {
		// The record names BOTH: what the provider asked for and what it
		// resolved to. An operator reading `git co` needs to be told it was a
		// checkout, and a reader of `git checkout` needs to know nobody typed
		// that.
		reason += " (requested as " + aliased + ")"
	}
	return refuseGitCommand(candidateDir, refusalLog, effective, reason, class, target, stderr)
}

// targetOf resolves the resource from the PINNED context, establishing the
// runtime's anchors first so nothing a provider supplied contributes to them.
// Every failure answers external_or_unknown, which is a refusal for the classes
// that reach here: unknown never widens authority.
func targetOf(pinned, candidateDir, scratchDir string) GitTargetClass {
	identity, err := resolveRepoIdentity(pinned)
	if err != nil {
		return GitTargetExternalOrUnknown
	}
	return ClassifyGitTarget(identity, EstablishGitAuthorityAnchors(candidateDir, scratchDir))
}

// refuseGitCommand records the refusal durably and tells the provider why.
//
// It is one function because every refusal has to do all of it: a refusal that
// executed nothing but recorded nothing would be invisible, and one that
// recorded without explaining would leave the worker to guess.
func refuseGitCommand(candidateDir, refusalLog string, args []string, reason string, class GitOperationClass, target GitTargetClass, stderr io.Writer) (int, error) {
	refusal := GitRefusal{Operation: boundedGitArgv(args), Reason: reason, Target: target}
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

// brokerGitEnv is THE environment for every Git this broker runs - both the
// resolution that decides and the execution that follows.
//
// One definition, because two were a way for authorization and execution to
// disagree. The provider's own environment is carried so that a permitted
// command behaves exactly as it would have without the guard, MINUS the
// brokered sentinel, which exists to make unshimmed Git fail closed and which
// the shim is the one caller allowed to see past.
//
// Consistency alone is not enough, though, and an earlier version of this
// comment stopped there. Two paths agreeing about a redirected repository still
// agree about the wrong one: authority granted over runtime_scratch_repo must
// not also be authority to write an index, an object store, or a committer
// identity somewhere else. So the provider's Git authority is normalized away
// rather than inherited.
//
// Three distinct powers travel in this environment, and only the first is
// obvious. GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY and
// their relatives move where Git reads and writes. GIT_CONFIG_COUNT with
// GIT_CONFIG_KEY_n/GIT_CONFIG_VALUE_n is `-c` spelled as an environment
// variable, so it would walk straight past argv classification and could set
// core.hooksPath. GIT_AUTHOR_* and GIT_COMMITTER_* name a committer without
// ever appearing in argv, which is the same identity question the deferred
// user.name/user.email rule answers for `-c`.
//
// Enumerating those was considered and rejected before, on the grounds that the
// list is long, version-dependent, and exactly what a boundary gets quietly
// wrong. That objection was right about enumeration and wrong about the
// conclusion: the rule here is a PREFIX, not a list. Every GIT_* variable is
// removed and only the pins below are put back, so a Git release that invents a
// new authority-bearing variable is already handled. Unknown fails closed.
func brokerGitEnv() []string {
	host := os.Environ()
	env := make([]string, 0, len(host)+len(brokerGitPins))
	for _, entry := range host {
		name, _, ok := strings.Cut(entry, "=")
		if !ok || strings.HasPrefix(name, "GIT_") || brokerGitStripped[name] {
			continue
		}
		env = append(env, entry)
	}
	// The pins are values, not inheritance, so the provider cannot unset them
	// by unsetting a variable. They restate for brokered Git the isolation
	// trustedGitEnv gives runtime Git.
	return append(env, brokerGitPins...)
}

// brokerGitStripped is the non-GIT_ half: names that still hand Git a command
// to run or a configuration tree to read, and so carry the same authority
// under a different spelling.
var brokerGitStripped = map[string]bool{
	"SSH_ASKPASS":     true,
	"SSH_AUTH_SOCK":   true,
	"XDG_CONFIG_HOME": true,
	"EDITOR":          true,
}

// brokerGitPins put back, as fixed values, the few GIT_* settings whose absence
// would be worse than their presence: without GIT_TERMINAL_PROMPT=0 a brokered
// command can block on a credential prompt, and without the configuration pins
// Git falls back to discovering the host user's system and global files.
var brokerGitPins = []string{
	"GIT_CONFIG_NOSYSTEM=1",
	"GIT_CONFIG_SYSTEM=/dev/null",
	"GIT_CONFIG_GLOBAL=/dev/null",
	"GIT_ATTR_NOSYSTEM=1",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_PAGER=cat",
	"GIT_OPTIONAL_LOCKS=0",
}

// brokerGitOutput asks Git a question in the same environment the command will
// execute in, with the binary resolved off the runtime's trusted search path
// rather than the provider's.
func brokerGitOutput(dir string, args ...string) ([]byte, error) {
	binary, err := gitBinary()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = dir, brokerGitEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// execRealGit runs a permitted command through the real binary.
//
// It resolves Git from the runtime's own trusted search path rather than from
// whatever PATH the provider is running under - the shim is FIRST on that path,
// so resolving by name here would make the broker invoke itself forever.
// THE dir PARAMETER IS THE EXECUTION CONTEXT, and its contract is explicit
// because leaving it implicit cost real work.
//
//	dir != ""  run the command in that directory.
//	dir == ""  the CALLER has already established the context; inherit it.
//
// The empty form exists for one caller: the broker, which pins its own working
// directory to a verified handle before deciding, so that the directory the
// command was classified against is the directory it executes in. Everything
// else must name where it runs.
//
// An earlier version of this function ignored the parameter entirely, on the
// reasoning that the broker had already chdir'd. It had - but the guard TESTS
// call this directly to prove a destructive command would destroy work, and
// with no Dir they ran `git reset --hard` and `git clean -fdx` in the
// development checkout instead of in their fixtures. The tests reported
// "unguarded git reset --hard did NOT discard the work", which was true of the
// fixture and false of the repository. See TestExecRealGitRunsWhereItIsTold.
func execRealGit(dir string, args []string, stdout, stderr io.Writer) (int, error) {
	binary, err := gitBinary()
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(binary, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	// THE SAME environment the resolution used. See brokerGitEnv.
	cmd.Env = brokerGitEnv()
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
