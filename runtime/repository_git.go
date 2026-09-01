package runtime

// RepositoryGitRunner is the trusted Git execution path for *repository
// control*: the clones, checkouts, commits, rebases, merges and fetches the
// runtime performs on its own behalf.
//
// It is deliberately a second profile, distinct from GitRunner in
// git_runner.go:
//
//   - GitRunner is the *broker* profile. It backs capabilities a model can
//     trigger, so it is local-only, never networked, never credentialed, and
//     its pathspecs are model-influenced.
//   - RepositoryGitRunner is the *controller* profile. Its subcommands and
//     flags are runtime literals, some of its operations must reach the
//     network, and one of them may be handed an operator-authorized
//     credential. What it must never do is let the candidate, the model, a
//     provider tool, or the ambient host environment choose the transport, the
//     remote, the credential, or any program Git executes.
//
// The two profiles share the low-level constants (trustedPATH, gitBinary) but
// not the policy. The duplication in the environment list is intentional:
// git_runner.go is frozen, and copying a dozen constant strings is cheaper
// than coupling two security policies to one another.
//
// The defect this closes: runGit/gitOutput built exec.Command("git", ...) with
// no Env, so every runtime-owned Git operation inherited the whole host
// environment - ambient GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE redirection, the
// user's ~/.gitconfig, credential helpers, askpass programs, an SSH agent, a
// pager, and external diff/filter programs.

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Remote identity
// ---------------------------------------------------------------------------

type gitTransport int

const (
	transportNone gitTransport = iota
	// transportFile is a filesystem repository. It cannot reach a network, so
	// it is the only transport the local profile is ever allowed to use.
	transportFile
	transportHTTPS
	// transportInsecureHTTP exists solely so in-package tests can bind a
	// remote operation to a local httptest endpoint. GovernedRemote never
	// produces it and no exported API can construct it, so it is not reachable
	// from outside this package.
	transportInsecureHTTP
)

func (t gitTransport) String() string {
	switch t {
	case transportFile:
		return "file"
	case transportHTTPS:
		return "https"
	case transportInsecureHTTP:
		return "http"
	}
	return "none"
}

// RemoteIdentity is the exact remote a runtime network operation is permitted
// to contact, in CANONICAL form. Its transport is unexported on purpose: a
// RemoteIdentity can only be produced by GovernedRemote from the governed
// run/repository model, so a model, provider, or tool argument cannot forge one
// or widen the transport set by supplying a URL.
//
// URL is the one frozen spelling of the identity, not the caller's. That is the
// whole point: GovernedRemote already treats
// https://github.com/o/r and https://github.com/o/r.git as the same repository
// when it validates them, so carrying the caller's spelling forward made two
// remotes Zenchron itself calls identical behave as different authorities
// later. The fifth-generation dogfood run met exactly that - the operator
// checkout's origin carried .git, the run's candidate origin did not - and
// every base.integrate attempt was refused as "not the governed remote" against
// a repository that had not drifted at all.
//
// Owner and Name are the validated GitHub identity components. They exist so
// equality is an identity comparison rather than a string comparison; for a
// local filesystem repository they are empty and the cleaned path IS the
// identity.
type RemoteIdentity struct {
	URL         string
	transport   gitTransport
	owner, name string
}

func (i RemoteIdentity) Transport() string { return i.transport.String() }

// Owner and Name expose the validated GitHub identity. They are empty for a
// local filesystem repository, which has no owner/name identity to expose.
func (i RemoteIdentity) Owner() string { return i.owner }
func (i RemoteIdentity) Name() string  { return i.name }

// Same reports whether two governed remotes are the SAME AUTHORITY.
//
// It compares the transport and the canonical identity - never a host string
// alone, and never the caller's spelling. A different owner, a different
// repository, a different transport, or a different local path is a different
// authority and stays refused.
func (i RemoteIdentity) Same(other RemoteIdentity) bool {
	return i.transport == other.transport &&
		i.owner == other.owner &&
		i.name == other.name &&
		i.URL == other.URL
}

