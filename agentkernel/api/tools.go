package api

import (
	"context"
	"encoding/json"
	"time"
)

// ToolSpec describes one tool to a provider. InputSchema is a JSON Schema
// object with additionalProperties false.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ToolCall is a model's proposal. It is untrusted until the broker validates
// it against a host grant.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolStatus is the outcome of one proposal.
type ToolStatus string

const (
	ToolOK      ToolStatus = "ok"
	ToolError   ToolStatus = "error"
	ToolRefused ToolStatus = "refused"
)

// ToolResult is what the model sees back. Output is bounded and may be
// filtered, but errors, exit status and truncation are always preserved, and
// FullOutput references the exact unfiltered bytes when anything was cut.
type ToolResult struct {
	CallID     string       `json:"call_id"`
	Status     ToolStatus   `json:"status"`
	Output     string       `json:"output"`
	Error      string       `json:"error,omitempty"`
	ExitCode   *int         `json:"exit_code,omitempty"`
	Truncated  bool         `json:"truncated"`
	FullOutput *ArtifactRef `json:"full_output,omitempty"`
	Grant      string       `json:"grant,omitempty"`
	// Mutated is true when the tool changed workspace state.
	Mutated bool `json:"mutated"`
}

// CommandRequest is a host-granted command to run. Argv comes only from a
// CommandGrant, never from model output.
type CommandRequest struct {
	Argv    []string      `json:"argv"`
	Dir     string        `json:"dir"`
	Timeout time.Duration `json:"timeout"`
}

// CommandResult is the bounded outcome of a command.
type CommandResult struct {
	ExitCode  int    `json:"exit_code"`
	Stdout    []byte `json:"stdout"`
	Stderr    []byte `json:"stderr"`
	Truncated bool   `json:"truncated"`
}

// CommandRunner is the host's process boundary. The kernel never spawns,
// contains or reaps processes itself.
type CommandRunner interface {
	Run(ctx context.Context, request CommandRequest) (CommandResult, error)
}

// ArtifactRef identifies stored bytes exactly.
type ArtifactRef struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	Producer  string `json:"producer"`
}

// ArtifactInput is content to store with its producer binding.
type ArtifactInput struct {
	MediaType string
	Producer  string
	Data      []byte
}

// ArtifactStore persists content-addressed artifacts. Get verifies integrity
// and fails rather than returning corrupt bytes.
type ArtifactStore interface {
	Put(ctx context.Context, input ArtifactInput) (ArtifactRef, error)
	Get(ctx context.Context, ref ArtifactRef) ([]byte, error)
}
