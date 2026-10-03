package domain

import (
	"fmt"
	"github.com/bogdaniel/zenchron-engineering/internal/pathpattern"
)

// ValidateBoundaryPatterns rejects unsupported patterns before boundary facts
// can be derived, including for models constructed directly in Go.
func ValidateBoundaryPatterns(model ProjectModel) error {
	if model.CriticalBoundaries == nil {
		return nil
	}
	for id, boundary := range *model.CriticalBoundaries {
		for _, pattern := range boundary.Paths {
			if err := pathpattern.Validate(pattern); err != nil {
				return fmt.Errorf("critical boundary %q: %w", id, err)
			}
		}
	}
	return nil
}
