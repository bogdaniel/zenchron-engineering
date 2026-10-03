package runtime

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// Only typed transport failures qualify. Unknown diagnostics and permanent
// request failures must not acquire retry authority through string guessing.
func transientConnectivity(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return !dns.IsNotFound && (dns.IsTemporary || dns.IsTimeout)
	}
	for _, code := range []error{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ETIMEDOUT} {
		if errors.Is(err, code) {
			return true
		}
	}
	var forge *GitHubTransientError
	return errors.As(err, &forge)
}

func connectivityBackoff(attempt int) time.Duration {
	delay := 30 * time.Second
	for n := 1; n < attempt && delay < 5*time.Minute; n++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		return 5 * time.Minute
	}
	return delay
}
