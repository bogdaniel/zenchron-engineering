package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/analysis"
)

type RepositoryTarget struct{ Identity, Remote, DefaultBranch string }

// ResolveRepository is intentionally conservative: an explicit identity wins;
// cwd inference requires exactly one origin URL with a recognizable identity.
func ResolveRepository(cwd, explicit string) (RepositoryTarget, error) {
	remote, err := gitOutput(cwd, "remote", "get-url", "origin")
	if err != nil {
		return RepositoryTarget{}, fmt.Errorf("repository target: %w", err)
	}
	identity, ok := githubIdentity(strings.TrimSpace(remote))
	if explicit != "" {
		identity = explicit
		ok = true
	}
	if !ok || identity == "" {
		return RepositoryTarget{}, fmt.Errorf("ambiguous origin; specify --repo owner/name")
	}
	branch, err := gitOutput(cwd, "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		branch = "origin/main"
	}
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "origin/")
	return RepositoryTarget{Identity: identity, Remote: strings.TrimSpace(remote), DefaultBranch: branch}, nil
}
func githubIdentity(remote string) (string, bool) {
	remote = strings.TrimSuffix(remote, ".git")
	if i := strings.Index(remote, "github.com/"); i >= 0 {
		p := strings.Split(strings.TrimPrefix(remote[i:], "github.com/"), "/")
		if len(p) == 2 && p[0] != "" && p[1] != "" {
			return p[0] + "/" + p[1], true
		}
	}
	return "", false
}

type WorkspaceIntegrityError struct{ Detail string }

func (e *WorkspaceIntegrityError) Error() string { return "workspace_integrity_violation: " + e.Detail }

type ConflictError struct{ Operation string }

func (e *ConflictError) Error() string { return e.Operation + " conflict" }

// Remote is the governed remote this workspace was cloned from. Network
// operations must resolve to exactly this remote.
//
// Credentials is the repository control-plane authorization this workspace was
// CONSTRUCTED with; there is no process-wide credential seam. It is supplied by
// the operator layer through Dependencies.Credentials, so two runtimes in one
// process hold two independent authorities and neither can reach the other's.
// It is never populated from repository config, from a candidate, from a
// provider, or from a tool argument, and the secret it holds reaches Git only
// through the runtime-owned askpass in repository_git.go.
type CandidateWorkspace struct {
	Dir, BaseRevision, TrustedMetadata, Remote string
	Credentials                                CredentialProvider
}

// CreateCandidateClone makes a full clone with independent .git metadata; it
// never uses git worktree, whose metadata is shared with its controller.
func CreateCandidateClone(stateDir, runID, remote, base string, credentials CredentialProvider) (CandidateWorkspace, error) {
	if runID == "" || remote == "" || base == "" {
		return CandidateWorkspace{}, fmt.Errorf("run, remote, and base are required")
	}
	dir := filepath.Join(stateDir, "runs", runID, "candidate")
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return CandidateWorkspace{}, err
	}
	if _, err := os.Stat(dir); err == nil {
		return CandidateWorkspace{}, fmt.Errorf("candidate workspace already exists")
	}
	identity, err := GovernedRemote(remote)
	if err != nil {
		return CandidateWorkspace{}, err
	}
	if _, err := remoteGit("", identity, credentials).run("clone", "--no-checkout", identity.URL, dir); err != nil {
		return CandidateWorkspace{}, err
	}
	if _, err := runGit(dir, "checkout", "--detach", base); err != nil {
		return CandidateWorkspace{}, err
	}
	if _, err := runGit(dir, "config", "user.name", "Zenchron Runtime"); err != nil {
		return CandidateWorkspace{}, err
	}
	if _, err := runGit(dir, "config", "user.email", "runtime@zenchron.invalid"); err != nil {
		return CandidateWorkspace{}, err
	}
	d, err := gitMetadataDigest(dir)
	if err != nil {
		return CandidateWorkspace{}, err
	}
	return CandidateWorkspace{
		Dir: dir, BaseRevision: base, TrustedMetadata: d, Remote: identity.URL,
		Credentials: credentials,
	}, nil
}
func (w CandidateWorkspace) AssertIntegrity() error {
	got, err := gitMetadataDigest(w.Dir)
	if err != nil {
		return err
	}
	if got != w.TrustedMetadata {
		return &WorkspaceIntegrityError{Detail: "Git metadata changed outside runtime"}
	}
	return nil
}

