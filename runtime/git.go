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

// CandidateRef is one exact candidate produced by one run, named well enough
// that another run can be given THAT tree and prove it got it.
//
// It exists because a candidate is an INTERNAL artifact before it is ever a
// pull request. The runtime could previously only materialize a workspace by
// cloning the governed remote, so a downstream stage could only receive an
// upstream candidate that had been PUBLISHED - and a candidate whose
// verification failed is never published, which is exactly when a reviewer is
// most needed. The fallback was to leave the consumer on the trusted base,
// which handed an independent reviewer an empty tree and called it a review.
//
// Publication is an authority-controlled external side effect. Internal
// transfer between two runtime-owned workspaces is execution plumbing. This
// type is the boundary between them: it carries no credential, names no remote,
// and authorizes nothing.
type CandidateRef struct {
	// RunID is the producer whose runtime-owned workspace holds the commit.
	RunID string `json:"run_id"`
	// StageID is the plan stage that produced it, for diagnostics that have to
	// name the work rather than the run.
	StageID string `json:"stage_id,omitempty"`
	// Revision and Tree are the EXACT subject. Both are carried because both
	// are checked: a commit id alone would let a rewritten commit answer for
	// the tree a reviewer was asked about.
	Revision string `json:"revision"`
	Tree     string `json:"tree"`
}

// Materializable reports whether this reference names a complete subject. A
// half-filled reference is refused rather than partially trusted: "some of the
// identity matched" is not the question a downstream stage is asking.
func (c CandidateRef) Materializable() bool {
	return c.RunID != "" && c.Revision != "" && c.Tree != ""
}

// MaterializeCandidate fetches one exact upstream candidate into this workspace
// and PROVES the workspace now holds it.
//
// The transfer is a local object fetch between two runtime-owned directories.
// It touches no remote and uses no credential: the producer's workspace is a
// real Git repository that this runtime created, wrote and owns, and the commit
// is already in it.
//
// The proof is the point. Fetching and checking out can fail in ways that leave
// a workspace looking plausible - a ref that resolved to something else, a
// checkout that silently kept the previous head - so HEAD and the tree are both
// read back and compared to the reference. A workspace that cannot be proven to
// be the exact candidate is an error, never a workspace the caller may use:
// the alternative is executing against a subject nobody asked for, which is the
// defect this function exists to close.
func MaterializeCandidate(dir string, ref CandidateRef, sourceDir string) error {
	if !ref.Materializable() {
		return fmt.Errorf("upstream candidate reference is incomplete: run=%q revision=%q tree=%q", ref.RunID, ref.Revision, ref.Tree)
	}
	if _, err := os.Stat(filepath.Join(sourceDir, ".git")); err != nil {
		return fmt.Errorf("upstream run %s has no workspace to take candidate %s from: %w", ref.RunID, short12(ref.Revision), err)
	}
	// --no-tags and an explicit revision: this takes exactly the object asked
	// for and nothing else the producer's workspace happens to carry.
	if _, err := runGit(dir, "fetch", "--no-tags", sourceDir, ref.Revision); err != nil {
		return fmt.Errorf("upstream candidate %s could not be taken from run %s: %w", short12(ref.Revision), ref.RunID, err)
	}
	if _, err := runGit(dir, "checkout", "--detach", ref.Revision); err != nil {
		return fmt.Errorf("upstream candidate %s could not be checked out: %w", short12(ref.Revision), err)
	}
	return AssertCandidateSubject(dir, ref)
}