// GovernedRemoteMismatchError is a DETERMINISTIC trust refusal: the remote a
// workspace is bound to is not the governed remote of this run. It is typed so
// an operation handler can route it as the identity refusal it is instead of
// retrying an answer that cannot change: the governed remote is configuration,
// nothing the runtime does alters it, and changing it would change the run's
// trusted subject.
type GovernedRemoteMismatchError struct{ Governed, Observed string }

func (e *GovernedRemoteMismatchError) Error() string {
	return "refused remote " + strconv.Quote(e.Observed) + ": not the governed remote " + strconv.Quote(e.Governed) + " for this workspace"
}

// GovernedRemote classifies a remote from the governed repository model and
// fails closed. M0 supports exactly two shapes:
//
//   - https://github.com/<owner>/<repo>[.git] - the only network transport;
//   - an absolute path to an existing directory - a filesystem repository the
//     runtime already controls (assurance checkouts, fixtures).
//
// Everything else is refused, including ssh://, git://, http://, file://,
// ext::, scp-style git@host:owner/repo, URLs carrying userinfo or a port,
// other hosts, relative paths, and anything that could be read as an option.
func GovernedRemote(remote string) (RemoteIdentity, error) {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return RemoteIdentity{}, fmt.Errorf("governed remote is required")
	}
	if strings.HasPrefix(remote, "-") || strings.ContainsAny(remote, "\x00\n\r") {
		return RemoteIdentity{}, fmt.Errorf("refused remote %q: not a remote", remote)
	}
	if strings.HasPrefix(remote, "https://") {
		u, err := url.Parse(remote)
		if err != nil {
			return RemoteIdentity{}, fmt.Errorf("refused remote %q: %w", remote, err)
		}
		if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return RemoteIdentity{}, fmt.Errorf("refused remote %q: only https://github.com is supported", remote)
		}
		parts := strings.Split(strings.TrimPrefix(strings.TrimSuffix(u.Path, ".git"), "/"), "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return RemoteIdentity{}, fmt.Errorf("refused remote %q: not owner/name", remote)
		}
		// The identity is the validated owner and repository, and the URL is
		// derived from them in one frozen spelling. The caller's optional .git
		// suffix is accepted and then dropped, because it was never part of
		// what makes this repository this repository.
		owner, name := parts[0], parts[1]
		return RemoteIdentity{
			URL:       "https://github.com/" + owner + "/" + name,
			transport: transportHTTPS, owner: owner, name: name,
		}, nil
	}
	if filepath.IsAbs(remote) && !strings.Contains(remote, "://") {
		// A local repository has no owner/name identity; its cleaned absolute
		// path is the identity, and it is deliberately NOT canonicalized into
		// anything GitHub-shaped.
		cleaned := filepath.Clean(remote)
		if info, err := os.Stat(cleaned); err == nil && info.IsDir() {
			return RemoteIdentity{URL: cleaned, transport: transportFile}, nil
		}
		return RemoteIdentity{}, fmt.Errorf("refused remote %q: no such local repository", remote)
	}
	return RemoteIdentity{}, fmt.Errorf("refused remote %q: unsupported transport", remote)
}

// ---------------------------------------------------------------------------
// Policies
// ---------------------------------------------------------------------------

// CredentialProvider is the typed auth boundary. Operator/global configuration
// may install one; repository configuration never can. The secret it returns is
// handed to Git through a runtime-owned, private, single-use askpass program -
// never through argv, never through the candidate's environment, never into
// repository config, canonical events, logs, or artifacts.
type CredentialProvider interface {
	Credential(RemoteIdentity) (username, secret string, err error)
}

