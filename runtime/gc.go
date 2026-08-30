package runtime

// gc.go is the runtime core of `autonomy gc`: conservative, plan-then-delete
// reclamation of heavyweight local material under the runtime state directory.
//
// The order is structural, not conventional:
//
//	discover -> prove eligibility -> optionally print the plan -> revalidate
//	-> delete one bounded target -> next target
//
// Nothing is deleted before its eligibility is proven, and nothing is proven
// once: prove() runs during planning AND again immediately before each
// deletion, because a run that was terminal and idle when the plan was printed
// can be holding a lease by the time the plan is executed. There is no
// metadata repair step afterwards, because nothing GC deletes is the authority
// for anything: every target is heavyweight material that a canonical row
// already explains BY REFERENCE. The journal, the runs and operations rows,
// and the provenance a waiting run needs are never touched.
//
// What is never eligible:
//
//   - anything belonging to a run that is not terminal (an active or waiting
//     run keeps its workspace, its checkouts, and its transcripts, so the run
//     stays explainable);
//   - anything belonging to a run holding a leased or running operation;
//   - anything younger than the retention window;
//   - <stateDir>/runtime.db and its sidecars, and every canonical row in it:
//     GC never opens a delete path that can reach them;
//   - a lock belonging to a runtime NewLockOwnerLiveness cannot prove dead;
//   - anything whose ownership cannot be proven - a directory under runs/ with
//     no durable run row, an artifact path no journal event references, or a
//     lock file whose name is not a runtime owner identity;
//   - anything that resolves outside the canonical state directory, or that
//     contains a symlink leaving it.
//
// What may become eligible after retention: the candidate workspace of a
// completed, cancelled, or failed run; its detached assurance checkouts; its
// raw local-only transcripts; and the ownership lock of a provably dead
// runtime. Sanitized artifacts are deliberately kept: they are the durable
// explainable derivative, and they are small.

