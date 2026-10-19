package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
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

// #639: an operator found no link to open. The startup line must name the URL
// and the token FILE, and must never contain the token itself.
func TestControlPlaneStartupPrintsURLNeverToken(t *testing.T) {
	state, err := os.MkdirTemp("/tmp", "zc-cp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dir, config, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git", func(c map[string]any) { c["state_dir"] = state })
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	built.release()
	out := &lockedBuffer{}
	// ponytail: Serve has no stop; the goroutine ends with the test binary.
	go func() { _, _ = controlPlane([]string{"--state-dir", state, "--listen", "127.0.0.1:0"}, out) }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), "\n") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	line := out.String()
	token, err := os.ReadFile(filepath.Join(state, controlplane.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "http://127.0.0.1:") || !strings.Contains(line, "/app") || !strings.Contains(line, controlplane.TokenFile) {
		t.Fatalf("startup output does not give the URL and token file: %q", line)
	}
	if strings.Contains(line, strings.TrimSpace(string(token))) {
		t.Fatalf("startup output leaked the token: %q", line)
	}
}
