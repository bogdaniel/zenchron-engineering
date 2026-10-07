package intelligence

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ErrStaleWorkspace reports a workspace file whose bytes no longer match the
// manifest the snapshot is being built from. Extraction stops rather than
// analysing content the snapshot identity does not describe.
var ErrStaleWorkspace = errors.New("intelligence: workspace file does not match the manifest")

// session is one extraction run: a base build or an overlay refresh. It is
// single-goroutine; the only state it shares is the read-only base it reuses.
type session struct {
	ctx      context.Context
	root     string
	settings Settings
	scope    Scope
	manifest Manifest
	entries  map[string]FileEntry
	contents map[string][]byte // verified or overlay-supplied bytes
	fset     *token.FileSet
	stats    *Stats

	modules    []Module
	incomplete []Incomplete // index-level: modules
	goDirs     map[string][]string
	dirOf      map[string]string // import path -> dir
	pathOf     map[string]string // dir -> import path

	// reuse holds base facts for directories the overlay proved unaffected;
	// baseTypes may serve their package types to importers.
	reuse     map[string]DirFacts
	baseTypes map[string]*types.Package

	parsed   map[string]*parsedDir
	checked  map[string]*checkedPkg
	checking map[string]bool
}

type checkedPkg struct {
	pkg    *types.Package
	info   *types.Info
	errors []string
}

func newSession(ctx context.Context, root string, settings Settings, scope Scope, m Manifest, contents map[string][]byte, stats *Stats) *session {
	entries := make(map[string]FileEntry, len(m.Files))
	for _, f := range m.Files {
		entries[f.Path] = f
	}
	return &session{
		ctx: ctx, root: root, settings: settings, scope: scope, manifest: m, entries: entries,
		contents: contents, fset: token.NewFileSet(), stats: stats,
		reuse: map[string]DirFacts{}, parsed: map[string]*parsedDir{},
		checked: map[string]*checkedPkg{}, checking: map[string]bool{},
	}
}

// read returns manifest-verified bytes. Anything not already held is read
// from the root and must hash to the manifest digest.
func (s *session) read(p string) ([]byte, error) {
	if b, ok := s.contents[p]; ok {
		return b, nil
	}
	entry, ok := s.entries[p]
	if !ok {
		return nil, fmt.Errorf("intelligence: %s is not in the manifest", p)
	}
	b, err := os.ReadFile(filepath.Join(s.root, filepath.FromSlash(p)))
	if err != nil {
		return nil, fmt.Errorf("intelligence: read %s: %w", p, err)
	}
	s.stats.FilesRead++
	if api.Digest(b) != entry.Digest {
		return nil, fmt.Errorf("%w: %s", ErrStaleWorkspace, p)
	}
	s.contents[p] = b
	return b, nil
}

// prepare discovers modules and package directories from the manifest.
func (s *session) prepare() error {
	for _, f := range s.manifest.Files {
		if path.Base(f.Path) != "go.mod" {
			continue
		}
		b, err := s.read(f.Path)
		if err != nil {
			return err
		}
		modPath, goDir, perr := parseGoMod(b)
		if perr != nil {
			s.incomplete = append(s.incomplete, Incomplete{Kind: IncompleteBadGoMod, Path: f.Path, Detail: perr.Error()})
			continue
		}
		s.modules = append(s.modules, Module{Path: modPath, Dir: path.Dir(f.Path), GoModDigest: f.Digest, GoDirective: goDir})
	}
	s.goDirs = goDirs(s.manifest)
	s.dirOf, s.pathOf = map[string]string{}, map[string]string{}
	for _, dir := range sortedKeys(s.goDirs) {
		ip, inModule := importPathFor(s.modules, dir)
		if !inModule {
			s.incomplete = append(s.incomplete, Incomplete{Kind: IncompleteNoModule, Path: dir,
				Detail: "no go.mod governs this directory; its import path is a placeholder"})
		}
		s.dirOf[ip], s.pathOf[dir] = dir, ip
	}
	return nil
}

