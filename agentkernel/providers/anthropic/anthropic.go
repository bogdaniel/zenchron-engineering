// Package anthropic adapts the Anthropic Messages API (POST /v1/messages) to
// the kernel's Provider port.
//
// Wire shapes follow primary documentation retrieved 2026-10-07
// (docs.anthropic.com now redirects to platform.claude.com):
//   - https://platform.claude.com/docs/en/api/messages
//   - https://platform.claude.com/docs/en/api/errors
//   - https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons
//   - https://platform.claude.com/docs/en/build-with-claude/thinking
//   - https://platform.claude.com/docs/en/build-with-claude/prompt-caching
//
// API version header: anthropic-version: 2023-06-01. Every call sends the
// full transcript; no server-side conversation state is used. No model name
// or price is assumed: both come from the host's ProviderBinding.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/internal/wire"
)

// APIVersion is the anthropic-version header value the wire shapes follow.
const APIVersion = "2023-06-01"

// replayTag marks Replay this adapter wrote. Within a tool-use turn the
// Messages API requires thinking blocks back unchanged and in order, so the
// exact assistant content travels in api.Message.Replay; Replay carrying any
// other tag came from a different adapter and is ignored.
const replayTag = "anthropic.messages/" + APIVersion

type replay struct {
	Adapter string          `json:"adapter"`
	Content json.RawMessage `json:"content"`
}

// Config wires one adapter instance. Every field is required.
type Config struct {
	// Endpoint is the full Messages URL, e.g. "https://api.anthropic.com/v1/messages".
	Endpoint string
	Doer     wire.Doer
	// Credentials resolves the binding's CredentialHandle on every call.
	Credentials      api.CredentialSource
	MaxResponseBytes int64
}

// Provider is the Messages API adapter.
type Provider struct {
	cfg Config
}

// New validates cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" || cfg.Doer == nil || cfg.Credentials == nil || cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("anthropic: endpoint, doer, credentials and max response bytes are required")
	}
	return &Provider{cfg: cfg}, nil
}