// LocalPolicy is the trust profile for repository-control operations that must
// not reach a network.
type LocalPolicy struct {
	// WritableConfigKeys is the complete set of repository config keys the
	// runtime may write with `git config`. Anything else is refused, so a
	// runtime-owned config change is always explicit and always lands in the
	// trusted metadata baseline.
	WritableConfigKeys []string
	// AllowFileTransport permits clone from a filesystem path. It is the
	// explicit local-transport capability: no network protocol is enabled when
	// it is on, so a "file" clone provably cannot leave the machine.
	AllowFileTransport bool
}

// RemotePolicy authorizes exactly one network-capable operation target.
type RemotePolicy struct {
	Identity    RemoteIdentity
	Credentials CredentialProvider
}

// RepositoryGitRunner executes one runtime-owned Git command.
type RepositoryGitRunner struct {
	// Dir is the runtime-owned repository. Empty means a repository-creating
	// operation (clone/init), which then runs in a private empty directory so
	// no ambient checkout is discoverable.
	Dir string
	// Local always applies. Remote is nil unless the operation is bound to a
	// governed remote, and nil means every transport is refused.
	Local  LocalPolicy
	Remote *RemotePolicy
}

// controlPolicy is the default repository-control local profile.
func controlPolicy() LocalPolicy {
	return LocalPolicy{
		WritableConfigKeys: []string{"user.name", "user.email"},
		AllowFileTransport: true,
	}
}

// ---------------------------------------------------------------------------
// Argument policy
// ---------------------------------------------------------------------------

// refusedSubcommands may contact a remote or rewrite execution config. clone,
// fetch and push are the only remote-capable subcommands, and they are
// authorized through a transport policy; the rest are refused outright in M0.
//
// push is not in this list, but the local profile still refuses it: like
// fetch, it requires a bound governed RemoteIdentity (see transportFor), so a
// runner with no Remote can no more push than it can fetch. guardPush
// additionally refuses every history-rewriting and ref-deleting form, so the
// only push this runner can perform is a fast-forward of one explicit ref to
// the one governed remote.
var refusedSubcommands = map[string]bool{
	"pull": true, "ls-remote": true, "submodule": true,
	"send-pack": true, "fetch-pack": true, "upload-pack": true, "upload-archive": true,
	"daemon": true, "http-fetch": true, "http-push": true, "remote-ext": true,
	"request-pull": true, "svn": true, "p4": true, "credential": true,
	"filter-branch": true, "instaweb": true,
}

// refusedFlagPrefixes are arguments that would move the repository, change the
// program Git executes, or re-open a transport the policy just closed.
var refusedFlagPrefixes = []string{
	"--upload-pack", "--receive-pack", "--exec-path", "--exec=", "--config-env",
	"--git-dir", "--work-tree", "--namespace", "--template", "--separate-git-dir",
	"--super-prefix", "--reference", "--recurse-submodules", "--recursive",
	"--ext-diff", "--ext=", "-c",
}

func (r RepositoryGitRunner) guard(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("trusted git requires a subcommand")
	}
	sub := args[0]
	if strings.HasPrefix(sub, "-") {
		return fmt.Errorf("trusted git refuses caller-supplied global flag %q", sub)
	}
	if refusedSubcommands[sub] {
		return fmt.Errorf("trusted git refuses subcommand %q", sub)
	}
	for _, a := range args {
		for _, bad := range refusedFlagPrefixes {
			if a == bad || strings.HasPrefix(a, bad+"=") {
				return fmt.Errorf("trusted git refuses argument %q", a)
			}
		}
	}
	if sub == "remote" {
		// Read-only remote inspection only; add/set-url would rewrite the
		// governed binding, and `remote show` contacts the network.
		if len(args) < 2 || (args[1] != "get-url" && args[1] != "-v") {
			return fmt.Errorf("trusted git refuses %q", strings.Join(args, " "))
		}
	}
	if sub == "config" {
		if err := r.guardConfigWrite(args[1:]); err != nil {
			return err
		}
	}
	if sub == "push" {
		if err := r.guardPush(args[1:]); err != nil {
			return err
		}
	}
	if r.Dir == "" && sub != "clone" && sub != "init" {
		return fmt.Errorf("trusted git requires a workspace for %q", sub)
	}
	if r.Dir != "" && !filepath.IsAbs(r.Dir) {
		return fmt.Errorf("trusted git requires an absolute workspace")
	}
	return nil
}

