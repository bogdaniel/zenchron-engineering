package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// The #470 acceptance fixture: one governed repository with a hundred and ten
// open issues, a registry with two agents of different provider kinds, and a
// controlled worker that every child run's invocation goes through.

const fleetFirstIssue = 101

// fleetBehaviour is what the controlled worker does for one child.
type fleetBehaviour int

const (
	fleetValidHandoff fleetBehaviour = iota
	fleetNoHandoff
	fleetMalformedHandoff
	// fleetRepositorySeeded writes a perfect report into the candidate
	// REPOSITORY, where a pre-seeded file would be, and not into the slot.
	fleetRepositorySeeded
	fleetPartialHandoff
	fleetFails
	// The checkpoint shapes (#489): the first invocation mutates and is cut
	// off, so the runtime checkpoints it; the continuation completes that
	// checkpoint with a bound checkpoint-completion claim and a handoff.
	// ThenComplete changes nothing further, ThenMutate changes more, and
	// ThenRestatesSHA writes a handoff restating the checkpoint commit.
	fleetCheckpointThenComplete
	fleetCheckpointThenMutate
	fleetCheckpointThenRestatesSHA
	// fleetNoChangeHandoff changes nothing and writes a valid handoff.
	fleetNoChangeHandoff
	// The #492 shapes: the engineering is done and the handoff is a
	// repairable protocol mistake, a bound violation, or (NoChange) a
	// repairable mistake from an invocation with nothing to bind.
	fleetCompletedWithUnresolved
	fleetMalformedJSON
	fleetOversizedHandoff
	fleetNoChangeInvalidHandoff
)

// fleetRepair is what the controlled worker does when it is invoked as a
// handoff repair (#492).
type fleetRepair int

const (
	// repairWritesNothing is the zero value: a repair that writes no document.
	repairWritesNothing fleetRepair = iota
	repairWritesPartial
	repairWritesCompleted
	repairRestatesCandidate
	repairFails
	// repairMutatesCandidate reaches past its working directory into the
	// real candidate workspace, then writes a perfect document.
	repairMutatesCandidate
	// repairStoppedMidway calls onRepair (an operator stop), then runs until
	// its context ends, writes a perfect document anyway, and attributes its
	// ending the way an executor does: by the cause that ended it.
	repairStoppedMidway
	// repairFinishesDespiteStop calls onRepair, waits for the stop to reach
	// it, then writes a perfect document and exits successfully by itself.
	repairFinishesDespiteStop
)

const fleetCompletedUnresolvedReport = `{"schema_version":"0.1","outcome":"completed","summary":"Implemented the change.","unresolved":["the docs page"]}`

const fleetPartialReport = `{"schema_version":"0.1","outcome":"partial","summary":"Half of it.","unresolved":["the migration"]}`

const fleetValidReport = `{"schema_version":"0.1","outcome":"completed","summary":"Implemented the change.","recommended_next":["add a benchmark"]}`

// fleetProvider is a concurrency-safe controlled worker. It records how many
// invocations were in flight at once, which is the observable the capacity law
// is stated in, and it never decides capacity itself.
type fleetProvider struct {
	mu          sync.Mutex
	active      int
	peak        int
	invocations map[string]int
	requests    map[string]ExecutionRequest
	behaviour   map[string]fleetBehaviour
	// repairs counts handoff repair invocations apart from engineering ones,
	// and repairRequests keeps the latest one each run received.
	repairs        map[string]int
	repairRequests map[string]ExecutionRequest
	repairWith     map[string]fleetRepair
	stateDir       string
	// crashOnRepair loses the controller in the middle of every repair.
	crashOnRepair bool
	// onRepair, when set, runs inside every repair invocation.
	onRepair func(runID string)
	// gather, when set, holds invocations until that many are in flight at
	// once, or the bound passes; once it has been observed, later ones run
	// free. It cannot create concurrency the scheduler did not grant - every
	// held invocation was already granted a slot - it only makes the
	// concurrency the scheduler DID grant observable.
	gather int
	bound  time.Duration
	// hold keeps every invocation in flight at least this long, so a slot
	// granted beyond the ceiling would overlap and show up in peak.
	hold time.Duration
}