// extract produces facts for every package directory, reusing proven-
// unaffected base facts.
func (s *session) extract() ([]DirFacts, error) {
	var out []DirFacts
	for _, dir := range sortedKeys(s.goDirs) {
		if err := s.ctx.Err(); err != nil {
			return nil, err
		}
		if df, ok := s.reuse[dir]; ok {
			s.stats.DirsReused++
			out = append(out, df)
			continue
		}
		df, err := s.extractDir(dir)
		if err != nil {
			return nil, err
		}
		s.stats.DirsExtracted++
		out = append(out, df)
	}
	return out, nil
}

// resolveImport serves indexed packages to go/types and an empty, complete package
// for everything else. Selectors into an empty package do not resolve, which
// is how calls into unindexed code become explicit unresolved edges instead of
// network or GOROOT lookups.
func (s *session) resolveImport(ip string) (*types.Package, error) {
	dir, indexed := s.dirOf[ip]
	if !indexed {
		return emptyPackage(ip), nil
	}
	if _, reused := s.reuse[dir]; reused && s.baseTypes[ip] != nil {
		return s.baseTypes[ip], nil
	}
	if s.checking[ip] {
		s.incomplete = append(s.incomplete, Incomplete{Kind: IncompleteImportCycle, Path: ip})
		return emptyPackage(ip), nil
	}
	c, err := s.checkMain(dir)
	if err != nil {
		return nil, err
	}
	return c.pkg, nil
}

func emptyPackage(ip string) *types.Package {
	name := path.Base(ip)
	if len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = path.Base(path.Dir(ip))
	}
	pkg := types.NewPackage(ip, strings.TrimSuffix(name, path.Ext(name)))
	pkg.MarkComplete()
	return pkg
}

// checkMain type-checks the non-test files of the package in dir, once.
func (s *session) checkMain(dir string) (*checkedPkg, error) {
	ip := s.pathOf[dir]
	if c, ok := s.checked[ip]; ok {
		return c, nil
	}
	pd, err := s.parseDir(dir)
	if err != nil {
		return nil, err
	}
	s.checking[ip] = true
	c, err := s.check(ip, pd.name, pd.main)
	delete(s.checking, ip)
	if err != nil {
		return nil, err
	}
	s.checked[ip] = c
	return c, nil
}

func (s *session) check(ip, name string, files []*ast.File) (*checkedPkg, error) {
	c := &checkedPkg{info: &types.Info{
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
	}}
	if len(files) == 0 {
		c.pkg = types.NewPackage(ip, name)
		c.pkg.MarkComplete()
		return c, nil
	}
	var fatal error
	conf := types.Config{
		Importer:    importerFunc(func(p string) (*types.Package, error) { return s.importOrFail(p, &fatal) }),
		FakeImportC: true,
		Error:       func(err error) { c.errors = append(c.errors, firstLine(err.Error())) },
	}
	s.stats.PackagesChecked++
	c.pkg, _ = conf.Check(ip, s.fset, files, c.info) // errors arrive through conf.Error
	if fatal != nil {
		return nil, fatal
	}
	return c, nil
}

// importOrFail keeps a workspace read failure from being absorbed by go/types
// as an ordinary "could not import" type error.
func (s *session) importOrFail(p string, fatal *error) (*types.Package, error) {
	pkg, err := s.resolveImport(p)
	if err != nil && *fatal == nil {
		*fatal = err
	}
	return pkg, err
}

type importerFunc func(string) (*types.Package, error)

func (f importerFunc) Import(p string) (*types.Package, error) { return f(p) }

// importStatus classifies an import path for the record and for unresolved
// call reasons.
func (s *session) importStatus(p string) string {
	if _, ok := s.dirOf[p]; ok {
		return ImportIndexed
	}
	if p == "C" {
		return ImportCgo
	}
	for _, m := range s.modules {
		if p == m.Path || strings.HasPrefix(p, m.Path+"/") {
			return ImportModuleNotIndexed
		}
	}
	first, _, _ := strings.Cut(p, "/")
	if !strings.Contains(first, ".") {
		return ImportStdlibNotIndexed
	}
	return ImportExternalNotIndexed
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
