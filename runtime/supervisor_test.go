package runtime

// The persistent supervisor and its control endpoint.
//
// The endpoint tests are the security ones: it is an operator-authority
// boundary, so what it refuses matters more than what it accepts.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Control endpoint
// ---------------------------------------------------------------------------

// controlStateDir is a SHORT owner-only directory. t.TempDir() embeds the test
// name, which pushes a socket address past the ~100-byte limit an operating
// system allows for one - a real state directory is short, and a test that
// tripped that limit would be testing the fixture rather than the endpoint.
func controlStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestControlEndpointIsLocalAndOwnerOnly(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	info, err := os.Lstat(listener.Path())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("the control endpoint is not a socket: %v", info.Mode())
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("the control endpoint is mode %#o and is reachable by other users", perm)
	}
	if listener.Path() != filepath.Join(stateDir, ControlSocketName) {
		t.Fatalf("the endpoint is not inside the operator state directory: %s", listener.Path())
	}
	// It answers, and it answers on the local socket only. There is no address
	// family here a remote host could reach.
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse {
			return ControlResponse{OK: true}
		})
	}()
	response, err := SendControl(stateDir, ControlRequest{Command: ControlPing})
	if err != nil || !response.OK {
		t.Fatalf("the endpoint did not answer a local request: %v %#v", err, response)
	}
}

// TestControlEndpointRefusesAnExposedStateDirectory is the floor: anything
// that can reach the endpoint can start coding agents under this account, so a
// state directory other users can enter is refused BEFORE the endpoint exists.
func TestControlEndpointRefusesAnExposedStateDirectory(t *testing.T) {
	stateDir := controlStateDir(t)
	if err := os.Chmod(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := ListenControl(stateDir)
	if err == nil {
		listener.Close()
		t.Fatal("a control endpoint was published into a directory other users can reach")
	}
	if _, statErr := os.Stat(filepath.Join(stateDir, ControlSocketName)); !os.IsNotExist(statErr) {
		t.Fatal("the refused endpoint was created anyway")
	}
	var typed *ControlEndpointError
	if !errors.As(err, &typed) || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("the refusal does not say how to repair it: %v", err)
	}
}

// TestControlEndpointRefusesASecondSupervisor keeps one owner per state
// directory, and proves the stale-socket path never steals a live endpoint.
func TestControlEndpointRefusesASecondSupervisor(t *testing.T) {
	stateDir := controlStateDir(t)
	first, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	go func() {
		_ = first.Serve(func(ControlRequest) ControlResponse { return ControlResponse{OK: true} })
	}()

	second, err := ListenControl(stateDir)
	if !errors.Is(err, ErrSupervisorAlreadyRunning) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("a second supervisor took a live endpoint: %v", err)
	}
	// The live endpoint still works: the refusal did not unlink it.
	if response, err := SendControl(stateDir, ControlRequest{Command: ControlPing}); err != nil || !response.OK {
		t.Fatalf("the refused second supervisor damaged the live endpoint: %v %#v", err, response)
	}
}

// TestStaleControlSocketIsReclaimedDeterministically is crash recovery: a
// socket file nobody is listening on is removed, but only after that is proven.
func TestStaleControlSocketIsReclaimedDeterministically(t *testing.T) {
	stateDir := controlStateDir(t)
	// A supervisor that died without cleaning up leaves exactly this.
	crashed, err := net.Listen("unix", ControlSocketPath(stateDir))
	if err != nil {
		t.Fatal(err)
	}
	crashed.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := crashed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(ControlSocketPath(stateDir)); err != nil {
		t.Fatalf("the fixture did not leave a stale socket: %v", err)
	}
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatalf("a stale socket was not reclaimed: %v", err)
	}
	defer listener.Close()
}

