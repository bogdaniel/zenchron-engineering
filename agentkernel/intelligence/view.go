package intelligence

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// ErrSnapshotMismatch reports a query for a workspace the view's snapshot
// does not describe. The view answers nothing rather than stale facts.
var ErrSnapshotMismatch = errors.New("intelligence: workspace does not match the snapshot")

// maxListed bounds each relation list inside one context item.
const maxListed = 20

// View binds one index to the workspace it describes. It implements
// api.ContextSource; every item is workspace-trusted data, never host
// instruction, and none is required.
type View struct {
	ix        *Index
	workspace api.WorkspaceRef
}

var _ api.ContextSource = (*View)(nil)

// NewView binds ix to workspace. The workspace manifest digest must be the
// snapshot's manifest digest: computing it with BuildManifest-equivalent
// selection is the host's proof that the snapshot describes these bytes.
func NewView(ix *Index, workspace api.WorkspaceRef) (*View, error) {
	if !api.ValidIdentifier(workspace.ID) {
		return nil, fmt.Errorf("intelligence: invalid workspace id %q", workspace.ID)
	}
	if workspace.ManifestDigest != ix.data.Identity.ManifestDigest {
		return nil, fmt.Errorf("%w: workspace %s, snapshot %s", ErrSnapshotMismatch,
			workspace.ManifestDigest, ix.data.Identity.ManifestDigest)
	}
	return &View{ix: ix, workspace: workspace}, nil
}

// ContextItems returns source, dependency and test items relevant to the
// objective, ordered by score then ID, at most query.Limit when positive.
func (v *View) ContextItems(ctx context.Context, q api.ContextQuery) ([]api.ContextItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if q.Workspace.ID != v.workspace.ID || q.Workspace.ManifestDigest != v.workspace.ManifestDigest {
		return nil, fmt.Errorf("%w: query workspace %s@%s, view %s@%s", ErrSnapshotMismatch,
			q.Workspace.ID, q.Workspace.ManifestDigest, v.workspace.ID, v.workspace.ManifestDigest)
	}
	terms := objectiveTerms(q.Objective)
	var items []api.ContextItem
	for _, df := range v.ix.data.Dirs {
		items = append(items, v.dirItems(df, terms)...)
	}
	slices.SortFunc(items, func(a, b api.ContextItem) int {
		if a.Score != b.Score {
			if a.Score > b.Score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ID, b.ID)
	})
	if q.Limit > 0 && len(items) > q.Limit {
		items = items[:q.Limit]
	}
	return items, nil
}

func (v *View) dirItems(df DirFacts, terms map[string]bool) []api.ContextItem {
	var items []api.ContextItem
	fileScore := map[string]float64{}
	for _, f := range df.Files {
		fileScore[f.Path] = float64(matchCount(nameTokens(f.Path), terms))
	}
	for _, sym := range df.Symbols {
		score := symbolScore(sym, terms)
		fileScore[sym.Evidence.File] += score
		if score == 0 || sym.Kind == KindConst || strings.HasSuffix(sym.Evidence.File, "_test.go") {
			continue
		}
		items = append(items, v.item(api.ContextDependency, "dep:"+sym.ID, v.dependencyText(sym), score))
		if tests := v.ix.SuggestedTests(sym.ID); len(tests.Tests) > 0 {
			items = append(items, v.item(api.ContextTest, "test:"+sym.ID, testText(sym, tests), score))
		}
	}
	for _, f := range df.Files {
		if fileScore[f.Path] == 0 {
			continue
		}
		kind := api.ContextSourceCode
		if f.Test {
			kind = api.ContextTest
		}
		items = append(items, v.item(kind, "file:"+f.Path, fileText(f, df), fileScore[f.Path]))
	}
	return items
}

func (v *View) item(kind api.ContextKind, subject, content string, score float64) api.ContextItem {
	return api.ContextItem{
		ID:            "intel." + string(kind) + "." + api.Digest([]byte(subject))[len("sha256:"):][:16],
		Kind:          kind,
		Trust:         api.TrustWorkspace,
		Content:       content,
		ContentDigest: api.Digest([]byte(content)),
		Revision:      v.ix.Key(),
		Score:         score,
	}
}

