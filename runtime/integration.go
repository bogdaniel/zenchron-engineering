package runtime

// Integration (#475): the deterministic Git composition for one WorkGraph
// (#472) integration attempt, inside an ordinary runtime-owned candidate
// workspace.
//
// It adds no second Git engine and no second candidate/commit path. Every
// step below reuses what the runtime already established: the local,
// credential-free object transfer MaterializeCandidate uses to move a commit
// between two runtime-owned workspaces, LocalGitAncestry's exit-status-based
// ancestry classification, and RestoreTrusted's existing hard-reset recovery.
// This file only sequences those over one integration.Contract's canonical
// plan and reads what Git itself reports.
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

// IntegrationSource is what a LIVE resolution of one consumed unit answers:
// where its producer's candidate workspace is, and which handoff is
// currently admitted for it. Both are read fresh by the caller for every
// call - never taken from the contract itself - so a contract built a
// moment earlier is proved still current rather than trusted. A commit that
// remains fetchable from some workspace is not evidence that it is still
// the WorkGraph's current admitted output; the HandoffID comparison below
// is.
type IntegrationSource struct {
	Dir       string
	HandoffID string
}

// IntegrationSources resolves one consumed unit id to its live
// IntegrationSource, so the exact commit an integration.Contract names can
// be fetched locally - the same local transfer MaterializeCandidate already
// performs for an upstream candidate reference - and proved still current.
type IntegrationSources func(unitID string) (IntegrationSource, bool)

// IntegrateInputs composes one integration.Contract inside ws.
//
// The caller must already have created ws at the contract's exact
// BaseRevision (CreateCandidateClone) - this function proves that and
// refuses otherwise, it does not choose or clone the base itself.
//
// Before anything is merged, ws's integrity is checked: a leftover conflict
// from a prior crashed attempt never moved HEAD, so it cannot trip this, and
// is cleaned up unconditionally; anything else that moved HEAD, refs or
// config away from the trusted baseline - a crash after an earlier step of
// a multi-input plan already committed, or genuine tampering - is refused
// rather than silently absorbed. This function never calls the runtime's
// RestoreTrusted recovery on an UNEXPLAINED divergence; only the existing
// FailureWorkspaceIntegrity route, with real operation provenance, decides
// whether and when that recovery runs.
//
// Once composition itself begins, EVERY outcome other than
// StatusIntegrated - a conflict, an invalidated input, or a hard Git error on
// any step - unconditionally returns ws to the base it started this call at,
// undoing any earlier step THIS SAME CALL already merged. Provenance for
// that reset is the call itself: nothing else can have touched ws between a
// commit this function just made and the reset that undoes it.
func IntegrateInputs(ws *CandidateWorkspace, contract integration.Contract, sources IntegrationSources) (integration.Result, error) {
	if err := contract.Validate(); err != nil {
		return integration.Result{}, err
	}
	if ws.BaseRevision != contract.BaseRevision {
		return integration.Result{}, fmt.Errorf(
			"integration workspace was created at base %s, not the contract's verified base %s",
			short12(ws.BaseRevision), short12(contract.BaseRevision))
	}
	// Safe unconditionally: a conflicted merge never moves HEAD, refs or
	// config, so aborting one cannot discard committed progress and cannot
	// change what AssertIntegrity below is about to decide.
	if err := abortLeftoverMerge(ws.Dir); err != nil {
		return integration.Result{}, err
	}
	if err := ws.AssertIntegrity(); err != nil {
		return integration.Result{}, err
	}

	result, err := composeAgainstVerifiedBase(ws, contract, sources)
	if result.Status == integration.StatusIntegrated && err == nil {
		return result, nil
	}
	// Discards whatever THIS call mutated: an input invalidated on step 2+,
	// or a hard Git error partway through the plan, must leave no earlier
	// step's successful merge standing, exactly like a reported conflict
	// already does.
	if resetErr := resetProgressMadeThisCall(ws); resetErr != nil {
		if err != nil {
			return integration.Result{}, fmt.Errorf("%w (cleanup also failed: %v)", err, resetErr)
		}
		return integration.Result{}, resetErr
	}
	return result, err
}

