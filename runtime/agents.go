package runtime

// An AGENT is a stable operator-chosen name for one execution worker. It is
// deliberately three separate facts rather than one:
//
//	agent id        the name an operator types and a run is bound to
//	provider kind   which adapter the composition root builds
//	trust mode      what the runtime may claim about the boundary around it
//
// Collapsing any two of them is how a configuration change silently becomes a
// trust change. An operator who renames an agent must not thereby move it into
// another trust mode, and a run that recorded `codex` must keep meaning the
// agent the operator called `codex` even after the underlying executable, model
// or version moves on.
//
// A FUTURE engineering ROLE - planner, implementer, reviewer - is a fourth
// concept and is deliberately absent here. #63 builds the workforce; #64 builds
// the planner that decides which workforce a piece of work needs. The seam this
// file leaves for it is capability metadata on a named agent, not a role
// catalogue: AgentIdentity and AgentReadiness are what a planner would reason
// over, and nothing in the kernel branches on either.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Provider kinds. The kind decides which adapter is constructed, which remote
// service or local executable sees the work, and which credential - if any -
// is presented. It is operator authority for exactly that reason.
const (
	// AgentKindCodexCLI drives the operator's installed, already-authenticated
	// Codex CLI.
	AgentKindCodexCLI = "codex_cli"
	// AgentKindClaudeCode drives the operator's installed Claude Code CLI.
	AgentKindClaudeCode = "claude_code"
	// AgentKindGeminiCLI drives the operator's installed Gemini CLI.
	AgentKindGeminiCLI = "gemini_cli"
	// AgentKindQwenCLI drives an installed, already-agentic Qwen coding CLI.
	// It is deliberately an AGENTIC CLI and never a raw completion endpoint: a
	// completion endpoint cannot edit a file, so supporting one would mean
	// building a second coding harness inside this repository. That is a
	// separate issue, and #63 does not need it.
	AgentKindQwenCLI = "qwen_cli"
	// AgentKindOpenAIResponses is the brokered OpenAI Responses provider. It
	// is the one PROTECTED kind: its isolation properties are proven and it
	// fails closed when they are not.
	AgentKindOpenAIResponses = "openai_responses"
)

// TrustMode is what the runtime may honestly claim about the boundary around
// one execution worker. It is not a capability and never an authority: an
// operator_trusted agent may author a change and still cannot authorize it.
type TrustMode string

const (
	// TrustOperatorTrusted is a tool the local operator installed,
	// authenticated and chose to run. Its candidate WRITE scope is bounded
	// wherever the provider supports it and it receives no publication
	// credential from Zenchron, but its host READ confinement is UNPROVEN and
	// is never converted to proven. Using it is operator authorization, not
	// evidence of isolation.
	TrustOperatorTrusted TrustMode = "operator_trusted"
	// TrustProtected is the stricter brokered architecture, whose claimed
	// filesystem, network and credential isolation is proven before use.
	TrustProtected TrustMode = "protected"
)

// agentKinds is the complete catalogue, with the trust mode each kind is
// entitled to claim. The trust mode is a property of the ADAPTER, not of
// configuration: a repository - or an operator typo - must not be able to move
// a native CLI into `protected` merely by writing the word, and an operator who
// states the wrong one gets a refusal rather than a silent correction.
var agentKinds = map[string]TrustMode{
	AgentKindCodexCLI:        TrustOperatorTrusted,
	AgentKindClaudeCode:      TrustOperatorTrusted,
	AgentKindGeminiCLI:       TrustOperatorTrusted,
	AgentKindQwenCLI:         TrustOperatorTrusted,
	AgentKindOpenAIResponses: TrustProtected,
}

