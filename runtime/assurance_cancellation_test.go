package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// scriptedDocker answers every docker call BaselineGoVerifier makes, as a
// healthy daemon would. The blockNth call whose arguments start with blockOn
// instead runs onBlock and then holds until its own context ends, which is
// how a hung daemon or a long container looks from the runtime.
type scriptedDocker struct {
	mu       sync.Mutex
	blockOn  string
	blockNth int
	onBlock  func()
	seen     map[string]int
	blocked  bool
	// startExits scripts the nth `start` (preparation and verification
	// alternate, so verification is every even one). A positive code is the
	// workload's own exit, which Docker records and `wait` reports; a negative
	// one is a start that failed before any workload ran.
	startExits   map[int]int
	startOutputs map[int][]byte
	exited       int
}

func (d *scriptedDocker) LookPath(string) error { return nil }
func (d *scriptedDocker) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	return d.Output(ctx, name, args, dir, env, grace)
}
func (d *scriptedDocker) Output(ctx context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	joined := strings.Join(args, " ")
	d.mu.Lock()
	if d.seen == nil {
		d.seen = map[string]int{}
	}
	verb := strings.Fields(joined)[0]
	d.seen[verb]++
	code := 0
	var output []byte
	hasOutput := false
	if verb == "start" {
		code = d.startExits[d.seen[verb]]
		output, hasOutput = d.startOutputs[d.seen[verb]]
	}
	block := d.blockOn != "" && strings.HasPrefix(joined, d.blockOn) && d.seen[verb] == d.blockNth
	if block {
		d.blocked = true
	}
	d.mu.Unlock()
	if block {
		if d.onBlock != nil {
			d.onBlock()
		}
		<-ctx.Done()
		return CommandOutput{}, ctx.Err()
	}
	switch {
	case code != 0:
		if code < 0 {
			return CommandOutput{ExitCode: 1, Stderr: []byte("Error response from daemon: cannot start container\n")}, errors.New("exit status 1")
		}
		d.mu.Lock()
		d.exited = code
		d.mu.Unlock()
		return CommandOutput{ExitCode: code, Stdout: []byte("--- FAIL: TestCandidate\nFAIL\n")}, fmt.Errorf("exit status %d", code)
	case verb == "start" && hasOutput:
		return CommandOutput{Stdout: output}, nil
	case strings.HasPrefix(joined, "inspect") && d.exitedCode() != 0:
		return CommandOutput{Stdout: []byte("false\n")}, nil
	case verb == "wait" && d.exitedCode() != 0:
		return CommandOutput{Stdout: []byte(strconv.Itoa(d.exitedCode()) + "\n")}, nil
	case verb == "rm":
		d.mu.Lock()
		d.exited = 0
		d.mu.Unlock()
		return CommandOutput{}, nil
	case strings.HasPrefix(joined, "info --format {{.ServerVersion}}"):
		return CommandOutput{Stdout: []byte("27.1.1\n")}, nil
	case strings.HasPrefix(joined, "info --format {{.ID}}"):
		return CommandOutput{Stdout: []byte("daemon-test-id\n")}, nil
	case strings.HasPrefix(joined, "image inspect"):
		return CommandOutput{Stdout: []byte(args[len(args)-1] + "\n")}, nil
	case strings.HasPrefix(joined, "inspect"):
		return CommandOutput{ExitCode: 1, Stderr: []byte("Error: No such object\n")}, errors.New("no such object")
	}
	return CommandOutput{}, nil // create, start, kill, wait, rm
}

func (d *scriptedDocker) exitedCode() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.exited
}

func (d *scriptedDocker) count(verb string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.seen[verb]
}

func (d *scriptedDocker) wasBlocked() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.blocked
}

func scriptedVerifier(t *testing.T, docker *scriptedDocker, artifacts string) BaselineGoVerifier {
	t.Helper()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "mod"), []byte("cache\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return BaselineGoVerifier{
		Sandbox:            DockerSandbox{Image: "sha256:" + strings.Repeat("a", 64), Executor: docker, Grace: 50 * time.Millisecond},
		ArtifactStore:      ArtifactStore{Root: artifacts},
		DependencyCacheDir: cache,
	}
}

// The two phases a cancellation can land in: preparation's readiness probe
// (the first docker info) and the verification container itself (the second
// docker start; the first is preparation's).
var assurancePhases = map[string]struct {
	on  string
	nth int
}{
	"prepare":      {"info", 1},
	"verification": {"start", 2},
}