func newFleetProvider() *fleetProvider {
	return &fleetProvider{invocations: map[string]int{}, requests: make(map[string]ExecutionRequest), behaviour: map[string]fleetBehaviour{},
		repairs: map[string]int{}, repairRequests: make(map[string]ExecutionRequest), repairWith: map[string]fleetRepair{}}
}

// WritesTypedResults: the controlled worker writes straight to the slot path.
func (p *fleetProvider) WritesTypedResults() bool { return true }

func (p *fleetProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *fleetProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if request.Purpose == InvocationHandoffRepair {
		return p.repair(ctx, request)
	}
	p.mu.Lock()
	p.active++
	p.peak = max(p.peak, p.active)
	p.invocations[request.RunID]++
	invocation := p.invocations[request.RunID]
	p.requests[request.RunID] = request
	behaviour := p.behaviour[request.RunID]
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()
	if p.gather > 0 {
		deadline := time.Now().Add(p.bound)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			reached := p.peak >= p.gather
			p.mu.Unlock()
			if reached {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	time.Sleep(p.hold)
	// Prose that LOOKS like a handoff, in the place a transcript would be. It
	// must never become one.
	if behaviour == fleetNoChangeHandoff {
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded}, os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
	}
	if behaviour == fleetNoChangeInvalidHandoff {
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded}, os.WriteFile(request.HandoffPath, []byte(fleetCompletedUnresolvedReport), 0o600)
	}
	if err := os.WriteFile(filepath.Join(request.CandidateDir, "candidate.go"),
		[]byte("package candidate\n// handoff: {\"outcome\":\"completed\"} done\nconst Run = \""+request.RunID+"\"\n"), 0o600); err != nil {
		return ExecutionResult{}, err
	}
	result := ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded}
	switch behaviour {
	case fleetCheckpointThenComplete, fleetCheckpointThenMutate, fleetCheckpointThenRestatesSHA:
		return fleetContinue(request, behaviour, invocation)
	case fleetFails:
		// A perfect handoff from an invocation that FAILED. It must never be
		// read: an unfinished invocation contributes no handoff.
		if request.HandoffPath != "" {
			if err := os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: OperationFailed,
			Failure: &ProviderFailure{Classification: FailureUnknown}}, nil
	case fleetValidHandoff:
		if request.HandoffPath != "" {
			return result, os.WriteFile(request.HandoffPath, []byte(fleetValidReport), 0o600)
		}
	case fleetRepositorySeeded:
		for _, name := range []string{"handoff.json", filepath.Base(request.HandoffPath)} {
			if err := os.WriteFile(filepath.Join(request.CandidateDir, name), []byte(fleetValidReport), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
	case fleetPartialHandoff:
		if request.HandoffPath != "" {
			return result, os.WriteFile(request.HandoffPath, []byte(fleetPartialReport), 0o600)
		}
	case fleetMalformedHandoff:
		if request.HandoffPath != "" {
			return result, os.WriteFile(request.HandoffPath, []byte(`{"schema_version":"0.1","outcome":"completed","summary":"s","run_id":"run-forged"}`), 0o600)
		}
	case fleetCompletedWithUnresolved:
		return result, os.WriteFile(request.HandoffPath, []byte(fleetCompletedUnresolvedReport), 0o600)
	case fleetMalformedJSON:
		return result, os.WriteFile(request.HandoffPath, []byte(`{"schema_version":"0.1","outcome":"completed",`), 0o600)
	case fleetOversizedHandoff:
		return result, os.WriteFile(request.HandoffPath, []byte(`{"schema_version":"0.1","outcome":"completed","summary":"`+
			strings.Repeat("s", 8<<10)+`"}`), 0o600)
	}
	return result, nil
}

// repair is the controlled worker invoked as a handoff repair.
func (p *fleetProvider) repair(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.mu.Lock()
	p.repairs[request.RunID]++
	p.repairRequests[request.RunID] = request
	behaviour, stateDir := p.repairWith[request.RunID], p.stateDir
	p.mu.Unlock()
	if p.crashOnRepair {
		crashRepair()
	}
	if p.onRepair != nil {
		p.onRepair(request.RunID)
	}
	result := ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded}
	write := func(document string) (ExecutionResult, error) {
		return result, os.WriteFile(request.HandoffPath, []byte(document), 0o600)
	}
	switch behaviour {
	case repairWritesPartial:
		return write(`{"schema_version":"0.1","outcome":"partial","summary":"Implemented the change.","unresolved":["the docs page"]}`)
	case repairWritesCompleted:
		return write(fleetValidReport)
	case repairRestatesCandidate:
		return write(`{"schema_version":"0.1","outcome":"completed","summary":"s","candidate_revision":"` + request.Candidate.Revision + `"}`)
	case repairFails:
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: OperationFailed, Executed: true, Failure: &ProviderFailure{Classification: FailureUnknown}},
			errors.New("the provider process exited 1")
	case repairStoppedMidway, repairFinishesDespiteStop:
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
			return ExecutionResult{}, errors.New("the repair was never reached by the stop")
		}
		if _, err := write(fleetValidReport); err != nil {
			return ExecutionResult{}, err
		}
		if behaviour == repairFinishesDespiteStop {
			return ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded, Executed: true}, nil
		}
		class := FailureControllerShutdown
		if ownerOfCancellation(ctx) == OwnerOperatorStop {
			class = FailureRunCancelled
		}
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: OperationCancelled, Executed: true, Failure: &ProviderFailure{Classification: class}}, ctx.Err()
	case repairMutatesCandidate:
		dir := candidateDir(stateDir, request.RunID)
		for name, content := range map[string]string{"candidate.go": "package candidate\n// rewritten by the repair\n", "planted.go": "package candidate\n"} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
				return ExecutionResult{}, err
			}
		}
		return write(fleetValidReport)
	}
	return result, nil
}

