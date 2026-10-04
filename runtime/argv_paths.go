package runtime

import (
	"path"
	"path/filepath"
	"strings"
)

// LOGICAL PATH REFERENCES IN RECORDED ARGV (#464).
//
// The argv a provider runs with names runtime-owned directories by their host
// spelling, and must. The argv PROVENANCE records is durable and leaves the
// machine with the journal, so it names the same directories by the role the
// runtime gave them instead:
//
//	invocation scratch root        $SCRATCH
//	typed-result directory         $RESULT
//	candidate workspace            $CANDIDATE/<relative>
//	anything else under StateDir   $STATE/<relative>
//	any other absolute path        <host-path>
//
// The projection is built from STRUCTURE, not by scrubbing strings: the
// recorded vector is the spec's own Args function applied to the invocation
// with every path-bearing field replaced by its logical reference. The spec
// that knows a value is the scratch directory is therefore the only thing that
// decides where `$SCRATCH` appears, Codex's writable_roots JSON is encoded from
// the token rather than rewritten after the fact, and the real vector is never
// touched.
//
// An absolute path under no runtime-owned root is recorded as <host-path>,
// never raw. Refusing the invocation instead would block real work on a
// diagnostic projection, which the maintainer decision rules out; the marker
// makes the loss visible where a silent omission would not.

const hostPathMarker = "<host-path>"

type pathRoot struct{ path, token string }

// recorded is the invocation as provenance sees it. A new path-bearing field
// on cliInvocation must be mapped here; the sentinel test in
// argv_paths_test.go fails if one is not.
func (i cliInvocation) recorded(stateDir string) cliInvocation {
	roots := []pathRoot{
		{i.ScratchDir, "$SCRATCH"}, {i.ResultDir, "$RESULT"},
		{i.CandidateDir, "$CANDIDATE"}, {stateDir, "$STATE"},
	}
	r := i
	r.Home = logicalPath(i.Home, roots)
	r.CandidateDir = logicalPath(i.CandidateDir, roots)
	r.ResultDir = logicalPath(i.ResultDir, roots)
	r.ScratchDir = logicalPath(i.ScratchDir, roots)
	r.ModelPreference = logicalArg(i.ModelPreference, roots)
	r.Agent.Model = logicalArg(i.Agent.Model, roots)
	r.RequiredTools = make([]string, len(i.RequiredTools))
	for n, tool := range i.RequiredTools {
		r.RequiredTools[n] = logicalArg(tool, roots)
	}
	return r
}

// recordedArgv is the durable argument vector for one invocation: the spec's
// own builder over the logical invocation, then prompt-redacted and bounded. A
// bare absolute element a spec spelled literally still never lands raw.
func recordedArgv(build func(cliInvocation) []string, i cliInvocation, stateDir string, promptFromEnd int) []string {
	recorded := redactedArgv(build(i.recorded(stateDir)), promptFromEnd)
	for n, arg := range recorded {
		if isHostPath(arg) {
			recorded[n] = hostPathMarker
		}
	}
	return recorded
}

// logicalArg maps a free-form operator value only when it is itself a host
// path; a model name or a tool name passes through unchanged.
func logicalArg(value string, roots []pathRoot) string {
	if !isHostPath(value) {
		return value
	}
	return logicalPath(value, roots)
}

// logicalPath names p by the most specific runtime-owned root containing it,
// matching whole path components, under the spelling given and the one with
// symlinks resolved (macOS's /tmp is /private/tmp). Empty stays empty so a
// spec's "is this grant present" test is unchanged.
func logicalPath(p string, roots []pathRoot) string {
	if p == "" {
		return ""
	}
	best, bestRest := hostPathMarker, -1
	for _, spelling := range pathSpellings(p) {
		for _, root := range roots {
			if root.path == "" {
				continue
			}
			for _, rootSpelling := range pathSpellings(root.path) {
				rest, ok := withinRoot(spelling, rootSpelling)
				if !ok || (bestRest >= 0 && len(rest) >= bestRest) {
					continue
				}
				best, bestRest = root.token, len(rest)
				if rest != "" {
					best += "/" + rest
				}
			}
		}
	}
	return best
}

// withinRoot reports p's remainder below root, on a component boundary:
// /tmp/ab is NOT within /tmp/a.
func withinRoot(p, root string) (string, bool) {
	if p == root {
		return "", true
	}
	prefix := root
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	rest, ok := strings.CutPrefix(p, prefix)
	return rest, ok
}

// pathSpellings is p normalized - forward slashes, cleaned, a lower-case drive
// letter - and, when it resolves, the same with symlinks evaluated.
func pathSpellings(p string) []string {
	spellings := []string{normalizedPath(p)}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if n := normalizedPath(resolved); n != spellings[0] {
			spellings = append(spellings, n)
		}
	}
	return spellings
}

func normalizedPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	if hasDriveRoot(p) {
		p = strings.ToLower(p[:1]) + p[1:]
	}
	return path.Clean(p)
}

// isHostPath reports an absolute path in either host family: a POSIX root, a
// Windows drive root, or a UNC/rooted backslash path.
func isHostPath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, `\`) || hasDriveRoot(s)
}

func hasDriveRoot(s string) bool {
	return len(s) >= 3 && s[1] == ':' && (s[2] == '/' || s[2] == '\\') &&
		(s[0]|0x20) >= 'a' && (s[0]|0x20) <= 'z'
}