// Every cancellation cause becomes the class its owner stands for, in both
// phases, exactly as a provider that never started would be classified.
func TestAssureClassifiesCancellationByItsOwner(t *testing.T) {
	causes := map[string]struct {
		cause error // nil: a deadline
		want  FailureClass
	}{
		"controller shutdown": {context.Canceled, FailureControllerShutdown},
		"operator stop":       {execution.ErrRunStopped, FailureRunCancelled},
		"inactivity":          {ErrProviderInactive, FailureProviderNoProgress},
		"deadline":            {nil, FailureExecutionIncomplete},
	}
	for phaseName, phase := range assurancePhases {
		for causeName, c := range causes {
			t.Run(phaseName+"/"+causeName, func(t *testing.T) {
				dir := filepath.Join(t.TempDir(), "checkout")
				commit := initFixtureRepo(t, dir, "go.mod", "module x\n")
				tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				docker := &scriptedDocker{blockOn: phase.on, blockNth: phase.nth}
				if c.cause == nil {
					var stop context.CancelFunc
					ctx, stop = context.WithTimeout(ctx, 200*time.Millisecond)
					defer stop()
				} else {
					docker.onBlock = func() { cancel(c.cause) }
				}
				v := scriptedVerifier(t, docker, t.TempDir())
				result, _ := v.Assure(ctx, AssuranceRequest{RunID: "run-1", Attempt: 1, Commit: commit, Tree: strings.TrimSpace(tree), CheckoutDir: dir, Contract: Ref{ID: "contract", Revision: "1"}})
				if !docker.wasBlocked() {
					t.Fatalf("the %s phase was never reached", phaseName)
				}
				if result.Passed || result.FailureClass != c.want {
					t.Fatalf("%s during %s classified %q (passed=%v), want %q", causeName, phaseName, result.FailureClass, result.Passed, c.want)
				}
			})
		}
	}
}

// A verification-phase readiness timeout is infrastructure, not a verdict.
func TestAssureVerificationProbeTimeoutIsTransientInfrastructure(t *testing.T) {
	old := dockerProbeCeiling
	dockerProbeCeiling = 200 * time.Millisecond
	t.Cleanup(func() { dockerProbeCeiling = old })
	dir := filepath.Join(t.TempDir(), "checkout")
	commit := initFixtureRepo(t, dir, "go.mod", "module x\n")
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	// docker info #1 and #2 are preparation's readiness and identity; #3 is
	// the verification run's own readiness probe.
	docker := &scriptedDocker{blockOn: "info", blockNth: 3}
	v := scriptedVerifier(t, docker, t.TempDir())
	result, runErr := v.Assure(context.Background(), AssuranceRequest{RunID: "run-1", Attempt: 1, Commit: commit, Tree: strings.TrimSpace(tree), CheckoutDir: dir, Contract: Ref{ID: "contract", Revision: "1"}})
	if !docker.wasBlocked() || !errors.Is(runErr, ErrSandboxUnavailable) {
		t.Fatalf("the verification probe did not time out as unavailable: %v", runErr)
	}
	if result.Passed || result.FailureClass != FailureTransientInfrastructure {
		t.Fatalf("a verification-phase probe timeout classified %q, want %q", result.FailureClass, FailureTransientInfrastructure)
	}
}

