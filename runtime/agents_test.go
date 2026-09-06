package runtime

// Every test here drives the agent registry and the native CLI adapters through
// a fake CommandExecutor. No real coding CLI is started, no model call is made,
// and nothing here can spend money.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// fakeAgentExecutor models an installed CLI. help is what the modeled program
// advertises, which is exactly what the capability probe reads: a fixture
// missing a required flag models a CLI whose permission mode cannot be
// established, and the adapter must refuse rather than run.
type fakeAgentExecutor struct {
	calls   []recordedCommand
	help    string
	version string
	found   bool
	err     error
	outputs []CommandOutput
}

func (f *fakeAgentExecutor) LookPath(string) error {
	if f.found {
		return nil
	}
	return errors.New("missing")
}

func (f *fakeAgentExecutor) Run(_ context.Context, name string, args []string, dir string, env []string, _ time.Duration) (CommandOutput, error) {
	f.record(name, args, dir, env)
	if len(f.outputs) > 0 {
		out := f.outputs[0]
		f.outputs = f.outputs[1:]
		return out, f.err
	}
	return CommandOutput{}, f.err
}

func (f *fakeAgentExecutor) Output(_ context.Context, name string, args []string, dir string, env []string, _ time.Duration) (CommandOutput, error) {
	f.record(name, args, dir, env)
	if len(args) == 1 && args[0] == "--version" {
		return CommandOutput{Stdout: []byte(f.version)}, nil
	}
	return CommandOutput{Stdout: []byte(f.help)}, nil
}

func (f *fakeAgentExecutor) record(name string, args []string, dir string, env []string) {
	f.calls = append(f.calls, recordedCommand{
		name: name,
		args: append([]string(nil), args...),
		dir:  dir,
		env:  append([]string(nil), env...),
	})
}

// execution returns the single recorded candidate invocation: the call that is
// not a --help probe and not a --version read.
func (f *fakeAgentExecutor) execution(t *testing.T) recordedCommand {
	t.Helper()
	for _, call := range f.calls {
		if len(call.args) == 0 {
			continue
		}
		last := call.args[len(call.args)-1]
		if last == "--help" || last == "--version" {
			continue
		}
		return call
	}
	t.Fatalf("no candidate invocation was recorded: %#v", f.calls)
	return recordedCommand{}
}

// capableHelp advertises every capability all four adapters require, so one
// fixture serves every kind and a test that degrades it degrades one exact
// flag.
const capableHelp = `Options:
  -c, --config <key=value>
  -s, --sandbox <SANDBOX_MODE>  [possible values: read-only, workspace-write, danger-full-access]
  -a, --ask-for-approval <APPROVAL_POLICY>
      --ignore-user-config
  -C, --cd <DIR>
  -p, --print
      --prompt <TEXT>
      --permission-mode <MODE>
      --approval-mode <MODE>
      --safe-mode
      --extensions <NAME>
  -m, --model <MODEL>
`

