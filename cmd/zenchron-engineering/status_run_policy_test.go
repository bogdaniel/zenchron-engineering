package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// INVARIANT (ADR-0003 B1, status truthfulness): an unverifiable budget member
// is rendered as unknown - in text and in JSON - and never as 0, 0s, or an
// omitted value that reads as "none".
func TestAnUnverifiableBudgetRendersAsUnknown(t *testing.T) {
	view := statusView{StatusReport: runtime.StatusReport{
		RunPolicy: runtime.RunPolicyStatus{
			Source:       runtime.RunPolicyLegacyControllerBinding,
			Unverifiable: []string{"lifecycle_deadline", "provider_inactivity_limit"},
		},
		Operation: &runtime.OperationStatus{Kind: "execution.invoke", InactivityLimitUnknown: true},
	}}

	var text bytes.Buffer
	if err := renderStatusText(&text, view); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	for _, want := range []string{"lifecycle_deadline=unknown", "provider_inactivity_limit=unknown", "inactivity limit unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("text status is missing %q:\n%s", want, out)
		}
	}
	for _, never := range []string{"lifecycle=0s", "inactivity=0s", "inactivity limit 0s"} {
		if strings.Contains(out, never) {
			t.Errorf("text status rendered an unknown as a value %q:\n%s", never, out)
		}
	}

	raw, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if budgets, present := decoded["budgets"]; !present || budgets != nil {
		t.Errorf("JSON budgets = %v (present %t), want an explicit null for unknown", budgets, present)
	}
	operation, _ := decoded["operation"].(map[string]any)
	if operation["inactivity_limit_unknown"] != true {
		t.Errorf("JSON operation %v does not mark the inactivity limit unknown, so its absence reads as no limit", operation)
	}

	// Known budgets still render as values.
	view.Budgets = &runtime.RunBudgets{WallLimit: time.Hour, ProviderInactivityLimit: 5 * time.Minute}
	view.RunPolicy.Unverifiable = nil
	text.Reset()
	if err := renderStatusText(&text, view); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "wall=1h0m0s") {
		t.Errorf("known budgets were not rendered:\n%s", text.String())
	}
}
