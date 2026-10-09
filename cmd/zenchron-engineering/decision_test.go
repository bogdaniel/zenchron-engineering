package main

import "testing"

// TestExtractKindFlag is #508 review B5's CLI surface: an explicit
// --kind overrides defaultDecisionOutcomeKind's guess, consumed before the
// shared autonomyFlags parser ever sees resolve's remaining arguments.
func TestExtractKindFlag(t *testing.T) {
	kind, rest, err := extractKindFlag([]string{"--kind", "selected_option", "--note", "pick B"})
	if err != nil || kind != "selected_option" || len(rest) != 2 || rest[0] != "--note" {
		t.Fatalf("kind=%q rest=%v err=%v", kind, rest, err)
	}
	kind, rest, err = extractKindFlag([]string{"--note", "pick B"})
	if err != nil || kind != "" || len(rest) != 2 {
		t.Fatalf("expected no --kind to pass through untouched: kind=%q rest=%v err=%v", kind, rest, err)
	}
	if _, _, err := extractKindFlag([]string{"--kind", ""}); err == nil {
		t.Fatal("expected an empty --kind value to be refused")
	}
}
