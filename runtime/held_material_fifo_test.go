//go:build !windows

package runtime

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// INVARIANT: identifying held material never blocks. A producer that left a
// FIFO where a tracked file was must not hang the reconcile goroutine: only a
// regular file is ever opened, and a FIFO is identified by its type.
func TestContentDigestOfAFIFODoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "tracked.go"), 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { done <- workspaceContentDigest(dir, []string{"tracked.go", "other.go"}) }()
	select {
	case digest := <-done:
		if digest == "" {
			t.Fatal("a FIFO made the whole digest unknown; it should be identified by type")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("workspaceContentDigest blocked on a FIFO")
	}
}
