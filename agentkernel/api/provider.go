package api

import (
	"context"
	"encoding/json"
	"fmt"
)

// Role is a transcript message role in the kernel's provider-neutral form.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one provider-neutral transcript entry.
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

// ProviderRequest is one bounded model call. It carries the full transcript:
// no adapter may depend on provider-held session state for correctness.
type ProviderRequest struct {
	Binding         ProviderBinding `json:"binding"`
	Messages        []Message       `json:"messages"`
	Tools           []ToolSpec      `json:"tools"`
	MaxOutputTokens int64           `json:"max_output_tokens"`
}

// StopReason is the provider-neutral reason a response ended.
type StopReason string

const (
	StopEnd       StopReason = "end"
	StopToolUse   StopReason = "tool_use"
	StopMaxTokens StopReason = "max_tokens"
	StopRefused   StopReason = "refused"
	StopOther     StopReason = "other"
)

// ProviderResponse is one normalized model response.
type ProviderResponse struct {
	Text      string     `json:"text,omitempty"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	Stop      StopReason `json:"stop"`
	// Usage is what the provider reported; nil counts are unknown.
	Usage                TokenUsage          `json:"usage"`
	ModelVersionObserved string              `json:"model_version_observed,omitempty"`
	Session              *SessionObservation `json:"session,omitempty"`
}

// SessionObservation reports provider-side session or prompt-cache reuse. It
// is an efficiency observation only and never correctness or authority.
type SessionObservation struct {
	ProviderID  string `json:"provider_id"`
	CacheReused bool   `json:"cache_reused"`
	// Ref is an opaque provider reference, recorded for diagnosis only. The
	// kernel never sends it back as a condition of correctness.
	Ref string `json:"ref,omitempty"`
}

// Provider is a model adapter. It performs exactly one bounded call per
// Complete and owns no retry, routing or budget decision.
type Provider interface {
	Complete(ctx context.Context, request ProviderRequest) (ProviderResponse, error)
}

// TokenEstimator is an optional provider capability for local token counts.
type TokenEstimator interface {
	EstimateTokens(text string) TokenEstimate
}

// ProviderErrorClass is the typed category of a provider failure.
type ProviderErrorClass string

const (
	ProviderTransport   ProviderErrorClass = "transport"
	ProviderRateLimited ProviderErrorClass = "rate_limited"
	ProviderUnavailable ProviderErrorClass = "unavailable"
	ProviderAuth        ProviderErrorClass = "auth"
	ProviderMalformed   ProviderErrorClass = "malformed"
	ProviderOversized   ProviderErrorClass = "oversized"
	ProviderRejected    ProviderErrorClass = "rejected"
	ProviderServer      ProviderErrorClass = "server"
	ProviderCancelled   ProviderErrorClass = "cancelled"
	ProviderDeadline    ProviderErrorClass = "deadline"
)

// ProviderError is the only error shape adapters return. Detail is redacted.
type ProviderError struct {
	Class     ProviderErrorClass `json:"class"`
	Status    int                `json:"status,omitempty"`
	Retryable bool               `json:"retryable"`
	Detail    string             `json:"detail,omitempty"`
}

func (e *ProviderError) Error() string {
	return fmt.Sprintf("provider %s (status %d): %s", e.Class, e.Status, e.Detail)
}

// Secret is a credential value that never serializes or prints.
type Secret struct{ value string }

// NewSecret wraps a credential value.
func NewSecret(value string) Secret { return Secret{value: value} }

// Reveal returns the value for use on the wire only.
func (s Secret) Reveal() string { return s.value }

func (Secret) String() string               { return "[redacted]" }
func (Secret) GoString() string             { return "[redacted]" }
func (Secret) MarshalJSON() ([]byte, error) { return json.Marshal("[redacted]") }

// CredentialSource resolves a binding's credential handle at call time. It is
// injected by the host; the kernel never looks credentials up itself.
type CredentialSource interface {
	Credential(ctx context.Context, handle string) (Secret, error)
}
