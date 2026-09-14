package pathpattern

import "testing"

func TestMatchesAny(t *testing.T) {
	for _, tc := range []struct {
		candidate, pattern string
		want               bool
	}{
		{"internal/auth/x", "internal/auth/**", true},
		{"internal/auth", "internal/auth/**", true},
		{"internal/author/x", "internal/auth/**", false},
		{"internal/auth/x", "internal/auth/x", true},
		{"internal/auth/x/y", "internal/auth/x", false},
		{"internal/auth/x", "./internal/auth/**", true},
		{"internal/auth/x", "internal/auth/*", false},
		{"internal/auth/x", "internal/auth/", false},
	} {
		if got := MatchesAny(tc.candidate, []string{tc.pattern}); got != tc.want {
			t.Errorf("%q %q = %v", tc.candidate, tc.pattern, got)
		}
	}
}
