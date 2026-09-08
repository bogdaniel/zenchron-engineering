package runtime

// NativeCodexProvider is the pre-#63 Codex adapter, kept as the MIGRATION
// surface for an operator configuration written before the agent registry
// existed. It is no longer an implementation: every behaviour below is the
// shared CLIAgentProvider lifecycle plus the codexSpec argument grammar, so
// there is exactly one native-CLI execution path and this type only supplies
// the legacy identity and the legacy environment.
//
// Two legacy properties are preserved deliberately, because migrating an
// operator's configuration must not change how their runs actually execute:
//
//   - The agent id stays "native-codex", which is the provider id the old
//     adapter recorded. Attempt transcripts, execution diagnostics and run
//     provenance therefore keep addressing the same worker instead of being
//     renamed by a migration.
//   - provider.credential_path is exported as BOTH HOME and CODEX_HOME, which
//     is what the old adapter did. A newly configured codex_cli agent instead
//     uses the operator's own home, because "use the CLI you already
//     authenticated" is what #63 asks for; changing an existing operator's
//     working setup underneath them is not a migration.
//
// The trust boundary is unchanged and is stated in cli_agent.go: Codex
// workspace-write bounds writes and denies tool network access, it does not
// confine reads of runtime state, the controller checkout, other runs or
// credentials, and this adapter has no independent mechanism to enforce that.
// Read confinement therefore stays UNPROVEN and RequireProtectedIsolation
// refuses this adapter for protected autonomous execution.

import (
	"context"
	"time"
)

type NativeCodexProvider struct {
	ArtifactStore   ArtifactStore
	Model, AuthMode string
	// CodexHome is the runtime-owned directory that holds the Codex provider
	// credential. Provider authentication is deliberately modeled as a path and
	// never as a token value: a path is not a credential, so it cannot leak a
	// usable secret into candidate command environments the way an exported
	// token would, and the credential itself stays on the control plane where
	// candidate commands never observe it. There is intentionally no token
	// field; a configuration that is not an existing runtime-owned directory
	// (an inline credential, for example) is refused.
	CodexHome string
	Executor  CommandExecutor
	Grace     time.Duration
}

// provider is the shared adapter this legacy surface delegates to.
func (p NativeCodexProvider) provider() CLIAgentProvider {
	return CLIAgentProvider{
		Agent: ResolvedAgent{
			ID: LegacyAgentCodex, Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted,
			Command: defaultAgentCommand(AgentKindCodexCLI), Model: p.Model,
			Home: p.CodexHome, DeclaredAuthMode: p.AuthMode, Unattended: true, Legacy: true,
		},
		ArtifactStore:     p.ArtifactStore,
		Executor:          p.Executor,
		Grace:             p.Grace,
		LegacyEnvironment: true,
	}
}

// codexRequiredExecFlags and codexRequiredRootFlags are the capabilities the
// Codex adapter depends on, read back from the ONE spec that states them. A
// doctor fixture that modelled an installed CLI would otherwise restate the
// flag list, and a spec change would leave the fixture advertising capabilities
// the adapter no longer asks for.
var (
	codexRequiredExecFlags = codexSpec.Probes[0].Required
	codexRequiredRootFlags = codexSpec.Probes[1].Required
)

func (p NativeCodexProvider) Isolation() ProviderIsolation { return p.provider().Isolation() }

// env is the child environment this legacy adapter would build. It stays a
// method here because the frozen credential-boundary test asserts the
// allowlist through it.
func (p NativeCodexProvider) env() []string {
	provider := p.provider()
	return provider.env(codexSpec, p.CodexHome)
}

// Identity reports this worker under its legacy agent id.
func (p NativeCodexProvider) Identity() AgentIdentity { return p.provider().Identity() }

// Probe answers readiness without spending anything.
func (p NativeCodexProvider) Probe(ctx context.Context) AgentReadiness {
	return p.provider().Probe(ctx)
}

// probe is the capability gate DiagnoseSandbox reads. It stays a method on this
// type because doctor's frozen sandbox diagnosis is written against it.
func (p NativeCodexProvider) probe(ctx context.Context) error {
	provider := p.provider()
	spec, err := provider.spec()
	if err != nil {
		return err
	}
	home, err := provider.home()
	if err != nil {
		return ErrSandboxUnavailable
	}
	return provider.probe(ctx, spec, home)
}

func (p NativeCodexProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	return p.provider().Execute(ctx, request)
}
