// Package api is the versioned public contract of the Agent Execution Kernel:
// the request, result, event and port types a host uses to run one bounded
// execution. It carries no engineering semantics: no candidate, plan, policy,
// acceptance or authority concept appears here.
package api

import (
	"context"
	"time"
)

// ExecutionVersion is the only request/result/event contract version this
// module accepts and emits.
const ExecutionVersion = "agentkernel.execution/v0.1"

// Executor runs one bounded execution. It returns a settled result for every
// request it could evaluate, including refusals; the error is reserved for a
// misconfigured executor that could not produce a result at all.
type Executor interface {
	Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error)
}

// Mode selects the structural shape of the tool surface.
type Mode string

const (
	// ModeReadOnly exposes only read/search tools. Write and command grants
	// are refused at validation, so the surface is read-only by construction.
	ModeReadOnly Mode = "read_only"
	// ModeReadWrite additionally admits file.write and command.run grants.
	ModeReadWrite Mode = "read_write"
)

// ExecutionRequest is everything one bounded execution may use. Every bound is
// explicit and finite; the kernel never widens one with a default.
type ExecutionRequest struct {
	Version     string            `json:"version"`
	ExecutionID string            `json:"execution_id"`
	AttemptID   string            `json:"attempt_id"`
	Objective   string            `json:"objective"`
	Mode        Mode              `json:"mode"`
	Workspace   WorkspaceRef      `json:"workspace"`
	Constraints Constraints       `json:"constraints"`
	Context     []ContextItem     `json:"context"`
	Grants      []Capability      `json:"grants"`
	Budget      Budget            `json:"budget"`
	Providers   []ProviderBinding `json:"providers"`
}

// WorkspaceRef binds an execution to an immutable workspace content manifest.
// A Git revision alone never identifies a dirty workspace.
type WorkspaceRef struct {
	ID             string `json:"id"`
	ManifestDigest string `json:"manifest_digest"`
	GitRevision    string `json:"git_revision,omitempty"`
	Dirty          bool   `json:"dirty"`
}

// Constraints are host decisions the kernel enforces but never relaxes.
type Constraints struct {
	// RequiredFeatures are kernel features the host requires; an unsupported
	// one refuses the request before any side effect.
	RequiredFeatures []string `json:"required_features,omitempty"`
	// RequireHostProvenIsolation admits only bindings whose isolation the host
	// has proven. The kernel itself never claims protected execution.
	RequireHostProvenIsolation bool `json:"require_host_proven_isolation"`
	// InstructionDigest binds the host instruction set the request was built
	// from. It is recorded in provenance, never interpreted.
	InstructionDigest string `json:"instruction_digest"`
}

// Budget is the finite envelope of one execution. Every numeric bound is
// required and positive except MaxProviderRetries, which may be zero.
type Budget struct {
	Deadline           time.Time     `json:"deadline"`
	MaxIterations      int           `json:"max_iterations"`
	MaxToolCalls       int           `json:"max_tool_calls"`
	MaxInputTokens     int64         `json:"max_input_tokens"`
	MaxOutputTokens    int64         `json:"max_output_tokens"`
	MaxArtifactBytes   int64         `json:"max_artifact_bytes"`
	MaxProviderRetries int           `json:"max_provider_retries"`
	Money              *MoneyCeiling `json:"money,omitempty"`
}

// MoneyCeiling is a hard monetary bound. It is admitted only when every
// eligible binding carries trusted pricing in the same currency, so the
// worst-case token reservation can be priced before a call is made.
type MoneyCeiling struct {
	Currency  string `json:"currency"`
	MaxMicros int64  `json:"max_micros"`
}

// CapabilityKind names one narrow family of operations a host can grant.
type CapabilityKind string

const (
	CapabilityFileRead   CapabilityKind = "file.read"
	CapabilityFileSearch CapabilityKind = "file.search"
	CapabilityFileWrite  CapabilityKind = "file.write"
	CapabilityCommand    CapabilityKind = "command.run"
)

// Capability is a host-owned grant. Authority never comes from a tool name or
// argument; a tool runs only under a grant of its kind.
type Capability struct {
	// Handle is opaque to the kernel and echoed in observations so the host
	// can revalidate at side-effect time.
	Handle string         `json:"handle"`
	Kind   CapabilityKind `json:"kind"`
	// Roots are slash-separated workspace-relative directories ("." is the
	// workspace root). Required for file kinds; absent for command.run.
	Roots []string `json:"roots,omitempty"`
	// Commands are the only commands a command.run grant admits. The model
	// chooses one by name and can never supply argv.
	Commands []CommandGrant `json:"commands,omitempty"`
}

// CommandGrant is one host-named command with fixed argv and a bounded run.
type CommandGrant struct {
	Name           string   `json:"name"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

// Isolation is a host statement about a provider binding's execution boundary.
type Isolation string

const (
	IsolationUnproven   Isolation = "unproven"
	IsolationHostProven Isolation = "host_proven"
)

// ProviderBinding is one provider identity the host has decided about. It
// holds no credential value; CredentialHandle names one the host resolves.
type ProviderBinding struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	// Model and ModelVersion identify what may run. ModelVersion is the
	// literal "unknown" when the host cannot pin it.
	Model             string    `json:"model"`
	ModelVersion      string    `json:"model_version"`
	Features          []string  `json:"features,omitempty"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	Eligible          bool      `json:"eligible"`
	Pinned            bool      `json:"pinned"`
	Preference        int       `json:"preference"`
	Isolation         Isolation `json:"isolation"`
	ContextWindow     int64     `json:"context_window"`
	MaxOutputTokens   int64     `json:"max_output_tokens"`
	CredentialHandle  string    `json:"credential_handle,omitempty"`
	Pricing           *Pricing  `json:"pricing,omitempty"`
}

// UnknownVersion is the explicit spelling of an unpinned model version.
const UnknownVersion = "unknown"

// Pricing is a trusted host-supplied rate card. Absent pricing means cost is
// unknown, never zero.
type Pricing struct {
	Currency                    string `json:"currency"`
	InputMicrosPerMillion       int64  `json:"input_micros_per_million"`
	OutputMicrosPerMillion      int64  `json:"output_micros_per_million"`
	CachedInputMicrosPerMillion int64  `json:"cached_input_micros_per_million"`
	// CacheWriteInputMicrosPerMillion is the rate for input tokens written to
	// a provider cache, charged instead of the input rate (not on top of it).
	CacheWriteInputMicrosPerMillion int64     `json:"cache_write_input_micros_per_million"`
	Source                          string    `json:"source"`
	Version                         string    `json:"version"`
	ObservedAt                      time.Time `json:"observed_at"`
}

// Kernel features a request may require.
const (
	FeatureTools            = "tools"
	FeatureArtifacts        = "artifacts"
	FeatureCancelProvenance = "cancellation_provenance"
)

// SupportedFeatures are the features this kernel version implements.
var SupportedFeatures = []string{FeatureTools, FeatureArtifacts, FeatureCancelProvenance}
