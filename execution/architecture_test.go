package execution_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/bogdaniel/zenchron-engineering"

// hostPackages are the governance and host packages an execution adapter must
// never reach (R2): an adapter that could import them could read or write the
// authority, evidence and scheduling state the host alone owns.
var hostPackages = []string{"runtime", "controlplane", "authority", "evidence", "planning", "orchestration"}

// importEdge is one import of one file, keyed by the importing package's
// repository-relative directory.
type importEdge struct {
	dir, file, path string
}

// moduleImports parses every Go file of THIS module - test files included, so
// a test cannot smuggle an edge the build would refuse - and skips nested
// modules (agentkernel, fixtures), which have their own go.mod.
func moduleImports(t *testing.T) []importEdge {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	var edges []importEdge
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path == root {
				return nil
			}
			if name == "testdata" || name == "vendor" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, spec := range parsed.Imports {
			imported, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			edges = append(edges, importEdge{dir: filepath.ToSlash(rel), file: path, path: imported})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) == 0 {
		t.Fatal("no imports found: the module walk is broken, so every rule below would pass vacuously")
	}
	return edges
}

func isStandardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func within(path, pkg string) bool {
	return path == pkg || strings.HasPrefix(path, pkg+"/")
}

// R1: the port is a leaf. It may name domain facts and nothing else of this
// repository, so no host or governance concept can leak into the seam.
func TestExecutionImportsOnlyStandardLibraryAndDomain(t *testing.T) {
	for _, edge := range moduleImports(t) {
		// This external test package importing execution itself is not an edge out.
		if edge.dir != "execution" || isStandardLibrary(edge.path) || edge.path == module+"/domain" || edge.path == module+"/execution" {
			continue
		}
		t.Errorf("R1: %s imports %q: package execution may import only the standard library and domain", edge.file, edge.path)
	}
}

// R2: an execution adapter is one bounded execution, not a host. It reaches
// none of the packages that own runs, authority, evidence or plans.
func TestExecutionAdaptersImportNoHostPackage(t *testing.T) {
	for _, edge := range moduleImports(t) {
		if !strings.HasPrefix(edge.dir, "execution/") {
			continue
		}
		for _, host := range hostPackages {
			if within(edge.path, module+"/"+host) {
				t.Errorf("R2: %s imports %q: an execution adapter may not import %s", edge.file, edge.path, host)
			}
		}
	}
}

// R3: adapters are chosen at the composition root and nowhere else, and the
// Agent Execution Kernel module is reached by exactly one package of this
// one, its host adapter execution/agentkernel (#518, ADR-0006), and only
// through the kernel's public packages.
func TestOnlyTheCompositionRootImportsAdapters(t *testing.T) {
	for _, edge := range moduleImports(t) {
		if within(edge.path, module+"/agentkernel") &&
			(edge.dir != "execution/agentkernel" || within(edge.path, module+"/agentkernel/internal")) {
			t.Errorf("R3: %s imports %q: only execution/agentkernel may import the agent kernel's public packages", edge.file, edge.path)
		}
		rel, inModule := strings.CutPrefix(edge.path, module+"/")
		if !inModule || !strings.HasPrefix(rel, "execution/") {
			continue
		}
		// An adapter may import its own subpackages; nothing else but cmd/ may.
		name, _, _ := strings.Cut(strings.TrimPrefix(rel, "execution/"), "/")
		if !within(edge.dir, "cmd") && !within(edge.dir, "execution/"+name) {
			t.Errorf("R3: %s imports adapter %q: only cmd/... may import execution adapters", edge.file, edge.path)
		}
	}
}
