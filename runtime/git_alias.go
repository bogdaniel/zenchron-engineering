package runtime

// Alias resolution, because the classifier has to be looking at the command
// real Git is going to run.
//
// THE HOLE THIS CLOSES. ClassifyGitCommand read the literal verb, and
// execRealGit then handed the original argv to real Git with the brokered
// sentinel removed. Real Git expands aliases itself, AFTER the broker has
// already authorized what it saw:
//
//	git config alias.co checkout      <- permitted; `config` is ordinary
//	git co -- implementation.go        <- classified as `co`, permitted
//	                                      real Git runs `checkout -- <path>`
//
// So a provider could author its own bypass out of two permitted commands.
// Repository configuration could too, because .git/config is candidate content
// and a checked-out tree can carry one.
//
// The invariant is therefore stated the only way that holds:
//
//	the command the broker classifies must be the effective Git operation
//	real Git will execute, including alias expansion.
//
// TWO CONSEQUENCES OF TAKING THAT LITERALLY, both deliberate.
//
// First, the lookup runs under the SAME configuration real Git will use - the
// same working directory, the same environment minus the sentinel, and the
// same leading global options. `git -c alias.co=checkout co -- f` is otherwise
// a second bypass one layer down: a lookup that ignored `-c` would not see the
// alias that the execution then honours.
//
// Second, a resolved command is EXECUTED in its resolved form. When no alias
// was involved that is the original argv unchanged, which is the overwhelming
// majority of invocations. When one was, running the expansion the broker
// actually classified is what removes the last gap - otherwise a provider
// could rewrite the alias between the lookup and the execution and have the
// two disagree.
//
// Nothing here executes alias content to find out what it means. A shell alias
// is refused on sight.

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// maxGitAliasExpansions bounds the chain. Git's own limit is not documented as
// a number, so this is the runtime's: a legitimate alias is one or two hops,
// and anything deeper is either a mistake or an attempt to exhaust the
// resolver. Reaching it is a refusal, not a pass.
const maxGitAliasExpansions = 8

// GitAliasUnresolvableError is the fail-closed refusal for a command whose
// effective operation could not be established.
//
// It exists because "I could not tell what this would do" and "this is safe"
// are different answers, and the boundary must never return the second when it
// means the first.
type GitAliasUnresolvableError struct{ Verb, Detail string }

func (e *GitAliasUnresolvableError) Error() string {
	return "cannot resolve what `git " + e.Verb + "` would do: " + e.Detail
}

// GitConfigOverrideRefusedError is the fail-closed refusal for an inline
// configuration override this boundary has not reasoned about.
//
// It is its own type, and its own message, because the provider has something
// to do about it and that is the whole point: drop the override, or ask the
// operator to allow the key. The blanket `-c` refusal it replaces said only
// that the operation "could not be resolved", which is true and unactionable,
// and a provider that cannot act on a refusal sends the command again.
type GitConfigOverrideRefusedError struct{ Key string }

func (e *GitConfigOverrideRefusedError) Error() string {
	return "configuration override `-c " + e.Key + "` is not permitted here:" +
		" Git has configuration keys whose values are programs it runs, paths it reads," +
		" or credentials it presents, and this key is not on the runtime's inert list." +
		" Run the same command without that override; Git's presentation and signing" +
		" toggles are accepted, and read-only Git is unaffected."
}

// splitGitCommand separates one Git argv into its leading global options, its
// verb, and the rest.
//
// The globals are kept rather than discarded because they change what the
// command means - `-C dir` chooses the repository, `-c k=v` changes the
// configuration the expansion is read from - so both the alias lookup and the
// execution have to carry them.
func splitGitCommand(args []string) (globals []string, verb string, rest []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") {
			return args[:i], arg, args[i+1:]
		}
		switch arg {
		case "-C", "-c", "--git-dir", "--work-tree", "--namespace", "--exec-path",
			"--super-prefix", "--config-env", "--attr-source":
			if i+1 < len(args) {
				i++
			}
		}
	}
	return args, "", nil
}

