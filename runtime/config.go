package runtime

// Configuration is two layers with strictly different authority.
//
// The OPERATOR (global) layer lives outside every repository the runtime works
// on. It is the only place that may authorize a credential, name the AI
// provider, name the assurance sandbox image, choose the state directory, or
// name the operator identity a run is recorded against. A repository the
// runtime has been asked to change therefore cannot rewrite the terms under
// which it is changed, nor who is recorded as having authorized the change.
//
// The REPOSITORY layer is an in-repo file. It may only TIGHTEN: every member it
// is allowed to name is a bound that may be lowered and never raised. There is
// no member for a credential, a provider identity, an operator identity, an
// endpoint, an assurance image, a state directory, or a transport, and
// repositoryScope refuses an unexpected top-level member BEFORE decoding - so
// adding a field to RepositoryConfig cannot by itself hand a repository a new
// authority.
//
// Both layers are strictly decoded the way domain/json.go decodes a contract:
// exactly one JSON value, no duplicate object members, and - additionally here,
// because configuration has no JSON Schema - no unknown members. Each layer
// digests to a stable SHA-256 over its canonical form, so a run records exactly
// which configuration governed it rather than a file path that may have moved.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// OperatorConfigEnv names the operator configuration file when no explicit path
// is supplied. The documented default is <user config dir>/zenchron/config.json.
const OperatorConfigEnv = "ZENCHRON_CONFIG"

// RepositoryConfigFile is the in-repo tighten-only file, read from the
// controller checkout root.
const RepositoryConfigFile = ".zenchron.json"

// Provider kinds. The kind selects which adapter the composition root builds;
// it is operator authority because it decides which remote service sees the
// work and which credential is presented.
const (
	ProviderOpenAI      = "openai"
	ProviderNativeCodex = "native-codex"
)

// GitHub credential modes. "none" is an explicit refusal to authorize forge
// writes, not an anonymous fallback.
const (
	GitHubCredentialCLI  = "github-cli"
	GitHubCredentialNone = "none"
)

// Watch enrolment. Watch observes ONLY repositories an operator listed in the
// global layer, so a repository can never put itself under automation by
// committing a file to itself.
const (
	// DefaultWatchLabel is the opt-in label an issue must carry.
	DefaultWatchLabel = "zenchron:auto"
	// MinWatchPollSeconds is the safe lower bound on the polling interval. It
	// is a floor, not a clamp: a configuration that polls faster is REFUSED,
	// so an operator learns the bound instead of silently getting a different
	// interval than the file states.
	MinWatchPollSeconds = 30
	// DefaultWatchPollSeconds is the interval when configuration names none.
	DefaultWatchPollSeconds = 60
)

// ConfigError is the typed failure for every configuration fault: a missing
// file, a file that is not strict JSON, a member the layer has no authority to
// name, and a bound a repository tried to raise.
type ConfigError struct {
	Path   string
	Detail string
}

func (e *ConfigError) Error() string {
	if e.Path == "" {
		return "configuration: " + e.Detail
	}
	return "configuration " + e.Path + ": " + e.Detail
}

// AssuranceConfig names the verification sandbox. The image is an operator
// decision: it is the program that decides whether a candidate passed.
type AssuranceConfig struct {
	Image              string `json:"image"`
	DockerHost         string `json:"docker_host,omitempty"`
	DependencyCacheDir string `json:"dependency_cache_dir,omitempty"`
}

// ProviderConfig selects the AI provider and points at its credential.
// CredentialPath is deliberately a PATH and never a token value: a path is not
// a usable secret, so configuration that is read, digested, and logged cannot
// carry one.
type ProviderConfig struct {
	Kind           string `json:"kind"`
	Model          string `json:"model"`
	AuthMode       string `json:"auth_mode,omitempty"`
	CredentialPath string `json:"credential_path"`
	Endpoint       string `json:"endpoint,omitempty"`
}

// GitHubConfig selects how the forge credential is obtained. It names a MODE,
// never a secret.
type GitHubConfig struct {
	CredentialMode string `json:"credential_mode"`
	Endpoint       string `json:"endpoint,omitempty"`
}

