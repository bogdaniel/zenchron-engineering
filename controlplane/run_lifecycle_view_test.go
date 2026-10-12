package controlplane

import (
	"strings"
	"testing"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestRunLifecycleButtonsUseGovernedBindingsAndExplainLimits(t *testing.T) {
	binding := strings.Repeat("a", 64)
	controller := Controller{ControllerBinding: binding}
	page := renderTemplate(t, runDetailTemplate, runDetailData{
		Status:     RunDetail{ID: "run-1", Disposition: rt.Active, Source: &Source{Repository: "acme/repo", Issue: 7}},
		Controller: controller,
	})
	for _, action := range []string{"resume", "refresh"} {
		needle := `data-action="/v1/runs/run-1/` + action + `" data-binding="` + binding + `"`
		if !strings.Contains(page, needle) {
			t.Errorf("%s lacks its exact route and controller binding", action)
		}
	}
	if strings.Contains(page, "Resume, retry, and source refresh are unavailable") || !strings.Contains(page, "Manual retry is unavailable") {
		t.Fatal("run detail has inaccurate lifecycle-action guidance")
	}
}
