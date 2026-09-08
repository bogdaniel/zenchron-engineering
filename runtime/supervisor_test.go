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
	"fmt"
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
func supervisorFixture(t *testing.T, fixture *phase8Fixture, ceiling int, discovery ...*WatchController) *Supervisor {
	t.Helper()
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	var intake *WatchController
	if len(discovery) == 1 {
		intake = discovery[0]
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Discovery: intake,
		Store:     fixture.store, Clock: fixture.clock, Owner: "owner-1",
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
		// One request is answered FIRST, so Serve has completed an Accept and
		// is blocked in the next one. That is the window the nil dereference
		// lived in; closing while the very first Accept is still blocked
		// exercises a different and easier path.
		response, err := SendControl(stateDir, ControlRequest{Command: ControlPing})
		if err != nil || !response.OK {
			t.Fatalf("the endpoint did not answer before the shutdown: %v %#v", err, response)
		}
		// Close concurrently with the second Accept, and twice: Close is
		// idempotent.
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

		// The DETERMINISTIC half, and the one that actually reproduces the
		// defect. The race above depends on Close landing inside a window a
		// few instructions wide, which is why CI hit it and a local run did
		// not - and why it cannot be relied on as a regression.
		//
		// The invariant underneath it does not need a race: Serve reads the
		// listener field on every iteration, so Close must not mutate it.
		// Calling Serve after Close reads exactly that field, and a Close that
		// cleared it panics here every time.
		if err := listener.Serve(func(ControlRequest) ControlResponse {
			return ControlResponse{OK: true}
		}); err != nil {
			t.Fatalf("serving a closed endpoint reported a fault rather than a shutdown: %v", err)
		}
	}
}

// TestAPreWriteAnswerIsNotCached is one half of the invalidation rule. A read
// that began before a write completes after it, holding state the write already
// moved past; that answer belongs to its own caller and must not become shared
// state.
//
// It deliberately has ONE job. An earlier version also exercised joining, and
// the two readers then raced to populate the cache - so whichever finished last
// decided the assertion, and the test passed with the guard removed.
func TestAPreWriteAnswerIsNotCached(t *testing.T) {
	inner := newBlockingForge()
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "before", State: GitHubOpen}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	first := make(chan GitHubPullRequest, 1)
	go func() {
		pr, _ := forge.PullRequest(context.Background(), repo, 1)
		first <- pr
	}()
	inner.waitForCall(t)

	// A write lands while that read is still in flight.
	if _, err := forge.CreatePullRequest(context.Background(), repo, GitHubPullRequestCreate{
		HeadRef: "zenchron/x", BaseRef: "main", Title: "t", Body: mustPublication(t, "body"),
	}); err != nil {
		t.Fatal(err)
	}
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "after", State: GitHubOpen}
	close(inner.release)

	// The pre-write read really did observe pre-write state; without that this
	// test would be asserting nothing.
	if stale := <-first; stale.HeadSHA != "before" {
		t.Fatalf("the fixture never produced a pre-write answer: head %q", stale.HeadSHA)
	}
	// A reader arriving now must see the CURRENT state. With the pre-write
	// answer cached it would see the state the write already moved past.
	pr, err := forge.PullRequest(context.Background(), repo, 1)
	if err != nil {
		t.Fatal(err)
	}
	if pr.HeadSHA != "after" {
		t.Fatalf("a pre-write answer was cached across the invalidation: head %q", pr.HeadSHA)
	}
}

