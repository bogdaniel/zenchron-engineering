package benchmark

import "errors"

// Measure is a value that may legitimately be unknown. Unknown carries its
// reason and a nil Value: it is never rendered as zero and never estimated.
type Measure[T any] struct {
	Value         *T     `json:"value"`
	UnknownReason string `json:"unknown_reason,omitempty"`
}

// Known states a measured value.
func Known[T any](v T) Measure[T] { return Measure[T]{Value: &v} }

// Unknown states that a value could not be measured, and why.
func Unknown[T any](reason string) Measure[T] { return Measure[T]{UnknownReason: reason} }

// IsKnown reports whether a value was measured.
func (m Measure[T]) IsKnown() bool { return m.Value != nil }

// Validate refuses a Measure that is both or neither: a value with a reason, or
// an unknown with no reason.
func (m Measure[T]) Validate() error {
	if m.Value != nil && m.UnknownReason != "" {
		return errors.New("a measure cannot be both known and unknown")
	}
	if m.Value == nil && m.UnknownReason == "" {
		return errors.New("an unknown measure must state its reason")
	}
	return nil
}
