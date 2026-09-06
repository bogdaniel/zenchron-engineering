//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

// The permission half of the control-endpoint boundary. It is platform-specific
// because "owner-only" is a POSIX mode statement; the transport itself is not.

import (
	"fmt"
	"os"
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
