package runtime

import "fmt"

// Reviewer independence (#233).
//
// A plan stage's independence is a POLICY-stated requirement, compiled and
// evaluated against the full IndependenceDimension vocabulary in
// planning/resolver.go. An ad hoc `review pr` has no plan and no compiled
// requirement to read, so this file states the one bar #233 itself requires
// rather than reusing that compiler: the material producer must never be
// treated as its own independent reviewer, and neither may a reviewer that
// is merely a DIFFERENT name for the same underlying vendor/model (#233's own
// worked example: two profiles of the same vendor family must not silently
// satisfy independence). Both checks are expressed over the same two facts -
// agent identity and vendor family - that planning/resolver.go's
// independenceClass already keys its execution_agent and vendor_family
// dimensions on, so a future caller that DOES have a compiled
// IndependenceRequirement can still be routed through this file's checks for
// those two dimensions without this file ever importing planning.
type ReviewIndependenceError struct {
	Dimension string
	Detail    string
}

func (e *ReviewIndependenceError) Error() string {
	return fmt.Sprintf("reviewer independence in dimension %q does not hold: %s", e.Dimension, e.Detail)
}

// CheckReviewIndependence refuses a reviewer agent that collapses into the
// producer agent of the same subject. It is called BEFORE a reviewer
// invocation is ever dispatched: independence is a precondition of running
// the review, not a property checked afterward.
func CheckReviewIndependence(producer, reviewer ResolvedAgent) error {
	if producer.ID != "" && producer.ID == reviewer.ID {
		return &ReviewIndependenceError{
			Dimension: "execution_agent",
			Detail:    fmt.Sprintf("the producer and the reviewer are both agent %q", producer.ID),
		}
	}
	producerFamily, reviewerFamily := VendorFamilyFor(producer.Kind), VendorFamilyFor(reviewer.Kind)
	// An "unknown" vendor family is not evidence of ANYTHING, including
	// independence: VendorFamilyFor's own doc comment is explicit that two
	// unrecognized vendors must never be read as independent merely because
	// the string "unknown" trivially differs from a real vendor name. Either
	// side being unrecognized means this check cannot confidently establish
	// independence at all, so it fails closed the same way a match does.
	if producerFamily == reviewerFamily || producerFamily == "unknown" || reviewerFamily == "unknown" {
		return &ReviewIndependenceError{
			Dimension: "vendor_family",
			Detail: fmt.Sprintf("producer agent %q (vendor family %q) and reviewer agent %q (vendor family %q) cannot be confirmed independent; "+
				"an unrecognized vendor family is never evidence of independence, and selecting a different agent of the same known vendor/model does not satisfy it either",
				producer.ID, producerFamily, reviewer.ID, reviewerFamily),
		}
	}
	return nil
}
