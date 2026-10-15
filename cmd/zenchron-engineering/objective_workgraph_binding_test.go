package main

import (
	"bytes"
	"testing"
)

// TestEveryPlanProposalBindsAWorkGraph pins a fact this package's dispatch
// chain already implements but never had a direct test: proposeSerialized
// calls proposePlanAndGraph unconditionally (plan.go:417), for the plain
// "plan" CLI command exactly as for "objective" and for the browser's
// /v1/objectives (revisePlan, objective.go:129, which calls the SAME
// proposeSerialized). A plan proposed any of those three ways is bound to a
// WorkGraph at propose time; only ACTIVATING its units awaits approval
// (BindObjectivePlan's own doc comment). This is what makes the dashboard's
// plan-detail "WorkGraph" panel (frontend/src/pages.tsx) light up for a
// dashboard-created objective with no separate "bind" step, CLI or
// otherwise - #471/#479's "observe actual WorkGraph progression" already
// has its backend half; nothing here needs building, only proving.
func TestEveryPlanProposalBindsAWorkGraph(t *testing.T) {
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	var out bytes.Buffer
	if _, err := autonomy([]string{"plan", "issue", "41", "--deterministic", "--text", "--config", configPath},
		planOverrides(t, 41), &out); err != nil {
		t.Fatalf("propose: %v\n%s", err, out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("work graph:")) {
		t.Fatalf("the plain plan-propose path must bind a WorkGraph exactly as the objective path does: %s", out.String())
	}
}