// guardConfigWrite keeps runtime config writes inside the declared allowlist
// and inside the repository. Global/system/file writes are refused outright.
func (r RepositoryGitRunner) guardConfigWrite(args []string) error {
	var positional []string
	for _, a := range args {
		switch {
		case a == "--global" || a == "--system" || a == "--file" || a == "--blob" ||
			strings.HasPrefix(a, "--file=") || strings.HasPrefix(a, "--blob="):
			return fmt.Errorf("trusted git refuses config scope %q", a)
		case strings.HasPrefix(a, "-"):
			continue
		default:
			positional = append(positional, a)
		}
	}
	if len(positional) < 2 {
		return nil // a read (--list, --get, ...)
	}
	key := strings.ToLower(positional[0])
	for _, allowed := range r.Local.WritableConfigKeys {
		if strings.ToLower(allowed) == key {
			return nil
		}
	}
	return fmt.Errorf("trusted git refuses to write repository config key %q", positional[0])
}

// refusedPushFlags would rewrite published history, delete refs, or push refs
// the caller did not name. The runtime publishes by fast-forwarding exactly one
// run-owned ref; after publication a moved base is a merge-from-base, never a
// force-push, so none of these has a legitimate runtime use.
var refusedPushFlags = map[string]bool{
	"-f": true, "--force": true, "--force-with-lease": true, "--force-if-includes": true,
	"-d": true, "--delete": true, "--mirror": true, "--all": true, "--tags": true,
	"--follow-tags": true, "--prune": true,
}

