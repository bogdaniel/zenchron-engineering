package api

import "context"

// ContextKind classifies a context item for selection priority.
type ContextKind string

const (
	ContextInstruction ContextKind = "instruction"
	ContextConstraint  ContextKind = "constraint"
	ContextTask        ContextKind = "task"
	ContextObservation ContextKind = "observation"
	ContextSourceCode  ContextKind = "source"
	ContextDependency  ContextKind = "dependency"
	ContextTest        ContextKind = "test"
	ContextMemory      ContextKind = "memory"
	ContextBackground  ContextKind = "background"
)

// Trust is the origin class of content. Only TrustHost content supplied in the
// request may become a system instruction; everything else is untrusted data.
type Trust string

const (
	TrustHost       Trust = "host"
	TrustWorkspace  Trust = "workspace"
	TrustToolOutput Trust = "tool_output"
	TrustModel      Trust = "model_derived"
	TrustMemory     Trust = "memory"
)

// ContextItem is one unit of context with its identity and origin.
type ContextItem struct {
	ID            string      `json:"id"`
	Kind          ContextKind `json:"kind"`
	Trust         Trust       `json:"trust"`
	Required      bool        `json:"required"`
	Content       string      `json:"content"`
	ContentDigest string      `json:"content_digest"`
	// Revision binds the item to the source revision or snapshot it came from.
	Revision string `json:"revision,omitempty"`
	// Ref points at the full content when Content is a disclosure excerpt.
	Ref *ArtifactRef `json:"ref,omitempty"`
	// Score orders optional items within one kind; higher first.
	Score float64 `json:"score,omitempty"`
}

// ContextManifest is the reproducible record of one context compilation.
type ContextManifest struct {
	Digest   string          `json:"digest"`
	Entries  []ManifestEntry `json:"entries"`
	Capacity Capacity        `json:"capacity"`
}

// ManifestEntry records why an item was selected or excluded.
type ManifestEntry struct {
	ItemID        string        `json:"item_id"`
	Kind          ContextKind   `json:"kind"`
	Trust         Trust         `json:"trust"`
	Required      bool          `json:"required"`
	Selected      bool          `json:"selected"`
	Reason        string        `json:"reason"`
	ContentDigest string        `json:"content_digest"`
	Tokens        TokenEstimate `json:"tokens"`
	Ref           *ArtifactRef  `json:"ref,omitempty"`
}

// Capacity is the token arithmetic a compilation was held to.
type Capacity struct {
	Window         int64 `json:"window"`
	ReservedOutput int64 `json:"reserved_output"`
	Used           int64 `json:"used"`
	// Exact is false when any count in Used is an approximation.
	Exact bool `json:"exact"`
}

// TokenEstimate is a count with its precision stated.
type TokenEstimate struct {
	Count int64 `json:"count"`
	Exact bool  `json:"exact"`
}

// ContextQuery asks a source for optional context relevant to an execution.
type ContextQuery struct {
	ExecutionID string       `json:"execution_id"`
	Objective   string       `json:"objective"`
	Workspace   WorkspaceRef `json:"workspace"`
	Limit       int          `json:"limit"`
}

// ContextSource supplies optional, untrusted context items. Implementations
// are repository intelligence views and memory stores. The kernel never
// calls ContextItems itself: a host serves a source with ServeContext (or its
// own worker on a ContextRequest channel), and a source that does not answer
// within its bound is noted unavailable.
type ContextSource interface {
	ContextItems(ctx context.Context, query ContextQuery) ([]ContextItem, error)
}
