//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

// The actual principal/capability boundary #508's governed decision actions
// rely on (#508 review, authority-proof caveat): every decision-resolve and
// workgraph-hold request reaches the supervisor ONLY through this control
// endpoint, whose only credential is OS-level file ownership of an owner-only
// Unix socket inside an owner-only directory (runtime/control_endpoint_unix.go).
// #508 does not re-implement or re-authorize that boundary; it is #398's, and
// these are the first direct tests of its own refusal behavior.
//
// What this file PROVES: the mechanism that would refuse a widened-permission
// directory or socket actually refuses one, mechanically, on this platform.
// What it does NOT and CANNOT prove in a single-user test process: that a
// DIFFERENT OS user is refused by the kernel at connect/open time - that
// requires a second real OS account, which this suite has no fixture for.
// That half of the guarantee is the operating system's own file-permission
// enforcement, not application logic, and is exactly as strong as "owner-only
// means owner-only" already is for every other owner-only path this runtime
// relies on (the state directory itself, its locks, its SQLite file). A
// worker's provider process is launched by this SAME operator account,
// without this state directory on its own path and with no credential or
// reference to the control socket in its ExecutionRequest - provider isolation
// (runtime/provider_isolation.go, ProviderIsolation) is the existing, separate
// claim about what a sandboxed provider can and cannot reach, and #508 does
// not extend or re-test it here.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAssertOwnerOnlyDirRefusesAWidenedDirectory(t *testing.T) {
	dir := shortStateDir(t)
	if err := assertOwnerOnlyDir(dir); err != nil {
		t.Fatalf("a fresh owner-only (os.MkdirTemp default 0700) directory was refused: %v", err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := assertOwnerOnlyDir(dir); err == nil {
		t.Fatal("a group/world-readable state directory was accepted")
	}
}

func TestAssertControlEndpointSecureRefusesAWidenedSocket(t *testing.T) {
	dir := shortStateDir(t)
	listener, err := ListenControl(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	path := listener.Path()
	if err := AssertControlEndpointSecure(path); err != nil {
		t.Fatalf("a freshly listened, owner-only socket was refused: %v", err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := AssertControlEndpointSecure(path); err == nil {
		t.Fatal("a group/world-reachable control socket was accepted; anything on this machine could reach it")
	}
}

func TestAssertControlEndpointSecureRefusesANonSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.sock")
	if err := os.WriteFile(path, []byte("not a socket"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AssertControlEndpointSecure(path); err == nil {
		t.Fatal("an ordinary file at the socket path was accepted as a control endpoint")
	}
}
