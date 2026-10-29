package intelligence_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
)

func TestBuildExtractsDeclarationsWithProvenance(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	syms := symbols(ix)
	for id, kind := range map[string]string{
		"example.com/basic/shapes.Shape":           intelligence.KindType,
		"example.com/basic/shapes.Square.Area":     intelligence.KindMethod,
		"example.com/basic/shapes.Total":           intelligence.KindFunc,
		"example.com/basic/report.LinuxOnly":       intelligence.KindFunc,
		"example.com/basic/report_test.TestReport": intelligence.KindFunc,
	} {
		s, ok := syms[id]
		if !ok || s.Kind != kind || s.Evidence.File == "" || s.Evidence.Line == 0 {
			t.Errorf("symbol %s = %+v, want kind %s with file/line evidence", id, s, kind)
		}
	}
	for _, absent := range []string{"example.com/basic/report.Extra", "example.com/basic/report.Newer", "example.com/basic/report.CgoThing"} {
		if _, ok := syms[absent]; ok {
			t.Errorf("%s is excluded by settings but was extracted", absent)
		}
	}
}

func TestCallEdgesSeparateStaticFromPossibleDynamicUnresolved(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	cs := calls(ix)
	const p = "example.com/basic/shapes."
	cases := []struct {
		caller, callee string
		class          intelligence.CallClass
		conf           intelligence.Confidence
	}{
		{p + "usesHelper", p + "helper", intelligence.CallStatic, intelligence.ConfidenceDeterministic},
		{p + "usesHelper", p + "Square.Side2", intelligence.CallStatic, intelligence.ConfidenceDeterministic},
		{"example.com/basic/report.Report", p + "Describe", intelligence.CallStatic, intelligence.ConfidenceDeterministic},
		{p + "Total", p + "Shape.Area", intelligence.CallPossible, intelligence.ConfidenceInferred},
		{p + "Apply", "f", intelligence.CallDynamic, intelligence.ConfidenceUnresolved},
		{p + "Describe", "fmt.Sprintf", intelligence.CallUnresolved, intelligence.ConfidenceUnresolved},
		{p + "CallByName", "reflect.ValueOf", intelligence.CallDynamic, intelligence.ConfidenceUnresolved},
	}
	for _, c := range cases {
		got, ok := findCall(cs, c.caller, c.callee)
		if !ok || got.Class != c.class || got.Evidence.Confidence != c.conf || got.Evidence.Line == 0 {
			t.Errorf("call %s -> %s = %+v (found %t), want %s/%s", c.caller, c.callee, got, ok, c.class, c.conf)
		}
	}
	for _, c := range cs {
		if c.Class != intelligence.CallStatic && c.Evidence.Confidence == intelligence.ConfidenceDeterministic {
			t.Errorf("non-static call labelled deterministic: %+v", c)
		}
	}
}

func TestIncompleteAnalysisIsRecorded(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	kinds := map[string]bool{}
	for _, df := range ix.Snapshot().Dirs {
		for _, in := range df.Incomplete {
			kinds[in.Kind+" "+in.Path] = true
		}
	}
	for _, want := range []string{
		"parse_error broken/broken.go",
		"build_excluded report/tagged.go",
		"build_excluded report/newer.go",
		"build_excluded report/cgo.go",
		"type_errors example.com/basic/shapes",
	} {
		if !kinds[want] {
			t.Errorf("missing incomplete record %q in %v", want, kinds)
		}
	}
	if _, ok := symbols(ix)["example.com/basic/broken.Good"]; !ok {
		t.Error("partially parsed file lost its parseable declarations")
	}
}

func TestQueriesNeverClaimCompletenessTheyLack(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	r := ix.Callers("example.com/basic/shapes.Square.Area")
	if r.Complete || len(r.Limits) == 0 {
		t.Fatalf("callers of an interface-implementing method reported complete: %+v", r)
	}
	if u := ix.Callers("example.com/basic/shapes.nosuch"); u.Complete {
		t.Fatal("unknown symbol reported complete")
	}

	pure := build(t, workspace(t, "pure"), linux, intelligence.Scope{})
	if r := pure.Callers("example.com/pure/a.leaf"); !r.Complete || len(r.Calls) != 1 {
		t.Fatalf("fully static module: callers of leaf = %+v, want one call, complete", r)
	}
	dyn := []byte("package a\n\nfunc Dyn(f func() int) int { return f() }\n")
	ov, err := pure.Overlay(context.Background(), intelligence.OverlaySpec{
		Root: workspace(t, "pure"), Changes: []intelligence.Change{{Path: "a/dyn.go", Content: dyn}}})
	if err != nil {
		t.Fatal(err)
	}
	if r := ov.Callers("example.com/pure/a.leaf"); r.Complete {
		t.Fatalf("a dynamic call in scope must make callers incomplete: %+v", r)
	}
}

