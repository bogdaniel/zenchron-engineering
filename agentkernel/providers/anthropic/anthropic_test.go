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
// kernel's Input includes cache reads and writes; the wire's input_tokens
// excludes both.
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
		for _, part := range []*int64{r.Usage.CachedInput, r.Usage.CacheWriteInput} {
			if part != nil {
				in -= *part
			}
		}
		usage["input_tokens"] = in
	}
	if r.Usage.Output != nil {
		usage["output_tokens"] = *r.Usage.Output
	}
	if r.Usage.CachedInput != nil {
		usage["cache_read_input_tokens"] = *r.Usage.CachedInput
	}
	if r.Usage.CacheWriteInput != nil {
		usage["cache_creation_input_tokens"] = *r.Usage.CacheWriteInput
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
// thinking blocks back unchanged and in order. The exact content travels in
// the transcript, so a fresh adapter instance replays it.
func TestThinkingReplayedVerbatim(t *testing.T) {
	content := `[{"type":"thinking","thinking":"","signature":"SIG=="},{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"a"}}]`
	first := `{"id":"msg_1","model":"m-1","content":` + content + `,"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":3}}`
	env := func(tr *conformance.Transport) conformance.Env {
		return conformance.Env{Transport: tr, Credentials: &conformance.Credentials{}, MaxResponseBytes: 4096}
	}
	tr1 := conformance.NewTransport(conformance.Reply{Body: []byte(first)})
	resp, err := newTarget(t, env(tr1)).Complete(context.Background(), testRequest(nil))
	if err != nil {
		t.Fatal(err)
	}
	follow := testRequest([]api.Message{
		{Role: api.RoleAssistant, ToolCalls: resp.ToolCalls, Replay: resp.Replay},
		{Role: api.RoleTool, ToolCallID: "toolu_1", Content: "ok"},
	})
	tr2 := conformance.NewTransport(conformance.Reply{Body: encode(api.ProviderResponse{Text: "done", Stop: api.StopEnd})})
	if _, err := newTarget(t, env(tr2)).Complete(context.Background(), follow); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(tr2.Sent()[0].Body, []byte(content)) {
		t.Fatalf("assistant turn not replayed verbatim:\n%s", tr2.Sent()[0].Body)
	}
}

// TestForeignReplayIgnored: Replay written by another adapter is never sent
// as Messages content.
func TestForeignReplayIgnored(t *testing.T) {
	tr := conformance.NewTransport(conformance.Reply{Body: encode(api.ProviderResponse{Text: "done", Stop: api.StopEnd})})
	call := api.ToolCall{ID: "toolu_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"a"}`)}
	foreign := json.RawMessage(`{"adapter":"openai.responses","content":[{"type":"injected"}]}`)
	req := testRequest([]api.Message{
		{Role: api.RoleAssistant, ToolCalls: []api.ToolCall{call}, Replay: foreign},
		{Role: api.RoleTool, ToolCallID: "toolu_1", Content: "ok"},
	})
	if _, err := newTarget(t, conformance.Env{Transport: tr, Credentials: &conformance.Credentials{}, MaxResponseBytes: 4096}).
		Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	body := tr.Sent()[0].Body
	if bytes.Contains(body, []byte("injected")) || !bytes.Contains(body, []byte(`"type":"tool_use","id":"toolu_1"`)) {
		t.Fatalf("foreign replay used or turn not reconstructed:\n%s", body)
	}
}

func TestUsageNormalization(t *testing.T) {
	raw := `{"id":"m","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":30,"cache_creation_input_tokens":5}}`
	resp, perr := parseResponse([]byte(raw), "b")
	if perr != nil {
		t.Fatal(perr)
	}
	if *resp.Usage.Input != 45 || *resp.Usage.CachedInput != 30 || *resp.Usage.CacheWriteInput != 5 || *resp.Usage.Output != 2 {
		t.Fatalf("usage %+v", resp.Usage)
	}
	if resp.Session == nil || !resp.Session.CacheReused {
		t.Fatal("cache reuse not observed")
	}
	oneHour := `{"id":"m","model":"x","content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2,` +
		`"cache_read_input_tokens":0,"cache_creation_input_tokens":5,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":5}}}`
	if resp, _ = parseResponse([]byte(oneHour), "b"); resp.Usage.CacheWriteInput != nil || *resp.Usage.Input != 15 {
		t.Fatalf("1h cache writes must leave the write count unknown: %+v", resp.Usage)
	}
}

func testRequest(extra []api.Message) api.ProviderRequest {
	msgs := append([]api.Message{{Role: api.RoleUser, Content: "hi"}}, extra...)
	return api.ProviderRequest{Binding: api.ProviderBinding{ID: "b", Model: "m", CredentialHandle: conformance.CredentialHandle},
		Messages: msgs, MaxOutputTokens: 8}
}
