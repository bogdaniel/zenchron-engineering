// zenchron-benchmark evaluates a manually recorded, paired benchmark dataset.
package main

import (
	"encoding/json"
	"fmt"
	"github.com/bogdaniel/zenchron-engineering/benchmark"
	"io"
	"os"
)

func run(r io.Reader, w io.Writer) error {
	var in benchmark.Input
	d := json.NewDecoder(r)
	d.DisallowUnknownFields()
	if err := d.Decode(&in); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON document")
	}
	result, err := benchmark.Evaluate(in)
	if err != nil {
		return err
	}
	e := json.NewEncoder(w)
	e.SetIndent("", "  ")
	return e.Encode(result)
}
func main() {
	if err := run(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