// agentFixture builds one native CLI agent over a fake executor, with a home
// directory that exists and a candidate workspace that exists.
func agentFixture(t *testing.T, kind string) (CLIAgentProvider, ExecutionRequest, *fakeAgentExecutor) {
	t.Helper()
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	home := filepath.Join(root, "home")
	for _, dir := range []string{candidate, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeAgentExecutor{found: true, help: capableHelp, version: "1.2.3\n"}
	provider := CLIAgentProvider{
		Agent: ResolvedAgent{
			ID: "worker", Kind: kind, TrustMode: TrustOperatorTrusted,
			Command: defaultAgentCommand(kind), Model: "test-model", Unattended: true,
		},
		ArtifactStore: ArtifactStore{Root: filepath.Join(root, "artifacts")},
		Executor:      fake,
		OperatorHome:  home,
	}
	request := ExecutionRequest{
		RunID: "run", OperationID: "run:execution.invoke:initial|1|base-sha", Attempt: 1,
		SourceSnapshot: Ref{ID: "issue-snapshot", Revision: "1"}, ControllerID: "controller",
		Base:      Ref{ID: "base", Revision: "base-sha"},
		Candidate: Candidate{Revision: "candidate-sha", Tree: "tree-sha"}, CandidateDir: candidate,
		Contract: Ref{ID: "contract", Revision: "4"}, Objective: "fix",
		TrustedInstructions: "trusted-only", Purpose: InvocationInitial,
	}
	return provider, request, fake
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

// TestLegacyProviderMigratesToOneAgentUnderItsOwnIdentity is the migration
// law: a configuration written before the registry existed keeps naming the
// same worker, in the same trust mode, so its transcripts and provenance stay
// addressable.
func TestLegacyProviderMigratesToOneAgentUnderItsOwnIdentity(t *testing.T) {
	for name, tc := range map[string]struct {
		provider  ProviderConfig
		wantID    string
		wantKind  string
		wantTrust TrustMode
	}{
		"native codex": {
			provider:  ProviderConfig{Kind: ProviderNativeCodex, Model: "m", CredentialPath: "/operator/codex-home"},
			wantID:    LegacyAgentCodex,
			wantKind:  AgentKindCodexCLI,
			wantTrust: TrustOperatorTrusted,
		},
		"brokered openai": {
			provider:  ProviderConfig{Kind: ProviderOpenAI, Model: "m", CredentialPath: "/operator/key"},
			wantID:    LegacyAgentOpenAI,
			wantKind:  AgentKindOpenAIResponses,
			wantTrust: TrustProtected,
		},
	} {
		t.Run(name, func(t *testing.T) {
			registry, err := OperatorConfig{Provider: tc.provider}.AgentRegistry()
			if err != nil {
				t.Fatal(err)
			}
			if !registry.Legacy() || registry.Default() != tc.wantID {
				t.Fatalf("legacy registry default = %q legacy=%v, want %q true", registry.Default(), registry.Legacy(), tc.wantID)
			}
			agent, err := registry.Agent("")
			if err != nil {
				t.Fatal(err)
			}
			if agent.ID != tc.wantID || agent.Kind != tc.wantKind || agent.TrustMode != tc.wantTrust || !agent.Legacy {
				t.Fatalf("migrated agent = %#v", agent)
			}
			if len(registry.IDs()) != 1 {
				t.Fatalf("migration invented extra agents: %v", registry.IDs())
			}
		})
	}
}

// TestLegacyConfigurationCanonicalizesWithoutTheNewMembers is the digest-
// stability law. Run identity is derived from the operator configuration
// digest, so a configuration written before #63 has to canonicalize exactly as
// it did before these members existed - otherwise every historical run would
// acquire a new identity on upgrade.
func TestLegacyConfigurationCanonicalizesWithoutTheNewMembers(t *testing.T) {
	config := OperatorConfig{
		StateDir: "/state", ProjectModelPath: "/model.json", PolicyPath: "/policy.json",
		Provider: ProviderConfig{Kind: ProviderNativeCodex, Model: "m", CredentialPath: "/codex"},
	}
	canonical, err := CanonicalJSON(config)
	if err != nil {
		t.Fatal(err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &members); err != nil {
		t.Fatal(err)
	}
	for _, added := range []string{"agents", "default_agent"} {
		if _, present := members[added]; present {
			t.Fatalf("member %q entered the canonical form of a configuration that does not set it: %s", added, canonical)
		}
	}
	if _, present := members["provider"]; !present {
		t.Fatalf("the pre-#63 provider member left the canonical form: %s", canonical)
	}
}

func TestAgentRegistryRefusesIncoherentConfiguration(t *testing.T) {
	valid := AgentConfig{Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)}
	for name, tc := range map[string]struct {
		config OperatorConfig
		detail string
	}{
		"unknown kind": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: "telepathy", TrustMode: "operator_trusted"}}},
			detail: "kind",
		},
		"trust mode raised by configuration": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: AgentKindClaudeCode, TrustMode: string(TrustProtected)}}},
			detail: "trust_mode",
		},
		"trust mode lowered by configuration": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: AgentKindOpenAIResponses, TrustMode: string(TrustOperatorTrusted)}}},
			detail: "trust_mode",
		},
		"credential named for a self-authenticating CLI": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted), CredentialPath: "/key"}}},
			detail: "credential_path",
		},
		"relative command path": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted), Command: "./bin/codex"}}},
			detail: "command",
		},
		"relative home": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted), Home: "home"}}},
			detail: "home",
		},
		"invalid agent id": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"Codex CLI": valid}},
			detail: "agent id",
		},
		"ambiguous default": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": valid, "b": valid}},
			detail: "default_agent",
		},
		"default names an agent that does not exist": {
			config: OperatorConfig{Agents: map[string]AgentConfig{"a": valid}, DefaultAgent: "b"},
			detail: "default_agent",
		},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := tc.config.AgentRegistry()
			if err == nil {
				t.Fatal("incoherent agent configuration was accepted")
			}
			if !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("refusal does not name %q: %v", tc.detail, err)
			}
		})
	}
}

