package runtime

import (
	"strings"
	"testing"
)

// TestPublishedProvenanceNamesTheControllerBuild proves the published body
// answers the question a reviewer of a runtime-opened pull request actually
// has: was the runtime that governed this change itself adopted?
//
// A pre-adoption build governing a change demonstrates runtime behaviour and
// says nothing about whether that runtime's own source should be adopted. The
// body must state that distinction rather than leaving it in local run state.
func TestPublishedProvenanceNamesTheControllerBuild(t *testing.T) {
	const disclaimer = "confers no authority on the controller's"

	preAdoption := ControllerBuild{
		Kind:           ControllerPreAdoptionBuild,
		Version:        "issue-29-abcdef12",
		SourceRevision: "abcdef1234567890abcdef1234567890abcdef12",
		SourceTree:     "1234567890abcdef1234567890abcdef12345678",
		BinarySHA256:   strings.Repeat("a", 64),
	}
	rendered := strings.Join(controllerBuildLines(preAdoption), "\n")
	for _, want := range []string{
		"| controller build | `pre_adoption_build` |",
		"| controller source | `" + preAdoption.SourceRevision + "` |",
		"| controller tree | `" + preAdoption.SourceTree + "` |",
		"| controller binary | `sha256:" + preAdoption.BinarySHA256 + "` |",
		disclaimer,
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("published provenance omits %q:\n%s", want, rendered)
		}
	}

	// An adopted controller carries the same checkable rows, but the refusal
	// sentence is not boilerplate: it is only true of an unadopted build, so
	// printing it always would make it noise the reviewer learns to skip.
	adopted := preAdoption
	adopted.Kind = ControllerAdopted
	rendered = strings.Join(controllerBuildLines(adopted), "\n")
	if !strings.Contains(rendered, "| controller build | `adopted` |") {
		t.Fatalf("adopted build not named:\n%s", rendered)
	}
	if strings.Contains(rendered, disclaimer) {
		t.Fatalf("an adopted controller must not carry the unadopted disclaimer:\n%s", rendered)
	}

	// An unattested controller claims nothing. Rendering empty provenance rows
	// would present that absence as a claim, so it is named as the absence.
	rendered = strings.Join(controllerBuildLines(ControllerBuild{Kind: ControllerUnattested}), "\n")
	if !strings.Contains(rendered, "unattested") || strings.Contains(rendered, "controller binary") {
		t.Fatalf("unattested build must be named, not rendered as empty claims:\n%s", rendered)
	}
}
