package runtime

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

type contextErrorDoer struct{}

func (contextErrorDoer) Do(req *http.Request) (*http.Response, error) {
	return nil, &url.Error{Op: req.Method, URL: req.URL.String(), Err: req.Context().Err()}
}

func TestGitHubRESTDoRawPreservesCallerLocalError(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		name := "canceled"
		if deadline {
			name = "deadline exceeded"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if deadline {
				ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
				defer cancel()
			}
			adapter := GitHubRESTAdapter{
				HTTP: contextErrorDoer{}, Credentials: staticCredential{secret: testToken},
			}
			status, headers, body, err := adapter.doRaw(ctx, testRepo, http.MethodGet, "/repos/zenchron/fixture", nil, nil, nil)
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("doRaw error = %v, want cause %v", err, ctx.Err())
			}
			if !callerLocalError(err) {
				t.Fatalf("transport error must remain caller-local: %v", err)
			}
			if status != 0 || headers != nil || body != nil {
				t.Fatalf("transport failure returned response data: %d, %v, %q", status, headers, body)
			}
			if !strings.Contains(err.Error(), ctx.Err().Error()) {
				t.Fatalf("transport diagnostic lost its cause: %v", err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("transport diagnostic contains the credential")
			}
		})
	}
}

// scriptedTransportDoer answers every request with the same transport-level
// error, modelling a host that cannot reach GitHub at all rather than one
// GitHub answered badly.
type scriptedTransportDoer struct{ err error }

func (d scriptedTransportDoer) Do(*http.Request) (*http.Response, error) { return nil, d.err }

// TestGitHubRESTDoRawClassifiesRecognizedConnectivityFailures pins the #380
// boundary: a DNS resolution failure or a refused/reset/unreachable dial is a
// recognized, typed statement that the request never reached GitHub, so it
// must become a *GitHubTransientError - the same outcome a 5xx or a 429
// already produces - rather than an opaque error an observation handler
// cannot route (which is what let source.observe burn its whole attempt
// budget in the same second during the live #380 dogfood run).
//
// The set stays closed: a DNS timeout and an arbitrary unrecognized transport
// error must NOT be reclassified, because guessing "the network is the
// problem" from a condition this boundary was not told to recognize is how a
// permanent fault becomes an infinite wait.
func TestGitHubRESTDoRawClassifiesRecognizedConnectivityFailures(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		transient bool
	}{
		{
			name: "dns resolution failure",
			err: &url.Error{Op: "Get", URL: "https://api.github.com/repos/zenchron/fixture", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "api.github.com", IsNotFound: true},
			}},
			transient: true,
		},
		{
			name: "connection refused",
			err: &url.Error{Op: "Get", URL: "https://api.github.com/repos/zenchron/fixture", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
			}},
			transient: true,
		},
		{
			name: "network unreachable",
			err: &url.Error{Op: "Get", URL: "https://api.github.com/repos/zenchron/fixture", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: &os.SyscallError{Syscall: "connect", Err: syscall.ENETUNREACH},
			}},
			transient: true,
		},
		{
			name: "dns timeout stays unrecognized",
			err: &url.Error{Op: "Get", URL: "https://api.github.com/repos/zenchron/fixture", Err: &net.OpError{
				Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "i/o timeout", Name: "api.github.com", IsTimeout: true},
			}},
			transient: false,
		},
		{
			name:      "unrecognized transport error",
			err:       &url.Error{Op: "Get", URL: "https://api.github.com/repos/zenchron/fixture", Err: errors.New("boom")},
			transient: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			adapter := GitHubRESTAdapter{
				HTTP: scriptedTransportDoer{err: tc.err}, Credentials: staticCredential{secret: testToken},
			}
			_, _, _, err := adapter.doRaw(context.Background(), testRepo, http.MethodGet, "/repos/zenchron/fixture", nil, nil, nil)
			if err == nil {
				t.Fatal("doRaw returned no error for a failed transport")
			}
			var transient *GitHubTransientError
			if got := errors.As(err, &transient); got != tc.transient {
				t.Fatalf("doRaw classified transient=%v, want %v (err=%v)", got, tc.transient, err)
			}
			if strings.Contains(err.Error(), testToken) {
				t.Fatal("transport diagnostic contains the credential")
			}
		})
	}
}
