//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

// The permission half of the control-endpoint boundary. It is platform-specific
// because "owner-only" is a POSIX mode statement; the transport itself is not.

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ControlEndpointMechanism names what the endpoint actually is, for doctor and
// for status. It is reported rather than assumed, so an operator can see the
// boundary they are relying on instead of trusting a document.
const ControlEndpointMechanism = "unix domain socket, owner-only (0600) inside the owner-only state directory"

// assertOwnerOnlyDir refuses a state directory other users can reach. It runs
// BEFORE the endpoint is created: publishing a control path into a
// group-writable directory would hand the operator's agents to anyone in that
// group, and creating it first and checking after would leave a window.
func assertOwnerOnlyDir(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("the state directory cannot be inspected: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("the state directory is mode %#o and is reachable by other users; run chmod 700 %s before serving a control endpoint", perm, dir)
	}
	return nil
}

// AssertControlEndpointSecure re-checks the boundary on an endpoint that
// already exists. Both the supervisor and every client call it, so a socket
// whose permissions were widened after it was created is refused by BOTH sides
// rather than trusted because it was safe when it was made.
func AssertControlEndpointSecure(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return &ControlEndpointError{Path: path, Detail: "no control endpoint is available: " + err.Error()}
	}
	if info.Mode()&os.ModeSocket == 0 {
		return &ControlEndpointError{Path: path, Detail: "is not a socket"}
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return &ControlEndpointError{Path: path, Detail: fmt.Sprintf("is mode %#o and is reachable by other users; anything that can reach it can start coding agents under this account", perm)}
	}
	return nil
}

// acquireControlStartLock serializes endpoint STARTUP across processes.
//
// Reclaiming a stale socket is dial-then-unlink, and between those two steps
// another supervisor can bind the path: A dials (nobody home), A binds, B dials
// (B's dial raced ahead and also found nobody home), B unlinks the socket A just
// bound, B binds a fresh inode - and two supervisors run, both driving the same
// store, which is exactly what the endpoint exists to prevent.
//
// The lock is an flock on a file beside the socket, held from before the dial
// until after the bind. flock is released by the kernel when the process exits,
// so a crash cannot leave a lock nobody can clear - which is the property an
// O_EXCL lock file would not have.
func acquireControlStartLock(stateDir string) (release func(), err error) {
	path := filepath.Join(stateDir, "serve.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("the control start lock cannot be opened: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		// Another supervisor holds it, which means one is starting or running.
		return nil, ErrSupervisorAlreadyRunning
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