// AgentKinds lists the configurable kinds in deterministic order. It exists for
// diagnostics and documentation; nothing decides anything from it.
func AgentKinds() []string {
	kinds := make([]string, 0, len(agentKinds))
	for kind := range agentKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// nativeCLIKind reports whether a kind is driven as a local process the
// operator already authenticated, rather than as a brokered remote provider.
func nativeCLIKind(kind string) bool {
	return kind != AgentKindOpenAIResponses && agentKinds[kind] != ""
}

// LegacyAgentID is the agent name a configuration written before #63 resolves
// to. There are exactly two, one per pre-#63 provider kind, and they are the
// provider ids those adapters already recorded - so an old run's transcripts,
// diagnostics and provenance keep addressing the same agent under the new
// model instead of being renamed by a migration.
const (
	LegacyAgentCodex  = "native-codex"
	LegacyAgentOpenAI = "openai-responses"
)

// AgentConfig is one named worker in the operator layer.
//
// Every credential-shaped member here is a PATH or a MODE and never a value,
// for the same reason ProviderConfig has always been: configuration is read,
// digested, printed by doctor and reported in status, so nothing in it may be a
// usable secret.
type AgentConfig struct {
	Kind      string `json:"kind"`
	TrustMode string `json:"trust_mode"`
	// Command is the executable. Empty means the kind's default program name,
	// resolved on the operator's PATH. An absolute path pins one exact
	// program; a relative path containing a separator is refused, because it
	// would resolve against whatever directory the runtime happens to be in.
	Command string `json:"command,omitempty"`
	// Model is the model this agent should ask its provider for. Empty means
	// the provider's own configured default, which is a truthful answer for a
	// CLI the operator has already configured.
	Model string `json:"model,omitempty"`
	// Home is the directory holding the provider's OWN authentication and
	// session state - the operator's real home for an already-authenticated
	// CLI, or a runtime-owned directory when the operator wants the agent
	// pinned to a separate profile. Empty means the operator's home.
	//
	// It is never copied into a candidate and never becomes a candidate
	// environment variable. See cli_agent.go for the environment allowlist.
	Home string `json:"home,omitempty"`
	// CredentialPath belongs to protected providers, which present a
	// Zenchron-held credential to a remote API. A native CLI authenticates
	// itself, so naming one here is refused rather than ignored: it would
	// state a Zenchron-held API credential for a worker Zenchron is
	// deliberately NOT authenticating.
	CredentialPath string `json:"credential_path,omitempty"`
	// Endpoint overrides the protected provider's API endpoint.
	Endpoint string `json:"endpoint,omitempty"`
	// AllowPermissionBypass is the operator's standing permission for this
	// agent to be invoked in its provider's unsafe bypass mode. It grants
	// nothing on its own: an invocation must ALSO request the bypass
	// explicitly, and the resulting attempt provenance records it forever. Two
	// independent statements are required precisely so a bypass can never be
	// reached by a default.
	AllowPermissionBypass bool `json:"allow_permission_bypass,omitempty"`
	// Unattended states whether this agent may be started by unattended
	// scheduling (supervisor discovery), as opposed to by an explicit operator
	// command. It is a POINTER so absent and an explicit false are different
	// statements; absent defaults to true for agents an operator configured
	// deliberately, and explicit false keeps an agent to explicit work only.
	Unattended *bool `json:"unattended,omitempty"`
}

// ResolvedAgent is one agent after defaulting and validation. It is the value
// every consumer reads; nothing consults raw AgentConfig outside this file.
type ResolvedAgent struct {
	ID                    string
	Kind                  string
	TrustMode             TrustMode
	Command               string
	Model                 string
	Home                  string
	CredentialPath        string
	Endpoint              string
	AllowPermissionBypass bool
	Unattended            bool
	// DeclaredAuthMode is an operator-STATED authentication mode. It exists
	// only for the pre-#63 provider block, which had one; a #63 agent has no
	// such member because an operator asserting how a third-party CLI
	// authenticates is a claim, not an observation, and the adapter observes
	// it instead. When it is set the provenance records it with
	// AuthSourceConfigured, so a stated mode is never mistaken for a measured
	// one.
	DeclaredAuthMode string
	// Legacy marks an agent SYNTHESIZED from a pre-#63 `provider` block rather
	// than named in `agents`. It preserves the old adapter's exact environment
	// semantics, which are not the ones a newly configured agent gets - see
	// cli_agent.go - so an old configuration keeps behaving the way it did.
	Legacy bool
}

// NativeCLI reports whether this agent is a local process the operator already
// authenticated.
func (a ResolvedAgent) NativeCLI() bool { return nativeCLIKind(a.Kind) }

// AgentIdentity is what a provider reports about itself for provenance. It is
// small on purpose: it is the part of an agent that a durable record and a
// future planner both need, and it contains nothing a provider could use to
// claim authority.
type AgentIdentity struct {
	AgentID   string    `json:"agent_id"`
	Kind      string    `json:"provider_kind"`
	TrustMode TrustMode `json:"trust_mode"`
	Model     string    `json:"model,omitempty"`
}

// AgentDescriptor is the optional capability of naming yourself. A provider
// that does not implement it is legal and simply contributes no agent identity;
// nothing infers one from a Go type name.
type AgentDescriptor interface{ Identity() AgentIdentity }

// AgentReadiness is a NON-BILLABLE readiness observation. Nothing here costs an
// inference call: an executable is found or not, a version string is printed or
// not, an authentication state file exists or not.
//
// AuthMode may legitimately be AuthModeUnknown. Zenchron can prove which
// adapter and executable IT invoked; it cannot always prove how a third-party
// CLI authenticated internally, and inventing "subscription" merely because
// Zenchron supplied no API key would be a fabricated fact.
type AgentReadiness struct {
	Available bool   `json:"available"`
	Version   string `json:"version,omitempty"`
	// Detail is a bounded, runtime-authored reason. It never carries provider
	// output verbatim beyond boundedDetail's ceiling and never a credential.
	Detail string `json:"detail,omitempty"`
	// AuthMode and AuthModeSource are the observation and its provenance. The
	// source is how the observation was made, which is what lets an operator
	// judge how much the claim is worth.
	AuthMode       string `json:"auth_mode,omitempty"`
	AuthModeSource string `json:"auth_mode_source,omitempty"`
}

// Authentication-mode observations. These are OBSERVATIONS with stated
// provenance, never assertions about a provider's internal billing.
const (
	// AuthModeUnknown is the honest default. It is not a failure.
	AuthModeUnknown = "unknown"
	// AuthModeLocalCLISession is an installed CLI whose own on-disk session or
	// authentication state was observed to exist. It says the CLI holds its own
	// authentication; it does not say which plan pays for it.
	AuthModeLocalCLISession = "local_cli_session"
	// AuthModeAPIKeyFile is a Zenchron-held operator credential file presented
	// to a remote API by a protected provider.
	AuthModeAPIKeyFile = "api_key_file"
)

// Authentication-mode provenance. The source is deliberately about the METHOD
// of observation.
const (
	AuthSourceProviderState = "provider_state_observed"
	AuthSourceConfigured    = "operator_configured"
	AuthSourceUnobserved    = "not_observed"
)

// AgentProber is the optional capability of answering "are you usable" without
// spending anything. Doctor and `autonomy agents` consume it; the reconcile
// loop never does, because readiness at planning time is not readiness at
// execution time and pretending otherwise would be a stale promise.
type AgentProber interface {
	Probe(ctx context.Context) AgentReadiness
}

// AgentRegistry is the resolved, validated set of named agents plus the
// operator's default. It is immutable once built.
type AgentRegistry struct {
	agents       map[string]ResolvedAgent
	order        []string
	defaultAgent string
	// legacy marks a registry synthesized from a pre-#63 `provider` block.
	legacy bool
}

// UnknownAgentError names an agent an operator asked for that this
// configuration does not define. It lists what IS defined, because the useful
// answer to a typo is the set of real names.
type UnknownAgentError struct {
	ID        string
	Available []string
}

func (e *UnknownAgentError) Error() string {
	if len(e.Available) == 0 {
		return "unknown agent " + strconv.Quote(e.ID) + "; no agents are configured"
	}
	return "unknown agent " + strconv.Quote(e.ID) + "; configured agents are " + strings.Join(e.Available, ", ")
}

// IDs lists configured agent ids in deterministic order.
func (r AgentRegistry) IDs() []string { return append([]string(nil), r.order...) }

// Default is the operator's default agent id, or empty when none is resolved.
func (r AgentRegistry) Default() string { return r.defaultAgent }

// Legacy reports a registry derived from a pre-#63 single-provider
// configuration rather than from an explicit `agents` block.
func (r AgentRegistry) Legacy() bool { return r.legacy }

// Agent resolves one id. An empty id resolves the default, which is what makes
// `autonomy run issue N` without --agent work.
func (r AgentRegistry) Agent(id string) (ResolvedAgent, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		id = r.defaultAgent
	}
	if id == "" {
		return ResolvedAgent{}, &UnknownAgentError{ID: "", Available: r.IDs()}
	}
	agent, ok := r.agents[id]
	if !ok {
		return ResolvedAgent{}, &UnknownAgentError{ID: id, Available: r.IDs()}
	}
	return agent, nil
}

