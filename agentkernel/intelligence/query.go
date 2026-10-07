package intelligence

import (
	"fmt"
	"slices"
)

// CallResult is a call query answer. Complete is true only when nothing in
// the snapshot could hide a further edge; otherwise Limits says why the
// answer may be partial. A missing edge is never proof of no dependency.
type CallResult struct {
	Calls    []Call   `json:"calls"`
	Complete bool     `json:"complete"`
	Limits   []string `json:"limits,omitempty"`
}

// TestResult lists suggested tests. It is never complete: associations are
// inferred, and coverage is unknown.
type TestResult struct {
	Tests    []TestAssociation `json:"tests"`
	Complete bool              `json:"complete"`
	Limits   []string          `json:"limits"`
}

// Symbol looks up a symbol by ID.
func (ix *Index) Symbol(id string) (Symbol, bool) {
	for _, df := range ix.data.Dirs {
		if i := slices.IndexFunc(df.Symbols, func(s Symbol) bool { return s.ID == id }); i >= 0 {
			return df.Symbols[i], true
		}
	}
	return Symbol{}, false
}

// Callers returns every recorded call whose callee is symbol, with the
// reasons the list may be incomplete.
// ponytail: linear scan over all calls; index by callee if queries dominate.
func (ix *Index) Callers(symbol string) CallResult {
	var r CallResult
	sym, found := ix.Symbol(symbol)
	opaque := 0
	for _, df := range ix.data.Dirs {
		for _, c := range df.Calls {
			if c.Callee == symbol {
				r.Calls = append(r.Calls, c)
			}
			if c.Class != CallStatic {
				opaque++
			}
		}
	}
	if !found {
		r.Limits = append(r.Limits, "symbol not in snapshot: callers unknown")
	}
	if opaque > 0 {
		r.Limits = append(r.Limits, fmt.Sprintf("%d possible/dynamic/unresolved call relations in the snapshot may reach this symbol", opaque))
	}
	if found && sym.Exported {
		r.Limits = append(r.Limits, "exported: callers outside the indexed workspace are not visible")
	}
	r.Limits = append(r.Limits, ix.scopeLimits()...)
	r.Complete = len(r.Limits) == 0
	return r
}

// Callees returns the calls made by symbol.
func (ix *Index) Callees(symbol string) CallResult {
	var r CallResult
	sym, found := ix.Symbol(symbol)
	opaque := 0
	for _, df := range ix.data.Dirs {
		for _, c := range df.Calls {
			if c.Caller != symbol {
				continue
			}
			r.Calls = append(r.Calls, c)
			if c.Class != CallStatic {
				opaque++
			}
		}
	}
	if !found {
		r.Limits = append(r.Limits, "symbol not in snapshot: callees unknown")
	}
	if opaque > 0 {
		r.Limits = append(r.Limits, fmt.Sprintf("%d calls of this symbol have no proven target", opaque))
	}
	if found && ix.declarationIncomplete(sym) {
		r.Limits = append(r.Limits, "the declaring file or package has incomplete analysis")
	}
	r.Complete = len(r.Limits) == 0
	return r
}

// SuggestedTests returns tests associated with symbol.
func (ix *Index) SuggestedTests(symbol string) TestResult {
	r := TestResult{Limits: []string{CoverageUnknown}}
	for _, t := range ix.data.Tests {
		if t.Target == symbol {
			r.Tests = append(r.Tests, t)
		}
	}
	if len(r.Tests) == 0 {
		r.Limits = append(r.Limits, "no association found; this is not evidence that no test exercises the symbol")
	}
	r.Limits = append(r.Limits, ix.scopeLimits()...)
	return r
}

// scopeLimits are the snapshot-wide reasons any relation query may be partial.
func (ix *Index) scopeLimits() []string {
	var out []string
	if len(ix.data.Identity.Scope.Packages) > 0 {
		out = append(out, "bounded scope: packages outside the requested directories are not indexed")
	}
	n := len(ix.data.Incomplete)
	for _, df := range ix.data.Dirs {
		n += len(df.Incomplete)
	}
	if n > 0 {
		out = append(out, fmt.Sprintf("%d incomplete-analysis records (parse errors, excluded files, cgo, type errors)", n))
	}
	return out
}

func (ix *Index) declarationIncomplete(sym Symbol) bool {
	for _, df := range ix.data.Dirs {
		if slices.ContainsFunc(df.Incomplete, func(in Incomplete) bool {
			return in.Path == sym.Evidence.File || in.Path == sym.Package || in.Path == sym.Package+" [tests]"
		}) {
			return true
		}
	}
	return false
}
