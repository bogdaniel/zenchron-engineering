package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// budgetProvider is an execution provider that can be told, per invocation,
// whether to change the workspace and how to stop. It exists because the
// distinction under test is exactly that pair.
type budgetProvider struct {
	steps    []budgetStep
	requests []ExecutionRequest
}

type budgetStep struct {
	mutate string // a filename to write, or "" for a zero-delta invocation
	stop   ProviderStop
	detail string
}

func (p *budgetProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *budgetProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	index := len(p.requests)
	p.requests = append(p.requests, request)
	step := budgetStep{}
	if index < len(p.steps) {
		step = p.steps[index]
	}
	// Every step defaults to the runtime's own iteration ceiling, because that
	// is the stop this whole matrix is about.
	if step.stop == "" {
		step.stop, step.detail = StopIterationBudget, "reasoning iterations exceeded 16"
	}
	if step.mutate != "" {
		if err := os.WriteFile(filepath.Join(request.CandidateDir, step.mutate), []byte("package candidate\n"), 0600); err != nil {
			return ExecutionResult{}, err
		}
	}
	if step.stop == StopCompleted {
		return ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}, nil
	}
	return ExecutionResult{ProviderID: "test-provider", Outcome: OperationFailed},
		&ProviderStopError{Reason: step.stop, Detail: step.detail}
}

func budgetFixture(t *testing.T, steps ...budgetStep) (*phase8Fixture, *budgetProvider, string) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	provider := &budgetProvider{steps: steps}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	// Reconcile returns at each stop condition; the watch loop re-drives. A
	// bounded number of passes stands in for that, so a retry the runtime
	// intends is actually taken and a loop the runtime does not intend is
	// still caught by the ceiling.
	for pass := 0; pass < 12; pass++ {
		fixture.reconcile(runID)
		if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
			break
		}
	}
	return fixture, provider, runID
}

// TestInitialZeroDeltaBudgetStopRetriesWithinTheExecutionBudget is case A.
//
// An initial invocation that reasoned through the runtime's own iteration
// ceiling without writing a patch used to be classified unknown and stop the
// run at attempt 1 of a configured 3 - so the execution budget was unreachable
// for the one failure it most obviously exists for. The runtime set that
// ceiling and observed it being reached; there is nothing undiagnosed about it.
func TestInitialZeroDeltaBudgetStopRetriesWithinTheExecutionBudget(t *testing.T) {
	// Two empty attempts, then real work.
	fixture, provider, runID := budgetFixture(t,
		budgetStep{}, budgetStep{mutate: "candidate.go", stop: StopCompleted})

	if len(provider.requests) < 2 {
		t.Fatalf("the run stopped after %d invocation(s); the execution budget was not consumed", len(provider.requests))
	}
	// Every retry is the SAME governed work: same source, base and contract.
	first := provider.requests[0]
	for i, request := range provider.requests[:2] {
		if request.Base != first.Base || request.Contract != first.Contract || request.SourceSnapshot != first.SourceSnapshot {
			t.Fatalf("invocation %d changed its governed bindings: %+v vs %+v", i, request, first)
		}
	}
	// The first two were initial implementation against the trusted base, and
	// left no checkpoint behind.
	// The retried invocation is still an INITIAL implementation against the
	// trusted base, not a continuation of work that was never produced.
	for i, request := range provider.requests[:2] {
		if request.Purpose != InvocationInitial {
			t.Fatalf("invocation %d purpose = %q, want an initial implementation", i, request.Purpose)
		}
		if request.Candidate.Revision != first.Base.Revision {
			t.Fatalf("invocation %d was bound to %q, not the trusted base %q", i, request.Candidate.Revision, first.Base.Revision)
		}
	}
	state := fixture.state(runID)
	if state.projection.Checkpoints != 0 {
		t.Fatalf("zero-delta invocations produced %d checkpoint(s)", state.projection.Checkpoints)
	}
	events := journalOf(t, fixture.runtime, runID)
	if count := countEvents(events, EventCandidateCommitted); count != 1 {
		t.Fatalf("candidate commits = %d, want exactly the one real change", count)
	}
}

