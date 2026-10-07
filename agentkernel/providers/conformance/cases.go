package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func testToolRoundTrip(t *testing.T, target Target) {
	call := api.ToolCall{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a.go"}`)}
	first := api.ProviderResponse{
		Text: "reading", ToolCalls: []api.ToolCall{call}, Stop: api.StopToolUse, ModelVersionObserved: Model + "-v1",
		Usage: api.TokenUsage{Input: api.Count(100), Output: api.Count(20), CachedInput: api.Count(0)},
	}
	second := api.ProviderResponse{Text: "done", Stop: api.StopEnd, ModelVersionObserved: Model + "-v1",
		Usage: api.TokenUsage{Input: api.Count(150), Output: api.Count(5), CachedInput: api.Count(0)}}
	p, env := build(t, target, Exchange{Response: &first}, Exchange{Response: &second})

	req := baseRequest()
	got, perr := complete(t, p, req)
	if perr != nil {
		t.Fatalf("first call: %v", perr)
	}
	if got.Stop != api.StopToolUse || len(got.ToolCalls) != 1 || got.ToolCalls[0].ID != call.ID || got.ToolCalls[0].Name != call.Name {
		t.Fatalf("tool call not round-tripped: %+v", got)
	}
	if !jsonEqual(got.ToolCalls[0].Arguments, call.Arguments) {
		t.Fatalf("arguments %s, want %s", got.ToolCalls[0].Arguments, call.Arguments)
	}
	wantUsage(t, got.Usage, first.Usage)
	if got.ModelVersionObserved != first.ModelVersionObserved {
		t.Fatalf("model observed %q, want %q", got.ModelVersionObserved, first.ModelVersionObserved)
	}

	req.Messages = append(req.Messages,
		api.Message{Role: api.RoleAssistant, Content: got.Text, ToolCalls: got.ToolCalls},
		api.Message{Role: api.RoleTool, ToolCallID: call.ID, Content: `{"status":"ok","output":"package a"}`})
	got, perr = complete(t, p, req)
	if perr != nil {
		t.Fatalf("second call: %v", perr)
	}
	if got.Stop != api.StopEnd || got.Text != "done" || len(got.ToolCalls) != 0 {
		t.Fatalf("final response %+v", got)
	}
	sent := bodies(target, p, env)
	if len(sent) != 2 {
		t.Fatalf("sent %d requests, want 2", len(sent))
	}
	obs := inspect(t, target, sent[1])
	if obs.Model != Model || obs.System != "host instruction" || obs.MaxOutputTokens != 256 {
		t.Fatalf("request mapping: %+v", obs)
	}
	if !obs.StrictTools || !slices.Equal(obs.ToolNames, []string{"read_file"}) {
		t.Fatalf("tools not sent strictly: %+v", obs)
	}
	if !slices.Equal(obs.CallIDs, []string{"call_1"}) || !slices.Equal(obs.ResultIDs, []string{"call_1"}) {
		t.Fatalf("full transcript not replayed: %+v", obs)
	}
}

func testUsageUnknown(t *testing.T, target Target) {
	none := api.ProviderResponse{Text: "x", Stop: api.StopEnd}
	partial := api.ProviderResponse{Text: "y", Stop: api.StopEnd, Usage: api.TokenUsage{Input: api.Count(7)}}
	p, _ := build(t, target, Exchange{Response: &none}, Exchange{Response: &partial})
	got, perr := complete(t, p, baseRequest())
	if perr != nil {
		t.Fatal(perr)
	}
	wantUsage(t, got.Usage, api.TokenUsage{})
	got, perr = complete(t, p, baseRequest())
	if perr != nil {
		t.Fatal(perr)
	}
	if got.Usage.Input == nil || *got.Usage.Input != 7 || got.Usage.Output != nil {
		t.Fatalf("partial usage must keep unknown output nil, got %s", fmtUsage(got.Usage))
	}
}

func testOutputLimit(t *testing.T, target Target) {
	resp := api.ProviderResponse{Text: "trunc", Stop: api.StopMaxTokens, Usage: api.TokenUsage{Input: api.Count(9), Output: api.Count(256)}}
	p, env := build(t, target, Exchange{Response: &resp})
	got, perr := complete(t, p, baseRequest())
	if perr != nil {
		t.Fatal(perr)
	}
	if got.Stop != api.StopMaxTokens || got.Text != "trunc" {
		t.Fatalf("got stop %q text %q, want max_tokens", got.Stop, got.Text)
	}
	if obs := inspect(t, target, bodies(target, p, env)[0]); obs.MaxOutputTokens != 256 {
		t.Fatalf("max output tokens sent %d, want 256", obs.MaxOutputTokens)
	}
}

func testRefusal(t *testing.T, target Target) {
	resp := api.ProviderResponse{Text: "no", Stop: api.StopRefused}
	p, _ := build(t, target, Exchange{Response: &resp})
	got, perr := complete(t, p, baseRequest())
	if perr != nil {
		t.Fatal(perr)
	}
	if got.Stop != api.StopRefused {
		t.Fatalf("stop %q, want refused", got.Stop)
	}
}

// testCancellation cancels while the call is in flight. Complete must return
// promptly with the cancelled class and leave nothing running.
func testCancellation(t *testing.T, target Target) {
	p, env := build(t, target, Exchange{Reply: Reply{Block: true}})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := p.Complete(ctx, baseRequest())
		done <- err
	}()
	if target.Wire {
		waitFor(t, env.Transport.Entered, "call to reach the transport")
	}
	cancel()
	select {
	case err := <-done:
		wantClass(t, providerError(t, err), api.ProviderCancelled, false)
	case <-time.After(waitBound):
		t.Fatal("Complete did not return after cancellation")
	}
}