// goModFixture is the governed-run fixture over a Go module, so the real
// BaselineGoVerifier reaches both of its docker phases.
func goModFixture(t *testing.T, docker *scriptedDocker) *phase8Fixture {
	f := newPhase8Fixture(t, func(origin string) {
		if err := os.WriteFile(filepath.Join(origin, "go.mod"), []byte("module candidate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	f.useAssurance(scriptedVerifier(t, docker, filepath.Join(f.stateDir, "artifacts")))
	return f
}

// reconcileUntilBlocked drives the run until the scripted docker call blocked.
func reconcileUntilBlocked(t *testing.T, f *phase8Fixture, ctx context.Context, runID string, docker *scriptedDocker) Outcome {
	t.Helper()
	var outcome Outcome
	for pass := 0; pass < 20 && !docker.wasBlocked(); pass++ {
		var err error
		if outcome, err = f.runtime.Reconcile(ctx, runID); err != nil {
			t.Fatal(err)
		}
	}
	if !docker.wasBlocked() {
		t.Fatal("the run never reached the blocked assurance docker call")
	}
	return outcome
}

// A controller shutdown during either assurance phase leaves the operation
// unsatisfied and the run waiting on controller_shutdown; after a restart the
// same assurance runs again and judges the candidate. FailureUnknown here
// settled the operation as satisfied and stranded the run at
// goal_state_reached.
func TestAShutdownDuringAssuranceIsResumedNotStranded(t *testing.T) {
	for phaseName, phase := range assurancePhases {
		t.Run(phaseName, func(t *testing.T) {
			ctx, shutdown := context.WithCancel(context.Background())
			defer shutdown()
			docker := &scriptedDocker{blockOn: phase.on, blockNth: phase.nth, onBlock: shutdown}
			if phaseName == "verification" {
				docker.startOutputs = map[int][]byte{4: []byte("recovered assurance output\n")}
			}
			f := goModFixture(t, docker)
			runID := f.start()
			outcome := reconcileUntilBlocked(t, f, ctx, runID, docker)
			if outcome.Disposition != Waiting || outcome.Reason != "controller_shutdown" {
				t.Fatalf("a shutdown during %s settled %+v, want a controller_shutdown wait", phaseName, outcome)
			}
			state := f.state(runID)
			key, wanted := bindAssuranceGo(state)
			if !wanted || state.satisfied(OpAssuranceGo, key) {
				t.Fatalf("a shutdown during %s satisfied assurance (wanted=%v)", phaseName, wanted)
			}
			starts := docker.count("start")
			f.runtime = f.newRuntime(f.deps) // the restarted controller
			for pass := 0; pass < 10 && !f.state(runID).satisfied(OpAssuranceGo, key); pass++ {
				f.reconcile(runID)
			}
			if docker.count("start") <= starts || !f.state(runID).satisfied(OpAssuranceGo, key) {
				t.Fatalf("assurance did not run again after the restart (starts %d -> %d)", starts, docker.count("start"))
			}
			if phaseName == "verification" {
				state := f.state(runID)
				for attempt, want := range map[int]string{1: "", 2: "recovered assurance output\n"} {
					ref, err := attemptTranscriptPrefix(baselineGoProviderID, AssuranceRequest{
						RunID: runID, Commit: state.projection.CandidateRevision, Attempt: attempt,
					}.AttemptRef())
					if err != nil {
						t.Fatal(err)
					}
					body, err := os.ReadFile(filepath.Join(f.stateDir, "artifacts", ref+".raw.log"))
					if err != nil || string(body) != want {
						t.Fatalf("assurance attempt %d transcript = %q, %v; want %q", attempt, body, err, want)
					}
				}
			}
		})
	}
}

// An operator stop during assurance is a stop, not a wait and not a verdict.
func TestAnOperatorStopDuringAssuranceCancelsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	docker := &scriptedDocker{blockOn: "start", blockNth: 2}
	f := goModFixture(t, docker)
	runID := f.start()
	docker.onBlock = func() {
		operatorStop(t, f, &runID)()
		cancel(execution.ErrRunStopped)
	}
	// The stop finishes the leased assurance operation underneath the pass
	// that is running it, so that pass may report the operation inactive.
	for pass := 0; pass < 20 && !docker.wasBlocked(); pass++ {
		_, _ = f.runtime.Reconcile(ctx, runID)
	}
	if !docker.wasBlocked() {
		t.Fatal("the run never reached the verification container")
	}
	outcome := f.reconcile(runID)
	if outcome.Disposition != Cancelled {
		t.Fatalf("an operator stop during assurance settled %+v, want cancelled", outcome)
	}
}

// cancelledConfirmation fails its first verification, then is shut down
// during the confirmation pass.
type cancelledConfirmation struct {
	FakeAssuranceProvider
	shutdown func()
}

func (p *cancelledConfirmation) Assure(ctx context.Context, r AssuranceRequest) (AssuranceResult, error) {
	if !r.Confirmation {
		return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureVerification}, nil
	}
	p.shutdown()
	return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1"}, ctx.Err()
}

// A shutdown during the confirmation pass routes as a shutdown, not as an
// unjudged FailureUnknown the reconciler would settle as satisfied.
func TestACancelledConfirmationPassRoutesByItsOwner(t *testing.T) {
	ctx, shutdown := context.WithCancel(context.Background())
	defer shutdown()
	_, class, err := AssuranceRerun(ctx, &cancelledConfirmation{shutdown: shutdown}, AssuranceRequest{})
	if err == nil || class != FailureControllerShutdown {
		t.Fatalf("a shutdown confirmation pass classified %q (%v), want %q", class, err, FailureControllerShutdown)
	}
}

// A verification-phase probe timeout retries assurance as infrastructure and
// spends no producer remediation.
func TestAVerificationProbeTimeoutRetriesWithoutRemediation(t *testing.T) {
	old := dockerProbeCeiling
	dockerProbeCeiling = 200 * time.Millisecond
	t.Cleanup(func() { dockerProbeCeiling = old })
	docker := &scriptedDocker{blockOn: "info", blockNth: 3}
	f := goModFixture(t, docker)
	runID := f.start()
	reconcileUntilBlocked(t, f, context.Background(), runID, docker)
	executions := len(f.provider.requests)
	state := f.state(runID)
	key, _ := bindAssuranceGo(state)
	for pass := 0; pass < 10 && !f.state(runID).satisfied(OpAssuranceGo, key); pass++ {
		f.reconcile(runID)
	}
	observed := automatedAssurancePayloads(t, f.state(runID).events)
	if len(observed) < 2 || observed[0].Passed || observed[0].FailureClass != FailureTransientInfrastructure {
		t.Fatalf("a verification-phase probe timeout must be observed as %q before a retry, got %+v", FailureTransientInfrastructure, observed)
	}
	if !f.state(runID).satisfied(OpAssuranceGo, key) || !observed[len(observed)-1].Passed {
		t.Fatal("assurance was not retried to a verdict after an infrastructure timeout")
	}
	if len(f.provider.requests) != executions {
		t.Fatalf("an infrastructure timeout spent producer remediation: %d -> %d executions", executions, len(f.provider.requests))
	}
}

// erroringConfirmation fails its first verification, then its confirmation
// pass errors with a class of its own and no cancellation.
type erroringConfirmation struct{ FakeAssuranceProvider }

func (p *erroringConfirmation) Assure(_ context.Context, r AssuranceRequest) (AssuranceResult, error) {
	if !r.Confirmation {
		return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureVerification}, nil
	}
	return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureTransientInfrastructure}, ErrSandboxUnavailable
}