// BudgetConfig is the operator ceiling for one run.
type BudgetConfig struct {
	WallLimitSeconds     int `json:"wall_limit_seconds"`
	MaxExecutionAttempts int `json:"max_execution_attempts"`
	// MaxExecutionContinuations bounds how many DISTINCT continuation
	// execution bindings one run may start. It is a different resource from
	// MaxExecutionAttempts, which bounds retries of ONE binding, and the two
	// were conflated until #54: productive continuation depth was spent out of
	// the retry budget, so a run doing four pieces of real work in a row was
	// terminated by a ceiling meant for four failures of the same work.
	//
	// It is a POINTER so that ABSENT and an explicit 0 are different facts.
	// They have to be: this is the one budget an operator configuration written
	// before #54 cannot contain, so absent must stay loadable and resolve to
	// the M1 default - but an operator who writes 0 has stated a malformed
	// bound, and every neighbouring budget refuses one. A plain int cannot tell
	// those two apart and would silently accept the malformed spelling.
	//
	// Absent resolves to DefaultMaxExecutionContinuations in resolved; explicit
	// zero and any negative value are refused in validate. No spelling of this
	// field means unbounded.
	MaxExecutionContinuations *int `json:"max_execution_continuations,omitempty"`
	MaxRemediationAttempts    int  `json:"max_remediation_attempts"`
	MaxAssuranceAttempts      int  `json:"max_assurance_attempts"`
}

// DefaultMaxExecutionContinuations is the M1 continuation depth for a new run.
// It is deliberately larger than the retry budget: retries repeat one piece of
// work, continuations are successive pieces of it, and a task needing eight
// productive steps is ordinary rather than pathological.
const DefaultMaxExecutionContinuations = 8

// resolved fills in the budget an operator configuration may omit. It is the
// only defaulting the configuration layer does, and it produces a finite
// positive bound, never an unbounded one.
//
// The default is allocated fresh rather than shared, so a later tighten can
// never write through it into the configuration it was derived from.
func (b BudgetConfig) resolved() BudgetConfig {
	if b.MaxExecutionContinuations == nil {
		fallback := DefaultMaxExecutionContinuations
		b.MaxExecutionContinuations = &fallback
	}
	return b
}

// continuations is the effective bound. It tolerates an unresolved
// configuration so that no reader can panic on a value it did not set.
func (b BudgetConfig) continuations() int {
	if b.MaxExecutionContinuations == nil {
		return DefaultMaxExecutionContinuations
	}
	return *b.MaxExecutionContinuations
}

// WatchConfig is the operator's watch enrolment. Repositories is the complete
// set watch may observe: there is no discovery crawler, so a repository that is
// not listed here is not watched, whatever it says about itself.
type WatchConfig struct {
	Repositories        []string `json:"repositories,omitempty"`
	Label               string   `json:"label,omitempty"`
	PollIntervalSeconds int      `json:"poll_interval_seconds,omitempty"`
	// MaxConcurrentRuns is the operator-authorized run ceiling for watch. It
	// feeds resolveMaxConcurrentRuns as the AUTHORIZED value; the ceiling rule
	// itself lives there and is not restated here.
	MaxConcurrentRuns int `json:"max_concurrent_runs,omitempty"`
}

// GCConfig is the operator's retention window for `autonomy gc`. It is
// operator authority for the same reason a budget ceiling is: a repository
// that could shorten it would be choosing how long the material explaining a
// run against that repository survives. repositoryScope is a stated allowlist
// and "gc" is absent from it, so an in-repo file naming it is refused as an
// authority violation before it is decoded.
//
// The member is omitempty and zero means DefaultGCRetention, so an operator
// who states nothing produces the same canonical configuration - and therefore
// the same configuration digest and the same derived run identities - as
// before this member existed.
type GCConfig struct {
	// RetentionHours is the global retention window in hours. Nothing younger
	// than this is ever eligible for reclamation.
	RetentionHours int `json:"retention_hours,omitempty"`
}

// GCRetention is the effective retention window. It is the only place the
// operator layer's value and the runtime default meet.
func (c OperatorConfig) GCRetention() time.Duration {
	if c.GC.RetentionHours <= 0 {
		return DefaultGCRetention
	}
	return time.Duration(c.GC.RetentionHours) * time.Hour
}

