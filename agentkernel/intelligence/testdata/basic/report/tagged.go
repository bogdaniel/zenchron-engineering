//go:build extra

package report

// Extra exists only with the extra build tag.
func Extra() string { return Report() }
