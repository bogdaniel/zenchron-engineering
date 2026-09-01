package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestDoctorReportsHowTheControllerWasResolved proves the CLI carries the
// resolution OUTCOME into doctor, not just a value.
//
// The first version of this wiring wrote `if build, err := controllerBuild();
// err == nil { ... }` and dropped the error on the floor. Doctor then saw the
// zero value and reported an unattested build - a legal, deliberate property -
// for a binary whose provenance simply could not be measured. The check was
// answering a question nobody asked.
func TestDoctorReportsHowTheControllerWasResolved(t *testing.T) {
	valid := runtime.ControllerBuild{
		Kind: runtime.ControllerAdopted, Version: "main-49736da0",
		SourceRevision: strings.Repeat("a", 40), SourceTree: strings.Repeat("b", 40),
		BinarySHA256: strings.Repeat("c", 64),
	}

	t.Run("a measurement failure reaches doctor as a failure", func(t *testing.T) {
		in := doctorInput(autonomyFlags{}, autonomyOverrides{
			ControllerBuildResolver: func() (runtime.ControllerBuild, error) {
				return runtime.ControllerBuild{}, fmt.Errorf("cannot measure the running executable")
			},
		})
		if in.ControllerBuildError == nil {
			t.Fatal("the resolution failure was discarded before doctor could see it")
		}
		if in.ControllerBuild.Attested() {
			t.Fatalf("a failed resolution produced a provenance claim: %#v", in.ControllerBuild)
		}
	})

	t.Run("a resolved build reaches doctor as itself", func(t *testing.T) {
		in := doctorInput(autonomyFlags{}, autonomyOverrides{
			ControllerBuildResolver: func() (runtime.ControllerBuild, error) { return valid, nil },
		})
		if in.ControllerBuildError != nil {
			t.Fatalf("a successful resolution reported an error: %v", in.ControllerBuildError)
		}
		if in.ControllerBuild != valid {
			t.Fatalf("controller build = %#v, want %#v", in.ControllerBuild, valid)
		}
	})

	// The explicit override still wins, so a CLI test can pin the identity it
	// asserts on instead of measuring whatever binary the test runner is.
	t.Run("an explicit override still wins", func(t *testing.T) {
		in := doctorInput(autonomyFlags{}, autonomyOverrides{
			ControllerBuild: &valid,
			ControllerBuildResolver: func() (runtime.ControllerBuild, error) {
				return runtime.ControllerBuild{}, fmt.Errorf("must not be consulted")
			},
		})
		if in.ControllerBuildError != nil || in.ControllerBuild != valid {
			t.Fatalf("override ignored: %#v / %v", in.ControllerBuild, in.ControllerBuildError)
		}
	})
}