// fleetContinue is the checkpoint shape: invocation 1 rewrites candidate.go
// and is cut off; every later one (a continuation) rewrites it identically -
// zero delta - unless told to mutate further, states a checkpoint-completion
// claim bound to the exact subject it was shown, and writes its handoff.
func fleetContinue(request ExecutionRequest, behaviour fleetBehaviour, invocation int) (ExecutionResult, error) {
	if invocation == 1 {
		return ExecutionResult{ProviderID: "fleet-worker", Outcome: OperationFailed, Failure: &ProviderFailure{Classification: FailureUnknown}},
			&ProviderStopError{Reason: StopIterationBudget, Detail: "reasoning iterations exceeded 16"}
	}
	if behaviour == fleetCheckpointThenMutate {
		if err := os.WriteFile(filepath.Join(request.CandidateDir, "continued.go"), []byte("package candidate\n"), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	report := fleetValidReport
	if behaviour == fleetCheckpointThenRestatesSHA {
		report = `{"schema_version":"0.1","outcome":"completed","summary":"s","candidate_revision":"` + request.Candidate.Revision + `"}`
	}
	return ExecutionResult{ProviderID: "fleet-worker", Outcome: Succeeded, Resolution: &FeedbackResolution{
		SchemaVersion: FeedbackResolutionSchemaVersion, Resolution: FeedbackResolutionCheckpointComplete,
		Subject: request.Candidate.Revision, Tree: request.Candidate.Tree,
	}}, os.WriteFile(request.HandoffPath, []byte(report), 0o600)
}

// request returns the latest request one run's worker received.
func (p *fleetProvider) request(runID string) ExecutionRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.requests[runID]
}

func (p *fleetProvider) setRepair(runID string, behaviour fleetRepair) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repairWith[runID] = behaviour
}