// All lists every resolved agent in deterministic order.
func (r AgentRegistry) All() []ResolvedAgent {
	agents := make([]ResolvedAgent, 0, len(r.order))
	for _, id := range r.order {
		agents = append(agents, r.agents[id])
	}
	return agents
}

// validAgentID bounds the name an operator may give a worker. It is the same
// shape a path component and a JSON member can both hold without escaping, so
// an agent id can address its own artifact namespace and appear in a status
// projection without a second encoding rule.
func validAgentID(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' || c == '_':
			// A leading separator would make an id that reads as a flag.
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// AgentRegistry resolves the effective agent set for this configuration.
//
// It has exactly two shapes and never a blend of them. An `agents` block is the
// stated registry. Its ABSENCE is a pre-#63 configuration, which is migrated by
// synthesizing exactly one agent from the `provider` block under that provider's
// own historical identity and trust mode - never reinterpreted as a different
// one, and never quietly joined by extra built-in agents the operator did not
// configure.
func (c OperatorConfig) AgentRegistry() (AgentRegistry, error) {
	if len(c.Agents) == 0 {
		return c.legacyAgentRegistry()
	}
	registry := AgentRegistry{agents: make(map[string]ResolvedAgent, len(c.Agents))}
	for id := range c.Agents {
		registry.order = append(registry.order, id)
	}
	sort.Strings(registry.order)
	for _, id := range registry.order {
		agent, err := resolveAgent(id, c.Agents[id])
		if err != nil {
			return AgentRegistry{}, err
		}
		registry.agents[id] = agent
	}
	defaultAgent := strings.TrimSpace(c.DefaultAgent)
	switch {
	case defaultAgent != "":
		if _, ok := registry.agents[defaultAgent]; !ok {
			return AgentRegistry{}, &ConfigError{Detail: fmt.Sprintf("default_agent %q is not one of the configured agents (%s)", defaultAgent, strings.Join(registry.order, ", "))}
		}
	case len(registry.order) == 1:
		// One configured agent is an unambiguous default. Requiring the
		// operator to name it twice would be ceremony, not safety.
		defaultAgent = registry.order[0]
	default:
		return AgentRegistry{}, &ConfigError{Detail: fmt.Sprintf("default_agent is required when more than one agent is configured (%s)", strings.Join(registry.order, ", "))}
	}
	registry.defaultAgent = defaultAgent
	return registry, nil
}

// legacyAgentRegistry migrates a pre-#63 single-provider configuration. The
// mapping is explicit and total: each historical provider kind becomes exactly
// one agent, under the id that provider already recorded, in the trust mode it
// already had.
func (c OperatorConfig) legacyAgentRegistry() (AgentRegistry, error) {
	var agent ResolvedAgent
	switch c.Provider.Kind {
	case ProviderNativeCodex:
		agent = ResolvedAgent{
			ID: LegacyAgentCodex, Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted,
			Command: defaultAgentCommand(AgentKindCodexCLI), Model: c.Provider.Model,
			// The pre-#63 adapter treated provider.credential_path as a
			// runtime-owned Codex home and exported it as both HOME and
			// CODEX_HOME. That is preserved exactly, which is why Legacy
			// exists: a newly configured codex_cli agent does NOT get that
			// environment, and silently changing an existing operator's
			// working configuration is not a migration.
			Home: c.Provider.CredentialPath, DeclaredAuthMode: c.Provider.AuthMode,
			Unattended: true, Legacy: true,
		}
	case ProviderOpenAI:
		agent = ResolvedAgent{
			ID: LegacyAgentOpenAI, Kind: AgentKindOpenAIResponses, TrustMode: TrustProtected,
			Model: c.Provider.Model, CredentialPath: c.Provider.CredentialPath,
			Endpoint: c.Provider.Endpoint, DeclaredAuthMode: c.Provider.AuthMode,
			Unattended: true, Legacy: true,
		}
	default:
		return AgentRegistry{}, &ConfigError{Detail: fmt.Sprintf("provider.kind %q has no agent migration", c.Provider.Kind)}
	}
	return AgentRegistry{
		agents:       map[string]ResolvedAgent{agent.ID: agent},
		order:        []string{agent.ID},
		defaultAgent: agent.ID,
		legacy:       true,
	}, nil
}

// resolveAgent validates one configured agent and applies the defaults that are
// safe to apply. Every refusal names the member, because a configuration fault
// an operator cannot locate is an outage.
func resolveAgent(id string, config AgentConfig) (ResolvedAgent, error) {
	member := func(name string) string { return "agents." + id + "." + name }
	refuse := func(detail string) (ResolvedAgent, error) { return ResolvedAgent{}, &ConfigError{Detail: detail} }
	if !validAgentID(id) {
		return refuse(fmt.Sprintf("agent id %q must be 1-64 characters of a-z, 0-9, '-' or '_' and may not start with a separator", id))
	}
	trust, known := agentKinds[config.Kind]
	if !known {
		return refuse(fmt.Sprintf("%s must be one of %s, got %q", member("kind"), strings.Join(AgentKinds(), ", "), config.Kind))
	}
	// The trust mode is STATED by the operator and CHECKED against the kind,
	// rather than defaulted from it. A configuration that says nothing about
	// trust is a configuration whose security posture nobody wrote down.
	if TrustMode(config.TrustMode) != trust {
		return refuse(fmt.Sprintf("%s must be %q for kind %q, got %q; a provider's trust mode is a property of its adapter and cannot be raised or lowered by configuration",
			member("trust_mode"), trust, config.Kind, config.TrustMode))
	}
	agent := ResolvedAgent{
		ID: id, Kind: config.Kind, TrustMode: trust,
		Model:                 strings.TrimSpace(config.Model),
		AllowPermissionBypass: config.AllowPermissionBypass,
		Unattended:            config.Unattended == nil || *config.Unattended,
	}
	command := strings.TrimSpace(config.Command)
	if nativeCLIKind(config.Kind) {
		if command == "" {
			command = defaultAgentCommand(config.Kind)
		}
		// A bare program name is resolved on the operator's PATH; an absolute
		// path pins one exact program. A relative path with a separator is
		// neither, and would resolve against whatever directory the process
		// happens to be in - including, for a candidate-adjacent cwd, one the
		// work being done could write to.
		if strings.ContainsRune(command, filepath.Separator) && !filepath.IsAbs(command) {
			return refuse(member("command") + " must be a bare program name or an absolute path, not a relative path")
		}
		agent.Command = command
		if home := strings.TrimSpace(config.Home); home != "" {
			if !filepath.IsAbs(home) {
				return refuse(member("home") + " must be an absolute path to the directory holding that CLI's own authentication state")
			}
			agent.Home = home
		}
		if strings.TrimSpace(config.CredentialPath) != "" {
			return refuse(member("credential_path") + " is not valid for a native CLI agent: the CLI authenticates itself and Zenchron holds no credential for it")
		}
		if strings.TrimSpace(config.Endpoint) != "" {
			return refuse(member("endpoint") + " is not valid for a native CLI agent")
		}
		return agent, nil
	}
	if command != "" {
		return refuse(member("command") + " is not valid for a brokered provider agent")
	}
	if strings.TrimSpace(config.Home) != "" {
		return refuse(member("home") + " is not valid for a brokered provider agent")
	}
	if config.AllowPermissionBypass {
		return refuse(member("allow_permission_bypass") + " is not valid for a brokered provider agent: its boundary is proven, not bypassable")
	}
	credential := strings.TrimSpace(config.CredentialPath)
	if credential == "" || !filepath.IsAbs(credential) {
		return refuse(member("credential_path") + " must be an absolute path to an operator-controlled credential")
	}
	if strings.TrimSpace(config.Model) == "" {
		return refuse(member("model") + " is required for a brokered provider agent")
	}
	agent.CredentialPath = credential
	agent.Endpoint = strings.TrimSpace(config.Endpoint)
	return agent, nil
}

// defaultAgentCommand is the program name a kind resolves to when the operator
// names none. It is the CLI's own conventional name, which is what an operator
// who already installed and authenticated it has on their PATH.
func defaultAgentCommand(kind string) string {
	switch kind {
	case AgentKindCodexCLI:
		return "codex"
	case AgentKindClaudeCode:
		return "claude"
	case AgentKindGeminiCLI:
		return "gemini"
	case AgentKindQwenCLI:
		return "qwen"
	default:
		return ""
	}
}

// ---------------------------------------------------------------------------
// Operator-facing readiness
// ---------------------------------------------------------------------------

// AgentStatus is one configured worker as `autonomy agents` shows it. It exists
// so an operator can answer "can I actually give work to this" without starting
// a run and without spending an inference call finding out.
type AgentStatus struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	TrustMode TrustMode `json:"trust_mode"`
	// Endpoint classifies WHAT is contacted: a local executable by name, or a
	// remote brokered API. It is a classification and never a credential.
	Endpoint string `json:"endpoint"`
	Model    string `json:"model,omitempty"`
	Default  bool   `json:"default"`
	AgentReadiness
	// Eligible reports whether an operator may give this agent work
	// explicitly, and Unattended whether the supervisor's automatic intake may
	// start work on it. They are separate because they are separate decisions:
	// an operator running an agent by hand is not the same as leaving it to
	// start work on its own.
	Eligible   bool `json:"eligible_for_explicit_work"`
	Unattended bool `json:"eligible_for_unattended_work"`
	// PermissionBypassAllowed surfaces standing operator permission for the
	// provider's unsafe mode. It is shown even when no run has used it,
	// because a standing permission is itself a posture an operator should see.
	PermissionBypassAllowed bool `json:"permission_bypass_allowed,omitempty"`
	// Legacy marks an agent synthesized from a pre-#63 provider block.
	Legacy bool `json:"legacy,omitempty"`
}