func TestSuggestedTestsAreInferredAndIncomplete(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	r := ix.SuggestedTests("example.com/basic/shapes.Total")
	methods := map[intelligence.Method]bool{}
	for _, a := range r.Tests {
		if a.Test == "example.com/basic/shapes.TestTotal" {
			methods[a.Evidence.Method] = true
		}
	}
	if !methods[intelligence.MethodNameHeuristic] || !methods[intelligence.MethodTestCall] {
		t.Fatalf("TestTotal associations = %+v, want name and static-call evidence", r.Tests)
	}
	if r.Complete || !slices.Contains(r.Limits, intelligence.CoverageUnknown) {
		t.Fatalf("suggested tests must be incomplete with coverage unknown: %+v", r)
	}
	if got := ix.SuggestedTests("example.com/basic/shapes.Square.Area").Tests; len(got) == 0 {
		t.Error("TestSquare_Area not associated with Square.Area")
	}
	if ext := ix.SuggestedTests("example.com/basic/report.Report").Tests; len(ext) == 0 {
		t.Error("external test package not associated with the package under test")
	}
	for _, a := range ix.Snapshot().Tests {
		if a.Evidence.Confidence != intelligence.ConfidenceInferred || a.Limit != intelligence.CoverageUnknown {
			t.Fatalf("test association not marked inferred with coverage limit: %+v", a)
		}
	}
	if none := ix.SuggestedTests("example.com/basic/shapes.Apply"); len(none.Tests) != 0 || none.Complete {
		t.Fatalf("no association must stay incomplete, not 'no tests': %+v", none)
	}
}

func TestBoundedExtractionReadsOnlyRequestedPackages(t *testing.T) {
	root := workspace(t, "basic")
	full := build(t, root, linux, intelligence.Scope{})
	bounded := build(t, root, linux, intelligence.Scope{Packages: []string{"report"}})
	st := bounded.Stats()
	if st.FilesHashed >= full.Stats().FilesHashed || st.DirsExtracted != 1 {
		t.Fatalf("bounded stats %+v vs full %+v: want fewer files and one directory", st, full.Stats())
	}
	if mentionsFile(t, bounded, "shapes/shapes.go") {
		t.Fatal("bounded index contains facts from an unrequested package")
	}
	c, ok := findCall(calls(bounded), "example.com/basic/report.Report", "shapes.Describe")
	if !ok || c.Class != intelligence.CallUnresolved || !strings.Contains(c.Reason, intelligence.ImportModuleNotIndexed) {
		t.Fatalf("call into an unindexed module package = %+v, want unresolved %s", c, intelligence.ImportModuleNotIndexed)
	}
	if r := bounded.Callers("example.com/basic/report.Report"); r.Complete {
		t.Fatal("bounded scope must not report complete callers")
	}
	if full.Key() == bounded.Key() {
		t.Fatal("scope must be part of the snapshot identity")
	}
}

func TestManifestIsDeterministicAndSkipsGitAndSymlinks(t *testing.T) {
	root := workspace(t, "pure")
	if err := writeFile(root, ".git/HEAD", []byte("ref")); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(root, "notes/readme.md", []byte("x")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a", "link.go")); err != nil {
		t.Fatal(err)
	}
	a := build(t, root, linux, intelligence.Scope{Exclude: []string{"*.md"}})
	b := build(t, root, linux, intelligence.Scope{Exclude: []string{"*.md"}})
	if a.Key() != b.Key() || fullDigest(t, a) != fullDigest(t, b) {
		t.Fatal("identical input produced different snapshots")
	}
	m := a.Snapshot().Manifest
	var paths []string
	for _, f := range m.Files {
		paths = append(paths, f.Path)
	}
	if want := []string{"a/a.go", "go.mod"}; !slices.Equal(paths, want) {
		t.Fatalf("manifest paths = %v, want %v", paths, want)
	}
	if !slices.Equal(m.Skipped, []string{"a/link.go"}) {
		t.Fatalf("symlink must be recorded as skipped, got %v", m.Skipped)
	}
}

func TestBuildRefusesInvalidSettings(t *testing.T) {
	root := workspace(t, "pure")
	for _, s := range []intelligence.Settings{
		{GOOS: "linux", GOARCH: "amd64"},
		{GoVersion: "1.25", GOOS: "linux", GOARCH: "amd64"},
		{GoVersion: "go1.25.0", GOARCH: "amd64"},
		{GoVersion: "go1.25.0", GOOS: "linux", GOARCH: "amd64", BuildTags: []string{"a b"}},
	} {
		if _, err := intelligence.Build(context.Background(), intelligence.BuildConfig{Root: root, Settings: s}); err == nil {
			t.Errorf("settings %+v accepted", s)
		}
	}
	if _, err := intelligence.Build(context.Background(), intelligence.BuildConfig{Root: root, Settings: linux,
		Scope: intelligence.Scope{Packages: []string{"../a"}}}); err == nil {
		t.Error("escaping package scope accepted")
	}
}