// OperatorConfig is the authorizing layer.
type OperatorConfig struct {
	StateDir         string          `json:"state_dir"`
	ProjectModelPath string          `json:"project_model_path"`
	PolicyPath       string          `json:"policy_path"`
	Assurance        AssuranceConfig `json:"assurance"`
	Provider         ProviderConfig  `json:"provider"`
	GitHub           GitHubConfig    `json:"github"`
	Budgets          BudgetConfig    `json:"budgets"`
	Watch            WatchConfig     `json:"watch,omitempty"`
	// GC is the operator's reclamation window for `autonomy gc`.
	GC GCConfig `json:"gc,omitempty"`
	// Operator names who a run is recorded as having been authorized by. It is
	// operator authority for the same reason a credential is: a repository that
	// could name it would be choosing its own approver. See operator.go.
	Operator OperatorIdentityConfig `json:"operator,omitempty"`
}

// RepositoryBudgets are the only settings a repository may address. Every
// member is a pointer so "absent" is distinguishable from "zero"; zero would
// otherwise read as a request to tighten to nothing.
type RepositoryBudgets struct {
	WallLimitSeconds          *int `json:"wall_limit_seconds,omitempty"`
	MaxExecutionAttempts      *int `json:"max_execution_attempts,omitempty"`
	MaxExecutionContinuations *int `json:"max_execution_continuations,omitempty"`
	MaxRemediationAttempts    *int `json:"max_remediation_attempts,omitempty"`
	MaxAssuranceAttempts      *int `json:"max_assurance_attempts,omitempty"`
}

// RepositoryWatch is the only part of watch a repository may address, and both
// members are bounds it may only LOOSEN for itself: fewer concurrent runs, or a
// LONGER interval between polls. There is deliberately no Repositories member
// and no Label member - enrolment and the opt-in label are operator authority,
// and repositoryWatchScope refuses either name before decoding.
type RepositoryWatch struct {
	PollIntervalSeconds *int `json:"poll_interval_seconds,omitempty"`
	MaxConcurrentRuns   *int `json:"max_concurrent_runs,omitempty"`
}

// RepositoryConfig is the tighten-only layer.
type RepositoryConfig struct {
	Budgets *RepositoryBudgets `json:"budgets,omitempty"`
	Watch   *RepositoryWatch   `json:"watch,omitempty"`
}

// repositoryScope is the complete set of top-level members an in-repo file may
// name. It is enforced separately from struct decoding so that the authority
// boundary is a stated list rather than an emergent property of a struct.
// "operator" is absent on purpose, alongside every credential-bearing member: a
// repository that could name an operator identity would be choosing who is
// recorded as authorizing the change it is asking for.
var repositoryScope = map[string]bool{"budgets": true, "watch": true}

// repositoryWatchScope is the same stated list one level down. "repositories"
// and "label" are absent on purpose: a repository that could name either would
// be enrolling itself into watch, which is exactly the authority this layer
// does not have.
var repositoryWatchScope = map[string]bool{"poll_interval_seconds": true, "max_concurrent_runs": true}

// Config is the governing configuration for one process invocation, together
// with the digest of exactly the two layers that produced it.
type Config struct {
	OperatorConfig
	Digest         ConfigDigest
	OperatorPath   string
	RepositoryPath string
}

// RunBudgets is the runtime-facing form of the effective budgets.
func (c Config) RunBudgets() RunBudgets {
	return RunBudgets{
		WallLimit:                 time.Duration(c.Budgets.WallLimitSeconds) * time.Second,
		MaxExecutionAttempts:      c.Budgets.MaxExecutionAttempts,
		MaxExecutionContinuations: c.Budgets.continuations(),
		MaxRemediationAttempts:    c.Budgets.MaxRemediationAttempts,
		MaxAssuranceAttempts:      c.Budgets.MaxAssuranceAttempts,
	}
}

