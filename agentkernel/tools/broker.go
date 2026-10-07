// Package tools is the capability broker: it validates model tool proposals
// against host grants and dispatches them to tool implementations. Generic
// dispatch lives here; implementations live beside it. Every Tool is built by
// this package: the built-in file tools, the command tool and host tools
// (NewHostTool), whose host code is reached only through a bounded hand-off.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Tool is one implementation the broker can dispatch to. NewBroker accepts
// only tools this package built (its own concrete type; a host type
// implementing Tool, or embedding one, is refused), so the broker never
// calls host code on the kernel's goroutine: a host's own tool is a
// NewHostTool served by a host worker.
type Tool interface {
	// Spec is what the provider sees.
	Spec() api.ToolSpec
	// Kind is the only capability kind under which the tool may run.
	Kind() api.CapabilityKind
	// Invoke runs with arguments already strictly validated against Spec and
	// with the grant the broker selected. It must recheck its own guards at
	// side-effect time (paths, preconditions) rather than trust the broker.
	Invoke(ctx context.Context, inv Invocation) (api.ToolResult, error)
	// Scope names what a call would touch so the broker can select a grant
	// that covers it (every capability kind is scoped). A file tool returns
	// workspace-relative Paths; a command tool returns the Command name.
	// Scope only reads already-validated arguments; it can never create or
	// widen a grant.
	Scope(arguments json.RawMessage) (Scope, error)
}

// Scope is what one call would touch.
type Scope struct {
	Paths   []string
	Command string
}

// Invocation is everything a tool may use for one call.
type Invocation struct {
	Call      api.ToolCall
	Grant     api.Capability
	Arguments json.RawMessage
	Artifacts api.ArtifactStore
	// Producer binds artifacts the tool records and is the ID of any hand-off
	// the call makes: a kernel sequence ("<execution>/<attempt>/tool-<n>"),
	// never model output.
	Producer string
	// OutputLimit bounds ToolResult.Output in bytes; full bytes go to an artifact.
	OutputLimit int
	// StepBudget, when set, charges one step of a composite tool beyond its
	// first against the execution budget; an error stops the composition.
	StepBudget func(ctx context.Context) error
}

// Broker owns registration and proposal validation.
type Broker struct {
	tools   map[string]Tool
	order   []string
	schemas map[string]objectSchema
}

// toolNamePattern is the intersection of what provider tool APIs accept.
var toolNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// NewBroker registers tools; duplicate or invalid names are refused, as is a
// tool whose input schema is outside the subset the broker can enforce, and
// any tool this package did not build.
func NewBroker(tools ...Tool) (*Broker, error) {
	b := &Broker{tools: map[string]Tool{}, schemas: map[string]objectSchema{}}
	for i, t := range tools {
		if own, ok := t.(*tool); !ok || own == nil {
			return nil, fmt.Errorf("tools[%d]: %T is not a tool built by package tools", i, t)
		}
		spec := t.Spec()
		if !toolNamePattern.MatchString(spec.Name) {
			return nil, fmt.Errorf("tools[%d]: invalid name %q", i, spec.Name)
		}
		if _, dup := b.tools[spec.Name]; dup {
			return nil, fmt.Errorf("tools[%d]: duplicate name %q", i, spec.Name)
		}
		if !t.Kind().Known() {
			return nil, fmt.Errorf("tool %q: unknown kind %q", spec.Name, t.Kind())
		}
		schema, err := parseSchema(spec.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", spec.Name, err)
		}
		b.tools[spec.Name], b.schemas[spec.Name] = t, schema
		b.order = append(b.order, spec.Name)
	}
	return b, nil
}

// permitted is the structural mode rule: outside read_write, a mutating kind
// is never offered or run, whatever grants claim.
func permitted(kind api.CapabilityKind, mode api.Mode) bool {
	return !kind.Mutating() || mode == api.ModeReadWrite
}

// Specs returns the specs of tools runnable under at least one grant in mode,
// in stable order. A tool with no matching grant is not offered at all.
func (b *Broker) Specs(grants []api.Capability, mode api.Mode) []api.ToolSpec {
	specs := []api.ToolSpec{}
	for _, name := range b.order {
		t := b.tools[name]
		hasGrant := slices.ContainsFunc(grants, func(g api.Capability) bool { return g.Kind == t.Kind() })
		if hasGrant && permitted(t.Kind(), mode) {
			specs = append(specs, t.Spec())
		}
	}
	return specs
}

// Env is per-execution dispatch configuration.
type Env struct {
	Grants      []api.Capability
	Mode        api.Mode
	Artifacts   api.ArtifactStore
	Producer    string
	OutputLimit int
	// StepBudget is passed to every Invocation (see Invocation.StepBudget).
	StepBudget func(ctx context.Context) error
}