// TestControlEndpointRefusesANonSocketAtItsPath keeps the runtime from
// deleting an operator's file on a guess.
func TestControlEndpointRefusesANonSocketAtItsPath(t *testing.T) {
	stateDir := controlStateDir(t)
	path := ControlSocketPath(stateDir)
	if err := os.WriteFile(path, []byte("operator data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if listener, err := ListenControl(stateDir); err == nil {
		listener.Close()
		t.Fatal("a regular file at the endpoint path was removed and replaced")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "operator data" {
		t.Fatalf("the operator's file was destroyed: %v %q", err, raw)
	}
}

// TestControlClientRefusesAWidenedEndpoint proves BOTH sides check. A socket
// whose permissions were widened after it was created is refused by the client
// too, rather than trusted because it was safe when it was made.
func TestControlClientRefusesAWidenedEndpoint(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse { return ControlResponse{OK: true} })
	}()
	if err := os.Chmod(listener.Path(), 0o666); err != nil {
		t.Skipf("this platform does not honour socket permissions: %v", err)
	}
	if _, err := SendControl(stateDir, ControlRequest{Command: ControlPing}); err == nil {
		t.Fatal("a client used a world-reachable control endpoint")
	}
}

// ---------------------------------------------------------------------------
// Supervisor
// ---------------------------------------------------------------------------

// supervisorRegistry is the operator registry a supervisor resolves run
// bindings against.
func supervisorRegistry(t *testing.T) AgentRegistry {
	t.Helper()
	registry, err := OperatorConfig{
		Agents: map[string]AgentConfig{
			"codex":  {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)},
			"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)},
		},
		DefaultAgent: "codex",
	}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// supervisorFixture builds a supervisor over the phase 8 fixture's store,
// driving the same repository-bound engine an operator command would.
func supervisorFixture(t *testing.T, fixture *phase8Fixture, ceiling int) *Supervisor {
	t.Helper()
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:          OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:      []GitHubRepo{repo},
		MaxConcurrentRuns: ceiling,
		PollInterval:      time.Minute,
		Agents:            supervisorRegistry(t),
		Runtime:           func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) { return fixture.runtime, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return supervisor
}

// TestSupervisorDrivesSeveralRunsUnderTheOperatorCeiling is the milestone's
// parallelism gate in its deterministic form: several durable runs, one
// process, distinct candidates, and never more at once than the operator
// authorized.
func TestSupervisorDrivesSeveralRunsUnderTheOperatorCeiling(t *testing.T) {
	fixture := newPhase8Fixture(t)
	first := fixture.start()
	fixture.issue = phase8Issue + 1
	fixture.forge.Issues[fixture.issue] = GitHubIssue{
		Number: fixture.issue, URL: "https://github.com/acme/repo/issues/42",
		Title: "second", Body: "second body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	second := fixture.start()
	if first == second {
		t.Fatal("two issues produced one run")
	}

	supervisor := supervisorFixture(t, fixture, 2)
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Driven) != 2 {
		t.Fatalf("the supervisor drove %d runs, want both: %#v", len(report.Driven), report.Driven)
	}
	if report.Capacity != 2 {
		t.Fatalf("capacity = %d, want the operator ceiling", report.Capacity)
	}
	// Distinct runs keep distinct candidates. Sharing one would be the failure
	// that makes concurrency unsafe rather than merely fast.
	firstDir := candidateDir(fixture.stateDir, first)
	secondDir := candidateDir(fixture.stateDir, second)
	if firstDir == secondDir {
		t.Fatal("two runs share one candidate workspace")
	}

	// A ceiling of one drives one, whatever is queued.
	bounded := supervisorFixture(t, fixture, 1)
	limited, err := bounded.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(limited.Driven) != 1 {
		t.Fatalf("the operator ceiling was exceeded: %d runs driven", len(limited.Driven))
	}
}

// TestSupervisorDrainStartsNothingAndCancelsNothing is the lifecycle
// distinction that matters most: draining is not cancelling.
func TestSupervisorDrainStartsNothingAndCancelsNothing(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	supervisor := supervisorFixture(t, fixture, 2)
	supervisor.Drain()

	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !report.Draining || len(report.Driven) != 0 {
		t.Fatalf("a draining supervisor started work: %#v", report)
	}
	if _, err := supervisor.Submit(context.Background(), ControlRequest{
		Repository: "acme/repo", Issue: 99,
	}); err == nil {
		t.Fatal("a draining supervisor accepted new work")
	}
	state := fixture.state(runID)
	if terminalDisposition(state.snapshot.Disposition) {
		t.Fatalf("draining made a run terminal: %q", state.snapshot.Disposition)
	}
	if countType(state.events, EventRunCancelled) != 0 {
		t.Fatalf("draining journalled a cancellation: %v", journalTypes(state.events))
	}
}

