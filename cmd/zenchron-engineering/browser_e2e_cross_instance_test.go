package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestBrowserCrossInstanceAuthentication runs a real browser against TWO
// concurrently running Control Plane instances on the same loopback
// hostname but different ports, each with its own real SQLite-backed state
// directory, token and listener - exactly the shape that silently collided
// before sessionCookieName scoped the console/app's cookie to the exact
// connected port (controlplane/web.go, frontend/src/api.ts): a browser's
// cookie jar is scoped by host, never by port, so signing in to one instance
// used to evict the other's session. Opt in on a Chrome host:
// ZENCHRON_BROWSER_E2E=1 go test ./cmd/zenchron-engineering -run '^TestBrowserCrossInstanceAuthentication$' -v
func TestBrowserCrossInstanceAuthentication(t *testing.T) {
	if os.Getenv("ZENCHRON_BROWSER_E2E") != "1" {
		t.Skip("set ZENCHRON_BROWSER_E2E=1 on a host with Chrome and Node")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	urlA, tokenA := startCrossInstanceFixture(t, "instance-a-run")
	urlB, tokenB := startCrossInstanceFixture(t, "instance-b-run")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "node", filepath.Join(root, "scripts", "check_cross_instance_auth.mjs"))
	command.Env = append(os.Environ(),
		"ZENCHRON_E2E_URL_A="+urlA, "ZENCHRON_E2E_TOKEN_A="+tokenA,
		"ZENCHRON_E2E_URL_B="+urlB, "ZENCHRON_E2E_TOKEN_B="+tokenB,
	)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
}

// startCrossInstanceFixture stands up one real, isolated Control Plane
// instance: its own temp state directory, SQLite-backed run (seeded through
// the same activeRun helper the full workspace E2E uses, never a design
// fixture), token and loopback listener. No controller process backs it, so
// its live observation genuinely fails rather than being stubbed to a
// canned value - Controller Health then shows a real "unreachable" read.
func startCrossInstanceFixture(t *testing.T, runID string) (url, token string) {
	t.Helper()
	state, err := os.MkdirTemp("/tmp", "zc-cross-instance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dir, config := planWorkspaceIn(t, state)
	activeRun(t, config, dir, runID)
	loaded, err := runtime.LoadConfig(config, dir)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := runtime.OpenReadStore(loaded.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	listener, err := controlplane.Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	token = hex.EncodeToString(b)
	observe := observeLiveController(loaded.StateDir)
	controllerRoot := filepath.Join(loaded.StateDir, "controller")
	api := &controlplane.API{Store: reader, Token: token, ControllerRoot: controllerRoot, Observe: observe, Listen: listener.Addr().String()}
	web := &controlplane.Web{Store: reader, Token: token, ControllerRoot: controllerRoot, Observe: observe}
	mux := http.NewServeMux()
	mux.Handle("/v1/", api.Handler())
	mux.Handle("/", web.Handler())
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close(); <-done })
	return "http://" + listener.Addr().String(), token
}
