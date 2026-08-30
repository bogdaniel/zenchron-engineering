package runtime

// GitRunner is the trusted Git execution path for brokered capabilities.
//
// The defect it closes: exec.Command with no Env inherits the whole host
// environment, so ambient GIT_* variables, the user's global Git config,
// credential helpers, askpass programs, an SSH agent, a pager, or an external
// diff program can all steer a Git operation that a model is able to trigger.
// This runner therefore builds the child environment from scratch and never
// consults os.Environ().
//
// Nothing here is model-controlled: the executable is resolved from a constant
// PATH, the environment is a constant list, the working directory is the
// runtime's candidate workspace, and every subcommand and flag is a literal
// supplied by the runtime. Only pathspec arguments derive from model input, and
// they reach this runner already normalized and guarded by ToolBroker.resolve.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// trustedPATH is the same explicit search path the runtime already uses for its
// other trusted tool invocations (see remediation.go). It is used both to find
// the git binary and as the child's PATH, so the binary that runs and the
// binary its subprocesses would find are chosen by the same runtime constant.
const trustedPATH = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

type GitRunner struct {
	// Dir is the runtime-owned workspace the operation runs in. It must be
	// absolute and is never derived from model input.
	Dir string
}

// trustedGitEnv is the complete environment of a trusted Git child process.
// It is a constant list plus two runtime-computed paths; os.Environ() is not
// read, so no host variable can appear here by inheritance.
func trustedGitEnv(home, ceiling string) []string {
	return []string{
		"PATH=" + trustedPATH,
		// Runtime-owned, empty, and removed after the call, so ~/.gitconfig,
		// ~/.netrc, and ~/.ssh of the host user are simply not present.
		"HOME=" + home,
		// Deterministic, locale-independent porcelain and diff output.
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
		// No system, global, or XDG configuration takes effect. /dev/null is a
		// readable, empty config file, which is what "no config" looks like to
		// Git without it having to fall back to a discovered path.
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		// No prompting, no pager, no terminal interaction of any kind.
		"GIT_TERMINAL_PROMPT=0",
		"GIT_PAGER=cat",
		"PAGER=cat",
		// Pathspecs come from model input, so magic pathspec syntax such as
		// ":(exclude)" or ":!" must not be interpreted.
		"GIT_LITERAL_PATHSPECS=1",
		// Repository discovery must not walk out of the workspace: if Dir is
		// not itself a repository the operation fails instead of silently
		// operating on some ancestor checkout.
		"GIT_CEILING_DIRECTORIES=" + ceiling,
		"GIT_OPTIONAL_LOCKS=0",
	}
	// Deliberately absent, and absent by construction rather than by
	// overriding: GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE, GIT_OBJECT_DIRECTORY,
	// GIT_ALTERNATE_OBJECT_DIRECTORIES, GIT_NAMESPACE, GIT_EXTERNAL_DIFF,
	// GIT_DIFF_OPTS, GIT_SSH, GIT_SSH_COMMAND, GIT_ASKPASS, SSH_ASKPASS,
	// SSH_AUTH_SOCK, GIT_PROXY_COMMAND, GIT_EDITOR, GIT_SEQUENCE_EDITOR,
	// XDG_CONFIG_HOME, and every credential-helper variable.
}

// gitBinary resolves git against trustedPATH rather than the host PATH.
// exec.Command would otherwise look the name up in the parent's PATH, which is
// exactly the ambient influence this runner exists to remove.
func gitBinary() (string, error) {
	for _, dir := range filepath.SplitList(trustedPATH) {
		p := filepath.Join(dir, "git")
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("trusted git binary not found on the runtime search path")
}

// run executes one runtime-owned Git command in Dir and returns its stdout.
func (r GitRunner) run(args ...string) ([]byte, error) {
	if !filepath.IsAbs(r.Dir) {
		return nil, fmt.Errorf("trusted git requires an absolute workspace")
	}
	binary, err := gitBinary()
	if err != nil {
		return nil, err
	}
	home, err := os.MkdirTemp("", "zenchron-git-home-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	cmd := exec.Command(binary, append([]string{"-C", r.Dir}, args...)...)
	cmd.Dir = r.Dir
	cmd.Env = trustedGitEnv(home, filepath.Dir(r.Dir))
	// nil Stdin is /dev/null, so anything that tried to prompt gets EOF.
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