// DescribeAgents answers readiness for every configured agent. The prober is
// injected so this stays free of provider construction - and so a test answers
// it without any executable at all.
//
// Nothing here spends money. Readiness is an executable being found, a
// capability being advertised, a version string being printed and a credential
// file existing; a paid call is never made to establish that a worker exists.
func DescribeAgents(ctx context.Context, registry AgentRegistry, prober func(ResolvedAgent) AgentProber) []AgentStatus {
	statuses := make([]AgentStatus, 0, len(registry.IDs()))
	for _, agent := range registry.All() {
		status := AgentStatus{
			ID: agent.ID, Kind: agent.Kind, TrustMode: agent.TrustMode,
			Endpoint: agentEndpoint(agent), Model: agent.Model,
			Default: agent.ID == registry.Default(), Legacy: agent.Legacy,
			PermissionBypassAllowed: agent.AllowPermissionBypass,
		}
		if prober != nil {
			if probe := prober(agent); probe != nil {
				status.AgentReadiness = probe.Probe(ctx)
			}
		}
		if status.Detail == "" && !status.Available {
			status.Detail = "no readiness probe is configured for this agent kind"
		}
		status.Eligible = status.Available
		status.Unattended = status.Available && agent.Unattended
		statuses = append(statuses, status)
	}
	return statuses
}

