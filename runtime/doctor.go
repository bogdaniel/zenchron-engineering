package runtime

// Doctor is the preflight for a REAL run: it answers, per capability, whether
// the thing a run will depend on is actually there, and says what to do when it
// is not.
//
// Three rules shape everything below.
//
//   - A check that cannot be answered is WARN or FAIL with a reason. There is
//     no silent PASS: "we could not tell" is never reported as health.
//   - No side effects that cost money or change the world. No provider
//     inference call is made, ever, and no GitHub write is made, ever. The
//     single forge call is a read-only discovery GET, and only when a forge
//     adapter and a repository were both explicitly configured.
//   - No secret reaches the report. Credentials are PROVEN RESOLVABLE and the
//     resolved value stays in a local variable that is never stored, formatted,
//     or returned. doctor_test.go asserts this with a seeded fake secret.
//
// Every dependency is a field of DoctorInput, so the whole report is
// reproducible offline with fakes.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// DoctorStatus is the per-check verdict.
type DoctorStatus string

const (
	DoctorPass DoctorStatus = "PASS"
	DoctorWarn DoctorStatus = "WARN"
	DoctorFail DoctorStatus = "FAIL"
)

// DoctorCheck is one answered question. Reason is always populated, including
// on PASS, so the report states what was actually proven rather than implying
// it from a status word.
type DoctorCheck struct {
	ID     string       `json:"id"`
	Group  string       `json:"group"`
	Status DoctorStatus `json:"status"`
	Reason string       `json:"reason"`
}

// DoctorReport is the whole preflight. Status is the worst check status, so a
// caller can gate on one value without re-deriving it.
type DoctorReport struct {
	Status DoctorStatus  `json:"status"`
	Checks []DoctorCheck `json:"checks"`
}

// Check returns one check by id. The CLI uses it to explain a specific failure.
func (r DoctorReport) Check(id string) (DoctorCheck, bool) {
	for _, check := range r.Checks {
		if check.ID == id {
			return check, true
		}
	}
	return DoctorCheck{}, false
}

// DoctorInput is the complete dependency set. Every field is supplied by the
// composition root; nothing here is discovered from ambient state.
type DoctorInput struct {
	// StateDir is the runtime state directory the run will use.
	StateDir string
	// Repository is the governed repository, when one is configured. An empty
	// Identity means no repository was specified and the repository-scoped
	// checks report WARN rather than passing on nothing.
	Repository RepositoryTarget

	// GitBinary and GitVersion are the trusted Git resolution seams. Nil uses
	// the real gitBinary() and `git version`.
	GitBinary  func() (string, error)
	GitVersion func() (string, error)

	// Credentials resolves the forge credential for both `git push` and the
	// REST calls. Nil means no credential is authorized.
	Credentials CredentialProvider

	// Provider is the configured execution provider. Its Isolation report, not
	// its configuration string, decides protected eligibility.
	Provider ExecutionProvider
	// ProviderCredentialPath is a PATH, never a secret. It is stat'd and never
	// read.
	ProviderCredentialPath string

	// Codex and Sandbox are handed to the frozen DiagnoseSandbox. Both carry an
	// injectable CommandExecutor, which is how the assurance checks run offline.
	Codex   NativeCodexProvider
	Sandbox DockerSandbox
	// DependencyCacheDir is the operator's module cache, when configured.
	DependencyCacheDir string

	// GitHub is the forge adapter. Nil means no read-only forge check is safe
	// to make, which is reported as WARN, not PASS.
	GitHub GitHubAdapter
	// DiscoveryLabel is the opt-in label. Empty means DefaultDiscoveryLabel.
	DiscoveryLabel string
	// GitHubCredentialMode is the operator's declared mode.
	GitHubCredentialMode string

	// OperatorConfigPath and RepositoryRoot are re-read from disk: whether the
	// two layers still load, still tighten, and still validate IS the check.
	OperatorConfigPath string
	RepositoryRoot     string

	// ProjectModel and Policy drive the governance diagnosis.
	ProjectModel domain.ProjectModel
	Policy       domain.EngineeringPolicy
}

// Doctor answers every check independently and returns the report. It never
// returns an error: a question that failed is a check result, not a control
// flow fault.
func Doctor(ctx context.Context, in DoctorInput) DoctorReport {
	checks := make([]DoctorCheck, 0, 25)
	checks = append(checks, doctorState(in)...)
	checks = append(checks, doctorGit(in)...)
	checks = append(checks, doctorProvider(in)...)
	checks = append(checks, doctorAssurance(in)...)
	checks = append(checks, doctorGitHub(ctx, in)...)
	checks = append(checks, doctorConfig(in)...)
	checks = append(checks, doctorGovernance(in))

	report := DoctorReport{Status: DoctorPass, Checks: checks}
	for _, check := range checks {
		if check.Status == DoctorFail {
			report.Status = DoctorFail
			break
		}
		if check.Status == DoctorWarn {
			report.Status = DoctorWarn
		}
	}
	return report
}