// refusedGitGlobals are the global options this boundary will not execute.
//
// They are refused because each one changes WHAT GIT RUNS or WHICH REPOSITORY
// IT RUNS AGAINST, and the broker's whole claim is that it classified the
// operation that was about to happen against the candidate.
//
// `--exec-path` is the one that makes this urgent rather than tidy.
// ClassifyGitCommand permits verbs it does not recognize - deliberately, since
// this is a destruction guard and not a Git allowlist - and Git resolves an
// unknown verb by looking for `git-<verb>` on its exec path. So a provider
// could drop a script in a directory it controls and run
// `git --exec-path=<dir> anything`: the classifier sees an unlisted verb,
// permits it, and execRealGit then runs that script with the brokered sentinel
// removed, which is precisely the reach the sentinel exists to deny. The verb
// being harmless-looking is the point.
//
// `-c` and `--config-env` are NOT here, and are classified by KEY instead. Git
// has configuration keys whose values are programs it executes, so most of an
// inline override is an execution redirect wearing different syntax - but not
// all of it, and refusing the whole option refused ordinary engineering Git as
// well. See gitConfigOverrideRefusal.
//
// `--git-dir`, `--work-tree`, `--namespace` and `--super-prefix` point the
// command at a different repository or a different tree. The broker is the
// CANDIDATE's Git; a command asking to be something else is not a command this
// boundary has classified.
//
// `-C` is deliberately NOT here. It changes directory and cannot change which
// program Git runs, and a destructive verb under it is still classified as
// destructive - so refusing it would cost `git -C subdir status` for nothing.
var refusedGitGlobals = map[string]string{
	"--exec-path":    "it redirects where Git resolves the program it runs",
	"--git-dir":      "it points the command at a different repository",
	"--work-tree":    "it points the command at a different working tree",
	"--namespace":    "it points the command at a different ref namespace",
	"--super-prefix": "it rewrites the paths the command addresses",
}

// refusedGitGlobal reports the first global option this boundary will not
// execute, in either its separate or its `--x=y` form.
func refusedGitGlobal(args []string) (string, string, bool) {
	for _, arg := range args {
		if arg == "--" {
			return "", "", false
		}
		name := arg
		if equals := strings.Index(arg, "="); equals > 0 {
			name = arg[:equals]
		}
		if reason, refused := refusedGitGlobals[name]; refused {
			return name, reason, true
		}
	}
	return "", "", false
}

// inertGitConfigKeys are the configuration keys an inline `-c` override may
// set, and it is an ALLOWLIST: a key that is not named here is refused, so a
// key nobody has reasoned about fails closed rather than arriving permitted.
//
// WHY THERE IS A LIST AT ALL. Refusing every `-c` was correct about what it
// refused and wrong about what it cost. Claude Code prefixes its Git argv with
// `-c`, so run run-ca6aecf437c10bc6d2983fe978c5c00a met this boundary 67 times
// in one attempt - 58 of them an ordinary `commit`, the rest `log`, `ls-files`
// and `remote`, none of which can discard anything. The worker could not read
// its own repository, read the refusal as a problem with its invocation, and
// retried until its inactivity window closed. A boundary that refuses
// engineering work it has no objection to is not a stricter boundary; it is the
// same boundary with a denial of service attached. See #248.
//
// WHAT EARNS A PLACE. A key here must be unable to name a program, redirect a
// path, reach a network, or change which repository or identity the command
// acts as - for ANY value, because only the key is classified. That is why
// `core.pager`, `core.editor`, `core.fsmonitor`, `core.hooksPath`,
// `core.sshCommand`, `core.askPass`, `credential.helper`, `gpg.program`,
// `filter.*`, `diff.external`, `http.*`, `protocol.*`, `url.*`, `safe.*`,
// `include.path`, `includeIf.*` and `alias.*` are all absent and stay absent:
// each of them is an execution, transport or authority redirect, and
// `core.pager` in particular is the exact escape git_guard_test.go already
// proves - `-c core.pager=<script> log` runs the script.
//
// The signing keys are here and the signing PROGRAM is not, which is the
// distinction the whole list turns on: `commit.gpgsign=false` chooses whether
// to sign, and `gpg.program` chooses what to execute.
var inertGitConfigKeys = map[string]bool{
	"commit.gpgsign": true, "tag.gpgsign": true, "log.showsignature": true,
	"core.quotepath": true, "core.abbrev": true, "core.checkstat": true,
	"core.trustctime": true, "core.precomposeunicode": true,
	"gc.auto": true, "maintenance.auto": true,
	"log.date": true, "color.ui": true,
}

// inertGitConfigSections are whole sections whose every key is inert. `advice.`
// is the only one: every key under it is a boolean that turns one of Git's
// hints on or off, and a hint cannot execute, redirect or authorize anything.
var inertGitConfigSections = []string{"advice."}