// Complete performs one bounded Messages call.
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
		Header:           http.Header{"X-Api-Key": {secret}, "Anthropic-Version": {APIVersion}},
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
	Model     string    `json:"model"`
	MaxTokens int64     `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
}

type message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolUseBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type toolResultBlock struct {
	Type      string `json:"type"`
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

// buildRequest maps the neutral transcript. Consecutive tool messages become
// one user turn of tool_result blocks, as the Messages API expects results
// for one assistant turn in the next user turn.
func buildRequest(req api.ProviderRequest) request {
	out := request{Model: req.Binding.Model, MaxTokens: req.MaxOutputTokens, Messages: []message{}}
	var system []string
	var results []any
	flush := func() {
		if len(results) > 0 {
			out.Messages = append(out.Messages, message{Role: "user", Content: results})
			results = nil
		}
	}
	for _, m := range req.Messages {
		if m.Role == api.RoleTool {
			results = append(results, toolResultBlock{Type: "tool_result", ToolUseID: m.ToolCallID, Content: m.Content, IsError: m.IsError})
			continue
		}
		flush()
		switch m.Role {
		case api.RoleSystem:
			system = append(system, m.Content)
		case api.RoleUser:
			out.Messages = append(out.Messages, message{Role: "user", Content: m.Content})
		case api.RoleAssistant:
			out.Messages = append(out.Messages, message{Role: "assistant", Content: assistantContent(m)})
		}
	}
	flush()
	out.System = strings.Join(system, "\n\n")
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Strict: true})
	}
	return out
}

func assistantContent(m api.Message) any {
	var r replay
	if json.Unmarshal(m.Replay, &r) == nil && r.Adapter == replayTag && len(r.Content) > 0 {
		return r.Content
	}
	var blocks []any
	if m.Content != "" {
		blocks = append(blocks, textBlock{Type: "text", Text: m.Content})
	}
	for _, c := range m.ToolCalls {
		blocks = append(blocks, toolUseBlock{Type: "tool_use", ID: c.ID, Name: c.Name, Input: c.Arguments})
	}
	return blocks
}

type response struct {
	ID         string          `json:"id"`
	Model      string          `json:"model"`
	Content    json.RawMessage `json:"content"`
	StopReason *string         `json:"stop_reason"`
	Usage      *struct {
		InputTokens              *int64 `json:"input_tokens"`
		OutputTokens             *int64 `json:"output_tokens"`
		CacheReadInputTokens     *int64 `json:"cache_read_input_tokens"`
		CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens"`
		CacheCreation            *struct {
			Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
		} `json:"cache_creation"`
	} `json:"usage"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// parseResponse normalizes a response. A turn carrying thinking blocks
// alongside tool_use keeps its exact content as Replay.
func parseResponse(raw []byte, providerID string) (api.ProviderResponse, *api.ProviderError) {
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return api.ProviderResponse{}, wire.Malformed("decode response: %v", err)
	}
	var blocks []block
	if err := json.Unmarshal(r.Content, &blocks); err != nil || r.StopReason == nil {
		return api.ProviderResponse{}, wire.Malformed("response needs a content array and stop_reason")
	}
	out := api.ProviderResponse{ModelVersionObserved: r.Model, Stop: stopReason(*r.StopReason)}
	thinking := false
	var text strings.Builder
	for _, b := range blocks {
		switch b.Type {
		case "text":
			text.WriteString(b.Text)
		case "tool_use":
			if b.ID == "" || b.Name == "" || !wire.ValidArguments(b.Input) {
				return api.ProviderResponse{}, wire.Malformed("tool_use needs id, name and object input")
			}
			out.ToolCalls = append(out.ToolCalls, api.ToolCall{ID: b.ID, Name: b.Name, Arguments: b.Input})
		case "thinking", "redacted_thinking":
			thinking = true
		}
	}
	out.Text = text.String()
	out.Usage = usage(r)
	if c := out.Usage.CachedInput; c != nil && *c > 0 {
		out.Session = &api.SessionObservation{ProviderID: providerID, CacheReused: true, Ref: r.ID}
	}
	if thinking && len(out.ToolCalls) > 0 {
		data, err := json.Marshal(replay{Adapter: replayTag, Content: r.Content})
		if err != nil {
			return api.ProviderResponse{}, wire.Malformed("encode replay: %v", err)
		}
		out.Replay = data
	}
	return out, nil
}

// usage normalizes to the kernel's convention that Input counts every input
// token. The Messages API's input_tokens, cache_read_input_tokens and
// cache_creation_input_tokens are mutually exclusive and additive, so Input is
// their sum; CachedInput is the cache reads and CacheWriteInput the cache
// writes. Writes at the 1-hour TTL are billed at a different rate than 5-minute
// writes, which one rate card cannot price, so their presence leaves
// CacheWriteInput unknown. An absent cache field adds nothing to Input and
// leaves its own count unknown.
func usage(r response) api.TokenUsage {
	if r.Usage == nil {
		return api.TokenUsage{}
	}
	u := api.TokenUsage{
		Output: r.Usage.OutputTokens, CachedInput: r.Usage.CacheReadInputTokens, CacheWriteInput: r.Usage.CacheCreationInputTokens,
	}
	if c := r.Usage.CacheCreation; c != nil && c.Ephemeral1hInputTokens != nil && *c.Ephemeral1hInputTokens > 0 {
		u.CacheWriteInput = nil
	}
	if r.Usage.InputTokens == nil {
		return u
	}
	// Input is the sum of input_tokens and both cache parts. A negative
	// summand or a sum past int64 is no count at all: input and the parts
	// become unknown rather than an understated (or wrapped) total.
	total := int64(0)
	for _, n := range []*int64{r.Usage.InputTokens, r.Usage.CacheReadInputTokens, r.Usage.CacheCreationInputTokens} {
		if n == nil {
			continue
		}
		if *n < 0 || total > math.MaxInt64-*n {
			u.CachedInput, u.CacheWriteInput = nil, nil
			return u
		}
		total += *n
	}
	u.Input = &total
	return u
}

func stopReason(s string) api.StopReason {
	switch s {
	case "end_turn":
		return api.StopEnd
	case "tool_use":
		return api.StopToolUse
	// model_context_window_exceeded also ends output at a token bound.
	case "max_tokens", "model_context_window_exceeded":
		return api.StopMaxTokens
	case "refusal":
		return api.StopRefused
	default: // stop_sequence, pause_turn and values added later
		return api.StopOther
	}
}