// agentEndpoint classifies what the agent contacts, without naming a
// credential or a secret-bearing URL.
func agentEndpoint(agent ResolvedAgent) string {
	if agent.NativeCLI() {
		return "local executable " + agent.Command
	}
	if agent.Endpoint != "" {
		return "brokered API (operator-configured endpoint)"
	}
	return "brokered API (provider default endpoint)"
}

// credentialFileProber is the readiness of a brokered provider: the operator
// credential the runtime itself presents exists and is owner-only. Its contents
// are never read, and no request is made - an account's ability to execute work
// can only be learned by making a paid call, which a readiness listing must
// never do.
type credentialFileProber struct{ Path string }

func (p credentialFileProber) Probe(context.Context) AgentReadiness {
	readiness := AgentReadiness{AuthMode: AuthModeAPIKeyFile, AuthModeSource: AuthSourceConfigured}
	if strings.TrimSpace(p.Path) == "" {
		readiness.Detail = "no provider credential is configured"
		return readiness
	}
	info, err := os.Stat(p.Path)
	switch {
	case err != nil:
		readiness.Detail = "the configured provider credential cannot be inspected"
	case !info.Mode().IsRegular():
		readiness.Detail = "the configured provider credential is not a regular file"
	case info.Mode().Perm()&0o077 != 0:
		readiness.Detail = "the configured provider credential is readable by other users; run chmod 600 on it"
	default:
		readiness.Available = true
		readiness.Detail = "the operator credential exists and is owner-only; its contents were not read, and no request was made, so this proves the credential is CONFIGURED and not that the account can execute work"
	}
	return readiness
}

// AgentProberFor builds the readiness probe for one agent. It is the composition
// root's provider-specific surface for readiness, and it is the ONLY place
// outside agent_specs.go that has to know a kind exists: a native CLI is probed
// by finding it and reading what it advertises, a brokered provider by
// inspecting the operator credential the runtime itself would present.
func AgentProberFor(agent ResolvedAgent, artifacts ArtifactStore, operatorHome string) AgentProber {
	if agent.NativeCLI() {
		return CLIAgentProvider{
			Agent: agent, ArtifactStore: artifacts, OperatorHome: operatorHome,
			LegacyEnvironment: agent.Legacy && agent.Kind == AgentKindCodexCLI,
		}
	}
	return credentialFileProber{Path: agent.CredentialPath}
}
