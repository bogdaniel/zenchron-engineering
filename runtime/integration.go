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
	"fmt"
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
// BaseRevision (CreateCandidateClone). This function does not trust that
// claim, or ws's own BaseRevision field: a legitimate later operation can
// advance a workspace's HEAD - and correctly refresh its trusted metadata
// baseline along with it - without this contract ever being told, so
// AssertIntegrity alone cannot prove ws is AT THE BASE THIS CONTRACT NAMES,
// only that nothing untracked has happened to it since whenever it was last
// trusted. IntegrateInputs additionally proves, directly against Git, that
// HEAD is exactly the contract's base and that the worktree and index are
// completely clean - no modified, staged, untracked or unmerged content -
// before composing anything. Anything else - a crash after an earlier step
// already committed, a workspace some other operation has since moved on,
// or genuine tampering - is refused outright.
//
// That precondition is also what makes this function's own destructive
// cleanup (RestoreTrusted's `git clean -fdx`) safe to use at all: starting
// from a provably pristine, exact-base workspace means there is never
// legitimate pre-existing material for it to lose. Once composition begins,
// every outcome other than StatusIntegrated unconditionally returns ws to
// that base, undoing any earlier step THIS SAME CALL already merged - but
// ONLY if this call actually advanced HEAD; a failure before any step
// committed leaves the already-pristine workspace untouched rather than
// running a destructive reset that was never needed.
func IntegrateInputs(ws *CandidateWorkspace, contract integration.Contract, sources IntegrationSources) (integration.Result, error) {
	if err := contract.Validate(); err != nil {
		return integration.Result{}, err
	}
	if ws.BaseRevision != contract.BaseRevision {
		return integration.Result{}, fmt.Errorf(
			"integration workspace was created at base %s, not the contract's verified base %s",
			short12(ws.BaseRevision), short12(contract.BaseRevision))
	}
	if err := ws.AssertIntegrity(); err != nil {
		return integration.Result{}, err
	}
	if err := assertCleanAtVerifiedBase(ws.Dir, contract.BaseRevision); err != nil {
		return integration.Result{}, err
	}

	result, committed, err := composeAgainstVerifiedBase(ws, contract, sources)
	if result.Status == integration.StatusIntegrated && err == nil {
		return result, nil
	}
	if !committed {
		// Nothing this call did advanced HEAD, so the workspace is still
		// exactly the pristine, verified base assertCleanAtVerifiedBase
		// just proved it was: there is nothing to discard, and running the
		// destructive hard reset anyway would risk nothing legitimate only
		// because there was nothing there to begin with - never a reason to
		// run it unnecessarily.
		return result, err
	}
	if resetErr := ws.RestoreTrusted(); resetErr != nil {
		if err != nil {
			return integration.Result{}, fmt.Errorf("%w (cleanup also failed: %v)", err, resetErr)
		}
		return integration.Result{}, resetErr
	}
	return result, err
}

