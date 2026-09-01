package runtime

import (
	"context"
	"fmt"
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

	partial := adopted
	partial.SourceTree = ""
	unknownKind := adopted
	unknownKind.Kind = "trusted"
	badDigest := adopted
	badDigest.BinarySHA256 = strings.ToUpper(strings.Repeat("c", 64))

	for name, tc := range map[string]struct {
		build   ControllerBuild
		err     error
		status  DoctorStatus
		prefix  string
		says    []string
		saysNot []string
	}{
		"adopted": {
			build:  adopted,
			status: DoctorPass,
			prefix: "this controller's build provenance is adopted:",
			says: []string{"main-49736da0", adopted.SourceRevision, adopted.SourceTree,
				"sha256:" + adopted.BinarySHA256, "established by external merge and never by the runtime itself"},
			saysNot: []string{"confers no authority", "a adopted build"},
		},
		"pre-adoption": {
			build:  preAdoption,
			status: DoctorPass,
			prefix: "this controller's build provenance is pre_adoption_build:",
			says: []string{"issue-29-b63d3148",
				"confers no authority on its own source"},
			saysNot: []string{"established by external merge", "a pre_adoption_build build"},
		},
		"unattested": {
			build:   ControllerBuild{Kind: ControllerUnattested},
			status:  DoctorWarn,
			prefix:  "this controller's build provenance is unattested:",
			says:    []string{"unattested", "records nothing about which source produced it"},
			saysNot: []string{"sha256:"},
		},
		// A build that could not be MEASURED is not a build that claims
		// nothing. Reporting the first as the second turns a broken preflight
		// into a design choice, and an operator reads WARN where the truth is
		// that nothing is known at all.
		"measurement failure": {
			err:     fmt.Errorf("cannot read the running executable: permission denied"),
			status:  DoctorFail,
			says:    []string{"could not be established", "permission denied", "not an unattested build"},
			saysNot: []string{"makes no provenance claim", "is a  build"},
		},
		"unknown kind": {
			build:   unknownKind,
			status:  DoctorFail,
			says:    []string{"not complete or not well formed", "trusted"},
			saysNot: []string{"confers no authority", "established by external merge"},
		},
		"partial attestation": {
			build:   partial,
			status:  DoctorFail,
			says:    []string{"not complete or not well formed", "source_tree"},
			saysNot: []string{"established by external merge"},
		},
		"malformed binary digest": {
			build:   badDigest,
			status:  DoctorFail,
			says:    []string{"not complete or not well formed", "binary_sha256"},
			saysNot: []string{"established by external merge"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			check := doctorController(DoctorInput{ControllerBuild: tc.build, ControllerBuildError: tc.err})
			if check.ID != "controller.build" || check.Group != doctorGroupController {
				t.Fatalf("check identity = %q/%q", check.Group, check.ID)
			}
			if check.Status != tc.status {
				t.Fatalf("status = %q, want %q: %s", check.Status, tc.status, check.Reason)
			}
			if tc.prefix != "" && !strings.HasPrefix(check.Reason, tc.prefix) {
				t.Fatalf("reason = %q, want prefix %q", check.Reason, tc.prefix)
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

	// A provenance that cannot be established, or that does not survive its own
	// validation rules, fails the WHOLE preflight. A diagnostic that reports a
	// broken claim as healthy is worse than no diagnostic, because it reads as
	// proof.
	for name, in := range map[string]DoctorInput{
		"unmeasurable": {ControllerBuildError: fmt.Errorf("boom")},
		"invalid":      {ControllerBuild: unknownKind},
	} {
		t.Run("report fails on "+name, func(t *testing.T) {
			if got := Doctor(context.Background(), in); got.Status != DoctorFail {
				t.Fatalf("report status = %q, want %q", got.Status, DoctorFail)
			}
		})
	}
}
