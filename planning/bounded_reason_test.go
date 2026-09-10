package planning

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// A readiness detail is cut on a rune boundary.
//
// Same defect as the runtime's stored fields and tool results: a byte offset
// through a multi-byte character produces invalid UTF-8, and the JSON encoding
// of a replacement character is larger than the character it replaced.
func TestABoundedReasonNeverSplitsARune(t *testing.T) {
	reason := boundedReason(strings.Repeat("a", 199) + strings.Repeat("\u0103", 4))
	if len(reason) > 200 {
		t.Fatalf("a bounded reason is %d bytes, above the 200-byte bound", len(reason))
	}
	if !utf8.ValidString(reason) {
		t.Fatalf("a bounded reason is not valid UTF-8: %q", reason)
	}
}
