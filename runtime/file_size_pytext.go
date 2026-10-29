package runtime

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// The canonical checker reads a manifest with Python's text semantics:
// read_text(...).splitlines() to cut it into lines, then str.strip() and
// str.lstrip() to judge a line blank or a comment and a field empty. The Go
// parser must cut and judge the same text the same way, or one manifest can be
// two different policies: an entry containing U+2028 followed by a malformed
// entry is two lines to Python and one valid entry with a longer reason to a
// parser that splits on "\n" alone, and a CR-only file is several lines to
// Python and one to Go.
//
// Both sets below are enumerated from Python itself and pinned by
// TestPythonTextSetsMatchTheCanonicalChecker, so the two cannot drift.

// isPythonLineSeparator is the set str.splitlines() splits on:
// \n, \v, \f, \r, \x1c, \x1d, \x1e, \x85, U+2028 and U+2029. (\x1f is
// whitespace but NOT a line separator.) A "\r\n" pair is one separator.
func isPythonLineSeparator(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// splitPythonLines is str.splitlines(): separators are not kept, a "\r\n" pair is
// one separator, consecutive separators give empty lines, and text after the
// last separator is a line only if it is not empty. The text must be valid UTF-8.
func splitPythonLines(text string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(text); {
		r, size := utf8.DecodeRuneInString(text[i:])
		if !isPythonLineSeparator(r) {
			i += size
			continue
		}
		lines = append(lines, text[start:i])
		i += size
		if r == '\r' && i < len(text) && text[i] == '\n' {
			i++
		}
		start = i
	}
	if start < len(text) {
		lines = append(lines, text[start:])
	}
	return lines
}

// isPythonSpace is what str.isspace(), and so strip() and lstrip(), treat as
// whitespace. unicode.IsSpace lacks \x1c-\x1f, which Python counts.
func isPythonSpace(r rune) bool { return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f) }

func trimPythonSpace(text string) string { return strings.TrimFunc(text, isPythonSpace) }

func trimLeftPythonSpace(text string) string { return strings.TrimLeftFunc(text, isPythonSpace) }