// counts reads one run's engineering and repair invocation counts.
func (p *fleetProvider) counts(runID string) (engineering, repairs int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.invocations[runID], p.repairs[runID]
}

func (p *fleetProvider) set(runID string, behaviour fleetBehaviour) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.behaviour[runID] = behaviour
}

// lockedAssurance serializes the shared assurance double, which concurrent
// children all call.
type lockedAssurance struct {
	mu    sync.Mutex
	inner *FakeAssuranceProvider
}

func (a *lockedAssurance) Assure(ctx context.Context, request AssuranceRequest) (AssuranceResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.inner.Results = append(a.inner.Results, AssuranceResult{ProviderID: "test-verifier", VerifierDefinition: "verifier-v1", Passed: true})
	return a.inner.Assure(ctx, request)
}

func (a *lockedAssurance) ProducedEvidenceClasses() []domain.EvidenceClass {
	return a.inner.ProducedEvidenceClasses()
}

type fleetFixture struct {
	*phase8Fixture
	worker   *fleetProvider
	capacity int
}

func newFleetFixture(t *testing.T, capacity int) *fleetFixture {
	t.Helper()
	fixture := newPhase8Fixture(t)
	// A hundred and ten open issues. Ten are named; the other hundred exist
	// so that "only what was named" is a claim with something to violate.
	for issue := fleetFirstIssue; issue < fleetFirstIssue+110; issue++ {
		fixture.forge.Issues[issue] = GitHubIssue{
			Number: issue, URL: fmt.Sprintf("https://github.com/acme/repo/issues/%d", issue),
			Title: UntrustedText(fmt.Sprintf("issue %d", issue)), Body: "body", State: GitHubOpen,
			UpdatedAt: fixture.clock.Now(), Author: GitHubActor{Login: "operator", ID: 7},
		}
	}
	worker := newFleetProvider()
	worker.stateDir = fixture.stateDir
	fixture.deps.Provider = worker
	fixture.deps.Assurance = &lockedAssurance{inner: &FakeAssuranceProvider{}}
	fixture.deps.MaxConcurrentRuns, fixture.deps.OperatorMaxConcurrentRuns = capacity, capacity
	// Observation is a separate class with its own ceiling (#85). It is set
	// wide so that the WORK ceiling under test is the only thing that bounds
	// how many workers run at once.
	fixture.deps.MaxConcurrentObservations = 10
	// The fixture clock steps on every read, so wall time here is a count of
	// clock reads, which concurrency and -race inflate. Budgets are not what
	// these tests are about; a wide one keeps them from deciding outcomes.
	fixture.deps.Budgets.WallLimit = 1000 * time.Hour
	// ONE live process owns every lease here, as one `serve` does. The base
	// fixture reports every owner dead, which - under a clock that steps on
	// every read - lets a sibling reclaim a slot whose worker is still
	// running, and would measure that fixture artifact instead of the
	// ceiling.
	fixture.deps.Liveness = OwnerLivenessFunc(func(owner string) bool { return owner == "owner-1" })
	// And, as in `serve`, one record of the leases this process's drivers
	// hold, shared by every engine the supervisor builds (#485).
	fixture.deps.DriverLeases = NewDriverLeases()
	return &fleetFixture{phase8Fixture: fixture, worker: worker, capacity: capacity}
}

