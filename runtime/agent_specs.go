package runtime

import (
	"encoding/json"
	"strings"
)

// The native-CLI catalogue: one spec per supported coding CLI.
//
// Provider-specific knowledge is owned by the provider adapter/spec layer: this
// catalogue, plus the provider files a spec field wires in - claude_stream.go
// holds Claude Code's stream-json parser, selected by claudeSpec.ProgressMode
// (#322). Adding another CLI means adding one spec here, one kind in agents.go,
// one branch in the composition root's factory, and tests. Nothing in the
// scheduler, the reconciler, the kernel, the authority evaluator, Git or the
// forge adapter learns the new provider's name or its event vocabulary.
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
	// THE ENDPOINT SAYING IT IS UNAVAILABLE. These are HTTP's own status
	// phrases, so they belong here rather than in any vendor's list, and they
	// are the gateway statuses only: a 500 is the provider failing at a
	// request, which says nothing about reachability, and classifying it as
	// unavailable would park a run on a wait no operator can clear.
	{"502 bad gateway", FailureProviderUnavailable},
	{"503 service unavailable", FailureProviderUnavailable},
	{"504 gateway timeout", FailureProviderUnavailable},
}

// nodeTransportSignals are the connectivity diagnostics a Node-based CLI emits,
// which is three of the four adapters here: Claude Code, Gemini and Qwen Code
// all surface libuv/undici errno strings when the host cannot reach their
// endpoint.
//
// They are errno TOKENS, not prose, which is what keeps this from being a
// substring swamp: ENOTFOUND is emitted by the resolver and means the name did
// not resolve, and no amount of ordinary model output contains it. Deliberately
// absent are ETIMEDOUT and undici's bare "fetch failed": the first is
// indistinguishable from a slow provider, and the second wraps every transport
// outcome including ones that are not connectivity at all.
//
// Silence is NOT here, and that is the boundary #238 draws: a host with no
// network that says nothing is FailureProviderNoProgress, and only a provider
// that NAMES its transport failure reaches FailureProviderUnavailable.
var nodeTransportSignals = []diagnosticSignal{
	{"getaddrinfo enotfound", FailureProviderUnavailable},
	{"getaddrinfo eai_again", FailureProviderUnavailable},
	{"econnrefused", FailureProviderUnavailable},
	{"econnreset", FailureProviderUnavailable},
	{"enetunreach", FailureProviderUnavailable},
	{"ehostunreach", FailureProviderUnavailable},
	{"enetdown", FailureProviderUnavailable},
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
	AuthStatePaths:                  []string{".codex/auth.json", "auth.json"},
	HomeEnv:                         "CODEX_HOME",
	Permission:                      cliPermissionModes{Safe: "approval=never,sandbox=workspace-write", Bypass: "sandbox=danger-full-access"},
	Sandbox:                         "workspace-write",
	WorkingDirectoryFlag:            true,
	SuppressesWorkspaceInstructions: true,
	Signals: []diagnosticSignal{
		{"usage limit reached", FailureProviderQuota},
		{"you've hit your usage limit", FailureProviderQuota},
		// A revoked or expired sign-in is an account PREREQUISITE the operator
		// restores with `codex login`, not a defect in the work. Left
		// unrecognized it classified as a terminal invocation failure, so a run
		// died on a condition that a login would have fixed - and the operator
		// had to read a provider transcript to discover that. Observed live:
		// "Your access token could not be refreshed because your refresh token
		// was revoked. Please log out and sign in again."
		{"refresh token was revoked", FailureProviderAccountUnavailable},
		{"please log out and sign in again", FailureProviderAccountUnavailable},
		// CODEX CANNOT REACH ITS ENDPOINT. Codex is a Rust binary over
		// reqwest/hyper, so its connectivity vocabulary is that stack's rather
		// than Node's: a resolver failure is reported as "dns error", and a
		// transport that never completed as "error sending request". Both are
		// statements that no exchange happened, which is what separates them
		// from a provider that answered badly.
		{"dns error", FailureProviderUnavailable},
		{"error sending request", FailureProviderUnavailable},
		{"connection refused", FailureProviderUnavailable},
		{"connection reset by peer", FailureProviderUnavailable},
		{"network is unreachable", FailureProviderUnavailable},
	},
	Args: func(i cliInvocation) []string {
		sandbox := "workspace-write"
		if i.Bypass {
			sandbox = "danger-full-access"
		}
		args := []string{"--ask-for-approval", "never", "exec", "--sandbox", sandbox, "--ignore-user-config"}
		if !i.Bypass {
			args = append(args, "-c", "sandbox_workspace_write.network_access=false")
			if i.ScratchDir != "" {
				roots, _ := json.Marshal([]string{i.ScratchDir})
				args = append(args, "-c", "sandbox_workspace_write.writable_roots="+string(roots))
			}
		}
		args = append(args, "-c", "project_doc_max_bytes=0")
		if i.Model() != "" {
			args = append(args, "--model", i.Model())
		}
		return append(args, "--cd", i.CandidateDir, i.Prompt)
	},
	// Codex's own read-only sandbox. The mode is one of the sandbox policy's
	// stated values, so the same flag the ordinary invocation uses to permit
	// workspace writes is what withholds them here - there is no second
	// mechanism to get wrong.
	ReadOnly: &cliReadOnlyMode{
		// `read-only` must be a CHOICE of --sandbox, not a phrase somewhere in
		// the help text - the same association the qwen probe needs, since this
		// adapter is about to pass `--sandbox read-only` and rely on it.
		Probe: cliHelpProbe{
			Args: []string{"exec", "--help"}, Required: []string{"--sandbox"},
			RequiredChoices: []cliFlagChoice{{Flag: "--sandbox", Value: "read-only"}},
		},
		Mode:    "read-only",
		Sandbox: "read-only",
		Args: func(i cliInvocation) []string {
			args := []string{"--ask-for-approval", "never", "exec", "--sandbox", "read-only", "--ignore-user-config",
				"-c", "sandbox_workspace_write.network_access=false", "-c", "project_doc_max_bytes=0"}
			if i.Model() != "" {
				args = append(args, "--model", i.Model())
			}
			return append(args, "--cd", i.CandidateDir, i.Prompt)
		},
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
//
// --output-format stream-json --verbose makes Claude's agent loop observable
// as newline-delimited JSON events, which is what supervises it (#322; see
// claude_stream.go). Both the editing and the plan invocation use it.
// --include-partial-messages is deliberately absent: token deltas add a line
// per token without being stronger evidence that the work advanced, so one
// long single content block can still go unobserved until it completes - the
// residual risk #322 accepts.
var claudeSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		// --output-format stream-json and --verbose are what make Claude's own
		// agent loop observable (#322). The installed CLI refuses stream-json
		// under --print without --verbose, so both are required: a binary
		// that lacks either is unavailable rather than silently supervised by
		// byte silence again.
		{
			Args: []string{"--help"}, Required: []string{"--print", "--permission-mode", "--model", "--safe-mode", "--output-format", "--verbose"},
			RequiredChoices: []cliFlagChoice{{Flag: "--output-format", Value: "stream-json"}},
		},
	},
	VersionArgs:                     []string{"--version"},
	AuthStatePaths:                  []string{".claude/.credentials.json"},
	HomeEnv:                         "CLAUDE_CONFIG_DIR",
	Permission:                      cliPermissionModes{Safe: "acceptEdits", Bypass: "bypassPermissions"},
	SuppressesWorkspaceInstructions: true,
	ProgressMode:                    progressStructuredClaudeEvents,
	Signals: append([]diagnosticSignal{
		{"usage limit reached", FailureProviderQuota},
		{"credit balance is too low", FailureProviderAccountUnavailable},
	}, nodeTransportSignals...),
	Args: func(i cliInvocation) []string {
		mode := "acceptEdits"
		if i.Bypass {
			mode = "bypassPermissions"
		}
		args := []string{"--print", "--output-format", "stream-json", "--verbose", "--permission-mode", mode, "--safe-mode"}
		if i.Model() != "" {
			args = append(args, "--model", i.Model())
		}
		// THE TWO NARROW GRANTS, and nothing else.
		//
		// Claude Code's sandbox confines tool access to the directories it was
		// given and gates command execution behind approval. Both defaults are
		// correct and stay on: `--safe-mode` is unchanged and the permission
		// mode is unchanged. What the first live dogfood proved is that a
		// reviewer under those defaults can neither write the typed result the
		// runtime demands - the slot is deliberately outside the candidate
		// workspace - nor run the `go` commands its contract obliges it to run,
		// not even `go version`.
		//
		// --add-dir names exactly the one runtime-owned directory the result
		// goes in. --allowedTools names exactly the executables the contract
		// already requires. Neither is a bypass, neither is arbitrary shell
		// authority, and a stage that needs neither is given neither.
		if i.ScratchDir != "" {
			args = append(args, "--add-dir", i.ScratchDir)
		}
		if i.ResultDir != "" {
			args = append(args, "--add-dir", i.ResultDir)
		}
		if allowed := claudeAllowedTools(i); len(allowed) > 0 {
			args = append(args, "--allowedTools", strings.Join(allowed, " "))
		}
		return claudePromptArg(args, i.Prompt)
	},
	// Claude Code's `plan` permission mode. It is the session mode in which the
	// model may read and reason and may not edit, which is exactly what a
	// planner-role stage needs. The probe requires the quoted choice as the CLI
	// prints it, so a renamed or removed mode makes the agent ineligible for
	// planning rather than quietly running it in an editing mode.
	ReadOnly: &cliReadOnlyMode{
		// Bound to the FLAG that carries it, like the codex and qwen probes.
		// Requiring the quoted token alone was tighter than a bare substring
		// and still not an association: help text that quotes "plan" for any
		// other reason satisfied it.
		Probe: cliHelpProbe{
			Args: []string{"--help"}, Required: []string{"--permission-mode"},
			RequiredChoices: []cliFlagChoice{{Flag: "--permission-mode", Value: "plan"}},
		},
		Mode: "plan",
		Args: func(i cliInvocation) []string {
			args := []string{"--print", "--output-format", "stream-json", "--verbose", "--permission-mode", "plan", "--safe-mode"}
			if i.Model() != "" {
				args = append(args, "--model", i.Model())
			}
			return claudePromptArg(args, i.Prompt)
		},
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
// Gemini also has NO ReadOnly mode here, which is the same kind of honest gap:
// its approval modes bound what is auto-approved rather than proving the model
// cannot write, so this adapter is ineligible for planner-role stages instead
// of claiming a restriction it cannot enforce.
//
// SuppressesWorkspaceInstructions is FALSE here, and that is a real difference
// rather than an oversight: the Gemini CLI exposes no flag that stops it
// loading a GEMINI.md from the working tree. The runtime-owned trusted
// instruction text still frames everything inside the workspace as data, but
// this adapter cannot prove the file was never read, so it does not claim to.
var geminiSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"--help"}, Required: []string{"--prompt", "--approval-mode", "--model", "--extensions"}},
	},
	VersionArgs:    []string{"--version"},
	AuthStatePaths: []string{".gemini/oauth_creds.json"},
	Permission:     cliPermissionModes{Safe: "auto_edit", Bypass: "yolo"},
	Signals: append([]diagnosticSignal{
		{"resource_exhausted", FailureProviderQuota},
		{"quota exceeded", FailureProviderQuota},
	}, nodeTransportSignals...),
	Args: func(i cliInvocation) []string {
		mode := "auto_edit"
		if i.Bypass {
			mode = "yolo"
		}
		args := []string{"--approval-mode", mode, "--extensions", "none"}
		if i.Model() != "" {
			args = append(args, "--model", i.Model())
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
//
// Only the credential file is auth evidence. A settings file or the state
// directory prove that Qwen has been CONFIGURED, which is not the same fact and
// would be reported under the same name - the exact overclaim the other three
// adapters had removed. Where no credential artifact exists the auth mode stays
// unknown, because an honest gap is worth more than a confident guess about
// somebody's session.
var qwenSpec = cliAgentSpec{
	Probes: []cliHelpProbe{
		{Args: []string{"--help"}, Required: []string{"--approval-mode", "--model", "--safe-mode"}},
	},
	VersionArgs:                     []string{"--version"},
	AuthStatePaths:                  []string{".qwen/oauth_creds.json"},
	HomeEnv:                         "QWEN_HOME",
	Permission:                      cliPermissionModes{Safe: "auto-edit", Bypass: "yolo"},
	SuppressesWorkspaceInstructions: true,
	Signals: append([]diagnosticSignal{
		{"resource_exhausted", FailureProviderQuota},
		{"quota exceeded", FailureProviderQuota},
	}, nodeTransportSignals...),
	Args: func(i cliInvocation) []string {
		mode := "auto-edit"
		if i.Bypass {
			mode = "yolo"
		}
		args := []string{"--approval-mode", mode, "--safe-mode"}
		if i.Model() != "" {
			args = append(args, "--model", i.Model())
		}
		return append(args, i.Prompt)
	},
	// Qwen Code inherited Gemini CLI's approval-mode flag and gained a `plan`
	// value of its own. Like every other flag in this file it is PROBED against
	// the installed binary: this adapter is not live-qualified here, so the
	// probe is what stands between an upstream difference and a planning
	// invocation that could write.
	ReadOnly: &cliReadOnlyMode{
		// The flag must be there AND `plan` must appear as a choice rather than
		// as a word in a sentence. A bare substring match on "plan" is
		// satisfied by "planned" or "explanation", which would let this adapter
		// believe in a mode the installed binary does not have.
		Probe: cliHelpProbe{
			Args: []string{"--help"}, Required: []string{"--approval-mode"},
			RequiredChoices: []cliFlagChoice{{Flag: "--approval-mode", Value: "plan"}},
		},
		Mode: "plan",
		Args: func(i cliInvocation) []string {
			args := []string{"--approval-mode", "plan", "--safe-mode"}
			if i.Model() != "" {
				args = append(args, "--model", i.Model())
			}
			return append(args, i.Prompt)
		},
	},
}

// claudePromptArg appends the positional prompt, ENDING OPTION PARSING first.
//
// Claude Code declares --add-dir <directories...> and --allowedTools <tools...>
// as VARIADIC, so its option parser keeps consuming arguments until something
// stops it. A prompt appended after either flag was absorbed into that flag's
// value list and never reached the model at all: a live run of
// `autonomy run issue 77 --agent claude` failed two seconds in with "Input must
// be provided either through stdin or as a prompt argument when using --print".
// Because the shipped operator configuration always declares required_tools,
// the tool grant was always built and so that was EVERY claude_code invocation,
// not an unlucky one.
//
// `--` is the smallest repair that is actually robust. Reordering the vector to
// put the prompt ahead of the variadic flags would fix today's two flags and
// reopen the defect the day a third grant is added after it; the terminator
// does not care what precedes it, and it additionally makes a prompt that
// itself begins with a dash unparseable as a flag. Stdin would also have
// worked and costs far more: sandbox.go leaves cmd.Stdin nil on purpose, so
// that no bounded process can ever block waiting for input.
//
// The terminator is not PROBED the way the flags around it are, because it is
// not a flag any CLI advertises. It does not need to be: a Claude Code that
// stopped honouring it would fail the invocation loudly, exactly as the defect
// above did, rather than leave the session unconstrained - which is the only
// outcome the probe mechanism exists to prevent.
//
// The prompt stays the LAST element, so cliAgentSpec.PromptArgFromEnd remains
// zero and provenance still redacts precisely this argument.
func claudePromptArg(args []string, prompt string) []string {
	return append(args, "--", prompt)
}

// claudeAllowedTools is the least-privilege tool grant for one invocation.
//
// It is built from facts the RUNTIME owns - whether this stage emits a typed
// result, and which executables its contract obliges it to run - never from
// anything a repository or a model can influence. An invocation that needs
// nothing gets no grant at all, which leaves Claude Code's defaults exactly as
// they were.
//
// Bash grants are per-executable rather than a blanket `Bash`, so allowing the
// worker to run `go test` does not also allow it to run anything else.
func claudeAllowedTools(i cliInvocation) []string {
	var allowed []string
	// GIT IS DELIBERATELY NOT GRANTED HERE, and that absence is the #241
	// answer for this provider rather than a gap in it.
	//
	// The first draft of the brokered boundary added `Bash(git *)` so that a
	// bare `git` - which resolves to the runtime's broker - would be the only
	// Git Claude could invoke. That is structurally true and it was still
	// wrong: this allowlist is derived from the invocation's own obligations,
	// so adding a standing grant would have widened the worker's command
	// surface to CREATE the capability the broker then has to guard, and it
	// broke the law that a stage needing neither a directory nor a tool is
	// given neither. #241 excludes permission widening explicitly.
	//
	// So Claude reaches Git only where a contract already obliges it, and then
	// only through the broker, because the guard directory is first on the
	// worker's search path and the brokered sentinel answers every other
	// spelling. The boundary does not rest on this file.
	if i.ResultDir != "" {
		// The typed result is WRITTEN, which needs the Write tool; --add-dir
		// above is what bounds where it may be written to.
		allowed = append(allowed, "Write")
	}
	for _, tool := range i.RequiredTools {
		if tool = strings.TrimSpace(tool); tool != "" {
			allowed = append(allowed, "Bash("+tool+" *)")
		}
	}
	return allowed
}
