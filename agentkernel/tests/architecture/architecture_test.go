// Package architecture checks the Agent Execution Kernel's dependency law at
// the source level, independently of build tags, GOOS or _test suffixes:
// every .go file in the module is parsed, including files the toolchain
// would never compile (//go:build ignore, other platforms). The toolchain
// view of the same law is scripts/check-imports.sh.
package architecture

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// hostRepository is the repository that hosts this module. No package of it
// other than this module may be imported.
const hostRepository = "github.com/bogdaniel/zenchron-engineering"

// forbiddenImports are escape hatches out of the module's type safety or
// dependency closure.
var forbiddenImports = map[string]string{
	"unsafe": "unsafe bridges are forbidden",
	"plugin": "runtime plugin loading is forbidden",
	"C":      "cgo is forbidden",
}

// parentEntries are top-level entries of the host checkout. A relative path
// literal that climbs out of a package and lands on one of these names is a
// parent-checkout reference even when the target happens not to escape the
// module directory lexically (for example "../runtime" written in engine/).
var parentEntries = []string{
	"runtime", "domain", "internal", "policy", "authority", "evidence", "analysis", "planning",
	"controlplane", "orchestration", "reassessment", "benchmarks", "fixtures", "cmd", "docs",
	"schemas", "scripts", "go.mod", "go.sum", "go.work", "AGENTS.md", "README.md", "ROADMAP.md",
}

// sharedDatabase is the host's runtime database file name, assembled so this
// file does not itself contain the literal it forbids.
var sharedDatabase = "runtime" + ".db"

type goFile struct {
	rel  string // slash path relative to the module root
	pkg  string // module-relative package directory ("." for the root)
	test bool
	ast  *ast.File
	fset *token.FileSet
}

type module struct {
	root  string
	path  string
	files []goFile
}

func loadModule(t *testing.T) module {
	t.Helper()
	root := moduleRoot(t)
	m := module{root: root, path: modulePath(t, root)}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && nestedModule(p) {
				return filepath.SkipDir // a fixture module with its own go.mod
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments)
		if err != nil {
			return fmt.Errorf("parse %s: %w", p, err)
		}
		rel := slashRel(root, p)
		m.files = append(m.files, goFile{
			rel: rel, pkg: pathDir(rel), test: strings.HasSuffix(p, "_test.go"), ast: f, fset: fset,
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.files) == 0 {
		t.Fatal("no Go files found; the walk is vacuous")
	}
	return m
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

func modulePath(t *testing.T, root string) string {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "module "); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	t.Fatal("go.mod has no module line")
	return ""
}

func nestedModule(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "go.mod"))
	return err == nil
}

func slashRel(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		panic(err)
	}
	return filepath.ToSlash(rel)
}

func pathDir(rel string) string {
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[:i]
	}
	return "."
}

func importPath(spec *ast.ImportSpec) string {
	p, err := strconv.Unquote(spec.Path.Value)
	if err != nil {
		panic(err)
	}
	return p
}

// TestImportsStayInsideModuleAndStdlib is the source-level dependency law.
func TestImportsStayInsideModuleAndStdlib(t *testing.T) {
	m := loadModule(t)
	for _, f := range m.files {
		for _, spec := range f.ast.Imports {
			p := importPath(spec)
			pos := f.fset.Position(spec.Pos())
			if why, bad := forbiddenImports[p]; bad {
				t.Errorf("%s:%d: import %q: %s", f.rel, pos.Line, p, why)
				continue
			}
			if p == m.path || strings.HasPrefix(p, m.path+"/") {
				continue
			}
			if p == hostRepository || strings.HasPrefix(p, hostRepository+"/") {
				t.Errorf("%s:%d: import %q: parent repository package outside %s", f.rel, pos.Line, p, m.path)
				continue
			}
			if first, _, _ := strings.Cut(p, "/"); strings.Contains(first, ".") {
				t.Errorf("%s:%d: import %q: non-stdlib dependency", f.rel, pos.Line, p)
			}
		}
	}
}

