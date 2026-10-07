// Package tools is the capability broker: it validates model tool proposals
// against host grants and dispatches them to host-supplied tool
// implementations. Generic dispatch lives here; implementations live beside it.
package tools

import (
	"context"
	"encoding/json"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Tool is one implementation the broker can dispatch to.
type Tool interface {
	// Spec is what the provider sees.
	Spec() api.ToolSpec
	// Kind is the only capability kind under which the tool may run.
	Kind() api.CapabilityKind
	// Invoke runs with arguments already strictly validated against Spec and
	// with the grant the broker selected. It must recheck its own guards at
	// side-effect time (paths, preconditions) rather than trust the broker.
	Invoke(ctx context.Context, inv Invocation) (api.ToolResult, error)
}

// Invocation is everything a tool may use for one call.
type Invocation struct {
	Call      api.ToolCall
	Grant     api.Capability
	Arguments json.RawMessage
	Artifacts api.ArtifactStore
	// Producer binds artifacts the tool records ("<execution>/<attempt>/<call>").
	Producer string
	// OutputLimit bounds ToolResult.Output in bytes; full bytes go to an artifact.
	OutputLimit int
}

// Broker owns registration and proposal validation.
type Broker struct {
	tools map[string]Tool
	order []string
}

// NewBroker registers tools; duplicate or invalid names are refused.
func NewBroker(tools ...Tool) (*Broker, error) { panic("lane B") }

// Specs returns the specs of tools runnable under at least one grant in mode,
// in stable order. A tool with no matching grant is not offered at all.
func (b *Broker) Specs(grants []api.Capability, mode api.Mode) []api.ToolSpec { panic("lane B") }

// Env is per-execution dispatch configuration.
type Env struct {
	Grants      []api.Capability
	Mode        api.Mode
	Artifacts   api.ArtifactStore
	Producer    string
	OutputLimit int
}

// Dispatch validates and runs one proposal. A refusal (unknown tool, no
// matching grant, invalid arguments, mode violation) returns status refused
// and never reaches the tool. err is non-nil only when the outcome of a side
// effect is unknown (for example a recording failure after a mutation); the
// engine then stops further side effects.
func (b *Broker) Dispatch(ctx context.Context, call api.ToolCall, env Env) (api.ToolResult, error) {
	panic("lane B")
}
