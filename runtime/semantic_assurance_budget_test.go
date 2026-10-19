package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ignoresCancellation models a semantic verifier that hangs until its context
// ends and then answers anyway, as a provider that does not honour cancellation
// would. Its answer comes from the real fake, so it is an ACCEPTED verdict.
type ignoresCancellation struct {
	inner *FakeSemanticAssuranceProvider
}

func (s ignoresCancellation) ProducedEvidenceClasses() []domain.EvidenceClass {
	return s.inner.ProducedEvidenceClasses()
}

func (s ignoresCancellation) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	select {
	case <-ctx.Done(): // ends at the allowance's deadline, 1ms after it starts
	case <-time.After(10 * time.Second): // only reached if the bound is missing
	}
	return s.inner.Assure(context.Background(), request)
}

// INVARIANT (#640): the physical assurance bound covers assurance.semantic too.
// A semantic verifier that outlives the allowance cannot produce an accepted
// verdict: nothing is journalled as semantic assurance, the operation does not
// succeed, the run fails with the assurance reason and publishes nothing.
func TestSemanticAssuranceCannotOutliveTheAllowance(t *testing.T) {
	budgets := RunBudgets{WallLimit: 30 * time.Minute, AttemptWallLimit: 30 * time.Minute, AssuranceWallLimit: 30 * time.Minute, MaxExecutionAttempts: 2}
	// Automated assurance spends all but one millisecond of the allowance. With
	// the clock frozen between steps that remainder is exact.
	fixture, provider, _ := assuranceFixture(t, budgets, 30*time.Minute-time.Millisecond,
		wallStep{mutate: true, complete: true, spend: time.Minute})
	semantic := &FakeSemanticAssuranceProvider{}
	fixture.deps.SemanticAssurance = ignoresCancellation{inner: semantic}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.clock.step = 0
	drive(fixture, provider, runID)

	if len(semantic.Requests) == 0 {
		t.Fatal("the scenario never reached semantic assurance; nothing was proven")
	}
	state := fixture.state(runID)
	// The frozen fixture clock charges the cut-off attempts nothing, so the
	// bounded attempt ceiling may end the run before the allowance does; either
	// way it is a failed budget boundary, never an accepted verdict.
	if !BudgetBoundary(state.snapshot.Disposition, state.snapshot.Reason) {
		t.Fatalf("run %s/%s, want a failed budget boundary", state.snapshot.Disposition, state.snapshot.Reason)
	}
	for _, event := range journalOf(t, fixture.runtime, runID) {
		if event.Type == EventSemanticAssuranceObserved {
			t.Fatalf("an expired semantic verification was journalled: %s", event.Payload)
		}
	}
	for _, op := range sortOperations(mapValues(state.snapshot.Operations)) {
		if op.Kind == OpAssuranceSemantic && op.State == Succeeded {
			t.Fatalf("semantic assurance %s succeeded after its allowance ended", op.ID)
		}
	}
	notPublished(t, fixture, runID)
}
