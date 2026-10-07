// Package openai adapts the OpenAI Responses API (POST /v1/responses) to the
// kernel's Provider port.
//
// Wire shapes follow primary documentation retrieved 2026-10-07:
//   - https://developers.openai.com/api/reference/resources/responses/methods/create
//   - https://developers.openai.com/api/reference/resources/responses/methods/retrieve
//   - https://developers.openai.com/api/docs/guides/function-calling
//   - https://developers.openai.com/api/docs/guides/error-codes
//
// (platform.openai.com/docs returned HTTP 403 to the fetcher on that date; the
// developers.openai.com pages above are OpenAI's current reference.) The
// Responses API carries no version header. Every call sends the full
// transcript with "store": false and never previous_response_id, so no
// server-side conversation state is needed for correctness. No model name or
// price is assumed: both come from the host's ProviderBinding.
package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/internal/wire"
)

// Config wires one adapter instance. Every field is required.
type Config struct {
	// Endpoint is the full Responses URL, e.g. "https://api.openai.com/v1/responses".
	Endpoint string
	Doer     wire.Doer
	// Credentials resolves the binding's CredentialHandle on every call.
	Credentials      api.CredentialSource
	MaxResponseBytes int64
}

// Provider is the Responses API adapter.
type Provider struct{ cfg Config }

// New validates cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" || cfg.Doer == nil || cfg.Credentials == nil || cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("openai: endpoint, doer, credentials and max response bytes are required")
	}
	return &Provider{cfg: cfg}, nil
}

// Complete performs one bounded Responses call.
func (p *Provider) Complete(ctx context.Context, req api.ProviderRequest) (api.ProviderResponse, error) {
	secret, perr := wire.Credential(ctx, p.cfg.Credentials, req.Binding.CredentialHandle)
	if perr != nil {
		return api.ProviderResponse{}, perr
	}
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderRejected, Detail: "encode request: " + err.Error()}
	}
	status, raw, perr := wire.Post(ctx, p.cfg.Doer, wire.Call{
		Endpoint:         p.cfg.Endpoint,
		Header:           http.Header{"Authorization": {"Bearer " + secret}},
		Body:             body,
		MaxResponseBytes: p.cfg.MaxResponseBytes,
		Secret:           secret,
	})
	if perr != nil {
		return api.ProviderResponse{}, perr
	}
	if status < 200 || status > 299 {
		return api.ProviderResponse{}, wire.Classify(status, raw, secret)
	}
	resp, perr := parseResponse(raw, req.Binding.ID)
	if perr != nil {
		perr.Detail = wire.Redact(perr.Detail, secret)
		return api.ProviderResponse{}, perr
	}
	return resp, nil
}

type request struct {
	Model           string `json:"model"`
	Instructions    string `json:"instructions,omitempty"`
	Input           []any  `json:"input"`
	Tools           []tool `json:"tools,omitempty"`
	MaxOutputTokens int64  `json:"max_output_tokens"`
	// Store is always false and previous_response_id is never sent: the
	// transcript is complete on every call.
	Store bool `json:"store"`
}

type tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

type messageItem struct {
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type functionCallItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type functionOutputItem struct {
	Type   string `json:"type"`
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

// buildRequest maps the neutral transcript. System messages become
// instructions; the Responses API has no is_error flag on function output, so
// the engine's tool message content already states the status.
func buildRequest(req api.ProviderRequest) request {
	out := request{Model: req.Binding.Model, MaxOutputTokens: req.MaxOutputTokens, Input: []any{}}
	var system []string
	for _, m := range req.Messages {
		switch m.Role {
		case api.RoleSystem:
			system = append(system, m.Content)
		case api.RoleUser:
			out.Input = append(out.Input, messageItem{Type: "message", Role: "user", Content: m.Content})
		case api.RoleAssistant:
			if m.Content != "" {
				out.Input = append(out.Input, messageItem{Type: "message", Role: "assistant", Content: m.Content})
			}
			for _, c := range m.ToolCalls {
				out.Input = append(out.Input, functionCallItem{Type: "function_call", CallID: c.ID, Name: c.Name, Arguments: string(c.Arguments)})
			}
		case api.RoleTool:
			out.Input = append(out.Input, functionOutputItem{Type: "function_call_output", CallID: m.ToolCallID, Output: m.Content})
		}
	}
	out.Instructions = strings.Join(system, "\n\n")
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.InputSchema, Strict: true})
	}
	return out
}