// TestNoLinknameDirectives refuses //go:linkname in any comment position.
func TestNoLinknameDirectives(t *testing.T) {
	m := loadModule(t)
	for _, f := range m.files {
		for _, group := range f.ast.Comments {
			for _, c := range group.List {
				if strings.HasPrefix(c.Text, "//go:linkname") {
					t.Errorf("%s:%d: //go:linkname directive", f.rel, f.fset.Position(c.Pos()).Line)
				}
			}
		}
	}
}

var requireOrReplace = regexp.MustCompile(`(?m)^\s*(require|replace|tool)\b`)

// TestModuleMetadataIsSelfContained refuses dependencies and workspaces.
func TestModuleMetadataIsSelfContained(t *testing.T) {
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if loc := requireOrReplace.FindIndex(data); loc != nil {
		t.Errorf("go.mod declares %q; Gate A admits no module dependency", strings.TrimSpace(string(data[loc[0]:loc[1]])))
	}
	for _, name := range []string{"go.work", "go.work.sum", "go.sum"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err == nil {
			t.Errorf("%s exists in the module directory", name)
		}
	}
}

// TestNoSymlinkEscapesModule refuses symlinks that resolve outside the module
// or do not resolve at all.
func TestNoSymlinkEscapesModule(t *testing.T) {
	root := moduleRoot(t)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.Type()&fs.ModeSymlink == 0 {
			return err
		}
		target, rerr := filepath.EvalSymlinks(p)
		if rerr != nil {
			t.Errorf("%s: unresolvable symlink: %v", slashRel(root, p), rerr)
			return nil
		}
		if target != realRoot && !strings.HasPrefix(target, realRoot+string(filepath.Separator)) {
			t.Errorf("%s: symlink escapes the module to %s", slashRel(root, p), target)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestNoParentCheckoutPathLiterals refuses string literals that reach into
// the host checkout. testdata fixtures are excluded. The rule is narrow to
// stay free of false positives: a relative literal (or an all-literal
// filepath.Join/path.Join) is flagged only when, resolved against the file's
// directory, it escapes the module or climbs onto a host top-level entry
// that the module does not itself contain; the shared runtime database name
// is flagged anywhere.
func TestNoParentCheckoutPathLiterals(t *testing.T) {
	m := loadModule(t)
	for _, f := range m.files {
		if slices.Contains(strings.Split(f.rel, "/"), "testdata") {
			continue
		}
		ast.Inspect(f.ast, func(n ast.Node) bool {
			var lit string
			switch x := n.(type) {
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				lit, _ = strconv.Unquote(x.Value)
			case *ast.CallExpr:
				joined, ok := literalJoin(x)
				if !ok {
					return true
				}
				lit = joined
			default:
				return true
			}
			if why := m.parentReference(f.pkg, lit); why != "" {
				t.Errorf("%s:%d: string %q %s", f.rel, f.fset.Position(n.Pos()).Line, lit, why)
			}
			return true
		})
	}
}

// literalJoin returns the joined value of filepath.Join/path.Join whose
// arguments are all string literals.
func literalJoin(call *ast.CallExpr) (string, bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Join" || len(call.Args) < 2 {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || (pkg.Name != "filepath" && pkg.Name != "path") {
		return "", false
	}
	parts := make([]string, 0, len(call.Args))
	for _, a := range call.Args {
		lit, ok := a.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(lit.Value)
		if err != nil {
			return "", false
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "/"), true
}

func (m module) parentReference(pkg, lit string) string {
	if strings.Contains(lit, sharedDatabase) {
		return "names the host runtime database"
	}
	if lit != ".." && !strings.HasPrefix(lit, "../") {
		return ""
	}
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join(filepath.FromSlash(pkg), filepath.FromSlash(lit))))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "climbs out of the module directory"
	}
	first, _, _ := strings.Cut(resolved, "/")
	if !slices.Contains(parentEntries, first) {
		return ""
	}
	if _, err := os.Lstat(filepath.Join(m.root, first)); err == nil {
		return "" // the module's own entry of that name (for example schemas/)
	}
	return fmt.Sprintf("climbs onto host checkout entry %q", first)
}