// TestProviderAndAgentsAreMutuallyExclusive keeps ONE statement of which
// workers exist. Two would need a precedence rule, and a precedence rule
// nobody wrote down is how an operator ends up running a different worker than
// the one they think they configured.
func TestProviderAndAgentsAreMutuallyExclusive(t *testing.T) {
	config := OperatorConfig{
		StateDir: "/state", ProjectModelPath: "/m.json", PolicyPath: "/p.json",
		Assurance: AssuranceConfig{Image: "sha256:" + strings.Repeat("a", 64)},
		Provider:  ProviderConfig{Kind: ProviderNativeCodex, Model: "m", CredentialPath: "/codex"},
		Agents: map[string]AgentConfig{
			"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)},
		},
		GitHub:  GitHubConfig{CredentialMode: GitHubCredentialNone},
		Budgets: BudgetConfig{WallLimitSeconds: 1, MaxExecutionAttempts: 1, MaxRemediationAttempts: 1, MaxAssuranceAttempts: 1},
	}
	err := config.validate("/config.json")
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("both configuration shapes were accepted at once: %v", err)
	}
}

func TestSingleConfiguredAgentIsTheDefault(t *testing.T) {
	registry, err := OperatorConfig{Agents: map[string]AgentConfig{
		"codex": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)},
	}}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if registry.Default() != "codex" {
		t.Fatalf("default = %q, want the only configured agent", registry.Default())
	}
	if _, err := registry.Agent("gemini"); err == nil {
		t.Fatal("an unconfigured agent resolved")
	}
	var unknown *UnknownAgentError
	if _, err := registry.Agent("gemini"); !errors.As(err, &unknown) || len(unknown.Available) != 1 {
		t.Fatalf("unknown agent refusal is not typed with the real names: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Native CLI adapters
// ---------------------------------------------------------------------------

// TestEveryNativeAgentRequestsItsLeastPrivilegeMode is the core trust-mode
// law: the runtime selects the least privilege that still permits the work,
// and never the provider's bypass, in ONE table so a new adapter cannot be
// added without stating both.
func TestEveryNativeAgentRequestsItsLeastPrivilegeMode(t *testing.T) {
	for kind, want := range map[string]struct {
		safe   []string
		bypass []string
	}{
		AgentKindCodexCLI: {
			safe:   []string{"--ask-for-approval never", "--sandbox workspace-write", "exec", "--ignore-user-config", "-c sandbox_workspace_write.network_access=false", "-c project_doc_max_bytes=0"},
			bypass: []string{"danger-full-access", "--dangerously-bypass-approvals-and-sandbox", "--full-auto"},
		},
		AgentKindClaudeCode: {
			safe:   []string{"--print", "--permission-mode acceptEdits", "--safe-mode"},
			bypass: []string{"bypassPermissions", "--dangerously-skip-permissions", "--bare"},
		},
		AgentKindGeminiCLI: {
			safe:   []string{"--approval-mode auto_edit", "--prompt"},
			bypass: []string{"yolo", "--yolo"},
		},
		AgentKindQwenCLI: {
			safe:   []string{"--approval-mode auto-edit", "--safe-mode"},
			bypass: []string{"yolo", "--yolo", "--bare", "--insecure"},
		},
	} {
		t.Run(kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, kind)
			if _, err := provider.Execute(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			args := strings.Join(fake.execution(t).args, " ")
			for _, required := range want.safe {
				if !strings.Contains(args, required) {
					t.Errorf("least-privilege constraint %q was not requested: %s", required, args)
				}
			}
			for _, forbidden := range want.bypass {
				if strings.Contains(args, forbidden) {
					t.Errorf("the adapter reached for the bypass %q on an ordinary run: %s", forbidden, args)
				}
			}
		})
	}
}

// TestNativeAgentRunsInTheCandidateWorkspace proves the workspace binding for
// both shapes: the CLI that takes a flag gets the exact directory, and the
// three that do not are started with it as their working directory.
func TestNativeAgentRunsInTheCandidateWorkspace(t *testing.T) {
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode, AgentKindGeminiCLI, AgentKindQwenCLI} {
		t.Run(kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, kind)
			result, err := provider.Execute(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			call := fake.execution(t)
			if call.dir != request.CandidateDir {
				t.Fatalf("working directory %q is not the candidate workspace %q", call.dir, request.CandidateDir)
			}
			if result.Invocation == nil {
				t.Fatal("no invocation provenance was recorded")
			}
			if result.Invocation.WorkspaceBound != cliAgentSpecs[kind].WorkingDirectoryFlag {
				t.Fatalf("workspace_bound = %v but this adapter %v pass a working-directory flag",
					result.Invocation.WorkspaceBound, cliAgentSpecs[kind].WorkingDirectoryFlag)
			}
			if result.Invocation.WorkspaceBound && !strings.Contains(strings.Join(call.args, " "), request.CandidateDir) {
				t.Fatalf("the working-directory flag did not name the candidate: %#v", call.args)
			}
		})
	}
}

