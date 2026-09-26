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
	// Toolchain is the operator's brokered worker execution environment. Its
	// zero value inherits the supervisor's, which is what every configuration
	// did before it existed.
	Toolchain ToolchainConfig
	// ProviderCredentialPath is a PATH, never a secret. It is stat'd and never
	// read.
	ProviderCredentialPath string

	// Codex and Sandbox are handed to the frozen DiagnoseSandbox. Both carry an
	// injectable CommandExecutor, which is how the assurance checks run offline.
	Codex   NativeCodexProvider
	Sandbox DockerSandbox
	// DependencyCacheDir is the operator's module cache, when configured.
	DependencyCacheDir string
	// SemanticAssurance is the independent semantic acceptance producer, when
	// one is configured. Nil is a truthful answer, not a fault: it means
	// semantic_acceptance is not producible and a contract requiring it is
	// refused before any expensive work.
	SemanticAssurance AssuranceProvider

	// GitHub is the forge adapter. Nil means no read-only forge check is safe
	// to make, which is reported as WARN, not PASS.
	GitHub GitHubAdapter
	// Governance is the separately authorized, read-only adoption observer.
	// It is never derived from the publication adapter or credential.
	Governance ForgeGovernance
	// DiscoveryLabel is the opt-in label. Empty means DefaultDiscoveryLabel.
	DiscoveryLabel string
	// GitHubCredentialMode is the operator's declared mode.
	GitHubCredentialMode string
	// OperatorGitHub resolves the HUMAN operator's own GitHub identity through
	// their local `gh` login, which is a DIFFERENT credential from the one the
	// runtime publishes with.
	//
	// It exists because the publication-identity check could previously only
	// name the account the runtime publishes as and ask the operator to compare
	// it themselves. #82's stated operator UX is the comparison, not the half
	// of it the runtime already knew: the two logins have to be resolved to say
	// whether the review loop actually works.
	//
	// Nil is a truthful answer - the comparison is reported as unmade - and it
	// is never a publication credential: doctor asks this adapter who the
	// OPERATOR is and nothing else.
	OperatorGitHub ForgeViewer
	// Feedback is the operator's admission rule. It is read for its permission
	// threshold, so doctor judges the human's permission against the bar
	// admission will actually apply rather than against a default.
	Feedback FeedbackPolicy

	// OperatorConfigPath and RepositoryRoot are re-read from disk: whether the
	// two layers still load, still tighten, and still validate IS the check.
	OperatorConfigPath string
	RepositoryRoot     string

	// ProjectModel and Policy drive the governance diagnosis.
	ProjectModel domain.ProjectModel
	Policy       domain.EngineeringPolicy

	// Agents is the operator's configured worker registry, already probed. It
	// is passed in rather than probed here so the preflight performs the SAME
	// readiness the `agents` command shows: two answers to "is this worker
	// usable" would eventually disagree.
	Agents []AgentStatus
	// ControlEndpoint is the supervisor's local control path, when one is
	// configured. It is an operator-authority boundary, so doctor reports what
	// it is and refuses to call an exposed one healthy.
	ControlEndpoint string
	// Storage is the operator's bound on local runtime state.
	Storage StateStorage

	// ControllerBuild is the provenance of the binary running this preflight.
	// The zero value is the truthful answer for an unattested build.
	//
	// ControllerBuildError is set instead when resolving that provenance
	// FAILED. The two are different answers and must stay different: an
	// unattested build is one that deliberately claims nothing, while a
	// resolution failure means the preflight could not establish what it is
	// running. Collapsing the second into the first would report a measurement
	// failure as a deliberate design choice.
	ControllerBuild      ControllerBuild
	ControllerBuildError error

	// ControllerRoot is where adopted generations and the "current" stable
	// projection live - the same value `controller status`, `controller
	// build-adopted` and succession itself already use. The entrypoint checks
	// read it to know what a canonical PATH entry must point at; it names no
	// authority of its own.
	ControllerRoot string
	// EntrypointPathEnv is the operator's shell PATH, read once at the
	// composition boundary rather than inside the check, so a diagnosis is
	// reproducible from a value a test can set directly. This is the one
	// place Doctor deliberately reads ambient state: the question being asked
	// is what a real shell would resolve, and answering it from anything else
	// would not be answering that question.
	EntrypointPathEnv string
}

