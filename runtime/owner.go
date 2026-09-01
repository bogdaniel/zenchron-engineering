package runtime

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// NewRuntimeOwner builds this process's lease-owner identity as
// host/pid/process-start-token. The start token binds the identity to a single
// process lifetime, so a recycled PID is never mistaken for the owner that
// recorded the lease.
func NewRuntimeOwner() string {
	token, _ := processStartToken(os.Getpid())
	return fmt.Sprintf("%s/%d/%s", ownerHost(), os.Getpid(), token)
}

func ownerHost() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	// The separator is structural; a hostname must never introduce a field.
	return strings.ReplaceAll(host, "/", "_")
}

// ProcessOwnerLiveness decides takeover eligibility from an owner identity
// produced by NewRuntimeOwner. It is deliberately one-sided: it reports alive
// unless it can positively prove the recorded process is gone. Wall-clock lease
// expiry alone never makes an owner dead, so an unparseable identity, a remote
// host, or an unreadable process all block takeover rather than allow it.
//
// StateDir selects the evidence. When it is set, the OS advisory ownership lock
// under <StateDir>/locks/runtime is the same-host death evidence: it is held
// for a runtime instance's whole lifetime and released by the kernel when that
// process dies, so it survives a hard crash where a PID probe cannot. When it
// is empty the weaker PID/start-token evidence is used, which is all a caller
// that never took a lock can offer.
type ProcessOwnerLiveness struct {
	Host     string
	StateDir string
}

func NewProcessOwnerLiveness() ProcessOwnerLiveness {
	return ProcessOwnerLiveness{Host: ownerHost()}
}

// NewLockOwnerLiveness proves death from the OS ownership lock held by every
// runtime instance that called AcquireOwnershipLock with the same state dir.
func NewLockOwnerLiveness(stateDir string) ProcessOwnerLiveness {
	return ProcessOwnerLiveness{Host: ownerHost(), StateDir: stateDir}
}

func (l ProcessOwnerLiveness) Alive(owner string) bool {
	host, pid, token, ok := parseOwner(owner)
	if !ok {
		return true
	}
	if host != l.Host {
		// Another host's process cannot be observed from here, and a lock file
		// visible on a shared filesystem says nothing about that host either.
		return true
	}
	if l.StateDir != "" {
		held, decided := ownerLockHeld(l.StateDir, owner)
		if !decided {
			return true
		}
		return held
	}
	if current, ok := processStartToken(pid); ok {
		// A different start token means the PID was recycled: the recorded
		// owner is provably gone.
		return token == "" || current == token
	}
	return processExists(pid)
}

func parseOwner(owner string) (host string, pid int, token string, ok bool) {
	parts := strings.Split(owner, "/")
	if len(parts) != 3 {
		return "", 0, "", false
	}
	pid, err := strconv.Atoi(parts[1])
	if err != nil || pid <= 0 {
		return "", 0, "", false
	}
	return parts[0], pid, parts[2], true
}

// processExists proves absence only on an explicit "no such process". Every
// other outcome, including a process owned by another user and platforms with
// no signal probe at all, is reported as alive.
func processExists(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return true
	}
	return !errors.Is(process.Signal(syscall.Signal(0)), syscall.ESRCH)
}

// OwnershipLock is the OS advisory lock a runtime instance holds for its whole
// lifetime. Hold it before recording any lease and release it on shutdown; a
// crash releases it automatically, which is what turns "the lock is acquirable"
// into proof that the recorded owner is gone.
type OwnershipLock struct {
	path string
	file *os.File
}

// AcquireOwnershipLock claims ownership for this instance and keeps the
// descriptor open until Release. It fails when another live process already
// holds the same owner identity, so it is also the startup guard against two
// runtimes sharing one identity.
func AcquireOwnershipLock(stateDir, owner string) (*OwnershipLock, error) {
	file, err := openOwnerLockFile(stateDir, owner)
	if err != nil {
		return nil, fmt.Errorf("acquiring runtime ownership lock for %q: %w", owner, err)
	}
	locked, err := tryLockFile(file, true)
	if err == nil && !locked {
		err = errors.New("the lock is already held by a live process")
	}
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquiring runtime ownership lock for %q: %w", owner, err)
	}
	return &OwnershipLock{path: ownerLockPath(stateDir, owner), file: file}, nil
}

// Path reports the lock file, for diagnosis only. Its existence is never
// evidence of anything.
func (l *OwnershipLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Release is idempotent and nil-safe so a shutdown path can defer it
// unconditionally. Closing the descriptor is what drops the kernel lock;
// removing the file afterwards is hygiene, not correctness.
func (l *OwnershipLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	err := file.Close()
	_ = os.Remove(l.path)
	return err
}

// ownerLockHeld reports whether a live process currently holds the ownership
// lock for owner. decided is false when the question could not be answered at
// all, which the caller must treat as "alive" rather than as death.
//
// The probe takes a shared lock: two schedulers probing the same owner never
// block each other, and only a genuine exclusive holder is reported as alive.
// The lock FILE is deliberately created when missing, so its presence or
// absence can never influence the answer.
func ownerLockHeld(stateDir, owner string) (held bool, decided bool) {
	file, err := openOwnerLockFile(stateDir, owner)
	if err != nil {
		return false, false
	}
	defer func() { _ = file.Close() }()
	locked, err := tryLockFile(file, false)
	if err != nil {
		return false, false
	}
	return !locked, true
}

func ownerLockPath(stateDir, owner string) string {
	// The owner identity is structurally host/pid/token; escaping collapses it
	// to a single path element that still reads back for diagnosis.
	return filepath.Join(stateDir, "locks", "runtime", url.PathEscape(owner))
}

func openOwnerLockFile(stateDir, owner string) (*os.File, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("a state dir is required")
	}
	if _, _, _, ok := parseOwner(owner); !ok {
		return nil, fmt.Errorf("%q is not a runtime owner identity", owner)
	}
	path := ownerLockPath(stateDir, owner)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
