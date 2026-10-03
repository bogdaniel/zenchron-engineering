package runtime

// #402: a runtime-authored candidate commit may never exist only in the
// workspace while durable state says the material is still uncommitted at the
// previous revision. Failures are injected through the kernel's own analyzer
// seam - a detector that refuses the Nth observation - so the commit is real
// and only what follows it fails.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
// record of it. The restarted runtime recognizes its own commit on top of the
// durable revision and completes it; it neither recommits nor calls it
// uncommitted.
func TestARestartAfterAnUnjournalledCommitReconcilesIt(t *testing.T) {
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
	head, _, count := runtimeCommitsPastBase(t, fixture, runID)
	if count != "1" || journalMentions(journalOf(t, fixture.runtime, runID), EventCandidateCommitted) {
		t.Fatal("crash scenario did not leave an unjournalled runtime commit")
	}
	// The lost process's lease on the attempt has to lapse before a successor
	// may take the operation over.
	fixture.clock.advance(2 * time.Minute)
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.reconcile(runID)
	committed := journalPayloads[CandidateCommittedPayload](t, journalOf(t, fixture.runtime, runID), EventCandidateCommitted)
	if len(committed) != 1 || committed[0].Commit != head {
		t.Fatalf("candidate.committed %+v, want the crashed attempt's commit %s", committed, head)
	}
	if _, _, count := runtimeCommitsPastBase(t, fixture, runID); count != "1" {
		t.Fatalf("%s runtime commits past the base after restart, want one", count)
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