// composeAgainstVerifiedBase performs the plan itself, and reports whether
// it advanced ws's HEAD at all. Its caller owns discarding any such progress
// on every outcome but a clean integration; this function only decides
// WHICH outcome happened and whether there is anything to discard.
func composeAgainstVerifiedBase(ws *CandidateWorkspace, contract integration.Contract, sources IntegrationSources) (result integration.Result, committed bool, err error) {
	for _, step := range contract.Plan() {
		source, ok := sources(step.UnitID)
		if !ok {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q names no live producer workspace", step.UnitID)), committed, nil
		}
		if source.HandoffID != step.HandoffID {
			return integration.Invalidated(contract, fmt.Sprintf(
				"integration input %q names admitted handoff %s, which is no longer current (current: %s)",
				step.UnitID, short12(step.HandoffID), short12(source.HandoffID))), committed, nil
		}
		if err := fetchExactCommit(ws.Dir, source.Dir, step.Commit, step.Tree); err != nil {
			return integration.Invalidated(contract,
				fmt.Sprintf("integration input %q could not be read at its admitted subject: %v", step.UnitID, err)), committed, nil
		}
		ancestor, err := LocalGitAncestry(ws.Dir)(contract.BaseRevision, step.Commit)
		if err != nil {
			return integration.Result{}, committed, fmt.Errorf("verifying integration input %q descends from the verified base: %w", step.UnitID, err)
		}
		if !ancestor {
			return integration.Invalidated(contract, fmt.Sprintf(
				"integration input %q (%s) is not a descendant of the verified base revision %s",
				step.UnitID, short12(step.Commit), short12(contract.BaseRevision))), committed, nil
		}
		conflicted, advanced, paths, mergeErr := mergeFetchedCommit(ws.Dir, step.Commit)
		if mergeErr != nil {
			return integration.Result{}, committed, mergeErr
		}
		if conflicted {
			// Clears the in-progress merge Git itself is still holding -
			// never destructive of pre-existing material, since `merge
			// --abort` only reverts what THIS merge attempt touched - so
			// the workspace holds no dangling merge state regardless of
			// whether the caller goes on to run the separate, committed-
			// gated destructive reset below.
			if err := abortMergeInProgress(ws.Dir); err != nil {
				return integration.Result{}, committed, err
			}
			detail := fmt.Sprintf("merging %q (%s) conflicts with material already composed from this attempt's earlier inputs",
				step.UnitID, short12(step.Commit))
			conflict, err := integration.NewConflict(integration.ConflictTextual, detail, paths)
			if err != nil {
				return integration.Result{}, committed, fmt.Errorf("integration input %q: %w", step.UnitID, err)
			}
			return integration.Blocked(contract, conflict), committed, nil
		}
		// A clean merge Git itself treats as a no-op ("Already up to
		// date" - this step's commit was already fully contained in
		// HEAD) succeeds without advancing anything; committed tracks
		// whether HEAD actually moved, never merely whether a merge
		// was attempted and did not conflict.
		if advanced {
			committed = true
		}
	}

	if !committed {
		// Every input was already fully contained in the verified base:
		// nothing was actually composed, so there is no new subject to
		// report integrated - reporting one here would silently hand
		// back the base itself as if it were new (requirement: the
		// output is a NEW exact commit/tree).
		return integration.Result{}, committed, fmt.Errorf(
			"integration contract for unit %q composed no new candidate: every input was already fully contained in the verified base %s",
			contract.UnitID, short12(contract.BaseRevision))
	}

	head, err := ws.head()
	if err != nil {
		return integration.Result{}, committed, err
	}
	metadata, err := gitMetadataDigest(ws.Dir)
	if err != nil {
		return integration.Result{}, committed, err
	}
	ws.TrustedMetadata = metadata
	digest, err := contract.Inputs.Digest()
	if err != nil {
		return integration.Result{}, committed, err
	}
	return integration.Integrated(contract, integration.IntegratedCandidate{
		Revision: head.Commit, Tree: head.Tree, InputsDigest: digest,
	}), committed, nil
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
// head, and reports whether that actually advanced HEAD: a merge Git itself
// considers a no-op ("Already up to date", because commit was already fully
// contained) succeeds without creating one.
//
// A merge failure is classified from Git's OWN repository state, not from
// the mere presence of an error: it is a conflict only if Git left unmerged
// paths, which is what a conflict IS. A merge that failed for any other
// reason - a bad object, a locked index, an environment failure - is
// aborted and its real cause is returned as an error, never mislabeled a
// conflict.
//
// A merge Git itself already judged successful can still leave this
// function unable to OBSERVE whether it advanced HEAD, if the post-merge
// read itself fails. That is never reported as advanced=false: an unknown
// mutation is not a proven no-op, and collapsing the two would let a caller
// skip the destructive-cleanup path for a workspace that may genuinely be
// mutated. It is reported as a *WorkspaceIntegrityError instead - the same
// vocabulary every other "this package can no longer vouch for the
// workspace" case in this file uses - so a caller routes it to the
// runtime's existing integrity-failure recovery rather than this function
// guessing.
func mergeFetchedCommit(dir, commit string) (conflicted, advanced bool, paths []string, err error) {
	before, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return false, false, nil, err
	}
	if _, mergeErr := runGit(dir, "merge", "--no-ff", "--no-edit", commit); mergeErr != nil {
		// Read BEFORE aborting: aborting restores the index and the
		// unmerged markers along with it.
		paths, err = conflictedPaths(dir)
		if err != nil {
			return false, false, nil, err
		}
		if len(paths) == 0 {
			if abortErr := abortMergeInProgress(dir); abortErr != nil {
				return false, false, nil, fmt.Errorf("%v (cleanup also failed: %v)", mergeErr, abortErr)
			}
			return false, false, nil, mergeErr
		}
		return true, false, paths, nil
	}
	if afterMergeBeforeHeadRead != nil {
		afterMergeBeforeHeadRead(dir)
	}
	after, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return false, false, nil, &WorkspaceIntegrityError{
			Detail: fmt.Sprintf("could not observe HEAD after a merge Git already judged successful: %v", err),
		}
	}
	return false, before != after, nil, nil
}

