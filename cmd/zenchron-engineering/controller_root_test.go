package main

import (
	"io"
	"path/filepath"
	"testing"
)

func TestControllerRootUsesAbsoluteIsolatedOverride(t *testing.T) {
	root := filepath.Join(t.TempDir(), "controller")
	t.Setenv("ZENCHRON_CONTROLLER_ROOT", root)
	if got := controllerRoot(); got != root {
		t.Fatalf("controllerRoot() = %q, want %q", got, root)
	}
}

func TestRunRejectsRelativeControllerRoot(t *testing.T) {
	t.Setenv("ZENCHRON_CONTROLLER_ROOT", "controller")
	if _, err := run([]string{"version"}, osCommands{}, io.Discard); err == nil {
		t.Fatal("run accepted a relative controller root")
	}
}
