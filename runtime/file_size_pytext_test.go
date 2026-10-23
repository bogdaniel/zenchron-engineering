package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func requirePython(t *testing.T) string {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not available")
	}
	return python
}

// pythonEval runs a snippet under the Python the canonical checker runs under,
// with no inherited git or home configuration, and returns its stdout.
func pythonEval(t *testing.T, python, code, stdin string) string {
	t.Helper()
	command := exec.Command(python, "-I", "-c", code)
	command.Env = []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C.UTF-8", "PYTHONIOENCODING=utf-8"}
	command.Stdin = strings.NewReader(stdin)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		t.Fatalf("python: %v %s", err, stderr.String())
	}
	return string(out)
}

func parseCodePoints(t *testing.T, line string) map[rune]bool {
	t.Helper()
	set := map[rune]bool{}
	for _, field := range strings.Fields(line) {
		value, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("unreadable code point %q: %v", field, err)
		}
		set[rune(value)] = true
	}
	return set
}

// The two character sets the manifest parser depends on are enumerated from the
// Python that runs the canonical checker and compared with the Go predicates over
// every code point, so a Python release or a Go edit that changes either is
// caught here and not by a manifest that reads two ways.
func TestPythonTextSetsMatchTheCanonicalChecker(t *testing.T) {
	python := requirePython(t)
	out := pythonEval(t, python, `
import sys
sep = [c for c in range(0x110000) if not 0xD800 <= c <= 0xDFFF and ("a" + chr(c) + "b").splitlines() != ["a" + chr(c) + "b"]]
space = [c for c in range(0x110000) if not 0xD800 <= c <= 0xDFFF and chr(c).isspace()]
print(" ".join(map(str, sep)))
print(" ".join(map(str, space)))
print(sys.version.split()[0])
`, "")
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected python output: %q", out)
	}
	separators, spaces := parseCodePoints(t, lines[0]), parseCodePoints(t, lines[1])
	for c := rune(0); c <= 0x10FFFF; c++ {
		if c >= 0xD800 && c <= 0xDFFF {
			continue
		}
		if got := isPythonLineSeparator(c); got != separators[c] {
			t.Errorf("python %s: U+%04X splits lines = %v in Python, %v in Go", lines[2], c, separators[c], got)
		}
		if got := isPythonSpace(c); got != spaces[c] {
			t.Errorf("python %s: U+%04X is whitespace = %v in Python, %v in Go", lines[2], c, spaces[c], got)
		}
	}
}

// splitlines itself, not just its separator set: CRLF is one separator, "\n\r"
// is two, consecutive separators give empty lines, and a trailing separator
// makes no extra line.
func TestSplitPythonLinesIsStrSplitlines(t *testing.T) {
	python := requirePython(t)
	corpus := []string{
		"", "a", "a\n", "a\n\n", "\n", "\r\n", "\r\n\r\n", "a\r\nb", "a\n\rb", "a\rb\r", "a b", "a b ",
		"a\u0085b", "a\fb", "a\vb", "a\x1cb\x1dc\x1ed", "a\x1fb", " ", "a\r\n b\nc\r",
	}
	input, err := json.Marshal(corpus)
	if err != nil {
		t.Fatal(err)
	}
	out := pythonEval(t, python, `
import json, sys
print(json.dumps([s.splitlines() for s in json.loads(sys.stdin.read())], ensure_ascii=True))
`, string(input))
	var want [][]string
	if err := json.Unmarshal([]byte(out), &want); err != nil {
		t.Fatalf("unreadable python output %q: %v", out, err)
	}
	for index, text := range corpus {
		got := splitPythonLines(text)
		if got == nil {
			got = []string{}
		}
		if !reflect.DeepEqual(got, want[index]) {
			t.Errorf("splitlines(%q): Go %q, Python %q", text, got, want[index])
		}
	}
}

// The same manifest is given to the real script and to the Go verifier. Where
// Python cuts it into lines differently from "\n" alone, the two must still read
// one policy: refuse the same manifests and accept the same ones.
func TestManifestLineSeparatorsAndWhitespaceReadTheSameAsTheCanonicalChecker(t *testing.T) {
	python := requirePython(t)
	script, err := os.ReadFile(filepath.Join("..", "scripts", "check_file_sizes.py"))
	if err != nil {
		t.Fatal(err)
	}
	const goodBaseline, goodExceptions = "base.go\t1100\n", "exc.go\t1400\treason\n"
	entry := "exc.go\t1400\treason"
	cases := []struct {
		name, baseline, exceptions string
		refused                    bool
	}{
		{"LF entries", goodBaseline, goodExceptions, false},
		{"CRLF entries", "base.go\t1100\r\n", entry + "\r\n", false},
		{"two valid entries split by U+2028", "base.go\t1100 exc.go\t1300\n", goodExceptions, false},
		{"a CR-only comment line, then an entry", goodBaseline, "# note\r" + entry + "\n", false},
		{"a NBSP-indented comment", goodBaseline, " # note\n" + entry + "\n", false},
		{"a unit-separator-indented comment (\\x1f is whitespace)", goodBaseline, "\x1f# note\n" + entry + "\n", false},
		{"a line holding only separators and spaces", goodBaseline, "  \x1f\r\n" + entry + "\n", false},
		{"U+2028 then an invalid entry", goodBaseline, entry + " not an entry\n", true},
		{"CR-only then an invalid entry", goodBaseline, entry + "\rgarbage\n", true},
		{"form feed then an invalid entry", goodBaseline, entry + "\fgarbage\n", true},
		{"vertical tab then an invalid entry", goodBaseline, entry + "\vgarbage\n", true},
		{"U+0085 then an invalid entry", goodBaseline, entry + "\u0085garbage\n", true},
		{"U+2029 then an invalid entry", goodBaseline, entry + " garbage\n", true},
		{"\\x1c then an invalid entry", goodBaseline, entry + "\x1cgarbage\n", true},
		{"\\x1d then an invalid entry", goodBaseline, entry + "\x1dgarbage\n", true},
		{"\\x1e then an invalid entry", goodBaseline, entry + "\x1egarbage\n", true},
		{"U+2028 then a duplicate path", goodBaseline, entry + " exc.go\t5000\tlater\n", true},
		{"a reason made only of a unit separator", goodBaseline, "exc.go\t1400\t\x1f\n", true},
	}
	for _, test := range cases {
		dir := t.TempDir()
		base, tree := sizeRepo(t, dir, map[string]string{
			"scripts/check_file_sizes.py": string(script),
			"file-size-baseline.tsv":      test.baseline,
			"file-size-exceptions.tsv":    test.exceptions,
			"exc.go":                      manyLines(1300),
			"base.go":                     manyLines(1100),
		}, "fixture")
		command := canonicalSizeChecker(t, python, dir)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		pythonRefused := command.Run() != nil && strings.Contains(stderr.String(), "FAIL file-size check")
		if pythonRefused != test.refused {
			t.Errorf("%s: the fixture is wrong, the canonical checker refused=%v want %v (%s)", test.name, pythonRefused, test.refused, stderr.String())
			continue
		}
		violations, goErr := CheckFileSizePolicy(dir, base, tree)
		if test.refused {
			if goErr == nil {
				t.Errorf("%s: the canonical checker refused the manifest and the Go verifier read it as a policy (%+v)", test.name, violations)
			}
			continue
		}
		if goErr != nil || len(violations) != 0 {
			t.Errorf("%s: the canonical checker accepted the manifest and Go refused or flagged it: %v %+v", test.name, goErr, violations)
		}
	}
}
