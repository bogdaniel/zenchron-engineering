package intelligence

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strconv"
	"strings"
)

// extractDir type-checks the package in dir (plus its in-package and external
// tests) and records its facts.
func (s *session) extractDir(dir string) (DirFacts, error) {
	main, err := s.checkMain(dir)
	if err != nil {
		return DirFacts{}, err
	}
	pd := s.parsed[dir]
	ip := s.pathOf[dir]
	df := DirFacts{Dir: dir, Files: pd.files, Incomplete: append([]Incomplete(nil), pd.incomplete...)}
	mod, _ := moduleFor(s.modules, dir)
	pkg := Package{ImportPath: ip, Dir: dir, Name: pd.name, Module: mod.Path, Files: filePaths(s.fset, pd.main, pd.itest)}
	df.Packages = append(df.Packages, pkg)
	s.newExtractor(&df, ip, main).files(pd.main)
	recordTypeErrors(&df, ip, main.errors)
	if len(pd.itest) > 0 {
		c, err := s.check(ip, pd.name, append(append([]*ast.File(nil), pd.main...), pd.itest...))
		if err != nil {
			return DirFacts{}, err
		}
		s.newExtractor(&df, ip, c).files(pd.itest)
		recordTypeErrors(&df, ip+" [tests]", c.errors)
	}
	if len(pd.xtest) > 0 {
		xip := ip + "_test"
		c, err := s.check(xip, pd.name+"_test", pd.xtest)
		if err != nil {
			return DirFacts{}, err
		}
		df.Packages = append(df.Packages, Package{ImportPath: xip, Dir: dir, Name: pd.name + "_test",
			Module: mod.Path, Test: true, Files: filePaths(s.fset, pd.xtest)})
		s.newExtractor(&df, xip, c).files(pd.xtest)
		recordTypeErrors(&df, xip, c.errors)
	}
	owner := map[string]string{}
	for _, p := range df.Packages {
		for _, f := range p.Files {
			owner[f] = p.ImportPath
		}
	}
	for i := range df.Files {
		df.Files[i].Package = owner[df.Files[i].Path]
	}
	return df, nil
}

// recordTypeErrors keeps type errors visible: most come from selectors into
// unindexed packages, and every one means some expression's type is unknown.
func recordTypeErrors(df *DirFacts, ip string, errs []string) {
	if len(errs) == 0 {
		return
	}
	df.Incomplete = append(df.Incomplete, Incomplete{Kind: IncompleteTypeErrors, Path: ip,
		Detail: fmt.Sprintf("%d type errors; first: %s", len(errs), errs[0])})
}

func filePaths(fset *token.FileSet, groups ...[]*ast.File) []string {
	var out []string
	for _, g := range groups {
		for _, f := range g {
			out = append(out, fset.Position(f.Package).Filename)
		}
	}
	return out
}

type extractor struct {
	s       *session
	out     *DirFacts
	pkgPath string
	info    *types.Info
	file    string
	reflect bool // current file imports reflect
	seen    map[string]bool
}

func (s *session) newExtractor(df *DirFacts, pkgPath string, c *checkedPkg) *extractor {
	return &extractor{s: s, out: df, pkgPath: pkgPath, info: c.info, seen: map[string]bool{}}
}

func (e *extractor) files(files []*ast.File) {
	for _, f := range files {
		e.file = e.s.fset.Position(f.Package).Filename
		e.reflect = false
		for _, spec := range f.Imports {
			p, _ := strconv.Unquote(spec.Path.Value) // the parser accepted it as a string literal
			e.reflect = e.reflect || p == "reflect"
			e.out.Imports = append(e.out.Imports, Import{Package: e.pkgPath, Path: p, Status: e.s.importStatus(p),
				Evidence: e.evidence(spec.Pos(), MethodSyntax, ConfidenceDeterministic)})
		}
		for _, decl := range f.Decls {
			e.decl(decl)
		}
	}
}

func (e *extractor) decl(decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		id := e.funcDecl(d)
		e.walk(id, d.Type)
		if d.Body != nil {
			e.walk(id, d.Body)
		}
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			e.spec(d.Tok, spec)
		}
	}
}

func (e *extractor) funcDecl(d *ast.FuncDecl) string {
	name := d.Name.Name
	if d.Recv == nil || len(d.Recv.List) == 0 {
		id := e.pkgPath + "." + name
		if name == "init" {
			id = fmt.Sprintf("%s.init@%s:%d", e.pkgPath, e.file, e.line(d.Pos()))
		}
		e.symbol(id, name, KindFunc, "", d.Name.Pos())
		return id
	}
	recv := receiverName(d.Recv.List[0].Type)
	id := e.pkgPath + "." + recv + "." + name
	e.symbol(id, name, KindMethod, recv, d.Name.Pos())
	return id
}

