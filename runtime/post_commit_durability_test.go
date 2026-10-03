package runtime

// #402: a runtime-authored candidate commit may never exist only in the
// workspace while durable state says the material is still uncommitted at the
// previous revision. Failures are injected through the kernel's own analyzer
// seam - a detector that refuses the Nth observation - so the commit is real
// and only what follows it fails.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/analysis"
	"github.com/bogdaniel/zenchron-engineering/domain"
)

// observationFault wraps the baseline detector and fails, or panics, on the
// observed-stage calls fail selects. Observation call 1 is the coordinator's
// post-commit observation; call 2 is the canonical buildKernelAt rebuild.
type observationFault struct {
	mu    sync.Mutex
	calls int
	fail  func(call int) bool
	crash bool
}

func (d *observationFault) Detect(model domain.ProjectModel, in analysis.Input) ([]domain.EngineeringFact, error) {
	facts, err := analysis.CriticalBoundaryDetector{}.Detect(model, in)
	if err != nil || in.Stage != domain.StageObserved {
		return facts, err
	}
	d.mu.Lock()
	d.calls++
	n := d.calls
	d.mu.Unlock()
	if !d.fail(n) {
		return facts, nil
	}
	if d.crash {
		panic("process lost after the runtime commit")
	}
	return nil, fmt.Errorf("injected observation failure %d", n)
}

func withFault(f *phase8Fixture, fault *observationFault) {
	f.runtime.flow = KernelFlow{Analyzer: analysis.NewAnalyzerWithDetectors(fault)}
}

// commitAttempts decodes every failed candidate.commit attempt the journal holds.
func commitAttempts(t *testing.T, events []EngineeringEvent) []commitFailure {
	t.Helper()
	var out []commitFailure
	for _, e := range events {
		if e.Type != EventOperationAfter {
			continue
		}
		op, err := decodePayload[RunOperation](e.Payload)
		if err != nil {
			t.Fatal(err)
		}
		if op.Kind != OpCandidateCommit || op.State != OperationFailed {
			continue
		}
		var failure commitFailure
		if err := json.Unmarshal(op.Result, &failure); err != nil {
			t.Fatal(err)
		}
		out = append(out, failure)
	}
	return out
}

func runtimeCommitsPastBase(t *testing.T, f *phase8Fixture, runID string) (head, tree, count string) {
	t.Helper()
	dir := candidateDir(f.stateDir, runID)
	return mustGit(t, dir, "rev-parse", "HEAD"), mustGit(t, dir, "rev-parse", "HEAD^{tree}"),
		mustGit(t, dir, "rev-list", "--count", f.base+"..HEAD")
}