// TestAPreWriteRequestIsNotJoined is the other half. A reader arriving while a
// pre-write read is STILL in flight must start its own request rather than
// waiting on one whose answer the write has already invalidated.
//
// The ordering is the assertion: the joiner has to arrive before anything is
// released. Releasing first retires the in-flight entry and leaves nothing to
// join, which is how an earlier version of this test proved nothing.
func TestAPreWriteRequestIsNotJoined(t *testing.T) {
	inner := newBlockingForge()
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "before", State: GitHubOpen}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	first := make(chan GitHubPullRequest, 1)
	go func() {
		pr, _ := forge.PullRequest(context.Background(), repo, 1)
		first <- pr
	}()
	inner.waitForCall(t)

	if _, err := forge.CreatePullRequest(context.Background(), repo, GitHubPullRequestCreate{
		HeadRef: "zenchron/x", BaseRef: "main", Title: "t", Body: mustPublication(t, "body"),
	}); err != nil {
		t.Fatal(err)
	}
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "after", State: GitHubOpen}

	joiner := make(chan GitHubPullRequest, 1)
	go func() {
		pr, _ := forge.PullRequest(context.Background(), repo, 1)
		joiner <- pr
	}()
	// The joiner must have reached the forge with its OWN request before
	// anything is released. Sleeping here would let a slow joiner arrive after
	// the first request had already retired, where there is nothing left to
	// join and the join decision is never exercised - the test would pass
	// without testing anything.
	inner.waitForSecondCall(t)
	close(inner.release)

	if stale := <-first; stale.HeadSHA != "before" {
		t.Fatalf("the fixture never produced a pre-write answer: head %q", stale.HeadSHA)
	}
	if joined := <-joiner; joined.HeadSHA != "after" {
		t.Fatalf("a reader joined a request that began before the write: head %q", joined.HeadSHA)
	}
}

// TestJoiningASharedRequestHonoursCancellation keeps one slow forge call from
// holding up an unrelated caller - and, through it, a whole supervisor tick -
// after that caller's context is done.
func TestJoiningASharedRequestHonoursCancellation(t *testing.T) {
	inner := newBlockingForge()
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "head", State: GitHubOpen}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	owner := make(chan struct{})
	go func() {
		defer close(owner)
		_, _ = forge.PullRequest(context.Background(), repo, 1)
	}()
	inner.waitForCall(t)

	// A second caller joins the in-flight request, then gives up.
	ctx, cancel := context.WithCancel(context.Background())
	joined := make(chan error, 1)
	go func() { _, err := forge.PullRequest(ctx, repo, 1); joined <- err }()
	cancel()

	select {
	case err := <-joined:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("a cancelled joiner returned %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a cancelled joiner stayed blocked on somebody else's request")
	}
	close(inner.release)
	<-owner
}

// blockingForge holds one read open until released, which is the only way to
// observe what happens to callers that arrive while a request is in flight.
type blockingForge struct {
	*FakeGitHubAdapter
	release chan struct{}
	called  chan struct{}
	// second closes when a SECOND request reaches the forge. A test that needs
	// a joiner to have made its own request cannot prove that by sleeping: on a
	// loaded machine the joiner may still be on its way, the first request
	// retires, and the joiner then reads current state through a path that
	// never exercised the join decision at all. That is a false green rather
	// than a false red, which is the harder kind to notice.
	second chan struct{}
	mu     sync.Mutex
	calls  int
}

func newBlockingForge() *blockingForge {
	return &blockingForge{
		FakeGitHubAdapter: NewFakeGitHubAdapter(),
		release:           make(chan struct{}),
		called:            make(chan struct{}),
		second:            make(chan struct{}),
	}
}

func (f *blockingForge) waitForCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.called:
	case <-time.After(10 * time.Second):
		t.Fatal("the forge was never called")
	}
}

// waitForSecondCall blocks until a second request has reached the forge. A
// reader that JOINED an in-flight request never issues one, so this timing out
// is itself the failure the caller is looking for.
func (f *blockingForge) waitForSecondCall(t *testing.T) {
	t.Helper()
	select {
	case <-f.second:
	case <-time.After(10 * time.Second):
		t.Fatal("no second request ever reached the forge: the reader joined the in-flight one instead of starting its own")
	}
}

