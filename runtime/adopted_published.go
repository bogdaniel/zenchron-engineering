package runtime

// WHAT IS ALREADY ON DISK, and why asking is not the same as reconciling.
//
// Controller version directories are immutable: publishing over one is refused
// rather than compared, because the installed artifact is the provenance of
// every run the controller in it has already governed. That law says nothing
// about READING one, and a controller that could not read one would be forced
// into the exact behaviour the law exists to prevent.
//
// The case is a crash. A successor is built and published, and the process
// dies before the transition is recorded. Nothing durable says an upgrade was
// under way, so the next attempt observes the same trusted main and builds the
// same revision again - into a directory that already exists, which is refused,
// every ten minutes, forever. The upgrade is stuck behind its own successful
// half.
//
// So the question asked here is narrow and answerable from evidence: IS THE
// SUCCESSOR FOR THIS EXACT SUBJECT ALREADY PUBLISHED? It is answered from the
// provenance the builder wrote beside the binary, and the binary is re-measured
// against it - a directory whose record and content disagree is not an artifact
// this controller adopts, it is a refusal. Nothing is written, nothing is
// replaced, and nothing is decided about a directory that names something else.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// AdoptedVersionName is the directory an adopted build of a revision is
// published as. The builder derives it and this derives it again, from one
// definition, so a lookup cannot go looking in a place a build would never
// write to.
func AdoptedVersionName(revision string) string { return "main-" + shortSHA(revision) }

// PublishedAdoptedController reports the successor already published for this
// exact subject, if there is one.
//
// found=false is "there is nothing here", which is the ordinary case and not a
// problem. An error is "there is something here and it does not hold up",
// which must never be quietly treated as nothing: that would rebuild into an
// immutability refusal, or worse, adopt whatever is there next time round.
func PublishedAdoptedController(controllerRoot string, subject RevisionRecord) (AdoptedBuildProvenance, bool, error) {
	var provenance AdoptedBuildProvenance
	directory := filepath.Join(controllerRoot, AdoptedVersionName(subject.Revision))
	record := filepath.Join(directory, "provenance.json")
	encoded, err := os.ReadFile(record)
	if os.IsNotExist(err) {
		return provenance, false, nil
	}
	if err != nil {
		return provenance, false, err
	}
	if err := json.Unmarshal(encoded, &provenance); err != nil {
		return provenance, false, fmt.Errorf("%s exists and its provenance could not be read: %w", directory, err)
	}
	// IT MUST BE THIS SUBJECT. A directory named for a revision is a claim
	// about a path; the record inside it is the claim about the artifact, and
	// they are allowed to disagree exactly once - here, where it is refused.
	if provenance.Source.Revision != subject.Revision || provenance.Source.Tree != subject.Tree {
		return AdoptedBuildProvenance{}, false, fmt.Errorf(
			"%s holds a build of %s/%s and the successor wanted is %s/%s",
			directory, shortSHA(provenance.Source.Revision), shortSHA(provenance.Source.Tree),
			shortSHA(subject.Revision), shortSHA(subject.Tree))
	}
	if provenance.Kind != ControllerAdopted {
		return AdoptedBuildProvenance{}, false, fmt.Errorf("%s holds a %q build, which is not a successor", directory, provenance.Kind)
	}
	// AND THE BINARY MUST BE WHAT THE RECORD SAYS. A record is a file beside
	// another file; measuring is what makes it evidence about the artifact
	// rather than about itself.
	measured, err := measureExecutable(provenance.OutputPath)
	if err != nil {
		return AdoptedBuildProvenance{}, false, fmt.Errorf("%s records a binary that could not be measured: %w", directory, err)
	}
	if measured != provenance.BinarySHA256 {
		return AdoptedBuildProvenance{}, false, fmt.Errorf(
			"%s records binary %s and the file measures %s",
			directory, shortSHA(provenance.BinarySHA256), shortSHA(measured))
	}
	return provenance, true, nil
}