func fileText(f File, df DirFacts) string {
	var b strings.Builder
	fmt.Fprintf(&b, "file: %s\npackage: %s\ndigest: %s\n", f.Path, f.Package, f.Digest)
	if !f.Included {
		fmt.Fprintf(&b, "not analysed: %s\n", f.Reason)
	}
	b.WriteString("declarations:\n")
	for _, s := range df.Symbols {
		if s.Evidence.File == f.Path {
			fmt.Fprintf(&b, "  %s %s (line %d)\n", s.Kind, displayName(s), s.Evidence.Line)
		}
	}
	return b.String()
}

func (v *View) dependencyText(sym Symbol) string {
	var b strings.Builder
	fmt.Fprintf(&b, "symbol: %s (%s) at %s:%d\n", sym.ID, sym.Kind, sym.Evidence.File, sym.Evidence.Line)
	writeCalls(&b, "callers", v.ix.Callers(sym.ID), func(c Call) string { return c.Caller })
	writeCalls(&b, "callees", v.ix.Callees(sym.ID), func(c Call) string { return c.Callee })
	return b.String()
}

func writeCalls(b *strings.Builder, label string, r CallResult, end func(Call) string) {
	fmt.Fprintf(b, "%s (complete=%t):\n", label, r.Complete)
	for i, c := range r.Calls {
		if i == maxListed {
			fmt.Fprintf(b, "  ... %d more\n", len(r.Calls)-maxListed)
			break
		}
		fmt.Fprintf(b, "  %s [%s, %s:%d]", end(c), c.Class, c.Evidence.File, c.Evidence.Line)
		if c.Reason != "" {
			fmt.Fprintf(b, " %s", c.Reason)
		}
		b.WriteString("\n")
	}
	for _, l := range r.Limits {
		fmt.Fprintf(b, "  limit: %s\n", l)
	}
}

func testText(sym Symbol, r TestResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "suggested tests for %s (complete=false):\n", sym.ID)
	for _, t := range r.Tests {
		fmt.Fprintf(&b, "  %s [%s, %s] %s:%d\n", t.Test, t.Evidence.Method, t.Evidence.Confidence, t.Evidence.File, t.Evidence.Line)
	}
	for _, l := range r.Limits {
		fmt.Fprintf(&b, "  limit: %s\n", l)
	}
	return b.String()
}

func displayName(s Symbol) string {
	if s.Receiver != "" {
		return s.Receiver + "." + s.Name
	}
	return s.Name
}

// symbolScore is deterministic lexical relevance: an exact name match counts
// more than a shared camelCase token.
func symbolScore(s Symbol, terms map[string]bool) float64 {
	score := float64(matchCount(nameTokens(displayName(s)), terms))
	if terms[strings.ToLower(s.Name)] {
		score += 2
	}
	return score
}

func matchCount(tokens []string, terms map[string]bool) int {
	n := 0
	for _, t := range slices.Compact(slices.Sorted(slices.Values(tokens))) {
		if terms[t] {
			n++
		}
	}
	return n
}

func objectiveTerms(objective string) map[string]bool {
	terms := map[string]bool{}
	for _, t := range nameTokens(objective) {
		terms[t] = true
	}
	for _, w := range strings.FieldsFunc(objective, func(r rune) bool { return !isWordRune(r) }) {
		if len(w) >= 3 {
			terms[strings.ToLower(w)] = true
		}
	}
	return terms
}

// nameTokens splits identifiers and paths into lower-case words of at least
// three characters: "parseGoMod" -> parse, mod; "pkg/http_server.go" -> pkg,
// http, server.
func nameTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) >= 3 {
			out = append(out, strings.ToLower(string(cur)))
		}
		cur = cur[:0]
	}
	var prev rune
	for _, r := range s {
		switch {
		case !isWordRune(r) || r == '_':
			flush()
		case unicode.IsUpper(r) && unicode.IsLower(prev):
			flush()
			cur = append(cur, r)
		default:
			cur = append(cur, r)
		}
		prev = r
	}
	flush()
	return out
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }
