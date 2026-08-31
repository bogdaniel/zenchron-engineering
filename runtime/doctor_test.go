package runtime

// Every test here is offline. No network is contacted, no Docker daemon is
// required, and no provider inference call is possible: the fake provider
// panics if Execute is ever reached, and the fake forge embeds a nil
// GitHubAdapter so any call other than the one read-only discovery GET panics
// too. Those panics are the proof, not a comment claiming it.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

const (
	doctorImage = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	// doctorSecret is deliberately distinctive so an accidental appearance
	// anywhere in the report is unambiguous.
	doctorSecret = "ZENCHRON-DOCTOR-FAKE-SECRET-9f3a2b"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// doctorExecutor answers exactly the probes DiagnoseSandbox makes. available
// false is a host with neither docker nor codex installed; toolchainMissing
// models the fifth dogfood's real image - one that starts containers happily
// and then cannot resolve go on the runtime sandbox path.
type doctorExecutor struct {
	available        bool
	toolchainMissing bool
}

func (e doctorExecutor) LookPath(name string) error {
	if e.available && (name == "docker" || name == "codex") {
		return nil
	}
	return errors.New("executable file not found: " + name)
}

func (e doctorExecutor) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	return e.Output(ctx, name, args, dir, env, grace)
}

func (e doctorExecutor) Output(_ context.Context, name string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	joined := strings.Join(args, " ")
	switch {
	case !e.available:
		return CommandOutput{ExitCode: 127}, errors.New("not installed")
	case name == "docker" && strings.HasPrefix(joined, "info"):
		return CommandOutput{Stdout: []byte("27.1.1\n")}, nil
	case name == "docker" && strings.HasPrefix(joined, "image inspect"):
		return CommandOutput{Stdout: []byte(doctorImage + "\n")}, nil
	case name == "codex":
		return CommandOutput{Stdout: []byte(strings.Join(append(append([]string{}, codexRequiredExecFlags...), codexRequiredRootFlags...), " "))}, nil
	// The toolchain probe is a real container run, so the whole bounded
	// lifecycle is modeled rather than special-cased.
	case name == "docker" && strings.HasPrefix(joined, "create"):
		return CommandOutput{}, nil
	case name == "docker" && strings.HasPrefix(joined, "start"):
		if e.toolchainMissing {
			return CommandOutput{ExitCode: 127, Stderr: []byte("sh: 1: go: not found\n")}, errors.New("container exited 127")
		}
		return CommandOutput{Stdout: []byte("/usr/local/go/bin/go\n/usr/local/go/bin/gofmt\ngo version go1.25.14 linux/arm64\n")}, nil
	case name == "docker" && strings.HasPrefix(joined, "inspect"):
		// The container already exited and was removed, which is what ends the
		// bounded run; the daemon says so on stderr.
		return CommandOutput{ExitCode: 1, Stderr: []byte("Error: No such object\n")}, errors.New("no such object")
	case name == "docker" && (strings.HasPrefix(joined, "wait") || strings.HasPrefix(joined, "rm")):
		return CommandOutput{}, nil
	}
	return CommandOutput{ExitCode: 1}, errors.New("unexpected command " + name + " " + joined)
}

// doctorProviderFake reports whatever isolation the test wants proven. Execute
// panics: a preflight that made a paid inference call would fail here loudly.
type doctorProviderFake struct{ isolation ProviderIsolation }

func (doctorProviderFake) Execute(context.Context, ExecutionRequest) (ExecutionResult, error) {
	panic("doctor must never make a provider inference call")
}
func (p doctorProviderFake) Isolation() ProviderIsolation { return p.isolation }

func doctorProvenIsolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead:  IsolationProven,
		FilesystemWrite: IsolationProven,
		NetworkDenied:   IsolationProven,
		CredentialScope: IsolationProven,
	}
}

type doctorCredential struct {
	secret string
	err    error
}

func (c doctorCredential) Credential(RemoteIdentity) (string, string, error) {
	if c.err != nil {
		return "", "", c.err
	}
	return gitHubCredentialUser, c.secret, nil
}

// doctorForge embeds a nil GitHubAdapter on purpose: every method except the
// read-only DiscoverIssues panics, so a write anywhere on the doctor path is
// impossible to miss.
type doctorForge struct {
	GitHubAdapter
	result DiscoveryResult
	err    error
}

