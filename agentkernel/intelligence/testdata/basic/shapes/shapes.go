package shapes

import "fmt"

// Shape is implemented by every figure.
type Shape interface {
	Area() float64
}

// Square is a concrete Shape.
type Square struct{ Side float64 }

// Area implements Shape.
func (s Square) Area() float64 { return s.Side * s.Side }

// NewSquare builds a Square.
func NewSquare(side float64) Square { return Square{Side: side} }

// Total sums areas through the interface.
func Total(shapes []Shape) float64 {
	t := 0.0
	for _, s := range shapes {
		t += s.Area()
	}
	return t
}

// Describe calls into an unindexed package.
func Describe(s Shape) string { return fmt.Sprintf("area %.1f", s.Area()) }

// Apply calls through a function value.
func Apply(f func(float64) float64, v float64) float64 { return f(v) }

func helper() int { return 1 }

func usesHelper() int { return helper() + NewSquare(1).Side2() }

// Side2 is a concrete method called statically.
func (s Square) Side2() int { return int(s.Side) * 2 }
