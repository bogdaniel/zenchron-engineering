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

// RecursiveSuffix marks a directory-prefix pattern. A repository file whose
// final segment is literally "**" cannot be told apart from it once it sits in
// a contract scope, so callers must refuse such paths (see IsAmbiguousLiteral).
const RecursiveSuffix = "/**"

// IsAmbiguousLiteral reports a literal repository path that would read as a
// recursive pattern if it were stored in a contract scope.
func IsAmbiguousLiteral(repositoryPath string) bool {
	return strings.HasSuffix(repositoryPath, RecursiveSuffix) || repositoryPath == "**"
}

// MatchesAny compares a canonical repository path with scope entries. An entry
// equal to the path always matches: contract scopes hold literal filenames,
// including ones with glob characters such as app/[id]/page.tsx. Only an
// entry that validates as dir/** matches by prefix, so any other glob spelling
// grants nothing beyond itself. Model boundaries are validated on decode.
func MatchesAny(candidate string, patterns []string) bool {
	candidate = strings.TrimPrefix(candidate, "./")
	if candidate == "" || candidate == "." {
		return false
	}
	for _, raw := range patterns {
		pattern := strings.TrimPrefix(raw, "./")
		if candidate == pattern {
			return true
		}
		if !strings.HasSuffix(pattern, RecursiveSuffix) || Validate(raw) != nil {
			continue
		}
		directory := strings.TrimSuffix(pattern, RecursiveSuffix)
		if candidate == directory || strings.HasPrefix(candidate, directory+"/") {
			return true
		}
	}
	return false
}