func (e *extractor) spec(tok token.Token, spec ast.Spec) {
	switch sp := spec.(type) {
	case *ast.TypeSpec:
		id := e.pkgPath + "." + sp.Name.Name
		e.symbol(id, sp.Name.Name, KindType, "", sp.Name.Pos())
		e.walk(id, sp.Type)
	case *ast.ValueSpec:
		kind := KindVar
		if tok == token.CONST {
			kind = KindConst
		}
		owner := e.pkgPath + "._"
		for _, n := range sp.Names {
			if n.Name == "_" {
				continue
			}
			id := e.pkgPath + "." + n.Name
			if owner == e.pkgPath+"._" {
				owner = id
			}
			e.symbol(id, n.Name, kind, "", n.Pos())
		}
		if sp.Type != nil {
			e.walk(owner, sp.Type)
		}
		for _, v := range sp.Values {
			e.walk(owner, v)
		}
	}
}

func (e *extractor) symbol(id, name, kind, recv string, pos token.Pos) {
	e.out.Symbols = append(e.out.Symbols, Symbol{ID: id, Package: e.pkgPath, Name: name, Kind: kind, Receiver: recv,
		Exported: token.IsExported(name), Evidence: e.evidence(pos, MethodSyntax, ConfidenceDeterministic)})
}

func receiverName(expr ast.Expr) string {
	for {
		switch x := expr.(type) {
		case *ast.StarExpr:
			expr = x.X
		case *ast.ParenExpr:
			expr = x.X
		case *ast.IndexExpr:
			expr = x.X
		case *ast.IndexListExpr:
			expr = x.X
		case *ast.Ident:
			return x.Name
		default:
			return "?"
		}
	}
}

// walk attributes every call and reference under n to owner. Function
// literals are attributed to the declaration that contains them.
func (e *extractor) walk(owner string, n ast.Node) {
	ast.Inspect(n, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			e.call(owner, x)
		case *ast.Ident:
			e.reference(owner, x)
		}
		return true
	})
}

func (e *extractor) reference(owner string, id *ast.Ident) {
	obj := e.info.Uses[id]
	if obj == nil || obj.Pkg() == nil {
		return
	}
	to := objectID(obj)
	if to == "" || e.seen["r\x00"+owner+"\x00"+to] {
		return
	}
	e.seen["r\x00"+owner+"\x00"+to] = true
	e.out.References = append(e.out.References, Reference{From: owner, To: to,
		Evidence: e.evidence(id.Pos(), MethodTypes, ConfidenceDeterministic)})
}

func (e *extractor) addCall(owner, callee string, class CallClass, reason string, pos token.Pos) {
	key := "c\x00" + owner + "\x00" + callee + "\x00" + string(class)
	if e.seen[key] {
		return
	}
	e.seen[key] = true
	conf := map[CallClass]Confidence{CallStatic: ConfidenceDeterministic, CallPossible: ConfidenceInferred}[class]
	if conf == "" {
		conf = ConfidenceUnresolved
	}
	e.out.Calls = append(e.out.Calls, Call{Caller: owner, Callee: callee, Class: class, Reason: reason,
		Evidence: e.evidence(pos, MethodTypes, conf)})
}

func (e *extractor) evidence(pos token.Pos, m Method, c Confidence) Evidence {
	return Evidence{File: e.file, Line: e.line(pos), Method: m, Confidence: c}
}

func (e *extractor) line(pos token.Pos) int { return e.s.fset.Position(pos).Line }

// objectID names a package-level object or method; locals, fields and
// package names have no symbol and yield "".
func objectID(obj types.Object) string {
	switch o := obj.(type) {
	case *types.Func:
		return funcID(o)
	case *types.TypeName, *types.Const, *types.Var:
		if obj.Parent() == obj.Pkg().Scope() {
			return obj.Pkg().Path() + "." + obj.Name()
		}
	}
	return ""
}

// funcID matches the symbol IDs funcDecl produces from syntax.
func funcID(f *types.Func) string {
	f = f.Origin()
	pkg := ""
	if f.Pkg() != nil {
		pkg = f.Pkg().Path()
	}
	recv := f.Type().(*types.Signature).Recv()
	if recv == nil {
		return pkg + "." + f.Name()
	}
	t := recv.Type()
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	if n, ok := t.(*types.Named); ok {
		return pkg + "." + n.Obj().Name() + "." + f.Name()
	}
	return pkg + ".?." + f.Name()
}

func isInterfaceMethod(f *types.Func) bool {
	recv := f.Type().(*types.Signature).Recv()
	return recv != nil && types.IsInterface(recv.Type())
}

func unparen(x ast.Expr) ast.Expr {
	for {
		p, ok := x.(*ast.ParenExpr)
		if !ok {
			return x
		}
		x = p.X
	}
}

var reflectCallNames = map[string]bool{"Call": true, "CallSlice": true, "MethodByName": true}

func isTypeExpr(x ast.Expr) bool {
	switch x.(type) {
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	}
	return false
}

func calleeText(x ast.Expr) string { return strings.TrimSpace(types.ExprString(x)) }
