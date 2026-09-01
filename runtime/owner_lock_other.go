//go:build !(darwin || dragonfly || freebsd || linux || netbsd || openbsd)

package runtime

import (
	"errors"
	"os"
)

// Without an OS advisory lock there is no crash-safe ownership evidence at all.
// Failing here is what keeps the takeover policy conservative on such a
// platform: an owner that cannot be probed is treated as alive, exactly as
// process_windows.go refuses its sandbox adapter rather than pretending.
func tryLockFile(*os.File, bool) (bool, error) {
	return false, errors.New("os advisory ownership lock unavailable on this platform")
}
