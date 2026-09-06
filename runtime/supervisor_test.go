package runtime

// The persistent supervisor and its control endpoint.
//
// The endpoint tests are the security ones: it is an operator-authority
// boundary, so what it refuses matters more than what it accepts.

import (
	"context"
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
		Runtime:           func(GitHubRepo) (*EngineeringRuntime, error) { return fixture.runtime, nil },
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
		Runtime: func(repo GitHubRepo) (*EngineeringRuntime, error) {
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