// TestSupervisorStopAllCancelsEachRunAsItself proves the third lifecycle
// action IS cancellation, journalled per run through the one cancellation path.
func TestSupervisorStopAllCancelsEachRunAsItself(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	supervisor := supervisorFixture(t, fixture, 2)

	outcomes, err := supervisor.StopAll("operator_stop_all")
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) == 0 {
		t.Fatal("stop-all cancelled nothing")
	}
	state := fixture.state(runID)
	if state.snapshot.Disposition != Cancelled {
		t.Fatalf("run disposition = %q, want cancelled", state.snapshot.Disposition)
	}
	if countType(state.events, EventRunCancelled) != 1 {
		t.Fatalf("cancellation was not journalled once per run: %v", journalTypes(state.events))
	}
	// It is idempotent, exactly as `stop RUN` is.
	if _, err := supervisor.StopAll("operator_stop_all"); err != nil {
		t.Fatal(err)
	}
	if countType(fixture.state(runID).events, EventRunCancelled) != 1 {
		t.Fatal("a repeated stop-all appended a second cancellation")
	}
}

// TestSupervisorRefusesARepositoryItDoesNotGovern keeps enrolment operator
// authority: a local control request selects among governed repositories and
// can never introduce one.
func TestSupervisorRefusesARepositoryItDoesNotGovern(t *testing.T) {
	fixture := newPhase8Fixture(t)
	supervisor := supervisorFixture(t, fixture, 2)
	if _, err := supervisor.Submit(context.Background(), ControlRequest{
		Repository: "attacker/repo", Issue: 1,
	}); err == nil {
		t.Fatal("a control request introduced a repository")
	}
}

// TestSupervisorReportsAFailingRunWithoutStoppingItsSiblings is the isolation
// property that makes several tasks in one process safe.
func TestSupervisorReportsAFailingRunWithoutStoppingItsSiblings(t *testing.T) {
	fixture := newPhase8Fixture(t)
	healthy := fixture.start()

	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Repositories:      []GitHubRepo{{Owner: "acme", Name: "repo"}},
		MaxConcurrentRuns: 2, PollInterval: time.Minute,
		Agents: supervisorRegistry(t),
		Runtime: func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) {
			return fixture.runtime, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A run whose repository the factory cannot build is reported, not thrown.
	broken := EngineeringRun{
		SchemaVersion: SchemaVersion, ID: "run-broken", Repository: "other/repo",
		Goal: issueGoal("other/repo", 7), Disposition: Active, CreatedAt: fixture.clock.Now(),
	}
	if err := fixture.store.PutRun(broken); err != nil {
		t.Fatal(err)
	}
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatalf("one unreachable run stopped the whole supervisor: %v", err)
	}
	var sawBroken, sawHealthy bool
	for _, driven := range report.Driven {
		switch driven.RunID {
		case "run-broken":
			sawBroken = driven.Error != ""
		case healthy:
			sawHealthy = true
		}
	}
	if !sawBroken || !sawHealthy {
		t.Fatalf("a failing run did not report independently of its sibling: %#v", report.Driven)
	}
}

// ---------------------------------------------------------------------------
// Shared repository observation
// ---------------------------------------------------------------------------

// countingForge counts what actually reaches the forge underneath the
// multiplexer.
type countingForge struct {
	*FakeGitHubAdapter
	mu    sync.Mutex
	calls int
}

func (f *countingForge) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.FakeGitHubAdapter.PullRequest(ctx, repo, number)
}

