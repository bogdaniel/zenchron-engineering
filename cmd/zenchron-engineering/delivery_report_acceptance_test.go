package main

import (
	"os"
	"testing"
)

// TestDeliveryReportAcceptance is the acceptance test for the objective
// delivery report (#693). It was written from the issue, not from an
// implementation, and it fails until the feature exists. It is opt-in like the
// other browser tests, so ordinary CI skips it, and it is listed only in
// browser-acceptance/delivery-report.txt, never in the global manifest, so it
// only gates an objective that declares the feature.
//
// A real browser takes the rehearsal's controlled objective (two parallel
// producers, an integrator, an independent reviewer, one human decision) until
// every producer and the integration are complete and the review still awaits
// human publication authority, then reads the report an operator would read.
func TestDeliveryReportAcceptance(t *testing.T) {
	if os.Getenv("ZENCHRON_REACT_BROWSER_E2E") != "1" {
		t.Skip("opt-in browser host required")
	}
	f := newRehearsalFixture(t)
	runBrowserFixtureWithScript(t, f.root, f.built, f.driver, "check_delivery_report.mjs", []string{
		"ZENCHRON_E2E_REACT=1", "ZENCHRON_REHEARSAL_GATES=" + f.gates,
	})
}
