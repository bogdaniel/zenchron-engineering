package intelligence

import (
	"go/ast"
	"go/types"
)

// call classifies one call expression. Only go/types-resolved direct calls
// into indexed packages are static; everything else stays visibly possible,
// dynamic or unresolved rather than being dropped.
func (e *extractor) call(owner string, c *ast.CallExpr) {
	fun := ast.Unparen(c.Fun)
	for {
		switch x := fun.(type) {
		case *ast.IndexExpr:
			fun = ast.Unparen(x.X)
			continue
		case *ast.IndexListExpr:
			fun = ast.Unparen(x.X)
			continue
		}
		break
	}
	switch f := fun.(type) {
	case *ast.Ident:
		e.callIdent(owner, f)
	case *ast.SelectorExpr:
		e.callSelector(owner, f)
	case *ast.StarExpr:
		if !e.namesType(ast.Unparen(f.X)) {
			e.addCall(owner, calleeText(fun), CallDynamic, "call through a dereferenced function pointer", c.Pos())
		}
	case *ast.FuncLit:
		// Immediately invoked literal: its body is walked in place.
	default:
		if !isTypeExpr(fun) {
			e.addCall(owner, calleeText(fun), CallDynamic, "callee is a computed expression", c.Pos())
		}
	}
}

func (e *extractor) namesType(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.Ident:
		_, ok := e.info.Uses[v].(*types.TypeName)
		return ok
	case *ast.SelectorExpr:
		_, ok := e.info.Uses[v.Sel].(*types.TypeName)
		return ok
	}
	return isTypeExpr(x)
}

func (e *extractor) callIdent(owner string, id *ast.Ident) {
	switch obj := e.info.Uses[id].(type) {
	case *types.Func:
		e.addCall(owner, funcID(obj), CallStatic, "", id.Pos())
	case *types.Builtin, *types.TypeName:
		// Builtins and conversions are not calls into code.
	case *types.Var:
		e.addCall(owner, id.Name, CallDynamic, "call through a function value", id.Pos())
	default:
		e.addCall(owner, id.Name, CallUnresolved, "identifier not resolved (dot import of an unindexed package or type error)", id.Pos())
	}
}

func (e *extractor) callSelector(owner string, sel *ast.SelectorExpr) {
	if s, ok := e.info.Selections[sel]; ok {
		e.callSelection(owner, sel, s)
		return
	}
	if x, ok := ast.Unparen(sel.X).(*ast.Ident); ok {
		if pn, ok := e.info.Uses[x].(*types.PkgName); ok {
			e.callQualified(owner, sel, pn.Imported().Path())
			return
		}
	}
	if e.reflect && reflectCallNames[sel.Sel.Name] {
		e.addCall(owner, calleeText(sel), CallDynamic, "reflection: target named at run time", sel.Sel.Pos())
		return
	}
	e.addCall(owner, calleeText(sel), CallUnresolved, "receiver type unknown (operand from an unindexed package or ill-typed)", sel.Sel.Pos())
}

func (e *extractor) callSelection(owner string, sel *ast.SelectorExpr, s *types.Selection) {
	fn, isFunc := s.Obj().(*types.Func)
	switch {
	case !isFunc:
		e.addCall(owner, calleeText(sel), CallDynamic, "call through a function-valued field", sel.Sel.Pos())
	case isInterfaceMethod(fn):
		e.addCall(owner, funcID(fn), CallPossible, "interface method: concrete target chosen at run time", sel.Sel.Pos())
	default:
		e.addCall(owner, funcID(fn), CallStatic, "", sel.Sel.Pos())
	}
}

func (e *extractor) callQualified(owner string, sel *ast.SelectorExpr, imported string) {
	if imported == "reflect" {
		e.addCall(owner, calleeText(sel), CallDynamic, "reflection", sel.Sel.Pos())
		return
	}
	switch obj := e.info.Uses[sel.Sel].(type) {
	case *types.Func:
		e.addCall(owner, funcID(obj), CallStatic, "", sel.Sel.Pos())
	case *types.TypeName, *types.Builtin:
		// Conversion to an indexed type.
	case *types.Var:
		e.addCall(owner, calleeText(sel), CallDynamic, "call through a package-level function value", sel.Sel.Pos())
	default:
		e.addCall(owner, calleeText(sel), CallUnresolved, "package "+imported+" is "+e.s.importStatus(imported), sel.Sel.Pos())
	}
}