// TestRunsInOneRepositoryShareOneObservation is the multiplexing law: several
// runs asking the same question of one repository produce one call, not one
// each.
func TestRunsInOneRepositoryShareOneObservation(t *testing.T) {
	inner := &countingForge{FakeGitHubAdapter: NewFakeGitHubAdapter()}
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "head", State: GitHubOpen}
	clock := newSteppingClock()
	forge := NewMultiplexedForge(inner, clock)
	forge.Window = time.Hour

	repo := GitHubRepo{Owner: "acme", Name: "repo"}
	for i := 0; i < 8; i++ {
		if _, err := forge.PullRequest(context.Background(), repo, 1); err != nil {
			t.Fatal(err)
		}
	}
	if inner.calls != 1 {
		t.Fatalf("eight runs produced %d forge calls, want one shared observation", inner.calls)
	}
	if forge.Calls()["PullRequest"] != 1 {
		t.Fatalf("the multiplexer miscounted its own calls: %#v", forge.Calls())
	}

	// A write invalidates: the answers describe state the runtime just moved.
	if _, err := forge.CreatePullRequest(context.Background(), repo, GitHubPullRequestCreate{
		HeadRef: "zenchron/x", BaseRef: "main", Title: "t", Body: mustPublication(t, "body"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := forge.PullRequest(context.Background(), repo, 1); err != nil {
		t.Fatal(err)
	}
	if inner.calls != 2 {
		t.Fatalf("a write did not invalidate the shared observation: %d calls", inner.calls)
	}
}

// TestRateLimitBackoffIsSharedAcrossRuns proves one run learning that the forge
// wants to be left alone is every run learning it.
func TestRateLimitBackoffIsSharedAcrossRuns(t *testing.T) {
	inner := NewFakeGitHubAdapter()
	refusal := &GitHubTransientError{
		Status: 429, Detail: "rate limited",
		RateLimit: RateLimitObservation{RetryAfter: time.Minute},
	}
	inner.Fail = func(GitHubCall) error { return refusal }
	clock := newSteppingClock()
	forge := NewMultiplexedForge(inner, clock)
	forge.Window = time.Nanosecond // defeat coalescing, so only backoff can stop a call

	repo := GitHubRepo{Owner: "acme", Name: "repo"}
	if _, err := forge.PullRequest(context.Background(), repo, 1); err == nil {
		t.Fatal("the refusal was not surfaced")
	}
	before := len(inner.Calls)
	// A DIFFERENT question about the same repository is refused without asking.
	if _, err := forge.Checks(context.Background(), repo, "head"); err == nil {
		t.Fatal("a sibling run was allowed to ask a repository the forge had just refused")
	}
	if len(inner.Calls) != before {
		t.Fatalf("shared backoff did not stop the sibling call: %d new calls", len(inner.Calls)-before)
	}
}

// ---------------------------------------------------------------------------
// State storage
// ---------------------------------------------------------------------------

// TestStateStorageCeilingRefusesBeforeAllocating is the resource law: the
// refusal happens before a workspace is created, so the failure is a typed wait
// rather than ENOSPC in the middle of a clone.
func TestStateStorageCeilingRefusesBeforeAllocating(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "existing"), make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	// Unbounded is the documented default and admits everything.
	if err := (StateStorage{Dir: dir}).Admit(); err != nil {
		t.Fatalf("an unconfigured ceiling refused work: %v", err)
	}
	// A ceiling below what one more workspace needs refuses, and says what it
	// measured against what it was allowed.
	tight := StateStorage{Dir: dir, CeilingBytes: 8192}
	err := tight.Admit()
	var typed *StateStorageError
	if !errors.As(err, &typed) {
		t.Fatalf("the ceiling did not refuse with a typed reason: %v", err)
	}
	if typed.CeilingBytes != 8192 || typed.UsedBytes < 4096 || typed.RequiredBytes == 0 {
		t.Fatalf("the refusal does not explain itself: %#v", typed)
	}
	// A generous ceiling admits.
	if err := (StateStorage{Dir: dir, CeilingBytes: 1 << 40}).Admit(); err != nil {
		t.Fatalf("a generous ceiling refused work: %v", err)
	}
	// The class waits rather than failing, and waiting is what keeps the
	// candidate and its evidence untouched.
	if RouteFailure(FailureStateStorageExhausted) != RouteWait {
		t.Fatalf("a full disk stops a run instead of waiting: %v", RouteFailure(FailureStateStorageExhausted))
	}
}

// ---------------------------------------------------------------------------
// Base drift under parallel work
// ---------------------------------------------------------------------------

// TestOneRunsBaseDriftDoesNotTouchItsSiblings is the parallel-work law: with
// several pull requests open against one base, one of them merging is normal
// operation. The runs that did not merge observe the base moving and integrate
// it through their OWN candidate; nothing reaches across and mutates a sibling.
func TestOneRunsBaseDriftDoesNotTouchItsSiblings(t *testing.T) {
	fixture := newPhase8Fixture(t)
	first := fixture.start()
	fixture.reconcile(first)

	fixture.issue = phase8Issue + 7
	fixture.forge.Issues[fixture.issue] = GitHubIssue{
		Number: fixture.issue, URL: "https://github.com/acme/repo/issues/48",
		Title: "sibling", Body: "sibling body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	second := fixture.start()
	fixture.reconcile(second)

	before := fixture.state(second)
	beforeRevision := before.projection.CandidateRevision
	beforeEvents := len(before.events)

	// Somebody merges. The base moves under everything that is still open.
	fixture.moveBase("merged.md", "another run landed\n")

	// Driving the FIRST run is the only thing that may touch the first run.
	supervisor := supervisorFixture(t, fixture, 1)
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}

	after := fixture.state(second)
	if after.projection.CandidateRevision != beforeRevision {
		t.Fatalf("a sibling's candidate moved because another run's base advanced: %q -> %q",
			beforeRevision, after.projection.CandidateRevision)
	}
	if len(after.events) < beforeEvents {
		t.Fatal("a sibling's journal lost events")
	}
	// The two candidates are separate workspaces, which is what makes the
	// isolation structural rather than a matter of ordering.
	if candidateDir(fixture.stateDir, first) == candidateDir(fixture.stateDir, second) {
		t.Fatal("two runs share one candidate workspace")
	}
}

// TestConcurrentDrivingOfOneRunIsSerializedByTheScheduler proves two controller
// actions cannot race one candidate. The lease is the mechanism, and it already
// existed: what matters here is that the supervisor drives THROUGH it rather
// than around it.
func TestConcurrentDrivingOfOneRunIsSerializedByTheScheduler(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()
	supervisor := supervisorFixture(t, fixture, 2)

	var wait sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if _, err := supervisor.Tick(context.Background()); err != nil {
				errs <- err
			}
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent driving of one run failed: %v", err)
	}
	// The journal is still a valid chain: replay is what would break if two
	// writers had raced the same candidate.
	state := fixture.state(runID)
	if _, err := Reduce(state.run, state.events); err != nil {
		t.Fatalf("concurrent driving corrupted the journal: %v", err)
	}
}

