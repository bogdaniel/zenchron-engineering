package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// subjectContentCommands are the Git subcommands that read committed content
// or ancestry. rawSubjectHelpers read it on behalf of their caller, from the
// directory they are handed.
var (
	subjectContentCommands = map[string]bool{
		"diff": true, "diff-tree": true, "merge-base": true, "cat-file": true,
		"ls-tree": true, "log": true, "rev-list": true, "show": true, "archive": true,
	}
	rawSubjectHelpers = map[string]bool{
		"diffPaths": true, "treeCommitPaths": true, "guardStagedContent": true, "readBlobs": true,
	}
)

// subjectContentReaders are the declarations allowed to read content or
// ancestry from a directory not named `store`, each with its reason.
var subjectContentReaders = map[string]string{
	"runtime/git.go:treeCommitPaths":            "helper; every caller is checked here",
	"runtime/git.go:guardStagedContent":         "helper; every caller is checked here",
	"runtime/git.go:readBlobs":                  "helper; every caller is checked here",
	"runtime/quarantine.go:restoreRefusedPaths": "pre-commit: returns refused paths in the producer's own workspace to HEAD",
	// The file-size check reads the verifier's ASSURANCE CHECKOUT, never the
	// candidate workspace: assuranceCheckout cloned it from the verified subject
	// store, and fileSizeVerdict proves its HEAD commit and tree are the exact
	// recorded ones (verifyAssuranceCheckout) before any of these run. These
	// entries are NOT permission for future callers to read an unverified
	// workspace: a new reader must read from subjectStore, or prove its directory
	// to be the exact recorded commit and tree as fileSizeVerdict does.
	"runtime/file_size_policy.go:manifestAtBase":       "reads the proven assurance checkout, which was cloned from the subject store",
	"runtime/file_size_policy.go:readFileSizeManifest": "reads the proven assurance checkout, which was cloned from the subject store",
	"runtime/file_size_policy.go:CheckFileSizePolicy":  "reads the proven assurance checkout, which was cloned from the subject store",
	"runtime/file_size_policy.go:countedBlobs":         "reads the proven assurance checkout, which was cloned from the subject store",
	// The browser step reads the same proven checkout: withBrowserAcceptance
	// calls verifyAssuranceCheckout (exact commit and tree) before either reader
	// runs, and re-proves the tree after the container.
	"runtime/browser_acceptance.go:readBrowserSelection":         "reads the base manifest from the proven assurance checkout, which was cloned from the subject store",
	"runtime/browser_acceptance_workspace.go:browserTreeEntries": "lists the exact tree and the base test surface in the proven assurance checkout, which was cloned from the subject store",
	"runtime/browser_acceptance_workspace.go:materializeEntries": "reads each blob of the composed tree from the proven assurance checkout and verifies it against its object id",
}

// TestPostCommitReadersUseTheSubjectStore is #437's search guard: in runtime
// production code, a Git call that reads committed content or ancestry - a
// content subcommand passed as a literal, or a raw reading helper - must run in
// a directory named `store` (the value subjectStore returned), or sit in a
// declaration listed above. A new reader that hands Git the candidate
// workspace fails here. The guard reads names, not data flow: it catches the
// accidental regression, not a deliberate misnaming.
func TestPostCommitReadersUseTheSubjectStore(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			key := "runtime/" + path + ":" + fn.Name.Name
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				callee, ok := call.Fun.(*ast.Ident)
				if !ok || !readsSubjectContent(callee.Name, call.Args) {
					return true
				}
				if dir, ok := call.Args[0].(*ast.Ident); ok && dir.Name == "store" {
					return true
				}
				if _, allowed := subjectContentReaders[key]; allowed {
					seen[key] = true
					return true
				}
				t.Errorf("%s (%s) reads committed content or ancestry outside the subject store; read it from subjectStore (#437)", fset.Position(call.Pos()), key)
				return true
			})
		}
	}
	for key := range subjectContentReaders {
		if !seen[key] {
			t.Errorf("%s is allowlisted but no longer reads content outside the store; remove it", key)
		}
	}
}

func readsSubjectContent(callee string, args []ast.Expr) bool {
	if rawSubjectHelpers[callee] {
		return true
	}
	at := map[string]int{"runGit": 1, "gitOutput": 1, "runGitInput": 2}[callee]
	if at == 0 || len(args) <= at {
		return false
	}
	lit, ok := args[at].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	sub, err := strconv.Unquote(lit.Value)
	return err == nil && subjectContentCommands[sub]
}
