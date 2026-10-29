package runtime

import (
	"errors"
	"strconv"
)

// UpdateFailure is the structured account of a refused upgrade build, so an
// operator and a program can tell WHICH step failed, how many times it was
// tried, whether the failure was infrastructure or deterministic, and which
// runtime-owned Docker operation to look at - not just "exit status 1" (#674).
//
// It is descriptive only. It is filled from the error the build already
// returned, after the fact, and nothing reads it to decide whether to trust,
// build, retry or activate anything.
type UpdateFailure struct {
	// Step is the build step that failed: "toolchain" or "build".
	Step string `json:"step"`
	// Attempts is how many times the step ran, the first try included.
	Attempts int `json:"attempts"`
	// Transient is whether the last failure was an infrastructure failure of
	// the Docker lifecycle (the only class retried). With Attempts>1 it means
	// the bounded retry was spent; false means a deterministic failure that
	// was never eligible.
	Transient   bool   `json:"transient"`
	OperationID string `json:"operation_id,omitempty"`
	Container   string `json:"container,omitempty"`
	// Stderr is the sanitised, bounded tail of what Docker or the compiler said.
	Stderr string `json:"stderr,omitempty"`
}

// updateFailureOf extracts the build failure from a refusal, or nil when the
// refusal was not a sandbox build step (a trust refusal, a measurement error).
func updateFailureOf(err error) *UpdateFailure {
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		return nil
	}
	return &UpdateFailure{
		Step: failure.Step, Attempts: failure.Attempts, Transient: failure.Transient,
		OperationID: failure.OperationID, Container: failure.Container, Stderr: failure.Stderr,
	}
}

// Lines are the label/value pairs `controller status` prints under the
// upgrade line. The stderr tail is already part of the refusal's own text, so
// it is not repeated here; the JSON form carries it as its own field.
func (f UpdateFailure) Lines() [][2]string {
	class := "deterministic (not retried)"
	if f.Transient {
		class = "infrastructure"
		if f.Attempts > 1 {
			class += ", bounded retry spent"
		}
	}
	lines := [][2]string{
		{"upgrade step", f.Step + " (" + strconv.Itoa(f.Attempts) + " attempt(s), " + class + ")"},
	}
	if f.OperationID != "" {
		lines = append(lines, [2]string{"docker operation", f.OperationID})
	}
	if f.Container != "" {
		lines = append(lines, [2]string{"docker container", f.Container})
	}
	return lines
}