// afterMergeBeforeHeadRead is a test seam: it runs after a merge attempt has
// already been judged successful by Git and before this function reads HEAD
// to decide whether it actually advanced - the exact window a test needs to
// inject an observation failure against a merge that genuinely already
// happened, the same pattern runtime/git.go's afterCommitGates/
// afterCommitUpdateRef already use for Commit's own post-mutation reads.
var afterMergeBeforeHeadRead func(dir string)

// statusEntry is one record of `git status --porcelain=v1 -z`: the two-
// letter index/worktree code and the path it names.
type statusEntry struct{ code, path string }

// workingTreeStatus reads the worktree/index status Git itself maintains -
// never content or ancestry, so this never reads a `diff`-class subcommand
// the #437 subject-store guard refuses outside the subject store. An
// in-progress merge conflict has no subject-store snapshot to read from in
// the first place; status answers from the index's own stage bits, which
// exist before anything is committed.
//
// It reads NUL-delimited (`-z`) records rather than one path per line: Git's
// line-oriented output C-quotes any path with a non-ASCII, control or quote
// byte (`"caf\303\251.txt"`, not the real UTF-8 bytes), and conflicted paths
// become the exact set VerifyRemediationScope treats as a remediation's
// permitted scope.
func workingTreeStatus(dir string) ([]statusEntry, error) {
	// --ignored: RestoreTrusted's recovery runs `git clean -fdx`, which
	// deletes ignored content along with ordinary untracked files. Without
	// --ignored, this read is blind to exactly the material that cleanup
	// would still remove, so a workspace holding it would pass this check
	// and then lose it anyway.
	out, err := gitOutput(dir, "status", "--porcelain=v1", "--ignored", "-z")
	if err != nil {
		return nil, err
	}
	records := strings.Split(out, "\x00")
	var entries []statusEntry
	for i := 0; i < len(records); i++ {
		record := records[i]
		if record == "" {
			continue
		}
		if len(record) < 4 {
			return nil, fmt.Errorf("unreadable git status record %q", record)
		}
		code := record[:2]
		entries = append(entries, statusEntry{code: code, path: record[3:]})
		if code[0] == 'R' || code[0] == 'C' {
			// A rename/copy record carries its OLD path as a second
			// NUL-terminated field; skip it rather than misread it as its
			// own record. Neither code is ever an unmerged one.
			i++
		}
	}
	return entries, nil
}

// isUnmergedStatus reports whether a `git status` code is one of Git's
// documented unmerged combinations.
func isUnmergedStatus(code string) bool {
	switch code {
	case "DD", "AU", "UD", "UA", "DU", "UU", "AA":
		return true
	}
	return false
}

// conflictedPaths reads which paths Git itself reports as unmerged, from
// `git status` rather than `git diff` - see workingTreeStatus.
func conflictedPaths(dir string) ([]string, error) {
	entries, err := workingTreeStatus(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		if isUnmergedStatus(e.code) {
			paths = append(paths, e.path)
		}
	}
	sort.Strings(paths)
	return paths, nil
}

// assertCleanAtVerifiedBase proves, directly against Git, that dir's HEAD is
// exactly revision and that its worktree and index are completely clean -
// no modified, staged, untracked or unmerged content. See IntegrateInputs
// for why a struct field recording where a workspace was cloned, or an
// integrity digest alone, cannot stand in for this.
func assertCleanAtVerifiedBase(dir, revision string) error {
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head = strings.TrimSpace(head); head != revision {
		return fmt.Errorf("integration workspace HEAD is %s, not the contract's verified base %s", short12(head), short12(revision))
	}
	entries, err := workingTreeStatus(dir)
	if err != nil {
		return err
	}
	if len(entries) > 0 {
		return fmt.Errorf("integration workspace is not clean at its base: %s (and %d more)", entries[0].path, len(entries)-1)
	}
	return nil
}

// abortMergeInProgress aborts a merge Git still considers unresolved. It is
// called only from within THIS SAME call, immediately after a merge attempt
// it just made failed without leaving an unmerged path - never speculatively
// at entry, where assertCleanAtVerifiedBase already refuses any leftover
// state outright rather than silently absorbing it.
func abortMergeInProgress(dir string) error {
	_, err := runGit(dir, "merge", "--abort")
	return err
}
