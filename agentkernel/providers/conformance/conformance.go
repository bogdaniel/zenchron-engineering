// Package conformance is the provider conformance suite shared by every
// adapter: strict tool round-trips, malformed and partial responses,
// cancellation, deadlines, usage unknowns, output limits, credential
// redaction, transport failures, oversized bodies and status classification.
// It runs offline against a fake transport; adapters supply only how a
// normalized response looks on their wire and how to read back what they sent.
package conformance

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Fixed values the suite asserts on.
const (
	FakeKey          = "sk-conformance-SECRET-7f3a9c"
	CredentialHandle = "conformance-key"
	MaxResponseBytes = 4096
	Model            = "conformance-model"
	// waitBound bounds every wait on an in-flight call; it is a failure
	// detector, never an ordering mechanism.
	waitBound = 5 * time.Second
)

// Exchange is one scripted call: a normalized response the target encodes,
// or a raw transport reply.
type Exchange struct {
	Response *api.ProviderResponse
	Reply    Reply
}

// Env is what Build receives.
type Env struct {
	// Transport replays Script for wire targets.
	Transport *Transport
	// Script is the same exchanges, for in-process targets.
	Script           []Exchange
	Credentials      *Credentials
	MaxResponseBytes int64
}

// Observed is what a target reports about one request it sent.
type Observed struct {
	Model           string
	System          string
	ToolNames       []string
	StrictTools     bool
	CallIDs         []string
	ResultIDs       []string
	MaxOutputTokens int64
}

// Target adapts one provider implementation to the suite.
type Target struct {
	// Wire is false for in-process targets; wire-only cases are skipped.
	Wire bool
	// Build returns the provider under test, wired to env.
	Build func(t *testing.T, env Env) api.Provider
	// Encode renders a normalized response as the target's 200 body (wire only).
	Encode func(resp api.ProviderResponse) []byte
	// Bodies returns the request bodies sent so far; nil uses env.Transport.
	Bodies func(p api.Provider, env Env) [][]byte
	// Inspect normalizes one sent request body.
	Inspect func(body []byte) (Observed, error)
}

// Credentials is a counting CredentialSource that always answers FakeKey.
type Credentials struct {
	Calls int
	Fail  bool
}

// Credential implements api.CredentialSource.
func (c *Credentials) Credential(_ context.Context, handle string) (api.Secret, error) {
	c.Calls++
	if c.Fail || handle != CredentialHandle {
		return api.Secret{}, &credentialError{}
	}
	return api.NewSecret(FakeKey), nil
}

// credentialError echoes the key on purpose: adapters must not carry a
// credential source's error text.
type credentialError struct{}

func (*credentialError) Error() string { return "lookup failed near " + FakeKey }

// Run executes the whole suite against target.
func Run(t *testing.T, target Target) {
	cases := []struct {
		name string
		wire bool
		run  func(*testing.T, Target)
	}{
		{"ToolRoundTrip", false, testToolRoundTrip},
		{"UsageUnknown", false, testUsageUnknown},
		{"OutputLimit", false, testOutputLimit},
		{"Refusal", false, testRefusal},
		{"Cancellation", false, testCancellation},
		{"Deadline", false, testDeadline},
		{"CredentialAtCallTime", true, testCredentialAtCallTime},
		{"CredentialFailureRedacted", true, testCredentialFailure},
		{"Malformed", true, testMalformed},
		{"Oversized", true, testOversized},
		{"TransportFailure", true, testTransportFailure},
		{"StatusClassification", true, testStatusClassification},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.wire && !target.Wire {
				t.Skip("wire-only case; target has no transport")
			}
			c.run(t, target)
		})
	}
}

func build(t *testing.T, target Target, script ...Exchange) (api.Provider, Env) {
	t.Helper()
	replies := make([]Reply, len(script))
	for i, ex := range script {
		replies[i] = ex.Reply
		if ex.Response != nil && target.Wire {
			replies[i] = Reply{Body: target.Encode(*ex.Response)}
		}
	}
	env := Env{Transport: NewTransport(replies...), Script: script, Credentials: &Credentials{}, MaxResponseBytes: MaxResponseBytes}
	return target.Build(t, env), env
}

func bodies(target Target, p api.Provider, env Env) [][]byte {
	if target.Bodies != nil {
		return target.Bodies(p, env)
	}
	var out [][]byte
	for _, s := range env.Transport.Sent() {
		out = append(out, s.Body)
	}
	return out
}

func binding() api.ProviderBinding {
	return api.ProviderBinding{
		ID: "conformance", Kind: "conformance", Model: Model, ModelVersion: api.UnknownVersion,
		ConfigFingerprint: "sha256:conformance", Eligible: true, Isolation: api.IsolationUnproven,
		ContextWindow: 100000, MaxOutputTokens: 1024, CredentialHandle: CredentialHandle,
	}
}

var readTool = api.ToolSpec{
	Name:        "read_file",
	Description: "Read one file.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`),
}

func baseRequest() api.ProviderRequest {
	return api.ProviderRequest{
		Binding: binding(),
		Messages: []api.Message{
			{Role: api.RoleSystem, Content: "host instruction"},
			{Role: api.RoleUser, Content: "do the task"},
		},
		Tools:           []api.ToolSpec{readTool},
		MaxOutputTokens: 256,
	}
}

func complete(t *testing.T, p api.Provider, req api.ProviderRequest) (api.ProviderResponse, *api.ProviderError) {
	t.Helper()
	resp, err := p.Complete(context.Background(), req)
	if err == nil {
		assertNoKey(t, resp)
		return resp, nil
	}
	return resp, providerError(t, err)
}

func providerError(t *testing.T, err error) *api.ProviderError {
	t.Helper()
	perr, ok := err.(*api.ProviderError)
	if !ok {
		t.Fatalf("error is %T (%v), want *api.ProviderError", err, err)
	}
	if strings.Contains(perr.Error(), FakeKey) {
		t.Fatalf("credential leaked into error: %q", perr.Error())
	}
	return perr
}

func assertNoKey(t *testing.T, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), FakeKey) {
		t.Fatalf("credential leaked into %T", v)
	}
}

func wantClass(t *testing.T, perr *api.ProviderError, class api.ProviderErrorClass, retryable bool) {
	t.Helper()
	if perr == nil {
		t.Fatalf("got success, want %s error", class)
	}
	if perr.Class != class || perr.Retryable != retryable {
		t.Fatalf("got %s retryable=%v (%s), want %s retryable=%v", perr.Class, perr.Retryable, perr.Detail, class, retryable)
	}
}

// InspectNeutral reads a neutral api.ProviderRequest body, the wire form of
// the local protocol and of in-process targets.
func InspectNeutral(body []byte) (Observed, error) {
	var r api.ProviderRequest
	if err := json.Unmarshal(body, &r); err != nil {
		return Observed{}, err
	}
	obs := Observed{Model: r.Binding.Model, MaxOutputTokens: r.MaxOutputTokens, StrictTools: len(r.Tools) > 0}
	for _, tl := range r.Tools {
		obs.ToolNames = append(obs.ToolNames, tl.Name)
	}
	for _, m := range r.Messages {
		switch m.Role {
		case api.RoleSystem:
			obs.System = m.Content
		case api.RoleAssistant:
			for _, c := range m.ToolCalls {
				obs.CallIDs = append(obs.CallIDs, c.ID)
			}
		case api.RoleTool:
			obs.ResultIDs = append(obs.ResultIDs, m.ToolCallID)
		}
	}
	return obs, nil
}
