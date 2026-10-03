package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
	"time"
)

// TransportCause is the typed cause of a failed transport exchange (#380).
// It is decided once, by transportCause, from typed errors only: never from
// error prose, a wrapper type, or a missing HTTP status.
type TransportCause string

const (
	// Fail closed: none of these clears by itself, or it is the caller's own.
	TransportCallerCancelled TransportCause = "caller_cancelled"
	TransportTLS             TransportCause = "tls"
	TransportProxy           TransportCause = "proxy_configuration"
	TransportDNSNotFound     TransportCause = "dns_not_found"
	TransportUnrecognized    TransportCause = "unrecognized"
	// Transport loss: the exchange never happened and may on a later probe.
	TransportDNSTemporary TransportCause = "dns_temporary"
	TransportTimeout      TransportCause = "timeout"
	TransportUnreachable  TransportCause = "unreachable"
	TransportReset        TransportCause = "reset"
	TransportRefused      TransportCause = "refused"
)

// Lost reports transport loss: the causes that earn transport_backoff.
func (c TransportCause) Lost() bool {
	switch c {
	case TransportDNSTemporary, TransportTimeout, TransportUnreachable, TransportReset, TransportRefused:
		return true
	}
	return false
}

// transportCause is the ONE transport classification. Every forge adapter maps
// its HTTP.Do error through transportFailure, and failed() reads the cause
// back through the error chain, so no adapter decides transience itself.
func transportCause(err error) TransportCause {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return TransportCallerCancelled
	}
	var verify *tls.CertificateVerificationError
	var record tls.RecordHeaderError
	var alert tls.AlertError
	var authority x509.UnknownAuthorityError
	var invalid x509.CertificateInvalidError
	var host x509.HostnameError
	if errors.As(err, &verify) || errors.As(err, &record) || errors.As(err, &alert) ||
		errors.As(err, &authority) || errors.As(err, &invalid) || errors.As(err, &host) {
		return TransportTLS
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		return TransportProxy
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		switch {
		case dns.IsNotFound:
			return TransportDNSNotFound
		case dns.IsTemporary || dns.IsTimeout:
			return TransportDNSTemporary
		}
		return TransportUnrecognized
	}
	switch {
	case errors.Is(err, syscall.ETIMEDOUT):
		return TransportTimeout
	case errors.Is(err, syscall.ENETUNREACH), errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETDOWN):
		return TransportUnreachable
	case errors.Is(err, syscall.ECONNRESET):
		return TransportReset
	case errors.Is(err, syscall.ECONNREFUSED):
		return TransportRefused
	}
	return TransportUnrecognized
}

// TransportError is a forge transport failure with its typed cause kept.
// Unwrap keeps the chain for callers that test for their own cancellation.
// Detail is the quoted diagnostic, left empty by an adapter whose request
// carries a secret it must not risk quoting (the App's signed assertion).
type TransportError struct {
	Cause  TransportCause
	Detail string
	Err    error
}

func (e *TransportError) Error() string {
	if e.Detail == "" {
		return "forge transport failure: " + string(e.Cause)
	}
	return "forge transport failure (" + string(e.Cause) + "): " + e.Detail
}
func (e *TransportError) Unwrap() error { return e.Err }

// transportFailure is how every forge adapter returns an HTTP.Do error.
func transportFailure(err error, quote bool) error {
	failure := &TransportError{Cause: transportCause(err), Err: err}
	if quote {
		failure.Detail = err.Error()
	}
	return failure
}

// RetryDisposition is the typed accounting of a failed operation's retry,
// persisted on the operation beside RetryNotBefore. RetryNotBefore is purely
// "not eligible before T"; reason, refund, external wait and observation
// binding are all read from the disposition's row in retryDispositions.
type RetryDisposition string

