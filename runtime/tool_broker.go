package runtime

// ToolBroker is the seam a protected provider uses instead of touching the
// filesystem itself:
//
//	reasoning provider -> tool request -> ToolBroker -> candidate operations
//
// Zenchron validates and executes every capability; the model never receives a
// path handle, a shell, or a credential. There is deliberately no credential
// field on this type: provider credentials stay in the control-plane process
// and can therefore never appear in a brokered command's environment.
//
// This pass builds the seam only. It is not a brokered remote-model adapter and
// contains no network model client.

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/analysis"
)

type ToolBroker struct {
	// CandidateDir is the only tree any capability may observe or change.
	CandidateDir string
	// Sandbox is the existing assurance isolation primitive, reused verbatim
	// for bounded command execution. There is no second sandbox.
	Sandbox DockerSandbox
	// MaxBytes bounds what a single brokered path may expose; 0 is unbounded.
	MaxBytes int64
}

// root is the symlink-resolved candidate workspace. Confinement is always
// proven against this, never against the configured string.
func (b ToolBroker) root() (string, error) {
	if b.CandidateDir == "" {
		return "", fmt.Errorf("brokered capability requires a candidate workspace")
	}
	resolved, err := filepath.EvalSymlinks(b.CandidateDir)
	if err != nil {
		return "", fmt.Errorf("candidate workspace unavailable")
	}
	return resolved, nil
}

// resolve is the single validation gate. It reuses
// analysis.NormalizeObservedChange (absolute, traversal, backslash, NUL, and
// Windows-volume paths) and GuardCandidate (sensitive names, symlinked leaves,
// size ceiling, credential-shaped content), then additionally proves that the
// deepest existing ancestor does not symlink out of the workspace, which is
// what an intermediate-directory link would otherwise do. It returns both the
// absolute path and the normalized workspace-relative path; only the latter is
// ever handed to Git, so an unnormalized spelling never reaches a subcommand.
func (b ToolBroker) resolve(rel string) (string, string, error) {
	root, err := b.root()
	if err != nil {
		return "", "", err
	}
	normalized, err := analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: []string{rel}, PathsKnown: true})
	if err != nil {
		return "", "", fmt.Errorf("unsafe brokered path %q", rel)
	}
	cleaned := normalized.Paths[0]
	// .git holds workspace VCS state, including remote URLs that can carry
	// credentials. It is runtime concern, never brokered model input.
	if cleaned == ".git" || strings.HasPrefix(cleaned, ".git/") {
		return "", "", fmt.Errorf("brokered path %q is runtime-owned workspace state", rel)
	}
	if err := GuardCandidate(root, []string{cleaned}, b.MaxBytes); err != nil {
		return "", "", err
	}
	full := filepath.Join(root, cleaned)
	existing := full
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing || len(parent) < len(root) {
			return "", "", fmt.Errorf("brokered path %q escapes the candidate workspace", rel)
		}
		existing = parent
	}
	real, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", "", fmt.Errorf("brokered path %q is not resolvable inside the candidate workspace", rel)
	}
	if real != root && !strings.HasPrefix(real, root+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("brokered path %q escapes the candidate workspace", rel)
	}
	return full, cleaned, nil
}

