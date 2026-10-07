// Package anthropic adapts the Anthropic Messages API (POST /v1/messages) to
// the kernel's Provider port.
//
// Wire shapes follow primary documentation retrieved 2026-10-07
// (docs.anthropic.com now redirects to platform.claude.com):
//   - https://platform.claude.com/docs/en/api/messages
//   - https://platform.claude.com/docs/en/api/errors
//   - https://platform.claude.com/docs/en/build-with-claude/handling-stop-reasons
//   - https://platform.claude.com/docs/en/build-with-claude/thinking
//
// API version header: anthropic-version: 2023-06-01. Every call sends the
// full transcript; no server-side conversation state is used. No model name
// or price is assumed: both come from the host's ProviderBinding.
package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/internal/wire"
)

// APIVersion is the anthropic-version header value the wire shapes follow.
const APIVersion = "2023-06-01"

// maxReplayTurns bounds the thinking-replay cache (see Provider.replay).
const maxReplayTurns = 256

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

	// The Messages API requires that, within a tool-use turn, thinking blocks
	// are passed back unchanged and in order; the neutral api.Message has no
	// slot for opaque provider blocks. So the adapter remembers the exact
	// content of assistant turns that carried thinking, keyed by their first
	// tool_use id, and replays it verbatim. A miss (another instance, or an
	// evicted turn) degrades to a reconstructed turn the API may reject.
	// ponytail: bounded FIFO in adapter memory; an api.Message replay field
	// would remove it (proposal recorded in the lane report).
	mu     sync.Mutex
	replay map[string]json.RawMessage
	order  []string
}

// New validates cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" || cfg.Doer == nil || cfg.Credentials == nil || cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("anthropic: endpoint, doer, credentials and max response bytes are required")
	}
	return &Provider{cfg: cfg, replay: map[string]json.RawMessage{}}, nil
}

// Complete performs one bounded Messages call.
func (p *Provider) Complete(ctx context.Context, req api.ProviderRequest) (api.ProviderResponse, error) {
	secret, perr := wire.Credential(ctx, p.cfg.Credentials, req.Binding.CredentialHandle)
	if perr != nil {
		return api.ProviderResponse{}, perr
	}
	body, err := json.Marshal(p.buildRequest(req))
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
	resp, content, perr := parseResponse(raw, req.Binding.ID)
	if perr != nil {
		perr.Detail = wire.Redact(perr.Detail, secret)
		return api.ProviderResponse{}, perr
	}
	if content != nil {
		p.remember(resp.ToolCalls[0].ID, content)
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
func (p *Provider) buildRequest(req api.ProviderRequest) request {
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
			out.Messages = append(out.Messages, message{Role: "assistant", Content: p.assistantContent(m)})
		}
	}
	flush()
	out.System = strings.Join(system, "\n\n")
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema, Strict: true})
	}
	return out
}

func (p *Provider) assistantContent(m api.Message) any {
	if len(m.ToolCalls) > 0 {
		if raw, ok := p.recall(m.ToolCalls[0].ID); ok {
			return raw
		}
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

func (p *Provider) remember(key string, content json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.replay[key]; ok {
		return
	}
	if len(p.order) == maxReplayTurns {
		delete(p.replay, p.order[0])
		p.order = p.order[1:]
	}
	p.replay[key] = content
	p.order = append(p.order, key)
}

func (p *Provider) recall(key string) (json.RawMessage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	raw, ok := p.replay[key]
	return raw, ok
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
	} `json:"usage"`
}

type block struct {
	Type  string          `json:"type"`
	Text  string          `json:"text"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// parseResponse returns the normalized response and, when the turn carries
// thinking blocks alongside tool_use, the raw content to replay.
func parseResponse(raw []byte, providerID string) (api.ProviderResponse, json.RawMessage, *api.ProviderError) {
	var r response
	if err := json.Unmarshal(raw, &r); err != nil {
		return api.ProviderResponse{}, nil, wire.Malformed("decode response: %v", err)
	}
	var blocks []block
	if err := json.Unmarshal(r.Content, &blocks); err != nil || r.StopReason == nil {
		return api.ProviderResponse{}, nil, wire.Malformed("response needs a content array and stop_reason")
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
				return api.ProviderResponse{}, nil, wire.Malformed("tool_use needs id, name and object input")
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
		return out, r.Content, nil
	}
	return out, nil, nil
}

// usage normalizes to the kernel's convention that Input counts every input
// token. The Messages API reports input_tokens excluding cache reads and
// writes, so all three are summed; an absent cache field adds nothing and an
// absent cache_read leaves CachedInput unknown.
func usage(r response) api.TokenUsage {
	if r.Usage == nil {
		return api.TokenUsage{}
	}
	u := api.TokenUsage{Output: r.Usage.OutputTokens, CachedInput: r.Usage.CacheReadInputTokens}
	if r.Usage.InputTokens != nil {
		total := *r.Usage.InputTokens
		for _, extra := range []*int64{r.Usage.CacheReadInputTokens, r.Usage.CacheCreationInputTokens} {
			if extra != nil {
				total += *extra
			}
		}
		u.Input = &total
	}
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
