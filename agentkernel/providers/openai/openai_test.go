package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, conformance.Target{
		Wire: true,
		Build: func(t *testing.T, env conformance.Env) api.Provider {
			p, err := New(Config{Endpoint: "https://openai.test/v1/responses", Doer: env.Transport,
				Credentials: env.Credentials, MaxResponseBytes: env.MaxResponseBytes})
			if err != nil {
				t.Fatal(err)
			}
			return p
		},
		Encode:  encode,
		Inspect: inspect,
	})
}

// encode renders a normalized response in the documented Responses shape.
func encode(r api.ProviderResponse) []byte {
	content := []map[string]any{}
	if r.Stop == api.StopRefused {
		content = append(content, map[string]any{"type": "refusal", "refusal": r.Text})
	} else if r.Text != "" {
		content = append(content, map[string]any{"type": "output_text", "text": r.Text, "annotations": []any{}})
	}
	output := []map[string]any{{"type": "message", "id": "msg_1", "role": "assistant", "status": "completed", "content": content}}
	for _, c := range r.ToolCalls {
		output = append(output, map[string]any{"type": "function_call", "id": "fc_" + c.ID, "call_id": c.ID,
			"name": c.Name, "arguments": string(c.Arguments), "status": "completed"})
	}
	body := map[string]any{"id": "resp_1", "object": "response", "model": r.ModelVersionObserved, "status": "completed", "output": output}
	if r.Stop == api.StopMaxTokens {
		body["status"] = "incomplete"
		body["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
	}
	usage := map[string]any{}
	if r.Usage.Input != nil {
		usage["input_tokens"] = *r.Usage.Input
	}
	if r.Usage.Output != nil {
		usage["output_tokens"] = *r.Usage.Output
	}
	if r.Usage.CachedInput != nil {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": *r.Usage.CachedInput}
	}
	if len(usage) > 0 {
		body["usage"] = usage
	}
	data, _ := json.Marshal(body)
	return data
}

func inspect(body []byte) (conformance.Observed, error) {
	var r struct {
		Model           string `json:"model"`
		Instructions    string `json:"instructions"`
		MaxOutputTokens int64  `json:"max_output_tokens"`
		Tools           []struct {
			Name   string `json:"name"`
			Strict bool   `json:"strict"`
		} `json:"tools"`
		Input []struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		} `json:"input"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return conformance.Observed{}, err
	}
	obs := conformance.Observed{Model: r.Model, System: r.Instructions, MaxOutputTokens: r.MaxOutputTokens, StrictTools: len(r.Tools) > 0}
	for _, tl := range r.Tools {
		obs.ToolNames = append(obs.ToolNames, tl.Name)
		obs.StrictTools = obs.StrictTools && tl.Strict
	}
	for _, it := range r.Input {
		switch it.Type {
		case "function_call":
			obs.CallIDs = append(obs.CallIDs, it.CallID)
		case "function_call_output":
			obs.ResultIDs = append(obs.ResultIDs, it.CallID)
		}
	}
	return obs, nil
}

// TestNoServerSideState pins the no-session contract: store is false and no
// previous_response_id is ever sent.
func TestNoServerSideState(t *testing.T) {
	tr := conformance.NewTransport(conformance.Reply{Body: encode(api.ProviderResponse{Text: "ok", Stop: api.StopEnd})})
	p, err := New(Config{Endpoint: "https://openai.test/v1/responses", Doer: tr, Credentials: &conformance.Credentials{}, MaxResponseBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	req := api.ProviderRequest{Binding: api.ProviderBinding{ID: "b", Model: "m", CredentialHandle: conformance.CredentialHandle},
		Messages: []api.Message{{Role: api.RoleUser, Content: "hi"}}, MaxOutputTokens: 8}
	if _, err := p.Complete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	sent := tr.Sent()[0]
	if !bytes.Contains(sent.Body, []byte(`"store":false`)) || bytes.Contains(sent.Body, []byte("previous_response_id")) {
		t.Fatalf("request depends on server state: %s", sent.Body)
	}
	if sent.Header.Get("Authorization") != "Bearer "+conformance.FakeKey {
		t.Fatal("bearer credential header missing")
	}
}

func TestFailedStatusIsServerError(t *testing.T) {
	_, perr := parseResponse([]byte(`{"id":"r","model":"m","status":"failed","error":{"code":"server_error","message":"x"},"output":[]}`), "b")
	if perr == nil || perr.Class != api.ProviderServer {
		t.Fatalf("got %v, want server error", perr)
	}
}

func TestInvalidFunctionArgumentsAreMalformed(t *testing.T) {
	raw := `{"id":"r","model":"m","status":"completed","output":[{"type":"function_call","call_id":"c","name":"n","arguments":"{\"a\":"}]}`
	_, perr := parseResponse([]byte(raw), "b")
	if perr == nil || perr.Class != api.ProviderMalformed {
		t.Fatalf("got %v, want malformed", perr)
	}
}
