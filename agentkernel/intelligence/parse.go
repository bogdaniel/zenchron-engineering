package intelligence

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"io"
	"path"
	"strings"
)

// parsedDir is one package directory after build-constraint evaluation and
// parsing, split the way the go tool splits it.
type parsedDir struct {
	dir   string
	name  string
	files []File
	main  []*ast.File // non-test files of the package
	itest []*ast.File // _test.go files in the same package
	xtest []*ast.File // _test.go files in package <name>_test
	// incomplete is per-file analysis loss found while parsing.
	incomplete []Incomplete
}

type parsedFile struct {
	path string
	ast  *ast.File
}

func (s *session) parseDir(dir string) (*parsedDir, error) {
	if pd, ok := s.parsed[dir]; ok {
		return pd, nil
	}
	pd := &parsedDir{dir: dir}
	ctxt := s.settings.buildContext(s.openFile)
	var kept []parsedFile
	for _, name := range s.goDirs[dir] {
		pf, err := s.parseFile(pd, ctxt.MatchFile, dir, name)
		if err != nil {
			return nil, err
		}
		if pf != nil {
			kept = append(kept, *pf)
		}
	}
	pd.split(kept)
	s.parsed[dir] = pd
	return pd, nil
}

// parseFile returns nil when the file is not part of the build. Only a
// workspace read failure is an error; everything else is recorded analysis
// loss.
func (s *session) parseFile(pd *parsedDir, match func(string, string) (bool, error), dir, name string) (*parsedFile, error) {
	p := path.Join(dir, name)
	file := File{Path: p, Digest: s.entries[p].Digest, Test: strings.HasSuffix(name, "_test.go")}
	ok, err := match(dir, name)
	var rerr *readError
	if errors.As(err, &rerr) {
		return nil, rerr.err
	}
	if err != nil || !ok {
		file.Reason = "excluded by build constraints"
		if err != nil {
			file.Reason = "invalid build constraint: " + err.Error()
		}
		pd.files = append(pd.files, file)
		pd.incomplete = append(pd.incomplete, Incomplete{Kind: IncompleteBuildExcluded, Path: p, Detail: file.Reason})
		return nil, nil
	}
	content, err := s.read(p)
	if err != nil {
		return nil, err
	}
	s.stats.FilesParsed++
	f, perr := parser.ParseFile(s.fset, p, content, parser.SkipObjectResolution)
	if perr != nil {
		pd.incomplete = append(pd.incomplete, Incomplete{Kind: IncompleteParseError, Path: p, Detail: firstLine(perr.Error())})
	}
	if f == nil {
		file.Reason = "unparseable"
		pd.files = append(pd.files, file)
		return nil, nil
	}
	if importsC(f) {
		if !s.settings.CgoEnabled {
			file.Reason = "cgo disabled in settings"
			pd.files = append(pd.files, file)
			pd.incomplete = append(pd.incomplete, Incomplete{Kind: IncompleteBuildExcluded, Path: p, Detail: file.Reason})
			return nil, nil
		}
		pd.incomplete = append(pd.incomplete, Incomplete{Kind: IncompleteCgo, Path: p, Detail: "C.* references are not analysed"})
	}
	file.Included = true
	pd.files = append(pd.files, file)
	return &parsedFile{path: p, ast: f}, nil
}

// split assigns files to the package, its in-package tests and its external
// test package. The package name comes from the first non-test file, as the go
// tool does; a file naming any other package is excluded and recorded.
func (pd *parsedDir) split(kept []parsedFile) {
	for _, pf := range kept {
		if !strings.HasSuffix(pf.path, "_test.go") {
			pd.name = pf.ast.Name.Name
			break
		}
	}
	if pd.name == "" && len(kept) > 0 {
		pd.name = strings.TrimSuffix(kept[0].ast.Name.Name, "_test")
	}
	for _, pf := range kept {
		name, test := pf.ast.Name.Name, strings.HasSuffix(pf.path, "_test.go")
		switch {
		case name == pd.name && !test:
			pd.main = append(pd.main, pf.ast)
		case name == pd.name:
			pd.itest = append(pd.itest, pf.ast)
		case test && name == pd.name+"_test":
			pd.xtest = append(pd.xtest, pf.ast)
		default:
			pd.exclude(pf.path, fmt.Sprintf("package %s, expected %s", name, pd.name))
		}
	}
}

func (pd *parsedDir) exclude(p, reason string) {
	for i := range pd.files {
		if pd.files[i].Path == p {
			pd.files[i].Included = false
			pd.files[i].Reason = reason
		}
	}
	pd.incomplete = append(pd.incomplete, Incomplete{Kind: IncompletePackageName, Path: p, Detail: reason})
}

func importsC(f *ast.File) bool {
	for _, spec := range f.Imports {
		if spec.Path.Value == `"C"` {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// readError marks a workspace read failure crossing go/build's OpenFile hook,
// so it is not mistaken for a malformed build constraint.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }

func (s *session) openFile(p string) (io.ReadCloser, error) {
	b, err := s.read(p)
	if err != nil {
		return nil, &readError{err: err}
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}
