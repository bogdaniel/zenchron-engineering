// Package wire holds the HTTP mechanics every model API adapter shares:
// the transport boundary, bounded response reads, status classification,
// cancellation mapping and credential redaction. Wire formats stay in the
// adapters; only knowledge common to all of them lives here.
package wire

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Doer is the network boundary an adapter calls. *http.Client satisfies it;
// tests inject a fake so no test makes a network call.
type Doer interface {
	Do(request *http.Request) (*http.Response, error)
}

// maxDetailBytes bounds provider error text carried into a ProviderError.
const maxDetailBytes = 512

// Call is one JSON POST.
type Call struct {
	Endpoint string
	Header   http.Header
	Body     []byte
	// MaxResponseBytes bounds the body read; a larger body is ProviderOversized.
	MaxResponseBytes int64
	// Secret is redacted from every error detail this call produces.
	Secret string
}

// Post sends the call and returns the HTTP status and the bounded body.
// Every failure is already a typed, redacted ProviderError.
func Post(ctx context.Context, doer Doer, call Call) (int, []byte, *api.ProviderError) {
	if err := contextError(ctx); err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, call.Endpoint, bytes.NewReader(call.Body))
	if err != nil {
		return 0, nil, &api.ProviderError{Class: api.ProviderRejected, Detail: Redact("build request: "+err.Error(), call.Secret)}
	}
	req.Header = call.Header.Clone()
	req.Header.Set("Content-Type", "application/json")
	resp, err := doer.Do(req)
	if err != nil {
		if cerr := contextError(ctx); cerr != nil {
			return 0, nil, cerr
		}
		return 0, nil, &api.ProviderError{Class: api.ProviderTransport, Retryable: true, Detail: Redact(err.Error(), call.Secret)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, call.MaxResponseBytes+1))
	if err != nil {
		if cerr := contextError(ctx); cerr != nil {
			return 0, nil, cerr
		}
		return 0, nil, &api.ProviderError{Class: api.ProviderTransport, Status: resp.StatusCode, Retryable: true,
			Detail: Redact("read body: "+err.Error(), call.Secret)}
	}
	if int64(len(body)) > call.MaxResponseBytes {
		return 0, nil, &api.ProviderError{Class: api.ProviderOversized, Status: resp.StatusCode,
			Detail: fmt.Sprintf("response body exceeds %d bytes", call.MaxResponseBytes)}
	}
	return resp.StatusCode, body, nil
}

// contextError maps a finished context to the typed cancellation classes.
func contextError(ctx context.Context) *api.ProviderError {
	switch {
	case ctx.Err() == nil:
		return nil
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return &api.ProviderError{Class: api.ProviderDeadline, Detail: "deadline exceeded"}
	default:
		return &api.ProviderError{Class: api.ProviderCancelled, Detail: "request cancelled"}
	}
}

// Classify turns a non-2xx response into a typed error. Retryable marks only
// transient classes; the engine decides whether any retry budget remains.
func Classify(status int, body []byte, secret string) *api.ProviderError {
	e := &api.ProviderError{Status: status, Detail: Redact(errorMessage(body), secret)}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		e.Class = api.ProviderAuth
	case status == http.StatusTooManyRequests:
		e.Class, e.Retryable = api.ProviderRateLimited, true
	// 529 is Anthropic's overloaded_error; 503 is OpenAI's overloaded signal.
	case status == 529 || status == http.StatusServiceUnavailable:
		e.Class, e.Retryable = api.ProviderUnavailable, true
	case status >= 500:
		e.Class, e.Retryable = api.ProviderServer, true
	default:
		e.Class = api.ProviderRejected
	}
	return e
}

// errorMessage extracts error.type and error.message, the shape both OpenAI
// and Anthropic document, falling back to a bounded raw excerpt.
func errorMessage(body []byte) string {
	var shaped struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	text := string(body)
	if json.Unmarshal(body, &shaped) == nil && shaped.Error.Message != "" {
		text = strings.TrimSpace(shaped.Error.Type + ": " + shaped.Error.Message)
	}
	if len(text) > maxDetailBytes {
		text = text[:maxDetailBytes] + "...(truncated)"
	}
	return text
}

// Redact removes secret from s. Redaction happens before a detail is stored,
// so no error, result or log can carry the credential value.
func Redact(s, secret string) string {
	if secret == "" {
		return s
	}
	return strings.ReplaceAll(s, secret, "[redacted]")
}

// Credential resolves a binding's credential at call time. The source's own
// error text is not carried: it is host-owned and may echo what it resolved.
func Credential(ctx context.Context, source api.CredentialSource, handle string) (string, *api.ProviderError) {
	if handle == "" || source == nil {
		return "", &api.ProviderError{Class: api.ProviderAuth, Detail: "binding has no resolvable credential handle"}
	}
	secret, err := source.Credential(ctx, handle)
	if err != nil {
		if cerr := contextError(ctx); cerr != nil {
			return "", cerr
		}
		return "", &api.ProviderError{Class: api.ProviderAuth, Detail: "credential source failed for handle " + handle}
	}
	if secret.Reveal() == "" {
		return "", &api.ProviderError{Class: api.ProviderAuth, Detail: "credential for handle " + handle + " is empty"}
	}
	return secret.Reveal(), nil
}

// Malformed builds the error for a 2xx body that does not match the contract.
func Malformed(format string, args ...any) *api.ProviderError {
	return &api.ProviderError{Class: api.ProviderMalformed, Status: http.StatusOK, Detail: fmt.Sprintf(format, args...)}
}

// ValidArguments reports whether raw is a JSON object, the only argument
// shape a strict tool schema admits.
func ValidArguments(raw []byte) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