func TestAnErroringConfirmationPassKeepsTheVerifiersClass(t *testing.T) {
	_, class, err := AssuranceRerun(context.Background(), &erroringConfirmation{}, AssuranceRequest{})
	if err == nil || class != FailureTransientInfrastructure {
		t.Fatalf("an erroring confirmation pass classified %q (%v), want the verifier's %q", class, err, FailureTransientInfrastructure)
	}
}

// An unpassed result that names no class is a verification failure, as
// currentHeadFailure reads it: it remediates exactly as verification_failure
// does and is never reinterpreted as a non-retryable stop.
func TestAnUnclassifiedFailedAssuranceRemediatesLikeVerification(t *testing.T) {
	remediate := func(class FailureClass) (executions int, reasons []string) {
		f := newPhase8Fixture(t)
		f.distinctMutations()
		results := []AssuranceResult{
			{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", FailureClass: class},
			{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", FailureClass: class},
		}
		results = append(results, passingAssurance().Results...)
		f.useAssurance(&FakeAssuranceProvider{Results: results})
		runID := f.start()
		for pass := 0; pass < 30; pass++ {
			reasons = append(reasons, f.reconcile(runID).Reason)
		}
		return len(f.provider.requests), reasons
	}
	want, wantReasons := remediate(FailureVerification)
	got, reasons := remediate("")
	if want < 2 || got != want {
		t.Fatalf("an unclassified failure ran %d executions, verification_failure ran %d (want remediation, and the same)", got, want)
	}
	for _, reason := range append(reasons, wantReasons...) {
		if strings.Contains(reason, "not_retryable") {
			t.Fatalf("a failed assurance was settled as a non-retryable stop: %v", reasons)
		}
	}
}

// A stop-routed class with no verdict and no journalled stop must not satisfy
// assurance: nothing would ever plan it again, and the run would strand at
// goal_state_reached.
func TestAStopRoutedAssuranceClassDoesNotSatisfyTheOperation(t *testing.T) {
	f := newPhase8Fixture(t)
	results := make([]AssuranceResult, 8)
	for i := range results {
		results[i] = AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", FailureClass: FailureRunCancelled}
	}
	verifier := &FakeAssuranceProvider{Results: results}
	f.useAssurance(verifier)
	runID := f.start()
	var outcome Outcome
	for pass := 0; pass < 20 && len(verifier.Requests) == 0; pass++ {
		outcome = f.reconcile(runID)
	}
	if len(verifier.Requests) == 0 {
		t.Fatal("assurance never ran")
	}
	for pass := 0; pass < 3; pass++ {
		outcome = f.reconcile(runID)
	}
	state := f.state(runID)
	key, _ := bindAssuranceGo(state)
	if state.satisfied(OpAssuranceGo, key) || outcome.Reason == "goal_state_reached" {
		t.Fatalf("a run_cancelled assurance with no stop satisfied the operation: %+v", outcome)
	}
}

// #454: a first run that fails and a confirmation that passes is flaky, and a
// flaky result is never passing evidence. No bundle is bound, the operation is
// not satisfied, nothing reaches authority, and the run stops non-retryably.
func TestAFlakyAssurancePassIsNeverEvidence(t *testing.T) {
	f := newPhase8Fixture(t)
	results := append([]AssuranceResult{{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", FailureClass: FailureVerification}}, passingAssurance().Results...)
	verifier := &FakeAssuranceProvider{Results: results}
	f.useAssurance(verifier)
	runID := f.start()
	var outcome Outcome
	for pass := 0; pass < 30 && outcome.Disposition != Failed; pass++ {
		outcome = f.reconcile(runID)
	}
	if len(verifier.Requests) < 2 || !verifier.Requests[1].Confirmation {
		t.Fatalf("the failing first run was not confirmed: %+v", verifier.Requests)
	}
	state := f.state(runID)
	key, _ := bindAssuranceGo(state)
	if state.satisfied(OpAssuranceGo, key) {
		t.Fatal("a flaky assurance satisfied the operation")
	}
	if len(state.projection.EvidenceBundles) != 0 {
		t.Fatalf("a flaky pass bound evidence: %+v", state.projection.EvidenceBundles)
	}
	flaky := false
	for _, p := range automatedAssurancePayloads(t, state.events) {
		if p.Passed || p.Bundle != (Ref{}) {
			t.Fatalf("a flaky result was observed as passing evidence: %+v", p)
		}
		flaky = flaky || p.FailureClass == FailureFlaky
	}
	if !flaky {
		t.Fatalf("no %s observation was journalled", FailureFlaky)
	}
	for _, e := range state.events {
		if e.Type == EventAuthorityEvaluated {
			t.Fatal("a flaky result reached authority")
		}
	}
	if outcome.Disposition != Failed || outcome.Reason != OpAssuranceGo+"_failure_not_retryable" {
		t.Fatalf("a flaky assurance settled %+v, want %s_failure_not_retryable", outcome, OpAssuranceGo)
	}
}

// passWithError answers the first call with failing, then claims a pass on
// the confirmation together with an error.
type passWithError struct{ FakeAssuranceProvider }

func (p *passWithError) Assure(_ context.Context, r AssuranceRequest) (AssuranceResult, error) {
	if !r.Confirmation {
		return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", FailureClass: FailureVerification}, nil
	}
	return AssuranceResult{ProviderID: "v", VerifierDefinition: "v1", Passed: true}, ErrSandboxUnavailable
}

// A provider that claims a pass together with an error has not passed, on
// either call, and binds no evidence.
func TestAPassReturnedWithAnErrorIsNeverEvidence(t *testing.T) {
	first := passingAssurance()
	first.Err = ErrSandboxUnavailable
	for name, provider := range map[string]AssuranceProvider{"first": first, "confirmation": &passWithError{}} {
		if result, _, err := AssuranceRerun(context.Background(), provider, AssuranceRequest{}); err == nil || result.Passed {
			t.Fatalf("%s: a result with an error was passed (%v)", name, err)
		}
	}
	f := newPhase8Fixture(t)
	verifier := passingAssurance()
	verifier.Err = ErrSandboxUnavailable
	f.useAssurance(verifier)
	runID := f.start()
	for pass := 0; pass < 30; pass++ {
		f.reconcile(runID)
	}
	if len(verifier.Requests) == 0 {
		t.Fatal("assurance never ran")
	}
	state := f.state(runID)
	if len(state.projection.EvidenceBundles) != 0 {
		t.Fatalf("a pass returned with an error bound evidence: %+v", state.projection.EvidenceBundles)
	}
	for _, e := range state.events {
		if e.Type == EventAuthorityEvaluated {
			t.Fatal("a pass returned with an error reached authority")
		}
	}
}