func inertGitConfigKey(key string) bool {
	key = strings.ToLower(key)
	if inertGitConfigKeys[key] {
		return true
	}
	for _, section := range inertGitConfigSections {
		if strings.HasPrefix(key, section) && strings.Count(key, ".") == 1 {
			return true
		}
	}
	return false
}

// gitConfigOverrideRefusal reports the first `-c` or `--config-env` key this
// boundary will not accept, and names it.
//
// Naming the key is not decoration. A provider told only that `-c` is refused
// has no way to learn which part of its own invocation to drop, and the one in
// #248 responded by sending the same command again; a provider told that
// `core.pager` is the objection can send the command without it.
//
// Both spellings are parsed, in both their joined and separated forms, because
// `git -c k=v`, `git -c` `k=v` and `git --config-env=k=VAR` are the same act.
// `--config-env` names an environment variable rather than a value, so its
// value is opaque here - which changes nothing, because the KEY is what decides.
func gitConfigOverrideRefusal(globals []string) (string, bool) {
	for i := 0; i < len(globals); i++ {
		arg := globals[i]
		setting := ""
		switch {
		case arg == "-c" || arg == "--config-env":
			if i+1 >= len(globals) {
				// An override with nothing to override is not resolvable, and
				// naming it is more useful than guessing at it.
				return arg, true
			}
			i++
			setting = globals[i]
		case strings.HasPrefix(arg, "--config-env="):
			setting = strings.TrimPrefix(arg, "--config-env=")
		default:
			continue
		}
		// `-c key` with no `=` is Git's shorthand for `key=true`. It is still a
		// key, and it is classified as one.
		key := setting
		if equals := strings.Index(setting, "="); equals >= 0 {
			key = setting[:equals]
		}
		if !inertGitConfigKey(key) {
			return key, true
		}
	}
	return "", false
}

// ResolveGitCommand expands args through the effective Git configuration and
// returns the command real Git will actually run.
//
// It fails closed: a shell alias, a cycle, an unreadable configuration, a
// malformed alias value or a chain that will not terminate all produce an
// error, and the caller refuses. An argv with no alias in it is returned
// unchanged, which is what happens for essentially every real invocation.
func ResolveGitCommand(candidateDir string, args []string) ([]string, error) {
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		globals, verb, rest := splitGitCommand(args)
		// REFUSED BEFORE ANYTHING IS RESOLVED OR RUN. A global that redirects
		// what Git executes defeats the boundary whatever the verb turns out to
		// be, so it is answered here rather than being carried into a
		// classification that would then be about the wrong thing.
		if name, reason, refused := refusedGitGlobal(globals); refused {
			return nil, &GitAliasUnresolvableError{Verb: name, Detail: reason}
		}
		// The same question for the option that is classified by KEY rather
		// than refused outright.
		if key, refused := gitConfigOverrideRefusal(globals); refused {
			return nil, &GitConfigOverrideRefusedError{Key: key}
		}
		if verb == "" {
			return args, nil
		}
		// A VERB GIT IMPLEMENTS IS NOT AN ALIAS, and Git will not expand one:
		// an alias whose name collides with a command is ignored. Mirroring
		// that is what keeps `git status` from being refused because somebody
		// once wrote `alias.status` into a config file.
		if gitImplements(candidateDir, verb) {
			return args, nil
		}
		if hop >= maxGitAliasExpansions {
			return nil, &GitAliasUnresolvableError{Verb: verb,
				Detail: fmt.Sprintf("the alias chain did not terminate within %d expansions", maxGitAliasExpansions)}
		}
		if seen[verb] {
			return nil, &GitAliasUnresolvableError{Verb: verb, Detail: "the alias expands to itself"}
		}
		seen[verb] = true

		value, defined, err := gitAliasValue(candidateDir, globals, verb)
		if err != nil {
			return nil, &GitAliasUnresolvableError{Verb: verb, Detail: err.Error()}
		}
		if !defined {
			// Not an alias and not a command Git advertises. Whatever it is,
			// real Git will decide - and it cannot be a destructive operation
			// this boundary classifies, because those are all commands.
			return args, nil
		}
		if strings.HasPrefix(strings.TrimSpace(value), "!") {
			// A SHELL ALIAS. Its meaning is whatever a shell makes of it, and
			// the one way to find that out is to run it - which is exactly what
			// a boundary deciding whether to permit it must not do. So it is
			// refused, and the refusal says why.
			return nil, &GitAliasUnresolvableError{Verb: verb,
				Detail: "it is a shell alias, and determining what it does would mean executing it"}
		}
		words, err := splitGitAliasValue(value)
		if err != nil {
			return nil, &GitAliasUnresolvableError{Verb: verb, Detail: err.Error()}
		}
		if len(words) == 0 {
			return nil, &GitAliasUnresolvableError{Verb: verb, Detail: "the alias expands to nothing"}
		}
		// Git appends the remaining arguments to the expansion, so
		// `git co -- f` with `alias.co = checkout` is `checkout -- f`.
		next := make([]string, 0, len(globals)+len(words)+len(rest))
		next = append(next, globals...)
		next = append(next, words...)
		next = append(next, rest...)
		args = next
	}
}

