package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
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

// firstLineWriter hands over the first startup line, so a test waits on the
// event rather than polling.
type firstLineWriter struct{ lines chan string }

func (w firstLineWriter) Write(p []byte) (int, error) {
	select {
	case w.lines <- string(p):
	default:
	}
	return len(p), nil
}

// #639: an operator found no link to open. The startup line must name the URL
// and the token FILE, and must never contain the token itself. The server is
// stopped and joined before the test returns, so no listener outlives it.
func TestControlPlaneStartupPrintsURLNeverToken(t *testing.T) {
	dir, config, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	state := built.config.StateDir
	built.release()

	ctx, stop := context.WithCancel(context.Background())
	out := firstLineWriter{lines: make(chan string, 1)}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = controlPlaneUntil(ctx, []string{"--state-dir", state, "--listen", "127.0.0.1:0"}, out)
	}()
	var line string
	select {
	case line = <-out.lines:
	case <-time.After(10 * time.Second):
		t.Fatal("no startup line")
	}
	address := regexp.MustCompile(`http://([0-9.]+:\d+)/app`).FindStringSubmatch(line)
	token, err := os.ReadFile(filepath.Join(state, controlplane.TokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if address == nil || !strings.Contains(line, controlplane.TokenFile) {
		t.Fatalf("startup output does not give the URL and token file: %q", line)
	}
	if strings.Contains(line, strings.TrimSpace(string(token))) {
		t.Fatalf("startup output leaked the token: %q", line)
	}
	stop()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("control plane did not stop after cancellation")
	}
	if conn, err := net.DialTimeout("tcp", address[1], time.Second); err == nil {
		_ = conn.Close()
		t.Fatalf("listener %s outlived the test", address[1])
	}
}