// composeAgainstVerifiedBase performs the plan itself. Its caller owns
// discarding any partial progress on every outcome but a clean integration;
// this function only decides WHICH outcome happened.
func composeAgainstVerifiedBase(ws *CandidateWorkspace, contract integration.Contract, sources IntegrationSources) (integration.Result, error) {
	for _, step := range contract.Plan() {
		source, ok := sources(step.UnitID)
		if !ok {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q names no live producer workspace", step.UnitID)), nil
		}
		if source.HandoffID != step.HandoffID {
			return integration.Invalidated(contract, fmt.Sprintf(
				"integration input %q names admitted handoff %s, which is no longer current (current: %s)",
				step.UnitID, short12(step.HandoffID), short12(source.HandoffID))), nil
		}
		if err := fetchExactCommit(ws.Dir, source.Dir, step.Commit, step.Tree); err != nil {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q could not be read at its admitted subject: %v", step.UnitID, err)), nil
		}
		ancestor, err := LocalGitAncestry(ws.Dir)(contract.BaseRevision, step.Commit)
		if err != nil {
			return integration.Result{}, fmt.Errorf("verifying integration input %q descends from the verified base: %w", step.UnitID, err)
		}
		if !ancestor {
			return integration.Invalidated(contract, fmt.Sprintf(
				"integration input %q (%s) is not a descendant of the verified base revision %s",
				step.UnitID, short12(step.Commit), short12(contract.BaseRevision))), nil
		}
		conflicted, paths, err := mergeFetchedCommit(ws.Dir, step.Commit)
		if err != nil {
			return integration.Result{}, err
		}
		if conflicted {
			detail := fmt.Sprintf("merging %q (%s) conflicts with material already composed from this attempt's earlier inputs",
				step.UnitID, short12(step.Commit))
			conflict, err := integration.NewConflict(integration.ConflictTextual, detail, paths)
			if err != nil {
				return integration.Result{}, fmt.Errorf("integration input %q: %w", step.UnitID, err)
			}
			return integration.Blocked(contract, conflict), nil
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

// mergeFetchedCommit merges one already-fetched commit into the current
// head.
//
// A merge failure is classified from Git's OWN repository state, not from
// the mere presence of an error: it is a conflict only if Git left unmerged
// paths, which is what a conflict IS. A merge that failed for any other
// reason - a bad object, a locked index, an environment failure - is
// aborted and its real cause is returned as an error, never mislabeled a
// conflict.
func mergeFetchedCommit(dir, commit string) (conflicted bool, paths []string, err error) {
	if _, mergeErr := runGit(dir, "merge", "--no-ff", "--no-edit", commit); mergeErr != nil {
		// Read BEFORE aborting: aborting restores the index and the
		// unmerged markers along with it.
		paths, err = conflictedPaths(dir)
		if err != nil {
			return false, nil, err
		}
		if len(paths) == 0 {
			if abortErr := abortLeftoverMerge(dir); abortErr != nil {
				return false, nil, fmt.Errorf("%v (cleanup also failed: %v)", mergeErr, abortErr)
			}
			return false, nil, mergeErr
		}
		return true, paths, nil
	}
	return false, nil, nil
}

// conflictedPaths reads which paths Git itself reports as unmerged.
//
// It reads NUL-delimited (`-z`) output rather than one path per line: Git's
// line-oriented output C-quotes any path with a non-ASCII, control or quote
// byte (`"caf\303\251.txt"`, not the two real bytes of "é"), and these paths
// become the exact set VerifyRemediationScope treats as a remediation's
// permitted scope. A quoted, misspelled path would never match the real
// changed path and would refuse every legitimate fix to that file. `-z`
// Git's own NUL-safe format, never quotes.
func conflictedPaths(dir string) ([]string, error) {
	out, err := gitOutput(dir, "diff", "--name-only", "-z", "--diff-filter=U")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// resetProgressMadeThisCall unconditionally returns ws to its verified base,
// aborting any merge left in progress first. It is called only from within
// one IntegrateInputs call, to discard commits that SAME call made for
// earlier plan steps once a later step is blocked, invalidated or fails -
// provenance this function has directly, since nothing else runs between
// those commits and this reset. It reuses RestoreTrusted, the runtime's
// existing hard-reset recovery, rather than a second reset path.
func resetProgressMadeThisCall(ws *CandidateWorkspace) error {
	if err := abortLeftoverMerge(ws.Dir); err != nil {
		return err
	}
	return ws.RestoreTrusted()
}

// abortLeftoverMerge cleans up a merge left in progress - after a crash, or
// after this file's own conflict path above - so neither AssertIntegrity nor
// a hard reset is ever attempted while Git still considers a merge
// unresolved. A conflicted merge never moves HEAD, refs or config, so this
// alone never changes what AssertIntegrity decides.
func abortLeftoverMerge(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git", "MERGE_HEAD")); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := runGit(dir, "merge", "--abort")
	return err
}
