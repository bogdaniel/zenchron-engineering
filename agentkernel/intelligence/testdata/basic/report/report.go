package report

import "example.com/basic/shapes"

// Report calls across packages.
func Report() string { return shapes.Describe(shapes.NewSquare(2)) }
