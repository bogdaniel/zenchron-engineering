package runtime

import (
	"strings"
	"testing"
)

func TestConfigDigestDifference(t *testing.T) {
	a, b := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for _, tt := range []struct {
		name                               string
		local, governing                   ConfigDigest
		member, localShort, governingShort string
	}{
		{"equal", ConfigDigest{a, b}, ConfigDigest{a, b}, "", "", ""},
		{"global first", ConfigDigest{a, b}, ConfigDigest{b, a}, "global", a[:12], b[:12]},
		{"repository", ConfigDigest{a, a}, ConfigDigest{a, b}, "repository", a[:12], b[:12]},
		{"same prefix", ConfigDigest{a, ""}, ConfigDigest{a[:63] + "b", ""}, "global", a[:12], a[:12]},
		{"absent", ConfigDigest{}, ConfigDigest{Repository: b}, "repository", "", b[:12]},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, l, g := ConfigDigestDifference(tt.local, tt.governing)
			if m != tt.member || l != tt.localShort || g != tt.governingShort {
				t.Fatalf("got %q %q %q", m, l, g)
			}
		})
	}
}