// OperatorConfigPath resolves the operator configuration file: an explicit path
// wins, then ZENCHRON_CONFIG, then the documented default under the user's
// configuration directory.
func OperatorConfigPath(explicit string) (string, error) {
	if strings.TrimSpace(explicit) != "" {
		return explicit, nil
	}
	if fromEnv := strings.TrimSpace(os.Getenv(OperatorConfigEnv)); fromEnv != "" {
		return fromEnv, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil || dir == "" {
		return "", &ConfigError{Detail: "no operator configuration path was supplied and no user configuration directory is available"}
	}
	return filepath.Join(dir, "zenchron", "config.json"), nil
}

// LoadConfig resolves both layers. repositoryRoot may be empty, which skips the
// repository layer entirely; a missing in-repo file is not an error, but a
// present and invalid one is.
func LoadConfig(explicitPath, repositoryRoot string) (Config, error) {
	operatorPath, err := OperatorConfigPath(explicitPath)
	if err != nil {
		return Config{}, err
	}
	operator, operatorDigest, err := LoadOperatorConfig(operatorPath)
	if err != nil {
		return Config{}, err
	}
	config := Config{OperatorConfig: operator, OperatorPath: operatorPath}
	config.Digest = ConfigDigest{Global: operatorDigest}
	if repositoryRoot == "" {
		return config, nil
	}
	repositoryPath := filepath.Join(repositoryRoot, RepositoryConfigFile)
	repository, repositoryDigest, present, err := LoadRepositoryConfig(repositoryPath)
	if err != nil {
		return Config{}, err
	}
	if !present {
		return config, nil
	}
	tightened, err := operator.Tighten(repository)
	if err != nil {
		if configErr, ok := err.(*ConfigError); ok {
			configErr.Path = repositoryPath
		}
		return Config{}, err
	}
	config.OperatorConfig = tightened
	config.RepositoryPath = repositoryPath
	config.Digest.Repository = repositoryDigest
	return config, nil
}

// LoadOperatorConfig reads, strictly decodes, validates, and digests the
// authorizing layer.
func LoadOperatorConfig(path string) (OperatorConfig, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return OperatorConfig{}, "", &ConfigError{Path: path, Detail: "unreadable operator configuration"}
	}
	var operator OperatorConfig
	if err := decodeStrictConfig(path, data, &operator); err != nil {
		return OperatorConfig{}, "", err
	}
	if err := operator.validate(path); err != nil {
		return OperatorConfig{}, "", err
	}
	// Resolve the one budget that may legitimately be absent BEFORE the digest
	// and before repository tightening, so every later reader - the tighten
	// ceiling, RunBudgets, provenance - sees the same effective number the run
	// will be bounded by. An absent field and an explicit 8 are the same
	// configuration and digest identically, because they are.
	operator.Budgets = operator.Budgets.resolved()
	digest, err := Digest(operator)
	if err != nil {
		return OperatorConfig{}, "", &ConfigError{Path: path, Detail: err.Error()}
	}
	return operator, digest, nil
}

// LoadRepositoryConfig reads, strictly decodes, and digests the tighten-only
// layer. An absent file reports present=false with no error.
func LoadRepositoryConfig(path string) (RepositoryConfig, string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return RepositoryConfig{}, "", false, nil
	}
	if err != nil {
		return RepositoryConfig{}, "", false, &ConfigError{Path: path, Detail: "unreadable repository configuration"}
	}
	// The scope check runs before decoding, so a member this layer has no
	// authority to name is refused as an authority violation and never as an
	// incidental unknown field.
	var members map[string]json.RawMessage
	if err := decodeStrictConfig(path, data, &members); err != nil {
		return RepositoryConfig{}, "", false, err
	}
	if err := refuseOutOfScope(path, members, repositoryScope, ""); err != nil {
		return RepositoryConfig{}, "", false, err
	}
	if watch, ok := members["watch"]; ok {
		var watchMembers map[string]json.RawMessage
		if err := decodeStrictConfig(path, watch, &watchMembers); err != nil {
			return RepositoryConfig{}, "", false, err
		}
		if err := refuseOutOfScope(path, watchMembers, repositoryWatchScope, "watch."); err != nil {
			return RepositoryConfig{}, "", false, err
		}
	}
	var repository RepositoryConfig
	if err := decodeStrictConfig(path, data, &repository); err != nil {
		return RepositoryConfig{}, "", false, err
	}
	digest, err := Digest(repository)
	if err != nil {
		return RepositoryConfig{}, "", false, &ConfigError{Path: path, Detail: err.Error()}
	}
	return repository, digest, true, nil
}