// guardPush binds a push to the governed remote and to an explicit refspec.
// The destination must be the runner's own RemoteIdentity URL: a remote NAME is
// refused, because a name is resolved from repository config, and repository
// config is exactly what a producer could have rewritten.
func (r RepositoryGitRunner) guardPush(args []string) error {
	if r.Remote == nil || r.Remote.Identity.URL == "" {
		return fmt.Errorf("trusted git: push requires a governed remote identity")
	}
	var positional []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			if refused, known := refusedPushFlags[a]; known && refused {
				return fmt.Errorf("trusted git refuses push argument %q", a)
			}
			if strings.HasPrefix(a, "--force") || strings.HasPrefix(a, "--delete") {
				return fmt.Errorf("trusted git refuses push argument %q", a)
			}
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) != 2 {
		return fmt.Errorf("trusted git: push requires exactly a governed remote and one refspec")
	}
	if positional[0] != r.Remote.Identity.URL {
		return fmt.Errorf("trusted git: push destination %q is not the governed remote", positional[0])
	}
	source, destination, ok := strings.Cut(positional[1], ":")
	if !ok || source == "" || destination == "" || strings.HasPrefix(source, "+") {
		return fmt.Errorf("trusted git: push requires a non-forced <source>:<destination> refspec")
	}
	if !strings.HasPrefix(destination, "refs/heads/") {
		return fmt.Errorf("trusted git: push may only update a branch ref")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Transport policy
// ---------------------------------------------------------------------------

// transportFor decides which single protocol this invocation may use.
func (r RepositoryGitRunner) transportFor(args []string) (gitTransport, error) {
	sub := args[0]
	if sub != "clone" && sub != "fetch" && sub != "push" {
		// No transport at all. Every protocol stays "never", which is what
		// makes a local repository-control operation provably offline.
		if r.Remote != nil {
			return transportNone, fmt.Errorf("trusted git: %q is not a remote operation", sub)
		}
		return transportNone, nil
	}
	if r.Remote != nil {
		if r.Remote.Identity.transport == transportNone || r.Remote.Identity.URL == "" {
			return transportNone, fmt.Errorf("trusted git: unbound remote identity")
		}
		if sub == "clone" {
			source, err := cloneSource(args)
			if err != nil {
				return transportNone, err
			}
			if source != r.Remote.Identity.URL {
				return transportNone, fmt.Errorf("trusted git: clone source %q is not the governed remote", source)
			}
		}
		return r.Remote.Identity.transport, nil
	}
	// Local profile. fetch and push always need a bound remote identity; clone
	// is allowed only from a filesystem repository, and only when the explicit
	// local-transport capability is enabled.
	if sub == "fetch" || sub == "push" {
		return transportNone, fmt.Errorf("trusted git: %s requires a governed remote identity", sub)
	}
	if !r.Local.AllowFileTransport {
		return transportNone, fmt.Errorf("trusted git: local transport capability not granted")
	}
	source, err := cloneSource(args)
	if err != nil {
		return transportNone, err
	}
	identity, err := GovernedRemote(source)
	if err != nil {
		return transportNone, err
	}
	if identity.transport != transportFile {
		return transportNone, fmt.Errorf("trusted git: %q needs a governed remote identity", source)
	}
	return transportFile, nil
}

// cloneSource returns the source argument of a clone, and requires the
// destination to be absolute because a repository-creating operation runs in a
// private empty working directory.
func cloneSource(args []string) (string, error) {
	var positional []string
	for _, a := range args[1:] {
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		positional = append(positional, a)
	}
	if len(positional) == 0 {
		return "", fmt.Errorf("trusted git: clone needs a source")
	}
	if len(positional) > 1 && !filepath.IsAbs(positional[1]) {
		return "", fmt.Errorf("trusted git: clone destination must be absolute")
	}
	return positional[0], nil
}

// ---------------------------------------------------------------------------
// Repository config safety
// ---------------------------------------------------------------------------

// deniedConfigSections are whole sections of .git/config that exist to make
// Git run a program, pick a credential, or rewrite a URL.
var deniedConfigSections = map[string]bool{
	"filter": true, "credential": true, "url": true, "alias": true,
	"include": true, "includeif": true, "difftool": true, "mergetool": true,
	"guitool": true, "protocol": true, "http": true, "https": true, "ssh": true,
	"gpg": true, "sendemail": true, "uploadpack": true, "receive": true,
	"instaweb": true, "browser": true, "imap": true, "trace2": true,
	"svn-remote": true, "man": true, "web": true,
}

// deniedConfigKeys are individual keys in otherwise legitimate sections.
// "*" matches a subsection.
var deniedConfigKeys = map[string]bool{
	"core.hookspath": true, "core.fsmonitor": true, "core.sshcommand": true,
	"core.pager": true, "core.editor": true, "core.askpass": true,
	"core.gitproxy": true, "core.alternaterefscommand": true,
	"core.attributesfile": true, "core.excludesfile": true,
	"diff.external": true, "diff.*.command": true, "diff.*.textconv": true,
	"merge.*.driver": true, "merge.*.cmd": true, "sequence.editor": true,
	"init.templatedir": true, "commit.template": true,
	"remote.*.uploadpack": true, "remote.*.receivepack": true,
	"remote.*.proxy": true, "remote.*.vcs": true, "remote.*.pushurl": true,
}

// assertRepositoryConfigSafe rejects a repository whose own .git/config could
// make Git execute a program or pick up a credential.
//
// This is the backstop for the .gitattributes attack: an attributes file can
// only *name* a clean/smudge/diff/merge driver, and the driver's command must
// come from config. System and global config are switched off by the
// environment, command-line -c beats repository config for the single-valued
// keys that matter, and this check covers the rest - including on the recovery
// path (RestoreTrusted), which has no integrity baseline to compare against.
func assertRepositoryConfigSafe(dir string) error {
	path := filepath.Join(dir, ".git", "config")
	f, err := os.Open(path)
	if err != nil {
		return nil // not a repository with a config file; nothing to vet
	}
	defer f.Close()
	section := ""
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			// "[section]", "[section \"sub\"]" and the legacy "[section.sub]"
			// all collapse to "section" or "section.*" for matching.
			end := strings.Index(line, "]")
			if end < 0 {
				return &WorkspaceIntegrityError{Detail: "unparsable repository config"}
			}
			head := line[1:end]
			name, sub := head, ""
			if i := strings.IndexAny(head, " \t"); i >= 0 {
				name, sub = head[:i], strings.TrimSpace(head[i:])
			} else if i := strings.Index(head, "."); i >= 0 {
				name, sub = head[:i], head[i+1:]
			}
			name = strings.ToLower(strings.TrimSpace(name))
			if deniedConfigSections[name] {
				return &WorkspaceIntegrityError{Detail: "repository config section [" + name + "] is not permitted"}
			}
			section = name
			if sub != "" {
				section = name + ".*"
			}
			// Git also accepts "[core] hooksPath = x", so whatever follows the
			// closing bracket on the same line is still a key.
			line = strings.TrimSpace(line[end+1:])
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
				continue
			}
		}
		key := line
		if i := strings.IndexAny(line, "="); i >= 0 {
			key = strings.TrimSpace(line[:i])
		}
		full := section + "." + strings.ToLower(key)
		if section == "" {
			full = strings.ToLower(key)
		}
		if deniedConfigKeys[full] {
			return &WorkspaceIntegrityError{Detail: "repository config key " + full + " is not permitted"}
		}
	}
	return scanner.Err()
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