const (
	DispositionTransportBackoff RetryDisposition = "transport_backoff"
	// The provider waits (#87): an external prerequisite, refunded, probed at
	// a fixed durable cadence until it clears and the same run resumes.
	DispositionProviderPrerequisiteWait RetryDisposition = "provider_prerequisite_wait"
	DispositionRateLimitWait            RetryDisposition = "rate_limit_wait"
	DispositionAccountWait              RetryDisposition = "account_wait"
)

type dispositionSemantics struct {
	// SpendsAttempt: the attempt that hit the condition stays spent.
	SpendsAttempt bool
	// SpendsActiveWork: the wait is charged to the active-work budget.
	SpendsActiveWork bool
	// FiniteAttemptAuthority: retries draw on the operation's existing
	// MaxAttempts, so the observation keeps its binding across the wait and
	// the last attempt stops instead of waiting.
	FiniteAttemptAuthority bool
	// ResumeCondition is what makes the operation eligible again.
	ResumeCondition string
	// Reason is the run's waiting reason; empty means the class's stated one.
	Reason string
	// Delay is the RetryNotBefore schedule; nil sets none.
	Delay func(attempt int) time.Duration
}

// ReasonConnectivityBackoff is transport_backoff's waiting reason.
const ReasonConnectivityBackoff = "connectivity_backoff"

// retryDispositions is the ONE accounting table. docs/spec/runtime-v0.1.md
// (Retry dispositions) states the same rows.
var retryDispositions = map[RetryDisposition]dispositionSemantics{
	DispositionTransportBackoff: {
		SpendsAttempt: true, SpendsActiveWork: false, FiniteAttemptAuthority: true,
		ResumeCondition: "retry_not_before has passed", Reason: ReasonConnectivityBackoff,
		Delay: connectivityBackoff,
	},
	DispositionProviderPrerequisiteWait: {ResumeCondition: "the provider endpoint recovers on its own; probed every 5 minutes", Delay: providerWaitProbe},
	DispositionRateLimitWait:            {ResumeCondition: "the provider allowance returns; probed every 5 minutes", Delay: providerWaitProbe},
	DispositionAccountWait:              {ResumeCondition: "an operator restores the provider account; probed every 5 minutes", Delay: providerWaitProbe},
}

// providerWaitProbe is the fixed cadence of a provider wait. The attempt is
// refunded, so an attempt-keyed backoff would never grow.
func providerWaitProbe(int) time.Duration { return 5 * time.Minute }

// retryDispositionFor is the ONE owner of which failure class carries a
// disposition. Every other class keeps the route RouteFailure gives it.
func retryDispositionFor(class FailureClass) RetryDisposition {
	switch class {
	case FailureConnectivity:
		return DispositionTransportBackoff
	case FailureProviderAccountUnavailable:
		return DispositionAccountWait
	case FailureProviderQuota, FailureProviderRateLimited:
		return DispositionRateLimitWait
	case FailureProviderUnavailable:
		return DispositionProviderPrerequisiteWait
	}
	return ""
}

// A disposition's wait is external wait by its table row, not by a second list.
func init() {
	for _, s := range retryDispositions {
		if !s.SpendsActiveWork && s.Reason != "" {
			externalWaitReasons[s.Reason] = true
		}
	}
}

// waitReasonOf names the wait a not-yet-eligible operation is in: its
// wait-routed class's stated reason (the provider rows state none: quota and
// rate limit share a disposition but not an operator action), else its
// disposition's. Never its timestamp.
func waitReasonOf(op RunOperation) string {
	if class, waiting := waitRoutedFailure(op.Result); waiting {
		return waitReason(class)
	}
	if s, ok := retryDispositions[op.RetryDisposition]; ok && s.Reason != "" {
		return s.Reason
	}
	return "operation_unavailable"
}

// awaitsRetry reports a recorded disposition whose successor attempt exists.
func awaitsRetry(op RunOperation) (dispositionSemantics, bool) {
	s, ok := retryDispositions[op.RetryDisposition]
	if !ok || op.State != OperationFailed || (s.FiniteAttemptAuthority && op.Attempt >= op.MaxAttempts) {
		return dispositionSemantics{}, false
	}
	return s, true
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