type response struct {
	ID                string `json:"id"`
	Model             string `json:"model"`
	Status            string `json:"status"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Output []outputItem `json:"output"`
	Usage  *struct {
		InputTokens        *int64 `json:"input_tokens"`
		OutputTokens       *int64 `json:"output_tokens"`
		InputTokensDetails *struct {
			CachedTokens *int64 `json:"cached_tokens"`
		} `json:"input_tokens_details"`
	} `json:"usage"`
}

type outputItem struct {
	Type      string `json:"type"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

func parseResponse(raw []byte, providerID string) (api.ProviderResponse, *api.ProviderError) {
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return api.ProviderResponse{}, wire.Malformed("decode response: %v", err)
	}
	if r.Status == "failed" {
		msg := "response failed"
		if r.Error != nil {
			msg = r.Error.Code + ": " + r.Error.Message
		}
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderServer, Status: http.StatusOK, Detail: msg}
	}
	if r.Status != "completed" && r.Status != "incomplete" {
		return api.ProviderResponse{}, wire.Malformed("unexpected response status %q", r.Status)
	}
	out := api.ProviderResponse{ModelVersionObserved: r.Model}
	refused, perr := collectOutput(r.Output, &out)
	if perr != nil {
		return api.ProviderResponse{}, perr
	}
	out.Stop = stopReason(r, refused, len(out.ToolCalls) > 0)
	if r.Usage != nil {
		out.Usage = api.TokenUsage{Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens}
		if r.Usage.InputTokensDetails != nil {
			out.Usage.CachedInput = r.Usage.InputTokensDetails.CachedTokens
		}
	}
	if c := out.Usage.CachedInput; c != nil && *c > 0 {
		out.Session = &api.SessionObservation{ProviderID: providerID, CacheReused: true, Ref: r.ID}
	}
	return out, nil
}

// collectOutput gathers text and strictly validated function calls. Unknown
// item types (reasoning and others) are skipped: they carry no content the
// neutral transcript can represent and are not needed for correctness.
func collectOutput(items []outputItem, out *api.ProviderResponse) (bool, *api.ProviderError) {
	refused := false
	var text strings.Builder
	for _, it := range items {
		switch it.Type {
		case "message":
			for _, c := range it.Content {
				switch c.Type {
				case "output_text":
					text.WriteString(c.Text)
				case "refusal":
					refused = true
					text.WriteString(c.Refusal)
				}
			}
		case "function_call":
			if it.CallID == "" || it.Name == "" || !wire.ValidArguments([]byte(it.Arguments)) {
				return false, wire.Malformed("function_call needs call_id, name and object arguments")
			}
			out.ToolCalls = append(out.ToolCalls, api.ToolCall{ID: it.CallID, Name: it.Name, Arguments: json.RawMessage(it.Arguments)})
		}
	}
	out.Text = text.String()
	return refused, nil
}

func stopReason(r response, refused, hasTools bool) api.StopReason {
	switch {
	case refused:
		return api.StopRefused
	case r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "max_output_tokens":
		return api.StopMaxTokens
	case r.Status == "incomplete" && r.IncompleteDetails != nil && r.IncompleteDetails.Reason == "content_filter":
		return api.StopRefused
	case r.Status == "incomplete":
		return api.StopOther
	case hasTools:
		return api.StopToolUse
	default:
		return api.StopEnd
	}
}
