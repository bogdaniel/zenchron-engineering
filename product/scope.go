package product

import "fmt"

// Scope is one step on the knowledge boundary ladder (#476). Promotion only
// ever moves a claim UP this ladder, and only through Promote - never
// automatically from where an agent happened to discover it.
type Scope string

const (
	ScopeExecution    Scope = "execution"
	ScopeWorkUnit     Scope = "work_unit"
	ScopeFeature      Scope = "feature"
	ScopeProduct      Scope = "product"
	ScopeOrganization Scope = "organization"
)

// scopeRank orders the ladder. It is the ONLY place the order is decided, so a
// promotion direction check and a context compiler's "at or below this scope"
// filter can never disagree about which way is up.
var scopeRank = map[Scope]int{
	ScopeExecution:    0,
	ScopeWorkUnit:     1,
	ScopeFeature:      2,
	ScopeProduct:      3,
	ScopeOrganization: 4,
}

// KnownScope reports whether s is a defined ladder step.
func KnownScope(s Scope) bool {
	_, ok := scopeRank[s]
	return ok
}

// ScopeRank exposes the ladder's order to callers outside this package - a
// store that must filter "at or below this scope" in SQL rather than in Go,
// without duplicating the ladder's definition.
func ScopeRank(s Scope) (int, bool) {
	rank, ok := scopeRank[s]
	return rank, ok
}

// compareScope orders two scopes, refusing either side a reader could not
// place on the ladder at all. An unrecognized scope is never treated as the
// lowest or highest step by default - that would let a typo silently narrow
// or silently widen access.
func compareScope(a, b Scope) (int, error) {
	ra, ok := scopeRank[a]
	if !ok {
		return 0, fmt.Errorf("unrecognized scope %q", a)
	}
	rb, ok := scopeRank[b]
	if !ok {
		return 0, fmt.Errorf("unrecognized scope %q", b)
	}
	switch {
	case ra < rb:
		return -1, nil
	case ra > rb:
		return 1, nil
	default:
		return 0, nil
	}
}