// INVARIANT (tests 1, 2, 3): whichever post-commit step fails, every attempt
// durably names the exact commit and tree, the producing operation and the
// failing stage; retries observe that same commit rather than committing again
// or failing on the moved head; the run holds committed_unobserved material
// at that commit; no assurance, authority or publication follows; and a
// reopened store reconstructs the same identity.
func TestAPostCommitFailureDurablyRecordsTheRuntimeCommit(t *testing.T) {
	for _, tc := range []struct {
		stage string
		fail  func(int) bool
	}{
		{commitStageObservation, func(int) bool { return true }},
		{commitStageReassessment, func(n int) bool { return n%2 == 0 }},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			fixture := newPhase8Fixture(t)
			withFault(fixture, &observationFault{fail: tc.fail})
			runID := fixture.start()
			outcome := fixture.reconcile(runID)
			if outcome.Disposition != Failed || outcome.Reason != OpCandidateCommit+attemptsExhaustedSuffix {
				t.Fatalf("outcome %s/%s", outcome.Disposition, outcome.Reason)
			}
			head, tree, count := runtimeCommitsPastBase(t, fixture, runID)
			if count != "1" {
				t.Fatalf("%s runtime commits past the base, want exactly one", count)
			}
			events := journalOf(t, fixture.runtime, runID)
			attempts := commitAttempts(t, events)
			if len(attempts) < 2 {
				t.Fatalf("%d failed commit attempts, want the retry exercised", len(attempts))
			}
			state := fixture.state(runID)
			executions := state.succeeded(OpExecutionInvoke)
			for i, a := range attempts {
				made := a.RuntimeCommit
				if a.Stage != tc.stage || made == nil || made.Commit != head || made.Tree != tree ||
					made.PathCount != 1 || made.Producing != executions[0].ID || a.Error == "" {
					t.Fatalf("attempt %d recorded %+v / %+v, want stage %s naming %s/%s", i+1, a, made, tc.stage, head, tree)
				}
			}
			if journalMentions(events, EventCandidateCommitted) || state.projection.CandidateRevision != "" {
				t.Fatal("a failed post-commit step reported a completed candidate")
			}
			if len(journalPayloads[AssuranceObservedPayload](t, events, EventAssuranceObserved)) != 0 {
				t.Fatal("assurance ran on an unobserved commit")
			}
			notPublished(t, fixture, runID)
			want := HeldMaterial{Kind: HeldCommittedUnobserved, Revision: head, Tree: tree, Operation: executions[0].ID,
				PathCount: 1, NextStep: OpCandidateCommit, BlockedBy: outcome.Reason, Disposition: HeldDisposition}
			if state.snapshot.HeldMaterial == nil {
				t.Fatal("no held material")
			}
			held := *state.snapshot.HeldMaterial
			held.ContentDigest = ""
			if held != want {
				t.Fatalf("held %+v, want %+v", held, want)
			}

			// Restart: a reopened store reconstructs the same commit identity.
			fixture.store.Close()
			store, err := OpenSQLiteOperationStore(fixture.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			deps := fixture.deps
			deps.Store = store
			fixture.runtime = fixture.newRuntime(deps)
			reopened := fixture.state(runID)
			commitOp, _ := reopened.operationByKey(OpCandidateCommit, executions[0].ID)
			if got := reopened.runtimeCommit(commitOp.ID); got == nil || got.Commit != head || got.Tree != tree {
				t.Fatalf("reopened store reconstructs %+v, want %s/%s", got, head, tree)
			}
			if !reflect.DeepEqual(reopened.snapshot.HeldMaterial, state.snapshot.HeldMaterial) {
				t.Fatalf("held material changed across restart: %+v", reopened.snapshot.HeldMaterial)
			}
		})
	}
}

// INVARIANT (tests 1 and 4): a retry after a post-commit failure completes the
// SAME commit - candidate.committed exactly once, naming the commit the failed
// attempt recorded - and never mints a second one.
func TestARetryObservesTheRecordedCommitInsteadOfRecommitting(t *testing.T) {
	fixture := newPhase8Fixture(t)
	withFault(fixture, &observationFault{fail: func(n int) bool { return n == 1 }})
	runID := fixture.start()
	fixture.reconcile(runID)
	events := journalOf(t, fixture.runtime, runID)
	attempts := commitAttempts(t, events)
	if len(attempts) != 1 || attempts[0].Stage != commitStageObservation || attempts[0].RuntimeCommit == nil {
		t.Fatalf("failed attempts %+v, want one observation failure naming the commit", attempts)
	}
	committed := journalPayloads[CandidateCommittedPayload](t, events, EventCandidateCommitted)
	if len(committed) != 1 || committed[0].Commit != attempts[0].RuntimeCommit.Commit || committed[0].Tree != attempts[0].RuntimeCommit.Tree {
		t.Fatalf("candidate.committed %+v, want exactly the recorded commit %+v", committed, attempts[0].RuntimeCommit)
	}
	if committed[0].PathsDigest != attempts[0].RuntimeCommit.PathsDigest {
		t.Fatalf("recovered path set %s differs from the recorded %s", committed[0].PathsDigest, attempts[0].RuntimeCommit.PathsDigest)
	}
	if _, _, count := runtimeCommitsPastBase(t, fixture, runID); count != "1" {
		t.Fatalf("%s runtime commits past the base, want one", count)
	}
}