// ReadFile is the repository read capability.
func (b ToolBroker) ReadFile(rel string) ([]byte, error) {
	full, _, err := b.resolve(rel)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

type SearchHit struct {
	Path string
	Line int
	Text string
}

// Search is the repository search capability. Scope is optional and, when
// given, is itself validated. WalkDir never follows symlinks, and every hit is
// re-read through ReadFile, so a guarded-out file is simply not searchable.
func (b ToolBroker) Search(pattern string, scope []string) ([]SearchHit, error) {
	expr, err := regexp.Compile(pattern)
	if err != nil {
		return nil, fmt.Errorf("invalid brokered search pattern")
	}
	root, err := b.root()
	if err != nil {
		return nil, err
	}
	roots := []string{root}
	if len(scope) > 0 {
		roots = nil
		for _, rel := range scope {
			full, _, err := b.resolve(rel)
			if err != nil {
				return nil, err
			}
			roots = append(roots, full)
		}
	}
	var hits []SearchHit
	for _, dir := range roots {
		walkErr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == ".git" {
					return fs.SkipDir
				}
				return nil
			}
			rel, relErr := filepath.Rel(root, p)
			if relErr != nil {
				return relErr
			}
			data, readErr := b.ReadFile(filepath.ToSlash(rel))
			if readErr != nil {
				return nil
			}
			for i, line := range strings.Split(string(data), "\n") {
				if expr.MatchString(line) {
					hits = append(hits, SearchHit{Path: filepath.ToSlash(rel), Line: i + 1, Text: line})
				}
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return hits, nil
}

// Diff is the candidate diff capability, scoped to validated pathspecs.
//
// --no-ext-diff refuses any external diff driver, whether it came from
// GIT_EXTERNAL_DIFF, diff.external, or a .gitattributes "diff=<driver>"
// attribute paired with a diff.<driver>.command; --no-textconv refuses the
// textconv programs that would otherwise run per blob. Neither can be reached
// through the environment either, because GitRunner builds it from scratch.
func (b ToolBroker) Diff(paths []string) (string, error) {
	root, err := b.root()
	if err != nil {
		return "", err
	}
	args := []string{"diff", "--no-ext-diff", "--no-textconv"}
	if len(paths) > 0 {
		args = append(args, "--")
		for _, rel := range paths {
			_, cleaned, err := b.resolve(rel)
			if err != nil {
				return "", err
			}
			args = append(args, cleaned)
		}
	}
	out, err := GitRunner{Dir: root}.run(args...)
	return string(out), err
}

// candidateMutation serializes brokered mutation of a candidate workspace, so
// the change observed after an apply is attributable to that apply.
// ponytail: one process-wide lock; make it per-workspace only if the runtime
// ever brokers two candidates concurrently.
var candidateMutation sync.Mutex

// workspaceChanges is the observation step: the set of paths Git itself reports
// as changed in the workspace. It is the authority on what a patch did, in the
// same way --numstat is the authority on what a patch claims to do.
func workspaceChanges(git GitRunner) (map[string]bool, error) {
	out, err := git.run("status", "--porcelain=v1", "--untracked-files=all", "-z")
	if err != nil {
		return nil, err
	}
	changed := map[string]bool{}
	for _, record := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		if len(record) < 4 {
			continue
		}
		changed[record[3:]] = true
	}
	return changed, nil
}

// ApplyPatch is the apply-patch capability. Provider-declared paths are never
// trusted: the affected paths are enumerated by Git itself (--numstat, which
// applies nothing), every one of them goes through resolve, --check proves the
// patch applies without touching the workspace, and after the apply the
// resulting workspace change is observed and compared against exactly that
// validated set.
func (b ToolBroker) ApplyPatch(patch []byte) error {
	root, err := b.root()
	if err != nil {
		return err
	}
	// The one dialect this runtime refuses BY NAME. It is not translated: a
	// second, permissive patch interpreter is exactly what would make the
	// applied change something other than what Git parsed. Naming it is what
	// lets a model correct itself in one turn instead of spending its whole
	// iteration budget on a grammar Git will never accept.
	if isBeginPatchDialect(patch) {
		return &PatchError{Stage: "parse", Detail: beginPatchRefusal}
	}
	git := GitRunner{Dir: root}
	file, err := os.CreateTemp("", "zenchron-brokered-*.patch")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(patch); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	// Every stage below uses the SAME flags, so what is discovered, what is
	// checked, and what is applied cannot disagree about the patch.
	//
	// --recount tells Git to derive each hunk's line counts from the hunk BODY
	// instead of trusting the @@ -old,count +new,count @@ header. The body stays
	// authoritative and context is still verified exactly as before - a patch
	// whose context does not match the workspace still fails - but a model that
	// got the arithmetic in a header wrong no longer has an otherwise coherent
	// diff rejected at parse time, which is what consumed most of the fourth
	// dogfood's reasoning iterations. Git remains the only parser: nothing here
	// interprets, rewrites, or repairs the patch text.
	listed, err := git.run("apply", "--numstat", "-z", "--recount", file.Name())
	if err != nil {
		return &PatchError{Stage: "parse", Detail: sanitizeGitDiagnostic(err, root, file.Name())}
	}
	validated := map[string]bool{}
	for _, record := range strings.Split(string(listed), "\x00") {
		// A -z numstat record is "added\tdeleted\t" followed by the raw path as
		// its own record, so only records without the count prefix are paths.
		if added := strings.Index(record, "\t"); added >= 0 {
			if deleted := strings.Index(record[added+1:], "\t"); deleted >= 0 {
				record = record[added+1+deleted+1:]
			}
		}
		if record == "" {
			continue
		}
		_, cleaned, err := b.resolve(record)
		if err != nil {
			return err
		}
		validated[cleaned] = true
	}
	// A real dry run. --check parses and tests every hunk against the
	// workspace and writes nothing, so a patch that would fail half-applied is
	// refused before the runtime takes the mutation lock.
	if _, err := git.run("apply", "--check", "--recount", file.Name()); err != nil {
		return &PatchError{Stage: "check", Detail: sanitizeGitDiagnostic(err, root, file.Name())}
	}
	candidateMutation.Lock()
	defer candidateMutation.Unlock()
	before, err := workspaceChanges(git)
	if err != nil {
		return err
	}
	if _, err := git.run("apply", "--recount", file.Name()); err != nil {
		return &PatchError{Stage: "apply", Detail: sanitizeGitDiagnostic(err, root, file.Name())}
	}
	after, err := workspaceChanges(git)
	if err != nil {
		return err
	}
	for path := range after {
		if before[path] || validated[path] {
			continue
		}
		// The workspace no longer matches what was validated. The runtime, not
		// this capability, owns recovery: CandidateWorkspace.RestoreTrusted
		// resets to the trusted baseline.
		return fmt.Errorf("brokered patch changed unvalidated path %q", path)
	}
	return nil
}

// RunCommand is the bounded command capability. It goes through the existing
// DockerSandbox: dockerBase mounts the candidate workspace and nothing else, so
// runtime state, the controller checkout, and other runs' state are absent, the
// environment is an explicit two-entry allowlist that cannot carry provider
// credentials, and networking is off. There is no option to enable it.
func (b ToolBroker) RunCommand(ctx context.Context, command []string) (CommandOutput, error) {
	if len(command) == 0 {
		return CommandOutput{}, fmt.Errorf("brokered command required")
	}
	root, err := b.root()
	if err != nil {
		return CommandOutput{}, err
	}
	args := append(dockerBase(root, false), "--workdir", "/candidate", b.Sandbox.Image)
	return b.Sandbox.run(ctx, append(args, command...))
}

// PatchError is a brokered patch failure that carries a bounded, sanitized
// diagnostic. The previous behaviour - "brokered patch is not a readable
// patch" - threw away everything a model needed to recover, so it re-sent the
// same broken patch until its iteration budget ran out. What is returned now is
// Git's own message about the path and hunk, with every host detail removed.
type PatchError struct {
	// Stage is which of the three identical-flag Git invocations refused:
	// parse (numstat), check (dry run), or apply.
	Stage  string
	Detail string
}

func (e *PatchError) Error() string {
	if e.Detail == "" {
		return "brokered patch refused at " + e.Stage
	}
	return "brokered patch refused at " + e.Stage + ": " + e.Detail
}

// beginPatchRefusal is deterministic text, not a template: the same refusal for
// the same mistake every time, so a model sees a stable correction.
const beginPatchRefusal = `candidate.apply_patch requires a git-compatible unified diff; ` +
	`"*** Begin Patch"/"*** Update File" syntax is not accepted. ` +
	`Send either "--- a/path", "+++ b/path", "@@ ..." or a full "diff --git a/path b/path" patch.`

// isBeginPatchDialect recognizes the envelope markers of the *** Begin Patch
// dialect. It only DETECTS: nothing translates it, and a patch carrying these
// markers is refused whole rather than partially interpreted.
func isBeginPatchDialect(patch []byte) bool {
	for _, line := range strings.Split(string(patch), "\n") {
		switch strings.TrimSpace(line) {
		case "*** Begin Patch", "*** End Patch":
			return true
		}
		trimmed := strings.TrimSpace(line)
		for _, marker := range []string{"*** Update File:", "*** Add File:", "*** Delete File:", "*** Move to:"} {
			if strings.HasPrefix(trimmed, marker) {
				return true
			}
		}
	}
	return false
}

// sanitizeGitDiagnostic turns a trusted-Git failure into something safe to hand
// back to a model. Git names the workspace root and the temporary patch file in
// its messages, and both are host paths; they are replaced by placeholders, any
// remaining absolute path is dropped, the existing transcript redactor removes
// credential-shaped text, and the result is bounded by the same field ceiling
// every durable diagnostic uses. Repository-relative paths and line numbers -
// the part a model actually needs - survive.
func sanitizeGitDiagnostic(err error, root, patchPath string) string {
	if err == nil {
		return ""
	}
	detail := err.Error()
	for placeholder, actual := range map[string]string{"<patch>": patchPath, "<candidate>": root} {
		if actual != "" {
			detail = strings.ReplaceAll(detail, actual, placeholder)
		}
	}
	detail = string(redactTranscript([]byte(detail)))
	// Anything still absolute is a host path this boundary never promised to
	// expose, so it is removed rather than trimmed.
	fields := strings.Fields(detail)
	kept := fields[:0]
	for _, field := range fields {
		if strings.HasPrefix(field, "/") || strings.Contains(field, "/.git/") {
			field = "<path>"
		}
		kept = append(kept, field)
	}
	return boundedDetail(strings.Join(kept, " "))
}