func (f *blockingForge) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	// The state is observed FIRST and the response is slow afterwards, which
	// is what a pre-write read actually is. Blocking before observing would
	// make the "slow" read return post-write state, so no stale answer would
	// exist and a test built on it would prove nothing - which is exactly what
	// the first version of this fake did.
	pr, err := f.FakeGitHubAdapter.PullRequest(ctx, repo, number)
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	switch n {
	case 1:
		close(f.called)
	case 2:
		close(f.second)
	}
	<-f.release
	return pr, err
}

// TestCancellationEventIdCarriesNoOperatorText is the one fix on this branch
// that had no regression at all, which I would rather close than leave noted.
//
// An event id is a primary key read back forever, and `stop-all --reason
// <text>` and the control endpoint both carry arbitrary operator text. The
// reason belongs in the payload, where the schema bounds it.
func TestCancellationEventIdCarriesNoOperatorText(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start()

	// A reason with a newline, a path separator and enough length to be a
	// problem if it ever reached an identity.
	reason := "operator/stop\nwith text " + strings.Repeat("x", 300)
	scheduler := Scheduler{Store: fixture.store, Clock: fixture.clock, Owner: "owner-1"}
	if _, err := CancelRun(fixture.store, scheduler, fixture.clock.Now(), runID, reason); err != nil {
		t.Fatal(err)
	}
	events, err := fixture.store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, event := range events {
		if event.Type != EventRunCancelled {
			continue
		}
		found = true
		if strings.Contains(event.ID, "xxx") || strings.Contains(event.ID, "\n") || strings.Contains(event.ID, "/") {
			t.Fatalf("operator text reached the durable event id: %q", event.ID)
		}
		// The reason is still recorded - it is simply recorded where a bound
		// applies to it.
		if !strings.Contains(string(event.Payload), "operator/stop") {
			t.Fatalf("the operator's stated reason was lost from the payload: %s", event.Payload)
		}
	}
	if !found {
		t.Fatalf("no cancellation was journalled: %v", journalTypes(events))
	}
}