func (f doctorForge) DiscoverIssues(context.Context, DiscoveryQuery) (DiscoveryResult, error) {
	return f.result, f.err
}

// ---------------------------------------------------------------------------
// Fixture
// ---------------------------------------------------------------------------

// doctorGovernanceFixture returns a ProjectModel and a policy that either does
// or does not grant the publication permission when the boundary fact is
// `unknown` - which is what predicted scope always resolves to.
func doctorGovernanceFixture(grantAtUnknown bool) (domain.ProjectModel, domain.EngineeringPolicy) {
	boundaries := map[string]domain.CriticalBoundary{
		"service": {Type: "service", Paths: []string{"internal/service/**"}},
	}
	model := domain.ProjectModel{
		SchemaVersion:      domain.SchemaVersion,
		ID:                 "model-doctor",
		Revision:           "1",
		Subject:            domain.Subject{Repository: "acme/widgets", Revision: "base"},
		CriticalBoundaries: &boundaries,
	}
	permissions := []domain.Action{{Type: PublicationActionType, Target: "main"}}
	effect := domain.PolicyEffect{Permissions: &permissions}
	rules := map[string]domain.PolicyRule{
		"publish-when-clear": {
			When:   domain.PolicyCondition{Fact: "service.boundary_modified", Equals: domain.FactFalse},
			Effect: effect,
		},
	}
	if grantAtUnknown {
		rules["publish-when-unknown"] = domain.PolicyRule{
			When:   domain.PolicyCondition{Fact: "service.boundary_modified", Equals: domain.FactUnknown},
			Effect: effect,
		}
	}
	return model, domain.EngineeringPolicy{
		SchemaVersion: domain.SchemaVersion, ID: "policy-doctor", Revision: "1", Rules: rules,
	}
}

type doctorFixture struct {
	t          *testing.T
	root       string
	stateDir   string
	cacheDir   string
	repoRoot   string
	configPath string
	credential string
	input      DoctorInput
}