func testDeadline(t *testing.T, target Target) {
	p, _ := build(t, target, Exchange{Reply: Reply{Block: true}})
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := p.Complete(ctx, baseRequest())
	if err == nil {
		t.Fatal("expired deadline returned success")
	}
	wantClass(t, providerError(t, err), api.ProviderDeadline, false)
}

func testCredentialAtCallTime(t *testing.T, target Target) {
	resp := api.ProviderResponse{Text: "ok", Stop: api.StopEnd}
	p, env := build(t, target, Exchange{Response: &resp}, Exchange{Response: &resp})
	for range 2 {
		if _, perr := complete(t, p, baseRequest()); perr != nil {
			t.Fatal(perr)
		}
	}
	if env.Credentials.Calls != 2 {
		t.Fatalf("credential resolved %d times for 2 calls; must resolve per call", env.Credentials.Calls)
	}
	for _, s := range env.Transport.Sent() {
		if bytes.Contains(s.Body, []byte(FakeKey)) {
			t.Fatal("credential appears in the request body")
		}
		if !headerCarries(s.Header, FakeKey) {
			t.Fatal("credential not sent in a header")
		}
	}
}

func testCredentialFailure(t *testing.T, target Target) {
	p, env := build(t, target)
	env.Credentials.Fail = true
	_, perr := complete(t, p, baseRequest())
	wantClass(t, perr, api.ProviderAuth, false)
	if len(env.Transport.Sent()) != 0 {
		t.Fatal("request sent without a credential")
	}
}

func testMalformed(t *testing.T, target Target) {
	for name, body := range map[string]string{
		"partial":  `{"id":"x","model":"m","conte`,
		"not_json": `<html>gateway</html>`,
		"empty":    ``,
		"wrong":    `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			p, _ := build(t, target, Exchange{Reply: Reply{Body: []byte(body)}})
			_, perr := complete(t, p, baseRequest())
			wantClass(t, perr, api.ProviderMalformed, false)
		})
	}
}

func testOversized(t *testing.T, target Target) {
	body := append([]byte(`{"pad":"`), bytes.Repeat([]byte("x"), MaxResponseBytes)...)
	p, _ := build(t, target, Exchange{Reply: Reply{Body: append(body, `"}`...)}})
	_, perr := complete(t, p, baseRequest())
	wantClass(t, perr, api.ProviderOversized, false)
}

func testTransportFailure(t *testing.T, target Target) {
	p, _ := build(t, target, Exchange{Reply: Reply{Err: errors.New("dial tcp: connection reset; auth=" + FakeKey)}})
	_, perr := complete(t, p, baseRequest())
	wantClass(t, perr, api.ProviderTransport, true)
}

func testStatusClassification(t *testing.T, target Target) {
	cases := []struct {
		status    int
		class     api.ProviderErrorClass
		retryable bool
	}{
		{http.StatusBadRequest, api.ProviderRejected, false},
		{http.StatusUnauthorized, api.ProviderAuth, false},
		{http.StatusForbidden, api.ProviderAuth, false},
		{http.StatusRequestEntityTooLarge, api.ProviderRejected, false},
		{http.StatusTooManyRequests, api.ProviderRateLimited, true},
		{http.StatusInternalServerError, api.ProviderServer, true},
		{http.StatusServiceUnavailable, api.ProviderUnavailable, true},
		{529, api.ProviderUnavailable, true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.status), func(t *testing.T) {
			body := `{"type":"error","error":{"type":"some_error","message":"bad key ` + FakeKey + `"}}`
			p, _ := build(t, target, Exchange{Reply: Reply{Status: c.status, Body: []byte(body)}})
			_, perr := complete(t, p, baseRequest())
			wantClass(t, perr, c.class, c.retryable)
			if perr.Status != c.status || !strings.Contains(perr.Detail, "[redacted]") {
				t.Fatalf("status %d detail %q: want status kept and key redacted", perr.Status, perr.Detail)
			}
		})
	}
}

func inspect(t *testing.T, target Target, body []byte) Observed {
	t.Helper()
	obs, err := target.Inspect(body)
	if err != nil {
		t.Fatalf("inspect sent request: %v", err)
	}
	return obs
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(waitBound):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func headerCarries(h http.Header, secret string) bool {
	for _, values := range h {
		for _, v := range values {
			if strings.Contains(v, secret) {
				return true
			}
		}
	}
	return false
}

func jsonEqual(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	ja, _ := json.Marshal(x)
	jb, _ := json.Marshal(y)
	return bytes.Equal(ja, jb)
}

func wantUsage(t *testing.T, got, want api.TokenUsage) {
	t.Helper()
	if fmtUsage(got) != fmtUsage(want) {
		t.Fatalf("usage %s, want %s", fmtUsage(got), fmtUsage(want))
	}
}

func fmtUsage(u api.TokenUsage) string {
	f := func(p *int64) string {
		if p == nil {
			return "unknown"
		}
		return fmt.Sprint(*p)
	}
	return fmt.Sprintf("in=%s out=%s cached=%s", f(u.Input), f(u.Output), f(u.CachedInput))
}
