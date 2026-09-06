package runtime

// The native-CLI catalogue: one spec per supported coding CLI.
//
// This file is where provider-specific knowledge is allowed to live, and it is
// the ONLY place it lives. Adding another CLI means adding one spec here, one
// kind in agents.go, one branch in the composition root's factory, and tests.
// Nothing in the scheduler, the reconciler, the kernel, the authority
// evaluator, Git or the forge adapter learns the new provider's name.
//
// Every spec states the flags this runtime depends on TWICE, on purpose: once
// in Probes, which requires the installed CLI to advertise them, and once in
// Args, which passes them. That is what makes a wrong or outdated flag fail
// CLOSED. If a CLI renames `--approval-mode`, the probe stops finding it and
// the agent reports unavailable; it never silently runs the CLI in whatever
// mode it defaults to. An adapter that guessed and ran anyway would be the one
// failure mode `operator_trusted` cannot tolerate: an unconstrained coding
// agent under the operator's account whose provenance describes it as
// constrained.
//
// A NOTE ON BILLING, because it is the property #63 cares most about. None of
// these specs sets an API-key environment variable, and none can: the child
// environment is an allowlist built from scratch in cli_agent.go, so
// CODEX_API_KEY, ANTHROPIC_API_KEY, GEMINI_API_KEY, GOOGLE_API_KEY and
// OPENAI_API_KEY are all absent from the invocation whether or not the
// operator's shell has them. Each of these CLIs treats such a variable as an
// override that displaces the interactive subscription session, so their
// absence is exactly what keeps "Zenchron supervised my Claude Code run" from
// silently meaning "Zenchron billed my Anthropic API account". The runtime does
// not claim to know which plan ultimately paid - see AuthModeUnknown - only
// that it substituted no credential of its own.

// diagnosticSignal maps one RECOGNIZED provider diagnostic onto a typed
// failure class. Matching is case-insensitive substring containment over the
// combined output.
//
// The list per provider is deliberately short. Guessing at another service's
// error taxonomy is how a transient throttle becomes a terminal stop, or an
// exhausted account becomes an infinite retry; an unrecognized diagnostic stays
// FailureUnknown and fails closed, exactly as ClassifyProviderFailure already
// does for the brokered provider.
type diagnosticSignal struct {
	Match string
	Class FailureClass
}

// sharedAgentSignals are transport-level diagnostics with the same meaning for
// every provider, because they are HTTP's vocabulary rather than any vendor's.
var sharedAgentSignals = []diagnosticSignal{
	{"429 too many requests", FailureProviderRateLimited},
	{"rate limit exceeded", FailureProviderRateLimited},
	{"rate_limit_exceeded", FailureProviderRateLimited},
	{"too many requests", FailureProviderRateLimited},
}

// codexSpec drives the installed Codex CLI.
//
// --ask-for-approval is a TOP-LEVEL flag and --sandbox is accepted by `exec`,
// so they are passed on the sides of the subcommand that actually parse them.
// --ignore-user-config stops arbitrary user configuration from redefining the
// sandbox, and the two -c overrides deny tool network access and refuse to load
// the candidate working tree's AGENTS.md as instructions.
var codexSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"exec", "--help"}, Required: []string{"--sandbox", "workspace-write", "--ignore-user-config", "--cd", "--config"}},
		{Args: []string{"--help"}, Required: []string{"--ask-for-approval"}},
	},
	VersionArgs:                     []string{"--version"},
	AuthStatePaths:                  []string{".codex/auth.json", ".codex/config.toml", "auth.json"},
	HomeEnv:                         "CODEX_HOME",
	Permission:                      cliPermissionModes{Safe: "approval=never,sandbox=workspace-write", Bypass: "sandbox=danger-full-access"},
	Sandbox:                         "workspace-write",
	WorkingDirectoryFlag:            true,
	SuppressesWorkspaceInstructions: true,
	Signals: []diagnosticSignal{
		{"usage limit reached", FailureProviderQuota},
		{"you've hit your usage limit", FailureProviderQuota},
	},
	Args: func(i cliInvocation) []string {
		sandbox := "workspace-write"
		if i.Bypass {
			sandbox = "danger-full-access"
		}
		args := []string{"--ask-for-approval", "never", "exec", "--sandbox", sandbox, "--ignore-user-config"}
		if !i.Bypass {
			args = append(args, "-c", "sandbox_workspace_write.network_access=false")
		}
		args = append(args, "-c", "project_doc_max_bytes=0")
		if i.Agent.Model != "" {
			args = append(args, "--model", i.Agent.Model)
		}
		return append(args, "--cd", i.CandidateDir, i.Prompt)
	},
}