// TestNativeAgentFailsClosedWithoutTheCapabilityItDependsOn is why the flag
// list is stated twice. A CLI that does not advertise the permission flag this
// runtime is about to pass is UNAVAILABLE; it is never run in whatever mode it
// happens to default to.
func TestNativeAgentFailsClosedWithoutTheCapabilityItDependsOn(t *testing.T) {
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode, AgentKindGeminiCLI, AgentKindQwenCLI} {
		t.Run(kind, func(t *testing.T) {
			for name, degrade := range map[string]func(*fakeAgentExecutor){
				"cli absent": func(f *fakeAgentExecutor) { f.found = false },
				"permission flag unadvertised": func(f *fakeAgentExecutor) {
					f.help = strings.NewReplacer(
						"--permission-mode", "--permit",
						"--approval-mode", "--approve",
						"--ask-for-approval", "--approve-for-me",
					).Replace(f.help)
				},
			} {
				t.Run(name, func(t *testing.T) {
					provider, request, fake := agentFixture(t, kind)
					degrade(fake)
					if _, err := provider.Execute(context.Background(), request); !errors.Is(err, ErrSandboxUnavailable) {
						t.Fatalf("want fail-closed capability refusal, got %v", err)
					}
					for _, call := range fake.calls {
						if len(call.args) > 0 && call.args[len(call.args)-1] == request.CandidateDir {
							t.Fatalf("the adapter executed without a proven permission mode: %#v", call.args)
						}
					}
				})
			}
		})
	}
}

// TestPermissionBypassNeedsTwoIndependentStatements is the bypass law. Neither
// operator configuration alone nor an invocation request alone reaches the
// unsafe mode, and when both are present the durable provenance says so.
func TestPermissionBypassNeedsTwoIndependentStatements(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindClaudeCode)
	provider.PermissionBypass = true

	var refused *PermissionBypassRefusedError
	if _, err := provider.Execute(context.Background(), request); !errors.As(err, &refused) {
		t.Fatalf("an unauthorized bypass was not refused: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Fatalf("a refused bypass still started a process: %#v", fake.calls)
	}

	provider.Agent.AllowPermissionBypass = true
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Invocation.PermissionBypass || result.Invocation.PermissionMode != "bypassPermissions" {
		t.Fatalf("an authorized bypass is not distinguishable in provenance: %#v", result.Invocation)
	}
	if !strings.Contains(strings.Join(fake.execution(t).args, " "), "bypassPermissions") {
		t.Fatalf("the authorized bypass was not actually requested: %#v", fake.execution(t).args)
	}

	// The ordinary run must remain distinguishable from it forever.
	constrained, _, _ := agentFixture(t, AgentKindClaudeCode)
	ordinary, err := constrained.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if ordinary.Invocation.PermissionBypass || ordinary.Invocation.PermissionMode != "acceptEdits" {
		t.Fatalf("a constrained run is not distinguishable from a bypass: %#v", ordinary.Invocation)
	}
}

