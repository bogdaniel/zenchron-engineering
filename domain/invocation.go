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
	// ProviderEnvironment is the EFFECTIVE provider-control environment the
	// runtime passed to the process (#391): one entry per name on the adapter's
	// explicit non-secret allowlist, in allowlist order, read from the very
	// slice the process received. Nothing outside the allowlist is recorded.
	ProviderEnvironment []EnvironmentEntry `json:"provider_environment,omitempty"`

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
	// byte_output, byte_output_excluding_transport_chatter (#314), or
	// structured_claude_events (#322).
	ProgressMode string `json:"progress_mode,omitempty"`
	// Bounded observations from a structured stream: how many events counted
	// as progress, how many main-thread tool calls were still open when the
	// process ended, how many permission denials the final result listed, and
	// how many lines were malformed or oversized.
	StructuredEvents int64 `json:"structured_progress_events,omitempty"`
	OpenToolsAtExit  int   `json:"open_tools_at_exit,omitempty"`
	// FinalResultObserved reports that the stream delivered a valid final
	// result. PermissionDenials and PermissionDeniedTools are READ from that
	// result, so without one they are UNKNOWN, not zero: they stay absent and
	// a reader must say so rather than print 0. An attempt killed at its
	// deadline typically has none.
	FinalResultObserved bool `json:"final_result_observed,omitempty"`
	PermissionDenials   int  `json:"permission_denials,omitempty"`
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
	// Truncated marks a MINIMAL record: the full observation could not fit
	// the durable payload ceiling, so only its fixed-size facts - bounds,
	// times, counters, termination cause - were kept. It is never set on an
	// ordinary invocation.
	Truncated bool `json:"provenance_truncated,omitempty"`
}

// EnvironmentEntry is one allowlisted provider-control variable. Value is
// ABSENT when the runtime did not pass the variable at all, and present - even
// as "" - when it did: unset and set-to-empty are different facts.
type EnvironmentEntry struct {
	Name  string  `json:"name"`
	Value *string `json:"value,omitempty"`
	// Bounded marks a recorded value that is not the passed value verbatim:
	// a credential-shaped substring was redacted, or it was cut to the field
	// bound.
	Bounded bool `json:"value_bounded,omitempty"`
}

// MaxProviderEnvironment bounds InvocationObservation.ProviderEnvironment.
const MaxProviderEnvironment = 24

// MinimalInvocationObservation is the fixed-size fallback of an observation
// that cannot be recorded whole. It keeps only members whose encoded size does
// not depend on provider or configuration text: booleans, clamped counters and
// durations, representable timestamps, the prompt digest when it is one, and
// the short runtime identifiers (termination cause, progress mode, denied
// tools) when they are identifiers. Everything dropped is dropped visibly, by
// Truncated.
func MinimalInvocationObservation(o InvocationObservation) InvocationObservation {
	minimal := InvocationObservation{
		WorkspaceBound: o.WorkspaceBound, WorkspaceInstructionsSuppressed: o.WorkspaceInstructionsSuppressed,
		PermissionBypass: o.PermissionBypass, OverranDeadline: o.OverranDeadline, GitGuarded: o.GitGuarded,
		FinalResultObserved: o.FinalResultObserved,
		Deadline:            representable(o.Deadline), StartedAt: representable(o.StartedAt), CompletedAt: representable(o.CompletedAt),
		Elapsed: time.Duration(clamp(int64(o.Elapsed))), InactivityLimit: time.Duration(clamp(int64(o.InactivityLimit))),
		StructuredEvents: clamp(o.StructuredEvents), OpenToolsAtExit: int(clamp(int64(o.OpenToolsAtExit))),
		PermissionDenials: int(clamp(int64(o.PermissionDenials))), ProtocolAnomalies: int(clamp(int64(o.ProtocolAnomalies))),
		ProcessID: int(clamp(int64(o.ProcessID))),
		Truncated: true,
	}
	if IsInvocationIdentifier(o.TerminationCause) {
		minimal.TerminationCause = o.TerminationCause
	}
	if IsInvocationIdentifier(o.ProgressMode) {
		minimal.ProgressMode = o.ProgressMode
	}
	if isSHA256Hex(o.PromptSHA256) {
		minimal.PromptSHA256 = o.PromptSHA256
	}
	for _, tool := range o.PermissionDeniedTools {
		if IsInvocationIdentifier(tool) && len(minimal.PermissionDeniedTools) < MaxPermissionDeniedTools {
			minimal.PermissionDeniedTools = append(minimal.PermissionDeniedTools, tool)
		}
	}
	return minimal
}

// IsInvocationIdentifier is the identifier shape a recorded tool name or
// runtime enum may take: Bash, WebFetch, mcp__server__tool, deadline_reached.
// It needs no escaping and is at most MaxPermissionDeniedToolBytes long.
func IsInvocationIdentifier(name string) bool {
	if name == "" || len(name) > MaxPermissionDeniedToolBytes {
		return false
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '.' || r == ':' || r == '-') {
			return false
		}
	}
	return true
}

// maxInvocationInteger is the I-JSON ceiling the canonicalizer enforces.
const maxInvocationInteger = 1<<53 - 1

func clamp(n int64) int64 { return max(0, min(n, maxInvocationInteger)) }

func representable(t *time.Time) *time.Time {
	if t == nil || t.Year() < 1 || t.Year() > 9999 {
		return nil
	}
	return t
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// MaxPermissionDeniedTools and MaxPermissionDeniedToolBytes bound
// InvocationObservation.PermissionDeniedTools.
const (
	MaxPermissionDeniedTools     = 8
	MaxPermissionDeniedToolBytes = 64
)