import (
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultGCRetention is the global retention window used when the operator
// states none. It is deliberately long: reclaiming disk is never urgent
// enough to lose the material that explains a recent run.
//
// ponytail: there is no gc member on OperatorConfig, so the CLI must pass this
// (or a flag value) explicitly. Add one to OperatorConfig when an operator
// needs to change it without a flag.
const DefaultGCRetention = 7 * 24 * time.Hour

// GCKind names what a target is, so an operator print explains itself.
type GCKind string

const (
	GCCandidateWorkspace GCKind = "candidate_workspace"
	GCAssuranceCheckout  GCKind = "assurance_checkout"
	GCRawTranscript      GCKind = "raw_transcript"
	GCRuntimeLock        GCKind = "runtime_lock"
)

// GCTarget is one bounded deletion. Reason is empty on an eligible target and
// states the refusal on a retained or skipped one.
type GCTarget struct {
	Kind   GCKind `json:"kind"`
	RunID  string `json:"run_id,omitempty"`
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// GCPlan is what the planner decided. A dry run prints it; a real run executes
// it. There is exactly one planner, so the two can never disagree about what
// GC considers eligible.
type GCPlan struct {
	StateDir string     `json:"state_dir"`
	Now      time.Time  `json:"now"`
	Eligible []GCTarget `json:"eligible"`
	Retained []GCTarget `json:"retained"`
}

// GCResult is what a real run did. Skipped holds targets the plan judged
// eligible and revalidation then refused - the concurrency case, reported
// rather than silently dropped.
type GCResult struct {
	Plan    GCPlan     `json:"plan"`
	Deleted []GCTarget `json:"deleted"`
	Skipped []GCTarget `json:"skipped"`
}

// Collector is the garbage collector for one runtime state directory.
//
// Liveness is the OS-lock ownership evidence from owner.go. It defaults to
// NewLockOwnerLiveness(StateDir), which is the ONLY mechanism this package has
// for deciding whether a runtime is alive; GC does not add a second one.
type Collector struct {
	Store     *SQLiteOperationStore
	Clock     Clock
	Liveness  OwnerLiveness
	StateDir  string
	Retention time.Duration

	// afterPlan runs between planning and the first deletion. It is the test
	// seam that makes the plan/delete window observable, so a test can make a
	// run active in exactly that window and prove revalidation catches it. It
	// is nil in every production path.
	afterPlan func()
}

func (c Collector) clock() Clock {
	if c.Clock == nil {
		return RealClock{}
	}
	return c.Clock
}

func (c Collector) liveness() OwnerLiveness {
	if c.Liveness == nil {
		return NewLockOwnerLiveness(c.StateDir)
	}
	return c.Liveness
}

func (c Collector) retention() time.Duration {
	if c.Retention <= 0 {
		return DefaultGCRetention
	}
	return c.Retention
}

// stateRoot canonicalizes the state directory and returns its filesystem
// identity. Both are re-derived before every deletion: a root that no longer
// canonicalizes to the same path, or whose inode changed, is a state directory
// that was replaced under us, and GC refuses rather than deleting into it.
func (c Collector) stateRoot() (string, os.FileInfo, error) {
	if strings.TrimSpace(c.StateDir) == "" {
		return "", nil, fmt.Errorf("gc requires a runtime state directory")
	}
	root, err := filepath.EvalSymlinks(c.StateDir)
	if err != nil {
		return "", nil, fmt.Errorf("gc: runtime state directory is unavailable")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", nil, fmt.Errorf("gc: runtime state directory is unavailable")
	}
	return root, info, nil
}

// ---------------------------------------------------------------------------
// Planning
// ---------------------------------------------------------------------------

// Plan discovers every candidate target and proves each one. It performs no
// deletion and no durable write, so it is safe to run against a state
// directory another runtime is driving. This is the ONE planner: Collect calls
// it too, so `--dry-run` output is the plan that would be executed.
func (c Collector) Plan() (GCPlan, error) {
	if c.Store == nil {
		return GCPlan{}, fmt.Errorf("gc requires a durable operation store")
	}
	root, _, err := c.stateRoot()
	if err != nil {
		return GCPlan{}, err
	}
	now := c.clock().Now()
	plan := GCPlan{StateDir: root, Now: now, Eligible: []GCTarget{}, Retained: []GCTarget{}}
	targets, err := c.discover(root)
	if err != nil {
		return GCPlan{}, err
	}
	for _, target := range targets {
		if reason := c.prove(root, target, now); reason != "" {
			target.Reason = reason
			plan.Retained = append(plan.Retained, target)
			continue
		}
		plan.Eligible = append(plan.Eligible, target)
	}
	return plan, nil
}

// discover enumerates what GC could conceivably reclaim. It only ever names
// paths it constructs from the canonical root, plus artifact paths a journal
// event already references, so an undiscovered file is never a deletion
// candidate at all.
func (c Collector) discover(root string) ([]GCTarget, error) {
	ids, err := c.runIDs(root)
	if err != nil {
		return nil, err
	}
	var targets []GCTarget
	for _, id := range ids {
		runDir := filepath.Join(root, "runs", id)
		candidate := filepath.Join(runDir, "candidate")
		if isDir(candidate) {
			targets = append(targets, GCTarget{Kind: GCCandidateWorkspace, RunID: id, Path: candidate})
		}
		entries, err := os.ReadDir(filepath.Join(runDir, "assurance"))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				targets = append(targets, GCTarget{Kind: GCAssuranceCheckout, RunID: id, Path: filepath.Join(runDir, "assurance", entry.Name())})
			}
		}
		for _, path := range c.rawArtifacts(id) {
			targets = append(targets, GCTarget{Kind: GCRawTranscript, RunID: id, Path: path})
		}
	}
	locks, err := os.ReadDir(filepath.Join(root, "locks", "runtime"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range locks {
		if entry.IsDir() {
			continue
		}
		targets = append(targets, GCTarget{Kind: GCRuntimeLock, Path: filepath.Join(root, "locks", "runtime", entry.Name())})
	}
	sort.Slice(targets, func(i, j int) bool {
		if targets[i].Kind != targets[j].Kind {
			return targets[i].Kind < targets[j].Kind
		}
		return targets[i].Path < targets[j].Path
	})
	return targets, nil
}

// runIDs is every run GC has heard of: a directory under runs/ and a run named
// by a durable operation. A directory with no run row still appears here so it
// is REPORTED as retained-for-unprovable-ownership rather than ignored.
func (c Collector) runIDs(root string) ([]string, error) {
	seen := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(root, "runs"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			seen[entry.Name()] = true
		}
	}
	operations, err := c.Store.AllOperations()
	if err != nil {
		return nil, err
	}
	for _, op := range operations {
		seen[op.RunID] = true
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// rawArtifacts are the run's raw, local-only transcripts as the journal itself
// records them. Sanitized artifacts are excluded: they remain the durable
// explainable derivative. An artifact this run never recorded is not returned,
// which is what makes ownership provable.
func (c Collector) rawArtifacts(runID string) []string {
	snapshot, _, ok := c.replay(runID)
	if !ok {
		return nil
	}
	seen, paths := map[string]bool{}, []string(nil)
	for _, artifact := range snapshot.Artifacts {
		if artifact.LocalOnly && !artifact.Sanitized && !seen[artifact.Path] {
			seen[artifact.Path] = true
			paths = append(paths, artifact.Path)
		}
	}
	sort.Strings(paths)
	return paths
}

// replay reads the run's canonical row and journal and folds them with the one
// reducer. settled is the time of the run's last journalled event, which is
// when the retention clock starts.
//
// ponytail: prove() replays per target, so a run with many targets replays
// several times. That is deliberate - each replay is the revalidation - and a
// per-run cache is the upgrade if a state directory ever holds enough runs for
// it to matter.
func (c Collector) replay(runID string) (snapshot RunSnapshot, settled time.Time, ok bool) {
	run, found, err := c.Store.Run(runID)
	if err != nil || !found {
		return RunSnapshot{}, time.Time{}, false
	}
	events, err := c.Store.Events(runID)
	if err != nil {
		return RunSnapshot{}, time.Time{}, false
	}
	snapshot, err = Reduce(run, events)
	if err != nil {
		return RunSnapshot{}, time.Time{}, false
	}
	settled = run.UpdatedAt
	if n := len(events); n > 0 {
		settled = events[n-1].OccurredAt
	}
	return snapshot, settled, true
}

// ---------------------------------------------------------------------------
// Proof
// ---------------------------------------------------------------------------

// prove is the single eligibility gate. It returns "" when the target may be
// deleted, or the reason it must be kept. Plan calls it while discovering and
// Collect calls it again immediately before deleting, so there is exactly one
// definition of eligibility and it is re-derived from durable state at both
// points.
func (c Collector) prove(root string, target GCTarget, now time.Time) string {
	if reason := confined(root, target.Path); reason != "" {
		return reason
	}
	if target.Kind == GCRuntimeLock {
		return c.proveLock(target)
	}
	snapshot, settled, ok := c.replay(target.RunID)
	if !ok {
		return "ownership cannot be proven: no coherent durable run"
	}
	if !terminalDisposition(snapshot.Disposition) {
		return "run is " + string(snapshot.Disposition) + ", not terminal"
	}
	operations, err := c.Store.Operations(target.RunID)
	if err != nil {
		return "durable operations are unreadable"
	}
	for _, op := range operations {
		if op.State == Leased || op.State == Running {
			return "run holds a " + string(op.State) + " operation"
		}
	}
	if now.Sub(settled) < c.retention() {
		return "within the retention window"
	}
	if target.Kind == GCRawTranscript && !rawArtifactOf(snapshot, target.Path) {
		return "ownership cannot be proven: no journalled raw artifact at this path"
	}
	if target.Kind != GCRawTranscript {
		if reason := escapingSymlink(root, target.Path); reason != "" {
			return reason
		}
	}
	return ""
}

// proveLock decides one ownership lock. The name must read back as a runtime
// owner identity, and the ONLY liveness evidence is NewLockOwnerLiveness, which
// reports alive unless it can positively prove the holder is gone. GC therefore
// deletes a lock only for a runtime the OS itself has released.
func (c Collector) proveLock(target GCTarget) string {
	owner, err := url.PathUnescape(filepath.Base(target.Path))
	if err != nil {
		return "ownership cannot be proven: lock name is not an owner identity"
	}
	if _, _, _, ok := parseOwner(owner); !ok {
		return "ownership cannot be proven: lock name is not an owner identity"
	}
	if c.liveness().Alive(owner) {
		return "lock belongs to a live runtime"
	}
	return ""
}

func rawArtifactOf(snapshot RunSnapshot, path string) bool {
	for _, artifact := range snapshot.Artifacts {
		if artifact.Path == path && artifact.LocalOnly && !artifact.Sanitized {
			return true
		}
	}
	return false
}

// confined is the path gate, following the confinement pattern
// ToolBroker.resolve already established: the root is canonicalized once, and a
// target is accepted only when its own symlink resolution returns the identical
// path. That single equality refuses a path escaping the root, a symlinked leaf,
// and a symlinked intermediate directory - which is exactly what a swapped
// component would otherwise be.
func confined(root, path string) string {
	if path != filepath.Clean(path) || !strings.HasPrefix(path, root+string(os.PathSeparator)) {
		return "path is outside the runtime state directory"
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "path is not resolvable inside the runtime state directory"
	}
	if resolved != path {
		return "path does not resolve to its recorded location"
	}
	return ""
}

// escapingSymlink refuses a directory target that contains a link out of the
// state directory. WalkDir never follows a symlink, so the scan itself cannot
// leave the tree; refusing the whole target rather than unlinking around the
// link keeps the refusal explicit instead of relying on os.RemoveAll's internal
// no-follow behaviour.
func escapingSymlink(root, dir string) string {
	refusal := ""
	_ = filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			refusal = "target cannot be fully inspected"
			return filepath.SkipAll
		}
		if entry.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil || !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			refusal = "target contains a symlink leaving the runtime state directory"
			return filepath.SkipAll
		}
		return nil
	})
	return refusal
}

func isDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir()
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

// Collect plans, then deletes. Every target is revalidated with the same
// prove() the planner used, immediately before it is removed, and the state
// directory's canonical path and filesystem identity are rechecked at the same
// point. A target that stopped being eligible in that window is reported in
// Skipped and left alone.
//
// Nothing durable is written afterwards: GC only removes material that a
// canonical row already explains by reference, so there is no metadata to
// repair and therefore no delete-first-repair-later window to get wrong.
func (c Collector) Collect() (GCResult, error) {
	root, identity, err := c.stateRoot()
	if err != nil {
		return GCResult{}, err
	}
	plan, err := c.Plan()
	if err != nil {
		return GCResult{}, err
	}
	if c.afterPlan != nil {
		c.afterPlan()
	}
	result := GCResult{Plan: plan, Deleted: []GCTarget{}, Skipped: []GCTarget{}}
	for _, target := range plan.Eligible {
		current, currentIdentity, err := c.stateRoot()
		if err != nil || current != root || current != plan.StateDir || !os.SameFile(identity, currentIdentity) {
			return result, fmt.Errorf("gc refused: the runtime state directory was replaced")
		}
		if reason := c.prove(root, target, c.clock().Now()); reason != "" {
			target.Reason = reason
			result.Skipped = append(result.Skipped, target)
			continue
		}
		if err := os.RemoveAll(target.Path); err != nil {
			return result, err
		}
		result.Deleted = append(result.Deleted, target)
	}
	return result, nil
}
