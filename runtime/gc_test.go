package runtime

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// gcBase is when the fixture's runs happened; the fixture clock stands 90 days
// later, so a 7-day retention window is comfortably exceeded and every "is it
// old enough" assertion is about the rule rather than about wall time.
var gcBase = time.Unix(1_700_000_000, 0).UTC()

const gcRetention = 7 * 24 * time.Hour

// gcFixture is one runtime state directory with a REAL durable store, laid out
// exactly as controller.go, git.go, and sandbox.go lay it out.
type gcFixture struct {
	t     *testing.T
	dir   string // the path an operator configures
	root  string // its canonical form, which is what GC works in
	store *SQLiteOperationStore
	clock *fakeClock
}

func newGCFixture(t *testing.T) *gcFixture {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &gcFixture{t: t, dir: dir, root: root, store: store, clock: &fakeClock{now: gcBase.Add(90 * 24 * time.Hour)}}
}

func (f *gcFixture) collector() Collector {
	return Collector{Store: f.store, Clock: f.clock, Liveness: NewLockOwnerLiveness(f.dir),
		StateDir: f.dir, Retention: gcRetention}
}

func (f *gcFixture) append(e EngineeringEvent) {
	f.t.Helper()
	if _, err := f.store.AppendEvent(e); err != nil {
		f.t.Fatal(err)
	}
}

func (f *gcFixture) createRun(id string) {
	f.t.Helper()
	if err := f.store.PutRun(EngineeringRun{SchemaVersion: SchemaVersion, ID: id, Repository: "o/r",
		Goal: "github-issue:o/r#1", Phase: Contract, Disposition: Active,
		CreatedAt: gcBase, UpdatedAt: gcBase}); err != nil {
		f.t.Fatal(err)
	}
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id + "-created", RunID: id,
		Type: EventRunCreated, OccurredAt: gcBase})
}

// gcMaterial is the heavyweight material one run leaves on disk.
type gcMaterial struct{ candidate, assurance, raw, sanitized string }

func (f *gcFixture) material(id string) gcMaterial {
	f.t.Helper()
	m := gcMaterial{
		candidate: filepath.Join(f.root, "runs", id, "candidate"),
		assurance: filepath.Join(f.root, "runs", id, "assurance", "commit-1"),
		raw:       filepath.Join(f.root, "artifacts", "assurance-"+id+".raw.log"),
		sanitized: filepath.Join(f.root, "artifacts", "assurance-"+id+".sanitized-candidate.log"),
	}
	for _, dir := range []string{m.candidate, m.assurance} {
		if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
			f.t.Fatal(err)
		}
		writeGCFile(f.t, filepath.Join(dir, "main.go"), "package main")
	}
	if err := os.MkdirAll(filepath.Dir(m.raw), 0o700); err != nil {
		f.t.Fatal(err)
	}
	writeGCFile(f.t, m.raw, "raw transcript with a secret")
	writeGCFile(f.t, m.sanitized, "redacted transcript")
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id + "-artifacts", RunID: id,
		Type: EventCandidateChanged, OccurredAt: gcBase, Artifacts: []Artifact{
			{Path: m.raw, SHA256: "raw", MediaType: "text/plain", LocalOnly: true},
			{Path: m.sanitized, SHA256: "sanitized", MediaType: "text/plain", Sanitized: true},
		}})
	return m
}

// authority journals the #7 decision that gates publication. It is the current
// authority evidence for the run, and GC must never touch it.
func (f *gcFixture) authority(id string) {
	f.t.Helper()
	payload, err := marshalPayloadJSON(AuthorityEvaluatedPayload{
		Decision: Ref{ID: "decision-1", Revision: "rev-1"},
		Action:   domain.Action{Type: "publish_pull_request", Target: "o/r"},
		Status:   domain.AuthorityAwaitingAuthority})
	if err != nil {
		f.t.Fatal(err)
	}
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id + "-authority", RunID: id,
		Type: EventAuthorityEvaluated, OccurredAt: gcBase, Payload: payload})
}