// RestoreTrusted is containment, not a legitimate mutation: it runs only after
// an integrity violation, on an operation that is already failing. The refreshed
// baseline therefore stays in memory and is never journalled - persisting it
// would let a tampered workspace re-baseline itself by tripping the check.
func (w *CandidateWorkspace) RestoreTrusted() error {
	if _, err := runGit(w.Dir, "reset", "--hard", w.BaseRevision); err != nil {
		return err
	}
	if _, err := runGit(w.Dir, "clean", "-fdx"); err != nil {
		return err
	}
	metadata, err := gitMetadataDigest(w.Dir)
	if err == nil {
		w.TrustedMetadata = metadata
	}
	return err
}

// FetchBase is runtime-owned metadata mutation; it refreshes the integrity
// baseline only after Git itself reports a successful fetch.
// boundRemote resolves a remote name against the workspace's own configuration
// and refuses anything that is not the governed remote this workspace was
// created from. A remote supplied by a model, a provider, or a mutated
// repository config therefore cannot redirect a network operation.
func (w CandidateWorkspace) boundRemote(remote string) (RemoteIdentity, error) {
	target := remote
	if !strings.ContainsAny(remote, "/:") {
		out, err := gitOutput(w.Dir, "remote", "get-url", remote)
		if err != nil {
			return RemoteIdentity{}, err
		}
		target = strings.TrimSpace(out)
	}
	identity, err := GovernedRemote(target)
	if err != nil {
		return RemoteIdentity{}, err
	}
	if w.Remote == "" {
		return identity, nil
	}
	// Both sides go through the SAME classifier and are compared as governed
	// IDENTITIES. Comparing the strings instead is what made two spellings of
	// one repository - the operator checkout's origin with .git, the run's
	// candidate origin without - into two authorities.
	governed, err := GovernedRemote(w.Remote)
	if err != nil {
		return RemoteIdentity{}, err
	}
	if !identity.Same(governed) {
		return RemoteIdentity{}, &GovernedRemoteMismatchError{Governed: governed.URL, Observed: identity.URL}
	}
	return identity, nil
}

func (w *CandidateWorkspace) FetchBase(remote string) error {
	if err := w.AssertIntegrity(); err != nil {
		return err
	}
	identity, err := w.boundRemote(remote)
	if err != nil {
		return err
	}
	if _, err := remoteGit(w.Dir, identity, w.Credentials).run("fetch", "--no-recurse-submodules", remote); err != nil {
		return err
	}
	metadata, err := gitMetadataDigest(w.Dir)
	if err == nil {
		w.TrustedMetadata = metadata
	}
	return err
}
func gitMetadataDigest(dir string) (string, error) {
	// --local keeps the baseline on the runtime-owned repository file. System
	// and global config are switched off for every runtime Git call, and the
	// runtime's own -c overrides would otherwise appear as "command line:"
	// entries and make the digest depend on per-call temporary paths.
	config, err := gitOutput(dir, "config", "--list", "--show-origin", "--local")
	if err != nil {
		return "", err
	}
	refs, err := gitOutput(dir, "for-each-ref", "--format=%(refname):%(objectname)")
	if err != nil {
		return "", err
	}
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return "", err
	}
	h := sha256.Sum256([]byte(config + "\n" + refs + "\n" + head))
	return hex.EncodeToString(h[:]), nil
}

type CommitResult struct {
	Commit, Tree string
	Paths        []string
}