// TestInitialZeroDeltaBudgetStopTerminatesPreciselyAtTheCeiling is case B: the
// budget bounds the retries, and running out of it is a precise bounded
// failure rather than a run that quietly concludes anything.
func TestInitialZeroDeltaBudgetStopTerminatesPreciselyAtTheCeiling(t *testing.T) {
	fixture, provider, runID := budgetFixture(t) // every invocation is zero-delta

	limit := fixture.deps.Budgets.MaxExecutionAttempts
	if limit == 0 {
		t.Fatal("the fixture has no execution attempt ceiling to prove")
	}
	if len(provider.requests) != limit {
		t.Fatalf("provider was invoked %d time(s) against a ceiling of %d", len(provider.requests), limit)
	}
	state := fixture.state(runID)
	if !terminalDisposition(state.snapshot.Disposition) {
		t.Fatalf("the run did not terminate: %q", state.snapshot.Disposition)
	}
	// The reason names the budget, not an unknown failure.
	if !strings.Contains(state.snapshot.Reason, "execution.invoke") || !strings.Contains(state.snapshot.Reason, "exhausted") {
		t.Fatalf("terminal reason = %q, want a bounded execution exhaustion", state.snapshot.Reason)
	}
	class, failing := state.currentHeadFailure()
	if failing && class != FailureExecutionIncomplete {
		t.Fatalf("recorded failure class = %q", class)
	}
}

// TestCheckpointSurvivesAZeroDeltaContinuation is case D, and it is the case
// the #49 run died on: two real checkpoints were committed and reassessed, one
// continuation added nothing, and the whole run was discarded.
func TestCheckpointSurvivesAZeroDeltaContinuation(t *testing.T) {
	fixture, provider, runID := budgetFixture(t,
		budgetStep{mutate: "first.go"}, // checkpoint
		budgetStep{},                   // zero-delta continuation
		budgetStep{mutate: "second.go", stop: StopCompleted}, // finishes
	)

	state := fixture.state(runID)
	events := journalOf(t, fixture.runtime, runID)

	// The checkpoint the first invocation produced is still the subject the
	// zero-delta continuation was bound to - not the base, and not a new
	// commit invented for a zero delta.
	if len(provider.requests) < 3 {
		t.Fatalf("only %d invocation(s); the continuation was not retried", len(provider.requests))
	}
	checkpoint := provider.requests[1].Candidate.Revision
	if checkpoint == "" {
		t.Fatal("the zero-delta continuation was not bound to the existing checkpoint")
	}
	if provider.requests[1].Purpose != InvocationContinuation {
		t.Fatalf("purpose = %q, want a continuation", provider.requests[1].Purpose)
	}
	if provider.requests[2].Candidate.Revision != checkpoint {
		t.Fatalf("the next continuation moved to %q; the checkpoint must be preserved exactly",
			provider.requests[2].Candidate.Revision)
	}
	// A zero delta creates no duplicate checkpoint, commit or reassessment.
	if got := countEvents(events, EventCandidateCheckpointed); got != 1 {
		t.Fatalf("checkpoints = %d, want exactly the one real one", got)
	}
	// The zero-delta continuation itself created nothing: the run's only
	// commits are the checkpoint and the completing change.
	if got := countEvents(events, EventCandidateCommitted); got > 1 {
		t.Fatalf("candidate commits = %d; a zero delta fabricated one", got)
	}
	_ = state
}

// TestZeroDeltaContinuationAtTheCeilingKeepsItsCheckpoint is case E: the run
// terminates precisely and the accumulated work stays inspectable rather than
// being reset or discarded.
func TestZeroDeltaContinuationAtTheCeilingKeepsItsCheckpoint(t *testing.T) {
	fixture, _, runID := budgetFixture(t, budgetStep{mutate: "first.go"}) // then zero-delta forever

	state := fixture.state(runID)
	if !terminalDisposition(state.snapshot.Disposition) {
		t.Fatalf("the run did not terminate: %q", state.snapshot.Disposition)
	}
	if state.projection.CandidateRevision == "" {
		t.Fatal("the accumulated checkpoint was discarded when the budget ran out")
	}
	if state.projection.CandidateComplete {
		t.Fatal("an exhausted run promoted its checkpoint to a complete candidate")
	}
	if got := countEvents(journalOf(t, fixture.runtime, runID), EventCandidateCheckpointed); got != 1 {
		t.Fatalf("checkpoints = %d, want the single real one preserved", got)
	}
}