// TestDiscoveryUnderASupervisorIsIntakeAndNeverADriver is the product law that
// `serve` owns scheduling.
//
// Automatic discovery is one optional INTAKE policy. Standalone `autonomy
// watch` also drives, because nothing else is running - but under a supervisor
// a discovery controller that drove would give one process two independently
// capacity-bounded advancement paths over the same durable runs. A single
// supervisor tick would hand a freshly claimed run to Reconcile once through
// discovery and again through the supervisor's own enumeration of non-terminal
// runs, which is the operator ceiling being enforced twice over rather than
// once.
func TestDiscoveryUnderASupervisorIsIntakeAndNeverADriver(t *testing.T) {
	fixture := newWatchFixture(t)
	optIn(fixture.forge, phase8Issue, fixture.clock.Now())

	intake, err := NewWatchController(WatchDependencies{
		Store:    fixture.store,
		Clock:    fixture.clock,
		Owner:    fixture.deps.Owner,
		Liveness: OwnerLivenessFunc(func(string) bool { return true }),
		GitHub:   fixture.forge,
		Settings: WatchSettings{
			Repositories: []GitHubRepo{repoA}, Label: DefaultWatchLabel,
			PollInterval: watchPollInterval, MaxConcurrentRuns: 1,
		},
		Runtime: func(GitHubRepo) (*EngineeringRuntime, error) { return fixture.runtime, nil },
		// What `serve` supplies.
		IntakeOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	report, err := supervisorFixture(t, fixture, 1, intake).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Discovery == nil || len(report.Discovery.Repositories) != 1 {
		t.Fatalf("discovery did not report its repository: %#v", report.Discovery)
	}
	discovered := report.Discovery.Repositories[0]

	// Intake still happened. Without this the test would pass on a controller
	// that did nothing at all, which is not the property being defended.
	if discovered.Discovered == 0 {
		t.Fatal("discovery claimed nothing, so this tick proves nothing about who drives")
	}
	runID := runIDFor(t, fixture.runtime, phase8Issue)
	if _, ok := storedRun(t, fixture, runID); !ok {
		t.Fatal("discovery did not create the durable run it is responsible for creating")
	}

	// ...and it drove nothing.
	if len(discovered.Driven) != 0 {
		t.Fatalf("discovery drove runs under a supervisor: %v", discovered.Driven)
	}
	// The supervisor did, exactly once.
	if len(report.Driven) != 1 || report.Driven[0].RunID != runID {
		t.Fatalf("the supervisor did not drive exactly the run discovery claimed: %#v", report.Driven)
	}
}

// TestASupersededRateLimitStillTeachesTheRepositoryToWait separates two facts
// the multiplexer had been treating as one.
//
// An ANSWER describes repository state, so an invalidating write can move past
// it and it must not be cached. A rate-limit instruction describes the FORGE,
// and nothing this process writes changes when the forge is willing to be asked
// again. Discarding the instruction because a sibling happened to write in the
// meantime sends every other run in the repository straight back into the same
// limit - the shared backoff exists precisely so the limit is discovered once.
func TestASupersededRateLimitStillTeachesTheRepositoryToWait(t *testing.T) {
	inner := newBlockingForge()
	refusal := &GitHubTransientError{
		Status: 429, Detail: "rate limited",
		RateLimit: RateLimitObservation{RetryAfter: time.Minute},
	}
	inner.Fail = func(call GitHubCall) error {
		// Only the slow READ is refused. The write has to succeed, because the
		// write is what supersedes the read's epoch.
		if call.Method == "PullRequest" {
			return refusal
		}
		return nil
	}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Nanosecond // defeat coalescing, so only backoff can stop a call
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	refused := make(chan error, 1)
	go func() {
		_, err := forge.PullRequest(context.Background(), repo, 1)
		refused <- err
	}()
	inner.waitForCall(t)

	// A write lands while the rate-limited read is still in flight, so the read
	// completes into an epoch that is no longer current.
	if _, err := forge.CreatePullRequest(context.Background(), repo, GitHubPullRequestCreate{
		HeadRef: "zenchron/x", BaseRef: "main", Title: "t", Body: mustPublication(t, "body"),
	}); err != nil {
		t.Fatal(err)
	}
	close(inner.release)
	if err := <-refused; err == nil {
		t.Fatal("the fixture never produced a rate-limit refusal, so this test proves nothing")
	}

	// A sibling run now asks a DIFFERENT question about the same repository. It
	// must be refused from the shared backoff without reaching the forge.
	before := len(inner.Calls)
	if _, err := forge.Checks(context.Background(), repo, "head"); err == nil {
		t.Fatal("a sibling was allowed to ask a repository the forge had just rate-limited")
	}
	if len(inner.Calls) != before {
		t.Fatalf("a superseded rate limit taught nobody: %d new calls reached the forge", len(inner.Calls)-before)
	}
}

// TestOneRunsCancellationIsNotSharedWithItsSiblings separates a third pair of
// facts the multiplexer had been treating as one.
//
// Errors are worth sharing when they describe the repository: a 404 or a 403 is
// the same answer for every run, and re-asking would only spend rate limit to
// be told the same thing. A caller's own cancellation or deadline describes the
// CALLER. Caching it would mean stopping one run answers its siblings' live
// questions with that run's cancellation until the window expires - `stop RUN`
// silently degrading every other run in the same repository, which is the exact
// opposite of what sharing an observation stream is for.
func TestOneRunsCancellationIsNotSharedWithItsSiblings(t *testing.T) {
	inner := NewFakeGitHubAdapter()
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "head", State: GitHubOpen}
	cancelled := true
	inner.Fail = func(GitHubCall) error {
		if cancelled {
			cancelled = false
			return context.Canceled
		}
		return nil
	}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour // the whole point is that the answer would persist
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	stopped, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := forge.PullRequest(stopped, repo, 1); err == nil {
		t.Fatal("the fixture never produced a cancellation, so this test proves nothing")
	}

	// A sibling with a live context asks the same question.
	pr, err := forge.PullRequest(context.Background(), repo, 1)
	if err != nil {
		t.Fatalf("a sibling inherited another run's cancellation: %v", err)
	}
	if pr.HeadSHA != "head" {
		t.Fatalf("the sibling did not get a real answer: head %q", pr.HeadSHA)
	}
}

// TestARepositoryErrorIsStillSharedOnce is the other side of that line. The fix
// above must not turn into "never cache errors": a refusal that describes the
// repository is exactly what the siblings should be spared from rediscovering,
// and re-asking would spend rate limit to be told the same thing N times.
func TestARepositoryErrorIsStillSharedOnce(t *testing.T) {
	inner := NewFakeGitHubAdapter()
	inner.Fail = func(GitHubCall) error {
		return &GitHubAPIError{Status: 404, Detail: "not found"}
	}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	if _, err := forge.PullRequest(context.Background(), repo, 1); err == nil {
		t.Fatal("the refusal was not surfaced")
	}
	before := len(inner.Calls)
	if _, err := forge.PullRequest(context.Background(), repo, 1); err == nil {
		t.Fatal("the shared refusal was not surfaced to the sibling")
	}
	if len(inner.Calls) != before {
		t.Fatalf("a repository refusal was rediscovered rather than shared: %d new calls", len(inner.Calls)-before)
	}
}

// operationFingerprints is the durable side-effect ledger a restart must not
// duplicate. Operation identity is what makes an already-satisfied step
// idempotent, so counting each id's terminal state is how "the second
// supervisor redid something" becomes visible rather than inferred.
func operationFingerprints(t *testing.T, fixture *phase8Fixture) map[string]string {
	t.Helper()
	operations, err := fixture.store.AllOperations()
	if err != nil {
		t.Fatal(err)
	}
	fingerprints := map[string]string{}
	for _, op := range operations {
		fingerprints[op.ID] = fmt.Sprintf("%s|%s|attempt=%d", op.Kind, op.State, op.Attempt)
	}
	return fingerprints
}

// agentBoundSupervisor builds a supervisor whose runtime carries the resolved
// agent, so a submission produces a run with a journalled agent binding rather
// than the fixture's unbound legacy run.
func agentBoundSupervisor(t *testing.T, fixture *phase8Fixture, ceiling int) *Supervisor {
	t.Helper()
	registry := supervisorRegistry(t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:     OwnerLivenessFunc(func(string) bool { return false }),
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: ceiling,
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
	return supervisor
}

// TestASupervisorRestartResumesWorkWithoutDuplicatingIt is the normative
// `serve` restart requirement: a restart reconstructs and resumes eligible
// durable work without creating duplicate logical runs or repeating side
// effects.
//
// The architecture makes this plausible - the supervisor owns no durable queue
// of its own and re-enumerates non-terminal runs from the store every tick - but
// plausible is not proved, and "recovery is replay" is exactly the kind of claim
// that stays true right up until something starts being remembered in memory.
// This pins it as behaviour instead of as a design intention.
func TestASupervisorRestartResumesWorkWithoutDuplicatingIt(t *testing.T) {
	fixture := newPhase8Fixture(t)
	issue := phase8Issue + 21
	fixture.forge.Issues[issue] = GitHubIssue{
		Number: issue, URL: "https://github.com/acme/repo/issues/21",
		Title: "restart", Body: "body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	submitted, err := agentBoundSupervisor(t, fixture, 1).Submit(context.Background(), ControlRequest{
		Repository: "acme/repo", Issue: issue, Agent: "codex",
	})
	if err != nil {
		t.Fatal(err)
	}
	runID := submitted.RunID

	// The journalled binding is the authority. Reading it here also stops the
	// comparison below from being satisfied by two empty strings, which is what
	// it silently was when this test was first written against an unbound run.
	boundAgent := fixture.state(runID).recordedAgent().AgentID
	if boundAgent == "" {
		t.Fatal("the run carries no agent binding, so the restart comparison would prove nothing")
	}

	// Supervisor A advances the run to whatever durable boundary it reaches.
	first, err := agentBoundSupervisor(t, fixture, 1).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Driven) != 1 || first.Driven[0].RunID != runID {
		t.Fatalf("the first supervisor did not drive the run: %#v", first.Driven)
	}
	before, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	beforeOps := operationFingerprints(t, fixture)
	if len(beforeOps) == 0 {
		t.Fatal("the first supervisor performed no durable operation, so there is no side effect to duplicate")
	}
	run, ok := storedRun(t, fixture, runID)
	if !ok {
		t.Fatal("the run was not durable after the first supervisor drove it")
	}
	if terminalDisposition(run.Disposition) {
		t.Fatalf("the run finished in one tick, so a restart would have nothing to resume: %s", run.Disposition)
	}

	// Supervisor A is gone. Nothing is cancelled and nothing is handed over:
	// B is constructed from the same durable store, exactly as a restarted
	// process would be, and is told nothing about what A had been doing.
	second, err := agentBoundSupervisor(t, fixture, 1).Tick(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// The same logical run resumes.
	if len(second.Driven) != 1 || second.Driven[0].RunID != runID {
		t.Fatalf("the restarted supervisor did not resume the same run: %#v", second.Driven)
	}
	after, err := fixture.store.Runs()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("the restart created a duplicate logical run: %d runs before, %d after", len(before), len(after))
	}
	resumed, ok := storedRun(t, fixture, runID)
	if !ok {
		t.Fatal("the run disappeared across the restart")
	}
	// The binding survives the process, not merely the tick. A restart that
	// re-derived the agent from today's default would silently move a run to a
	// different worker, which is the failure this is really guarding.
	if resumedAgent := fixture.state(runID).recordedAgent().AgentID; resumedAgent != boundAgent {
		t.Fatalf("the agent binding changed across the restart: %q then %q", boundAgent, resumedAgent)
	}
	if resumed.Base.Revision != run.Base.Revision {
		t.Fatalf("the base moved across the restart: %q then %q", run.Base.Revision, resumed.Base.Revision)
	}

	// No already-succeeded step was performed again. A restart is allowed to
	// ADD operations - that is what resuming means - but an operation that had
	// already SUCCEEDED must not be reopened or retried.
	afterOps := operationFingerprints(t, fixture)
	for id, fingerprint := range beforeOps {
		now, ok := afterOps[id]
		if !ok {
			t.Fatalf("operation %s vanished across the restart", id)
		}
		if strings.Contains(fingerprint, string(Succeeded)) && now != fingerprint {
			t.Fatalf("a succeeded operation was redone across the restart: %s was %q, now %q", id, fingerprint, now)
		}
	}
}