// TestNativeAgentEnvironmentIsAnAllowlistWithoutBillingOverrides is the
// billing-truth law. Every one of these CLIs treats its API-key variable as an
// override that displaces the operator's interactive subscription session, so
// an environment built from scratch is what keeps a supervised subscription run
// from silently becoming metered API usage.
func TestNativeAgentEnvironmentIsAnAllowlistWithoutBillingOverrides(t *testing.T) {
	for _, ambient := range []string{
		"CODEX_API_KEY", "CODEX_ACCESS_TOKEN", "OPENAI_API_KEY",
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
		"GEMINI_API_KEY", "GOOGLE_API_KEY",
		"GITHUB_TOKEN", "GH_TOKEN", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY",
	} {
		t.Setenv(ambient, "leaked-value-9c3")
	}
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode, AgentKindGeminiCLI, AgentKindQwenCLI} {
		t.Run(kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, kind)
			if _, err := provider.Execute(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			for _, call := range fake.calls {
				for _, entry := range call.env {
					name, value, _ := strings.Cut(entry, "=")
					if name != "PATH" && name != "HOME" {
						t.Fatalf("environment is not an allowlist: %q reached %s", name, kind)
					}
					if strings.Contains(value, "leaked-value-9c3") {
						t.Fatalf("an ambient credential reached %s through %s", kind, name)
					}
				}
			}
		})
	}
}

// TestPinnedAgentHomeIsExportedAndOperatorHomeIsNot states the difference
// between an agent using the operator's own already-authenticated CLI state and
// one pinned to a separate profile.
func TestPinnedAgentHomeIsExportedAndOperatorHomeIsNot(t *testing.T) {
	provider, request, fake := agentFixture(t, AgentKindCodexCLI)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(fake.execution(t).env, " "), "CODEX_HOME=") {
		t.Fatalf("an agent using the operator's own home redirected the CLI's state: %#v", fake.execution(t).env)
	}

	pinned, request, fake := agentFixture(t, AgentKindCodexCLI)
	pinned.Agent.Home = t.TempDir()
	if _, err := pinned.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	env := strings.Join(fake.execution(t).env, " ")
	if !strings.Contains(env, "CODEX_HOME="+pinned.Agent.Home) || !strings.Contains(env, "HOME="+pinned.Agent.Home) {
		t.Fatalf("a pinned agent home was not bound: %#v", fake.execution(t).env)
	}
}

// TestInvocationProvenanceRecordsThePostureWithoutThePrompt is the provenance
// law: every security-relevant flag survives forever, and the untrusted prompt
// text does not enter a canonical payload.
func TestInvocationProvenanceRecordsThePostureWithoutThePrompt(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindCodexCLI)
	request.Objective = "a very distinctive objective phrase 9c3"
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	provenance := result.Invocation
	if provenance == nil {
		t.Fatal("no invocation provenance was recorded")
	}
	if provenance.AgentID != "worker" || provenance.ProviderKind != AgentKindCodexCLI || provenance.TrustMode != TrustOperatorTrusted {
		t.Fatalf("agent identity is not recorded: %#v", provenance)
	}
	if provenance.Version != "1.2.3" || provenance.Executable != "codex" {
		t.Fatalf("executable identity is not recorded: %#v", provenance)
	}
	if provenance.SandboxMode != "workspace-write" || provenance.PermissionMode == "" {
		t.Fatalf("effective sandbox and permission mode are not recorded: %#v", provenance)
	}
	joined := strings.Join(provenance.Argv, " ")
	if strings.Contains(joined, "9c3") {
		t.Fatalf("untrusted prompt text entered the durable argv: %s", joined)
	}
	if !strings.Contains(joined, "[prompt]") || !strings.Contains(joined, "--sandbox") {
		t.Fatalf("argv lost either the prompt placeholder or the security flags: %s", joined)
	}
	if len(provenance.PromptSHA256) != 64 {
		t.Fatalf("the prompt is not addressable by digest: %q", provenance.PromptSHA256)
	}
	// The canonical payload ceiling is what makes this a bound rather than a
	// hope: provenance has to fit in an event.
	encoded, err := CanonicalJSON(provenance)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxCanonicalPayloadBytes {
		t.Fatalf("invocation provenance does not fit the canonical payload ceiling: %d bytes", len(encoded))
	}
}

