package domain

import "time"

// InvocationObservation is the non-secret, bounded explanation of HOW one
// provider invocation ran: the effective command, the modes and bounds it ran
// under, and how it ended. It is defined once, here, because two durable homes
// carry it (#327): a run attempt's execution.attempt_provenance journal event,
// and a planning revision's PlanReasoningProvenance. Two definitions would drift
// about what "the same invocation" means.
//
// Every member is an OBSERVATION. None of them authorizes anything, and nothing
// here carries a prompt, provider prose, a tool input or a credential: the
// prompt is represented by its digest and its argv slot by a placeholder.
//
// Run-attempt identity (operation id, physical attempt identity) and the
// counted #241 Git refusals are deliberately NOT here: a planning invocation has
// neither a run operation nor a candidate to protect, and inventing either for
// symmetry would be a misleading zero.
type InvocationObservation struct {
	Executable string `json:"executable"`
	Version    string `json:"provider_version,omitempty"`
	// SandboxMode is empty when the provider exposes no selectable sandbox.
	SandboxMode    string `json:"sandbox_mode,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	// PermissionBypass records an explicitly authorized unsafe invocation. It
	// is omitempty, so its ABSENCE in every ordinary attempt is the norm and
	// its presence is conspicuous.
	PermissionBypass bool   `json:"permission_bypass,omitempty"`
	AuthMode         string `json:"auth_mode,omitempty"`
	AuthModeSource   string `json:"auth_mode_source,omitempty"`
	// WorkspaceBound reports that the invocation named the runtime-owned
	// workspace explicitly with a working-directory flag. Only one of the
	// supported CLIs offers one; for the rest the workspace is the bounded
	// process's working directory, which is equally exact and is recorded as
	// such rather than claimed as a flag that was not passed.
	WorkspaceBound bool `json:"workspace_bound"`
	// WorkspaceInstructionsSuppressed reports whether instruction files inside
	// the workspace were kept out of the CLI's own context.
	WorkspaceInstructionsSuppressed bool `json:"workspace_instructions_suppressed"`
	// Argv is the effective argument vector, bounded, with the prompt element
	// replaced by a placeholder. A vector cut short ends in "[argv truncated]".
	Argv         []string `json:"argv,omitempty"`
	PromptSHA256 string   `json:"prompt_sha256,omitempty"`

	// THE AUTHORITY THIS INVOCATION ACTUALLY RAN UNDER, and what it did with
	// it, so an invocation that outlives its bound explains itself from durable
	// state instead of costing a forensic reconstruction of a transcript.
	Deadline        *time.Time    `json:"execution_deadline,omitempty"`
	StartedAt       *time.Time    `json:"execution_started_at,omitempty"`
	CompletedAt     *time.Time    `json:"execution_completed_at,omitempty"`
	Elapsed         time.Duration `json:"observed_wall_elapsed,omitempty"`
	OverranDeadline bool          `json:"overran_deadline,omitempty"`
	// TerminationCause is why the process stopped: it returned on its own, the
	// runtime ended it at the deadline, or the runtime ended it because it had
	// produced no recognized progress for the whole inactivity window.
	TerminationCause string `json:"termination_cause,omitempty"`
	// InactivityLimit is the no-progress window this invocation ran under, and
	// zero when none was in force.
	InactivityLimit time.Duration `json:"inactivity_limit,omitempty"`
	// ProgressMode is the oracle that measured progress against that window:
	// byte_output, or structured_claude_events (#322).
	ProgressMode string `json:"progress_mode,omitempty"`
	// Bounded observations from a structured stream: how many events counted
	// as progress, how many main-thread tool calls were still open when the
	// process ended, how many permission denials the final result listed, and
	// how many lines were malformed or oversized.
	StructuredEvents  int64 `json:"structured_progress_events,omitempty"`
	OpenToolsAtExit   int   `json:"open_tools_at_exit,omitempty"`
	PermissionDenials int   `json:"permission_denials,omitempty"`
	// PermissionDeniedTools is the bounded SET of tool identifiers the typed
	// permission_denials[].tool_name named: at most MaxPermissionDeniedTools,
	// identifier characters only, and never a tool input, an argument or the
	// denial text. It is diagnostic evidence for a least-privilege review, not
	// a grant request.
	PermissionDeniedTools []string `json:"permission_denied_tools,omitempty"`
	ProtocolAnomalies     int      `json:"protocol_anomalies,omitempty"`
	// ProcessID is the pid - and process-group id - the runtime owned.
	ProcessID int `json:"process_id,omitempty"`
	// GitGuarded reports that this invocation ran under the brokered Git
	// boundary of #241. Its ABSENCE is the durable fact that matters.
	GitGuarded bool `json:"git_guarded,omitempty"`
}

// MaxPermissionDeniedTools and MaxPermissionDeniedToolBytes bound
// InvocationObservation.PermissionDeniedTools.
const (
	MaxPermissionDeniedTools     = 8
	MaxPermissionDeniedToolBytes = 64
)