// newDoctorFixture builds a fully healthy environment. Each test then breaks
// exactly one thing, so a failure names one cause.
func newDoctorFixture(t *testing.T) *doctorFixture {
	t.Helper()
	root := t.TempDir()
	f := &doctorFixture{
		t:          t,
		root:       root,
		stateDir:   filepath.Join(root, "state"),
		cacheDir:   filepath.Join(root, "cache"),
		repoRoot:   filepath.Join(root, "repo"),
		configPath: filepath.Join(root, "config.json"),
		credential: filepath.Join(root, "provider-credential"),
	}
	for _, dir := range []string{f.stateDir, f.cacheDir, f.repoRoot} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(f.credential, []byte("not-read-by-the-doctor\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A healthy environment has a PROVISIONED cache. An empty directory is the
	// fifth dogfood's exact condition and is deliberately not healthy.
	if err := os.MkdirAll(filepath.Join(f.cacheDir, "download", "sumdb"), 0o700); err != nil {
		t.Fatal(err)
	}
	f.writeOperatorConfig(nil)
	f.writeRepositoryConfig(`{"budgets":{"wall_limit_seconds":300}}`)

	model, policy := doctorGovernanceFixture(true)
	executor := doctorExecutor{available: true}
	f.input = DoctorInput{
		StateDir:   f.stateDir,
		Repository: RepositoryTarget{Identity: "acme/widgets", Remote: "https://github.com/acme/widgets", DefaultBranch: "main"},
		// The git seams are injected so the report does not depend on which
		// git the test container happens to ship.
		GitBinary:              func() (string, error) { return "/usr/bin/git", nil },
		GitVersion:             func() (string, error) { return "git version 2.43.0", nil },
		Credentials:            doctorCredential{secret: doctorSecret},
		Provider:               doctorProviderFake{isolation: doctorProvenIsolation()},
		ProviderCredentialPath: f.credential,
		Codex:                  NativeCodexProvider{Executor: executor},
		Sandbox:                DockerSandbox{Image: doctorImage, Executor: executor},
		DependencyCacheDir:     f.cacheDir,
		GitHub: doctorForge{result: DiscoveryResult{
			Repo:      GitHubRepo{Owner: "acme", Name: "widgets"},
			Label:     DefaultDiscoveryLabel,
			Pages:     1,
			RateLimit: RateLimitObservation{Remaining: 4931, ResetAt: time.Unix(1800000000, 0).UTC()},
		}},
		GitHubCredentialMode: GitHubCredentialCLI,
		OperatorConfigPath:   f.configPath,
		RepositoryRoot:       f.repoRoot,
		ProjectModel:         model,
		Policy:               policy,
	}
	return f
}

func (f *doctorFixture) writeOperatorConfig(mutate func(map[string]any)) {
	f.t.Helper()
	config := map[string]any{
		"state_dir":          f.stateDir,
		"project_model_path": filepath.Join(f.root, "model.json"),
		"policy_path":        filepath.Join(f.root, "policy.json"),
		"assurance":          map[string]any{"image": doctorImage, "dependency_cache_dir": f.cacheDir},
		"provider":           map[string]any{"kind": ProviderNativeCodex, "model": "gpt-5-codex", "credential_path": f.credential},
		"github":             map[string]any{"credential_mode": GitHubCredentialCLI},
		"budgets": map[string]any{
			"wall_limit_seconds": 900, "max_execution_attempts": 2,
			"max_remediation_attempts": 2, "max_assurance_attempts": 2,
		},
		"watch": map[string]any{
			"repositories": []string{"acme/widgets"}, "label": DefaultDiscoveryLabel,
			"poll_interval_seconds": 60, "max_concurrent_runs": 1,
		},
	}
	if mutate != nil {
		mutate(config)
	}
	data, err := json.Marshal(config)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.configPath, data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *doctorFixture) writeRepositoryConfig(body string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.repoRoot, RepositoryConfigFile), []byte(body), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *doctorFixture) run() DoctorReport {
	f.t.Helper()
	return Doctor(context.Background(), f.input)
}

// requireCheck asserts one check's status and that its reason names something
// actionable. A PASS with an empty reason is itself a failure: the report has
// to state what was proven.
func requireCheck(t *testing.T, report DoctorReport, id string, want DoctorStatus, mustContain ...string) DoctorCheck {
	t.Helper()
	check, ok := report.Check(id)
	if !ok {
		t.Fatalf("report has no check %q", id)
	}
	if check.Status != want {
		t.Fatalf("check %s: want %s, got %s (%s)", id, want, check.Status, check.Reason)
	}
	if strings.TrimSpace(check.Reason) == "" {
		t.Fatalf("check %s reported %s with no reason", id, check.Status)
	}
	for _, fragment := range mustContain {
		if !strings.Contains(check.Reason, fragment) {
			t.Fatalf("check %s reason does not mention %q: %s", id, fragment, check.Reason)
		}
	}
	return check
}

// ---------------------------------------------------------------------------
// A healthy environment
// ---------------------------------------------------------------------------

func TestDoctorHealthyEnvironmentPassesEveryCheck(t *testing.T) {
	report := newDoctorFixture(t).run()
	for _, check := range report.Checks {
		if check.Status != DoctorPass {
			t.Errorf("check %s is %s in a healthy environment: %s", check.ID, check.Status, check.Reason)
		}
	}
	if report.Status != DoctorPass {
		t.Fatalf("report status is %s, want PASS", report.Status)
	}
	// The list is asserted so a check cannot quietly disappear and leave the
	// preflight passing on a question it stopped asking.
	want := []string{
		"state.dir", "state.schema", "state.sqlite", "state.lock", "state.liveness",
		"git.binary", "git.features", "git.remote", "git.credential", "git.isolation",
		"provider.isolation", "provider.credential",
		"assurance.docker_endpoint", "assurance.image", "assurance.verifier_sandbox",
		"assurance.boundaries", "assurance.toolchain", "assurance.dependency_cache",
		"assurance.dependency_preparation",
		"github.credential", "github.identity", "github.rate_limit",
		"config.global", "config.repository", "config.tighten", "config.watch",
		"governance.publication_scope",
	}
	if len(report.Checks) != len(want) {
		t.Fatalf("report has %d checks, want %d", len(report.Checks), len(want))
	}
	for _, id := range want {
		if _, ok := report.Check(id); !ok {
			t.Errorf("report is missing check %q", id)
		}
	}
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

func TestDoctorFailsWhenProtectedProviderIsolationIsUnproven(t *testing.T) {
	f := newDoctorFixture(t)
	unproven := doctorProvenIsolation()
	unproven.FilesystemRead = IsolationUnproven
	unproven.Rationale = "the adapter cannot confine reads of runtime state"
	f.input.Provider = doctorProviderFake{isolation: unproven}

	report := f.run()
	requireCheck(t, report, "provider.isolation", DoctorFail, "filesystem read confinement", "unproven")
	if report.Status != DoctorFail {
		t.Fatalf("report status is %s, want FAIL", report.Status)
	}
}

func TestDoctorFailsWhenNoProviderIsConfigured(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.Provider = nil
	requireCheck(t, f.run(), "provider.isolation", DoctorFail, "no execution provider is configured")
}

// ---------------------------------------------------------------------------
// Assurance
// ---------------------------------------------------------------------------

func TestDoctorFailsWhenVerifierSandboxIsUnavailable(t *testing.T) {
	f := newDoctorFixture(t)
	unavailable := doctorExecutor{available: false}
	f.input.Sandbox.Executor = unavailable
	f.input.Codex.Executor = unavailable

	report := f.run()
	requireCheck(t, report, "assurance.verifier_sandbox", DoctorFail, "unavailable")
	requireCheck(t, report, "assurance.boundaries", DoctorFail, "unavailable")
	requireCheck(t, report, "assurance.dependency_preparation", DoctorFail, "unavailable")
	if report.Status != DoctorFail {
		t.Fatalf("report status is %s, want FAIL", report.Status)
	}
}

func TestDoctorFailsOnUnpinnedAssuranceImageAndBadDockerEndpoint(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.Sandbox.Image = "golang:1.25"
	f.input.Sandbox.Endpoint = DockerEndpoint{Host: "ssh://build@example.com"}

	report := f.run()
	requireCheck(t, report, "assurance.image", DoctorFail, "sha256:")
	requireCheck(t, report, "assurance.docker_endpoint", DoctorFail, "endpoint")
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

func TestDoctorFailsWithTypedGitHubAuthRequired(t *testing.T) {
	for name, mutate := range map[string]func(*doctorFixture){
		"no credential provider": func(f *doctorFixture) { f.input.Credentials = nil },
		"mode none":              func(f *doctorFixture) { f.input.GitHubCredentialMode = GitHubCredentialNone },
		"resolution fails": func(f *doctorFixture) {
			f.input.Credentials = doctorCredential{err: &GitHubAuthError{Detail: "gh is not logged in"}}
		},
		"empty secret": func(f *doctorFixture) { f.input.Credentials = doctorCredential{secret: "   "} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newDoctorFixture(t)
			mutate(f)
			report := f.run()
			check := requireCheck(t, report, "github.credential", DoctorFail)
			if !strings.HasPrefix(check.Reason, WatchWaitingGitHubAuth+":") {
				t.Fatalf("github.credential is not the typed %s outcome: %s", WatchWaitingGitHubAuth, check.Reason)
			}
			// A probe that is guaranteed to fail must not be attempted, and it
			// must not be reported as health either.
			requireCheck(t, report, "github.identity", DoctorWarn, "credential is unavailable")
			requireCheck(t, report, "github.rate_limit", DoctorWarn)
			if report.Status != DoctorFail {
				t.Fatalf("report status is %s, want FAIL", report.Status)
			}
		})
	}
}

func TestDoctorWarnsWhenNoForgeAdapterIsConfigured(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.GitHub = nil
	report := f.run()
	requireCheck(t, report, "github.identity", DoctorWarn, "no forge adapter is configured")
	requireCheck(t, report, "github.rate_limit", DoctorWarn)
	if report.Status != DoctorWarn {
		t.Fatalf("report status is %s, want WARN", report.Status)
	}
}

func TestDoctorReportsExhaustedAndUnreportedRateLimit(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.GitHub = doctorForge{result: DiscoveryResult{Label: DefaultDiscoveryLabel, Pages: 1}}
	requireCheck(t, f.run(), "github.rate_limit", DoctorWarn, "no rate-limit budget")

	f.input.GitHub = doctorForge{result: DiscoveryResult{
		Label: DefaultDiscoveryLabel, Pages: 1,
		RateLimit: RateLimitObservation{Remaining: 0, ResetAt: time.Unix(1800000000, 0).UTC()},
	}}
	requireCheck(t, f.run(), "github.rate_limit", DoctorFail, "exhausted")
}

// ---------------------------------------------------------------------------
// Runtime state
// ---------------------------------------------------------------------------

func TestDoctorFailsOnUnsupportedNewerSQLiteSchema(t *testing.T) {
	f := newDoctorFixture(t)
	path := filepath.Join(f.stateDir, "runtime.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	report := f.run()
	requireCheck(t, report, "state.schema", DoctorFail, "newer than supported", "upgrade this binary")
	requireCheck(t, report, "state.sqlite", DoctorFail)
	if report.Status != DoctorFail {
		t.Fatalf("report status is %s, want FAIL", report.Status)
	}
}

func TestDoctorFailsOnWorldReadableStateDir(t *testing.T) {
	f := newDoctorFixture(t)
	if err := os.Chmod(f.stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	requireCheck(t, f.run(), "state.dir", DoctorFail, "chmod 700")
}

func TestDoctorFailsWhenStateDirIsMissing(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.StateDir = ""
	report := f.run()
	for _, id := range []string{"state.dir", "state.schema", "state.sqlite", "state.lock", "state.liveness"} {
		requireCheck(t, report, id, DoctorFail)
	}
}

func TestDoctorProvesOwnerLivenessEvidence(t *testing.T) {
	requireCheck(t, newDoctorFixture(t).run(), "state.liveness", DoctorPass, "advisory ownership lock")
}

// ---------------------------------------------------------------------------
// Repository Git
// ---------------------------------------------------------------------------

func TestDoctorFailsOnGitTooOldForConfigIsolation(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.GitVersion = func() (string, error) { return "git version 2.30.2", nil }
	requireCheck(t, f.run(), "git.features", DoctorFail, "GIT_CONFIG_GLOBAL")
}

func TestDoctorWarnsOnUnparseableGitVersion(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.GitVersion = func() (string, error) { return "git version next", nil }
	requireCheck(t, f.run(), "git.features", DoctorWarn, "unparseable")
}

func TestDoctorFailsWhenTrustedGitIsMissing(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.GitBinary = func() (string, error) {
		return "", errors.New("trusted git binary not found on the runtime search path")
	}
	// The version seam is cleared too, so the version question genuinely
	// depends on resolving the binary rather than on an injected answer.
	f.input.GitVersion = nil
	report := f.run()
	requireCheck(t, report, "git.binary", DoctorFail, "install git")
	requireCheck(t, report, "git.features", DoctorFail)
}

func TestDoctorRefusesAnUngovernedRemote(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.Repository.Remote = "git@github.com:acme/widgets.git"
	report := f.run()
	requireCheck(t, report, "git.remote", DoctorFail, "refused remote")
	requireCheck(t, report, "git.credential", DoctorFail)
}

func TestDoctorWarnsWhenNoRepositoryIsSpecified(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.Repository = RepositoryTarget{}
	report := f.run()
	requireCheck(t, report, "git.remote", DoctorWarn, "no repository was specified")
	requireCheck(t, report, "git.credential", DoctorWarn)
	requireCheck(t, report, "governance.publication_scope", DoctorWarn, "no default branch")
}

// TestDoctorAssertsGitEnvironmentIsolation is the standing assertion that the
// environment repository Git actually runs with still carries no ambient
// influence. It fails the day a variable is added back.
func TestDoctorAssertsGitEnvironmentIsolation(t *testing.T) {
	requireCheck(t, newDoctorFixture(t).run(), "git.isolation", DoctorPass, "no askpass")
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

func TestDoctorFailsWhenRepositoryConfigWidensABound(t *testing.T) {
	f := newDoctorFixture(t)
	f.writeRepositoryConfig(`{"budgets":{"wall_limit_seconds":99999}}`)

	report := f.run()
	requireCheck(t, report, "config.repository", DoctorPass)
	requireCheck(t, report, "config.tighten", DoctorFail, "only LOWER a bound")
	requireCheck(t, report, "config.watch", DoctorFail, "tightening relation was refused")
	if report.Status != DoctorFail {
		t.Fatalf("report status is %s, want FAIL", report.Status)
	}
}

func TestDoctorRefusesARepositoryConfigOutOfScope(t *testing.T) {
	f := newDoctorFixture(t)
	// A repository naming an operator-authority member is an authority
	// violation, not an incidental unknown field.
	f.writeRepositoryConfig(`{"watch":{"repositories":["attacker/repo"]}}`)
	requireCheck(t, f.run(), "config.repository", DoctorFail, "watch.repositories")
}

func TestDoctorFailsOnInvalidGlobalConfig(t *testing.T) {
	f := newDoctorFixture(t)
	f.writeOperatorConfig(func(config map[string]any) {
		config["assurance"] = map[string]any{"image": "golang:1.25"}
	})
	report := f.run()
	requireCheck(t, report, "config.global", DoctorFail, "sha256:")
	// A layer that did not load leaves the layered questions unanswered, and
	// unanswered is never PASS.
	for _, id := range []string{"config.repository", "config.tighten", "config.watch"} {
		requireCheck(t, report, id, DoctorFail, "did not load")
	}
}

func TestDoctorFailsOnUnenrolledWatchAndBadRegistration(t *testing.T) {
	f := newDoctorFixture(t)
	f.writeOperatorConfig(func(config map[string]any) {
		config["watch"] = map[string]any{"repositories": []string{}, "poll_interval_seconds": 60}
	})
	requireCheck(t, f.run(), "config.watch", DoctorWarn, "no repository is enrolled")

	f.writeOperatorConfig(func(config map[string]any) {
		config["watch"] = map[string]any{"repositories": []string{"git@host:acme/widgets"}, "poll_interval_seconds": 60}
	})
	// An unusable registration is refused by the operator layer itself.
	requireCheck(t, f.run(), "config.global", DoctorFail, "watch.repositories")
}

// ---------------------------------------------------------------------------
// Governance
// ---------------------------------------------------------------------------

func TestDoctorWarnsWhenPolicyCannotAuthorizePublicationFromUnknownScope(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.ProjectModel, f.input.Policy = doctorGovernanceFixture(false)

	report := f.run()
	check := requireCheck(t, report, "governance.publication_scope", DoctorWarn,
		PublicationActionType, "privilege EXPANSION", "Nothing was granted")
	if !strings.Contains(check.Reason, "unknown") {
		t.Fatalf("the warning does not explain the unknown predicted scope: %s", check.Reason)
	}
	// Policy-authoring feedback is a WARN, never a FAIL, and it must never
	// grant anything: the report is the only output.
	if report.Status != DoctorWarn {
		t.Fatalf("report status is %s, want WARN", report.Status)
	}
}

func TestDoctorDoesNotWarnWhenPolicyGrantsPublicationFromUnknownScope(t *testing.T) {
	// The healthy fixture's policy grants at `unknown`; this is the negative
	// half of the pair, so the warning cannot be a constant.
	requireCheck(t, newDoctorFixture(t).run(), "governance.publication_scope", DoctorPass,
		PublicationActionType, "does not depend on a privilege expansion")
}

func TestDoctorWarnsWhenThePredictedContractCannotCompile(t *testing.T) {
	f := newDoctorFixture(t)
	f.input.ProjectModel = domain.ProjectModel{}
	requireCheck(t, f.run(), "governance.publication_scope", DoctorWarn, "could not be compiled")
}

// ---------------------------------------------------------------------------
// No secrets
// ---------------------------------------------------------------------------

// TestDoctorReportNeverCarriesACredential seeds a distinctive secret into every
// credential seam the doctor touches and asserts it appears nowhere in the
// report - not in a reason, not in the serialized form the CLI prints.
func TestDoctorReportNeverCarriesACredential(t *testing.T) {
	for name, mutate := range map[string]func(*doctorFixture){
		"healthy": func(*doctorFixture) {},
		"world-readable state dir": func(f *doctorFixture) {
			if err := os.Chmod(f.stateDir, 0o755); err != nil {
				f.t.Fatal(err)
			}
		},
		"unproven provider": func(f *doctorFixture) {
			f.input.Provider = doctorProviderFake{isolation: ProviderIsolation{Rationale: "nothing is proven"}}
		},
		"sandbox unavailable": func(f *doctorFixture) {
			f.input.Sandbox.Executor = doctorExecutor{}
			f.input.Codex.Executor = doctorExecutor{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDoctorFixture(t)
			mutate(f)
			report := f.run()

			serialized, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(serialized), doctorSecret) {
				t.Fatalf("the serialized report carries the credential: %s", serialized)
			}
			for _, check := range report.Checks {
				if strings.Contains(check.ID+check.Group+check.Reason, doctorSecret) {
					t.Fatalf("check %s carries the credential: %s", check.ID, check.Reason)
				}
			}
			// The credential really was resolved - otherwise this test would
			// pass by never having had a secret to leak.
			requireCheck(t, report, "git.credential", DoctorPass, "secret value was not read")
			requireCheck(t, report, "github.credential", DoctorPass, "secret value was not read")
		})
	}
}
