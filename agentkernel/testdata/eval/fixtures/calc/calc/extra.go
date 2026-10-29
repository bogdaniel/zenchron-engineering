//go:build extra

package calc

// Mul is compiled only with the "extra" build tag.
func Mul(a, b int) int { return a * b }
