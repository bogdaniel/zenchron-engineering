package runtime

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
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