func pass(group, id, reason string) DoctorCheck {
	return DoctorCheck{ID: id, Group: group, Status: DoctorPass, Reason: reason}
}
func warn(group, id, reason string) DoctorCheck {
	return DoctorCheck{ID: id, Group: group, Status: DoctorWarn, Reason: reason}
}
func fail(group, id, reason string) DoctorCheck {
	return DoctorCheck{ID: id, Group: group, Status: DoctorFail, Reason: reason}
}

// ---------------------------------------------------------------------------
// Runtime state
// ---------------------------------------------------------------------------

const doctorGroupState = "state"

func doctorState(in DoctorInput) []DoctorCheck {
	return []DoctorCheck{
		doctorStateDir(in),
		doctorStateSchema(in),
		doctorStateSQLite(in),
		doctorStateLock(in),
		doctorStateLiveness(in),
	}
}

// doctorStateDir deliberately does not create the directory. A preflight that
// repairs what it measures cannot report on it.
func doctorStateDir(in DoctorInput) DoctorCheck {
	const id = "state.dir"
	if strings.TrimSpace(in.StateDir) == "" {
		return fail(doctorGroupState, id, "no state directory is configured; set state_dir in the operator configuration")
	}
	if !filepath.IsAbs(in.StateDir) {
		return fail(doctorGroupState, id, "state directory "+in.StateDir+" is not absolute; state_dir must be an absolute path")
	}
	info, err := os.Stat(in.StateDir)
	if err != nil {
		return fail(doctorGroupState, id, "state directory "+in.StateDir+" cannot be inspected: "+err.Error()+"; create it with mode 0700")
	}
	if !info.IsDir() {
		return fail(doctorGroupState, id, in.StateDir+" is not a directory; state_dir must name a directory")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fail(doctorGroupState, id, fmt.Sprintf("state directory %s is mode %#o and is readable by other users; run chmod 700 %s", in.StateDir, perm, in.StateDir))
	}
	return pass(doctorGroupState, id, "state directory "+in.StateDir+" exists and is owner-only")
}

// doctorStateSchema reads PRAGMA user_version directly rather than inferring it
// from an open failure, so "the schema is too new" and "the file will not open"
// are two independently answered questions.
func doctorStateSchema(in DoctorInput) DoctorCheck {
	const id = "state.schema"
	if strings.TrimSpace(in.StateDir) == "" {
		return fail(doctorGroupState, id, "no state directory is configured, so the schema version cannot be read")
	}
	path := filepath.Join(in.StateDir, "runtime.db")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return pass(doctorGroupState, id, fmt.Sprintf("no database exists yet; the first run creates it at schema version %d", sqliteSchemaVersion))
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return warn(doctorGroupState, id, "the schema version could not be read from "+path+": "+err.Error())
	}
	defer func() { _ = db.Close() }()
	var found int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&found); err != nil {
		return warn(doctorGroupState, id, "the schema version could not be read from "+path+": "+err.Error())
	}
	switch {
	case found > sqliteSchemaVersion:
		return fail(doctorGroupState, id, UnsupportedSchemaError{Found: found, Supported: sqliteSchemaVersion}.Error()+"; this database was written by a newer binary, so upgrade this binary rather than downgrading the database")
	case found < sqliteSchemaVersion:
		return pass(doctorGroupState, id, fmt.Sprintf("schema version %d will be migrated to %d on open", found, sqliteSchemaVersion))
	default:
		return pass(doctorGroupState, id, fmt.Sprintf("schema version %d is current", found))
	}
}

func doctorStateSQLite(in DoctorInput) DoctorCheck {
	const id = "state.sqlite"
	if strings.TrimSpace(in.StateDir) == "" {
		return fail(doctorGroupState, id, "no state directory is configured, so the runtime database cannot be opened")
	}
	store, err := OpenSQLiteOperationStore(in.StateDir)
	if err != nil {
		return fail(doctorGroupState, id, "the runtime database at "+filepath.Join(in.StateDir, "runtime.db")+" could not be opened: "+err.Error())
	}
	if err := store.Close(); err != nil {
		return fail(doctorGroupState, id, "the runtime database could not be closed cleanly: "+err.Error())
	}
	return pass(doctorGroupState, id, "the runtime database at "+filepath.Join(in.StateDir, "runtime.db")+" opens and migrates")
}

