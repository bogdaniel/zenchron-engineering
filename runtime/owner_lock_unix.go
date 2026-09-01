//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package runtime

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes a non-blocking advisory lock on an open file. The lock
// belongs to the open file description, so the kernel drops it when the holding
// process dies for any reason - normal exit, SIGKILL, panic, or power loss.
// That is the whole point: no cooperative cleanup is required for a crashed
// owner to stop being an owner.
//
// It reports false with a nil error only when another open file description
// currently holds a conflicting lock. A non-nil error means the platform could
// not answer, and every caller must then refuse to conclude anything.
func tryLockFile(file *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	switch err := syscall.Flock(int(file.Fd()), how|syscall.LOCK_NB); {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}
