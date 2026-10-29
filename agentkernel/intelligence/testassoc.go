package intelligence

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

var testPrefixes = []string{"Test", "Benchmark", "Fuzz", "Example"}

// linkTests associates test functions with symbols of the package they test,
// by name and by static call. Both are inferred: a test that names or calls a
// symbol may still assert nothing about it.
func linkTests(dirs []DirFacts) []TestAssociation {
	targets := map[string]Symbol{} // ID -> non-test symbol
	for _, df := range dirs {
		for _, sym := range df.Symbols {
			if !strings.HasSuffix(sym.Evidence.File, "_test.go") {
				targets[sym.ID] = sym
			}
		}
	}
	var out []TestAssociation
	seen := map[string]bool{}
	add := func(test Symbol, target string, m Method) {
		if _, ok := targets[target]; !ok || seen[test.ID+"\x00"+target+"\x00"+string(m)] {
			return
		}
		seen[test.ID+"\x00"+target+"\x00"+string(m)] = true
		out = append(out, TestAssociation{Test: test.ID, Target: target, Limit: CoverageUnknown,
			Evidence: Evidence{File: test.Evidence.File, Line: test.Evidence.Line, Method: m, Confidence: ConfidenceInferred}})
	}
	for _, df := range dirs {
		tests := map[string]Symbol{}
		for _, sym := range df.Symbols {
			subject, ok := testSubject(sym)
			if !ok {
				continue
			}
			tests[sym.ID] = sym
			pkg := strings.TrimSuffix(sym.Package, "_test")
			add(sym, pkg+"."+subject, MethodNameHeuristic)
			add(sym, pkg+"."+lowerFirst(subject), MethodNameHeuristic)
			if recv, method, ok := strings.Cut(subject, "_"); ok {
				add(sym, pkg+"."+recv+"."+method, MethodNameHeuristic)
				add(sym, pkg+"."+recv, MethodNameHeuristic)
			}
		}
		for _, c := range df.Calls {
			test, ok := tests[c.Caller]
			if !ok || c.Class != CallStatic {
				continue
			}
			if targets[c.Callee].Package == strings.TrimSuffix(test.Package, "_test") {
				add(test, c.Callee, MethodTestCall)
			}
		}
	}
	slices.SortFunc(out, func(a, b TestAssociation) int {
		return strings.Compare(a.Test+"\x00"+a.Target+"\x00"+string(a.Evidence.Method),
			b.Test+"\x00"+b.Target+"\x00"+string(b.Evidence.Method))
	})
	return out
}

// testSubject returns "Xxx" for TestXxx, BenchmarkXxx, FuzzXxx and ExampleXxx
// functions in _test.go files, following the go test naming rule that the
// character after the prefix is not lower case. TestMain is the test binary's
// entry point, not a test of anything named Main.
func testSubject(sym Symbol) (string, bool) {
	if sym.Kind != KindFunc || sym.Name == "TestMain" || !strings.HasSuffix(sym.Evidence.File, "_test.go") {
		return "", false
	}
	for _, p := range testPrefixes {
		rest, ok := strings.CutPrefix(sym.Name, p)
		if !ok {
			continue
		}
		r, _ := utf8.DecodeRuneInString(rest)
		if rest == "" || unicode.IsLower(r) {
			return "", false
		}
		return strings.TrimPrefix(rest, "_"), rest != "_"
	}
	return "", false
}

// lowerFirst lets TestParseConfig find the unexported parseConfig.
func lowerFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToLower(r)) + s[n:]
}