func (f *gcFixture) settle(id, eventType string) {
	f.t.Helper()
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id + "-" + eventType, RunID: id,
		Type: eventType, OccurredAt: gcBase})
}

func writeGCFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func gcExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Lstat(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func mustExist(t *testing.T, what, path string) {
	t.Helper()
	if !gcExists(t, path) {
		t.Fatalf("%s was removed: %s", what, path)
	}
}

func mustNotExist(t *testing.T, what, path string) {
	t.Helper()
	if gcExists(t, path) {
		t.Fatalf("%s was not reclaimed: %s", what, path)
	}
}

func retainedReason(plan GCPlan, path string) (string, bool) {
	for _, target := range plan.Retained {
		if target.Path == path {
			return target.Reason, true
		}
	}
	return "", false
}

func planned(targets []GCTarget, path string) bool {
	for _, target := range targets {
		if target.Path == path {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Preservation
// ---------------------------------------------------------------------------

// An active run is being driven right now. Everything it owns stays, however
// old the material looks.
func TestGCPreservesActiveRunMaterial(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-active")
	m := f.material("run-active")

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{m.candidate, m.assurance, m.raw} {
		if planned(plan.Eligible, path) {
			t.Fatalf("an active run's material was planned for deletion: %s", path)
		}
		if reason, ok := retainedReason(plan, path); !ok || !strings.Contains(reason, "not terminal") {
			t.Fatalf("active run material %s retained for %q, want the non-terminal refusal", path, reason)
		}
	}
	result, err := f.collector().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("gc deleted %v while a run was active", result.Deleted)
	}
	mustExist(t, "the active run's workspace", m.candidate)
	mustExist(t, "the active run's assurance checkout", m.assurance)
	mustExist(t, "the active run's transcript", m.raw)
	mustExist(t, "the run state directory", filepath.Join(f.root, "runs", "run-active"))
}

// A waiting run is parked, not finished: the provenance that explains WHY it is
// waiting - its journal, its authority decision, and the material those refer
// to - must survive a collection.
func TestGCPreservesWaitingRunProvenance(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-waiting")
	m := f.material("run-waiting")
	f.authority("run-waiting")
	f.settle("run-waiting", EventRunWaiting)

	before, err := f.store.Events("run-waiting")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	after, err := f.store.Events("run-waiting")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("gc altered the journal of a waiting run")
	}
	snapshot, err := f.store.Replay("run-waiting")
	if err != nil || snapshot.Disposition != Waiting {
		t.Fatalf("the waiting run no longer replays: %v %v", snapshot.Disposition, err)
	}
	mustExist(t, "the waiting run's workspace", m.candidate)
	mustExist(t, "the waiting run's transcript", m.raw)
	mustExist(t, "the waiting run's sanitized evidence", m.sanitized)
}

// The journal, the canonical rows, and current authority evidence are never
// reclaimed, even while an unrelated eligible run IS being collected.
func TestGCNeverRemovesJournalCanonicalRowsOrAuthorityEvidence(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-waiting")
	waiting := f.material("run-waiting")
	f.authority("run-waiting")
	f.settle("run-waiting", EventRunWaiting)

	f.createRun("run-done")
	done := f.material("run-done")
	f.settle("run-done", EventRunCompleted)

	database := filepath.Join(f.root, "runtime.db")
	waitingEvents, err := f.store.Events("run-waiting")
	if err != nil {
		t.Fatal(err)
	}
	doneEvents, err := f.store.Events("run-done")
	if err != nil {
		t.Fatal(err)
	}

	result, err := f.collector().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Deleted) == 0 {
		t.Fatal("the collection under test deleted nothing, so it proves nothing")
	}
	mustExist(t, "the runtime database", database)
	for _, id := range []string{"run-waiting", "run-done"} {
		if _, ok, err := f.store.Run(id); err != nil || !ok {
			t.Fatalf("the canonical run row for %s is gone: %v", id, err)
		}
	}
	if got, err := f.store.Events("run-waiting"); err != nil || !reflect.DeepEqual(got, waitingEvents) {
		t.Fatalf("the waiting run's journal changed: %v", err)
	}
	if got, err := f.store.Events("run-done"); err != nil || !reflect.DeepEqual(got, doneEvents) {
		t.Fatalf("the collected run's journal changed: %v", err)
	}
	mustExist(t, "current authority evidence", waiting.sanitized)
	mustExist(t, "current authority evidence", waiting.raw)
	// The collected run's sanitized derivative stays: it is the durable
	// explainable artifact, and only the raw local transcript is reclaimed.
	mustExist(t, "the sanitized derivative", done.sanitized)
	mustNotExist(t, "the raw transcript of a collected run", done.raw)
}

// ---------------------------------------------------------------------------
// Reclamation
// ---------------------------------------------------------------------------

func TestGCRemovesEligibleTerminalRunMaterial(t *testing.T) {
	for _, settled := range []string{EventRunCompleted, EventRunFailed, EventRunCancelled} {
		t.Run(settled, func(t *testing.T) {
			f := newGCFixture(t)
			f.createRun("run-old")
			m := f.material("run-old")
			f.settle("run-old", settled)

			result, err := f.collector().Collect()
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Skipped) != 0 {
				t.Fatalf("revalidation refused an uncontended target: %v", result.Skipped)
			}
			mustNotExist(t, "the candidate workspace", m.candidate)
			mustNotExist(t, "the assurance checkout", m.assurance)
			mustNotExist(t, "the raw transcript", m.raw)
			mustExist(t, "the sanitized derivative", m.sanitized)
		})
	}
}