// TestAuthModeIsObservedOrUnknownAndNeverInvented is the auth-truth law. A
// missing observation is `unknown`, never "subscription" inferred from the fact
// that Zenchron supplied no API key.
func TestAuthModeIsObservedOrUnknownAndNeverInvented(t *testing.T) {
	provider, request, _ := agentFixture(t, AgentKindClaudeCode)
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Invocation.AuthMode != AuthModeUnknown || result.Invocation.AuthModeSource != AuthSourceUnobserved {
		t.Fatalf("an unobserved authentication state was reported as something else: %#v", result.Invocation)
	}

	observed, request, _ := agentFixture(t, AgentKindClaudeCode)
	if err := os.MkdirAll(filepath.Join(observed.OperatorHome, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(observed.OperatorHome, ".claude", ".credentials.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err = observed.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Invocation.AuthMode != AuthModeLocalCLISession || result.Invocation.AuthModeSource != AuthSourceProviderState {
		t.Fatalf("an observed CLI session was not recorded as one: %#v", result.Invocation)
	}
}

// TestAgentFailureClassificationIsRecognizedOrUnknown keeps an unrecognized
// diagnostic fail-closed. Guessing a wait out of an unknown error is how a
// terminal fault becomes an endless one.
func TestAgentFailureClassificationIsRecognizedOrUnknown(t *testing.T) {
	for _, tc := range []struct {
		diagnostic string
		want       FailureClass
	}{
		{"Error: 429 Too Many Requests", FailureProviderRateLimited},
		{"you've hit your usage limit reached for this window", FailureProviderQuota},
		{"selected model is at capacity", FailureTransientProvider},
		{"permission denied", FailureUnknown},
	} {
		provider, request, fake := agentFixture(t, AgentKindCodexCLI)
		fake.err = errors.New("cli exited non-zero")
		fake.outputs = []CommandOutput{{Stdout: []byte(tc.diagnostic), ExitCode: 1}}
		result, err := provider.Execute(context.Background(), request)
		if err == nil {
			t.Fatalf("provider failure was not surfaced for %q", tc.diagnostic)
		}
		if result.Failure == nil || result.Failure.Classification != tc.want {
			t.Fatalf("classification for %q = %#v, want %q", tc.diagnostic, result.Failure, tc.want)
		}
		if RouteFailure(tc.want) == RouteWait && !waitRoutesWithoutBurningAnAttempt(tc.want) {
			t.Fatalf("a capacity wait for %q would consume an engineering attempt", tc.diagnostic)
		}
	}
}

// waitRoutesWithoutBurningAnAttempt states the property the reconciler
// implements: a wait-routed class restores the scheduler attempt, so provider
// capacity never spends a run's remediation budget.
func waitRoutesWithoutBurningAnAttempt(class FailureClass) bool {
	return waitReasons[class] != ""
}

// TestNativeAgentIsNeverProtectedEligible keeps the trust boundary honest: no
// native CLI, in any mode, satisfies the protected isolation requirement.
func TestNativeAgentIsNeverProtectedEligible(t *testing.T) {
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode, AgentKindGeminiCLI, AgentKindQwenCLI} {
		provider, _, _ := agentFixture(t, kind)
		if err := RequireProtectedIsolation(provider); err == nil {
			t.Fatalf("%s was admitted for protected autonomous execution", kind)
		}
		if provider.Isolation().FilesystemRead == IsolationProven {
			t.Fatalf("%s claimed proven host read confinement", kind)
		}
	}
}
