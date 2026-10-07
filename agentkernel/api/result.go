package api

import "time"

// Outcome describes how an execution ended. It never states that the objective
// was satisfied or that any engineering outcome may be accepted.
type Outcome string

const (
	OutcomeCompleted  Outcome = "completed"
	OutcomeFailed     Outcome = "failed"
	OutcomeBlocked    Outcome = "blocked"
	OutcomeCancelled  Outcome = "cancelled"
	OutcomeExhausted  Outcome = "exhausted"
	OutcomeIncomplete Outcome = "incomplete"
)

// Cause is the typed reason behind an Outcome.
type Cause string

const (
	CauseLoopCompleted        Cause = "loop_completed"        // completed
	CauseBudgetExhausted      Cause = "budget_exhausted"      // exhausted
	CauseProviderFailed       Cause = "provider_failed"       // failed
	CauseProviderRefused      Cause = "provider_refused"      // failed
	CauseProviderUnavailable  Cause = "provider_unavailable"  // blocked
	CauseToolFailed           Cause = "tool_failed"           // failed
	CauseHostCancelled        Cause = "host_cancelled"        // cancelled
	CauseInvalidRequest       Cause = "invalid_request"       // blocked
	CauseInsufficientCapacity Cause = "insufficient_capacity" // blocked
	CauseNoEligibleProvider   Cause = "no_eligible_provider"  // blocked
	CauseRecordingFailed      Cause = "recording_failed"      // incomplete
)

// BudgetDimension names the bound that was exhausted.
type BudgetDimension string

const (
	DimensionDeadline      BudgetDimension = "deadline"
	DimensionIterations    BudgetDimension = "iterations"
	DimensionToolCalls     BudgetDimension = "tool_calls"
	DimensionInputTokens   BudgetDimension = "input_tokens"
	DimensionOutputTokens  BudgetDimension = "output_tokens"
	DimensionArtifactBytes BudgetDimension = "artifact_bytes"
	DimensionRetries       BudgetDimension = "provider_retries"
	DimensionMoney         BudgetDimension = "money"
)

// CancellationProvenance is the host-supplied reason behind a cancellation.
// context cancellation alone never establishes one; unknown stays explicit.
type CancellationProvenance string

const (
	CancelUnknown            CancellationProvenance = "unknown"
	CancelOperatorStop       CancellationProvenance = "operator_stop"
	CancelControllerShutdown CancellationProvenance = "controller_shutdown"
	CancelDeadline           CancellationProvenance = "deadline"
)

// Termination is the single settled observation of how an execution ended.
// It is produced once; nothing observed later rewrites it.
type Termination struct {
	Outcome      Outcome                `json:"outcome"`
	Cause        Cause                  `json:"cause"`
	Dimension    BudgetDimension        `json:"dimension,omitempty"`
	Cancellation CancellationProvenance `json:"cancellation,omitempty"`
	Detail       string                 `json:"detail,omitempty"`
	ObservedAt   time.Time              `json:"observed_at"`
}

// ExecutionResult is observation only: it has no acceptance, merge or
// authorization field by design.
type ExecutionResult struct {
	Version      string           `json:"version"`
	ExecutionID  string           `json:"execution_id"`
	AttemptID    string           `json:"attempt_id"`
	Termination  Termination      `json:"termination"`
	FinalText    string           `json:"final_text,omitempty"`
	Observations []Observation    `json:"observations"`
	Artifacts    []ArtifactRef    `json:"artifacts"`
	Usage        Usage            `json:"usage"`
	Context      *ContextManifest `json:"context,omitempty"`
	Routing      *RoutingDecision `json:"routing,omitempty"`
	Provenance   Provenance       `json:"provenance"`
	EventCount   int64            `json:"event_count"`
}

// Observation is one thing the execution saw or did, in order.
type Observation struct {
	Seq      int64        `json:"seq"`
	Kind     EventKind    `json:"kind"`
	Detail   string       `json:"detail,omitempty"`
	ToolCall string       `json:"tool_call,omitempty"`
	Grant    string       `json:"grant,omitempty"`
	Ref      *ArtifactRef `json:"ref,omitempty"`
}

// Provenance binds the result to exactly what ran.
type Provenance struct {
	KernelVersion         string               `json:"kernel_version"`
	ProviderID            string               `json:"provider_id,omitempty"`
	ProviderKind          string               `json:"provider_kind,omitempty"`
	Model                 string               `json:"model,omitempty"`
	ModelVersionBound     string               `json:"model_version_bound,omitempty"`
	ModelVersionObserved  string               `json:"model_version_observed,omitempty"`
	ConfigFingerprint     string               `json:"config_fingerprint,omitempty"`
	WorkspaceID           string               `json:"workspace_id"`
	WorkspaceDigest       string               `json:"workspace_digest"`
	InstructionDigest     string               `json:"instruction_digest"`
	ContextManifestDigest string               `json:"context_manifest_digest,omitempty"`
	Isolation             Isolation            `json:"isolation"`
	Sessions              []SessionObservation `json:"sessions,omitempty"`
}

// KernelVersion identifies this implementation in provenance.
const KernelVersion = "agentkernel/0.1.0-gate-a"

// RoutingDecision is the explainable record of provider selection.
type RoutingDecision struct {
	Chosen     string             `json:"chosen,omitempty"`
	Candidates []RoutingCandidate `json:"candidates"`
	Blocked    string             `json:"blocked,omitempty"`
}

// RoutingCandidate is one binding's evaluation, in final rank order.
type RoutingCandidate struct {
	ProviderID string   `json:"provider_id"`
	Eligible   bool     `json:"eligible"`
	Rank       int      `json:"rank"`
	Reasons    []string `json:"reasons"`
}
