package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestInvocationFailureGatesCandidateAdmission(t *testing.T) {
	for _, tc := range []struct {
		name  string
		class FailureClass
		paths int
	}{
		{"background_mutated", FailureProviderBackgroundWorkUnresolved, 5},
		{"background_unchanged", FailureProviderBackgroundWorkUnresolved, 0},
		{"other_failure_mutated", FailureUnknown, 5},
		{"success_mutated", "", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPhase8Fixture(t)
			f.deps.Budgets.MaxExecutionAttempts = 1
			result := ExecutionResult{ProviderID: "test-provider", Outcome: Succeeded}
			if tc.class != "" {
				// Even a provider's success outcome cannot override its failure.
				result.Failure = &ProviderFailure{Classification: tc.class}
			}
			f.provider.Result = result
			f.provider.mutate = func(dir string) error {
				for i := 0; i < tc.paths; i++ {
					if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("change%d.txt", i)), []byte("preserved work\n"), 0600); err != nil {
						return err
					}
				}
				return nil
			}
			f.runtime = f.newRuntime(f.deps)
			id := f.start()
			f.reconcile(id)
			state := f.state(id)
			events := journalOf(t, f.runtime, id)
			if tc.class == "" {
				onlyPayload[CandidateCommittedPayload](t, events, EventCandidateCommitted)
				if state.projection.Assurance == nil || !state.projection.Assurance.Passed {
					t.Fatal("successful mutation was not assured")
				}
				return
			}
			for _, e := range events {
				switch e.Type {
				case EventCandidateCommitted, EventAssuranceObserved, EventAuthorityEvaluated, EventExecutionCompleted:
					t.Fatalf("refused invocation crossed %s", e.Type)
				}
			}
			notPublished(t, f, id)
			for _, op := range state.snapshot.Operations {
				if op.Kind == OpCandidateCommit || op.Kind == OpAssuranceGo {
					t.Fatalf("refusal planned completed candidate work: %s", op.Kind)
				}
			}
			if state.snapshot.Reason == ReasonGoalStateReached {
				t.Fatal("refused invocation reached goal")
			}
			ops, _ := executions(t, state)
			if len(ops) != 1 || ops[0].State != OperationFailed {
				t.Fatalf("invocation not failed: %+v", ops)
			}
			held := state.snapshot.HeldMaterial
			if tc.paths == 0 {
				if held != nil {
					t.Fatalf("unchanged refusal held material: %+v", held)
				}
				return
			}
			if held == nil || held.Kind != HeldUncommitted || held.PathCount != tc.paths || held.ContentDigest == "" {
				t.Fatalf("refused material not held: %+v", held)
			}
			for i := 0; i < tc.paths; i++ {
				data, err := os.ReadFile(filepath.Join(candidateDir(f.deps.StateDir, id), fmt.Sprintf("change%d.txt", i)))
				if err != nil || string(data) != "preserved work\n" {
					t.Fatalf("material lost: %q %v", data, err)
				}
			}
			f.runtime = f.newRuntime(f.deps)
			f.reconcile(id)
			if f.state(id).snapshot.HeldMaterial == nil {
				t.Fatal("restart lost held material")
			}
		})
	}
}

// A refusal spends the same finite retry authority even when its files persist.
func TestMutatedRefusalDoesNotResetRetryBudget(t *testing.T) {
	f := newPhase8Fixture(t)
	f.deps.Budgets.MaxExecutionAttempts = 3
	f.provider.Result = ExecutionResult{
		ProviderID: "test-provider", Outcome: Succeeded,
		Failure: &ProviderFailure{Classification: FailureProviderBackgroundWorkUnresolved},
	}
	f.provider.mutate = func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "partial.txt"), []byte("partial"), 0600)
	}
	f.runtime = f.newRuntime(f.deps)
	id := f.start()
	f.reconcile(id)
	state := f.state(id)
	ops, _ := executions(t, state)
	if len(ops) != 1 || ops[0].Attempt != 3 || ops[0].State != OperationFailed ||
		state.snapshot.HeldMaterial == nil {
		t.Fatalf("retry authority or preservation changed: ops=%+v held=%+v", ops, state.snapshot.HeldMaterial)
	}
	notPublished(t, f, id)
	for _, op := range state.snapshot.Operations {
		if op.Kind == OpCandidateCommit || op.Kind == OpAssuranceGo {
			t.Fatalf("refusal planned completed candidate work: %s", op.Kind)
		}
	}
}