// allowedDeps is the production package dependency map between module
// packages (module-relative directories). A package absent from this map
// fails until it is reviewed and added. cmd/, examples/ and tests/ may import
// any module package. Test files may additionally import testSupport.
var allowedDeps = map[string][]string{
	"internal/strictjson":     {},
	"api":                     {"internal/strictjson"},
	"context":                 {"api"},
	"routing":                 {"api"},
	"tools":                   {"api", "internal/strictjson"},
	"storage":                 {"api"},
	"memory":                  {"api", "internal/strictjson", "storage"},
	"intelligence":            {"api", "internal/strictjson", "storage"},
	"engine":                  {"api", "context", "routing", "tools"},
	"providers/internal/wire": {"api"},
	"providers/conformance":   {"api"},
	"providers/scripted":      {"api"},
	"providers/openai":        {"api", "providers/internal/wire"},
	"providers/anthropic":     {"api", "providers/internal/wire"},
	"providers/local":         {"api", "internal/strictjson", "providers/internal/wire"},
}

var unrestrictedRoots = []string{"cmd", "examples", "tests"}

var testSupport = []string{"api", "context", "storage", "tools", "providers/scripted", "providers/conformance"}

func unrestricted(pkg string) bool {
	first, _, _ := strings.Cut(pkg, "/")
	return slices.Contains(unrestrictedRoots, first)
}

type edge struct{ from, to string }

// moduleGraph returns production and test edges between module packages.
func (m module) moduleGraph() (prod, test map[edge][]string) {
	prod, test = map[edge][]string{}, map[edge][]string{}
	for _, f := range m.files {
		for _, spec := range f.ast.Imports {
			p := importPath(spec)
			if !strings.HasPrefix(p, m.path+"/") {
				continue
			}
			e := edge{from: f.pkg, to: strings.TrimPrefix(p, m.path+"/")}
			if e.from == e.to {
				continue // external test package of the same directory
			}
			target := prod
			if f.test {
				target = test
			}
			target[e] = append(target[e], f.rel)
		}
	}
	return prod, test
}

// TestPackageDependencyMap fails on any module-internal import edge outside
// the allowed map, and prints the actual graph under -v (edge list and a
// Mermaid flowchart) for docs/architecture.md.
func TestPackageDependencyMap(t *testing.T) {
	m := loadModule(t)
	prod, test := m.moduleGraph()
	for _, f := range m.files {
		if _, known := allowedDeps[f.pkg]; !known && !unrestricted(f.pkg) && !f.test {
			t.Errorf("%s: package %q is not in the reviewed dependency map", f.rel, f.pkg)
		}
	}
	for e, files := range prod {
		if !unrestricted(e.from) && !slices.Contains(allowedDeps[e.from], e.to) {
			t.Errorf("%s -> %s not allowed (production import in %s)", e.from, e.to, strings.Join(files, ", "))
		}
	}
	for e, files := range test {
		if unrestricted(e.from) || slices.Contains(allowedDeps[e.from], e.to) || slices.Contains(testSupport, e.to) {
			continue
		}
		t.Errorf("%s -> %s not allowed (test import in %s)", e.from, e.to, strings.Join(files, ", "))
	}
	t.Log("\n" + renderGraph(prod))
}

func renderGraph(prod map[edge][]string) string {
	edges := make([]edge, 0, len(prod))
	for e := range prod {
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].from != edges[j].from {
			return edges[i].from < edges[j].from
		}
		return edges[i].to < edges[j].to
	})
	var sb strings.Builder
	sb.WriteString("production package dependencies (module-relative):\n")
	for _, e := range edges {
		fmt.Fprintf(&sb, "  %s -> %s\n", e.from, e.to)
	}
	sb.WriteString("\n```mermaid\nflowchart TD\n")
	for _, e := range edges {
		fmt.Fprintf(&sb, "  %s --> %s\n", mermaidID(e.from), mermaidID(e.to))
	}
	sb.WriteString("```\n")
	return sb.String()
}

func mermaidID(pkg string) string {
	return strings.NewReplacer("/", "_", ".", "_", "-", "_").Replace(pkg) + `["` + pkg + `"]`
}
