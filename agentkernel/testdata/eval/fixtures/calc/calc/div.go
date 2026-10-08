package calc

import "errors"

// ErrDivideByZero is returned by Div when b is zero.
var ErrDivideByZero = errors.New("calc: divide by zero")

// Div returns a divided by b.
func Div(a, b int) (int, error) {
	if b == 0 {
		return 0, ErrDivideByZero
	}
	return a / b, nil
}