// refuseOutOfScope reports a member the in-repo layer has no authority to name
// as an authority violation rather than an incidental unknown field.
func refuseOutOfScope(path string, members map[string]json.RawMessage, scope map[string]bool, prefix string) error {
	for member := range members {
		if !scope[member] {
			return &ConfigError{Path: path, Detail: fmt.Sprintf("repository configuration may not set %q: it is operator authority", prefix+member)}
		}
	}
	return nil
}

// Tighten applies a repository layer to an operator layer. A proposed bound may
// only move DOWN. Raising one is REFUSED rather than clamped: silently handing
// back the operator's number would let a repository probe the ceiling and never
// learn it had been overruled.
func (c OperatorConfig) Tighten(repository RepositoryConfig) (OperatorConfig, error) {
	tightened := c
	// An absent budgets member proposes nothing; it must not skip the rest of
	// the layer, which is why this is a nil-safe zero rather than an early
	// return.
	budgets := repository.Budgets
	if budgets == nil {
		budgets = &RepositoryBudgets{}
	}
	// The continuation ceiling is tightened through a local copy: the operator
	// value is behind a pointer that `tightened := c` shares with c, so writing
	// through it would silently retighten the configuration it came from.
	continuations := tightened.Budgets.continuations()
	proposals := []struct {
		name     string
		proposed *int
		ceiling  *int
	}{
		{"budgets.wall_limit_seconds", budgets.WallLimitSeconds, &tightened.Budgets.WallLimitSeconds},
		{"budgets.max_execution_attempts", budgets.MaxExecutionAttempts, &tightened.Budgets.MaxExecutionAttempts},
		{"budgets.max_execution_continuations", budgets.MaxExecutionContinuations, &continuations},
		{"budgets.max_remediation_attempts", budgets.MaxRemediationAttempts, &tightened.Budgets.MaxRemediationAttempts},
		{"budgets.max_assurance_attempts", budgets.MaxAssuranceAttempts, &tightened.Budgets.MaxAssuranceAttempts},
	}
	for _, proposal := range proposals {
		if proposal.proposed == nil {
			continue
		}
		if *proposal.proposed < 1 {
			return OperatorConfig{}, &ConfigError{Detail: fmt.Sprintf("%s must be at least 1", proposal.name)}
		}
		if *proposal.proposed > *proposal.ceiling {
			return OperatorConfig{}, &ConfigError{Detail: fmt.Sprintf("repository configuration may only tighten %s: %d exceeds the operator bound %d", proposal.name, *proposal.proposed, *proposal.ceiling)}
		}
		*proposal.ceiling = *proposal.proposed
	}
	tightened.Budgets.MaxExecutionContinuations = &continuations
	if err := tightened.tightenWatch(repository.Watch); err != nil {
		return OperatorConfig{}, err
	}
	return tightened, nil
}

// tightenWatch is the same tighten-only rule for the two watch bounds. Polling
// is inverted and stated inverted: a repository may only ask to be polled LESS
// often, so the proposal must be a LONGER interval than the operator's. Because
// the operator interval is already at or above MinWatchPollSeconds, this rule
// also makes a below-the-floor proposal from a repository unreachable.
func (c *OperatorConfig) tightenWatch(watch *RepositoryWatch) error {
	if watch == nil {
		return nil
	}
	settings, err := c.WatchSettings()
	if err != nil {
		return &ConfigError{Detail: err.Error()}
	}
	if proposed := watch.MaxConcurrentRuns; proposed != nil {
		if *proposed < 1 {
			return &ConfigError{Detail: "watch.max_concurrent_runs must be at least 1"}
		}
		if *proposed > settings.MaxConcurrentRuns {
			return &ConfigError{Detail: fmt.Sprintf("repository configuration may only tighten watch.max_concurrent_runs: %d exceeds the operator bound %d", *proposed, settings.MaxConcurrentRuns)}
		}
		c.Watch.MaxConcurrentRuns = *proposed
	}
	if proposed := watch.PollIntervalSeconds; proposed != nil {
		operatorSeconds := int(settings.PollInterval / time.Second)
		if *proposed < operatorSeconds {
			return &ConfigError{Detail: fmt.Sprintf("repository configuration may only tighten watch.poll_interval_seconds: polling every %ds is more often than the operator interval of %ds", *proposed, operatorSeconds)}
		}
		c.Watch.PollIntervalSeconds = *proposed
	}
	return nil
}

