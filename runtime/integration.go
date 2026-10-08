package runtime

// Integration (#475): the deterministic Git composition for one WorkGraph
// (#472) integration attempt, inside an ordinary runtime-owned candidate
// workspace.
//
// It adds no second Git engine and no second candidate/commit path. Every
// step below reuses what runtime/git.go already established: the local,
// credential-free object transfer MaterializeCandidate uses to move a commit
// between two runtime-owned workspaces, and the merge/ConflictError
// vocabulary IntegrateBase/Rebase already use to classify a Git conflict
// deterministically. This file only sequences those over one
// integration.Contract's canonical plan and reads what Git itself reports.
//
// It grants no merge or release authority. A clean result is a NEW candidate
// that still needs its own fresh admission, assurance and review - exactly
// like any other EngineeringRun's commit - never a subject this file marks
// accepted.
import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/integration"
)

// IntegrationSources resolves one consumed unit id to the runtime-owned
// directory holding its producer's candidate workspace, so the exact commit
// an integration.Contract names can be fetched locally - the same local
// transfer MaterializeCandidate already performs for an upstream candidate
// reference.
type IntegrationSources func(unitID string) (dir string, ok bool)

// IntegrateInputs composes one integration.Contract inside ws.
//
// The caller must already have created ws at the contract's exact
// BaseRevision (CreateCandidateClone) - this function proves that and
// refuses otherwise, it does not choose or clone the base itself. On
// StatusIntegrated, ws's HEAD is the new candidate. On every other outcome,
// and before every attempt begins, ws is unconditionally returned to its
// verified base: a merge this call could not complete, a merge a prior
// crashed attempt left mid-flight, and a commit a crashed attempt already
// made for an earlier step of a multi-input plan are all cleaned up the same
// way, so a restarted attempt always recomposes the whole plan from the same
// clean base rather than compounding an abandoned one or leaving two
// competing partial integrations behind.
func IntegrateInputs(ws *CandidateWorkspace, contract integration.Contract, sources IntegrationSources) (integration.Result, error) {
	if err := contract.Validate(); err != nil {
		return integration.Result{}, err
	}
	// The workspace's OWN construction-time base, not wherever a crashed
	// attempt last left its HEAD, is what proves this is the right
	// workspace for this contract. resetToVerifiedBase below then makes the
	// Git state agree with it unconditionally.
	if ws.BaseRevision != contract.BaseRevision {
		return integration.Result{}, fmt.Errorf(
			"integration workspace was created at base %s, not the contract's verified base %s",
			short12(ws.BaseRevision), short12(contract.BaseRevision))
	}
	if err := resetToVerifiedBase(ws); err != nil {
		return integration.Result{}, err
	}

	for _, step := range contract.Plan() {
		dir, ok := sources(step.UnitID)
		if !ok {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q names no readable producer workspace", step.UnitID)), nil
		}
		if err := fetchExactCommit(ws.Dir, dir, step.Commit, step.Tree); err != nil {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q could not be read at its admitted subject: %v", step.UnitID, err)), nil
		}
		if !isAncestor(ws.Dir, contract.BaseRevision, step.Commit) {
			return integration.Invalidated(contract, fmt.Sprintf(
				"integration input %q (%s) is not a descendant of the verified base revision %s",
				step.UnitID, short12(step.Commit), short12(contract.BaseRevision))), nil
		}
		conflicted, err := mergeFetchedCommit(ws.Dir, step.Commit)
		if err != nil {
			return integration.Result{}, err
		}
		if conflicted {
			paths, err := conflictedPaths(ws.Dir)
			if err != nil {
				return integration.Result{}, err
			}
			if err := resetToVerifiedBase(ws); err != nil {
				return integration.Result{}, err
			}
			detail := fmt.Sprintf("merging %q (%s) conflicts with material already composed from this attempt's earlier inputs",
				step.UnitID, short12(step.Commit))
			return integration.Blocked(contract, integration.NewConflict(integration.ConflictTextual, detail, paths)), nil
		}
	}

	head, err := ws.head()
	if err != nil {
		return integration.Result{}, err
	}
	metadata, err := gitMetadataDigest(ws.Dir)
	if err != nil {
		return integration.Result{}, err
	}
	ws.TrustedMetadata = metadata
	digest, err := contract.Inputs.Digest()
	if err != nil {
		return integration.Result{}, err
	}
	return integration.Integrated(contract, integration.IntegratedCandidate{
		Revision: head.Commit, Tree: head.Tree, InputsDigest: digest,
	}), nil
}

// fetchExactCommit fetches exactly one commit object from a local,
// runtime-owned source workspace and proves the fetched object's tree
// matches what the contract named - the same proof AssertCandidateSubject
// makes for an upstream candidate reference, applied here to one merge
// input before anything merges it.
func fetchExactCommit(dir, sourceDir, commit, tree string) error {
	if !isDir(sourceDir) {
		return fmt.Errorf("producer workspace %s does not exist", sourceDir)
	}
	if _, err := runGit(dir, "fetch", "--no-tags", sourceDir, commit); err != nil {
		return fmt.Errorf("commit %s could not be fetched: %w", short12(commit), err)
	}
	got, err := gitOutput(dir, "rev-parse", commit+"^{tree}")
	if err != nil {
		return err
	}
	if got = strings.TrimSpace(got); got != tree {
		return fmt.Errorf("commit %s has tree %s, not the admitted tree %s", short12(commit), short12(got), short12(tree))
	}
	return nil
}

// isAncestor proves base precedes commit, so the contract's base revision is
// VERIFIED rather than merely stated: an input that does not descend from it
// did not actually start from the base this attempt claims to compose on top
// of.
func isAncestor(dir, base, commit string) bool {
	_, err := runGit(dir, "merge-base", "--is-ancestor", base, commit)
	return err == nil
}

// mergeFetchedCommit merges one already-fetched commit into the current
// head and reports whether Git itself refused it as a conflict - the same
// classification IntegrateBase already uses, reused here rather than
// reinvented as a second conflict path.
func mergeFetchedCommit(dir, commit string) (conflicted bool, err error) {
	if _, err := runGit(dir, "merge", "--no-ff", "--no-edit", commit); err != nil {
		return true, nil
	}
	return false, nil
}

// conflictedPaths reads which paths Git itself reports as unmerged. It must
// be read BEFORE the merge is aborted: aborting restores the index and the
// unmerged markers along with it.
func conflictedPaths(dir string) ([]string, error) {
	out, err := gitOutput(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// resetToVerifiedBase unconditionally returns ws to its verified base
// revision, aborting any merge left in progress first. Called before every
// attempt and after every blocked one, so neither a crash between two steps
// of a multi-input plan nor a conflict on any step but the first ever leaves
// a later attempt composing on top of another attempt's abandoned partial
// progress. It reuses RestoreTrusted, the runtime's existing hard-reset
// recovery, rather than a second reset path.
func resetToVerifiedBase(ws *CandidateWorkspace) error {
	if err := abortLeftoverMerge(ws.Dir); err != nil {
		return err
	}
	return ws.RestoreTrusted()
}

// abortLeftoverMerge cleans up a merge a prior attempt left mid-flight -
// after a crash, or after this file's own conflict path above - so
// RestoreTrusted's hard reset is never attempted while Git still considers a
// merge in progress.
func abortLeftoverMerge(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := runGit(dir, "merge", "--abort")
	return err
}
