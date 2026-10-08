package runtime

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

func TestAttemptHardFuseDefaultIsThreeHoursAndRunBounded(t *testing.T) {
	for _, tc := range []struct {
		name               string
		wall, stated, want time.Duration
	}{
		{"unstated multi-hour run", 6 * time.Hour, 0, 3 * time.Hour},
		{"shorter run", 90 * time.Minute, 0, 90 * time.Minute},
		{"explicit 25 minute fuse", 6 * time.Hour, 25 * time.Minute, 25 * time.Minute},
		{"explicit longer fuse", 6 * time.Hour, 4 * time.Hour, 4 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture, _ := wallFixture(t, RunBudgets{WallLimit: tc.wall, AttemptWallLimit: tc.stated})
			runID := fixture.start()
			status, err := fixture.runtime.Status(runID)
			if err != nil {
				t.Fatal(err)
			}
			if status.Budgets.AttemptWallLimit != tc.want {
				t.Fatalf("frozen fuse %s, want %s", status.Budgets.AttemptWallLimit, tc.want)
			}
		})
	}
}

func TestRepositoryCannotWidenAnUnstatedAttemptHardFuse(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		wall, stated, proposed int
		refused                bool
	}{
		{"default ceiling", 21600, 0, 10800, false},
		{"tighten default", 21600, 0, 7200, false},
		{"cannot widen default", 21600, 0, 14400, true},
		{"shorter run ceiling", 3600, 0, 3601, true},
		{"explicit longer ceiling", 21600, 14400, 14400, false},
		{"explicit shorter ceiling", 21600, 1500, 1501, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := OperatorConfig{Budgets: BudgetConfig{WallLimitSeconds: tc.wall, AttemptWallLimitSeconds: tc.stated}}
			tightened, err := config.Tighten(RepositoryConfig{Budgets: &RepositoryBudgets{AttemptWallLimitSeconds: &tc.proposed}})
			if (err != nil) != tc.refused {
				t.Fatalf("Tighten: %v, want refused=%v", err, tc.refused)
			}
			if !tc.refused && tightened.Budgets.AttemptWallLimitSeconds != tc.proposed {
				t.Fatal("accepted ceiling was not exact")
			}
		})
	}
	config := OperatorConfig{Budgets: BudgetConfig{WallLimitSeconds: 21600}}
	tightened, err := config.Tighten(RepositoryConfig{})
	if err != nil || tightened.Budgets.AttemptWallLimitSeconds != 0 {
		t.Fatal("absent repository fuse did not stay absent", err)
	}
}

func TestAttemptHardFuseChangesOnlyNewRunsAcrossRestart(t *testing.T) {
	for _, old := range []time.Duration{25 * time.Minute, 6 * time.Hour} {
		t.Run(old.String(), func(t *testing.T) {
			// 6h also represents the persisted pre-#497 default under a 6h run.
			fixture, _ := wallFixture(t, RunBudgets{WallLimit: 6 * time.Hour, AttemptWallLimit: old})
			runID := fixture.start()
			before, err := fixture.runtime.Status(runID)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenSQLiteOperationStore(fixture.stateDir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			deps := fixture.deps
			deps.Store = reopened
			deps.Budgets.AttemptWallLimit = 0
			fixture.runtime = fixture.newRuntime(deps)
			after, err := fixture.runtime.Status(runID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Budgets.AttemptWallLimit != old || after.RunPolicy.SHA256 != before.RunPolicy.SHA256 {
				t.Fatal("restart changed the historical frozen fuse")
			}
			next, err := fixture.runtime.StartIssueRun(context.Background(), fixture.issue, NewGeneration)
			if err != nil {
				t.Fatal(err)
			}
			current, err := fixture.runtime.Status(next.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if next.RunID == runID || current.Budgets.AttemptWallLimit != 3*time.Hour {
				t.Fatalf("new run did not freeze the new default: %+v", current.Budgets)
			}
		})
	}
}

// The provider boundary supplies recognized progress observations on an injected
// clock. #238/#322/#362 retain the process/oracle/termination-owner proofs.
type hardFuseProgressProvider struct {
	provider *wallProvider
}

func (p *hardFuseProgressProvider) Isolation() ProviderIsolation { return p.provider.Isolation() }
func (p *hardFuseProgressProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	record := execution.ProgressRecorder(ctx)
	for step := 1; step <= 18; step++ {
		p.provider.clock.advance(5 * time.Minute)
		if request.Deadline == nil || !p.provider.clock.Now().Before(*request.Deadline) {
			return ExecutionResult{}, fmt.Errorf("healthy provider hit its fuse at progress %d", step)
		}
		record(ProviderProgress{Key: fmt.Sprintf("%d:%d", request.Attempt, step)})
	}
	result, err := p.provider.Execute(ctx, request)
	result.Invocation.Deadline = request.Deadline
	result.Invocation.InactivityLimit = request.Budgets.InactivityLimit
	return result, err
}

func TestHealthyInvocationBeyond25MinutesUsesExactHardFuse(t *testing.T) {
	fixture, provider := wallFixture(t, RunBudgets{WallLimit: 6 * time.Hour}, wallStep{complete: true, mutate: true, spend: time.Second})
	fixture.deps.Provider = &hardFuseProgressProvider{provider: provider}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)
	if len(provider.requests) != 1 {
		t.Fatalf("healthy invocation count %d, want one", len(provider.requests))
	}
	state := fixture.state(runID)
	ops, _ := executions(t, state)
	if len(ops) != 1 || ops[0].State != Succeeded {
		t.Fatalf("healthy invocation did not complete: %+v", ops)
	}
	op, _, found, err := fixture.store.Operation(ops[0].ID)
	if err != nil || !found {
		t.Fatalf("read durable operation: %v", err)
	}
	started := ops[0]
	if started.DeadlineBound != BoundAttemptWall || !started.Deadline.Equal(started.StartedAt.Add(3*time.Hour)) {
		t.Fatalf("incorrect physical fuse: %+v", op)
	}
	if op.ConsumedExecution < 90*time.Minute || op.ConsumedExecution > 91*time.Minute {
		t.Fatalf("healthy work was not charged: %s", op.ConsumedExecution)
	}
	if remaining := ProviderInactivityRemaining(10*time.Minute, op, fixture.clock.Now()); remaining <= 0 {
		t.Fatal("recognized progress lost inactivity authority")
	}
	provenance := onlyPayload[ExecutionAttemptProvenance](t, state.events, EventExecutionAttemptProvenance)
	if provenance.Invocation.DeadlineBound != BoundAttemptWall || provenance.Invocation.Deadline == nil || !provenance.Invocation.Deadline.Equal(*started.Deadline) {
		t.Fatal("provenance did not preserve the exact effective hard fuse")
	}
}