// WatchSettings is the EFFECTIVE watch configuration: the operator layer with
// defaults applied and every repository identity validated. Defaults are
// applied here and never written back into the decoded layer, so a digest keeps
// recording the file as written.
//
// It is also the validator: operator validation calls it, so there is one
// statement of what a usable watch configuration is.
type WatchSettings struct {
	// Repositories is the complete enrolled set, in configuration order.
	Repositories []GitHubRepo
	// Label is the opt-in label an issue must carry to be picked up.
	Label string
	// PollInterval is at or above MinWatchPollSeconds.
	PollInterval time.Duration
	// MaxConcurrentRuns is the resolved ceiling; see resolveMaxConcurrentRuns.
	MaxConcurrentRuns int
}

// validRepositoryPart bounds one side of owner/name to the characters GitHub
// itself allows. GovernedRemote validates a URL, where the host is a separate
// component; a bare "owner/name" has no such separation, so "git@host:owner"
// would otherwise pass as an owner. This is a character class, not a second
// parser: the parse is still parseGitHubRepo plus GovernedRemote.
func validRepositoryPart(part string) bool {
	if part == "" || part == "." || part == ".." {
		return false
	}
	for _, r := range part {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return false
		}
	}
	return true
}

func (c OperatorConfig) WatchSettings() (WatchSettings, error) {
	settings := WatchSettings{
		Label:             strings.TrimSpace(c.Watch.Label),
		PollInterval:      time.Duration(c.Watch.PollIntervalSeconds) * time.Second,
		MaxConcurrentRuns: resolveMaxConcurrentRuns(0, c.Watch.MaxConcurrentRuns),
	}
	if settings.Label == "" {
		settings.Label = DefaultWatchLabel
	}
	if c.Watch.PollIntervalSeconds == 0 {
		settings.PollInterval = DefaultWatchPollSeconds * time.Second
	}
	if settings.PollInterval < MinWatchPollSeconds*time.Second {
		return WatchSettings{}, fmt.Errorf("watch.poll_interval_seconds must be at least %d", MinWatchPollSeconds)
	}
	enrolled := map[string]bool{}
	for _, entry := range c.Watch.Repositories {
		repo, err := parseGitHubRepo(strings.TrimSpace(entry))
		if err == nil {
			// identity() is GovernedRemote applied to the clone URL, so watch
			// enrolment accepts exactly the owner/name shapes the runtime is
			// already allowed to contact - there is no second parser.
			_, err = repo.identity()
		}
		if err == nil && !(validRepositoryPart(repo.Owner) && validRepositoryPart(repo.Name)) {
			err = fmt.Errorf("%q is not owner/name", entry)
		}
		if err != nil {
			return WatchSettings{}, fmt.Errorf("watch.repositories: %w", err)
		}
		// GitHub owner/name are case-insensitive, so two spellings of one
		// repository are one enrolment and are refused as the duplicate they
		// are rather than becoming two watchers of the same source.
		key := strings.ToLower(repo.String())
		if enrolled[key] {
			return WatchSettings{}, fmt.Errorf("watch.repositories: %q is enrolled more than once", repo.String())
		}
		enrolled[key] = true
		settings.Repositories = append(settings.Repositories, repo)
	}
	return settings, nil
}

