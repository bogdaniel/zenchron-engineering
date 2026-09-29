//go:build dragonfly || freebsd || netbsd || openbsd || solaris

package runtime

// rootExited has no non-reaping exit probe wired on these platforms: the Go
// syscall package exposes no waitid for them. It reports false, so a context
// that fires before the reaper delivers terminates the root for the
// context's cause - the pre-#213 ordering. linux and darwin, the supported
// controller platforms, use waitid(WNOWAIT) instead.
func rootExited(int) bool { return false }
