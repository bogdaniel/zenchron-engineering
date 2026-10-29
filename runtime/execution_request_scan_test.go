package runtime

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// The acceptance-H guard answers a structural Go question - which composite
// literals build the host execution request, and does each one key Attempt -
// so it reads the syntax tree, not the text. Comments and strings are not
// code, an import alias is still the same package, and every element of a
// slice or map of requests is its own literal.

const hostModulePath = "github.com/bogdaniel/zenchron-engineering"

// hostRequestTypes names the host execution request under every spelling:
// runtime.ExecutionRequest is an alias of execution.Request since #521. A
// same-named type of another package (agentkernel's api.ExecutionRequest,
// built by the Gate B adapter) is a different type and is not a producer.
var hostRequestTypes = map[string]string{
	hostModulePath + "/runtime":   "ExecutionRequest",
	hostModulePath + "/execution": "Request",
}

// executionRequestOffenders scans the module rooted at root for host request
// literals that omit the scheduler attempt, and counts the literals checked.
// Nested modules (#511), .git and fixtures are skipped.
func executionRequestOffenders(root string) ([]string, int, error) {
	var offenders []string
	checked := 0
	err := filepath.WalkDir(root, func(file string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return skipScanDir(root, file, entry.Name())
		}
		if !strings.HasSuffix(entry.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		found, n, err := executionRequestFileOffenders(rel, src, path.Join(hostModulePath, path.Dir(rel)))
		offenders = append(offenders, found...)
		checked += n
		return err
	})
	return offenders, checked, err
}

func skipScanDir(root, dir, name string) error {
	if name == ".git" || name == "fixtures" {
		return filepath.SkipDir
	}
	if dir == root {
		return nil
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
		return filepath.SkipDir
	}
	return nil
}

// executionRequestFileOffenders checks one file whose directory has import
// path pkgPath. Build tags are not consulted: every file is parsed.
func executionRequestFileOffenders(name string, src []byte, pkgPath string) ([]string, int, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, 0, fmt.Errorf("parse %s: %w", name, err)
	}
	scan := requestScan{isHost: hostRequestMatcher(file, pkgPath)}
	ast.Inspect(file, func(n ast.Node) bool {
		if lit, ok := n.(*ast.CompositeLit); ok && lit.Type != nil {
			scan.visit(lit, lit.Type)
		}
		return true
	})
	var offenders []string
	for _, found := range scan.offenders {
		pos := fset.Position(found.lit.Pos())
		offenders = append(offenders, fmt.Sprintf("%s:%d: %s%s",
			name, pos.Line, sourceFirstLine(src, pos.Offset), found.why))
	}
	return offenders, scan.checked, nil
}

type requestOffender struct {
	lit *ast.CompositeLit
	why string
}

type requestScan struct {
	isHost    func(ast.Expr) bool
	checked   int
	offenders []requestOffender
}

// visit checks lit as a literal of type typ. A container of requests has its
// elided element literals checked one by one: in []T{{A}, {Attempt: 2}} the
// second element's Attempt says nothing about the first. Typed inner literals
// are reached by ast.Inspect itself.
func (s *requestScan) visit(lit *ast.CompositeLit, typ ast.Expr) {
	if s.isHost(typ) {
		s.check(lit)
		return
	}
	elem := containerElement(typ)
	if elem == nil {
		return
	}
	for _, element := range lit.Elts {
		if kv, ok := element.(*ast.KeyValueExpr); ok {
			element = kv.Value
		}
		if inner, ok := element.(*ast.CompositeLit); ok && inner.Type == nil {
			s.visit(inner, elem)
		}
	}
}

func (s *requestScan) check(lit *ast.CompositeLit) {
	s.checked++
	// A zero value states nothing and cannot omit a field.
	if len(lit.Elts) == 0 {
		return
	}
	if _, keyed := lit.Elts[0].(*ast.KeyValueExpr); !keyed {
		s.offenders = append(s.offenders, requestOffender{lit, " (unkeyed literal)"})
		return
	}
	for _, element := range lit.Elts {
		kv := element.(*ast.KeyValueExpr) // a struct literal is all keyed or all not
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Attempt" {
			return
		}
	}
	s.offenders = append(s.offenders, requestOffender{lit, ""})
}