func doctorStateLock(in DoctorInput) DoctorCheck {
	const id = "state.lock"
	if strings.TrimSpace(in.StateDir) == "" {
		return fail(doctorGroupState, id, "no state directory is configured, so the ownership lock cannot be taken")
	}
	lock, err := AcquireOwnershipLock(in.StateDir, NewRuntimeOwner())
	if err != nil {
		return fail(doctorGroupState, id, "the runtime ownership lock could not be taken: "+err.Error()+"; another process may already own this state directory")
	}
	if err := lock.Release(); err != nil {
		return fail(doctorGroupState, id, "the runtime ownership lock could not be released: "+err.Error())
	}
	return pass(doctorGroupState, id, "the runtime ownership lock under "+filepath.Join(in.StateDir, "locks", "runtime")+" can be taken and released")
}

// doctorStateLiveness proves the OWNER LIVENESS EVIDENCE exists, not merely
// that NewLockOwnerLiveness returns a value. On a platform with no advisory
// locks the probe cannot decide, which is a FAIL: without crash-safe evidence
// a dead owner can never be taken over.
func doctorStateLiveness(in DoctorInput) DoctorCheck {
	const id = "state.liveness"
	if strings.TrimSpace(in.StateDir) == "" {
		return fail(doctorGroupState, id, "no state directory is configured, so owner liveness has no evidence to read")
	}
	owner := NewRuntimeOwner()
	lock, err := AcquireOwnershipLock(in.StateDir, owner)
	if err != nil {
		return fail(doctorGroupState, id, "owner liveness could not be probed because the ownership lock could not be taken: "+err.Error())
	}
	// The probes run while the lock is still HELD: that is what makes "held"
	// and "alive" mean anything. Releasing first would prove only that a
	// released lock reads as free.
	held, decided := ownerLockHeld(in.StateDir, owner)
	alive := NewLockOwnerLiveness(in.StateDir).Alive(owner)
	releaseErr := lock.Release()
	switch {
	case !decided:
		return fail(doctorGroupState, id, "this platform cannot decide whether an ownership lock is held, so a crashed owner could never be proven dead and takeover would be blocked forever; run the runtime on a platform with OS advisory locks")
	case !held:
		return fail(doctorGroupState, id, "the ownership lock this process holds did not read back as held, so owner liveness evidence is not trustworthy on "+in.StateDir)
	case releaseErr != nil:
		return fail(doctorGroupState, id, "the ownership lock could not be released after probing liveness: "+releaseErr.Error())
	}
	if !alive {
		return fail(doctorGroupState, id, "owner liveness reported this live process as dead while it held the lock, so the lock evidence under "+in.StateDir+" is not trustworthy")
	}
	return pass(doctorGroupState, id, "owner liveness reads the OS advisory ownership lock, so a crashed owner is provably dead")
}

// ---------------------------------------------------------------------------
// Repository Git
// ---------------------------------------------------------------------------

const doctorGroupGit = "git"

// doctorMinGitMajor/Minor is 2.32, the release that made GIT_CONFIG_GLOBAL and
// GIT_CONFIG_SYSTEM take effect. Below it those two variables are silently
// ignored, and the host user's global and system Git configuration - including
// credential helpers and hook paths - leaks into every runtime Git call. That
// is the one version bound the isolation in repository_git.go actually depends
// on, so it is the one that is checked.
const (
	doctorMinGitMajor = 2
	doctorMinGitMinor = 32
)

func doctorGit(in DoctorInput) []DoctorCheck {
	return []DoctorCheck{
		doctorGitBinary(in),
		doctorGitFeatures(in),
		doctorGitRemote(in),
		doctorGitCredential(in),
		doctorGitIsolation(),
	}
}

func (in DoctorInput) gitBinary() (string, error) {
	if in.GitBinary != nil {
		return in.GitBinary()
	}
	return gitBinary()
}

func doctorGitBinary(in DoctorInput) DoctorCheck {
	const id = "git.binary"
	binary, err := in.gitBinary()
	if err != nil {
		return fail(doctorGroupGit, id, err.Error()+"; install git into one of "+trustedPATH)
	}
	return pass(doctorGroupGit, id, "the trusted git binary resolves to "+binary+" from the runtime search path, not from the host PATH")
}

func doctorGitFeatures(in DoctorInput) DoctorCheck {
	const id = "git.features"
	version, err := in.gitVersion()
	if err != nil {
		return fail(doctorGroupGit, id, "the trusted git binary could not report its version: "+err.Error())
	}
	major, minor, ok := parseGitVersion(version)
	if !ok {
		return warn(doctorGroupGit, id, "the trusted git binary reported an unparseable version "+strconv.Quote(version)+", so GIT_CONFIG_GLOBAL/GIT_CONFIG_SYSTEM support cannot be confirmed")
	}
	if major < doctorMinGitMajor || (major == doctorMinGitMajor && minor < doctorMinGitMinor) {
		return fail(doctorGroupGit, id, fmt.Sprintf("git %d.%d is older than %d.%d, where GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM take effect; on this version the host user's global and system Git configuration would silently apply to runtime Git calls. Upgrade git.", major, minor, doctorMinGitMajor, doctorMinGitMinor))
	}
	return pass(doctorGroupGit, id, fmt.Sprintf("git %d.%d honours GIT_CONFIG_GLOBAL and GIT_CONFIG_SYSTEM, so host and user Git configuration is excluded", major, minor))
}