// supervisor builds a supervisor over the fixture's CURRENT store, exactly as
// a restarted `serve` would be built: nothing carried over in memory.
func (f *fleetFixture) supervisor() *Supervisor {
	f.t.Helper()
	registry := supervisorRegistry(f.t)
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		f.t.Fatal(err)
	}
	supervisor, err := NewSupervisor(SupervisorDependencies{
		Store: f.store, Clock: f.clock, Owner: "owner-1", StateDir: f.stateDir,
		Liveness:     f.deps.Liveness,
		Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: f.capacity,
		// The supervisor offers more turns than there are work slots, so a
		// bound that held only because few runs were driven would not hold.
		MaxConcurrentObservations: 10,
		PollInterval:              time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			deps := f.deps
			deps.Store, deps.Agent, deps.Agents = f.store, agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return supervisor
}

func fleetIssues(n int) []int {
	issues := make([]int, n)
	for i := range issues {
		issues[i] = fleetFirstIssue + i
	}
	return issues
}

func (f *fleetFixture) orchestrate(supervisor *Supervisor, agent string, issues []int) OrchestrationView {
	f.t.Helper()
	view, err := supervisor.Orchestrate(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: agent, Issues: issues, Operator: "operator@example",
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return view
}

// drive ticks until every item has settled into a state the fixture does not
// move on from, or fails the test.
func (f *fleetFixture) drive(supervisor *Supervisor, batchID string) OrchestrationView {
	f.t.Helper()
	for range 40 {
		report, err := supervisor.Tick(context.Background())
		if err != nil {
			f.t.Fatal(err)
		}
		for _, problem := range report.Orchestration {
			f.t.Log("orchestration: " + problem)
		}
		for _, driven := range report.Driven {
			if driven.Error != "" {
				f.t.Logf("driven %s: %s", driven.RunID, driven.Error)
			}
		}
		f.clock.advance(61 * time.Second)
		view := f.status(batchID)
		settled := 0
		for _, item := range view.Items {
			switch item.State {
			case orchestration.ItemHandoffPending:
				// Admission is a later supervisor pass; wait for its durable
				// decision before asserting this fixture's final transfer outcome.
				if item.Handoff != orchestration.HandoffReported {
					settled++
				}
			case orchestration.ItemCompleted, orchestration.ItemPartial, orchestration.ItemFailed, orchestration.ItemStopped:
				settled++
			}
		}
		if settled == len(view.Items) {
			return view
		}
	}
	view := f.status(batchID)
	for _, item := range view.Items {
		f.t.Logf("issue %d: %s %s/%s handoff=%s reason=%s", item.Issue, item.State, item.Phase, item.Disposition, item.Handoff, item.Reason)
	}
	f.t.Fatalf("the batch never settled: %+v", view.Counts)
	return view
}

func (f *fleetFixture) status(batchID string) OrchestrationView {
	f.t.Helper()
	view, err := OrchestrationStatus(f.store, f.stateDir, batchID, f.clock.Now())
	if err != nil {
		f.t.Fatal(err)
	}
	return view
}

// TestTenIssuesBecomeOneBatchOfTenOrdinaryRuns is #470 acceptance A, I and J:
// one durable batch, ten distinct ordinary runs bound to the named agent,
// nothing for the hundred issues nobody named, no duplicate on a resubmission
// whose reply was lost - and the same semantics for two agents of different
// provider kinds.
func TestTenIssuesBecomeOneBatchOfTenOrdinaryRuns(t *testing.T) {
	for _, agent := range []string{"claude", "codex"} {
		t.Run(agent, func(t *testing.T) {
			fixture := newFleetFixture(t, 10)
			supervisor := fixture.supervisor()
			view := fixture.orchestrate(supervisor, agent, fleetIssues(10))
			if view.Counts.Total != 10 || len(view.Items) != 10 || view.AgentID != agent {
				t.Fatalf("view = %+v", view)
			}
			runs, err := fixture.store.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if len(runs) != 10 {
				t.Fatalf("%d runs exist for a batch naming ten issues out of a hundred and ten open", len(runs))
			}
			seen := map[string]bool{}
			for _, item := range view.Items {
				run, ok := storedRun(t, fixture.phase8Fixture, item.RunID)
				if !ok || seen[item.RunID] {
					t.Fatalf("issue %d has no distinct run: %s", item.Issue, item.RunID)
				}
				seen[item.RunID] = true
				if run.Goal != issueGoal("acme/repo", item.Issue) || run.AgentID != agent ||
					run.Orchestration == nil || run.Orchestration.BatchID != view.BatchID || run.Plan != nil {
					t.Fatalf("run %s is not an ordinary run of issue %d bound to %s and the batch: %+v", run.ID, item.Issue, agent, run)
				}
				if bound := recordedAgentOf(t, fixture.state(run.ID)).AgentID; bound != agent {
					t.Fatalf("run %s journals agent %q", run.ID, bound)
				}
			}
			// A lost reply: the operator sends the same request again, in a
			// different order, to a supervisor built fresh from the store.
			reversed := fleetIssues(10)
			for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
				reversed[i], reversed[j] = reversed[j], reversed[i]
			}
			again := fixture.orchestrate(fixture.supervisor(), agent, reversed)
			if again.BatchID != view.BatchID {
				t.Fatalf("a resubmission created batch %s beside %s", again.BatchID, view.BatchID)
			}
			after, err := fixture.store.Runs()
			if err != nil {
				t.Fatal(err)
			}
			if len(after) != 10 {
				t.Fatalf("a resubmission created %d more runs", len(after)-10)
			}
			batches, err := fixture.store.OrchestrationBatches()
			if err != nil || len(batches) != 1 {
				t.Fatalf("batches = %d (%v), want exactly one", len(batches), err)
			}
		})
	}
}

// TestTheSchedulerAloneBoundsBatchConcurrency is #470 acceptance B. The same
// ten-item batch runs at most three workers at once under a ceiling of three,
// every item still gets its turn, and all ten run at once under a ceiling of
// ten. The batch has no pool of its own to compete with the scheduler.
func TestTheSchedulerAloneBoundsBatchConcurrency(t *testing.T) {
	for _, capacity := range []int{3, 10} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			fixture := newFleetFixture(t, capacity)
			// The bound is only a safety net: the target is reached as soon
			// as the scheduler grants it, and a peak below the ceiling after
			// waiting this long is the failure being tested for.
			fixture.worker.gather, fixture.worker.bound, fixture.worker.hold = capacity, 30*time.Second, 300*time.Millisecond
			supervisor := fixture.supervisor()
			view := fixture.orchestrate(supervisor, "claude", fleetIssues(10))
			settled := fixture.drive(supervisor, view.BatchID)
			fixture.worker.mu.Lock()
			peak, executed := fixture.worker.peak, len(fixture.worker.invocations)
			fixture.worker.mu.Unlock()
			if peak != capacity {
				t.Fatalf("peak concurrent workers = %d under a ceiling of %d", peak, capacity)
			}
			for _, item := range settled.Items {
				if item.State != orchestration.ItemCompleted {
					t.Logf("issue %d: %s %s/%s reason=%s", item.Issue, item.State, item.Phase, item.Disposition, item.Reason)
				}
			}
			if executed != 10 || settled.Counts.Completed != 10 {
				t.Fatalf("%d of 10 items were executed and %d completed: %+v", executed, settled.Counts.Completed, settled.Counts)
			}
		})
	}
}

// TestOneFailingChildDoesNotStopItsPeers is #470 acceptance C.
func TestOneFailingChildDoesNotStopItsPeers(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(10))
	failing := view.Items[3].RunID
	fixture.worker.set(failing, fleetFails)
	settled := fixture.drive(supervisor, view.BatchID)
	if settled.Counts.Completed != 9 || settled.Counts.Total != 10 {
		t.Fatalf("one failing child changed its peers' outcome: %+v", settled.Counts)
	}
	for _, item := range settled.Items {
		if item.RunID == failing {
			if item.State != orchestration.ItemFailed || item.Handoff != orchestration.HandoffNone {
				t.Fatalf("the failing child projected as %s with handoff %s", item.State, item.Handoff)
			}
			if events := handoffEvents(t, fixture.store, failing); len(events) != 0 {
				t.Fatalf("a failed invocation's handoff was read: %v", journalTypes(events))
			}
			continue
		}
		if item.State != orchestration.ItemCompleted {
			t.Fatalf("peer issue %d is %s", item.Issue, item.State)
		}
	}
}