func (w *CandidateWorkspace) Commit(message string, maxBytes int64) (CommitResult, error) {
	if err := w.AssertIntegrity(); err != nil {
		return CommitResult{}, err
	}
	paths, err := changedPaths(w.Dir)
	if err != nil {
		return CommitResult{}, err
	}
	if len(paths) == 0 {
		return CommitResult{}, fmt.Errorf("candidate has no changes")
	}
	if err := GuardCandidate(w.Dir, paths, maxBytes); err != nil {
		return CommitResult{}, err
	}
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		return CommitResult{}, err
	}
	if _, err := runGit(w.Dir, "commit", "--no-gpg-sign", "-m", message); err != nil {
		return CommitResult{}, err
	}
	commit, err := gitOutput(w.Dir, "rev-parse", "HEAD")
	if err != nil {
		return CommitResult{}, err
	}
	tree, err := gitOutput(w.Dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return CommitResult{}, err
	}
	status, err := gitOutput(w.Dir, "status", "--porcelain=v1")
	if err != nil {
		return CommitResult{}, err
	}
	if strings.TrimSpace(status) != "" {
		return CommitResult{}, fmt.Errorf("candidate not clean after runtime commit")
	}
	metadata, err := gitMetadataDigest(w.Dir)
	if err != nil {
		return CommitResult{}, err
	}
	w.TrustedMetadata = metadata
	return CommitResult{Commit: strings.TrimSpace(commit), Tree: strings.TrimSpace(tree), Paths: paths}, nil
}
func changedPaths(dir string) ([]string, error) {
	out, err := gitOutput(dir, "status", "--porcelain=v1", "--untracked-files=all", "--ignored=matching", "-z")
	if err != nil {
		return nil, err
	}
	records := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var paths []string
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if rec == "" {
			continue
		}
		if strings.HasPrefix(rec, "!! ") {
			return nil, fmt.Errorf("ignored candidate file %q", rec[3:])
		}
		if len(rec) < 4 {
			continue
		}
		status := rec[:2]
		p := rec[3:]
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			// -z emits the original path as the following record; the
			// non-z code only kept the destination, so preserve that.
			i++
		}
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths, nil
}
func (w CandidateWorkspace) ObservedChange(result CommitResult) (analysis.ObservedChange, error) {
	return analysis.NormalizeObservedChange(analysis.ObservedChange{Paths: result.Paths, PathsKnown: true})
}
func (w *CandidateWorkspace) Rebase(base string) (CommitResult, error) {
	if err := w.AssertIntegrity(); err != nil {
		return CommitResult{}, err
	}
	if _, err := runGit(w.Dir, "rebase", base); err != nil {
		_ = runGitIgnore(w.Dir, "rebase", "--abort")
		return CommitResult{}, &ConflictError{Operation: "rebase"}
	}
	result, err := w.head()
	if err != nil {
		return result, err
	}
	w.TrustedMetadata, err = gitMetadataDigest(w.Dir)
	return result, err
}

// IntegrateBase merges rather than force-pushing after publication. Publishing is
// intentionally absent: this method only prepares a local candidate head.
func (w *CandidateWorkspace) IntegrateBase(base string, published bool) (CommitResult, error) {
	if !published {
		return w.Rebase(base)
	}
	if err := w.AssertIntegrity(); err != nil {
		return CommitResult{}, err
	}
	if _, err := runGit(w.Dir, "merge", "--no-edit", base); err != nil {
		_ = runGitIgnore(w.Dir, "merge", "--abort")
		return CommitResult{}, &ConflictError{Operation: "merge_from_base"}
	}
	result, err := w.head()
	if err != nil {
		return result, err
	}
	w.TrustedMetadata, err = gitMetadataDigest(w.Dir)
	return result, err
}
func (w CandidateWorkspace) head() (CommitResult, error) {
	c, e := gitOutput(w.Dir, "rev-parse", "HEAD")
	if e != nil {
		return CommitResult{}, e
	}
	t, e := gitOutput(w.Dir, "rev-parse", "HEAD^{tree}")
	if e != nil {
		return CommitResult{}, e
	}
	return CommitResult{Commit: strings.TrimSpace(c), Tree: strings.TrimSpace(t)}, nil
}
func gitOutput(dir string, args ...string) (string, error) {
	b, e := runGit(dir, args...)
	return string(b), e
}

// runGit is the trusted local repository-control path. It never reaches a
// network: every protocol is "never" except the explicit filesystem capability
// a local clone needs. Remote operations must go through remoteGit, which binds
// them to a governed RemoteIdentity.
func runGit(dir string, args ...string) ([]byte, error) {
	return RepositoryGitRunner{Dir: dir, Local: controlPolicy()}.run(args...)
}

// remoteGit binds one network-capable operation to a governed RemoteIdentity
// and to the credential the caller was constructed with. The credential is a
// parameter, never a package variable: nothing a process installs globally can
// widen, narrow, or swap the authority of an already-constructed workspace.
func remoteGit(dir string, identity RemoteIdentity, credentials CredentialProvider) RepositoryGitRunner {
	return RepositoryGitRunner{
		Dir:    dir,
		Local:  controlPolicy(),
		Remote: &RemotePolicy{Identity: identity, Credentials: credentials},
	}
}
func runGitIgnore(dir string, args ...string) error { _, err := runGit(dir, args...); return err }