// TestAJoinerDoesNotAdoptTheOwnersCancellation closes the other half of the
// caller-scoped leak. Refusing to CACHE an owner's cancellation is not enough
// while a sibling that merely arrived during the same request still receives it
// directly: same leak, other path.
//
// The sibling here is live throughout. Only the run that happened to own the
// in-flight request is stopped, which is exactly the shape of `stop RUN` while
// other runs in the repository are mid-tick.
func TestAJoinerDoesNotAdoptTheOwnersCancellation(t *testing.T) {
	inner := newBlockingForge()
	inner.PullRequests[1] = GitHubPullRequest{Number: 1, HeadSHA: "head", State: GitHubOpen}
	stoppedRun := true
	inner.Fail = func(GitHubCall) error {
		// The owner's request fails the way a cancelled caller's does; the
		// sibling's own request must still be answered.
		if stoppedRun {
			stoppedRun = false
			return context.Canceled
		}
		return nil
	}
	forge := NewMultiplexedForge(inner, newSteppingClock())
	forge.Window = time.Hour
	repo := GitHubRepo{Owner: "acme", Name: "repo"}

	owner := make(chan error, 1)
	go func() {
		_, err := forge.PullRequest(context.Background(), repo, 1)
		owner <- err
	}()
	inner.waitForCall(t)

	// The sibling joins while the owner's request is still in flight. Its own
	// context is live and stays live.
	joiner := make(chan GitHubPullRequest, 1)
	joinerErr := make(chan error, 1)
	go func() {
		pr, err := forge.PullRequest(context.Background(), repo, 1)
		joiner <- pr
		joinerErr <- err
	}()
	// The joiner has to have JOINED before the owner's request completes.
	// Releasing first would retire the entry, leave nothing to join, and
	// exercise the cold path while looking like it tested the join path.
	waitForJoin(t, forge)
	close(inner.release)

	if err := <-owner; err == nil {
		t.Fatal("the fixture never cancelled the owner, so this test proves nothing")
	}
	if err := <-joinerErr; err != nil {
		t.Fatalf("a live sibling adopted the stopped run's cancellation: %v", err)
	}
	if pr := <-joiner; pr.HeadSHA != "head" {
		t.Fatalf("the sibling did not get a real answer: head %q", pr.HeadSHA)
	}
}

