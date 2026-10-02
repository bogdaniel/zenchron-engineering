package main

import (
	"io"
	"testing"
)

func TestControlPlaneCommandRejectsInvalidInvocation(t *testing.T) {
	for _, args := range [][]string{
		{"control-plane"},
		{"control-plane", "unexpected"},
		{"control-plane", "--unknown"},
		{"control-plane", "--state-dir", t.TempDir()},
	} {
		code, err := run(args, osCommands{}, io.Discard)
		if code == 0 || err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
