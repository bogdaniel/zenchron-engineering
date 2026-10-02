package runtime

import (
	"errors"
	"net"
	"syscall"
	"time"
)

// Only typed transport errors qualify here; arbitrary diagnostic prose,
// authentication failures and missing executables do not imply connectivity.
func transientConnectivity(err error) bool {
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	for _, code := range []error{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.ENETDOWN, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.ETIMEDOUT} {
		if errors.Is(err, code) {
			return true
		}
	}
	var network *net.OpError
	return errors.As(err, &network) && network.Timeout()
}

func connectivityBackoff(attempt int) time.Duration {
	delay := 30 * time.Second
	for i := 1; i < attempt && delay < 5*time.Minute; i++ {
		delay *= 2
	}
	if delay > 5*time.Minute {
		delay = 5 * time.Minute
	}
	return delay
}

// Replay, not process memory or a new observation epoch, owns the deadline.
// A succeeded retry replaces the failed operation document and clears it.
func (s *runState) connectivityRetryAt() time.Time {
	var until time.Time
	for _, op := range s.snapshot.Operations {
		if op.State == OperationFailed && op.RetryNotBefore.After(until) {
			until = op.RetryNotBefore
		}
	}
	return until
}

// A wait event advances the observation epoch but cannot mint a fresh attempt
// budget for the observation that failed.
func (s *runState) observationRetryKey(kind string) string {
	var pending *RunOperation
	for _, op := range s.snapshot.Operations {
		if op.Kind == kind && op.State == OperationFailed && !op.RetryNotBefore.IsZero() {
			if pending == nil || op.ID < pending.ID {
				copy := op
				pending = &copy
			}
		}
	}
	if pending != nil {
		return bindingOf(*pending)
	}
	return s.epochKey()
}
