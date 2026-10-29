package intelligence_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
)

const circleGo = `package shapes

// Circle is added by the overlay.
type Circle struct{ R float64 }

// Area implements Shape.
func (c Circle) Area() float64 { return 3 * c.R * c.R }
`

func readFixture(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "basic", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// overlayScenario applies changes as an overlay of the base, then checks the
// base is untouched and the overlay equals a fresh build of the changed tree.
// It returns the overlay for scenario-specific assertions.
func overlayScenario(t *testing.T, settings *intelligence.Settings, changes ...intelligence.Change) (*intelligence.Index, *intelligence.Index) {
	t.Helper()
	root := workspace(t, "basic")
	base := build(t, root, linux, intelligence.Scope{})
	before := fullDigest(t, base)
	ov, err := base.Overlay(context.Background(), intelligence.OverlaySpec{Root: root, Changes: changes, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	if fullDigest(t, base) != before {
		t.Fatal("overlay mutated the base snapshot")
	}
	want := linux
	if settings != nil {
		want = *settings
	}
	fresh := build(t, workspace(t, "basic", changes...), want, intelligence.Scope{})
	if snapshotDigest(t, ov) != snapshotDigest(t, fresh) {
		t.Fatalf("overlay facts differ from a fresh build of the same tree\noverlay stats %+v", ov.Stats())
	}
	if ov.Key() != fresh.Key() || ov.Key() == base.Key() {
		t.Fatal("overlay identity must equal the fresh identity and differ from the base")
	}
	if b := ov.Snapshot().Overlay; b == nil || b.BaseKey != base.Key() {
		t.Fatalf("overlay binding = %+v, want base key %s", b, base.Key())
	}
	return base, ov
}

func TestOverlayEditRefreshesDependents(t *testing.T) {
	edited := strings.Replace(readFixture(t, "shapes/shapes.go"), "func NewSquare(", "func MakeSquare(", 1)
	edited = strings.Replace(edited, "NewSquare(1)", "MakeSquare(1)", 1)
	_, ov := overlayScenario(t, nil, intelligence.Change{Path: "shapes/shapes.go", Content: []byte(edited)})
	if _, ok := symbols(ov)["example.com/basic/shapes.NewSquare"]; ok {
		t.Fatal("edited-away symbol survived")
	}
	c, ok := findCall(calls(ov), "example.com/basic/report.Report", "NewSquare")
	if !ok || c.Class != intelligence.CallUnresolved {
		t.Fatalf("importer's call to the removed function = %+v, want unresolved", c)
	}
	if st := ov.Stats(); st.DirsReused != 1 || st.DirsExtracted != 2 {
		t.Fatalf("stats %+v: want broken reused, shapes and its importer report re-extracted", st)
	}
}

func TestOverlayAddFile(t *testing.T) {
	_, ov := overlayScenario(t, nil, intelligence.Change{Path: "shapes/circle.go", Content: []byte(circleGo)})
	if _, ok := symbols(ov)["example.com/basic/shapes.Circle.Area"]; !ok {
		t.Fatal("added method missing")
	}
}

func TestOverlayDeleteRemovesRelations(t *testing.T) {
	base, ov := overlayScenario(t, nil, intelligence.Change{Path: "shapes/reflect.go", Delete: true})
	if !mentionsFile(t, base, "shapes/reflect.go") {
		t.Fatal("fixture precondition: base has reflect.go facts")
	}
	if mentionsFile(t, ov, "shapes/reflect.go") {
		t.Fatal("deleted file still has relations")
	}
	if _, ok := symbols(ov)["example.com/basic/shapes.CallByName"]; ok {
		t.Fatal("symbol of a deleted file survived")
	}
}

func TestOverlayRenameMovesProvenance(t *testing.T) {
	content := []byte(readFixture(t, "shapes/reflect.go"))
	_, ov := overlayScenario(t, nil, intelligence.Change{Path: "shapes/dyn.go", RenamedFrom: "shapes/reflect.go", Content: content})
	if mentionsFile(t, ov, "shapes/reflect.go") {
		t.Fatal("renamed-away path still has relations")
	}
	if s := symbols(ov)["example.com/basic/shapes.CallByName"]; s.Evidence.File != "shapes/dyn.go" {
		t.Fatalf("renamed symbol evidence = %+v, want shapes/dyn.go", s.Evidence)
	}
}

func TestOverlayGoModChangeInvalidatesModule(t *testing.T) {
	gomod := []byte("module example.com/renamed\n\ngo 1.25\n")
	_, ov := overlayScenario(t, nil, intelligence.Change{Path: "go.mod", Content: gomod})
	if st := ov.Stats(); st.DirsReused != 0 {
		t.Fatalf("go.mod change reused %d directories", st.DirsReused)
	}
	if b := ov.Snapshot().Overlay; b.Invalidation != "module" {
		t.Fatalf("invalidation = %q, want module", b.Invalidation)
	}
	if _, ok := symbols(ov)["example.com/renamed/broken.Good"]; !ok {
		t.Fatal("module path change not applied to unaffected-looking directories")
	}
}

func TestOverlayBuildTagChangeInvalidatesEverything(t *testing.T) {
	tagged := linux
	tagged.BuildTags = []string{"extra"}
	_, ov := overlayScenario(t, &tagged)
	syms := symbols(ov)
	if _, ok := syms["example.com/basic/report.Extra"]; !ok {
		t.Fatal("tag-gated file not extracted after the tag was enabled")
	}
	if b := ov.Snapshot().Overlay; b.Invalidation != "all" || ov.Stats().DirsReused != 0 {
		t.Fatalf("binding %+v stats %+v: want full invalidation", b, ov.Stats())
	}

	darwin := linux
	darwin.GOOS = "darwin"
	_, ov = overlayScenario(t, &darwin)
	if _, ok := symbols(ov)["example.com/basic/report.LinuxOnly"]; ok {
		t.Fatal("GOOS change kept a linux-only symbol")
	}
}

func TestOverlayToolchainChangeInvalidates(t *testing.T) {
	newer := linux
	newer.GoVersion = "go1.26.0"
	_, ov := overlayScenario(t, &newer)
	if _, ok := symbols(ov)["example.com/basic/report.Newer"]; !ok {
		t.Fatal("go1.26-gated file not extracted for a go1.26 toolchain")
	}
}

func TestOverlayRefusesUndeclaredWorkspaceDrift(t *testing.T) {
	root := workspace(t, "basic")
	base := build(t, root, linux, intelligence.Scope{})
	if err := writeFile(root, "shapes/reflect.go", []byte("package shapes\n")); err != nil {
		t.Fatal(err)
	}
	edited := readFixture(t, "shapes/shapes.go") + "\nfunc Extra2() {}\n"
	_, err := base.Overlay(context.Background(), intelligence.OverlaySpec{Root: root,
		Changes: []intelligence.Change{{Path: "shapes/shapes.go", Content: []byte(edited)}}})
	if !errors.Is(err, intelligence.ErrStaleWorkspace) {
		t.Fatalf("overlay over drifted bytes: err = %v, want ErrStaleWorkspace", err)
	}
}

func TestOverlayRefusesInvalidChanges(t *testing.T) {
	root := workspace(t, "basic")
	base := build(t, root, linux, intelligence.Scope{})
	for _, cs := range [][]intelligence.Change{
		{{Path: "../escape.go", Content: []byte("x")}},
		{{Path: "/abs.go", Content: []byte("x")}},
		{{Path: "shapes/nope.go", Delete: true}},
		{{Path: "shapes/a.go", RenamedFrom: "shapes/nope.go"}},
		{{Path: "shapes/shapes.go", Delete: true, Content: []byte("x")}},
		{{Path: "shapes/x.go", Content: []byte("a")}, {Path: "shapes/x.go", Content: []byte("b")}},
	} {
		if _, err := base.Overlay(context.Background(), intelligence.OverlaySpec{Root: root, Changes: cs}); err == nil {
			t.Errorf("changes %+v accepted", cs)
		}
	}
}

// TestConcurrentOverlaysAreIsolated runs overlays of one base in parallel; each
// must see only its own change and the base must stay byte-identical.
func TestConcurrentOverlaysAreIsolated(t *testing.T) {
	root := workspace(t, "basic")
	base := build(t, root, linux, intelligence.Scope{})
	before := fullDigest(t, base)
	const n = 8
	results := make([]*intelligence.Index, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// report imports shapes: every overlay type-checks against the
			// base's shared, read-only shapes package types.
			src := fmt.Sprintf("package report\n\nimport \"example.com/basic/shapes\"\n\n"+
				"func Only%d() float64 { return shapes.NewSquare(%d).Area() }\n", i, i)
			results[i], errs[i] = base.Overlay(context.Background(), intelligence.OverlaySpec{Root: root,
				Changes: []intelligence.Change{{Path: "report/only.go", Content: []byte(src)}}})
		}()
	}
	wg.Wait()
	for i, ov := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if ov.Stats().DirsReused != 2 {
			t.Fatalf("overlay %d stats %+v: want shapes and broken reused", i, ov.Stats())
		}
		if c, ok := findCall(calls(ov), fmt.Sprintf("example.com/basic/report.Only%d", i), "Square.Area"); !ok || c.Class != intelligence.CallStatic {
			t.Fatalf("overlay %d: call through reused base types = %+v", i, c)
		}
		syms := symbols(ov)
		for j := range n {
			_, has := syms[fmt.Sprintf("example.com/basic/report.Only%d", j)]
			if has != (i == j) {
				t.Fatalf("overlay %d sees Only%d = %t", i, j, has)
			}
		}
	}
	if fullDigest(t, base) != before {
		t.Fatal("concurrent overlays mutated the base")
	}
}
