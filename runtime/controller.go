package runtime

// controller.go is the Phase 8 composition root: it validates the dependency
// set once, derives the durable run identity, and exposes the operator-facing
// reads. It contains no orchestration - the reconcile loop lives in
// reconciler.go and the bounded side effects live in operations.go, so nothing
// in cmd/ ever has to know how a run advances.
//
// Two properties are structural rather than conventional:
//
//   - A run's identity is derived from the repository, the source issue, and
//     the operator configuration digest. It never contains a local path, so
//     the same run resumes from a different checkout, a different working
//     directory, or a different process.
//   - An execution provider that cannot prove protected isolation is refused
//     at construction, not at the point of use. There is no path through this
//     package that reaches a provider which failed RequireProtectedIsolation.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ---------------------------------------------------------------------------
// Dependency set
// ---------------------------------------------------------------------------

// Controller kinds. The kind is the coarse trust classification of the binary
// driving a run, and it is what makes "an unadopted build drove this run"
// answerable from durable state alone.
const (
	// ControllerPreAdoptionBuild is a build of source that has NOT been adopted
	// into the trusted default branch - the controller a self-hosting run uses
	// to prove out its own change before that change is merged.
	ControllerPreAdoptionBuild = "pre_adoption_build"
	// ControllerAdopted is a build of source that HAS been adopted.
	ControllerAdopted = "adopted"
	// ControllerUnattested is the absence of build metadata: nothing was
	// injected at build time, so the binary can make no provenance claim. It is
	// never recorded as a payload; it is what a run without recorded provenance
	// reads back as, so an unattested controller can never be mistaken for an
	// adopted one.
	ControllerUnattested = "unattested"
)

// ControllerBuild is the exact binary driving a run. It answers "which code is
// this, and can I get it back" without consulting anything outside the run:
// the kind classifies the trust of the source, the revision and tree identify
// the exact source state the build came from, and BinarySHA256 identifies the
// artifact that actually ran.
//
// Three properties are structural rather than conventional:
//
//   - It carries NO operator configuration. Controller provenance and
//     configuration identity are separate facts, so a configuration change can
//     never masquerade as a different binary. ConfigDigest is represented
//     independently, and ControllerSHA256 binds both without merging them.
//   - SourceRevision and SourceTree are injected by the controlled build
//     (-ldflags -X) from the exact checkout that produced the binary. They are
//     never discovered from ambient state at run time, where the checkout may
//     have moved on.
//   - BinarySHA256 is computed from the executable actually running, not read
//     from an adjacent metadata file. A binary cannot contain its own final
//     digest, so it is measured at startup instead of embedded.
type ControllerBuild struct {
	Kind           string `json:"kind"`
	Version        string `json:"version"`
	SourceRevision string `json:"source_revision"`
	SourceTree     string `json:"source_tree"`
	BinarySHA256   string `json:"binary_sha256"`
}

// Attested reports whether this build makes a provenance CLAIM. An unattested
// controller is legal - it is what a plain `go build` produces - but it claims
// nothing, records no provenance, and is distinguishable from both kinds that
// do. Naming the unattested kind explicitly is still no claim; setting any
// other field is.
func (b ControllerBuild) Attested() bool {
	return (b.Kind != "" && b.Kind != ControllerUnattested) ||
		b.Version != "" || b.SourceRevision != "" || b.SourceTree != "" || b.BinarySHA256 != ""
}

// validate is the construction-time check: nothing injected is legal, but a
// partially injected build is not. A binary that claims a source revision
// without a binary digest is exactly the provenance gap this type exists to
// close, so it is refused rather than recorded.
func (b ControllerBuild) validate() error {
	if !b.Attested() {
		return nil
	}
	return b.validateAttested()
}

