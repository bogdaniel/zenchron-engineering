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
// ControllerInstanceLock does not fill the gap either. Its path contains the owner
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
	"sync"
)

// controllerRoleLockName is the single path the role is taken at. It is
// deliberately not parameterised: a role lock whose path varied by caller would
// be as exclusive as the callers agreed to be.
//
// THE PATHNAME IS A RESERVED PERSISTENT ANCHOR, and the inode behind it is part
// of this protocol's durable substrate. Zenchron code must never unlink,
// rename or replace it - not in Release, not in garbage collection, not in
// cleanup. With advisory locks authority attaches to the INODE and not to the
// name, so replacing the file while a controller holds it produces two
// processes each correctly holding an exclusive lock on a different inode.
// That is controller-root corruption rather than a recoverable ownership
// transition, and nothing here tries to reconcile it.
//
// ENVIRONMENTAL ASSUMPTION: the controller root is on a filesystem with working
// advisory lock semantics. That is true of ordinary local filesystems and is
// not universally true of network ones, so moving a controller root onto a
// network filesystem is a compatibility question to be answered before it is
// done rather than an implicit assumption made here.
const controllerRoleLockName = "controller-role"

// ControllerRoleLease is the held role, as a CAPABILITY rather than as a fact
// anybody can look up.
//
// The distinction is the whole design. A queryable role - Held(), Valid(),
// StillOwned() - is an invitation to write
//
//	if role.Held() { ... later ... mutate() }
//
// which is check-then-act with a longer gap than usual, and the gap is a
// process release. So there is no such method. Authority is exercised by
// PASSING A FUNCTION INTO the capability, which runs it while the capability is
// structurally still held, or refuses.
//
// The capability is backed by the live resource. Not a path, not an owner
// string, not a boolean - an open descriptor whose kernel lock is the role. A
// token that could outlive the lock would be a capability that outlives the
// authority it represents, which is the failure this type exists to prevent.
type ControllerRoleLease struct {
	// mu serializes authority against release, exactly as the work-admission
	// gate serializes admission against draining: authority sections hold it
	// for reading, release takes it exclusively. That is what makes "after
	// Release returns, nothing authorized by this lease can still commit" a
	// property rather than a hope.
	mu       sync.RWMutex
	released bool
	path     string
	file     *os.File
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
func AcquireControllerRole(stateDir string) (*ControllerRoleLease, error) {
	path := ControllerRoleLockPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("claiming the controller role: %w", err)
	}
	// A SYMLINK AT THE ANCHOR IS REFUSED RATHER THAN FOLLOWED. Following one
	// would take the role on whatever inode it names, which is the split-inode
	// failure by another route. This detects a misconfigured or tampered
	// controller root; it is not a defence against a racing attacker, and does
	// not pretend to be.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("claiming the controller role: %s is a symlink, and the role anchor must be a regular file", path)
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
	return &ControllerRoleLease{path: path, file: file}, nil
}

// WithAuthority runs op while this process structurally holds the role.
//
// It is the ONLY way the role authorizes anything. The operation executes
// inside the same boundary a release has to take, so a release that begins
// mid-operation waits for it and an operation that begins after a release is
// refused. There is no window in which the answer changes between being read
// and being used, because it is never read - it is held.
//
// Concurrent authority sections are permitted: two privileged operations under
// one live role are not in conflict with each other, only with the role ending.
func (l *ControllerRoleLease) WithAuthority(op func() error) error {
	if l == nil {
		return &ControllerRoleUnheldError{Detail: "no controller role lease was supplied"}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.released || l.file == nil {
		return &ControllerRoleUnheldError{Detail: "the controller role lease has been released"}
	}
	return op()
}

// ControllerRoleUnheldError is an operation refused for want of the role. It is
// distinct from a failure of the operation itself: nothing was attempted.
type ControllerRoleUnheldError struct{ Detail string }

func (e *ControllerRoleUnheldError) Error() string {
	if e.Detail == "" {
		return "this process does not hold the controller role"
	}
	return "this process does not hold the controller role: " + e.Detail
}

// Release gives the role up. It is TERMINAL: a released lease authorizes
// nothing afterwards and is never revived, so taking the role again produces a
// new capability rather than reanimating this one. It is idempotent and
// nil-safe so a shutdown path can defer it unconditionally.
//
// It waits for authority sections already running, which is the point: a
// release that returned while a privileged operation was still committing
// would be a release that did not release.
//
// THE FILE IS NOT REMOVED, and that is a correctness property rather than
// laziness. Unlinking a lock path lets a second process create a NEW file at
// the same name and lock that, while a third still holds a lock on the
// unlinked inode - two holders of one role, each correctly locked, neither
// visible to the other. The inode has to outlive the holders.
func (l *ControllerRoleLease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.released = true
	if l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	return file.Close()
}

// Path reports the lock file, FOR DIAGNOSIS ONLY. It is deliberately the only
// observable this type exposes, and it answers nothing about authority: there
// is no Held, Valid or StillOwned, because an authorization question that can
// be answered without exercising authority can be answered too early.
func (l *ControllerRoleLease) Path() string {
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