// Doctor answers every check independently and returns the report. It never
// returns an error: a question that failed is a check result, not a control
// flow fault.
func Doctor(ctx context.Context, in DoctorInput) DoctorReport {
	checks := make([]DoctorCheck, 0, 25)
	checks = append(checks, doctorState(in)...)
	checks = append(checks, doctorGit(in)...)
	checks = append(checks, doctorProvider(in)...)
	checks = append(checks, doctorAssurance(ctx, in)...)
	checks = append(checks, doctorGitHub(ctx, in)...)
	checks = append(checks, doctorConfig(in)...)
	checks = append(checks, doctorGovernance(in))
	checks = append(checks, doctorController(in))
	checks = append(checks, doctorEntrypoint(in)...)
	checks = append(checks, doctorAgents(in)...)
	checks = append(checks, doctorControlEndpoint(in))
	checks = append(checks, doctorStateStorage(in))

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
// Controller provenance
// ---------------------------------------------------------------------------

const doctorGroupController = "controller"

// doctorController reports WHICH BINARY is running the preflight.
//
// Every other check answers a question about the environment. This one answers
// the question an operator has about the tool asking them: is this an adopted
// runtime, or a build of source that has not itself been adopted? The
// distinction already exists in the model and is already recorded on every run
// the runtime creates, but it was not observable from any read-only command, so
// the one moment it matters most - standing in front of a freshly built
// controller and asking what it is - had no answer.
//
// It is a report, never a gate. An unattested build is legal and says so; a
// pre-adoption build is legal and says so. Neither is a FAIL, because which
// build you are running is a fact about your situation, not a fault in it.
func doctorController(in DoctorInput) DoctorCheck {
	const id = "controller.build"
	// A resolution failure is not an unattested build. "I claim nothing" is a
	// deliberate, legal property of a plain go build; "I could not find out
	// what I am" is a broken preflight reporting on itself. Reporting the
	// second as the first would turn a measurement failure into a design
	// choice, and an operator would read WARN where they should read FAIL.
	if in.ControllerBuildError != nil {
		return fail(doctorGroupController, id,
			"the provenance of the running binary could not be established: "+in.ControllerBuildError.Error()+
				". This is not an unattested build - a build that claims nothing says so, and this one could not be measured at all")
	}
	build := in.ControllerBuild
	if !build.Attested() {
		return warn(doctorGroupController, id,
			"this controller's build provenance is unattested: it records nothing about which source produced it, which is legal")
	}
	// The SAME validation the runtime constructor trusts, not a second opinion.
	// A partial or malformed claim is refused rather than displayed as a
	// healthy build: provenance that does not survive its own rules is worse
	// than none, because it reads as proof.
	if err := build.validateAttested(); err != nil {
		return fail(doctorGroupController, id,
			"this controller states a provenance that is not complete or not well formed: "+err.Error()+
				". A partial claim is refused rather than shown as a healthy build")
	}
	detail := fmt.Sprintf("this controller's build provenance is %s: version %s, source %s, tree %s, binary sha256:%s",
		build.Kind, build.Version, build.SourceRevision, build.SourceTree, build.BinarySHA256)
	if build.Kind != ControllerAdopted {
		return pass(doctorGroupController, id, detail+
			". A build of source that has not itself been adopted demonstrates runtime behaviour only; it confers no authority on its own source")
	}
	return pass(doctorGroupController, id, detail+
		". Its source is adopted, which is a fact established by external merge and never by the runtime itself")
}

// ---------------------------------------------------------------------------
// PATH entrypoint
// ---------------------------------------------------------------------------

const doctorGroupInstall = "install"

// doctorEntrypoint answers #319: is there ONE canonical local install, and
// does the operator's shell actually reach it?
//
// It never mutates anything - `controller install` is the one place that
// does - and it never guesses which of several PATH entries an operator
// meant. A stale copy earlier on PATH is reported as exactly that, even when
// a canonical entrypoint also exists further along: the shell would resolve
// the stale one, and a report that looked past it to the healthy entry
// further down would be a green diagnosis of a broken installation.
//
// The diagnosis is computed once, against the SAME durable controller
// authority `controller status` and `controller install` already consult,
// and handed to both checks below: two independently-opened stores could in
// principle observe two different moments of the same database, which is
// exactly the kind of second opinion #319 must not introduce.
func doctorEntrypoint(in DoctorInput) []DoctorCheck {
	if strings.TrimSpace(in.ControllerRoot) == "" {
		return []DoctorCheck{
			warn(doctorGroupInstall, "install.entrypoint", "no controller root is configured, so the canonical PATH entrypoint cannot be diagnosed"),
			warn(doctorGroupInstall, "install.path_shadowing", "no controller root is configured, so PATH shadowing cannot be diagnosed"),
		}
	}
	if strings.TrimSpace(in.StateDir) == "" {
		reason := "no state directory is configured, so durable controller authority cannot be consulted to diagnose the PATH entrypoint"
		return []DoctorCheck{
			warn(doctorGroupInstall, "install.entrypoint", reason),
			warn(doctorGroupInstall, "install.path_shadowing", reason),
		}
	}
	store, err := OpenSQLiteOperationStore(in.StateDir)
	if err != nil {
		reason := "the runtime database could not be opened to diagnose the PATH entrypoint: " + err.Error()
		return []DoctorCheck{
			warn(doctorGroupInstall, "install.entrypoint", reason),
			warn(doctorGroupInstall, "install.path_shadowing", reason),
		}
	}
	defer store.Close()
	diagnosis := DiagnoseEntrypoint(in.EntrypointPathEnv, in.ControllerRoot, store)
	return []DoctorCheck{doctorEntrypointCanonical(diagnosis), doctorEntrypointShadowing(diagnosis)}
}

// doctorEntrypointCanonical states the canonical public entrypoint path, the
// path the shell actually resolves, the adopted target it should reach, and
// whether it does.
func doctorEntrypointCanonical(diagnosis EntrypointDiagnosis) DoctorCheck {
	const id = "install.entrypoint"
	if diagnosis.Winner == nil {
		return warn(doctorGroupInstall, id, fmt.Sprintf(
			"no %s is on PATH; the canonical entrypoint would be a symlink to %s. Run `controller install` to establish it",
			EntrypointExecutableName, diagnosis.CanonicalTarget))
	}
	if !diagnosis.Canonical {
		return fail(doctorGroupInstall, id, fmt.Sprintf(
			"the %s that resolves on PATH is %s, and it is not the canonical entrypoint: %s. Canonical target: %s. Run `controller install` to replace the resolved entry with a symlink to it",
			EntrypointExecutableName, diagnosis.Winner.Path, diagnosis.Detail, diagnosis.CanonicalTarget))
	}
	if !diagnosis.AuthorityConsistent {
		return fail(doctorGroupInstall, id, fmt.Sprintf(
			"the canonical entrypoint %s is a symlink through %s, and durable controller authority does not support it: %s",
			diagnosis.Winner.Path, diagnosis.CanonicalTarget, diagnosis.Detail))
	}
	if !diagnosis.ReachesAdopted {
		return fail(doctorGroupInstall, id, fmt.Sprintf(
			"the canonical entrypoint %s does not reach the adopted target %s: %s",
			diagnosis.Winner.Path, diagnosis.CanonicalTarget, diagnosis.Detail))
	}
	return pass(doctorGroupInstall, id, fmt.Sprintf(
		"the %s that resolves on PATH (%s) is the canonical entrypoint and reaches the adopted target %s",
		EntrypointExecutableName, diagnosis.Winner.Path, diagnosis.CanonicalTarget))
}

// doctorEntrypointShadowing names every PATH entry other than the one that
// wins, so a stale or duplicate copy is visible even when it happens not to
// be the one the shell currently resolves.
func doctorEntrypointShadowing(diagnosis EntrypointDiagnosis) DoctorCheck {
	const id = "install.path_shadowing"
	if diagnosis.Winner == nil {
		return pass(doctorGroupInstall, id, fmt.Sprintf("no %s is on PATH at all, so there is nothing to shadow", EntrypointExecutableName))
	}
	shadowed := diagnosis.Shadowed()
	if len(shadowed) == 0 {
		return pass(doctorGroupInstall, id, fmt.Sprintf("%s appears at exactly one place on PATH (%s)", EntrypointExecutableName, diagnosis.Winner.Path))
	}
	paths := make([]string, len(shadowed))
	for i, candidate := range shadowed {
		paths[i] = candidate.Path
	}
	return warn(doctorGroupInstall, id, fmt.Sprintf(
		"%s also exists later on PATH at %s, shadowed by the resolved %s; remove the stale entries so they cannot be reached by a different PATH order",
		EntrypointExecutableName, strings.Join(paths, ", "), diagnosis.Winner.Path))
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
	lock, err := AcquireControllerInstanceLock(in.StateDir, NewRuntimeOwner())
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
	lock, err := AcquireControllerInstanceLock(in.StateDir, owner)
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
		return fail(doctorGroupProvider, id, "no execution provider is configured, so no protected-eligible provider exists; configure an agents registry or set provider.kind")
	}
	// An operator_trusted worker is a TRUTHFUL classification, not a degraded
	// protected one. Reporting it as a failure would tell an operator running
	// their own authenticated Codex or Claude Code CLI that their setup is
	// broken, when what is actually true is that this worker's host read
	// confinement is unproven. That distinction is the whole point of having
	// two trust modes, and doctor states it rather than collapsing it into red.
	//
	// It states only what is ENFORCED. This text used to end "ineligible for
	// work whose policy requires protected execution", which reads as a rule
	// the runtime applies; there is no policy vocabulary for requiring
	// protected execution, so nothing matches work to a trust mode. e09b170
	// removed the same claim from the durable documentation and this is the
	// last copy of it. See the known limitations in ROADMAP.md.
	if agent, ok := defaultAgentStatus(in); ok && agent.TrustMode == TrustOperatorTrusted {
		return pass(doctorGroupProvider, id, fmt.Sprintf(
			"the default agent %q runs as %s: Zenchron injects no publication credential and selects the provider's least-privilege automation mode, "+
				"but a CLI running under your account can read what your account can read, so host read confinement is UNPROVEN. A protected agent must "+
				"prove that boundary before it executes anything, and a run created under protected is never continued by an operator_trusted worker; "+
				"choosing this worker is your decision, because policy cannot yet require protected execution", agent.ID, TrustOperatorTrusted))
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
	// A native CLI authenticates ITSELF. Zenchron holds no credential for it,
	// deliberately, and demanding one would be asking an operator to copy a
	// token into configuration for a tool they have already logged into - which
	// is exactly what this milestone exists to stop requiring.
	if agent, ok := defaultAgentStatus(in); ok && nativeCLIKind(agent.Kind) {
		return pass(doctorGroupProvider, id, fmt.Sprintf(
			"the default agent %q is a local CLI that authenticates itself; Zenchron holds no credential for it and injects none. "+
				"Its observed authentication state is %q (%s)", agent.ID, agent.AuthMode, agent.AuthModeSource))
	}
	path := strings.TrimSpace(in.ProviderCredentialPath)
	if path == "" {
		return fail(doctorGroupProvider, id, "no provider credential path is configured; set the brokered agent's credential_path to an operator-controlled file")
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

func doctorAssurance(ctx context.Context, in DoctorInput) []DoctorCheck {
	// The frozen diagnosis is called exactly once; the three Docker-derived
	// fields are separate questions and are reported separately.
	diagnosis := DiagnoseSandbox(in.Codex, in.Sandbox)
	return []DoctorCheck{
		doctorDockerEndpoint(in),
		doctorAssuranceImage(in),
		doctorVerifierSandbox(diagnosis),
		doctorSandboxBoundaries(diagnosis),
		doctorAssuranceToolchain(ctx, in, diagnosis),
		doctorDependencyCache(in),
		doctorSemanticAssurance(in),
		doctorDependencyPreparation(in, diagnosis),
	}
}

// doctorAssuranceToolchain proves the CONFIGURED image resolves the verifier's
// toolchain under the runtime's exact sandbox environment. It is deliberately
// not derived from Docker readiness: a reachable daemon holding the pinned image
// says a container can start, and the fifth dogfood started containers all the
// way to exit 127 on `go: not found`. The probe runs the same dockerBase
// boundary with no network, mounts only an empty temporary directory, downloads
// nothing, and touches no cache and no candidate.
func doctorAssuranceToolchain(ctx context.Context, in DoctorInput, diagnosis SandboxDoctor) DoctorCheck {
	const id = "assurance.toolchain"
	if diagnosis.VerifierSandbox != "enforceable" {
		return fail(doctorGroupAssurance, id, "the verifier sandbox is "+diagnosis.VerifierSandbox+", so the configured image could not be asked whether it resolves a Go toolchain")
	}
	out, err := in.Sandbox.ProbeToolchain(ctx)
	if err != nil {
		return fail(doctorGroupAssurance, id, "the pinned assurance image did not resolve the Go toolchain on the runtime sandbox path "+sandboxPATH+": "+boundedDetail(sanitizedDetail(probeDetail(out, err)))+". The image must carry a toolchain reachable on that path; the runtime never falls back to a host Go")
	}
	resolved := strings.Fields(strings.TrimSpace(string(out.Stdout)))
	if len(resolved) < 3 {
		return fail(doctorGroupAssurance, id, "the pinned assurance image answered the toolchain probe incompletely: "+boundedDetail(sanitizedDetail(strings.Join(resolved, " "))))
	}
	return pass(doctorGroupAssurance, id, "the pinned assurance image resolves go and gofmt on the runtime sandbox path "+sandboxPATH+" ("+boundedDetail(sanitizedDetail(strings.Join(resolved, " ")))+"), proven by running it with no network and nothing but an empty directory mounted")
}

// probeDetail keeps the container's own words when it has any, because "go: not
// found" and "no such image" need different operator actions.
func probeDetail(out CommandOutput, err error) string {
	detail := strings.TrimSpace(string(out.Stderr))
	if detail == "" {
		detail = strings.TrimSpace(string(out.Stdout))
	}
	if detail == "" {
		return err.Error()
	}
	return detail
}

// doctorSemanticAssurance reports whether the INDEPENDENT semantic acceptance
// producer is configured and can actually initialize. It makes no model call:
// a paid inference request is not a readiness check, and this one is free.
//
// Absent is reported, not failed - a configuration with no semantic producer is
// coherent, and the runtime refuses a contract requiring that class before any
// expensive work. What IS a failure is a producer that is configured, declares
// the class, and then cannot initialize: that would let a contract pass
// fulfillability and then fail after the work was done.
func doctorSemanticAssurance(in DoctorInput) DoctorCheck {
	const id = "assurance.semantic"
	if in.SemanticAssurance == nil {
		return warn(doctorGroupAssurance, id, "no independent semantic acceptance producer is configured, so evidence class "+
			string(SemanticEvidenceClass)+" is not producible; a contract requiring it is refused before any execution rather than failing after the work")
	}
	producible := ProducibleEvidenceClasses(in.SemanticAssurance)
	if !producible[SemanticEvidenceClass] {
		return fail(doctorGroupAssurance, id, fmt.Sprintf("the configured semantic assurance provider %T does not declare %s, so it cannot discharge a semantic acceptance claim",
			in.SemanticAssurance, SemanticEvidenceClass))
	}
	verifier, ok := in.SemanticAssurance.(OpenAISemanticVerifier)
	if !ok {
		return pass(doctorGroupAssurance, id, fmt.Sprintf("an independent semantic acceptance producer (%T) declares %s", in.SemanticAssurance, SemanticEvidenceClass))
	}
	if strings.TrimSpace(verifier.Model) == "" {
		return fail(doctorGroupAssurance, id, "the semantic assurance provider declares "+string(SemanticEvidenceClass)+" but names no model, so it cannot answer")
	}
	if _, err := verifier.credential(""); err != nil {
		return fail(doctorGroupAssurance, id, "the semantic assurance provider declares "+string(SemanticEvidenceClass)+
			" but its credential is unavailable: "+err.Error()+"; its contents were not read")
	}
	if _, err := semanticToolDefinitions(); err != nil {
		return fail(doctorGroupAssurance, id, "the semantic assurance read-only tool surface does not validate: "+err.Error())
	}
	return pass(doctorGroupAssurance, id, "the independent semantic acceptance producer "+semanticProviderID+" declares "+
		string(SemanticEvidenceClass)+", resolves its credential, and advertises only read-only tools; verifier definition "+
		SemanticVerifierDefinition()[:16]+", model "+verifier.Model+". No inference request was made")
}

// doctorDependencyCache proves the operator-provisioned cache is actually
// provisioned. The frozen runtime contract is that verification is offline
// against pre-warmed material it never downloads, so a cache that is absent,
// not a directory, or EMPTY is a false readiness claim, not a warning: the
// fifth dogfood's cache existed, held 0 bytes, and doctor reported FAIL=0.
func doctorDependencyCache(in DoctorInput) DoctorCheck {
	const id = "assurance.dependency_cache"
	cache := strings.TrimSpace(in.DependencyCacheDir)
	if cache == "" {
		return fail(doctorGroupAssurance, id, "no assurance.dependency_cache_dir is configured; offline verification has no trusted module material to read and never downloads any")
	}
	if !filepath.IsAbs(cache) {
		return fail(doctorGroupAssurance, id, "the dependency cache "+cache+" is not an absolute path")
	}
	info, err := os.Stat(cache)
	if err != nil {
		return fail(doctorGroupAssurance, id, "the dependency cache "+cache+" cannot be inspected: "+err.Error()+"; provision it from the trusted base module graph before a run")
	}
	if !info.IsDir() {
		return fail(doctorGroupAssurance, id, cache+" is not a directory, so it cannot be the module cache")
	}
	entries, err := os.ReadDir(cache)
	if err != nil {
		return fail(doctorGroupAssurance, id, "the dependency cache "+cache+" cannot be read: "+err.Error())
	}
	if len(entries) == 0 {
		return fail(doctorGroupAssurance, id, "the dependency cache "+cache+" is EMPTY. Offline verification reads pre-warmed operator-provisioned modules and downloads nothing, so an empty cache cannot verify any candidate; provision it from the trusted base module graph with the pinned image")
	}
	// Go keeps downloaded module zips and their checksums under
	// <GOMODCACHE>/cache/download, which is what a pre-warmed cache is FOR.
	downloads := "no cache/download tree yet"
	if _, err := os.Stat(filepath.Join(cache, "cache", "download")); err == nil {
		downloads = "it holds a cache/download module tree"
	}
	return pass(doctorGroupAssurance, id, fmt.Sprintf("the dependency cache %s exists and is provisioned (%d top-level entries; %s); nothing was downloaded or written to answer this", cache, len(entries), downloads))
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

// doctorDependencyPreparation is now the JOINT statement: preparation is
// enforceable only when the sandbox can run AND the configured image proved it
// resolves a toolchain. It no longer restates Docker readiness under a name that
// implies more than it checked.
func doctorDependencyPreparation(in DoctorInput, diagnosis SandboxDoctor) DoctorCheck {
	const id = "assurance.dependency_preparation"
	if diagnosis.DependencyPreparation != "enforceable" {
		return fail(doctorGroupAssurance, id, "dependency preparation is "+diagnosis.DependencyPreparation+": the sandbox and the pinned image together did not prove they can run the Go toolchain offline, so verification would fail before it judged anything")
	}
	cache := strings.TrimSpace(in.DependencyCacheDir)
	if cache == "" {
		return fail(doctorGroupAssurance, id, "no assurance.dependency_cache_dir is configured, so offline verification has no trusted module material to read")
	}
	return pass(doctorGroupAssurance, id, "the pinned image runs the Go toolchain offline and the trusted module cache at "+cache+" is mounted read-only into every verification")
}

// ---------------------------------------------------------------------------
// GitHub
// ---------------------------------------------------------------------------

const doctorGroupGitHub = "github"

func doctorGitHub(ctx context.Context, in DoctorInput) []DoctorCheck {
	credential := doctorGitHubCredential(in)
	identity, rate := doctorGitHubRead(ctx, in, credential)
	return []DoctorCheck{credential, doctorPublicationIdentity(ctx, in), doctorGitHubGovernance(ctx, in), identity, rate}
}

// doctorGitHubGovernance probes disclosure, not whether repository policy is
// sufficient for adoption. Raw adapter errors and provenance details are never
// reported: they can contain credential or response data.
func doctorGitHubGovernance(ctx context.Context, in DoctorInput) DoctorCheck {
	const id = "github.governance"
	if in.Governance == nil {
		return fail("github", id, "no governance observer is configured; set github.governance_credential_mode to github-cli for controller build-adopted; serve does not require it")
	}
	if in.Governance.GovernanceProvenance().Role != CredentialRoleGovernance {
		return fail("github", id, "the observer does not identify as governance-observation; configure a separate governance credential")
	}
	repo, err := ParseGitHubRepo(in.Repository.Identity)
	if err != nil {
		return warn("github", id, "governance visibility was not probed; select a valid repository with --repo owner/name")
	}
	rulesets, err := in.Governance.Rulesets(ctx, repo)
	if err != nil {
		return fail("github", id, "governance observation failed; verify the configured governance credential resolves and can read repository rulesets and bypass_actors")
	}
	if len(rulesets) == 0 {
		return warn("github", id, "governance read succeeded but no rulesets were returned; bypass_actors visibility remains unverified")
	}
	for _, ruleset := range rulesets {
		if !ruleset.BypassActorsKnown {
			return fail("github", id, "governance credential cannot disclose bypass_actors for every ruleset; authorize an identity with governance visibility before controller build-adopted")
		}
	}
	return pass("github", id, "governance credential resolved and disclosed rulesets including bypass_actors; adoption policy is validated by controller build-adopted")
}

// doctorViewer resolves the account the runtime publishes as, when the adapter
// can answer. It is a read; doctor makes no write.
func doctorViewer(adapter any, identity string) (GitHubActor, bool) {
	viewer, ok := adapter.(ForgeViewer)
	if !ok || viewer == nil || strings.TrimSpace(identity) == "" {
		return GitHubActor{}, false
	}
	repo, err := ParseGitHubRepo(identity)
	if err != nil {
		return GitHubActor{}, false
	}
	actor, err := viewer.Viewer(context.Background(), repo)
	if err != nil || strings.TrimSpace(actor.Login) == "" {
		return GitHubActor{}, false
	}
	return actor, true
}

// doctorPermission resolves one login's current repository permission.
// PermissionUnresolved is the answer whenever the question could not be asked,
// and it is never read as an admission.
func doctorPermission(ctx context.Context, in DoctorInput, login string) GitHubPermission {
	permissions, ok := in.GitHub.(ForgeActorPermissions)
	if !ok {
		return PermissionUnresolved
	}
	repo, err := ParseGitHubRepo(in.Repository.Identity)
	if err != nil {
		return PermissionUnresolved
	}
	permission, err := permissions.RepositoryPermission(ctx, repo, login)
	if err != nil {
		return PermissionUnresolved
	}
	return permission
}

// doctorPublicationIdentity answers whether the operator can give their own
// workers feedback.
//
// The runtime refuses feedback it authored itself, by identity, so it cannot
// talk itself into a loop. When it publishes with the operator's own `gh`
// credential, the runtime IS the operator as far as GitHub is concerned, and
// that guard refuses the operator's reviews along with its own. Everything
// behaves exactly as designed and the central #63 workflow - review the pull
// request, the right worker continues - silently cannot happen.
//
// It was silent until a live run hit it: the runtime observed the review, bound
// it to the exact head, and recorded "authored by this runtime, so admitting it
// would let the system feed itself" about a human being. This states it up
// front, before an operator spends a subscription discovering it.
func doctorPublicationIdentity(ctx context.Context, in DoctorInput) DoctorCheck {
	const id = "github.publication_identity"
	switch in.GitHubCredentialMode {
	case GitHubCredentialNone:
		return warn(doctorGroupGitHub, id, "github.credential_mode is \"none\", so the runtime publishes nothing and no publication identity exists")
	case GitHubCredentialToken, GitHubCredentialApp:
		return doctorSeparatedIdentities(ctx, in, id)
	}
	return warn(doctorGroupGitHub, id,
		"the runtime publishes with your own `gh` credential, so it acts as YOU on GitHub. Feedback authored by the publishing identity is refused so the "+
			"system cannot feed itself - which means your own reviews and comments will not reach a worker, and the review loop is unavailable. Give the "+
			"runtime an identity of its own with github.credential_mode \""+GitHubCredentialApp+"\" (a GitHub App installation, which is a machine account "+
			"rather than a second person) or \""+GitHubCredentialToken+"\" with github.token_path (a dedicated runtime account's token); nothing here "+
			"weakens the self-loop guard")
}

// doctorSeparatedIdentities answers #82's operator question - can this operator
// give their own workers feedback - by resolving BOTH halves and comparing them.
//
// The mode proves a separate CREDENTIAL, not a separate ACCOUNT: a personal
// access token for the operator's own login sits in that file just as happily
// as a dedicated runtime account's. Claiming distinctness from the mode alone
// would be the same kind of untrue statement this check exists to make, and
// naming only the publishing account - which is what this check used to do -
// left the comparison to an operator who has no way to know it matters.
//
// It is a WARN rather than a FAIL because runs still execute, publish and
// verify; what is unavailable is the remediation loop.
func doctorSeparatedIdentities(ctx context.Context, in DoctorInput, id string) DoctorCheck {
	publication, resolved := doctorViewer(in.GitHub, in.Repository.Identity)
	if !resolved {
		return warn(doctorGroupGitHub, id,
			"the runtime is configured with a publication identity of its own, but the account it authenticates as could not be resolved, so this check "+
				"cannot say whether it is a different actor from you - and feedback admission fails closed until that identity resolves")
	}
	operator, operatorResolved := doctorViewer(in.OperatorGitHub, in.Repository.Identity)
	if !operatorResolved {
		return warn(doctorGroupGitHub, id, fmt.Sprintf(
			"publication identity %q; human feedback actor UNRESOLVED - your own GitHub login could not be read from your local `gh` session, so the two "+
				"halves cannot be compared. Run `gh auth login`; the self-loop guard stays active either way", publication.Login))
	}
	permission := doctorPermission(ctx, in, operator.Login)
	facts := fmt.Sprintf("publication identity %q; human feedback actor %q (permission: %s); self-loop guard active",
		publication.Login, operator.Login, permission)
	threshold := in.Feedback.threshold()
	switch {
	case strings.EqualFold(publication.Login, operator.Login):
		return warn(doctorGroupGitHub, id, facts+fmt.Sprintf(
			" - but those are the SAME account. The runtime is still acting as you, so your own reviews are refused as self-authored and will not reach a "+
				"worker. The credential is separate; the identity is not. Point github.credential_mode %q at a GitHub App installation, or %q at a "+
				"dedicated runtime account's token", GitHubCredentialApp, GitHubCredentialToken))
	case permission == PermissionUnresolved:
		return warn(doctorGroupGitHub, id, facts+fmt.Sprintf(
			" - the two accounts are distinct, so the runtime's own comments are refused by identity, but your permission on %s could not be resolved and "+
				"an unresolved permission is never an admission. Your reviews will not reach a worker until it answers. If the runtime publishes as a "+
				"GitHub App, the likely cause is that the App's repository permissions do not cover the collaborator-permission lookup: widen them, "+
				"approve the change on the installation, and run this again", in.Repository.Identity))
	case !permission.AtLeast(threshold):
		return warn(doctorGroupGitHub, id, facts+fmt.Sprintf(
			" - the two accounts are distinct, so the runtime's own comments are refused by identity, but your login holds %q where feedback admission "+
				"requires %q. Grant your account that permission on %s, or lower feedback.min_permission deliberately",
			permission, threshold, in.Repository.Identity))
	}
	return pass(doctorGroupGitHub, id, facts+
		" - the two are distinct accounts and you clear the feedback permission threshold, so the runtime's own comments are refused by identity while "+
		"your reviews are admitted as engineering feedback")
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

// ---------------------------------------------------------------------------
// Agents, control endpoint and state storage
// ---------------------------------------------------------------------------

// defaultAgentStatus is the worker a run would use when the operator names
// none. Several checks answer differently for a self-authenticating local CLI
// than for a brokered provider, and this is the one place that distinction is
// resolved.
func defaultAgentStatus(in DoctorInput) (AgentStatus, bool) {
	for _, agent := range in.Agents {
		if agent.Default {
			return agent, true
		}
	}
	return AgentStatus{}, false
}

const (
	doctorGroupAgents     = "agents"
	doctorGroupSupervisor = "supervisor"
)

// doctorAgents reports each configured worker INDEPENDENTLY. One unusable agent
// is not a broken system: an operator with Codex installed and Gemini not is in
// a perfectly ordinary state, and a report that collapsed that into one verdict
// would tell them nothing about which work they can actually start.
//
// An unavailable agent is therefore WARN, not FAIL. The one FAIL is having no
// usable worker at all, because that is the state in which no work can begin.
func doctorAgents(in DoctorInput) []DoctorCheck {
	if len(in.Agents) == 0 {
		// A pre-#63 single-provider configuration has no named agents, and it
		// is a legal configuration whose provider is diagnosed by the provider
		// checks above. Saying so is a WARN rather than a FAIL: nothing is
		// broken, and per-agent readiness simply has nothing to report.
		return []DoctorCheck{warn(doctorGroupAgents, "agents.configured",
			"no named execution agents were resolved, so per-agent readiness was not evaluated. "+
				"A configuration written before the agent registry existed is diagnosed by the provider checks instead; "+
				"add an `agents` registry to name the coding CLIs you have installed")}
	}
	checks := make([]DoctorCheck, 0, len(in.Agents)+1)
	usable := 0
	for _, agent := range in.Agents {
		id := "agent." + agent.ID
		detail := fmt.Sprintf("%s (%s, %s): %s", agent.ID, agent.Kind, agent.TrustMode, agent.Detail)
		if agent.Version != "" {
			detail += " [version " + agent.Version + "]"
		}
		// Standing permission for a provider's unsafe mode is surfaced even
		// when unused: it is a posture, and an operator should not have to read
		// a configuration file to discover it.
		if agent.PermissionBypassAllowed {
			detail += "; this agent is configured to ALLOW its provider's permission bypass when an invocation explicitly requests it"
		}
		if agent.Available {
			usable++
			checks = append(checks, pass(doctorGroupAgents, id, detail))
			continue
		}
		checks = append(checks, warn(doctorGroupAgents, id, detail))
	}
	if usable == 0 {
		checks = append(checks, fail(doctorGroupAgents, "agents.usable",
			"no configured agent is usable, so no work can be started; install or authenticate one of the configured coding CLIs"))
	} else {
		checks = append(checks, pass(doctorGroupAgents, "agents.usable",
			fmt.Sprintf("%d of %d configured agents can be invoked", usable, len(in.Agents))))
	}
	return append(checks, doctorWorkerToolchain(in))
}

// doctorWorkerToolchain answers whether a WORKER can attempt the toolchain
// obligations its contract will carry.
//
// The assurance container has always had this check - assurance.toolchain
// proves the pinned image resolves go and gofmt on its own pinned path - and
// the workers that produce the candidates it verifies had none. So the
// verifier's environment was reproducible and the producer's was whatever shell
// started `serve`, and the gap was invisible until a worker had already spent
// an invocation reporting it could not verify its own work.
//
// An undeclared toolchain is a WARN rather than a pass: nothing is broken, the
// inherited environment may well be fine, and the runtime genuinely cannot
// promise that it is.
func doctorWorkerToolchain(in DoctorInput) DoctorCheck {
	const id = "provider.toolchain"
	if !in.Toolchain.Declared() {
		return warn(doctorGroupAgents, id,
			"no worker toolchain is configured, so an execution worker inherits this supervisor's PATH and the runtime cannot establish that it can run the tools its acceptance obligations require. "+
				"Set toolchain.path and toolchain.required_tools to broker a reproducible environment and get a refusal before an invocation is spent rather than a worker that reports it could not verify its own work")
	}
	probe := CLIAgentProvider{Toolchain: in.Toolchain}
	if missing := probe.missingTools(); len(missing) > 0 {
		return fail(doctorGroupAgents, id, fmt.Sprintf(
			"the brokered worker execution environment cannot resolve %s on %s, so a mutating stage is refused before it is dispatched rather than after it fails to verify its own work",
			strings.Join(missing, ", "), in.Toolchain.SearchPath()))
	}
	if len(in.Toolchain.RequiredTools) == 0 {
		return pass(doctorGroupAgents, id, fmt.Sprintf(
			"the brokered worker execution environment is %s; no required tools are declared, so nothing is refused before dispatch",
			in.Toolchain.SearchPath()))
	}
	// WHAT THIS PROVES, stated exactly. Resolution on the brokered path is not
	// permission to execute: the first live dogfood had this check PASS while
	// Claude Code refused every `go` command through its own permission layer,
	// `go version` included. A PASS that implied readiness was asserting more
	// than it had tested, so it now names resolution and points at the
	// per-agent grant that turns resolution into attemptability.
	return pass(doctorGroupAgents, id, fmt.Sprintf(
		"the brokered worker execution environment RESOLVES %s on %s. Resolution is not permission: a provider that gates command execution is additionally granted exactly these executables for the invocation, which each agent check above reports",
		strings.Join(in.Toolchain.RequiredTools, ", "), in.Toolchain.SearchPath()))
}

// doctorControlEndpoint reports the supervisor's authority boundary.
//
// It is a FAIL rather than a warning when the endpoint exists and is reachable
// by other users, because anything that can reach it can start operator_trusted
// coding agents under this account. An ABSENT endpoint is not a fault at all:
// no supervisor is running, which is an ordinary state.
func doctorControlEndpoint(in DoctorInput) DoctorCheck {
	const id = "supervisor.endpoint"
	path := strings.TrimSpace(in.ControlEndpoint)
	if path == "" && strings.TrimSpace(in.StateDir) != "" {
		path = ControlSocketPath(in.StateDir)
	}
	if path == "" {
		return warn(doctorGroupSupervisor, id, "no state directory is configured, so the supervisor control endpoint could not be located")
	}
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return pass(doctorGroupSupervisor, id,
			"no supervisor is running: there is no control endpoint at "+path+". Start one with `zenchron-engineering serve`. "+
				"When it exists it is a "+ControlEndpointMechanism+", and it is local-machine only: there is no network listener")
	}
	if err := AssertControlEndpointSecure(path); err != nil {
		return fail(doctorGroupSupervisor, id, err.Error())
	}
	return pass(doctorGroupSupervisor, id,
		"the supervisor control endpoint at "+path+" is a "+ControlEndpointMechanism+
			". It is local-machine only and carries no stored credential: reaching it requires filesystem access to an owner-only path")
}

// doctorStateStorage reports what the runtime state directory holds against the
// operator's ceiling. Parallel candidate clones make disk an operator-level
// resource, and the honest failure is a typed refusal before allocation rather
// than ENOSPC in the middle of a clone.
func doctorStateStorage(in DoctorInput) DoctorCheck {
	const id = "state.storage"
	if strings.TrimSpace(in.StateDir) == "" {
		return warn(doctorGroupState, id, "no state directory is configured, so its size could not be measured")
	}
	storage := in.Storage
	if storage.Dir == "" {
		storage.Dir = in.StateDir
	}
	used, err := storage.Usage()
	if err != nil {
		return warn(doctorGroupState, id, "the state directory size could not be measured: "+err.Error())
	}
	if storage.CeilingBytes <= 0 {
		// No ceiling is the documented default, and it is what every
		// configuration had before the bound existed. It is reported with its
		// advice rather than as a warning: an operator who has not hit the
		// problem has nothing to repair.
		return pass(doctorGroupState, id, fmt.Sprintf(
			"the state directory holds %d bytes and no ceiling is configured. Each concurrent run adds a full candidate clone, "+
				"so set storage.max_state_bytes to get a typed refusal before the disk fills rather than an error mid-clone", used))
	}
	if err := storage.Admit(); err != nil {
		return fail(doctorGroupState, id, err.Error())
	}
	return pass(doctorGroupState, id, fmt.Sprintf(
		"the state directory holds %d bytes against an operator ceiling of %d, with room for another candidate workspace",
		used, storage.CeilingBytes))
}
