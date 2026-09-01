//go:build !linux

package runtime

// Start-time binding is unavailable without procfs, so a recycled PID cannot be
// distinguished from the process that recorded a lease. That is exactly why the
// takeover policy stays conservative: liveness then falls back to proving the
// PID is absent, and anything short of that proof counts as alive.
func processStartToken(int) (string, bool) { return "", false }
