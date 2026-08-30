package runtime

// ToolSurface is the complete set of engineering capabilities a brokered
// reasoning provider may request. It is deliberately a closed enumeration:
//
//	model -> tool request (JSON) -> ToolSurface (validate) -> ToolBroker (execute)
//
// The model never receives a path handle, a file descriptor, a process, or a
// shell. It emits a tool NAME and JSON ARGUMENTS; Zenchron decides whether that
// request is admissible and, if so, performs the operation itself. There is
// deliberately no unrestricted shell tool: candidate.run is a bounded argv
// executed through ToolBroker.RunCommand, which is the existing DockerSandbox
// path with networking off and only the candidate workspace mounted.
//
// Arguments are decoded strictly. An unknown tool name, malformed JSON, an
// unknown argument field, or an empty required field is REFUSED and the refusal
// is returned to the model as an ordinary tool result so it can correct itself.
// A refusal never falls back to executing a different operation.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// The M0 tool surface. These names are the wire contract with the model.
const (
	ToolRepoRead            = "repo.read"
	ToolRepoSearch          = "repo.search"
	ToolCandidateDiff       = "candidate.diff"
	ToolCandidateApplyPatch = "candidate.apply_patch"
	ToolCandidateRun        = "candidate.run"
)

// defaultToolResultBytes bounds what a single tool result may push back into
// the reasoning context, so one large file or search cannot exhaust the token
// budget in a single turn.
const defaultToolResultBytes = 64 << 10

type ToolSurface struct {
	Broker ToolBroker
	// MaxResultBytes bounds one tool result; 0 uses defaultToolResultBytes.
	MaxResultBytes int
}

// toolDefinition is the OpenAI function-tool declaration. Schemas are strict
// (additionalProperties false, every property required) so the API itself
// rejects most malformed calls before they reach us; ToolSurface re-validates
// regardless, because the schema is a convenience and not the boundary.
type toolDefinition struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Strict      bool           `json:"strict"`
	Parameters  map[string]any `json:"parameters"`
}

func schema(properties map[string]any, required ...string) map[string]any {
	return map[string]any{"type": "object", "properties": properties, "required": required, "additionalProperties": false}
}

func stringsArray(description string) map[string]any {
	return map[string]any{"type": "array", "description": description, "items": map[string]any{"type": "string"}}
}

// toolDefinitions is the exact surface advertised to the model. Nothing outside
// this list is dispatchable, so adding a capability is a deliberate edit here.
func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{Type: "function", Name: ToolRepoRead, Strict: true,
			Description: "Read one file from the candidate workspace by repository-relative path.",
			Parameters:  schema(map[string]any{"path": map[string]any{"type": "string", "description": "Repository-relative path inside the candidate workspace."}}, "path")},
		{Type: "function", Name: ToolRepoSearch, Strict: true,
			Description: "Search the candidate workspace with a Go regular expression.",
			Parameters: schema(map[string]any{
				"pattern": map[string]any{"type": "string", "description": "Go regular expression."},
				"scope":   stringsArray("Optional repository-relative paths to restrict the search to."),
			}, "pattern", "scope")},
		{Type: "function", Name: ToolCandidateDiff, Strict: true,
			Description: "Show the candidate's uncommitted diff, optionally restricted to paths.",
			Parameters:  schema(map[string]any{"paths": stringsArray("Optional repository-relative paths.")}, "paths")},
		{Type: "function", Name: ToolCandidateApplyPatch, Strict: true,
			Description: "Apply a git-compatible unified diff to the candidate workspace. Every affected path is validated before anything is written. " +
				"ACCEPTED: a patch starting with \"--- a/path\" / \"+++ b/path\" / \"@@ ...\", or a full \"diff --git a/path b/path\" patch. " +
				"REJECTED: \"*** Begin Patch\", \"*** Update File:\", \"*** Add File:\", \"*** Delete File:\", \"*** End Patch\" - that dialect is not a unified diff and is refused, not translated. " +
				"Zenchron validates the patch with `git apply`, which is the only parser: the hunk BODY is authoritative and line counts in @@ headers are recounted, but context lines must match the file as it is on disk right now. " +
				"After a context mismatch, repo.read the file again and rebuild the patch from what it actually contains.",
			Parameters: schema(map[string]any{"patch": map[string]any{"type": "string", "description": "A git-compatible unified diff. Context lines must match the current file contents."}}, "patch")},
		{Type: "function", Name: ToolCandidateRun, Strict: true,
			Description: "Run one bounded command in the candidate sandbox: no network, only the candidate workspace mounted. This is an argv, not a shell.",
			Parameters:  schema(map[string]any{"command": stringsArray("Command argv; the first element is the program.")}, "command")},
	}
}