// Dispatch validates and runs one proposal. A refusal (unknown tool, no
// matching grant, invalid arguments, mode violation) returns status refused
// and never reaches the tool. err is non-nil only when the outcome of a side
// effect is unknown (for example a recording failure after a mutation); the
// engine then stops further side effects.
func (b *Broker) Dispatch(ctx context.Context, call api.ToolCall, env Env) (api.ToolResult, error) {
	t, grant, reason := b.admit(call, env)
	if reason != "" {
		return api.ToolResult{CallID: call.ID, Status: api.ToolRefused, Error: reason}, nil
	}
	res, err := t.Invoke(ctx, Invocation{
		Call: call, Grant: grant, Arguments: call.Arguments, Artifacts: env.Artifacts,
		Producer: env.Producer, OutputLimit: env.OutputLimit, StepBudget: env.StepBudget,
	})
	res.CallID, res.Grant = call.ID, grant.Handle
	var unknown error
	if err != nil {
		res.Status = api.ToolError
		res.Error = strings.TrimSpace(res.Error + " " + err.Error())
		if t.Kind().Mutating() || res.Mutated {
			// A write or command that failed mid-way may have changed state.
			unknown = fmt.Errorf("tool %q: outcome unknown: %w", call.Name, err)
		}
	}
	res, err = bound(ctx, res, env)
	return res, errors.Join(unknown, err)
}

// admit returns the tool and the host grant a call runs under, or the reason
// it is refused. Nothing in the call can create or widen a grant: a grant is
// only ever selected from env.Grants.
func (b *Broker) admit(call api.ToolCall, env Env) (Tool, api.Capability, string) {
	t, ok := b.tools[call.Name]
	if !ok {
		return nil, api.Capability{}, fmt.Sprintf("unknown tool %q", call.Name)
	}
	if env.OutputLimit <= 0 {
		return nil, api.Capability{}, "output limit must be positive"
	}
	if !permitted(t.Kind(), env.Mode) {
		return nil, api.Capability{}, fmt.Sprintf("tool %q needs %s, not permitted in mode %q", call.Name, t.Kind(), env.Mode)
	}
	if err := b.schemas[call.Name].validate(call.Arguments); err != nil {
		return nil, api.Capability{}, "invalid arguments: " + err.Error()
	}
	scope, err := scopeOf(t, call.Arguments)
	if err != nil {
		return nil, api.Capability{}, "invalid arguments: " + err.Error()
	}
	for _, g := range env.Grants {
		if g.Kind == t.Kind() && covers(g, scope) {
			return t, g, ""
		}
	}
	return nil, api.Capability{}, fmt.Sprintf("no %s grant covers this call", t.Kind())
}

func scopeOf(t Tool, args json.RawMessage) (Scope, error) {
	scope, err := t.Scope(args)
	if err != nil {
		return Scope{}, err
	}
	if t.Kind() == api.CapabilityCommand && scope.Command == "" {
		return Scope{}, errors.New("no command named")
	}
	if t.Kind() != api.CapabilityCommand && len(scope.Paths) == 0 {
		return Scope{}, errors.New("no path named")
	}
	return scope, nil
}

func covers(g api.Capability, scope Scope) bool {
	if g.Kind == api.CapabilityCommand {
		return slices.ContainsFunc(g.Commands, func(c api.CommandGrant) bool { return c.Name == scope.Command })
	}
	for _, p := range scope.Paths {
		if _, ok := rootFor(g, p); !ok {
			return false
		}
	}
	return true
}

// rootFor returns the granted root containing p. p must be a clean
// workspace-relative path outside repository metadata; anything else is
// contained by no root.
func rootFor(g api.Capability, p string) (string, bool) {
	if !api.ValidRelativePath(p) || isGitMetadata(p) {
		return "", false
	}
	for _, r := range g.Roots {
		if !api.ValidRelativePath(r) {
			continue
		}
		if r == "." || p == r || strings.HasPrefix(p, r+"/") {
			return r, true
		}
	}
	return "", false
}

// bound holds Output to env.OutputLimit. Whenever bytes are cut it records
// the exact full output as an artifact, unless the tool already referenced a
// superset of it. Error, exit code and Mutated pass through unchanged.
func bound(ctx context.Context, res api.ToolResult, env Env) (api.ToolResult, error) {
	if len(res.Output) <= env.OutputLimit {
		return res, nil
	}
	if res.FullOutput == nil {
		ref, err := record(ctx, env.Artifacts, env.Producer, []byte(res.Output))
		if err != nil {
			return unrecorded(res, err)
		}
		res.FullOutput = &ref
	}
	res.Output = cut(res.Output, env.OutputLimit)
	res.Truncated = true
	return res, nil
}

// unrecorded reports full output that could not be kept. After a mutation the
// outcome is unknown to the engine; otherwise the call simply failed.
func unrecorded(res api.ToolResult, err error) (api.ToolResult, error) {
	res.Output, res.Truncated, res.FullOutput = "", true, nil
	res.Status = api.ToolError
	res.Error = strings.TrimSpace(res.Error + " full output could not be recorded: " + err.Error())
	if res.Mutated {
		return res, fmt.Errorf("recording tool output after a mutation: %w", err)
	}
	return res, nil
}

func record(ctx context.Context, store api.ArtifactStore, producer string, data []byte) (api.ArtifactRef, error) {
	if store == nil {
		return api.ArtifactRef{}, errors.New("no artifact store configured")
	}
	return store.Put(ctx, api.ArtifactInput{MediaType: "text/plain; charset=utf-8", Producer: producer, Data: data})
}

// cut shortens s to at most n bytes without splitting a UTF-8 sequence.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// isGitMetadata reports whether p names or descends into a .git entry. Writes
// there (hooks, config) would execute or redirect code on the host, and reads
// expose remotes and credentials, so no grant reaches them. The comparison is
// case-insensitive because macOS and Windows file systems are.
func isGitMetadata(p string) bool {
	for _, part := range strings.Split(p, "/") {
		if strings.EqualFold(part, ".git") {
			return true
		}
	}
	return false
}