// containerElement is the element type of a slice, array or map type (the map
// value), with a pointer stripped; nil for anything else.
func containerElement(typ ast.Expr) ast.Expr {
	var elem ast.Expr
	switch t := typ.(type) {
	case *ast.ArrayType:
		elem = t.Elt
	case *ast.MapType:
		elem = t.Value
	default:
		return nil
	}
	if star, ok := elem.(*ast.StarExpr); ok {
		return star.X
	}
	return elem
}

// hostRequestMatcher resolves type expressions through the file's own imports.
// An unqualified name matches inside the target package itself (not its
// external _test package) or through a dot-import of it.
func hostRequestMatcher(file *ast.File, pkgPath string) func(ast.Expr) bool {
	qualified := map[string]string{} // local import name -> type name
	unqualified := map[string]bool{}
	if name, ok := hostRequestTypes[pkgPath]; ok && !strings.HasSuffix(file.Name.Name, "_test") {
		unqualified[name] = true
	}
	for _, spec := range file.Imports {
		importPath := strings.Trim(spec.Path.Value, `"`)
		typeName, ok := hostRequestTypes[importPath]
		if !ok {
			continue
		}
		local := path.Base(importPath)
		if spec.Name != nil {
			local = spec.Name.Name
		}
		switch local {
		case "_":
		case ".":
			unqualified[typeName] = true
		default:
			qualified[local] = typeName
		}
	}
	return func(typ ast.Expr) bool {
		switch t := typ.(type) {
		case *ast.Ident:
			return unqualified[t.Name]
		case *ast.SelectorExpr:
			pkg, ok := t.X.(*ast.Ident)
			return ok && qualified[pkg.Name] == t.Sel.Name
		}
		return false
	}
}

func sourceFirstLine(src []byte, offset int) string {
	line := string(src[offset:])
	if at := strings.IndexByte(line, '\n'); at >= 0 {
		line = line[:at]
	}
	return strings.TrimSpace(line)
}