// splitGitAliasValue splits an alias value the way Git's own split_cmdline
// does: whitespace separates, single and double quotes group, and a backslash
// escapes inside double quotes.
//
// An unbalanced quote is an ERROR rather than a best guess. Git treats it as a
// bad configuration value and so does this: a value nobody can parse the same
// way twice is not something to authorize a decision on.
func splitGitAliasValue(value string) ([]string, error) {
	var words []string
	var current strings.Builder
	var quote rune
	started := false
	for i := 0; i < len(value); i++ {
		c := rune(value[i])
		switch {
		case quote == '"' && c == '\\' && i+1 < len(value):
			i++
			current.WriteByte(value[i])
			started = true
		case quote != 0 && c == quote:
			quote = 0
		case quote == 0 && (c == '\'' || c == '"'):
			quote, started = c, true
		case quote == 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			if started {
				words = append(words, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("the alias value has an unbalanced quote")
	}
	if started {
		words = append(words, current.String())
	}
	return words, nil
}

// gitAliasValue reads alias.<verb> from the configuration this command will
// actually run under.
//
// The globals are passed through so that a `-c alias.x=...` override is
// visible here exactly as it will be to the execution. Exit status 1 is Git's
// "not set", which is an answer; anything else is a configuration this code
// could not read, which is a refusal.
func gitAliasValue(candidateDir string, globals []string, verb string) (string, bool, error) {
	args := make([]string, 0, len(globals)+3)
	args = append(args, globals...)
	args = append(args, "config", "--get", "alias."+verb)
	out, code, err := effectiveGit(candidateDir, args...)
	switch {
	case err != nil && code == 0:
		return "", false, err
	case code == 1:
		return "", false, nil
	case code != 0:
		return "", false, fmt.Errorf("git config exited %d reading alias.%s", code, verb)
	}
	return strings.TrimRight(out, "\r\n"), true, nil
}

// gitImplements reports whether Git advertises verb as one of its own
// commands, which is the condition under which it ignores a same-named alias.
//
// The list comes from the SAME binary that will run the command, so the answer
// is that binary's rather than this file's opinion of what Git contains. Where
// the installed Git is too old to list its commands the answer is "no", which
// makes an alias shadowing a command expandable here and therefore refusable -
// the conservative direction, and one that costs nothing unless somebody has
// written a config Git itself is ignoring.
func gitImplements(candidateDir, verb string) bool {
	for _, known := range gitBuiltinCommands(candidateDir) {
		if known == verb {
			return true
		}
	}
	return false
}

// gitBuiltinCommands is memoized per candidate workspace: the answer is a
// property of the installed binary, and the resolver asks for it once per
// expansion hop.
var gitBuiltins sync.Map // candidateDir -> []string

func gitBuiltinCommands(candidateDir string) []string {
	if cached, ok := gitBuiltins.Load(candidateDir); ok {
		return cached.([]string)
	}
	var commands []string
	if out, code, err := effectiveGit(candidateDir, "--list-cmds=builtins"); err == nil && code == 0 {
		for _, line := range strings.Split(out, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				commands = append(commands, line)
			}
		}
	}
	gitBuiltins.Store(candidateDir, commands)
	return commands
}

// effectiveGit runs one read-only Git query under the same configuration the
// brokered execution will use.
//
// It resolves the binary the way execRealGit does - from the runtime's own
// trusted search path, never by name - because the guard directory is first on
// the provider's path and resolving by name here would make the broker call
// itself. The environment is the provider's minus the sentinel, which is
// exactly what the execution gets, so the configuration this reads is the
// configuration that will apply.
func effectiveGit(candidateDir string, args ...string) (string, int, error) {
	binary, err := gitBinary()
	if err != nil {
		return "", 0, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = candidateDir
	cmd.Env = withoutBrokeredGitDir(os.Environ())
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			return stdout.String(), exit.ExitCode(), nil
		}
		return stdout.String(), 0, runErr
	}
	return stdout.String(), 0, nil
}