// Retention is a real window, not a formality.
func TestGCKeepsTerminalRunMaterialInsideRetention(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-fresh")
	m := f.material("run-fresh")
	f.settle("run-fresh", EventRunCompleted)
	f.clock.now = gcBase.Add(gcRetention - time.Second)

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if reason, ok := retainedReason(plan, m.candidate); !ok || !strings.Contains(reason, "retention") {
		t.Fatalf("a freshly settled run was retained for %q, want the retention refusal", reason)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "material inside the retention window", m.candidate)
}

// ---------------------------------------------------------------------------
// One planner
// ---------------------------------------------------------------------------

// The dry run and the real run must be the same decision. Collect executes the
// plan Plan produced; if they were computed by different logic, this fails.
func TestGCDryRunPlanEqualsRealRunPlan(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-old")
	f.material("run-old")
	f.settle("run-old", EventRunCompleted)
	f.createRun("run-live")
	f.material("run-live")
	if err := os.MkdirAll(filepath.Join(f.root, "runs", "run-orphan", "candidate"), 0o700); err != nil {
		t.Fatal(err)
	}

	dryRun, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Eligible) == 0 || len(dryRun.Retained) == 0 {
		t.Fatalf("the fixture must plan both outcomes: %+v", dryRun)
	}
	result, err := f.collector().Collect()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dryRun, result.Plan) {
		t.Fatalf("the dry-run plan and the executed plan disagree:\ndry: %+v\nreal: %+v", dryRun, result.Plan)
	}
	if !reflect.DeepEqual(dryRun.Eligible, result.Deleted) {
		t.Fatalf("the executed deletions are not the planned eligibility:\nplanned: %+v\ndeleted: %+v", dryRun.Eligible, result.Deleted)
	}
}

// ---------------------------------------------------------------------------
// Confinement
// ---------------------------------------------------------------------------

// A symlink planted inside a GC target may not become a path out of the state
// directory. GC refuses the whole target and touches nothing outside.
func TestGCRefusesSymlinkEscape(t *testing.T) {
	f := newGCFixture(t)
	outside := t.TempDir()
	writeGCFile(t, filepath.Join(outside, "precious.txt"), "must survive")

	f.createRun("run-old")
	m := f.material("run-old")
	f.settle("run-old", EventRunCompleted)
	if err := os.Symlink(outside, filepath.Join(m.candidate, "escape")); err != nil {
		t.Fatal(err)
	}

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if planned(plan.Eligible, m.candidate) {
		t.Fatal("a workspace containing an escaping symlink was planned for deletion")
	}
	if reason, ok := retainedReason(plan, m.candidate); !ok || !strings.Contains(reason, "symlink") {
		t.Fatalf("the escaping workspace was retained for %q, want the symlink refusal", reason)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "the refused workspace", m.candidate)
	mustExist(t, "a file outside the state directory", filepath.Join(outside, "precious.txt"))
	mustExist(t, "the directory outside the state directory", outside)
}

