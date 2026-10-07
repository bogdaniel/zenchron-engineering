// Package calc is a tiny fixture package for the kernel-eval corpus.
package calc

// Add returns the sum of a and b.
func Add(a, b int) int {
	return a - b // BUG: subtracts instead of adding
}
