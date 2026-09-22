package runtime

// THE CONTROLLER ROLE: exclusive permission to be the controller, held by the
// kernel rather than by a convention.
//
// Until now nothing held it. `serve` was made exclusive by binding the control
// socket first - "a second supervisor is refused before it starts competing for
// leases" - which works by accident of the filesystem and makes the
// COMMUNICATION PATH the ownership mechanism. Two consequences, both fatal to a
// handoff: transferring ownership would mean transferring the socket, and a
// stale socket file is a fact about a directory rather than about a process.
//
// OwnershipLock does not fill the gap either. Its path contains the owner
// identity - host, pid, token - so two controllers take two different locks and
// exclude nothing. It is a liveness probe for one instance, which is what it
// was built to be.
//
// This is the missing primitive: ONE well-known path, an exclusive advisory
// lock, and no metadata anybody has to believe. Holding it is the fact. A
// crashed holder is released by the kernel, so there is no stale state to
// interpret, no pid to compare and no reuse to reason about.
//
// WHAT IT IS NOT. It is not activation - a controller can hold the role while
// being forbidden to serve, which is exactly the successor's position during a
// handoff - and it is not identity. It answers one question: may this process
// act as the controller at all.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// controllerRoleLockName is the single path the role is taken at. It is
// deliberately not parameterised: a role lock whose path varied by caller would
// be as exclusive as the callers agreed to be.
const controllerRoleLockName = "controller-role"

// ControllerRoleLock is the held role. The descriptor stays open for its
// lifetime, because the open descriptor IS the claim.
type ControllerRoleLock struct {
	path string
	file *os.File
}

// ControllerRoleLockPath is where the role is taken, for diagnosis only. Its
// existence is never evidence that anybody holds it.
func ControllerRoleLockPath(stateDir string) string {
	return filepath.Join(stateDir, "locks", controllerRoleLockName)
}

// AcquireControllerRole takes the controller role, or reports that another live
// process holds it.
//
// It never blocks. A caller that waited would be a second controller queued to
// take over the moment the first one hiccuped, which is not a thing this
// protocol wants to exist: the successor acquires only after the predecessor
// has deliberately released.
func AcquireControllerRole(stateDir string) (*ControllerRoleLock, error) {
	path := ControllerRoleLockPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("claiming the controller role: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claiming the controller role: %w", err)
	}
	locked, err := tryLockFile(file, true)
	if err == nil && !locked {
		err = errors.New("another live process holds the controller role")
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("claiming the controller role: %w", err)
	}
	return &ControllerRoleLock{path: path, file: file}, nil
}

// Release gives the role up. It is idempotent and nil-safe so a shutdown path
// can defer it unconditionally.
//
// THE FILE IS NOT REMOVED, and that is a correctness property rather than
// laziness. Unlinking a lock path lets a second process create a NEW file at
// the same name and lock that, while a third still holds a lock on the
// unlinked inode - two holders of one role, each correctly locked, neither
// visible to the other. The inode has to outlive the holders.
func (l *ControllerRoleLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}

// Held reports whether this lock is still held by this process.
func (l *ControllerRoleLock) Held() bool { return l != nil && l.file != nil }

// Path reports the lock file, for diagnosis only.
func (l *ControllerRoleLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// ControllerRoleHeld reports whether some live process holds the role.
//
// decided is false when the question could not be answered, which a caller must
// treat as "held" rather than as free: an unanswerable ownership question is
// the one case where guessing costs two controllers.
//
// The probe takes a SHARED lock, so two processes asking do not block each
// other and only a genuine exclusive holder reads as held. The file is created
// when missing, precisely so its presence cannot influence the answer.
func ControllerRoleHeld(stateDir string) (held bool, decided bool) {
	path := ControllerRoleLockPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return true, false
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return true, false
	}
	defer func() { _ = file.Close() }()
	locked, err := tryLockFile(file, false)
	if err != nil {
		return true, false
	}
	return !locked, true
}