// A journalled artifact path that points outside the state directory is not a
// GC target, whatever the journal says.
func TestGCRefusesPathEscape(t *testing.T) {
	f := newGCFixture(t)
	outside := t.TempDir()
	escaped := filepath.Join(outside, "outside.raw.log")
	writeGCFile(t, escaped, "must survive")
	// Deliberately unnormalized: an uncleaned traversal must be refused on its
	// spelling, before anything resolves it.
	traversal := filepath.Join(f.root, "artifacts") + "/../../traversal.raw.log"
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(traversal)), 0o700); err != nil {
		t.Fatal(err)
	}
	writeGCFile(t, filepath.Clean(traversal), "must survive")

	f.createRun("run-old")
	f.material("run-old")
	f.append(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "run-old-escaped", RunID: "run-old",
		Type: EventCandidateChanged, OccurredAt: gcBase, Artifacts: []Artifact{
			{Path: escaped, SHA256: "x", MediaType: "text/plain", LocalOnly: true},
			{Path: traversal, SHA256: "y", MediaType: "text/plain", LocalOnly: true},
		}})
	f.settle("run-old", EventRunCompleted)

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{escaped, traversal} {
		if planned(plan.Eligible, path) {
			t.Fatalf("a path outside the state directory was planned for deletion: %s", path)
		}
		if reason, ok := retainedReason(plan, path); !ok || !strings.Contains(reason, "outside") {
			t.Fatalf("%s was retained for %q, want the confinement refusal", path, reason)
		}
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "an artifact outside the state directory", escaped)
	mustExist(t, "a traversal target above the state directory", filepath.Clean(traversal))
}

