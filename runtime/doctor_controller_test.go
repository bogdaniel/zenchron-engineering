package runtime

import (
	"context"
	"strings"
	"testing"
)

// TestDoctorNamesTheRunningControllerBuild is the check an operator standing in
// front of a freshly built binary needs: what IS this thing? The distinction
// between an adopted runtime and a build of source that has not itself been
// adopted already existed in the model and was already recorded on every run,
// but no read-only command would say it out loud.
//
// It reports; it never gates. Which build you are running is a fact about your
// situation, not a fault in it, so no case here is a FAIL.
func TestDoctorNamesTheRunningControllerBuild(t *testing.T) {
	adopted := ControllerBuild{
		Kind: ControllerAdopted, Version: "main-49736da0",
		SourceRevision: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40),
		BinarySHA256: strings.Repeat("c", 64),
	}
	preAdoption := adopted
	preAdoption.Kind = ControllerPreAdoptionBuild
	preAdoption.Version = "issue-29-b63d3148"

	for name, tc := range map[string]struct {
		build   ControllerBuild
		status  DoctorStatus
		says    []string
		saysNot []string
	}{
		"adopted": {
			build:  adopted,
			status: DoctorPass,
			says: []string{"adopted build", "main-49736da0", adopted.SourceRevision, adopted.SourceTree,
				"sha256:" + adopted.BinarySHA256, "established by external merge and never by the runtime itself"},
			saysNot: []string{"confers no authority"},
		},
		"pre-adoption": {
			build:  preAdoption,
			status: DoctorPass,
			says: []string{"pre_adoption_build build", "issue-29-b63d3148",
				"confers no authority on its own source"},
			saysNot: []string{"established by external merge"},
		},
		"unattested": {
			build:   ControllerBuild{Kind: ControllerUnattested},
			status:  DoctorWarn,
			says:    []string{"no provenance claim", "unattested"},
			saysNot: []string{"sha256:"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			check := doctorController(DoctorInput{ControllerBuild: tc.build})
			if check.ID != "controller.build" || check.Group != doctorGroupController {
				t.Fatalf("check identity = %q/%q", check.Group, check.ID)
			}
			if check.Status != tc.status {
				t.Fatalf("status = %q, want %q: %s", check.Status, tc.status, check.Reason)
			}
			for _, want := range tc.says {
				if !strings.Contains(check.Reason, want) {
					t.Fatalf("reason omits %q: %s", want, check.Reason)
				}
			}
			for _, unwanted := range tc.saysNot {
				if strings.Contains(check.Reason, unwanted) {
					t.Fatalf("reason wrongly claims %q: %s", unwanted, check.Reason)
				}
			}
		})
	}

	// The check is part of the report, and an unattested controller does not
	// fail the preflight: a legal build is not a fault.
	report := Doctor(context.Background(), DoctorInput{})
	if _, found := report.Check("controller.build"); !found {
		t.Fatal("the doctor report does not include the controller check")
	}
}