// waitForJoin blocks until a caller has adopted an in-flight request. It polls
// a counter rather than sleeping a guessed interval: a joiner that never joins
// makes this time out and fail, where a sleep would let the test proceed down
// the cold path and pass without testing anything.
func waitForJoin(t *testing.T, forge *MultiplexedForge) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for forge.joinCount() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no caller ever joined the in-flight request")
		}
		time.Sleep(time.Millisecond)
	}
}

// TestBuildingAnEngineDoesNotHoldTheCacheLock. Under `serve` the runtime factory
// resolves the publication identity, which is a live forge request. Holding the
// engine-cache mutex across it serialized every run goroutine in a tick and
// every operator submission behind one slow GitHub response — and with
// context.Background() inside that call, an unresponsive forge was not
// cancellable during shutdown either.
//
// The property is that a SLOW build for one pairing does not stop a different
// pairing from being served.
func TestBuildingAnEngineDoesNotHoldTheCacheLock(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := supervisorRegistry(t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	building := make(chan struct{}, 1)
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:     OwnerLivenessFunc(func(string) bool { return false }),
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: 2,
		PollInterval: time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			if agent.ID == "codex" { // the slow one, standing in for a hung forge
				building <- struct{}{}
				<-release
			}
			deps := fixture.deps
			deps.Agent, deps.Agents = agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	slow := make(chan error, 1)
	go func() { _, err := supervisor.engine("acme/repo", "codex"); slow <- err }()
	select {
	case <-building:
	case <-time.After(10 * time.Second):
		t.Fatal("the slow build never started")
	}

	// A different pairing must be servable while that build is still in flight.
	done := make(chan error, 1)
	go func() { _, err := supervisor.engine("acme/repo", "claude"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the second pairing failed to build: %v", err)
		}
	case <-time.After(10 * time.Second):
		close(release)
		t.Fatal("a second pairing was blocked behind an in-flight build, so the cache lock is still held across it")
	}
	close(release)
	if err := <-slow; err != nil {
		t.Fatalf("the slow build failed: %v", err)
	}
}

// TestAFailedEngineBuildIsNotCachedForever. A forge that was unreachable for one
// tick must not make a repository-and-agent pairing permanently unusable.
func TestAFailedEngineBuildIsNotCachedForever(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := supervisorRegistry(t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1",
		Liveness:     OwnerLivenessFunc(func(string) bool { return false }),
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: 1,
		PollInterval: time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			if fail {
				return nil, errors.New("the forge was unreachable")
			}
			deps := fixture.deps
			deps.Agent, deps.Agents = agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := supervisor.engine("acme/repo", "codex"); err == nil {
		t.Fatal("a failing build reported success")
	}
	fail = false
	if _, err := supervisor.engine("acme/repo", "codex"); err != nil {
		t.Fatalf("the pairing stayed broken after the forge recovered: %v", err)
	}
}

// TestConcurrentSupervisorStartsElectOneWinner closes the reclaim race. Two
// processes starting at once could both dial a stale socket, both find nobody
// listening, and the second unlink the socket the first had just bound - so two
// supervisors ran, competing for one store, which is the outcome the endpoint
// exists to prevent.
func TestConcurrentSupervisorStartsElectOneWinner(t *testing.T) {
	dir := controlStateDir(t)
	// A stale socket from a crashed supervisor: present, but nobody listening.
	stale, err := net.Listen("unix", ControlSocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	stale.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := stale.Close(); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	listeners := make([]*ControlListener, racers)
	errs := make([]error, racers)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			listeners[i], errs[i] = ListenControl(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i := range listeners {
		if errs[i] == nil {
			winners++
			t.Cleanup(func() { _ = listeners[i].Close() })
			continue
		}
		if !errors.Is(errs[i], ErrSupervisorAlreadyRunning) {
			t.Fatalf("a loser failed for the wrong reason: %v", errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("%d supervisors bound the control endpoint at once, want exactly 1", winners)
	}
}

// A control command's deadline covers the work it asks for.
//
// `plan revise` clones a repository and invokes a planner in its own
// non-mutating mode, which is bounded in minutes. A 30-second deadline did not
// stop that work - the supervisor finished and persisted the revision either
// way - it only stopped the operator from being told, which is the effect
// without the answer.
func TestAControlDeadlineCoversTheWorkTheCommandAsksFor(t *testing.T) {
	if quick, revise := ControlDeadline(ControlStatus), ControlDeadline(ControlPlanRevise); revise <= quick {
		t.Fatalf("a plan revision is allowed %s and a status read %s: the long command needs the longer deadline", revise, quick)
	}
	if revise := ControlDeadline(ControlPlanRevise); revise < 10*time.Minute {
		t.Fatalf("a plan revision is allowed %s, which is under the planner's own bound", revise)
	}
	if unknown := ControlDeadline("something-else"); unknown != ControlDeadline(ControlStatus) {
		t.Fatalf("an unrecognized command is allowed %s, want the ordinary deadline", unknown)
	}
}

// An operator's note reaches the journal the same way whichever process
// records the decision.
//
// The local path truncates a note to the payload field bound before
// journalling; the delegated path sent it whole and let the request size bound
// refuse the connection, so a long note failed the command in one terminal and
// was accepted in the other.
func TestALongNoteIsTruncatedRatherThanRefused(t *testing.T) {
	long := strings.Repeat("z", 32<<10)
	bounded := BoundedNote(long)
	if len(bounded) != maxPayloadFieldBytes {
		t.Fatalf("a note of %d bytes bounded to %d, want the payload field bound %d", len(long), len(bounded), maxPayloadFieldBytes)
	}
	if short := BoundedNote("read it"); short != "read it" {
		t.Fatalf("a short note was altered: %q", short)
	}

	// And the bounded request fits the socket's own request bound with room to
	// spare, so the command is never refused for its note.
	encoded, err := json.Marshal(ControlRequest{
		Command: ControlPlanApprove, PlanID: "plan-x", Revision: 1,
		Digest: strings.Repeat("a", 64), Note: bounded,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= maxControlRequestBytes {
		t.Fatalf("a bounded request is %d bytes, at or above the %d-byte request bound", len(encoded), maxControlRequestBytes)
	}
}
