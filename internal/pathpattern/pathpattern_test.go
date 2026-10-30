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
		{"app/[id]/page.tsx", "app/[id]/page.tsx", true},
		{"app/[id]/page.tsx", "./app/[id]/page.tsx", true},
		{"app/[id]/page.tsx", "app/[id]/**", false},
		{"app/[id]/page.tsx", "app/**", true},
		{"app/[x]/page.tsx", "app/[id]/page.tsx", false},
		{"a{b}.go", "a{b}.go", true},
		{"a:b?.go", "a:b?.go", true},
		{"a.go", "*.go", false},
		{"a.go", "?.go", false},
		{"app/[id]/page.tsx", "app/[a-z]/page.tsx", false},
		{"dir/a", "dir/*", false},
		{".", ".", false},
		{"", "", false},
		{"internal/auth", "internal/auth/**", true},
	} {
		if got := MatchesAny(tc.candidate, []string{tc.pattern}); got != tc.want {
			t.Errorf("%q %q = %v", tc.candidate, tc.pattern, got)
		}
	}
}

func TestIsAmbiguousLiteral(t *testing.T) {
	for path, want := range map[string]bool{
		"dir/**": true, "**": true, "a/b/**": true,
		"dir/*": false, "dir/**x": false, "dir/x**": false, "app/[id]/page.tsx": false,
	} {
		if got := IsAmbiguousLiteral(path); got != want {
			t.Errorf("%q = %v", path, got)
		}
	}
}