// claudeSpec drives the installed Claude Code CLI.
//
// --print is the non-interactive mode. --permission-mode acceptEdits is the
// least-privilege automation mode that can still edit the candidate: the
// session writes files in its working directory without prompting, and
// everything else still goes through Claude Code's own permission machinery.
// --safe-mode is the instruction-isolation counterpart of Codex's
// project_doc_max_bytes=0: it keeps CLAUDE.md, hooks, plugins, skills and MCP
// servers out of the session, so a candidate repository cannot supply
// instructions or tools to the worker changing it.
//
// Claude Code has no working-directory flag, so the workspace is the bounded
// process's cwd. That is equally exact - the process runner sets it - and the
// provenance says so rather than claiming a flag that was not passed.
//
// --bare is deliberately never passed: it switches Claude Code onto strict
// API-key authentication, which would turn a supervised subscription session
// into metered API billing.
var claudeSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"--help"}, Required: []string{"--print", "--permission-mode", "--model", "--safe-mode"}},
	},
	VersionArgs:                     []string{"--version"},
	AuthStatePaths:                  []string{".claude/.credentials.json", ".claude.json", ".claude/settings.json"},
	HomeEnv:                         "CLAUDE_CONFIG_DIR",
	Permission:                      cliPermissionModes{Safe: "acceptEdits", Bypass: "bypassPermissions"},
	SuppressesWorkspaceInstructions: true,
	Signals: []diagnosticSignal{
		{"usage limit reached", FailureProviderQuota},
		{"credit balance is too low", FailureProviderAccountUnavailable},
	},
	Args: func(i cliInvocation) []string {
		mode := "acceptEdits"
		if i.Bypass {
			mode = "bypassPermissions"
		}
		args := []string{"--print", "--permission-mode", mode, "--safe-mode"}
		if i.Agent.Model != "" {
			args = append(args, "--model", i.Agent.Model)
		}
		return append(args, i.Prompt)
	},
}

// geminiSpec drives the installed Gemini CLI.
//
// --prompt is the non-interactive mode and --approval-mode auto_edit is the
// least-privilege mode that still permits file edits; `yolo` approves
// everything and is the bypass this runtime never selects on its own. -e none
// disables extensions, which is the closest thing the CLI offers to Codex's
// instruction isolation.
//
// SuppressesWorkspaceInstructions is FALSE here, and that is a real difference
// rather than an oversight: the Gemini CLI exposes no flag that stops it
// loading a GEMINI.md from the working tree. The runtime-owned trusted
// instruction text still frames everything inside the workspace as data, but
// this adapter cannot prove the file was never read, so it does not claim to.
var geminiSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"--help"}, Required: []string{"--prompt", "--approval-mode", "--model"}},
	},
	VersionArgs:    []string{"--version"},
	AuthStatePaths: []string{".gemini/oauth_creds.json", ".gemini/settings.json"},
	Permission:     cliPermissionModes{Safe: "auto_edit", Bypass: "yolo"},
	Signals: []diagnosticSignal{
		{"resource_exhausted", FailureProviderQuota},
		{"quota exceeded", FailureProviderQuota},
	},
	Args: func(i cliInvocation) []string {
		mode := "auto_edit"
		if i.Bypass {
			mode = "yolo"
		}
		args := []string{"--approval-mode", mode, "--extensions", "none"}
		if i.Agent.Model != "" {
			args = append(args, "--model", i.Agent.Model)
		}
		return append(args, "--prompt", i.Prompt)
	},
}

// qwenSpec drives an installed, already-agentic Qwen coding CLI.
//
// It is deliberately an AGENTIC CLI and not a raw local completion endpoint. A
// completion endpoint cannot edit a file, so supporting one would mean building
// a second coding harness - a tool loop, a patch applier, a retry policy -
// inside this repository. That is a different issue and #63 does not need it.
//
// Qwen Code began as a Gemini CLI fork and its flag grammar has since diverged:
// the approval mode is spelled auto-edit with a HYPHEN, and it gained its own
// --safe-mode. The probe is what verifies both on the operator's actual
// installation rather than trusting the lineage.
//
// The prompt is positional. Qwen's array-valued flags greedily absorb a
// following positional, so nothing array-valued is ever placed before it.
var qwenSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"--help"}, Required: []string{"--approval-mode", "--model", "--safe-mode"}},
	},
	VersionArgs:                     []string{"--version"},
	AuthStatePaths:                  []string{".qwen/oauth_creds.json", ".qwen/settings.json", ".qwen"},
	HomeEnv:                         "QWEN_HOME",
	Permission:                      cliPermissionModes{Safe: "auto-edit", Bypass: "yolo"},
	SuppressesWorkspaceInstructions: true,
	Signals: []diagnosticSignal{
		{"resource_exhausted", FailureProviderQuota},
		{"quota exceeded", FailureProviderQuota},
	},
	Args: func(i cliInvocation) []string {
		mode := "auto-edit"
		if i.Bypass {
			mode = "yolo"
		}
		args := []string{"--approval-mode", mode, "--safe-mode"}
		if i.Agent.Model != "" {
			args = append(args, "--model", i.Agent.Model)
		}
		return append(args, i.Prompt)
	},
}