// TestOneSupervisorDrivesEachRunWithItsOwnAgent is the milestone's headline
// scenario, and it is the one a single-agent supervisor would quietly break:
// two issues, two different workers, one process. Driving both with whichever
// agent the supervisor was started with would be a silent provider handoff on
// every tick - the exact thing a run's durable agent binding exists to prevent.
func TestOneSupervisorDrivesEachRunWithItsOwnAgent(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := supervisorRegistry(t)

	// Each run is created by an engine bound to its own agent, which is what
	// `run issue N --agent X` does.
	started := map[string]string{}
	for agentID, issue := range map[string]int{"codex": phase8Issue, "claude": phase8Issue + 3} {
		agent, err := registry.Agent(agentID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.forge.Issues[issue] = GitHubIssue{
			Number: issue, URL: "https://github.com/acme/repo/issues/1",
			Title: "work", Body: "body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
		}
		deps := fixture.deps
		deps.Agent, deps.Agents = agent, registry
		engine := fixture.newRuntime(deps)
		outcome, err := engine.StartIssueRun(context.Background(), issue, AdoptCompatibleGeneration)
		if err != nil {
			t.Fatal(err)
		}
		started[outcome.RunID] = agentID
	}
	if len(started) != 2 {
		t.Fatalf("two issues produced %d runs", len(started))
	}

	// The supervisor asks the factory for an engine per pairing. What it asks
	// for is the assertion: each run must be offered to the agent it is bound
	// to, and to no other.
	var mu sync.Mutex
	requested := map[string]string{}
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:          OwnerLivenessFunc(func(string) bool { return false }),
		Repositories:      []GitHubRepo{repo},
		MaxConcurrentRuns: 2, PollInterval: time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			mu.Lock()
			requested[agent.ID] = agent.Kind
			mu.Unlock()
			deps := fixture.deps
			deps.Agent, deps.Agents = agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if requested["codex"] != AgentKindCodexCLI || requested["claude"] != AgentKindClaudeCode {
		t.Fatalf("the supervisor did not drive each run with its own agent: %#v", requested)
	}

	// And each run's binding is unchanged: being driven is not a handoff.
	for runID, agentID := range started {
		if recorded := fixture.state(runID).recordedAgent().AgentID; recorded != agentID {
			t.Fatalf("run %s was bound to %q and is now %q", runID, agentID, recorded)
		}
	}
}

// TestSupervisorAcceptsASubmissionForAnyConfiguredAgent is the intake half of
// the same law: an operator submits per-issue agents to ONE supervisor.
func TestSupervisorAcceptsASubmissionForAnyConfiguredAgent(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := supervisorRegistry(t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: 2,
		PollInterval: time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			deps := fixture.deps
			deps.Agent, deps.Agents = agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for agentID, issue := range map[string]int{"codex": phase8Issue + 11, "claude": phase8Issue + 12} {
		fixture.forge.Issues[issue] = GitHubIssue{
			Number: issue, URL: "https://github.com/acme/repo/issues/2",
			Title: "work", Body: "body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
		}
		outcome, err := supervisor.Submit(context.Background(), ControlRequest{
			Repository: "acme/repo", Issue: issue, Agent: agentID,
		})
		if err != nil {
			t.Fatalf("submitting issue %d to agent %q was refused: %v", issue, agentID, err)
		}
		if recorded := fixture.state(outcome.RunID).recordedAgent().AgentID; recorded != agentID {
			t.Fatalf("issue %d was bound to %q, want %q", issue, recorded, agentID)
		}
	}
	// An agent the operator did not configure is still refused: a control
	// request selects among configured workers and never introduces one.
	if _, err := supervisor.Submit(context.Background(), ControlRequest{
		Repository: "acme/repo", Issue: phase8Issue + 13, Agent: "gemini",
	}); err == nil {
		t.Fatal("a submission introduced an unconfigured agent")
	}
}

// TestEveryActiveRunGetsATurnUnderACeiling is the fairness rule. A run is
// non-terminal for its whole lifetime, not only while it is inside Reconcile,
// so selecting a fixed age-ordered prefix would make a ceiling of one mean "the
// oldest run, forever" - and every later submission would wait for it to
// finish. The multi-task workflow would have been one task at a time wearing a
// fleet view.
func TestEveryActiveRunGetsATurnUnderACeiling(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := supervisorRegistry(t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	// Three durable active runs, one at a time.
	var runIDs []string
	for i := 0; i < 3; i++ {
		issue := phase8Issue + 20 + i
		fixture.forge.Issues[issue] = GitHubIssue{
			Number: issue, URL: "https://github.com/acme/repo/issues/3",
			Title: "work", Body: "body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
		}
		outcome, err := fixture.runtime.StartIssueRun(context.Background(), issue, AdoptCompatibleGeneration)
		if err != nil {
			t.Fatal(err)
		}
		runIDs = append(runIDs, outcome.RunID)
	}

	var mu sync.Mutex
	driven := map[string]int{}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: 1,
		PollInterval: time.Minute, Agents: registry,
		Runtime: func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) {
			return fixture.runtime, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for tick := 0; tick < 3; tick++ {
		report, err := supervisor.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Driven) != 1 {
			t.Fatalf("tick %d drove %d runs, want the ceiling of one", tick, len(report.Driven))
		}
		mu.Lock()
		driven[report.Driven[0].RunID]++
		mu.Unlock()
	}
	// Three ticks at a ceiling of one must have reached three distinct runs.
	if len(driven) != len(runIDs) {
		t.Fatalf("only %d of %d active runs were ever driven: %#v", len(driven), len(runIDs), driven)
	}
}

// TestSupervisorSurvivesAnUnreadableTick is the isolation rule applied to the
// tick itself. A supervisor that exited because one enumeration failed would
// take every healthy run down with it, which is the same failure the per-run
// isolation already forbids.
func TestSupervisorSurvivesAnUnreadableTick(t *testing.T) {
	fixture := newPhase8Fixture(t)
	supervisor := supervisorFixture(t, fixture, 2)
	// Close the store underneath it: the next enumeration cannot succeed.
	if err := fixture.store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatalf("one unreadable enumeration ended the supervisor: %v", err)
	}
	if report.Error == "" {
		t.Fatal("the failure was swallowed instead of reported")
	}
}

// TestFeedbackFailureIsReportedRatherThanLookingLikeSilence keeps two very
// different states apart: a pull request nobody commented on, and a forge that
// has stopped answering. They produced identical output before, so an operator
// waiting for their review to reach a worker could not tell which they were in.
func TestFeedbackFailureIsReportedRatherThanLookingLikeSilence(t *testing.T) {
	fixture, runID := feedbackFixture(t)
	fixture.deps.Agent = ResolvedAgent{ID: "codex", Kind: AgentKindCodexCLI, TrustMode: TrustOperatorTrusted}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.forge.Fail = func(call GitHubCall) error {
		if call.Method == "PullRequestComments" || call.Method == "Reviews" {
			return &GitHubTransientError{Status: 503, Detail: "unavailable"}
		}
		return nil
	}
	supervisor := supervisorFixture(t, fixture, 2)
	report, err := supervisor.Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, driven := range report.Driven {
		if driven.RunID == runID && driven.FeedbackError != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("a broken feedback path was indistinguishable from no feedback: %#v", report.Driven)
	}
}

// TestSupervisorNeedsAUsableAgentRegistry fails at construction rather than on
// the first piece of work, so an operator learns it from `serve` refusing to
// start instead of from a run that mysteriously never moves.
func TestSupervisorNeedsAUsableAgentRegistry(t *testing.T) {
	fixture := newPhase8Fixture(t)
	_, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Repositories: []GitHubRepo{{Owner: "acme", Name: "repo"}},
		Runtime: func(GitHubRepo, ResolvedAgent) (*EngineeringRuntime, error) {
			return fixture.runtime, nil
		},
	})
	if err == nil {
		t.Fatal("a supervisor was constructed with no usable agent registry")
	}
	if !strings.Contains(err.Error(), "agent registry") {
		t.Fatalf("the refusal does not name the missing registry: %v", err)
	}
}

