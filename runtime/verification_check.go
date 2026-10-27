package runtime

import (
	"errors"
	"fmt"
)

// A requirement is satisfied only by evidence that proves THAT requirement. A
// passing automated-test observation proves the Go suite passed; it does not
// prove a specialised obligation such as the file-size policy was checked. So a
// verifier records each specialised, deterministic check it COMPLETED as a typed
// record on the assurance observation, and a claim that needs such a check is
// discharged only from an observation that carries a matching one.
//
// The record is optional and additive. An observation written before it existed
// carries none, and none is ever read as proof: absence means "not recorded",
// never "passed", and replay never invents one.

const (
	// CheckKindFileSizePolicy is the repository's file-size policy
	// (AGENTS.md section 4) checked on the exact candidate tree.
	CheckKindFileSizePolicy = "file_size_policy"
	// CheckKindBrowserAcceptance is the real-browser acceptance run against the
	// exact candidate, in the network-less sandbox (browser_acceptance.go).
	CheckKindBrowserAcceptance = "browser_acceptance"

	CheckOutcomePassed = "passed"
	CheckOutcomeFailed = "failed"

	maxVerificationChecks = 8
)

// VerificationCheck is one completed, typed check. Every member is a
// runtime-composed machine token; none is candidate text.
type VerificationCheck struct {
	// Kind says which check this is; new kinds are added here, not as new
	// boolean members on the observation.
	Kind string `json:"kind"`
	// Checker and CheckerRevision identify the implementation that ran it, so a
	// change to its rules is a change of identity.
	Checker         string `json:"checker"`
	CheckerRevision string `json:"checker_revision"`
	Outcome         string `json:"outcome"`
	// Commit and Tree are the exact candidate the check was run on.
	Commit string `json:"commit"`
	Tree   string `json:"tree"`
	// BaseRevision is the governing base revision whose rules the check used.
	BaseRevision string `json:"base_revision,omitempty"`
	// The members below are recorded by the browser check: the digest of the
	// exact tests it was asked to run, the digest of the base-revision test
	// sources that ran, what happened to each test, the environment it ran in,
	// the immutable transcript and the digest of the artifacts the tests wrote.
	// They are absent on a check that does not run tests.
	Selection       string        `json:"selection,omitempty"`
	Surface         string        `json:"surface,omitempty"`
	Results         *CheckResults `json:"results,omitempty"`
	Environment     string        `json:"environment,omitempty"`
	Artifact        string        `json:"artifact,omitempty"`
	ArtifactsSHA256 string        `json:"artifacts_sha256,omitempty"`
}

// CheckResults counts what happened to the tests a check selected. A selected
// test that produced no result is neither passed nor failed nor skipped, so
// Passed == Total is the only count that proves everything ran and passed.
type CheckResults struct {
	Total   int `json:"total"`
	Passed  int `json:"passed"`
	Failed  int `json:"failed"`
	Skipped int `json:"skipped"`
}

// validateVerificationChecks bounds the optional list and requires each record
// to be complete. A record that does not name its candidate is not a record.
func validateVerificationChecks(checks []VerificationCheck) error {
	if len(checks) > maxVerificationChecks {
		return fmt.Errorf("checks names %d records, above the %d record bound", len(checks), maxVerificationChecks)
	}
	var errs []error
	for index, check := range checks {
		prefix := fmt.Sprintf("checks[%d].", index)
		errs = append(errs,
			required(prefix+"kind", check.Kind),
			required(prefix+"checker", check.Checker),
			required(prefix+"checker_revision", check.CheckerRevision),
			required(prefix+"outcome", check.Outcome),
			required(prefix+"commit", check.Commit),
			required(prefix+"tree", check.Tree),
			bounded(prefix+"base_revision", check.BaseRevision))
		if check.Outcome != CheckOutcomePassed && check.Outcome != CheckOutcomeFailed {
			errs = append(errs, fmt.Errorf("%soutcome %q is neither %q nor %q", prefix, check.Outcome, CheckOutcomePassed, CheckOutcomeFailed))
		}
		errs = append(errs, bounded(prefix+"selection", check.Selection), bounded(prefix+"surface", check.Surface), bounded(prefix+"environment", check.Environment),
			bounded(prefix+"artifact", check.Artifact), bounded(prefix+"artifacts_sha256", check.ArtifactsSHA256))
		if check.Kind == CheckKindBrowserAcceptance {
			errs = append(errs, required(prefix+"selection", check.Selection), required(prefix+"surface", check.Surface), required(prefix+"environment", check.Environment),
				required(prefix+"artifact", check.Artifact), required(prefix+"base_revision", check.BaseRevision))
			if check.Results == nil {
				errs = append(errs, fmt.Errorf("%sresults is required for a browser acceptance record", prefix))
			}
		}
	}
	return errors.Join(errs...)
}