// AssertCandidateSubject proves a workspace holds the exact candidate named.
//
// It is separate from materialization because it is also asked LATER, before a
// provider executes: materializing at clone time and executing minutes after it
// are different moments, and the question "is this still the tree the
// assignment froze" has to be answerable at the second one.
func AssertCandidateSubject(dir string, ref CandidateRef) error {
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head = strings.TrimSpace(head); head != ref.Revision {
		return fmt.Errorf("workspace is at %s and the assigned upstream candidate is %s", short12(head), short12(ref.Revision))
	}
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	if tree = strings.TrimSpace(tree); tree != ref.Tree {
		return fmt.Errorf("workspace tree is %s and the assigned upstream candidate's tree is %s", short12(tree), short12(ref.Tree))
	}
	return nil
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
	// Excluded names the runtime-owned paths this commit deliberately did not
	// carry, in full. It is never empty silently: a commit that left something
	// in the workspace behind says which paths, so an operator reading the
	// journal sees the same ownership decision the commit made.
	Excluded []string
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
	// After the path gate, so what is joined onto the workspace root here has
	// already been proven to be a safe relative path.
	debris, err := classifyRuntimeDebris(w.Dir, paths)
	if err != nil {
		return CommitResult{}, err
	}
	eligible := withoutPaths(paths, debris.Excluded)
	// The OUTPUT half of the credential boundary. Admission proved the
	// workspace was clean before the producer was shown it; this proves the
	// producer did not introduce a credential value into what is about to
	// become a runtime-owned commit. A value found here is REFUSED, not
	// redacted and not ignored: redacting it would commit a rewritten version
	// of the producer's work, and ignoring it would publish the secret.
	if err := scanPathsForCredentialValues(w.Dir, paths); err != nil {
		return CommitResult{}, err
	}
	// NOTHING BUT DEBRIS IS NOT A CANDIDATE. Committing here would mint an
	// empty-tree commit in the name of work nobody did; the workspace is
	// answered for exactly as an unchanged one is, and the paths that were
	// left behind are named so the answer is not mistaken for "nothing
	// happened".
	if len(eligible) == 0 {
		return CommitResult{}, fmt.Errorf("candidate holds no change a runtime commit can carry, only runtime-owned scratch: %s", quotedPaths(debris.Excluded))
	}
	// THE EXCLUSION IS AN INDEX WRITE, NEVER A WORKTREE WRITE.
	//
	// `git rm --cached` removes an entry and leaves the directory on disk
	// exactly as the producer left it. Nothing here deletes, resets or cleans
	// anything a candidate workspace holds, so the #241 guarantee that dirty
	// candidate work is never discarded is untouched - the runtime declines to
	// RECORD a path it cannot carry, which is a different act from destroying
	// it. It is also why the excluded scratch is still on disk afterwards for
	// an operator to look at.
	//
	// The order matters. Gitlinks already in the index are dropped FIRST, so a
	// path that has stopped being a repository - a producer that staged a
	// gitlink and then renamed the nested `.git` away - is staged as the
	// ordinary content it now is by the `add` below, rather than being carried
	// as an unresolvable gitlink. `-f` is index-only under `--cached`: it
	// covers an entry an interrupted attempt left staged, and can no more
	// touch the worktree than the plain form can.
	//
	// The pathspecs are exact paths. This runner sets GIT_LITERAL_PATHSPECS, so
	// a directory a producer named with a bracket or an asterisk in it names
	// itself and nothing else - which is also why the exclusion cannot be
	// expressed as `add` pathspec magic and is done here instead.
	for _, p := range debris.Unlink {
		if _, err := runGit(w.Dir, "rm", "--cached", "-q", "-f", "--ignore-unmatch", "--", p); err != nil {
			return CommitResult{}, err
		}
	}
	if _, err := runGit(w.Dir, "add", "-A", "--"); err != nil {
		return CommitResult{}, err
	}
	// AFTER the add, because that is what creates the gitlink for a nested
	// repository the producer left behind; removing it beforehand would remove
	// an entry `add -A` immediately puts back.
	for _, p := range debris.Excluded {
		if _, err := runGit(w.Dir, "rm", "--cached", "-q", "-f", "--ignore-unmatch", "--", p); err != nil {
			return CommitResult{}, err
		}
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
	// THE CLEANLINESS PROBE ASKS ABOUT CANDIDATE STATE, NOT ABOUT DEBRIS.
	//
	// Reading the whole status is what refused commit f0f72ba: one excluded
	// scratch repository kept mutating between the commit and the probe, so a
	// commit that had captured every candidate path correctly was thrown away,
	// and every remaining attempt met the same condition. The question the
	// probe exists to ask is whether the runtime's own commit captured the
	// candidate; a path the runtime already decided it does not carry cannot
	// answer that question either way.
	residue, err := dirtyPathsOutside(w.Dir, debris.Excluded)
	if err != nil {
		return CommitResult{}, err
	}
	if len(residue) > 0 {
		return CommitResult{}, fmt.Errorf("candidate not clean after runtime commit: %s", quotedPaths(residue))
	}
	metadata, err := gitMetadataDigest(w.Dir)
	if err != nil {
		return CommitResult{}, err
	}
	w.TrustedMetadata = metadata
	return CommitResult{
		Commit: strings.TrimSpace(commit), Tree: strings.TrimSpace(tree),
		Paths: eligible, Excluded: debris.Excluded,
	}, nil
}

// runtimeDebris is the runtime-owned split of a dirty candidate workspace into
// what a governed commit may carry and what it provably cannot.
//
// Excluded are worktree paths that are THEMSELVES Git repositories. Unlink are
// gitlinks already recorded in this workspace's index. The two overlap and are
// not the same question, which is why they are two fields.
type runtimeDebris struct{ Excluded, Unlink []string }

// classifyRuntimeDebris decides, from the runtime's own reading of the
// workspace, which changed paths a runtime-owned commit cannot carry content
// for - BEFORE anything is staged.
//
// WHAT THE OWNERSHIP FACT IS. A commit cannot carry a nested Git repository.
// `git add -A` does not record the directory's files; it records a 160000
// gitlink naming a commit that exists only inside the nested repository, so the
// content is in no tree this runtime owns. The determination is structural -
// does this path hold its own `.git` - and it is made by the runtime from the
// filesystem and the index. No repository file declares it, no `.gitignore`
// widens or narrows it, and no provider output grants it: a tracked file can
// never be named `.git` because Git refuses that path component in an index,
// so candidate CONTENT cannot manufacture an exclusion.
//
// WHAT MADE IT VISIBLE was a killed attempt: run
// run-5fa7aff09147d45bf7c3a05d504033f7 inherited its own `go test` temporary
// tree on recovery, the runtime wrote commit f0f72ba, and the post-commit
// cleanliness probe then refused it. The probe was catching a sixth of what
// happened. f0f72ba carries SIX gitlinks across two in-tree scratch roots -
// fixture origins, nested candidate workspaces and assurance checkouts - and
// `git status` reported exactly one, because a parent reports a gitlink as
// modified only while the nested worktree is dirty. The other five produced a
// gitlink, a clean probe, and a publishable tree that does not hold the
// content: `git show HEAD:<path>` answers "exists on disk, but not in HEAD".
//
// WHY EXCLUSION RATHER THAN REFUSAL, which is what this was. Refusing the whole
// commit is correct about the gitlink and wrong about the run: the condition is
// a property of the inherited workspace, so it is identical on every remaining
// attempt, and a crashed run therefore lost every candidate edit it had
// actually produced. Exclusion cannot hide a candidate mutation that refusal
// would have preserved, because a commit containing the gitlink carries the
// same zero bytes of that subtree. The only difference between the two is
// whether the rest of the candidate reaches a tree at all.
//
// WHAT EXCLUSION IS NOT ALLOWED TO HIDE, and does not. Tracked candidate
// content inside a directory a producer later ran `git init` on is reported by
// Git under its own paths, never as one nested-repository path, so it never
// arrives here and is committed normally. A NEW file a producer puts inside a
// repository it created is excluded - and was never committable under any
// behaviour this function could have. Every excluded path is carried out in
// CommitResult.Excluded and journalled, so the decision is visible rather than
// quiet.
//
// BOTH WAYS IN ARE STILL CLOSED. A nested repository a producer created is
// untracked, so it appears as a changed path and the worktree answers for it. A
// gitlink ALREADY recorded appears in no changed path at all once its nested
// worktree is clean, so the index is asked too, and the entry is dropped from
// the index before the commit is written. That covers the hostile shape which
// overturned an earlier certification: staging a gitlink and then renaming the
// nested `.git` away makes every worktree predicate answer wrongly, and the
// answer here is to unlink the index entry and let the directory be committed
// as the ordinary content it now is - the producer's files land in the tree
// instead of a gitlink nobody can resolve.
//
// A repository that genuinely uses submodules cannot be a candidate here, and
// that is the honest answer rather than an omission: this runtime cannot show a
// submodule's content to assurance either.
func classifyRuntimeDebris(dir string, paths []string) (runtimeDebris, error) {
	excluded, unlink := map[string]bool{}, map[string]bool{}
	nested := func(p string) bool {
		_, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p), ".git"))
		return err == nil
	}
	for _, p := range paths {
		// Git reports an untracked nested repository as a directory, trailing
		// separator and all, because it does not descend into one.
		if p = strings.TrimSuffix(p, "/"); nested(p) {
			excluded[p] = true
		}
	}
	staged, err := gitOutput(dir, "ls-files", "--stage", "-z")
	if err != nil {
		return runtimeDebris{}, err
	}
	for _, record := range strings.Split(strings.TrimRight(staged, "\x00"), "\x00") {
		if !strings.HasPrefix(record, "160000 ") {
			continue
		}
		tab := strings.IndexByte(record, '\t')
		if tab < 0 {
			continue
		}
		p := record[tab+1:]
		// A gitlink whose directory is GONE is being REMOVED, and `add -A`
		// already stages exactly that. Unlinking it as well would be the same
		// index write twice, and excluding it would block the one way out of
		// this state.
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p))); os.IsNotExist(err) {
			continue
		}
		unlink[p] = true
		if nested(p) {
			excluded[p] = true
		}
	}
	return runtimeDebris{Excluded: sortedPathSet(excluded), Unlink: sortedPathSet(unlink)}, nil
}

