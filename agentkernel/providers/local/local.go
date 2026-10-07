// Package local is the local-provider seam: an adapter for a model server
// that speaks the kernel's own neutral JSON, so a local backend can be
// attached without the kernel adopting any vendor wire format.
//
// Protocol (agentkernel.local/v0.1), one call per Complete:
//
//	POST <Endpoint>
//	Content-Type: application/json
//	Authorization: Bearer <secret>      (only when the binding names a credential handle)
//	body: api.ProviderRequest as JSON
//
//	200 body: api.ProviderResponse as JSON, decoded strictly (unknown fields
//	refused); a null usage count means unknown, never zero.
//	non-2xx: classified like any HTTP provider; {"error":{"message":...}}
//	is used for the detail when present.
//
// No real local backend ships in Gate A; the seam is exercised through the
// shared provider conformance suite with a fake transport.
package local

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/internal/wire"
)

// Config wires one adapter instance. Credentials may be nil when no binding
// served by this endpoint names a credential handle.
type Config struct {
	Endpoint         string
	Doer             wire.Doer
	Credentials      api.CredentialSource
	MaxResponseBytes int64
}

// Provider is the local-protocol adapter.
type Provider struct{ cfg Config }

// New validates cfg.
func New(cfg Config) (*Provider, error) {
	if cfg.Endpoint == "" || cfg.Doer == nil || cfg.MaxResponseBytes <= 0 {
		return nil, fmt.Errorf("local: endpoint, doer and max response bytes are required")
	}
	return &Provider{cfg: cfg}, nil
}

var knownStops = []api.StopReason{api.StopEnd, api.StopToolUse, api.StopMaxTokens, api.StopRefused, api.StopOther}

// Complete performs one bounded call.
func (p *Provider) Complete(ctx context.Context, req api.ProviderRequest) (api.ProviderResponse, error) {
	header := http.Header{}
	secret := ""
	if req.Binding.CredentialHandle != "" {
		var perr *api.ProviderError
		if secret, perr = wire.Credential(ctx, p.cfg.Credentials, req.Binding.CredentialHandle); perr != nil {
			return api.ProviderResponse{}, perr
		}
		header.Set("Authorization", "Bearer "+secret)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return api.ProviderResponse{}, &api.ProviderError{Class: api.ProviderRejected, Detail: "encode request: " + err.Error()}
	}
	status, raw, perr := wire.Post(ctx, p.cfg.Doer, wire.Call{
		Endpoint: p.cfg.Endpoint, Header: header, Body: body, MaxResponseBytes: p.cfg.MaxResponseBytes, Secret: secret,
	})
	if perr != nil {
		return api.ProviderResponse{}, perr
	}
	if status < 200 || status > 299 {
		return api.ProviderResponse{}, wire.Classify(status, raw, secret)
	}
	resp, perr := parse(raw)
	if perr != nil {
		perr.Detail = wire.Redact(perr.Detail, secret)
		return api.ProviderResponse{}, perr
	}
	return resp, nil
}

func parse(raw []byte) (api.ProviderResponse, *api.ProviderError) {
	var resp api.ProviderResponse
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&resp); err != nil {
		return api.ProviderResponse{}, wire.Malformed("decode response: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return api.ProviderResponse{}, wire.Malformed("trailing data after response")
	}
	if !slices.Contains(knownStops, resp.Stop) {
		return api.ProviderResponse{}, wire.Malformed("unknown stop %q", resp.Stop)
	}
	for _, c := range resp.ToolCalls {
		if c.ID == "" || c.Name == "" || !wire.ValidArguments(c.Arguments) {
			return api.ProviderResponse{}, wire.Malformed("tool call needs id, name and object arguments")
		}
	}
	return resp, nil
}
