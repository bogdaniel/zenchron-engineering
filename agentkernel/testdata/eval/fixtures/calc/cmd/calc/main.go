// Command calc prints a fixed sum.
package main

import (
	"fmt"

	"example.com/calc/calc"
	"example.com/calc/strutil"
)

func main() {
	fmt.Println(calc.Add(2, 3), strutil.Reverse("calc"))
}
