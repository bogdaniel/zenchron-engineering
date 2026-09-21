package runtime

// WHICH RESOURCE a Git invocation will act on.
//
// #241 decides what a command intends to do, from argv alone, and that property
// is worth keeping: argv is immutable, so the classification cannot be raced.
// What argv cannot answer is WHERE that intent lands, and answering it
// everywhere is how the boundary came to govern things it does not protect.
//
// Run run-0bd3b7ab3a773b88566570b7a99a7a0e made 63 refusals. Six were the
// provider's own read probes, zero came from the model, and the rest were the
// candidate's OWN TEST SUITE: `go test ./...` spawns processes that inherit the
// brokered PATH, and those tests create throwaway fixture repositories and
// operate on them deliberately. One of them fails outright - it commits into a
// t.TempDir() fixture that has nothing to do with the governed candidate, and
// the broker refused it because the decision never asked which repository was
// being committed to.
//
// So authority follows the protected resource. Not process ancestry, not
// command shape, not who spawned what.
//
// THE COUNTER-LAW MATTERS AS MUCH. A subprocess must not become a general
// escape hatch: "tests bypass the broker", "children get real Git" and "temp
// directories are trusted" are each one spawn away from unbounded Git over the
// candidate. Nothing here trusts a caller. Every comparison is against identity
// the runtime established itself, before any provider process existed.
//
// This file answers the question only. Nothing here changes what is permitted;
// see git_authority.go for where the answer is used.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// GitTargetClass is the resource a Git invocation resolves to.
type GitTargetClass string

const (
	// GitTargetCandidate is the governed candidate workspace. Everything #241
	// refuses here is still refused.
	GitTargetCandidate GitTargetClass = "candidate_repo"
	// GitTargetRuntimeScratch is the attempt's own temp root - the directory
	// the runtime created, granted, and pointed TMPDIR at, so that a test's
	// t.TempDir() repository lands in an authority scope deliberately permitted
	// rather than accidentally governed.
	GitTargetRuntimeScratch GitTargetClass = "runtime_scratch_repo"
	// GitTargetExternalOrUnknown is everything else, INCLUDING every case the
	// runtime could not resolve. It is not a permission: it is where an
	// operator's unrelated repositories live, and a native operator_trusted
	// worker must not be handed write authority over them merely because a
	// command happened not to point at the candidate.
	GitTargetExternalOrUnknown GitTargetClass = "external_or_unknown"
)

// fileID is a resource identity: the filesystem's, not the path's.
//
// Paths are caller-mutable and the inode is the thing being protected. A pinned
// directory survives its own path being renamed and replaced - the handle
// follows the inode - so comparing identity this way is what makes a symlink, a
// rename or a substituted directory unable to change the answer.
//
// os.SameFile is the portable spelling of "same device and inode". The
// platform struct behind it does not exist on every target this controller
// cross-compiles for, and a resource model that breaks the Windows build is not
// a resource model this repository can carry.
type fileID struct{ info os.FileInfo }

func identify(path string) (fileID, error) {
	info, err := os.Stat(path)
	if err != nil {
		return fileID{}, err
	}
	return fileID{info: info}, nil
}

func (f fileID) valid() bool { return f.info != nil }

func (f fileID) same(other fileID) bool {
	return f.valid() && other.valid() && os.SameFile(f.info, other.info)
}

// repoIdentity is one repository, as the filesystem knows it.
//
// CommonDir is the COMMON git directory, never the per-worktree one. A linked
// worktree of the candidate reports its own git directory -
// .git/worktrees/<name> - while sharing the candidate's refs and object store
// through the common one. Identifying by the per-worktree directory would let
// `git worktree add /tmp/x` mint a location that looks unrelated and is not.
type repoIdentity struct {
	CommonDir, WorkTree string
	CommonID, WorkID    fileID
}