// INVARIANT (test 5): process loss between the Git commit and any journal
// record of it. Nothing durable proves the head is the runtime's, so recovery
// FAILS CLOSED: the restarted runtime never adopts it and never commits again.
// Every attempt is refused naming the observed head as unproven (never as a
// runtime commit), the head is preserved, and the run holds unproven_head
// material for operator release (#344) - not uncommitted work at the base.
func TestARestartAfterAnUnjournalledCommitHoldsItAsUnproven(t *testing.T) {
	fixture := newPhase8Fixture(t)
	withFault(fixture, &observationFault{fail: func(n int) bool { return n == 1 }, crash: true})
	runID := fixture.start()
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the injected crash did not happen")
			}
		}()
		_, _ = fixture.runtime.Reconcile(t.Context(), runID)
	}()
	head, tree, count := runtimeCommitsPastBase(t, fixture, runID)
	if count != "1" || journalMentions(journalOf(t, fixture.runtime, runID), EventCandidateCommitted) {
		t.Fatal("crash scenario did not leave an unjournalled runtime commit")
	}
	// The lost process's lease on the attempt has to lapse before a successor
	// may take the operation over.
	fixture.clock.advance(2 * time.Minute)
	fixture.runtime = fixture.newRuntime(fixture.deps)
	outcome := fixture.reconcile(runID)
	events := journalOf(t, fixture.runtime, runID)
	if journalMentions(events, EventCandidateCommitted) || len(journalPayloads[AssuranceObservedPayload](t, events, EventAssuranceObserved)) != 0 {
		t.Fatalf("an unproven head was adopted (outcome %s/%s)", outcome.Disposition, outcome.Reason)
	}
	if outcome.Disposition != Failed || outcome.Reason != OpCandidateCommit+attemptsExhaustedSuffix {
		t.Fatalf("outcome %s/%s", outcome.Disposition, outcome.Reason)
	}
	attempts := commitAttempts(t, events)
	if len(attempts) < 2 {
		t.Fatalf("%d refused attempts after restart, want the retry exercised", len(attempts))
	}
	for i, a := range attempts {
		if a.Stage != commitStageWorkspace || a.RuntimeCommit != nil || a.UnprovenHead == nil ||
			a.UnprovenHead.Commit != head || a.UnprovenHead.Tree != tree {
			t.Fatalf("attempt %d recorded %+v / %+v, want a workspace refusal naming %s as unproven", i+1, a, a.UnprovenHead, head)
		}
	}
	if got, _, count := runtimeCommitsPastBase(t, fixture, runID); got != head || count != "1" {
		t.Fatalf("head %s with %s commits past the base, want %s preserved and never recommitted", got, count, head)
	}
	notPublished(t, fixture, runID)
	held := fixture.state(runID).snapshot.HeldMaterial
	if held == nil || held.Kind != HeldUnprovenHead || held.Revision != head || held.Tree != tree || held.ContentDigest != "" ||
		held.Operation != "" || held.PathCount != 0 || held.NextStep != HeldNextOperatorRelease {
		t.Fatalf("held %+v, want unproven_head at %s", held, head)
	}
}

// INVARIANT (test 6): no false recovery. A commit refused BEFORE Git wrote it
// records no runtime commit, and the material stays uncommitted at the base.
func TestARefusedCommitIsStillUncommitted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.provider.mutate = func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "id_rsa"), []byte("not a key\n"), 0600)
	}
	runID := fixture.start()
	fixture.reconcile(runID)
	attempts := commitAttempts(t, journalOf(t, fixture.runtime, runID))
	if len(attempts) == 0 {
		t.Fatal("the commit was not refused")
	}
	for _, a := range attempts {
		if a.RuntimeCommit != nil || a.Stage != commitStageCommit {
			t.Fatalf("a refused commit recorded %+v", a)
		}
	}
	if _, _, count := runtimeCommitsPastBase(t, fixture, runID); count != "0" {
		t.Fatalf("%s commits past the base", count)
	}
	if held := fixture.state(runID).snapshot.HeldMaterial; held == nil || held.Kind != HeldUncommitted || held.Revision != fixture.base {
		t.Fatalf("held %+v, want uncommitted material at the base", held)
	}
}

// forgeGit runs host Git as a provider that outlived its invocation would:
// no runtime policy, its own flags.
func forgeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	fixtureGit(t, dir, append([]string{"-c", "commit.gpgSign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
}

// INVARIANT (#402 review B1, the reviewer's PoC): a commit the PROVIDER made,
// dressed as the runtime's - runtime author, this run's exact message, parent
// the recorded revision - is never adopted. Every candidate.commit attempt is
// refused as a workspace integrity failure, none records it as a runtime
// commit, nothing is committed, assured or published, and the forged commit is
// preserved rather than reset.
func TestAProviderForgedRuntimeCommitIsNeverAdopted(t *testing.T) {
	fixture := newPhase8Fixture(t)
	var runID, message, dir string
	fixture.provider.mutate = func(candidate string) error {
		dir = candidate
		mustWrite(t, filepath.Join(dir, "candidate.go"), "package candidate\n")
		return nil
	}
	// The provider OUTLIVES its invocation: the forgery lands after execution
	// has been observed, at the runtime's next external call.
	fixture.inject(func(GitHubCall) error {
		if dir != "" && message != "" {
			mustWrite(t, filepath.Join(dir, "id_rsa"), pemPrivateKeyBlock()+"\n")
			forgeGit(t, dir, "add", "-A")
			forgeGit(t, dir, "commit", "-q", "-m", message)
			message = ""
		}
		return nil
	})
	runID = fixture.start()
	message = candidateCommitMessage(fixture.state(runID))
	outcome := fixture.reconcile(runID)
	forged := mustGit(t, dir, "rev-parse", "HEAD")
	if mustGit(t, dir, "log", "-1", "--format=%ae", forged) != "runtime@zenchron.invalid" || forged == fixture.base {
		t.Fatal("the forgery did not happen as a runtime-identity commit")
	}
	events := journalOf(t, fixture.runtime, runID)
	if journalMentions(events, EventCandidateCommitted) || len(journalPayloads[AssuranceObservedPayload](t, events, EventAssuranceObserved)) != 0 {
		t.Fatalf("a forged commit was adopted (outcome %s/%s)", outcome.Disposition, outcome.Reason)
	}
	attempts := commitAttempts(t, events)
	if len(attempts) < 2 {
		t.Fatalf("%d failed commit attempts (outcome %s/%s), want the retry exercised", len(attempts), outcome.Disposition, outcome.Reason)
	}
	for i, a := range attempts {
		if a.Stage != commitStageWorkspace || a.RuntimeCommit != nil || a.UnprovenHead == nil || a.UnprovenHead.Commit != forged {
			t.Fatalf("attempt %d recorded %+v / %+v, want a workspace refusal naming %s as unproven", i+1, a, a.UnprovenHead, forged)
		}
	}
	notPublished(t, fixture, runID)
	if got := mustGit(t, dir, "rev-parse", "HEAD"); got != forged {
		t.Fatalf("the forged commit was not preserved: head %s, was %s", got, forged)
	}
	if held := fixture.state(runID).snapshot.HeldMaterial; held == nil || held.Kind != HeldUnprovenHead || held.Revision != forged ||
		held.Operation != "" || held.NextStep != HeldNextOperatorRelease {
		t.Fatalf("held %+v, want unproven_head at %s", held, forged)
	}
}

// forgeOutput is forgeGit that returns Git's output.
func forgeOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// recoveryCase is a real unjournalled runtime commit (a crash right after Git
// wrote it), ready to be tampered with before its retry asks to recover it.
type recoveryCase struct {
	fixture *phase8Fixture
	dir     string
	state   *runState
	valid   *runtimeCommit // what the crashed attempt would have recorded
}

func newRecoveryCase(t *testing.T) recoveryCase {
	t.Helper()
	fixture := newPhase8Fixture(t)
	withFault(fixture, &observationFault{fail: func(n int) bool { return n == 1 }, crash: true})
	runID := fixture.start()
	func() {
		defer func() { _ = recover() }()
		_, _ = fixture.runtime.Reconcile(t.Context(), runID)
	}()
	dir := candidateDir(fixture.stateDir, runID)
	head, tree := mustGit(t, dir, "rev-parse", "HEAD"), mustGit(t, dir, "rev-parse", "HEAD^{tree}")
	paths, err := diffPaths(dir, fixture.base, head, "--no-renames")
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := gitMetadataDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return recoveryCase{fixture, dir, fixture.state(runID), &runtimeCommit{
		commitRecord: commitRecord{Commit: head, Tree: tree, PathCount: len(paths), MetadataDigest: metadata},
		PathsDigest:  pathsDigest(paths),
	}}
}

func (c *recoveryCase) recover(prior *runtimeCommit) (*CommitResult, error) {
	_, result, err := c.fixture.runtime.recoverRuntimeCommit(c.state, prior, &WorkspaceIntegrityError{Detail: "candidate head moved"})
	return result, err
}

// amend replaces the runtime commit with one of the same parent, identity and
// message, carrying whatever change left in the worktree.
func (c *recoveryCase) amend(t *testing.T, change func()) {
	t.Helper()
	change()
	forgeGit(t, c.dir, "add", "-A")
	forgeGit(t, c.dir, "commit", "-q", "--amend", "--no-edit")
}

// INVARIANT (#402 review): recovery FAILS CLOSED. With no recorded runtime
// commit nothing is adopted - not the runtime's own unjournalled commit, and
// not a forgery whose bytes skip-worktree hides from the worktree (N1) or whose
// parent a graft rewrites (N2). With a recorded commit, only that exact commit
// is adopted: a different commit, tampered metadata, a dirty worktree or a
// record that does not match is refused. Every refusal keeps the original
// integrity error and leaves the head where it was.
func TestRecoveryAdoptsOnlyTheRecordedRuntimeCommit(t *testing.T) {
	t.Run("control: recorded commit", func(t *testing.T) {
		c := newRecoveryCase(t)
		if result, err := c.recover(c.valid); err != nil || result.Commit != c.valid.Commit {
			t.Fatalf("the recorded commit was not recovered: %v", err)
		}
	})
	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, c *recoveryCase) *runtimeCommit // returns the recorded commit, if any
	}{
		{"no recorded commit: the runtime's own commit", func(*testing.T, *recoveryCase) *runtimeCommit { return nil }},
		{"no recorded commit: skip-worktree hides a committed secret", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			c.amend(t, func() { mustWrite(t, filepath.Join(c.dir, "id_rsa"), pemPrivateKeyBlock()+"\n") })
			forgeGit(t, c.dir, "update-index", "--skip-worktree", "id_rsa")
			mustWrite(t, filepath.Join(c.dir, "id_rsa"), "not a key\n")
			if status := forgeOutput(t, c.dir, "status", "--porcelain"); status != "" {
				t.Fatalf("the variant is not clean to status: %q", status)
			}
			return nil
		}},
		{"no recorded commit: a graft fakes the parent", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			mustWrite(t, filepath.Join(c.dir, "secret.txt"), awsSecretAssignment()+"\n")
			forgeGit(t, c.dir, "add", "secret.txt")
			forgeGit(t, c.dir, "commit", "-q", "-m", "history carrying a secret")
			forgeGit(t, c.dir, "rm", "-q", "secret.txt")
			forgeGit(t, c.dir, "commit", "-q", "-m", candidateCommitMessage(c.state))
			head := forgeOutput(t, c.dir, "rev-parse", "HEAD")
			mustWrite(t, filepath.Join(c.dir, ".git", "info", "grafts"), head+" "+c.fixture.base+"\n")
			if parent := forgeOutput(t, c.dir, "log", "-1", "--format=%P", "HEAD"); parent != c.fixture.base {
				t.Fatalf("the graft did not fake the parent: %s", parent)
			}
			return nil
		}},
		{"a different commit than recorded", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			c.amend(t, func() { mustWrite(t, filepath.Join(c.dir, "id_rsa"), pemPrivateKeyBlock()+"\n") })
			return c.valid
		}},
		{"tampered config", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			forgeGit(t, c.dir, "config", "zenchron.tampered", "yes")
			return c.valid
		}},
		{"a dirty tree", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			mustWrite(t, filepath.Join(c.dir, "stray.go"), "package stray\n")
			return c.valid
		}},
		{"a mismatched recorded commit", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			prior := *c.valid
			prior.Commit = c.fixture.base
			return &prior
		}},
		{"mismatched recorded paths", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			prior := *c.valid
			prior.PathsDigest = pathsDigest([]string{"elsewhere.go"})
			return &prior
		}},
		{"mismatched recorded metadata", func(t *testing.T, c *recoveryCase) *runtimeCommit {
			prior := *c.valid
			prior.MetadataDigest = textDigest("other metadata")
			return &prior
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newRecoveryCase(t)
			prior := tc.tamper(t, &c)
			head := mustGit(t, c.dir, "rev-parse", "HEAD")
			result, err := c.recover(prior)
			if err == nil {
				t.Fatalf("adopted %+v", result)
			}
			var integrity *WorkspaceIntegrityError
			if !errors.As(err, &integrity) || integrity.Detail != "candidate head moved" {
				t.Fatalf("refusal %v does not carry the original integrity error", err)
			}
			if got := mustGit(t, c.dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refusal moved the head from %s to %s", head, got)
			}
			t.Logf("refused: %v", err)
		})
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