// decodeToolArguments is the strict gate. json.Decoder with
// DisallowUnknownFields refuses a field the tool does not define, and the
// trailing-content check refuses a second JSON value smuggled after the first.
func decodeToolArguments(raw []byte, into any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("malformed tool arguments: %v", err)
	}
	if decoder.More() {
		return fmt.Errorf("malformed tool arguments: trailing content")
	}
	return nil
}

// Invoke validates and performs one model-requested operation. It never
// returns an error to the caller and never panics out: a refusal or a broker
// failure comes back as an ordinary tool result with failed=true, which the
// reasoning loop feeds to the model and counts toward no-progress detection.
func (s ToolSurface) Invoke(ctx context.Context, name string, arguments []byte) (result string, failed bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			result, failed = fmt.Sprintf("tool error: %s failed unexpectedly", name), true
		}
	}()
	output, err := s.dispatch(ctx, name, arguments)
	if err != nil {
		return "tool error: " + err.Error(), true
	}
	return s.bound(output), false
}

func (s ToolSurface) dispatch(ctx context.Context, name string, arguments []byte) (string, error) {
	switch name {
	case ToolRepoRead:
		var args struct {
			Path string `json:"path"`
		}
		if err := decodeToolArguments(arguments, &args); err != nil {
			return "", err
		}
		if args.Path == "" {
			return "", fmt.Errorf("%s requires a non-empty path", name)
		}
		data, err := s.Broker.ReadFile(args.Path)
		return string(data), err
	case ToolRepoSearch:
		var args struct {
			Pattern string   `json:"pattern"`
			Scope   []string `json:"scope"`
		}
		if err := decodeToolArguments(arguments, &args); err != nil {
			return "", err
		}
		if args.Pattern == "" {
			return "", fmt.Errorf("%s requires a non-empty pattern", name)
		}
		hits, err := s.Broker.Search(args.Pattern, args.Scope)
		if err != nil {
			return "", err
		}
		var out strings.Builder
		for _, hit := range hits {
			out.WriteString(hit.Path + ":" + strconv.Itoa(hit.Line) + ": " + hit.Text + "\n")
		}
		return out.String(), nil
	case ToolCandidateDiff:
		var args struct {
			Paths []string `json:"paths"`
		}
		if err := decodeToolArguments(arguments, &args); err != nil {
			return "", err
		}
		return s.Broker.Diff(args.Paths)
	case ToolCandidateApplyPatch:
		var args struct {
			Patch string `json:"patch"`
		}
		if err := decodeToolArguments(arguments, &args); err != nil {
			return "", err
		}
		if args.Patch == "" {
			return "", fmt.Errorf("%s requires a non-empty patch", name)
		}
		if err := s.Broker.ApplyPatch([]byte(args.Patch)); err != nil {
			return "", err
		}
		return "patch applied", nil
	case ToolCandidateRun:
		var args struct {
			Command []string `json:"command"`
		}
		if err := decodeToolArguments(arguments, &args); err != nil {
			return "", err
		}
		if len(args.Command) == 0 {
			return "", fmt.Errorf("%s requires a non-empty command", name)
		}
		output, err := s.Broker.RunCommand(ctx, args.Command)
		// A non-zero exit is a legitimate observation, not a refusal: the model
		// must see failing test output to be able to act on it.
		text := "exit=" + strconv.Itoa(output.ExitCode) + "\nstdout:\n" + string(output.Stdout) + "\nstderr:\n" + string(output.Stderr)
		if err != nil && output.ExitCode == 0 {
			return "", err
		}
		return text, nil
	default:
		// An unknown name is refused by name. It is never mapped onto a
		// neighbouring tool and never falls through to a shell.
		return "", fmt.Errorf("unknown tool %q; the available tools are %s", name, strings.Join([]string{ToolRepoRead, ToolRepoSearch, ToolCandidateDiff, ToolCandidateApplyPatch, ToolCandidateRun}, ", "))
	}
}

func (s ToolSurface) bound(output string) string {
	limit := s.MaxResultBytes
	if limit <= 0 {
		limit = defaultToolResultBytes
	}
	if len(output) <= limit {
		return output
	}
	return output[:limit] + "\n[truncated by Zenchron: tool result exceeded " + strconv.Itoa(limit) + " bytes]"
}
