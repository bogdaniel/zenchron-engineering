// Package pathpattern implements exact and directory-recursive repository patterns.
package pathpattern

import (
	"fmt"
	"path"
	"strings"
)

// UnsupportedPatternError identifies a pattern whose meaning cannot be evaluated.
type UnsupportedPatternError struct{ Pattern string }

func (e *UnsupportedPatternError) Error() string {
	return fmt.Sprintf("unsupported repository path pattern %q: expected exact path or dir/**", e.Pattern)
}

// Validate accepts canonical repository-relative paths, optionally prefixed by
// ./ and optionally suffixed by /**. Other glob syntax is unsupported.
func Validate(pattern string) error {
	base := strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/**")
	if base == "" || base == "." || path.IsAbs(base) || path.Clean(base) != base || strings.ContainsAny(base, "\\\x00*?[]{}:") {
		return &UnsupportedPatternError{Pattern: pattern}
	}
	for _, segment := range strings.Split(base, "/") {
		if segment == ".." {
			return &UnsupportedPatternError{Pattern: pattern}
		}
	}
	return nil
}

// MatchesAny compares a canonical repository path with exact or dir/** patterns.
// Invalid patterns never grant scope; model boundaries must be validated first.
func MatchesAny(candidate string, patterns []string) bool {
	candidate = strings.TrimPrefix(candidate, "./")
	for _, pattern := range patterns {
		if Validate(pattern) != nil {
			continue
		}
		pattern = strings.TrimPrefix(pattern, "./")
		if candidate == pattern {
			return true
		}
		if strings.HasSuffix(pattern, "/**") {
			directory := strings.TrimSuffix(pattern, "/**")
			if candidate == directory || strings.HasPrefix(candidate, directory+"/") {
				return true
			}
		}
	}
	return false
}