// resolveRepoIdentity asks GIT what repository an execution context resolves
// to, rather than reimplementing discovery.
//
// Git's precedence across --git-dir, GIT_DIR, --work-tree, GIT_WORK_TREE, -C,
// cwd discovery, ceiling directories, .git FILES and bare repositories is
// intricate and moves between versions. A second implementation of it is a
// second opinion that will eventually disagree with the binary actually
// executing - which is the failure ResolveGitCommand already exists to prevent
// for aliases, one layer along.
//
// It is asked through the TRUSTED runner, so the question is not itself
// answerable by anything the provider put on its PATH.
func resolveRepoIdentity(cwd string) (repoIdentity, error) {
	out, err := RepositoryGitRunner{Dir: cwd, Local: controlPolicy()}.run("rev-parse", "--git-common-dir")
	if err != nil {
		return repoIdentity{}, err
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return repoIdentity{}, fmt.Errorf("no common git directory")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(cwd, common)
	}
	identity := repoIdentity{}
	if identity.CommonDir, err = canonicalPath(common); err != nil {
		return repoIdentity{}, err
	}
	if identity.CommonID, err = identify(identity.CommonDir); err != nil {
		return repoIdentity{}, err
	}
	// A BARE REPOSITORY HAS NO WORK TREE, and `rev-parse --show-toplevel`
	// fails rather than answering. That is not an error here: it is one of the
	// two shapes a repository comes in, and the common directory alone decides
	// for it.
	top, topErr := RepositoryGitRunner{Dir: cwd, Local: controlPolicy()}.run("rev-parse", "--show-toplevel")
	if topErr == nil {
		if work := strings.TrimSpace(string(top)); work != "" {
			if identity.WorkTree, err = canonicalPath(work); err != nil {
				return repoIdentity{}, err
			}
			if identity.WorkID, err = identify(identity.WorkTree); err != nil {
				return repoIdentity{}, err
			}
		}
	}
	return identity, nil
}

// canonicalPath resolves every symlink and returns an absolute path, so that
// two spellings of one directory compare equal and a containment test is about
// the filesystem rather than about how the caller wrote it down.
func canonicalPath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return filepath.Abs(resolved)
}

// GitAuthorityAnchors is the identity the RUNTIME established, and against
// which every target is compared.
//
// Nothing a provider says contributes to it. A workspace that cannot be
// identified yields no anchor at all, which classifies every target
// external_or_unknown - so the boundary degrades into refusing more, never into
// permitting more.
type GitAuthorityAnchors struct {
	Candidate     repoIdentity
	ScratchRoot   string
	ScratchRootID fileID
}

// EstablishGitAuthorityAnchors resolves the candidate repository and the
// attempt's scratch root.
//
// Failure is not fatal to the caller: it produces anchors that match nothing,
// and a target matching nothing is external_or_unknown. An absent scratch grant
// is likewise not a default - it is simply no scratch authority, which is what
// a contract with no toolchain scratch should get.
func EstablishGitAuthorityAnchors(candidateDir, scratchDir string) GitAuthorityAnchors {
	anchors := GitAuthorityAnchors{}
	if strings.TrimSpace(candidateDir) != "" {
		if identity, err := resolveRepoIdentity(candidateDir); err == nil {
			anchors.Candidate = identity
		}
	}
	if strings.TrimSpace(scratchDir) != "" {
		if root, err := canonicalPath(scratchDir); err == nil {
			if id, idErr := identify(root); idErr == nil {
				anchors.ScratchRoot, anchors.ScratchRootID = root, id
			}
		}
	}
	return anchors
}