// validateAttested is also the run.created payload schema: a recorded
// provenance is always complete.
func (b ControllerBuild) validateAttested() error {
	switch b.Kind {
	case ControllerPreAdoptionBuild, ControllerAdopted:
	default:
		return fmt.Errorf("controller kind %q is neither %q nor %q", b.Kind, ControllerPreAdoptionBuild, ControllerAdopted)
	}
	if !isSHA256Hex(b.BinarySHA256) {
		return errors.New("controller binary_sha256 must be the 64 lowercase hex digits of the running executable")
	}
	return errors.Join(
		required("version", b.Version),
		required("source_revision", b.SourceRevision),
		required("source_tree", b.SourceTree),
	)
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// ConfigDigest is the operator configuration this run was created under. It is
// part of the run identity: a run created under different governance settings
// is a different run, not the same run silently continued.
type ConfigDigest struct {
	Global     string `json:"global"`
	Repository string `json:"repository"`
}

// RunBudgets bounds one run. WallLimit terminates a run that cannot finish;
// the attempt ceilings bound each bounded operation kind independently, so a
// failing verifier cannot consume the execution provider's budget.
type RunBudgets struct {
	WallLimit            time.Duration `json:"wall_limit"`
	MaxExecutionAttempts int           `json:"max_execution_attempts"`
	// MaxExecutionContinuations bounds DISTINCT continuation execution
	// bindings for one run. MaxExecutionAttempts bounds retries of ONE
	// binding. They are independent resources: three retries of continuation|A
	// spend three attempts and one continuation.
	//
	// It is omitempty because a run persisted before #54 has no value for it,
	// and that absence is meaningful - see runState.continuationLimit.
	MaxExecutionContinuations int `json:"max_execution_continuations,omitempty"`
	MaxRemediationAttempts    int `json:"max_remediation_attempts"`
	MaxAssuranceAttempts      int `json:"max_assurance_attempts"`
}

// Dependencies is the complete, explicit input to a runtime instance. Every
// external system is a seam; nothing here is discovered from ambient state.
type Dependencies struct {
	Store     *SQLiteOperationStore
	Clock     Clock
	Owner     string
	Liveness  OwnerLiveness
	GitHub    GitHubAdapter
	Provider  ExecutionProvider
	Assurance AssuranceProvider
	// SemanticAssurance is the INDEPENDENT semantic acceptance producer. It is
	// optional: without it a contract requiring semantic_acceptance is refused
	// as unfulfillable before any expensive work, which is the honest state of a
	// configuration that cannot answer that question.
	SemanticAssurance AssuranceProvider
	Artifacts         ArtifactStore
	ProjectModel      domain.ProjectModel
	Policy            domain.EngineeringPolicy
	StateDir          string
	Repository        RepositoryTarget
	Remote            RemoteIdentity
	// Credentials is the single operator-controlled repository control-plane
	// authority for this runtime: it authorizes the GitHub REST adapter, the
	// runtime-owned base clone/fetch, and the runtime-owned push. It is
	// injected here and threaded explicitly into every workspace this runtime
	// constructs, so two runtimes in one process never share one authority.
	Credentials  CredentialProvider
	ControllerID string
	// ControllerBuild is this controller's provenance. It is injected by the
	// composition root from build-time metadata, never discovered here.
	ControllerBuild ControllerBuild
	ConfigDigest    ConfigDigest
	Budgets         RunBudgets
	// MaxConcurrentRuns requests a ceiling on concurrently driven runs. It is
	// clamped to OperatorMaxConcurrentRuns, so a CLI flag or repository
	// configuration can only LOWER the ceiling; neither can raise it.
	MaxConcurrentRuns int
	// OperatorMaxConcurrentRuns is the explicitly operator-authorized ceiling.
	// Zero means the M0 default of one. It exists as a separate field so that
	// "raise the ceiling" is structurally unreachable from anything a
	// repository can influence.
	OperatorMaxConcurrentRuns int
}

// Outcome is what one Reconcile settled on. It is the CLI's whole answer.
type Outcome struct {
	RunID       string      `json:"run_id"`
	Disposition Disposition `json:"disposition"`
	Reason      string      `json:"reason,omitempty"`
}

// ExitCode maps the outcome onto the stable run-mode exits in runtime.go.
func (o Outcome) ExitCode() int {
	switch o.Disposition {
	case Completed:
		return ExitCompleted
	case Failed:
		return ExitFailed
	case Cancelled:
		return ExitCancelled
	case Waiting, Active:
		return ExitWaiting
	default:
		return ExitInvalid
	}
}

// ---------------------------------------------------------------------------
// Typed diagnostics
// ---------------------------------------------------------------------------

// RunConflictError is the refusal StartOrResumeIssueRun returns when durable
// state for the derived identity exists but does not describe this work. It is
// deliberately not a silent new run: an incoherent durable row is an operator
// problem, not something to route around.
type RunConflictError struct{ RunID, Detail string }

func (e *RunConflictError) Error() string {
	return "run_conflict: run " + e.RunID + ": " + e.Detail
}

// DependencyError names the missing or ineligible dependency.
type DependencyError struct{ Detail string }

func (e *DependencyError) Error() string { return "invalid runtime dependencies: " + e.Detail }

// ---------------------------------------------------------------------------
// Runtime
// ---------------------------------------------------------------------------

// EngineeringRuntime is one controller instance bound to one repository.
type EngineeringRuntime struct {
	deps       Dependencies
	scheduler  Scheduler
	flow       KernelFlow
	repo       GitHubRepo
	controller string // digest binding controller identity to configuration
}

// maxRunGenerations bounds the identity probe. A repository that has burned
// through this many terminal runs for one issue has an operator problem, not a
// scheduling problem.
const maxRunGenerations = 64

// NewEngineeringRuntime validates the dependency set and refuses an execution
// provider that cannot prove protected isolation. Every other constructor
// concern is deliberately absent: no directories are created, no database is
// opened, and no network call is made, so constructing a runtime is safe.
func NewEngineeringRuntime(d Dependencies) (*EngineeringRuntime, error) {
	if d.Store == nil {
		return nil, &DependencyError{Detail: "a durable operation store is required"}
	}
	if d.GitHub == nil {
		return nil, &DependencyError{Detail: "a GitHub adapter is required"}
	}
	if d.Provider == nil {
		return nil, &DependencyError{Detail: "an execution provider is required"}
	}
	if d.Assurance == nil {
		return nil, &DependencyError{Detail: "an assurance provider is required"}
	}
	if d.Artifacts.Root == "" {
		return nil, &DependencyError{Detail: "a local artifact store root is required"}
	}
	if strings.TrimSpace(d.StateDir) == "" {
		return nil, &DependencyError{Detail: "an operator state directory is required"}
	}
	if strings.TrimSpace(d.ControllerID) == "" {
		return nil, &DependencyError{Detail: "a controller identity is required"}
	}
	if err := d.ControllerBuild.validate(); err != nil {
		return nil, &DependencyError{Detail: "controller provenance: " + err.Error()}
	}
	if strings.TrimSpace(d.Owner) == "" {
		return nil, &DependencyError{Detail: "a scheduler owner identity is required"}
	}
	if d.Repository.Identity == "" || d.Repository.Remote == "" || d.Repository.DefaultBranch == "" {
		return nil, &DependencyError{Detail: "the repository target needs an identity, a remote, and a default branch"}
	}
	if d.Remote.URL == "" {
		return nil, &DependencyError{Detail: "a governed remote identity is required"}
	}
	if _, err := domain.Encode(d.ProjectModel); err != nil {
		return nil, &DependencyError{Detail: "project model: " + err.Error()}
	}
	if _, err := domain.Encode(d.Policy); err != nil {
		return nil, &DependencyError{Detail: "engineering policy: " + err.Error()}
	}
	// Fail closed on isolation before anything can reach the provider.
	if err := RequireProtectedIsolation(d.Provider); err != nil {
		return nil, &DependencyError{Detail: err.Error()}
	}
	repo, err := parseGitHubRepo(d.Repository.Identity)
	if err != nil {
		return nil, &DependencyError{Detail: err.Error()}
	}
	if d.Clock == nil {
		d.Clock = RealClock{}
	}
	d.Budgets = d.Budgets.defaults()
	// ControllerSHA256 binds BOTH facts a run must not silently change under:
	// the exact build and the operator configuration. They are separate members
	// rather than one merged blob, so the structured provenance recorded beside
	// this digest stays independent of configuration - and an unattested build
	// contributes no member at all, which keeps the digest of a run created
	// before provenance existed byte-identical to what it was.
	var build *ControllerBuild
	if d.ControllerBuild.Attested() {
		build = &d.ControllerBuild
	}
	controller, err := Digest(struct {
		Controller string           `json:"controller"`
		Build      *ControllerBuild `json:"build,omitempty"`
		Config     ConfigDigest     `json:"config"`
	}{d.ControllerID, build, d.ConfigDigest})
	if err != nil {
		return nil, err
	}
	return &EngineeringRuntime{
		deps: d,
		scheduler: Scheduler{
			Store: d.Store, Clock: d.Clock, Owner: d.Owner, Liveness: d.Liveness,
			LeaseDuration:     time.Minute,
			MaxConcurrentRuns: resolveMaxConcurrentRuns(d.MaxConcurrentRuns, d.OperatorMaxConcurrentRuns),
		},
		flow:       KernelFlow{},
		repo:       repo,
		controller: controller,
	}, nil
}

func (b RunBudgets) defaults() RunBudgets {
	if b.WallLimit <= 0 {
		b.WallLimit = time.Hour
	}
	if b.MaxExecutionAttempts <= 0 {
		b.MaxExecutionAttempts = 2
	}
	if b.MaxExecutionContinuations <= 0 {
		b.MaxExecutionContinuations = DefaultMaxExecutionContinuations
	}
	if b.MaxRemediationAttempts <= 0 {
		b.MaxRemediationAttempts = 2
	}
	if b.MaxAssuranceAttempts <= 0 {
		b.MaxAssuranceAttempts = 2
	}
	return b
}

func parseGitHubRepo(identity string) (GitHubRepo, error) {
	parts := strings.Split(identity, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return GitHubRepo{}, fmt.Errorf("repository identity %q is not owner/name", identity)
	}
	return GitHubRepo{Owner: parts[0], Name: parts[1]}, nil
}

// ---------------------------------------------------------------------------
// Run identity
// ---------------------------------------------------------------------------

// issueRunID derives the durable run identity. It binds the repository, the
// source issue, the operator configuration, and a generation counter - and
// nothing else. In particular it never binds a filesystem path, so the same
// logical run is found again from any checkout.
func issueRunID(repository string, issue int, config ConfigDigest, generation int) (string, error) {
	d, err := Digest(struct {
		Repository string       `json:"repository"`
		Issue      int          `json:"issue"`
		Config     ConfigDigest `json:"config"`
		Generation int          `json:"generation"`
	}{repository, issue, config, generation})
	if err != nil {
		return "", err
	}
	return "run-" + d[:32], nil
}

// issueGoal is the run's durable statement of which source it answers. It is
// runtime-authored text, not issue text: the untrusted title and body are
// pinned in the source snapshot, never in the run's identity fields.
func issueGoal(repository string, issue int) string {
	return "github-issue:" + repository + "#" + strconv.Itoa(issue)
}

// StartOrResumeIssueRun is idempotent. It probes the deterministic identity
// space in generation order and takes the first coherent answer: an existing
// non-terminal run for this issue is resumed, a terminal one is stepped over,
// and only an unused identity creates a run. A durable row that disagrees with
// the requested work is a typed refusal, never a silent second run.
func (r *EngineeringRuntime) StartOrResumeIssueRun(ctx context.Context, issue int) (string, error) {
	outcome, err := r.StartIssueRun(ctx, issue, AdoptCompatibleGeneration)
	return outcome.RunID, err
}

// GenerationMode states what a caller MEANT by asking to work on an issue.
//
// It exists because the two meanings were one command. `run issue <n>` derives a
// deterministic identity and adopted whatever non-terminal run it found there,
// whichever controller had created it - so an invocation intended to start a
// fresh generation under a new controller silently reconciled a historical run
// instead. That is how run-0943e257539346f8763db04505cbf322 gained an event
// from a batch that believed it was starting something new.
type GenerationMode int

const (
	// AdoptCompatibleGeneration continues an existing non-terminal generation
	// when it was created by THIS controller, and refuses - naming the run -
	// when it was created by another. It never creates a second generation
	// alongside a live one.
	AdoptCompatibleGeneration GenerationMode = iota
	// NewGeneration always creates a new run with a new id. An existing
	// generation is left exactly as it is: append-only journal, candidate
	// workspace, disposition, all untouched.
	NewGeneration
)

// StartOutcome says WHICH run a caller got and whether it is new. A caller that
// asked for fresh work and received an adopted run would otherwise have no way
// to tell, which is the whole defect.
type StartOutcome struct {
	RunID   string
	Adopted bool
	// AdoptedFrom is the controller identity that created an adopted run. It is
	// this controller's own digest whenever adoption happened at all, because
	// adoption across controllers is refused.
	AdoptedFrom string
}

// RunAdoptionRefusedError is the typed refusal for a live generation this
// controller did not create. It names the run so an operator can inspect it,
// and says what to do: continue it with the controller that owns it, or ask for
// a new generation explicitly.
type RunAdoptionRefusedError struct {
	RunID, Owner, Detail string
}

func (e *RunAdoptionRefusedError) Error() string {
	return "run " + e.RunID + " is a live generation created by a different controller (" + short12(e.Owner) +
		"): " + e.Detail + ". Continue it with that controller, or create a new generation explicitly"
}

func short12(digest string) string {
	if len(digest) > 12 {
		return digest[:12]
	}
	return digest
}

// StartIssueRun is the one entry point for beginning work on an issue. Which
// generation a caller gets is stated by the mode, never inferred.
func (r *EngineeringRuntime) StartIssueRun(ctx context.Context, issue int, mode GenerationMode) (StartOutcome, error) {
	if issue <= 0 {
		return StartOutcome{}, fmt.Errorf("issue number must be positive")
	}
	goal := issueGoal(r.deps.Repository.Identity, issue)
	for generation := 0; generation < maxRunGenerations; generation++ {
		runID, err := issueRunID(r.deps.Repository.Identity, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return StartOutcome{}, err
		}
		existing, ok, err := r.deps.Store.Run(runID)
		if err != nil {
			return StartOutcome{}, err
		}
		if !ok {
			// A free slot. Under either mode this is a NEW run, and the source
			// claim below is what keeps two writers from taking the same one.
			created, err := r.createRun(ctx, runID, goal)
			return StartOutcome{RunID: created}, err
		}
		if existing.Repository != r.deps.Repository.Identity || existing.Goal != goal {
			return StartOutcome{}, &RunConflictError{RunID: runID, Detail: "durable run describes different work"}
		}
		snapshot, err := r.deps.Store.Replay(runID)
		if err != nil {
			return StartOutcome{}, &RunConflictError{RunID: runID, Detail: err.Error()}
		}
		if terminalDisposition(snapshot.Disposition) {
			// A finished generation is a boundary: the next slot is the next
			// generation, under either mode.
			continue
		}
		if mode == NewGeneration {
			// A live generation is never adopted by a caller that asked for a
			// fresh one, and it is never disturbed either. The search moves to
			// the next slot, leaving this run exactly as it is.
			continue
		}
		if existing.ControllerSHA256 != r.controller {
			return StartOutcome{}, &RunAdoptionRefusedError{
				RunID: runID, Owner: existing.ControllerSHA256,
				Detail: "adopting it would reconcile another controller's work under this one",
			}
		}
		return StartOutcome{RunID: runID, Adopted: true, AdoptedFrom: existing.ControllerSHA256}, nil
	}
	return StartOutcome{}, fmt.Errorf("issue %d has exhausted %d run generations", issue, maxRunGenerations)
}

func (r *EngineeringRuntime) createRun(_ context.Context, runID, goal string) (string, error) {
	now := r.deps.Clock.Now()
	budgets := r.deps.Budgets.defaults()
	run := EngineeringRun{
		SchemaVersion:    SchemaVersion,
		ID:               runID,
		Repository:       r.deps.Repository.Identity,
		Goal:             goal,
		Phase:            Contract,
		Disposition:      Active,
		Base:             Ref{ID: r.deps.Repository.DefaultBranch},
		Candidate:        Candidate{Branch: candidateBranch(runID)},
		ControllerSHA256: r.controller,
		// A new run persists the bounds it was created under. Today the
		// continuation bound is the one runState reads back from here, so it
		// is the one whose terminal decision replays from durable state rather
		// than from whatever is configured afterwards; the wall limit and the
		// attempt ceilings are still read live. Persisting the whole record
		// now is what lets the rest follow without another schema change.
		Budgets:   &budgets,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// ClaimRun is the cross-process source claim: one conditional INSERT on the
	// derived identity, so the database decides which process created this run.
	// A caller that loses adopts the winner's run - it must not fall through to
	// PutRun, whose upsert would overwrite the row the winner already hashed
	// its genesis event against. See source_claim.go.
	claimed, err := r.deps.Store.ClaimRun(run)
	if err != nil {
		return "", err
	}
	if !claimed {
		return runID, nil
	}
	// The genesis event is where the structured provenance becomes durable. The
	// run row can only carry the digest (its shape is frozen), so the fields the
	// digest is over are recorded here, in the same append-only, hash-chained
	// journal - not in an adjacent metadata file that could be edited or lost.
	// An unattested controller records nothing, which is exactly the claim it is
	// entitled to make.
	var provenance json.RawMessage
	if r.deps.ControllerBuild.Attested() {
		provenance, err = marshalPayloadJSON(r.deps.ControllerBuild)
		if err != nil {
			return "", err
		}
	}
	if _, err := r.deps.Store.AppendEvent(EngineeringEvent{
		SchemaVersion: SchemaVersion,
		ID:            runID + "-created",
		RunID:         runID,
		Type:          EventRunCreated,
		OccurredAt:    now,
		Payload:       provenance,
	}); err != nil {
		return "", err
	}
	return runID, nil
}

// candidateBranch is deterministic and run-owned. Nothing else may publish to
// it, and it is derived from the run identity rather than the issue, so two
// generations of the same issue never share a branch.
func candidateBranch(runID string) string { return "zenchron/" + runID }

func terminalDisposition(d Disposition) bool {
	return d == Completed || d == Failed || d == Cancelled
}

// ---------------------------------------------------------------------------
// Operator reads
// ---------------------------------------------------------------------------

// ControllerIdentity is who is driving the run and under what configuration.
// Build and ConfigDigest are reported side by side and never merged: an
// operator can see that the binary is unchanged while the configuration moved,
// or the reverse, instead of one digest that says only "something differs".
type ControllerIdentity struct {
	ID     string `json:"id"`
	SHA256 string `json:"sha256"`
	// Build is the provenance recorded by the controller that CREATED this run,
	// replayed from the journal. It is deliberately not this process's own
	// build: the question a run has to answer is which binary drove it.
	Build        ControllerBuild `json:"build"`
	ConfigDigest ConfigDigest    `json:"config_digest"`
	// Changed reports that the durable run was created by a different
	// controller/configuration than the one reading it now.
	Changed bool `json:"changed"`
}

// recordedControllerBuild replays the provenance the creating controller wrote
// into the genesis event. A run created before provenance existed, or by an
// unattested build, reads back as unattested - never as adopted.
func (s *runState) recordedControllerBuild() ControllerBuild {
	for _, event := range s.events {
		if event.Type != EventRunCreated {
			continue
		}
		var build ControllerBuild
		if len(event.Payload) > 0 && json.Unmarshal(event.Payload, &build) == nil {
			return build
		}
		break
	}
	return ControllerBuild{Kind: ControllerUnattested}
}

// OperationStatus is the operator's view of the current bounded operation.
type OperationStatus struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	State       OperationState `json:"state"`
	Attempt     int            `json:"attempt"`
	MaxAttempts int            `json:"max_attempts"`
	StartedAt   *time.Time     `json:"started_at,omitempty"`
	HeartbeatAt *time.Time     `json:"heartbeat_at,omitempty"`
	Elapsed     time.Duration  `json:"elapsed"`
}

// SourceIdentity is the pinned, untrusted source the run answers. The title
// and body are deliberately absent: only their digests are reported, so an
// operator read never re-emits untrusted text as if it were runtime state.
type SourceIdentity struct {
	Repository string `json:"repository"`
	Issue      int    `json:"issue"`
	URL        string `json:"url"`
	State      string `json:"state"`
	UpdatedAt  string `json:"updated_at"`
	Operator   string `json:"operator"`
	Digest     string `json:"digest"`
	// IntentChanged reports that the snapshot moved after it was pinned.
	IntentChanged bool `json:"intent_changed"`
}

// PublicationAuthority is the #7 decision that gates push and pull request
// publication. It is a reference plus a status, never a re-derived verdict.
type PublicationAuthority struct {
	Decision Ref                    `json:"decision"`
	Action   domain.Action          `json:"action"`
	Status   domain.AuthorityStatus `json:"status"`
}

// StatusReport is the complete operator answer for one run. Every field is
// JSON-serializable and derives from replayed state.
type StatusReport struct {
	SchemaVersion string             `json:"schema_version"`
	RunID         string             `json:"run_id"`
	Repository    string             `json:"repository"`
	Goal          string             `json:"goal"`
	Source        SourceIdentity     `json:"source"`
	Controller    ControllerIdentity `json:"controller"`
	Phase         Phase              `json:"phase"`
	Disposition   Disposition        `json:"disposition"`
	Reason        string             `json:"reason,omitempty"`
	Base          Ref                `json:"base"`
	Candidate     Candidate          `json:"candidate"`
	Contract      Ref                `json:"contract"`
	Operation     *OperationStatus   `json:"operation,omitempty"`

	CreatedAt time.Time     `json:"created_at"`
	Now       time.Time     `json:"now"`
	Elapsed   time.Duration `json:"elapsed"`

	Evidence             []Ref                   `json:"evidence,omitempty"`
	Assurance            *AssuranceObservation   `json:"assurance,omitempty"`
	PullRequest          *PullRequestObservation `json:"pull_request,omitempty"`
	PublicationAuthority *PublicationAuthority   `json:"publication_authority,omitempty"`
	// AuthorityRequest is the human-authority request the run currently
	// produces, or nil when it needs none. It is a projection regenerated on
	// every read, exactly like the rest of this report.
	AuthorityRequest *AuthorityRequest `json:"authority_request,omitempty"`
	Attempts         map[string]int    `json:"attempts,omitempty"`
	// ExecutionDiagnostic is the latest sanitized execution failure, projected
	// from the journal. It is the operator-facing answer to "why did execution
	// fail", and it is bounded, redacted identity and classification only: the
	// bulk provider material stays in the local-only artifact the ArtifactRef
	// names and is never rendered here.
	ExecutionDiagnostic *ExecutionDiagnostic `json:"execution_diagnostic,omitempty"`
	// ExecutionPriorContext explains which earlier attempts of the last
	// execution operation supplied observations to a retry, and what the
	// runtime bound dropped. It names attempt numbers and byte counts only;
	// the observations themselves stay in the local-only attempt artifacts.
	ExecutionPriorContext *PriorAttemptObservations `json:"execution_prior_attempt_context,omitempty"`
	Budgets               RunBudgets                `json:"budgets"`
	StateSHA256           string                    `json:"state_sha256"`
}

// Status replays the run and reports it. It performs no network call and no
// side effect, so it is safe to read a run another process is driving.
func (r *EngineeringRuntime) Status(runID string) (StatusReport, error) {
	state, err := r.load(runID)
	if err != nil {
		return StatusReport{}, err
	}
	now := r.deps.Clock.Now()
	report := StatusReport{
		SchemaVersion: SchemaVersion,
		RunID:         state.run.ID,
		Repository:    state.run.Repository,
		Goal:          state.run.Goal,
		Controller: ControllerIdentity{
			ID:           r.deps.ControllerID,
			SHA256:       state.run.ControllerSHA256,
			Build:        state.recordedControllerBuild(),
			ConfigDigest: r.deps.ConfigDigest,
			Changed:      state.controllerChanged,
		},
		Phase:                 state.phase(),
		Disposition:           state.snapshot.Disposition,
		Reason:                state.snapshot.Reason,
		Base:                  Ref{ID: r.deps.Repository.DefaultBranch, Revision: state.baseRevision()},
		Candidate:             Candidate{Branch: candidateBranch(state.run.ID), Revision: state.projection.CandidateRevision, Tree: state.projection.CandidateTree},
		Contract:              state.projection.Contract,
		CreatedAt:             state.run.CreatedAt,
		Now:                   now,
		Elapsed:               now.Sub(state.run.CreatedAt),
		Evidence:              state.projection.EvidenceBundles,
		Assurance:             state.projection.Assurance,
		PullRequest:           state.projection.PullRequest,
		Attempts:              state.projection.Attempts,
		Budgets:               r.deps.Budgets,
		StateSHA256:           state.snapshot.StateSHA256,
		PublicationAuthority:  publicationAuthorityOf(state),
		ExecutionDiagnostic:   state.projection.ExecutionDiagnostic,
		ExecutionPriorContext: state.projection.ExecutionPriorContext,
	}
	if state.source != nil {
		report.Source = SourceIdentity{
			Repository:    state.source.Repository,
			Issue:         state.source.Issue,
			URL:           state.source.URL,
			State:         state.source.State,
			UpdatedAt:     state.source.UpdatedAt,
			Operator:      state.source.Operator,
			Digest:        state.source.Digest,
			IntentChanged: state.projection.SourceIntentChanged,
		}
	}
	// A run that cannot be authorized at all - a changed controller, a moved
	// source, a terminal run - has no CURRENT request. That is an ordinary
	// answer to "what needs a human", not a status failure, so the typed
	// refusal is reported as the absence of a request.
	request, err := r.authorityRequest(state)
	if err != nil {
		var refused *AuthorityRefusedError
		if !errors.As(err, &refused) {
			return StatusReport{}, err
		}
		request = nil
	}
	report.AuthorityRequest = request
	if op, ok := state.currentOperation(); ok {
		status := OperationStatus{
			ID: op.ID, Kind: op.Kind, State: op.State,
			Attempt: op.Attempt, MaxAttempts: op.MaxAttempts,
			StartedAt: op.StartedAt, Elapsed: OperationElapsed(op, now),
		}
		if op.Lease != nil {
			heartbeat := op.Lease.HeartbeatAt
			status.HeartbeatAt = &heartbeat
		}
		report.Operation = &status
	}
	return report, nil
}

func publicationAuthorityOf(state *runState) *PublicationAuthority {
	decision, ok := state.publicationDecision()
	if !ok {
		return nil
	}
	return &PublicationAuthority{Decision: decision.Decision, Action: decision.Action, Status: decision.Status}
}

// Journal returns the run's append-only events in sequence order. It is the
// audit read: nothing is filtered, summarized, or re-ordered.
func (r *EngineeringRuntime) Journal(runID string) ([]EngineeringEvent, error) {
	return r.deps.Store.Events(runID)
}

// marshalPayloadJSON is the one place a typed Phase 8 payload becomes event
// bytes. Keeping it here means a handler never hand-rolls JSON.
func marshalPayloadJSON(payload any) (json.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}