func (c OperatorConfig) validate(path string) error {
	refuse := func(detail string) error { return &ConfigError{Path: path, Detail: detail} }
	for _, required := range []struct{ name, value string }{
		{"state_dir", c.StateDir},
		{"project_model_path", c.ProjectModelPath},
		{"policy_path", c.PolicyPath},
	} {
		if strings.TrimSpace(required.value) == "" {
			return refuse(required.name + " is required")
		}
		if !filepath.IsAbs(required.value) {
			return refuse(required.name + " must be an absolute path")
		}
	}
	if !strings.HasPrefix(c.Assurance.Image, "sha256:") {
		return refuse("assurance.image must be a sha256: digest, so the verifying program is pinned")
	}
	if c.Assurance.DependencyCacheDir != "" && !filepath.IsAbs(c.Assurance.DependencyCacheDir) {
		return refuse("assurance.dependency_cache_dir must be an absolute path")
	}
	if c.Provider.Kind != ProviderOpenAI && c.Provider.Kind != ProviderNativeCodex {
		return refuse(fmt.Sprintf("provider.kind must be %q or %q", ProviderOpenAI, ProviderNativeCodex))
	}
	if strings.TrimSpace(c.Provider.Model) == "" {
		return refuse("provider.model is required")
	}
	if !filepath.IsAbs(c.Provider.CredentialPath) {
		return refuse("provider.credential_path must be an absolute path to an operator-controlled credential")
	}
	if c.GitHub.CredentialMode != GitHubCredentialCLI && c.GitHub.CredentialMode != GitHubCredentialNone {
		return refuse(fmt.Sprintf("github.credential_mode must be %q or %q", GitHubCredentialCLI, GitHubCredentialNone))
	}
	for _, bound := range []struct {
		name  string
		value int
	}{
		{"budgets.wall_limit_seconds", c.Budgets.WallLimitSeconds},
		{"budgets.max_execution_attempts", c.Budgets.MaxExecutionAttempts},
		{"budgets.max_remediation_attempts", c.Budgets.MaxRemediationAttempts},
		{"budgets.max_assurance_attempts", c.Budgets.MaxAssuranceAttempts},
	} {
		if bound.value < 1 {
			return refuse(bound.name + " must be at least 1")
		}
	}
	// max_execution_continuations is bounded exactly like its neighbours: a
	// STATED value must be at least 1, so an explicit 0 is refused like an
	// explicit 0 anywhere else in this block. The single difference is that it
	// may be ABSENT, because operator configurations written before #54 cannot
	// contain it and refusing those files would make the field's introduction
	// an outage. Absent resolves to the M1 default in BudgetConfig.resolved.
	if stated := c.Budgets.MaxExecutionContinuations; stated != nil && *stated < 1 {
		return refuse("budgets.max_execution_continuations must be at least 1")
	}
	if c.GC.RetentionHours < 0 {
		return refuse("gc.retention_hours must not be negative")
	}
	if _, err := c.WatchSettings(); err != nil {
		return refuse(err.Error())
	}
	return nil
}

// LoadEngineeringPolicy loads a strict, schema-validated EngineeringPolicy. It
// is the policy counterpart of analysis.LoadProjectModel and shares its shape:
// read, domain.Decode, no partial value on failure.
func LoadEngineeringPolicy(path string) (domain.EngineeringPolicy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return domain.EngineeringPolicy{}, fmt.Errorf("read EngineeringPolicy: %w", err)
	}
	policy, err := domain.Decode[domain.EngineeringPolicy](data)
	if err != nil {
		return domain.EngineeringPolicy{}, fmt.Errorf("load EngineeringPolicy: %w", err)
	}
	return policy, nil
}

// decodeStrictConfig decodes exactly one JSON value with no duplicate object
// members and no unknown members.
func decodeStrictConfig(path string, data []byte, out any) error {
	if err := rejectDuplicateConfigMembers(data); err != nil {
		return &ConfigError{Path: path, Detail: err.Error()}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return &ConfigError{Path: path, Detail: err.Error()}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return &ConfigError{Path: path, Detail: "input must contain exactly one JSON value"}
	}
	return nil
}

func rejectDuplicateConfigMembers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	return walkConfigMembers(decoder, "")
}

func walkConfigMembers(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter == '{' {
		seen := map[string]bool{}
		for decoder.More() {
			key, err := decoder.Token()
			if err != nil {
				return err
			}
			member, ok := key.(string)
			if !ok {
				return errors.New("JSON object member name must be a string")
			}
			if seen[member] {
				return &domain.DuplicateMemberError{Path: path, Member: member}
			}
			seen[member] = true
			if err := walkConfigMembers(decoder, path+"/"+member); err != nil {
				return err
			}
		}
	} else {
		for decoder.More() {
			if err := walkConfigMembers(decoder, path); err != nil {
				return err
			}
		}
	}
	_, err = decoder.Token()
	return err
}