// ClassifyGitTarget decides which resource an execution context acts on.
//
// THE CANDIDATE TEST IS A PAIR, and the pair is not decoration. A mismatched
// git directory and work tree destroys candidate work while REPORTING an
// unrelated git directory:
//
//	git --git-dir=<other>/.git --work-tree=<candidate> checkout -- .
//	  --git-common-dir -> <other>/.git      (looks unrelated)
//	  --show-toplevel  -> <candidate>       (acts on the candidate)
//
// Uncommitted candidate work is gone, and a git-directory-only model permits
// it. So either half matching is enough to make a target the candidate's.
//
// Those redirection flags are refused outright today, and this does not depend
// on relaxing them. The pair is what makes the model correct if they ever are.
//
// WORK TREES OVERLAP IN BOTH DIRECTIONS. A work tree inside the candidate still
// writes candidate files; a work tree that CONTAINS the candidate - `/`, or any
// ancestor - brings it into scope too. A nested repository physically under the
// candidate is therefore the candidate's by this rule and needs no exception:
// its files are paid-for workspace state, and that a runtime commit cannot
// serialize them is a statement about serialization, not about whether they may
// be erased.
func ClassifyGitTarget(target repoIdentity, anchors GitAuthorityAnchors) GitTargetClass {
	if !target.CommonID.valid() {
		return GitTargetExternalOrUnknown
	}
	if target.CommonID.same(anchors.Candidate.CommonID) {
		return GitTargetCandidate
	}
	if worktreesOverlap(target, anchors.Candidate) {
		return GitTargetCandidate
	}
	// THE SCRATCH ROOT MUST HOLD THE WHOLE REPOSITORY. One half inside the
	// attempt's temp root and the other outside it is not a scratch
	// repository; it is a way to spell something else, and it is refused as
	// unknown rather than guessed at.
	if within(target.CommonDir, anchors.ScratchRoot, anchors.ScratchRootID) &&
		(target.WorkTree == "" || within(target.WorkTree, anchors.ScratchRoot, anchors.ScratchRootID)) {
		return GitTargetRuntimeScratch
	}
	return GitTargetExternalOrUnknown
}

// worktreesOverlap answers the ancestry half of the candidate test.
//
// EXACT MATCH IS BY INODE; paths are never compared for it. ANCESTRY cannot be:
// an inode does not know its ancestors, so containment is computed on canonical
// symlink-resolved paths and ANCHORED by re-identifying both endpoints. If an
// endpoint has moved since it was resolved, the honest answer is not "no
// overlap" - it is that the question can no longer be answered, and the
// fail-closed reading of that is the candidate's.
func worktreesOverlap(target, candidate repoIdentity) bool {
	if target.WorkTree == "" || candidate.WorkTree == "" {
		return false
	}
	if target.WorkID.same(candidate.WorkID) {
		return true
	}
	if !anchored(target.WorkTree, target.WorkID) || !anchored(candidate.WorkTree, candidate.WorkID) {
		return true
	}
	return pathContains(candidate.WorkTree, target.WorkTree) || pathContains(target.WorkTree, candidate.WorkTree)
}

// anchored reports whether a canonical path still identifies what it did when
// it was resolved.
func anchored(path string, id fileID) bool {
	if !id.valid() {
		return false
	}
	now, err := identify(path)
	return err == nil && now.same(id)
}

// within reports whether a canonical path lies inside a root whose identity is
// re-checked, so a root swapped underneath answers false - and false here means
// "not scratch", which is the refusing direction.
func within(path, root string, id fileID) bool {
	if path == "" || root == "" || !anchored(root, id) {
		return false
	}
	return pathContains(root, path)
}

// effectiveCwd applies the `-C` globals the way Git does: in order, each one
// relative to the directory the previous ones produced.
//
// The execution context has to be the COMMAND'S OWN. Resolving a target from
// one directory and executing in another is not a smaller version of the
// defect this repair is for - it is a laundering path, because a destructive
// command classified against a scratch repository would land wherever the
// execution context actually pointed.
func effectiveCwd(base string, args []string) (string, error) {
	cwd := base
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			break
		}
		if args[i] != "-C" {
			if !strings.HasPrefix(args[i], "-") {
				break
			}
			continue
		}
		if i+1 >= len(args) {
			return "", fmt.Errorf("-C requires a directory")
		}
		i++
		if filepath.IsAbs(args[i]) {
			cwd = args[i]
			continue
		}
		cwd = filepath.Join(cwd, args[i])
	}
	return cwd, nil
}

// withoutDirectoryGlobals removes the `-C` options the broker has already
// applied by establishing its own working directory, so Git does not apply them
// a second time and land somewhere nobody asked for. Everything else passes
// through untouched.
func withoutDirectoryGlobals(args []string) []string {
	out := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			return append(out, args[i:]...)
		}
		if args[i] == "-C" {
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			return append(out, args[i:]...)
		}
		out = append(out, args[i])
	}
	return out
}