func sortedPathSet(set map[string]bool) []string {
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func withinAny(p string, roots []string) bool {
	for _, root := range roots {
		if p == root || strings.HasPrefix(p, root+"/") {
			return true
		}
	}
	return false
}

func withoutPaths(paths, excluded []string) []string {
	if len(excluded) == 0 {
		return paths
	}
	kept := make([]string, 0, len(paths))
	for _, p := range paths {
		if !withinAny(strings.TrimSuffix(p, "/"), excluded) {
			kept = append(kept, p)
		}
	}
	return kept
}

// dirtyPathsOutside is the post-commit question, asked of candidate state only.
func dirtyPathsOutside(dir string, excluded []string) ([]string, error) {
	paths, err := statusPaths(dir, false)
	if err != nil {
		return nil, err
	}
	return withoutPaths(paths, excluded), nil
}

// quotedPaths names paths in runtime evidence. Quoted, because neither
// `status -z` nor `ls-files -z` quotes a path and these messages are
// journalled: a producer that names a directory with a newline in it would
// otherwise write its own line into runtime evidence. No privilege crosses,
// but forgeable evidence is worth less than evidence that cannot be forged.
//
// Every offending path is named. Production had six, and an operator shown one
// example of a workspace-wide condition will go looking for a one-off.
func quotedPaths(paths []string) string {
	named := make([]string, 0, len(paths))
	for _, p := range paths {
		named = append(named, fmt.Sprintf("%q", p))
	}
	sort.Strings(named)
	return strings.Join(named, ", ")
}

func changedPaths(dir string) ([]string, error) { return statusPaths(dir, true) }

// candidateChangedPaths answers "what did the producer change" under the SAME
// ownership rule the commit applies.
//
// Asking it any other way is how a recovered run reached candidate.commit with
// nothing to commit: an inherited scratch repository is a changed path, so
// "the candidate changed" was true, the commit was planned, and the commit then
// had no candidate mutation to carry. Two answers to one question is the defect;
// this is the one answer.
func candidateChangedPaths(dir string) ([]string, error) {
	paths, err := changedPaths(dir)
	if err != nil {
		return nil, err
	}
	debris, err := classifyRuntimeDebris(dir, paths)
	if err != nil {
		return nil, err
	}
	return withoutPaths(paths, debris.Excluded), nil
}

// statusPaths reads one workspace status. refuseIgnored is what tells the two
// callers apart: the pre-commit read refuses an ignored candidate file, because
// a candidate-controlled `.gitignore` must not decide what a runtime commit
// leaves out, while the post-commit probe is only asking what is still dirty.
func statusPaths(dir string, refuseIgnored bool) ([]string, error) {
	args := []string{"status", "--porcelain=v1", "--untracked-files=all", "-z"}
	if refuseIgnored {
		args = append(args, "--ignored=matching")
	}
	out, err := gitOutput(dir, args...)
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
