package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/conformance"
)

func newTarget(t *testing.T, env conformance.Env) api.Provider {
	p, err := New(Config{Endpoint: "https://anthropic.test/v1/messages", Doer: env.Transport,
		Credentials: env.Credentials, MaxResponseBytes: env.MaxResponseBytes})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{Wire: true, Build: newTarget, Encode: encode, Inspect: inspect})
}

var stopNames = map[api.StopReason]string{
	api.StopEnd: "end_turn", api.StopToolUse: "tool_use", api.StopMaxTokens: "max_tokens", api.StopRefused: "refusal",
}

// encode renders a normalized response in the documented Messages shape. The
// kernel's Input includes cache reads, the wire's input_tokens does not.
func encode(r api.ProviderResponse) []byte {
	content := []map[string]any{}
	if r.Text != "" {
		content = append(content, map[string]any{"type": "text", "text": r.Text})
	}
	for _, c := range r.ToolCalls {
		content = append(content, map[string]any{"type": "tool_use", "id": c.ID, "name": c.Name, "input": c.Arguments})
	}
	body := map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": r.ModelVersionObserved,
		"content": content, "stop_reason": stopNames[r.Stop], "stop_sequence": nil}
	usage := map[string]any{}
	if r.Usage.Input != nil {
		in := *r.Usage.Input
		if r.Usage.CachedInput != nil {
			in -= *r.Usage.CachedInput
		}
		usage["input_tokens"] = in
	}
	if r.Usage.Output != nil {
		usage["output_tokens"] = *r.Usage.Output
	}
	if r.Usage.CachedInput != nil {
		usage["cache_read_input_tokens"] = *r.Usage.CachedInput
		usage["cache_creation_input_tokens"] = 0
	}
	if len(usage) > 0 {
		body["usage"] = usage
	}
	data, _ := json.Marshal(body)
	return data
}

func inspect(body []byte) (conformance.Observed, error) {
	var r struct {
		Model     string `json:"model"`
		System    string `json:"system"`
		MaxTokens int64  `json:"max_tokens"`
		Tools     []struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
		} `json:"tools"`
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return conformance.Observed{}, err
	}
	obs := conformance.Observed{Model: r.Model, System: r.System, MaxOutputTokens: r.MaxTokens, StrictTools: len(r.Tools) > 0}
	for _, tl := range r.Tools {
		obs.ToolNames = append(obs.ToolNames, tl.Name)
		obs.StrictTools = obs.StrictTools && tl.Strict
	}
	for _, m := range r.Messages {
		var blocks []struct {
			Type      string `json:"type"`
			ID        string `json:"id"`
			ToolUseID string `json:"tool_use_id"`
		}
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue // plain string content
		}
		for _, b := range blocks {
			switch b.Type {
			case "tool_use":
				obs.CallIDs = append(obs.CallIDs, b.ID)
			case "tool_result":
				obs.ResultIDs = append(obs.ResultIDs, b.ToolUseID)
			}
		}
	}
	return obs, nil
}

func TestHeaders(t *testing.T) {
	tr := conformance.NewTransport(conformance.Reply{Body: encode(api.ProviderResponse{Text: "ok", Stop: api.StopEnd})})
	p := newTarget(t, conformance.Env{Transport: tr, Credentials: &conformance.Credentials{}, MaxResponseBytes: 4096})
	if _, err := p.Complete(context.Background(), testRequest(nil)); err != nil {
		t.Fatal(err)
	}
	h := tr.Sent()[0].Header
	if h.Get("X-Api-Key") != conformance.FakeKey || h.Get("Anthropic-Version") != APIVersion {
		t.Fatalf("headers %v", h)
	}
}

// TestThinkingReplayedVerbatim: within a tool-use turn the API requires
// thinking blocks back unchanged and in order.
func TestThinkingReplayedVerbatim(t *testing.T) {
	content := `[{"type":"thinking","thinking":"","signature":"SIG=="},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a"}}]`
	first := `{"id":"msg_1","model":"m-1","content":` + content + `,"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
	second := encode(api.ProviderResponse{Text: "done", Stop: api.StopEnd})
	tr := conformance.NewTransport(conformance.Reply{Body: []byte(first)}, conformance.Reply{Body: second})
	p := newTarget(t, conformance.Env{Transport: tr, Credentials: &conformance.Credentials{}, MaxResponseBytes: 4096})
	resp, err := p.Complete(context.Background(), testRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	follow := testRequest([]api.Message{
		{Role: api.RoleAssistant, ToolCalls: resp.ToolCalls},
		{Role: api.RoleTool, ToolCallID: "toolu_1", Content: "ok"},
	})
	if _, err := p.Complete(context.Background(), follow); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(tr.Sent()[1].Body, []byte(content)) {
		t.Fatalf("assistant turn not replayed verbatim:\n%s", tr.Sent()[1].Body)
	}
}

func TestUsageNormalization(t *testing.T) {
	raw := `{"id":"m","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`
	resp, _, perr := parseResponse([]byte(raw), "b")
	if perr != nil {
		t.Fatal(perr)
	}
	if *resp.Usage.Input != 45 || *resp.Usage.CachedInput != 30 || *resp.Usage.Output != 2 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.Session == nil || !resp.Session.CacheReused {
		t.Fatal("cache reuse not observed")
	}
}

func testRequest(extra []api.Message) api.ProviderRequest {
	msgs := append([]api.Message{{Role: api.RoleUser, Content: "hi"}}, extra...)
	return api.ProviderRequest{Binding: api.ProviderBinding{ID: "b", Model: "m", CredentialHandle: conformance.CredentialHandle},
		Messages: msgs, MaxOutputTokens: 8}
}