// TestExecutionRequestScanSkipsNestedModules: a directory with its own go.mod
// is a separate module. It cannot construct this module's request without
// requiring it, so a same-named type there is not a producer.
func TestExecutionRequestScanSkipsNestedModules(t *testing.T) {
	root := t.TempDir()
	offending := "package p\n\nimport \"" + hostModulePath + "/execution\"\n\nvar _ = execution.Request{RunID: \"r\"}\n"
	files := map[string]string{
		"go.mod":        "module " + hostModulePath + "\n",
		"a.go":          offending,
		"nested/go.mod": "module example.com/nested\n",
		"nested/b.go":   offending,
	}
	for name, content := range files {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	offenders, _, err := executionRequestOffenders(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(offenders) != 1 || !strings.HasPrefix(offenders[0], "a.go:5: ") {
		t.Fatalf("offenders = %q, want only the root module's a.go", offenders)
	}
}

// TestExecutionRequestScanRecognizesTheHostTypeStructurally: each case is a
// whole file, parsed by the same per-file check the repository scan uses.
func TestExecutionRequestScanRecognizesTheHostTypeStructurally(t *testing.T) {
	const runtimePkg, executionPkg = hostModulePath + "/runtime", hostModulePath + "/execution"
	const importRuntime = `import "` + runtimePkg + `"` + "\n"
	const importExecution = `import "` + executionPkg + `"` + "\n"
	cases := []struct {
		name, pkgPath, source string
		want                  []int // offending lines
	}{
		{"unqualified in runtime", runtimePkg, "package runtime\nvar _ = ExecutionRequest{RunID: \"a\"}\n", []int{2}},
		{"keyed Attempt", runtimePkg, "package runtime\nvar _ = ExecutionRequest{RunID: \"a\", Attempt: 1}\n", nil},
		{"zero value", runtimePkg, "package runtime\nvar _ = ExecutionRequest{}\n", nil},
		{"unkeyed literal", runtimePkg, "package runtime\nvar _ = ExecutionRequest{\"a\"}\n", []int{2}},
		{"masked slice element", runtimePkg,
			"package runtime\nvar _ = []ExecutionRequest{\n{RunID: \"a\"},\n{RunID: \"b\", Attempt: 2},\n}\n", []int{3}},
		{"map value", runtimePkg, "package runtime\nvar _ = map[string]ExecutionRequest{\"k\": {RunID: \"a\"}}\n", []int{2}},
		{"pointer slice elided", runtimePkg, "package runtime\nvar _ = []*ExecutionRequest{{RunID: \"a\"}}\n", []int{2}},
		{"nested slices", runtimePkg, "package runtime\nvar _ = [][]ExecutionRequest{{{RunID: \"a\"}}}\n", []int{2}},
		{"address of qualified", "example.com/x", "package x\n" + importRuntime +
			"var _ = &runtime.ExecutionRequest{RunID: \"a\"}\n", []int{3}},
		{"external test package imports runtime", runtimePkg, "package runtime_test\n" + importRuntime +
			"var _ = runtime.ExecutionRequest{RunID: \"a\"}\nvar _ = ExecutionRequest{RunID: \"b\"}\n", []int{3}},
		{"import alias", "example.com/x", "package x\nimport rt \"" + runtimePkg + "\"\n" +
			"var _ = rt.ExecutionRequest{RunID: \"x\"}\n", []int{3}},
		{"dot import", "example.com/x", "package x\nimport . \"" + executionPkg + "\"\n" +
			"var _ = Request{RunID: \"x\"}\n", []int{3}},
		{"execution.Request", "example.com/x", "package x\n" + importExecution +
			"var _ = execution.Request{RunID: \"x\"}\n", []int{3}},
		{"execution.Request with Attempt", "example.com/x", "package x\n" + importExecution +
			"var _ = execution.Request{RunID: \"x\", Attempt: 1}\n", nil},
		{"unqualified in execution", executionPkg, "package execution\nvar _ = Request{RunID: \"x\"}\n", []int{2}},
		{"api.ExecutionRequest", "example.com/x", "package x\nimport \"" + hostModulePath + "/agentkernel/api\"\n" +
			"var _ = api.ExecutionRequest{Version: \"v\"}\n", nil},
		{"same-named type elsewhere", "example.com/x", "package x\ntype ExecutionRequest struct{ RunID string }\n" +
			"var _ = ExecutionRequest{RunID: \"x\"}\n", nil},
		{"unimported qualifier", "example.com/x", "package x\nvar _ = runtime.ExecutionRequest{RunID: \"x\"}\n", nil},
		{"string and comment", runtimePkg, "package runtime\n// ExecutionRequest{RunID: \"c\"}\n" +
			"var _ = \"ExecutionRequest{RunID: \\\"s\\\"}\"\n", nil},
		{"braces in a string field", runtimePkg, "package runtime\nvar _ = []ExecutionRequest{\n" +
			"{RunID: \"}{\", Attempt: 1},\n{RunID: \"{\"},\n}\n", []int{4}},
	}
	for _, tc := range cases {
		found, _, err := executionRequestFileOffenders("f.go", []byte(tc.source), tc.pkgPath)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		var lines []int
		for _, offender := range found {
			var line int
			if _, err := fmt.Sscanf(offender, "f.go:%d:", &line); err != nil {
				t.Fatalf("%s: malformed offender %q", tc.name, offender)
			}
			lines = append(lines, line)
		}
		if fmt.Sprint(lines) != fmt.Sprint(tc.want) {
			t.Errorf("%s: offending lines %v, want %v (%q)", tc.name, lines, tc.want, found)
		}
	}
}

// TestExecutionRequestScanRefusesUnparseableFiles: a file the scan cannot read
// is a file it cannot vouch for, so it fails rather than skipping silently.
func TestExecutionRequestScanRefusesUnparseableFiles(t *testing.T) {
	_, _, err := executionRequestFileOffenders("broken.go", []byte("package x\nvar _ = {"), "example.com/x")
	if err == nil || !strings.Contains(err.Error(), "broken.go") {
		t.Fatalf("err = %v, want a parse error naming broken.go", err)
	}
}
