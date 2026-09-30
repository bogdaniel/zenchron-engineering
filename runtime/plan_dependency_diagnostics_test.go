package runtime

import (
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestDependencyDiagnosticsAreReadOnlyAndTransitive(t *testing.T) {
	plan := domain.EngineeringPlan{Stages: []domain.PlanStage{
		{ID: "producer"},
		{ID: "review", DependsOn: []string{"waiting", "producer"}},
		{ID: "gate", DependsOn: []string{"review", "producer"}},
		{ID: "waiting"},
		{ID: "unrelated"},
	}}
	for _, reason := range []string{"run_wall_budget_exhausted", "assurance_blocked", ""} {
		t.Run(reason, func(t *testing.T) {
			snapshot := PlanSnapshot{Stages: map[string]PlanStageProjection{
				"producer": {StageID: "producer", State: PlanStageFailed, Reason: reason},
			}}
			view := snapshot.withDependencyBlockers(plan)
			wantReason := reason
			if wantReason == "" {
				wantReason = "failed"
			}
			want := []PlanDependencyBlocker{{StageID: "producer", Reason: wantReason}}
			for _, id := range []string{"review", "gate"} {
				stage := view.Stages[id]
				if stage.State != PlanStagePending || stage.Reason != "" || !reflect.DeepEqual(stage.BlockedBy, want) {
					t.Fatalf("%s: %+v", id, stage)
				}
				if ready, _ := planDependenciesSatisfied(plan.Stages[map[string]int{"review": 1, "gate": 2}[id]], snapshot); ready {
					t.Fatal("blocked stage became executable")
				}
			}
			if len(snapshot.Stages) != 1 || len(snapshot.Stages["producer"].BlockedBy) != 0 {
				t.Fatal("diagnostics mutated source snapshot")
			}
			if len(view.Stages["unrelated"].BlockedBy) != 0 || len(view.Stages["waiting"].BlockedBy) != 0 {
				t.Fatal("ordinary pending stage annotated")
			}
			snapshot.Stages["producer"] = PlanStageProjection{StageID: "producer", State: PlanStageCompleted}
			if got := snapshot.withDependencyBlockers(plan).Stages["gate"].BlockedBy; len(got) != 0 {
				t.Fatalf("stale blockers: %+v", got)
			}
		})
	}
}