func (in DoctorInput) gitVersion() (string, error) {
	if in.GitVersion != nil {
		return in.GitVersion()
	}
	binary, err := in.gitBinary()
	if err != nil {
		return "", err
	}
	// The same environment discipline as GitRunner: os.Environ() is never
	// consulted, so the answer describes the binary the runtime would run and
	// nothing ambient can steer it.
	cmd := exec.Command(binary, "version")
	cmd.Env = []string{"PATH=" + trustedPATH, "LC_ALL=C", "LANG=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat"}
	cmd.Stdin = nil
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// parseGitVersion reads "git version 2.43.0" and tolerates suffixes such as
// Apple's "(Apple Git-154)".
func parseGitVersion(version string) (major, minor int, ok bool) {
	fields := strings.Fields(version)
	for _, field := range fields {
		parts := strings.SplitN(field, ".", 3)
		if len(parts) < 2 {
			continue
		}
		major, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil {
			continue
		}
		return major, minor, true
	}
	return 0, 0, false
}

func doctorGitRemote(in DoctorInput) DoctorCheck {
	const id = "git.remote"
	if strings.TrimSpace(in.Repository.Remote) == "" {
		return warn(doctorGroupGit, id, "no repository was specified, so the governed remote was not resolved; pass a repository to check it")
	}
	identity, err := GovernedRemote(in.Repository.Remote)
	if err != nil {
		return fail(doctorGroupGit, id, err.Error())
	}
	return pass(doctorGroupGit, id, "the governed remote "+identity.URL+" resolves over the "+identity.Transport()+" transport")
}

// doctorGitCredential proves a credential is RESOLVABLE. The resolved secret
// lives in a local variable and is never placed in the report.
func doctorGitCredential(in DoctorInput) DoctorCheck {
	const id = "git.credential"
	if in.Credentials == nil {
		return warn(doctorGroupGit, id, "no credential provider is authorized, so a network remote cannot be pushed; set github.credential_mode in the operator configuration to authorize one")
	}
	if strings.TrimSpace(in.Repository.Remote) == "" {
		return warn(doctorGroupGit, id, "no repository was specified, so no governed remote exists to resolve a credential for")
	}
	identity, err := GovernedRemote(in.Repository.Remote)
	if err != nil {
		return fail(doctorGroupGit, id, "the governed remote could not be resolved, so no credential can be issued for it: "+err.Error())
	}
	user, secret, err := in.Credentials.Credential(identity)
	if err != nil {
		return fail(doctorGroupGit, id, "a credential for "+identity.URL+" could not be resolved: "+err.Error())
	}
	if strings.TrimSpace(secret) == "" {
		return fail(doctorGroupGit, id, "the credential provider returned an empty secret for "+identity.URL+", which would authenticate as nobody")
	}
	// user is not a secret; the secret half is deliberately never formatted.
	return pass(doctorGroupGit, id, "a credential for "+identity.URL+" resolves as user "+strconv.Quote(user)+"; the secret value was not read into this report")
}

// doctorGitIsolation asserts the repositoryGitEnv guarantees on the environment
// the runtime would actually build, rather than restating them as prose.
func doctorGitIsolation() DoctorCheck {
	const id = "git.isolation"
	const home, template = "/doctor-probe-home", "/doctor-probe-template"
	env := repositoryGitEnv(home, template, "")
	for _, required := range []string{
		"HOME=" + home,
		"GIT_TEMPLATE_DIR=" + template,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_ATTR_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_LITERAL_PATHSPECS=1",
		"PATH=" + trustedPATH,
	} {
		if !containsExact(env, required) {
			return fail(doctorGroupGit, id, "the repository Git environment is missing "+required+", so hook and configuration isolation is not enforced")
		}
	}
	for _, forbidden := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_NAMESPACE", "GIT_EXTERNAL_DIFF",
		"GIT_SSH", "GIT_SSH_COMMAND", "GIT_ASKPASS", "SSH_ASKPASS",
		"SSH_AUTH_SOCK", "GIT_PROXY_COMMAND", "GIT_EDITOR", "XDG_CONFIG_HOME",
	} {
		if hasEnvName(env, forbidden) {
			return fail(doctorGroupGit, id, "the repository Git environment carries "+forbidden+", which is an ambient influence on runtime Git calls")
		}
	}
	return pass(doctorGroupGit, id, "runtime Git calls run with an empty runtime-owned HOME and template directory, no system/global/XDG configuration, no askpass, no SSH agent, and no external diff")
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasEnvName(env []string, name string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, name+"=") {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Provider
// ---------------------------------------------------------------------------

const doctorGroupProvider = "provider"

func doctorProvider(in DoctorInput) []DoctorCheck {
	return []DoctorCheck{doctorProviderIsolation(in), doctorProviderCredential(in)}
}

// doctorProviderIsolation makes no inference call. It reads the provider's own
// isolation claim, which is the only thing protected eligibility depends on.
func doctorProviderIsolation(in DoctorInput) DoctorCheck {
	const id = "provider.isolation"
	if in.Provider == nil {
		return fail(doctorGroupProvider, id, "no execution provider is configured, so no protected-eligible provider exists; set provider.kind in the operator configuration")
	}
	if err := RequireProtectedIsolation(in.Provider); err != nil {
		return fail(doctorGroupProvider, id, err.Error())
	}
	reporter, _ := in.Provider.(IsolationReporter)
	isolation := reporter.Isolation()
	return pass(doctorGroupProvider, id, fmt.Sprintf("provider %T is protected-eligible: filesystem read %s, filesystem write %s, network denial %s, credential confinement %s", in.Provider, isolation.FilesystemRead, isolation.FilesystemWrite, isolation.NetworkDenied, isolation.CredentialScope))
}

// doctorProviderCredential stats the credential PATH and never reads it. A path
// is not a usable secret, which is exactly why configuration names one.
func doctorProviderCredential(in DoctorInput) DoctorCheck {
	const id = "provider.credential"
	path := strings.TrimSpace(in.ProviderCredentialPath)
	if path == "" {
		return fail(doctorGroupProvider, id, "no provider credential path is configured; set provider.credential_path to an operator-controlled file")
	}
	if !filepath.IsAbs(path) {
		return fail(doctorGroupProvider, id, "provider credential path "+path+" is not absolute; provider.credential_path must be an absolute path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return fail(doctorGroupProvider, id, "the provider credential referenced at "+path+" cannot be inspected: "+err.Error())
	}
	if !info.Mode().IsRegular() {
		return fail(doctorGroupProvider, id, path+" is not a regular file, so it cannot hold the provider credential")
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		return fail(doctorGroupProvider, id, fmt.Sprintf("the provider credential at %s is mode %#o and is readable by other users; run chmod 600 %s", path, perm, path))
	}
	// The distinction is stated in the PASS itself. A resolvable credential is
	// not a fundable account: whether the provider will actually execute work
	// can only be learned by asking it, and asking costs money and runs
	// inference, which a preflight must never do. A run that meets an exhausted
	// balance therefore waits on provider_account_unavailable at execution
	// time; doctor cannot and does not promise otherwise.
	return pass(doctorGroupProvider, id, "the provider credential referenced at "+path+" exists and is owner-only; its contents were not read. This proves the credential is CONFIGURED, not that the provider account can execute: account state is only observable by making a paid request, which this preflight never does")
}

// ---------------------------------------------------------------------------
// Assurance
// ---------------------------------------------------------------------------

const doctorGroupAssurance = "assurance"

func doctorAssurance(in DoctorInput) []DoctorCheck {
	// The frozen diagnosis is called exactly once; the three Docker-derived
	// fields are separate questions and are reported separately.
	diagnosis := DiagnoseSandbox(in.Codex, in.Sandbox)
	return []DoctorCheck{
		doctorDockerEndpoint(in),
		doctorAssuranceImage(in),
		doctorVerifierSandbox(diagnosis),
		doctorSandboxBoundaries(diagnosis),
		doctorDependencyPreparation(in, diagnosis),
	}
}

func doctorDockerEndpoint(in DoctorInput) DoctorCheck {
	const id = "assurance.docker_endpoint"
	identity, err := in.Sandbox.Endpoint.identity()
	if err != nil {
		return fail(doctorGroupAssurance, id, err.Error()+"; assurance.docker_host must be a unix:// socket path or a tcp:// endpoint with no userinfo, query, or fragment")
	}
	return pass(doctorGroupAssurance, id, "the trusted Docker endpoint is "+identity)
}

func doctorAssuranceImage(in DoctorInput) DoctorCheck {
	const id = "assurance.image"
	image := strings.TrimSpace(in.Sandbox.Image)
	if image == "" {
		return fail(doctorGroupAssurance, id, "no assurance image is configured, so the program that decides whether a candidate passed is unknown; set assurance.image to a sha256: digest")
	}
	if !strings.HasPrefix(image, "sha256:") {
		return fail(doctorGroupAssurance, id, "assurance image "+image+" is a tag, not a pinned digest; set assurance.image to a sha256: digest so the verifying program cannot change underneath a run")
	}
	return pass(doctorGroupAssurance, id, "the assurance image is pinned to "+image)
}

func doctorVerifierSandbox(diagnosis SandboxDoctor) DoctorCheck {
	const id = "assurance.verifier_sandbox"
	if diagnosis.VerifierSandbox != "enforceable" {
		return fail(doctorGroupAssurance, id, "the verifier sandbox is "+diagnosis.VerifierSandbox+": Docker is missing, the daemon is unreachable, or the pinned assurance image is not present locally. Without it no candidate can be verified, so no run can complete.")
	}
	return pass(doctorGroupAssurance, id, "the Docker verifier sandbox is enforceable: the daemon answers and the pinned assurance image is present")
}

// doctorSandboxBoundaries checks the boundary the runtime would actually apply,
// by inspecting the container arguments it builds, and then confirms the daemon
// can enforce them.
func doctorSandboxBoundaries(diagnosis SandboxDoctor) DoctorCheck {
	const id = "assurance.boundaries"
	args := strings.Join(dockerBase("/doctor-probe-candidate", true), " ")
	for _, required := range []string{
		"--network none",
		"--read-only",
		"--cap-drop ALL",
		"--security-opt no-new-privileges",
	} {
		if !strings.Contains(args, required) {
			return fail(doctorGroupAssurance, id, "the assurance container boundary is missing "+required+", so the no-socket, network-off, read-only guarantee is not enforced")
		}
	}
	if strings.Contains(args, "docker.sock") || strings.Contains(args, "/var/run/docker") {
		return fail(doctorGroupAssurance, id, "the assurance container boundary mounts the Docker socket, which would hand the verified candidate control of the daemon")
	}
	if diagnosis.OfflineVerification != "enforceable" {
		return fail(doctorGroupAssurance, id, "the boundary arguments are correct but offline verification is "+diagnosis.OfflineVerification+", so no daemon is available to enforce them")
	}
	return pass(doctorGroupAssurance, id, "assurance containers run with no network, no Docker socket, a read-only root, all capabilities dropped, and no-new-privileges, and the daemon can enforce it")
}

func doctorDependencyPreparation(in DoctorInput, diagnosis SandboxDoctor) DoctorCheck {
	const id = "assurance.dependency_preparation"
	if diagnosis.DependencyPreparation != "enforceable" {
		return fail(doctorGroupAssurance, id, "dependency preparation is "+diagnosis.DependencyPreparation+", so module dependencies cannot be fetched into the cache and offline verification would fail on the first uncached module")
	}
	cache := strings.TrimSpace(in.DependencyCacheDir)
	if cache == "" {
		return warn(doctorGroupAssurance, id, "the sandbox can prepare dependencies but no assurance.dependency_cache_dir is configured, so every run re-downloads modules and offline verification has nothing to read")
	}
	info, err := os.Stat(cache)
	if err != nil {
		return fail(doctorGroupAssurance, id, "the dependency cache directory "+cache+" cannot be inspected: "+err.Error())
	}
	if !info.IsDir() {
		return fail(doctorGroupAssurance, id, cache+" is not a directory, so it cannot be the module cache")
	}
	return pass(doctorGroupAssurance, id, "dependency preparation is enforceable and the module cache at "+cache+" exists")
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

const doctorGroupGitHub = "github"

func doctorGitHub(ctx context.Context, in DoctorInput) []DoctorCheck {
	credential := doctorGitHubCredential(in)
	identity, rate := doctorGitHubRead(ctx, in, credential)
	return []DoctorCheck{credential, identity, rate}
}

// doctorGitHubCredential reports the typed github_auth_required outcome rather
// than an opaque string, because that is the state a caller has to route to an
// operator rather than retry.
func doctorGitHubCredential(in DoctorInput) DoctorCheck {
	const id = "github.credential"
	if in.GitHubCredentialMode == GitHubCredentialNone {
		return fail(doctorGroupGitHub, id, (&GitHubAuthError{Detail: "github.credential_mode is \"none\", which is an explicit refusal to authorize forge writes; a run cannot publish a pull request"}).Error())
	}
	if in.Credentials == nil {
		return fail(doctorGroupGitHub, id, (&GitHubAuthError{Detail: "no credential provider is authorized; set github.credential_mode to \"" + GitHubCredentialCLI + "\" and log in with gh auth login"}).Error())
	}
	if strings.TrimSpace(in.Repository.Remote) == "" {
		return warn(doctorGroupGitHub, id, "a credential provider is authorized but no repository was specified, so no governed remote exists to resolve it for")
	}
	identity, err := GovernedRemote(in.Repository.Remote)
	if err != nil {
		return fail(doctorGroupGitHub, id, (&GitHubAuthError{Detail: "the governed remote could not be resolved: " + err.Error()}).Error())
	}
	_, secret, err := in.Credentials.Credential(identity)
	if err != nil {
		return fail(doctorGroupGitHub, id, err.Error())
	}
	if strings.TrimSpace(secret) == "" {
		return fail(doctorGroupGitHub, id, (&GitHubAuthError{Detail: "the credential provider returned an empty secret for " + identity.URL}).Error())
	}
	return pass(doctorGroupGitHub, id, "a forge credential for "+identity.URL+" resolves in mode "+strconv.Quote(in.GitHubCredentialMode)+"; the secret value was not read into this report")
}

// doctorGitHubRead makes at most ONE call, a conditional read-only discovery
// GET. There is no write anywhere on this path. It is skipped entirely unless a
// forge adapter and a repository were both explicitly configured and the
// credential already proved out, because a probe that is guaranteed to fail
// teaches nothing and still spends rate-limit budget.
func doctorGitHubRead(ctx context.Context, in DoctorInput, credential DoctorCheck) (identity, rate DoctorCheck) {
	const identityID, rateID = "github.identity", "github.rate_limit"
	skip := func(reason string) (DoctorCheck, DoctorCheck) {
		return warn(doctorGroupGitHub, identityID, reason), warn(doctorGroupGitHub, rateID, reason)
	}
	if in.GitHub == nil {
		return skip("no forge adapter is configured, so no read-only forge observation was made")
	}
	if credential.Status == DoctorFail {
		return skip("the forge credential is unavailable, so no read-only forge observation was attempted")
	}
	repo, err := parseGitHubRepo(in.Repository.Identity)
	if err != nil {
		return skip("no usable owner/name repository is configured, so no read-only forge observation was made: " + err.Error())
	}
	label := in.DiscoveryLabel
	if label == "" {
		label = DefaultDiscoveryLabel
	}
	result, err := in.GitHub.DiscoverIssues(ctx, DiscoveryQuery{Repo: repo, Label: label})
	if err != nil {
		reason := "the read-only discovery observation of " + repo.String() + " failed: " + err.Error()
		return fail(doctorGroupGitHub, identityID, reason), warn(doctorGroupGitHub, rateID, "no rate-limit budget was observed because the discovery observation failed")
	}
	identity = pass(doctorGroupGitHub, identityID, fmt.Sprintf("a read-only discovery observation of %s succeeded with opt-in label %q (%d opted-in issues, %d pages, no write was made)", repo.String(), result.Label, len(result.Issues), result.Pages))
	switch {
	case result.RateLimit.Remaining <= 0 && result.RateLimit.ResetAt.IsZero() && result.RateLimit.RetryAfter == 0:
		rate = warn(doctorGroupGitHub, rateID, "the forge reported no rate-limit budget on the observation, so remaining budget is unknown and must be treated as exhausted")
	case result.RateLimit.Remaining <= 0:
		rate = fail(doctorGroupGitHub, rateID, fmt.Sprintf("the forge rate-limit budget is exhausted; it resets at %s", result.RateLimit.ResetAt.UTC().Format("2006-01-02T15:04:05Z")))
	default:
		rate = pass(doctorGroupGitHub, rateID, fmt.Sprintf("the forge reported %d requests remaining", result.RateLimit.Remaining))
	}
	return identity, rate
}

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const doctorGroupConfig = "config"

// doctorConfig re-reads both layers from disk. That the two layers still load
// strictly, still tighten, and still validate IS the check; asserting it
// against an already-loaded Config would only assert that loading succeeded
// once.
func doctorConfig(in DoctorInput) []DoctorCheck {
	const globalID, repositoryID, tightenID, watchID = "config.global", "config.repository", "config.tighten", "config.watch"
	unanswered := func(reason string) []DoctorCheck {
		return []DoctorCheck{
			fail(doctorGroupConfig, repositoryID, reason),
			fail(doctorGroupConfig, tightenID, reason),
			fail(doctorGroupConfig, watchID, reason),
		}
	}
	path, err := OperatorConfigPath(in.OperatorConfigPath)
	if err != nil {
		reason := "the operator configuration path could not be resolved: " + err.Error()
		return append([]DoctorCheck{fail(doctorGroupConfig, globalID, reason)}, unanswered(reason)...)
	}
	operator, _, err := LoadOperatorConfig(path)
	if err != nil {
		reason := err.Error()
		return append([]DoctorCheck{fail(doctorGroupConfig, globalID, reason)}, unanswered("the operator layer did not load, so this could not be answered: "+reason)...)
	}
	checks := []DoctorCheck{pass(doctorGroupConfig, globalID, "the operator configuration at "+path+" decodes strictly, names no unknown member, and validates")}

	var repository RepositoryConfig
	if strings.TrimSpace(in.RepositoryRoot) == "" {
		checks = append(checks, warn(doctorGroupConfig, repositoryID, "no repository root was supplied, so the in-repo "+RepositoryConfigFile+" was not read"))
	} else {
		repositoryPath := filepath.Join(in.RepositoryRoot, RepositoryConfigFile)
		loaded, _, present, err := LoadRepositoryConfig(repositoryPath)
		switch {
		case err != nil:
			checks = append(checks, fail(doctorGroupConfig, repositoryID, err.Error()))
		case !present:
			checks = append(checks, pass(doctorGroupConfig, repositoryID, "no in-repo "+RepositoryConfigFile+" is present, so the operator layer governs unchanged"))
		default:
			repository = loaded
			checks = append(checks, pass(doctorGroupConfig, repositoryID, "the in-repo "+repositoryPath+" decodes strictly and names only members a repository has authority over"))
		}
	}

	effective, err := operator.Tighten(repository)
	if err != nil {
		checks = append(checks,
			fail(doctorGroupConfig, tightenID, err.Error()+"; the in-repo layer may only LOWER a bound, never raise it"),
			fail(doctorGroupConfig, watchID, "the tightening relation was refused, so the effective watch registration could not be resolved"))
		return checks
	}
	checks = append(checks, pass(doctorGroupConfig, tightenID, "the in-repo layer only tightens: every bound it names is at or below the operator ceiling"))

	settings, err := effective.WatchSettings()
	if err != nil {
		return append(checks, fail(doctorGroupConfig, watchID, err.Error()))
	}
	if len(settings.Repositories) == 0 {
		return append(checks, warn(doctorGroupConfig, watchID, "no repository is enrolled in watch.repositories, so the watch loop would observe nothing"))
	}
	return append(checks, pass(doctorGroupConfig, watchID, fmt.Sprintf("%d watch registration(s) resolve to governed remotes, polled every %ds with opt-in label %q", len(settings.Repositories), int(settings.PollInterval.Seconds()), settings.Label)))
}

// ---------------------------------------------------------------------------
// Governance
// ---------------------------------------------------------------------------

const doctorGroupGovernance = "governance"

// doctorGovernance is POLICY-AUTHORING feedback, not an authority decision. It
// grants nothing and never can.
//
// The condition it detects: policy.Compile requires a non-empty allowed-path
// set, and the runtime cannot predict which files an issue will touch, so it
// compiles the predicted contract with a placeholder scope and PathsKnown
// false - which makes every boundary fact honestly `unknown`. A policy that
// only grants the publication permission once the paths ARE known therefore
// resolves NO publication permission at predicted stage. When the candidate is
// later observed and the permission appears, that is a privilege EXPANSION,
// which #8 correctly refuses: reassessment caps the permission set to the
// current contract's and records requested_privilege_expansion, so the run
// waits forever on an expansion nothing will ever grant.
//
// The detection is therefore exact and cheap: compile the same predicted
// contract the runtime would compile and look for the publication permission.
// Absent means the run is already doomed to that wait.
func doctorGovernance(in DoctorInput) DoctorCheck {
	const id = "governance.publication_scope"
	branch := strings.TrimSpace(in.Repository.DefaultBranch)
	if branch == "" {
		return warn(doctorGroupGovernance, id, "no default branch is configured, so the publication action policy would be asked about is unknown and this could not be answered")
	}
	state, err := KernelFlow{}.Compile(SourceSnapshot{
		ID:               "doctor-preflight",
		Objective:        "preflight: the objective of a real run is not known in advance",
		AcceptanceIntent: runtimeAcceptanceIntent,
		PredictedPaths:   []string{predictedScopePlaceholder},
		PathsKnown:       false,
	}, in.ProjectModel, in.Policy, "contract-doctor-preflight", "1")
	if err != nil {
		return warn(doctorGroupGovernance, id, "the predicted work contract could not be compiled from this ProjectModel and policy, so publication authority could not be predicted: "+err.Error())
	}
	action := domain.Action{Type: PublicationActionType, Target: branch}
	for _, permitted := range state.Contract.Permissions {
		if permitted == action {
			return pass(doctorGroupGovernance, id, "policy grants "+PublicationActionType+":"+branch+" from predicted (unknown) scope, so publication does not depend on a privilege expansion")
		}
	}
	return warn(doctorGroupGovernance, id, "policy does not grant "+PublicationActionType+":"+branch+" from predicted scope. The runtime cannot predict which files an issue will touch, so predicted scope is honestly `unknown`; a policy that only grants publication once the paths are known makes that grant a privilege EXPANSION at reassessment, which is correctly refused, and the run would wait on requested_privilege_expansion forever. Add a rule granting this permission for the `unknown` value of the boundary facts it matches on. Nothing was granted by this diagnosis.")
}
