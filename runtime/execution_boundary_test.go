package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// providerFiles hold the native execution implementations: everything one
// bounded model/tool execution needs, and nothing the host owns.
var providerFiles = []string{
	"cli_agent.go", "agent_specs.go", "claude_stream.go", "openai_provider.go",
	"openai_tool_names.go", "native_codex.go", "transport_chatter.go", "tool_broker.go",
}

// hostFilePatterns are the host files that drive an execution through
// execution.Port and must see a provider only through that port and the named
// capability interfaces.
var hostFilePatterns = []string{
	"scheduler.go", "operations.go", "reconciler.go", "supervisor.go", "controller*.go",
	"planner.go", "handoff_repair.go", "orchestration.go", "workgraph.go", "plan_reconciler.go",
}

// TestExecutionBoundaryHostTouchesNoProviderInternals is #521's R4. A host file
// that named a provider-declared identifier would couple run/attempt ownership
// to one implementation, and an anonymous interface assertion is a capability
// nobody declared: Gate B swaps the implementation behind execution.Port, so
// both must fail here rather than in the swap. Other anonymous assertions in
// host files (the liveness observer in controller.go) are not about the port.
func TestExecutionBoundaryHostTouchesNoProviderInternals(t *testing.T) {
	fset := token.NewFileSet()
	declared := map[string]string{}
	for _, file := range providerFiles {
		for _, name := range topLevelNames(t, parseBoundaryFile(t, fset, file)) {
			declared[name] = file
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no provider-declared identifiers; the enumeration is broken")
	}
	hosts := hostFiles(t)
	for _, file := range hosts {
		parsed := parseBoundaryFile(t, fset, file)
		for _, ident := range packageLevelReferences(parsed) {
			if owner, ok := declared[ident.Name]; ok {
				t.Errorf("%s: host references %s, declared in provider file %s", fset.Position(ident.Pos()), ident.Name, owner)
			}
		}
		ast.Inspect(parsed, func(node ast.Node) bool {
			assertion, ok := node.(*ast.TypeAssertExpr)
			if !ok {
				return true
			}
			_, anonymous := assertion.Type.(*ast.InterfaceType)
			if anonymous && assertsOnProvider(assertion) {
				t.Errorf("%s: anonymous interface assertion on deps.Provider; declare a named capability interface instead", fset.Position(assertion.Pos()))
			}
			return true
		})
	}
}

// assertsOnProvider reports an assertion on a value reached as <x>.Provider,
// which is how every host file holds the execution port (deps.Provider).
func assertsOnProvider(assertion *ast.TypeAssertExpr) bool {
	selector, ok := assertion.X.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "Provider"
}

func parseBoundaryFile(t *testing.T, fset *token.FileSet, file string) *ast.File {
	t.Helper()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func hostFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range hostFilePatterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) == 0 {
			t.Fatalf("host pattern %q matches no file; update the R4 host list", pattern)
		}
		for _, match := range matches {
			if !strings.HasSuffix(match, "_test.go") {
				files = append(files, match)
			}
		}
	}
	sort.Strings(files)
	return files
}

// topLevelNames are a file's package-level functions, types, variables and
// constants. Methods are reached through a value and are judged by the type.
func topLevelNames(t *testing.T, file *ast.File) []string {
	t.Helper()
	var names []string
	for _, decl := range file.Decls {
		switch decl := decl.(type) {
		case *ast.FuncDecl:
			if decl.Recv == nil {
				names = append(names, decl.Name.Name)
			}
		case *ast.GenDecl:
			for _, spec := range decl.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					names = append(names, spec.Name.Name)
				case *ast.ValueSpec:
					for _, name := range spec.Names {
						names = append(names, name.Name)
					}
				}
			}
		}
	}
	return names
}

// packageLevelReferences are the identifiers a file uses that the parser could
// not resolve inside the file, i.e. package-level names from sibling files.
// Selector fields and struct-literal keys are names of members, not
// references.
//
// ponytail: a map literal keyed by a bare provider constant is skipped as if
// it were a struct key; switch to go/types if such a literal ever appears.
func packageLevelReferences(file *ast.File) []*ast.Ident {
	members := map[*ast.Ident]bool{}
	ast.Inspect(file, func(node ast.Node) bool {
		switch node := node.(type) {
		case *ast.SelectorExpr:
			members[node.Sel] = true
		case *ast.KeyValueExpr:
			if key, ok := node.Key.(*ast.Ident); ok {
				members[key] = true
			}
		}
		return true
	})
	var refs []*ast.Ident
	ast.Inspect(file, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if ok && ident.Obj == nil && !members[ident] {
			refs = append(refs, ident)
		}
		return true
	})
	return refs
}

// TestExecutionBoundaryAbsentCapabilitiesAreDeliberate pins the capabilities a
// provider must NOT have, which a compile-time assertion cannot express. The
// brokered API provider's tools are bound to the candidate workspace, so it
// cannot write the runtime-owned result slots; the CLI adapters run commands
// in this process's environment, so the host probes their toolchain itself.
func TestExecutionBoundaryAbsentCapabilitiesAreDeliberate(t *testing.T) {
	if _, ok := any(OpenAIProvider{}).(TypedResultWriter); ok {
		t.Error("OpenAIProvider must not claim the typed result capability")
	}
	for _, provider := range []any{CLIAgentProvider{}, NativeCodexProvider{}} {
		if _, ok := provider.(ToolchainProber); ok {
			t.Errorf("%T must not probe its own toolchain; the host resolves it", provider)
		}
	}
}
