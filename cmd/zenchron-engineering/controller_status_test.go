package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// THE EXIT CONTRACT distinguishes severities that a healthy/unhealthy split
// would merge. Zero availability and split-brain authority are not the same
// thing, and automation must be able to tell them apart.
func TestControllerStatusExitCodes(t *testing.T) {
	for _, test := range []struct {
		name   string
		status runtime.ControllerStatus
		want   int
	}{
		{"consistent and serving", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.Serving}, 0},
		{"consistent and not serving", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.NotServing}, 0},
		{"consistent with a drifted pointer", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.NotServing,
			Projection: runtime.ProjectionObservation{State: runtime.ProjectionDrift}}, 0},
		{"nothing observed", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableConsistent, Serving: runtime.ServingUnknown}, 0},
		{"no activation recorded", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableUnrecorded, Serving: runtime.ServingUnknown}, 0},
		{"two authorities", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableViolation, Serving: runtime.Serving}, 1},
		{"a torn observation", runtime.ControllerStatus{
			DurableConsistency: runtime.DurableUnstable, Serving: runtime.ServingUnknown}, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := statusExitCode(test.status); got != test.want {
				t.Fatalf("exit = %d, want %d", got, test.want)
			}
		})
	}
}

// The rendering says UNKNOWN for what was not observed, rather than leaving a
// blank a reader would take for "none".
func TestControllerStatusRendersUnknownRatherThanBlank(t *testing.T) {
	var out bytes.Buffer
	renderControllerStatus(&out, runtime.ControllerStatus{
		DurableConsistency: runtime.DurableConsistent,
		Serving:            runtime.ServingUnknown,
		Live:               runtime.LiveObservation{Reachable: false, Detail: "no control endpoint is available"},
		Projection:         runtime.ProjectionObservation{State: runtime.ProjectionMissing},
	})
	rendered := out.String()
	for _, want := range []string{"role ownership", "UNKNOWN", "work admission", "serving", "UNKNOWN"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the rendering omits %q:\n%s", want, rendered)
		}
	}
	// A reachable controller that reported neither fact renders the same way.
	out.Reset()
	renderControllerStatus(&out, runtime.ControllerStatus{
		DurableConsistency: runtime.DurableConsistent, Serving: runtime.ServingUnknown,
		Live: runtime.LiveObservation{Reachable: true, Snapshot: &runtime.LiveControllerSnapshot{}},
	})
	if strings.Count(out.String(), "UNKNOWN") < 2 {
		t.Fatalf("unreported facts did not render as unknown:\n%s", out.String())
	}
}
