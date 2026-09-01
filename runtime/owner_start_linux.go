//go:build linux

package runtime

// processStartToken reuses the /proc starttime field that already provides
// non-reusable process identity for owned-process tracking.
func processStartToken(pid int) (string, bool) { return linuxProcessStartTime(pid) }