// repositoryGitEnv is the complete environment of a repository-control Git
// child. os.Environ() is never consulted, so no host variable can appear here
// by inheritance.
//
// Absent by construction, not by override: GIT_DIR, GIT_WORK_TREE,
// GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY, GIT_ALTERNATE_OBJECT_DIRECTORIES,
// GIT_NAMESPACE, GIT_EXTERNAL_DIFF, GIT_DIFF_OPTS, GIT_SSH, GIT_SSH_COMMAND,
// GIT_ASKPASS (unless a credential is authorized, below), SSH_ASKPASS,
// SSH_AUTH_SOCK, GIT_PROXY_COMMAND, GIT_EDITOR, GIT_SEQUENCE_EDITOR,
// XDG_CONFIG_HOME, and every credential variable.
func repositoryGitEnv(home, template, askpass string) []string {
	env := []string{
		"PATH=" + trustedPATH,
		// Runtime-owned, empty, and removed after the call: the host user's
		// ~/.gitconfig, ~/.netrc, and ~/.ssh are simply not present.
		"HOME=" + home,
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_LITERAL_PATHSPECS=1",
		// An empty runtime-owned template, so neither a user template nor a
		// system template can seed hooks into a clone or init.
		"GIT_TEMPLATE_DIR=" + template,
	}
	if askpass != "" {
		env = append(env, "GIT_ASKPASS="+askpass)
	}
	return env
}

// runtimeConfigOverrides are applied to every repository-control operation, not
// just the ones capable of triggering them, so the policy is uniform and one
// missed call site cannot become the hole. Command-line -c outranks repository
// config for these single-valued keys, and the empty credential.helper resets
// any helper list a repository tried to install.
func (r RepositoryGitRunner) runtimeConfigOverrides(hooks string, transport gitTransport) []string {
	args := []string{
		"-c", "core.hooksPath=" + hooks,
		"-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false",
		"-c", "tag.gpgSign=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "credential.helper=",
		"-c", "protocol.allow=never",
		"-c", "protocol.ext.allow=never",
	}
	switch transport {
	case transportFile:
		args = append(args, "-c", "protocol.file.allow=always")
	case transportHTTPS:
		args = append(args, "-c", "protocol.https.allow=always")
	case transportInsecureHTTP:
		args = append(args, "-c", "protocol.http.allow=always")
	}
	return args
}

// ---------------------------------------------------------------------------
// Credential seam
// ---------------------------------------------------------------------------