// TestNormalCompletionWithNoDeltaNeedsNoFakeCommit is case F: a continuation
// that finishes without further change inherits the checkpoint rather than
// manufacturing a commit for zero bytes.
func TestNormalCompletionWithNoDeltaNeedsNoFakeCommit(t *testing.T) {
	fixture, _, runID := budgetFixture(t,
		budgetStep{mutate: "first.go"},
		budgetStep{stop: StopCompleted}, // completes, changes nothing
	)
	events := journalOf(t, fixture.runtime, runID)
	if got := countEvents(events, EventCandidateCommitted) + countEvents(events, EventCandidateCheckpointed); got != 1 {
		t.Fatalf("candidate commits/checkpoints = %d, want exactly one for the single real change", got)
	}
	if fixture.state(runID).projection.CandidateRevision == "" {
		t.Fatal("the inherited checkpoint was lost")
	}
	// Inheriting the revision is only half of it: a run that completes has to
	// arrive at a COMPLETE candidate, not a permanently checkpointed one.
	if !fixture.state(runID).projection.CandidateComplete {
		t.Fatal("a completed zero-delta continuation left its candidate incomplete")
	}
}

// TestUnknownProviderFailuresStayUnknown is cases G and H. A previous
// checkpoint is not permission to retry every error: only a bound the runtime
// itself set is a diagnosed stop.
func TestUnknownProviderFailuresStayUnknown(t *testing.T) {
	for name, steps := range map[string][]budgetStep{
		"with no prior work": {{stop: StopProviderError, detail: "provider refused"}},
		"with a checkpoint": {
			{mutate: "first.go"},
			{stop: StopProviderError, detail: "provider refused"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture, provider, runID := budgetFixture(t, steps...)
			state := fixture.state(runID)
			class, failing := state.currentHeadFailure()
			if failing && class == FailureExecutionIncomplete {
				t.Fatal("an undiagnosed provider failure was treated as a runtime-bounded stop")
			}
			if got, want := len(provider.requests), len(steps); got != want {
				t.Fatalf("provider invoked %d time(s); an unknown failure must not consume retries (want %d)", got, want)
			}
			if !terminalDisposition(state.snapshot.Disposition) {
				t.Fatalf("an unknown failure did not stop the run: %q", state.snapshot.Disposition)
			}
		})
	}
}

// TestBudgetRoutingReplaysDeterministically is case J: the decision after a
// zero-delta stop is a function of durable state, so a restart between the
// checkpoint and the continuation reaches the same next step.
func TestBudgetRoutingReplaysDeterministically(t *testing.T) {
	fixture, _, runID := budgetFixture(t,
		budgetStep{mutate: "first.go"},
		budgetStep{},
	)
	first := fixture.state(runID)
	firstKey, firstWanted := bindExecutionInvoke(first)

	// A fresh load of the same journal is the restart.
	replayed := fixture.state(runID)
	replayedKey, replayedWanted := bindExecutionInvoke(replayed)

	if firstWanted != replayedWanted || firstKey != replayedKey {
		t.Fatalf("replay changed the next execution binding: %q/%t vs %q/%t",
			firstKey, firstWanted, replayedKey, replayedWanted)
	}
	if firstWanted && !strings.HasPrefix(firstKey, "continuation|") {
		t.Fatalf("next execution binding = %q, want a continuation of the exact checkpoint", firstKey)
	}
}

func countEvents(events []EngineeringEvent, kind string) int {
	count := 0
	for _, e := range events {
		if e.Type == kind {
			count++
		}
	}
	return count
}
