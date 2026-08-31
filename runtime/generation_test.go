package runtime

// Defect P. `run issue <n>` derived a deterministic identity and adopted
// whatever non-terminal run it found there, whichever controller had created
// it, so an invocation meant to start a fresh generation under a new controller
// reconciled a historical run instead - which is how
// run-0943e257539346f8763db04505cbf322 gained an event from a batch that
// believed it was starting something new.

import (
	"context"
	"errors"
	"testing"
)

func startFixture(t *testing.T) *phase8Fixture {
	t.Helper()
	return newPhase8Fixture(t)
}

// TestAdoptionRequiresTheSameController is the security half: a live generation
// this controller did not create is refused, by name, rather than reconciled.
func TestAdoptionRequiresTheSameController(t *testing.T) {
	fixture := startFixture(t)
	first, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if first.Adopted {
		t.Fatal("the first start adopted something that did not exist")
	}

	// Same controller: adoption is allowed and reported as adoption.
	again, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatalf("this controller could not continue its own live generation: %v", err)
	}
	if !again.Adopted || again.RunID != first.RunID {
		t.Fatalf("same-controller start returned %+v, want an adoption of %s", again, first.RunID)
	}

	// A different controller: refused, naming the run, without touching it.
	before := len(journalOf(t, fixture.runtime, first.RunID))
	other := fixture.deps
	other.ControllerBuild = ControllerBuild{
		Kind: ControllerPreAdoptionBuild, Version: "other", SourceRevision: "r", SourceTree: "t",
		BinarySHA256: "00000000000000000000000000000000000000000000000000000000000000ff",
	}
	stranger := fixture.newRuntime(other)
	_, err = stranger.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	var refused *RunAdoptionRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a foreign controller adopted a live generation: %v", err)
	}
	if refused.RunID != first.RunID {
		t.Fatalf("the refusal names run %q, want %q", refused.RunID, first.RunID)
	}
	if after := len(journalOf(t, fixture.runtime, first.RunID)); after != before {
		t.Fatalf("a refused adoption appended %d events to the existing run", after-before)
	}
}

// TestNewGenerationNeverAdoptsAndNeverDisturbs is the explicit fresh-generation
// operation: a new run id every time, and the previous generation left exactly
// as it was.
func TestNewGenerationNeverAdoptsAndNeverDisturbs(t *testing.T) {
	fixture := startFixture(t)
	first, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatal(err)
	}
	fixture.reconcile(first.RunID)
	beforeEvents := len(journalOf(t, fixture.runtime, first.RunID))
	beforeRun, _, err := fixture.store.Run(first.RunID)
	if err != nil {
		t.Fatal(err)
	}

	second, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, NewGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if second.Adopted {
		t.Fatal("an explicit new generation reported itself adopted")
	}
	if second.RunID == first.RunID {
		t.Fatalf("an explicit new generation reused run id %s", first.RunID)
	}
	// The old generation is immutable: same events, same disposition, same head.
	if after := len(journalOf(t, fixture.runtime, first.RunID)); after != beforeEvents {
		t.Fatalf("creating a new generation appended %d events to the old one", after-beforeEvents)
	}
	afterRun, _, err := fixture.store.Run(first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if afterRun.Disposition != beforeRun.Disposition || afterRun.Candidate != beforeRun.Candidate {
		t.Fatalf("the old generation moved: %+v -> %+v", beforeRun, afterRun)
	}
	// And a third explicit generation is distinct again.
	third, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, NewGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if third.RunID == first.RunID || third.RunID == second.RunID {
		t.Fatalf("a third generation reused an id: %s", third.RunID)
	}
}

// TestGenerationModesAcrossDispositions covers the states a previous generation
// can be in.
func TestGenerationModesAcrossDispositions(t *testing.T) {
	for name, settle := range map[string]func(*phase8Fixture, string){
		"waiting": func(f *phase8Fixture, runID string) {
			state := f.state(runID)
			if _, err := f.runtime.settle(state, Waiting, "awaiting_authority"); err != nil {
				f.t.Fatal(err)
			}
		},
		"failed": func(f *phase8Fixture, runID string) {
			state := f.state(runID)
			if _, err := f.runtime.settle(state, Failed, "no_progress"); err != nil {
				f.t.Fatal(err)
			}
		},
		"cancelled": func(f *phase8Fixture, runID string) {
			state := f.state(runID)
			if _, err := f.runtime.settle(state, Cancelled, "operator_stopped"); err != nil {
				f.t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := startFixture(t)
			first, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
			if err != nil {
				t.Fatal(err)
			}
			settle(fixture, first.RunID)

			adopted, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
			if err != nil {
				t.Fatal(err)
			}
			terminal := name != "waiting"
			if terminal {
				// A finished generation is a boundary: the next start is a new
				// run, not a resurrection of the old one.
				if adopted.Adopted || adopted.RunID == first.RunID {
					t.Fatalf("a %s generation was adopted: %+v", name, adopted)
				}
			} else if !adopted.Adopted || adopted.RunID != first.RunID {
				t.Fatalf("a waiting generation was not continued: %+v", adopted)
			}
			// An explicit new generation is always a new run, whatever the old
			// one's disposition.
			fresh, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, NewGeneration)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Adopted || fresh.RunID == first.RunID {
				t.Fatalf("an explicit new generation returned %+v", fresh)
			}
		})
	}
}

// TestWatchKeepsAdoptionSemantics states watch's behaviour explicitly: it
// continues its own live generation and never creates a second one alongside
// it, because an unattended loop must not multiply runs for one issue.
func TestWatchKeepsAdoptionSemantics(t *testing.T) {
	fixture := startFixture(t)
	first, err := fixture.runtime.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	again, err := fixture.runtime.StartOrResumeIssueRun(context.Background(), fixture.issue)
	if err != nil {
		t.Fatal(err)
	}
	if again != first {
		t.Fatalf("the watch entry point created a second generation: %s then %s", first, again)
	}
}