// TestControlRequestIsBoundedWhileReading proves the ceiling is enforced by the
// reader. A local process sending a very long line with no newline must not be
// able to make the supervisor allocate it in full and only then be refused.
func TestControlRequestIsBoundedWhileReading(t *testing.T) {
	stateDir := controlStateDir(t)
	listener, err := ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		_ = listener.Serve(func(ControlRequest) ControlResponse {
			return ControlResponse{OK: true}
		})
	}()

	connection, err := net.DialTimeout("unix", listener.Path(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(30 * time.Second))
	// Over the ceiling, and deliberately never newline-terminated. The overage
	// is kept modest so the write still fits the socket buffer: the server
	// stops reading as soon as the bound is passed, and a much larger payload
	// would fail the WRITE on a broken pipe before the refusal could be read -
	// correct behaviour, but it would test the socket rather than the bound.
	oversized := append([]byte(`{"command":"ping","reason":"`), bytes.Repeat([]byte("A"), maxControlRequestBytes+1024)...)
	if _, err := connection.Write(oversized); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(connection).ReadBytes('\n')
	if err != nil {
		t.Fatalf("the endpoint did not answer an oversized request: %v", err)
	}
	var response ControlResponse
	if err := json.Unmarshal(reply, &response); err != nil {
		t.Fatal(err)
	}
	if response.OK || !strings.Contains(response.Error, "size bound") {
		t.Fatalf("an oversized control request was not refused: %#v", response)
	}
}

// TestClosingTheEndpointWhileServingIsSafe is the regression for a nil
// dereference CI caught and a local run did not: Close cleared the listener
// field while Serve was between two Accept calls, which is the shape of every
// shutdown - Serve runs for the supervisor's whole life and Close is what ends
// it. Repeating the cycle is what makes the timing window reachable.
func TestClosingTheEndpointWhileServingIsSafe(t *testing.T) {
	for i := 0; i < 25; i++ {
		stateDir := controlStateDir(t)
		listener, err := ListenControl(stateDir)
		if err != nil {
			t.Fatal(err)
		}
		served := make(chan error, 1)
		go func() {
			served <- listener.Serve(func(ControlRequest) ControlResponse {
				return ControlResponse{OK: true}
			})
		}()
		// Close concurrently with Accept, and twice: Close is idempotent.
		if err := listener.Close(); err != nil {
			t.Fatalf("closing a serving endpoint failed: %v", err)
		}
		if err := listener.Close(); err != nil {
			t.Fatalf("a repeated close failed: %v", err)
		}
		select {
		case err := <-served:
			// A closed listener is a shutdown, not a fault.
			if err != nil {
				t.Fatalf("Serve reported a closed listener as an error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Serve did not return after the endpoint was closed")
		}
	}
}
