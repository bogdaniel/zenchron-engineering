package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// #203: status distinguishes budget exhaustion holding valuable material from
// exhaustion with no material result, and names the held identity, the step it
// could not take, the unadmittable successor and why.
func TestStatusDistinguishesHeldMaterialFromNoMaterial(t *testing.T) {
	render := func(report runtime.StatusReport) (string, string) {
		t.Helper()
		view := statusView{StatusReport: report}
		var out bytes.Buffer
		if err := renderStatusText(&out, view); err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(out.String()), " "), nextOperatorAction(view)
	}
	failed := runtime.StatusReport{RunID: "run-1", Disposition: runtime.Failed, Reason: "run_wall_budget_exhausted"}

	text, next := render(failed)
	if !strings.Contains(text, "held material: none (no material result)") {
		t.Fatalf("exhaustion with no material is not stated:\n%s", text)
	}
	if strings.Contains(next, "holding") {
		t.Fatalf("next action claims held material: %s", next)
	}

	failed.HeldMaterial = &runtime.HeldMaterial{Kind: runtime.HeldUncommitted, Revision: "0123456789abcdef", Operation: "op-7",
		PathCount: 3, NextStep: runtime.OpCandidateCommit, BlockedBy: "run_wall_budget_exhausted",
		Successor: "continuation", SuccessorUnavailable: "run_active_work_exhausted", Disposition: runtime.HeldDisposition}
	text, next = render(failed)
	for _, want := range []string{
		"held material: held uncommitted rev=0123456789ab tree=unknown",
		"held content: operation=op-7 paths=3 digest=unknown",
		"held next step: candidate.commit successor=continuation unavailable: run_active_work_exhausted blocked by run_wall_budget_exhausted",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status does not render %q:\n%s", want, text)
		}
	}
	if !strings.Contains(next, "holding uncommitted material at 0123456789ab") || !strings.Contains(next, "nothing was published") {
		t.Fatalf("next action does not name the held material: %s", next)
	}

	// Not a budget boundary: no held line at all.
	text, _ = render(runtime.StatusReport{RunID: "run-2", Disposition: runtime.Failed, Reason: "no_progress"})
	if strings.Contains(text, "held material") {
		t.Fatalf("a non-budget failure renders a held-material line:\n%s", text)
	}
}
