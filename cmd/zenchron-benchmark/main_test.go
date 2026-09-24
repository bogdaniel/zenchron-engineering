package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestInvalidInput(t *testing.T) {
	for _, s := range []string{`{}`, `{"unexpected":1}`, `{} {}`, `not json`} {
		var out bytes.Buffer
		if run(strings.NewReader(s), &out) == nil {
			t.Fatalf("accepted %s", s)
		}
		if out.Len() != 0 {
			t.Fatal("wrote report for invalid data")
		}
	}
}