// writeAskpass materializes a single-use askpass program. The secret lives only
// inside a 0700 file in a 0700 runtime-owned directory that is removed when the
// command returns; it never reaches argv, the environment, repository config,
// or any artifact. Candidate execution has no bind mount for this directory.
func writeAskpass(dir, username, secret string) (string, error) {
	path := filepath.Join(dir, "askpass")
	script := "#!/bin/sh\ncase \"$1\" in\n*[Uu]sername*) printf '%s\\n' " +
		shellQuote(username) + " ;;\n*) printf '%s\\n' " + shellQuote(secret) + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		return "", err
	}
	return path, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// runExecProbe runs the probe. It is a variable because the condition it
// detects is a MOUNT FLAG, which no test can create portably and which root
// cannot bypass either - so the refusal branch is reachable in a test only by
// substituting the exec itself.
var runExecProbe = func(path string) error { return exec.Command(path).Run() }

// assertTempExecutable proves the private root can actually run a program
// before the credential seam depends on one.
//
// GIT_ASKPASS is a PROGRAM, and the runtime writes it into a private directory
// under TMPDIR. A host that mounts its temp filesystem noexec - ordinary
// hardening, and exactly what this runtime's own assurance sandbox does to
// /tmp - cannot execute it. Git's report of that is "could not read Username
// for <remote>": credential-shaped, and wrong. The credential is present and
// correct; the filesystem refused to run the messenger.
//
// So the condition is proven here, with a probe that carries no secret, and
// named for what it is. The runtime does not relocate the directory: TMPDIR is
// the operator's own control for this, and silently moving runtime-owned
// material somewhere the operator did not choose is a worse answer than saying
// which directory is the problem.
func assertTempExecutable(dir string) error {
	probe := filepath.Join(dir, "exec-probe")
	if err := os.WriteFile(probe, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		return err
	}
	defer os.Remove(probe)
	if err := runExecProbe(probe); err != nil {
		return fmt.Errorf("temp directory %s does not permit execution, so the runtime-owned Git askpass program cannot run; set TMPDIR to a directory that allows execution: %w", os.TempDir(), err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func (r RepositoryGitRunner) run(args ...string) ([]byte, error) {
	if err := r.guard(args); err != nil {
		return nil, err
	}
	transport, err := r.transportFor(args)
	if err != nil {
		return nil, err
	}
	if r.Dir != "" {
		if err := assertRepositoryConfigSafe(r.Dir); err != nil {
			return nil, err
		}
	}
	binary, err := gitBinary()
	if err != nil {
		return nil, err
	}
	root, err := os.MkdirTemp("", "zenchron-repo-git-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(root)
	home, hooks, template, cwd := filepath.Join(root, "home"), filepath.Join(root, "hooks"),
		filepath.Join(root, "template"), filepath.Join(root, "cwd")
	for _, d := range []string{home, hooks, template, cwd} {
		if err := os.Mkdir(d, 0700); err != nil {
			return nil, err
		}
	}
	askpass := ""
	if r.Remote != nil && r.Remote.Credentials != nil {
		if err := assertTempExecutable(root); err != nil {
			return nil, err
		}
		user, secret, err := r.Remote.Credentials.Credential(r.Remote.Identity)
		if err != nil {
			return nil, fmt.Errorf("git credential unavailable")
		}
		if askpass, err = writeAskpass(root, user, secret); err != nil {
			return nil, err
		}
	}
	full := r.runtimeConfigOverrides(hooks, transport)
	if r.Dir != "" {
		full = append(full, "-C", r.Dir)
	}
	full = append(full, args...)
	cmd := exec.Command(binary, full...)
	cmd.Dir = cwd
	if r.Dir != "" {
		cmd.Dir = r.Dir
	}
	cmd.Env = repositoryGitEnv(home, template, askpass)
	// nil Stdin is /dev/null, so anything that tried to prompt gets EOF.
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
