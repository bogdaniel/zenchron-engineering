package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestStatusTextAccountingModes(t *testing.T) {
	for _, mode := range []string{"legacy", "split"} {
		t.Run(mode, func(t *testing.T) {
			report := runtime.StatusReport{AccountingMode: mode, ActiveElapsed: 11 * time.Minute, AssuranceElapsed: 10 * time.Minute, ExecutionElapsed: time.Minute}
			if mode == "legacy" {
				report.ExecutionElapsed = report.ActiveElapsed
			}
			var out bytes.Buffer
			if err := renderStatusText(&out, statusView{StatusReport: report}); err != nil {
				t.Fatal(err)
			}
			text := out.String()
			if mode == "legacy" {
				for _, want := range []string{"legacy", "11m0s (authoritative legacy charge)", "10m0s (overlapping execution charge)"} {
					if !strings.Contains(text, want) {
						t.Fatalf("missing %q: %s", want, text)
					}
				}
				if strings.Contains(text, "(execution ") {
					t.Fatalf("legacy additive framing: %s", text)
				}
			} else if !strings.Contains(text, "11m0s (execution 1m0s, assurance 10m0s)") {
				t.Fatalf("missing split: %s", text)
			}
		})
	}
}