// A state directory that was swapped for another one between planning and
// deleting is refused rather than deleted into.
func TestGCRefusesWhenTheStateDirectoryIsReplaced(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-old")
	m := f.material("run-old")
	f.settle("run-old", EventRunCompleted)

	parent := filepath.Dir(f.dir)
	link, other := filepath.Join(parent, "link"), filepath.Join(parent, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(f.dir, link); err != nil {
		t.Fatal(err)
	}
	collector := f.collector()
	collector.StateDir = link
	collector.afterPlan = func() {
		if err := os.Remove(link); err != nil {
			t.Error(err)
		}
		if err := os.Symlink(other, link); err != nil {
			t.Error(err)
		}
	}
	if _, err := collector.Collect(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("gc collected into a replaced state directory: %v", err)
	}
	mustExist(t, "the workspace of the original state directory", m.candidate)
}

// ---------------------------------------------------------------------------
// Ownership and liveness
// ---------------------------------------------------------------------------

// Material whose ownership cannot be proven is never eligible, whatever it
// looks like on disk.
func TestGCRefusesUnprovableOwnership(t *testing.T) {
	f := newGCFixture(t)
	orphan := filepath.Join(f.root, "runs", "run-orphan", "candidate")
	if err := os.MkdirAll(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	locks := filepath.Join(f.root, "locks", "runtime")
	if err := os.MkdirAll(locks, 0o700); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(locks, "not-an-owner-identity")
	writeGCFile(t, stray, "")

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{orphan, stray} {
		if planned(plan.Eligible, path) {
			t.Fatalf("unowned material was planned for deletion: %s", path)
		}
		if reason, ok := retainedReason(plan, path); !ok || !strings.Contains(reason, "ownership cannot be proven") {
			t.Fatalf("%s was retained for %q, want the unprovable-ownership refusal", path, reason)
		}
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "a run directory with no durable run row", orphan)
	mustExist(t, "a lock file that is not an owner identity", stray)
}

// The lock of a runtime that is genuinely alive is preserved, and only the OS
// lock evidence decides that. The holder is a real child process holding a real
// kernel lock, so killing it is the only thing that makes its lock collectable.
func TestGCPreservesLockOfLiveRuntimeAndCollectsADeadOne(t *testing.T) {
	f := newGCFixture(t)
	live := startLockHolder(t, f.dir, "")
	dead := startLockHolder(t, f.dir, "")
	livePath := filepath.Join(f.root, "locks", "runtime", lockFileName(live.owner))
	deadPath := filepath.Join(f.root, "locks", "runtime", lockFileName(dead.owner))
	mustExist(t, "the live holder's lock", livePath)
	mustExist(t, "the second holder's lock", deadPath)

	dead.kill(t)

	plan, err := f.collector().Plan()
	if err != nil {
		t.Fatal(err)
	}
	if planned(plan.Eligible, livePath) {
		t.Fatal("the lock of a LIVE runtime was planned for deletion")
	}
	if reason, ok := retainedReason(plan, livePath); !ok || !strings.Contains(reason, "live runtime") {
		t.Fatalf("the live lock was retained for %q, want the liveness refusal", reason)
	}
	if !planned(plan.Eligible, deadPath) {
		t.Fatalf("the lock of a provably dead runtime was not eligible: %+v", plan)
	}
	if _, err := f.collector().Collect(); err != nil {
		t.Fatal(err)
	}
	mustExist(t, "the lock of a live runtime", livePath)
	mustNotExist(t, "the lock of a dead runtime", deadPath)
	if !NewLockOwnerLiveness(f.dir).Alive(live.owner) {
		t.Fatal("the live holder stopped being alive during the collection")
	}
}

// lockFileName mirrors owner.go's escaping so the test names the same file the
// runtime does.
func lockFileName(owner string) string { return filepath.Base(ownerLockPath("x", owner)) }

// ---------------------------------------------------------------------------
// Revalidation
// ---------------------------------------------------------------------------

// Eligibility proven at plan time is not eligibility at delete time. A run that
// becomes active - here by acquiring a lease, which is exactly the concurrent
// acquisition a second runtime performs - is refused between the plan and the
// deletion it was already in.
func TestGCRevalidationRefusesARunThatBecomesActive(t *testing.T) {
	f := newGCFixture(t)
	f.createRun("run-old")
	m := f.material("run-old")
	f.settle("run-old", EventRunCompleted)

	collector := f.collector()
	collector.afterPlan = func() {
		lease := &Lease{Owner: "host/1/token", HeartbeatAt: f.clock.now, ExpiresAt: f.clock.now.Add(time.Minute)}
		if _, ok, err := f.store.PutOperation(RunOperation{SchemaVersion: SchemaVersion, ID: "op-late",
			RunID: "run-old", Kind: "provider", IdempotencyKey: "late", State: Leased, MaxAttempts: 2,
			CreatedAt: f.clock.now, Lease: lease}, 0); err != nil || !ok {
			t.Errorf("the concurrent lease was not recorded: %v %v", ok, err)
		}
	}
	result, err := collector.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Plan.Eligible) == 0 {
		t.Fatal("the plan found nothing eligible, so revalidation is not under test")
	}
	if len(result.Deleted) != 0 {
		t.Fatalf("gc deleted material of a run that became active: %v", result.Deleted)
	}
	if len(result.Skipped) != len(result.Plan.Eligible) {
		t.Fatalf("revalidation skipped %d of %d planned targets", len(result.Skipped), len(result.Plan.Eligible))
	}
	for _, target := range result.Skipped {
		if !strings.Contains(target.Reason, "leased") {
			t.Fatalf("%s was skipped for %q, want the leased-operation refusal", target.Path, target.Reason)
		}
	}
	mustExist(t, "the workspace of a run that became active", m.candidate)
	mustExist(t, "the assurance checkout of a run that became active", m.assurance)
	mustExist(t, "the transcript of a run that became active", m.raw)
}
