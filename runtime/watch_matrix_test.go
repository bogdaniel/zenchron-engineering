package runtime

// Phase 9 §16: the two-watcher acceptance matrix.
//
// Every scenario below runs TWO watchers that share nothing a single process
// could fake on their behalf:
//
//   - two *SQLiteOperationStore handles over the same database file. Two
//     *sql.DB values share no cache, no transaction and no mutex, so every
//     ordering asserted here has to come out of SQLite itself.
//   - two runtime owner identities, each backed by a REAL kernel ownership
//     lock. Liveness is NewLockOwnerLiveness over the shared state dir, which
//     is the production evidence - an OS advisory lock the kernel releases when
//     its holder dies - and never a test stub that answers yes.
//   - two EngineeringRuntimes with their own provider and verifier, and two
//     WatchControllers.
//
// What IS shared is the world: one durable database file, one real git origin,
// one forge double, one frozen clock. The forge double documents itself as
// single-goroutine, so watchers reach it through sharedForge, whose mutex is
// the TEST's - it keeps two goroutines from racing the double's maps and is
// never a mechanism the product depends on. Nothing in this file may pass
// because two controllers happen to live in one address space.
//
// Assertions read durable state: the runs table, the journal, the operation
// rows and the real git remote. A TickReport is used only as evidence that a
// watcher ran, never as evidence that it was right.
//
// Waiting is bounded and never a sleep-until-it-probably-worked: where a window
// must exist for a racing watcher to misbehave in, the winner PARKS inside its
// leased operation and polls the durable store until a deadline. If the ceiling
// holds, the poll finds nothing and the park is pure cost; if it does not, the
// second driven run appears in the store and the test fails.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The shared world
// ---------------------------------------------------------------------------

const (
	// matrixWindow bounds every wait in this file. It is a deadline, not a
	// timing assumption: no assertion depends on it elapsing.
	matrixWindow = 300 * time.Millisecond
	matrixPoll   = 2 * time.Millisecond
	// matrixExpiry is comfortably past the one-minute scheduler lease, so
	// "the heartbeat is gone" is never in doubt in D and E.
	matrixExpiry = 5 * time.Minute
)

// watchMatrix is the world two independent watchers share.
type watchMatrix struct {
	*phase8Fixture
	forgeMu sync.Mutex
}

func newWatchMatrix(t *testing.T) *watchMatrix {
	t.Helper()
	return &watchMatrix{phase8Fixture: newWatchFixture(t)}
}

// watcherStopped is the panic a crash test raises. It unwinds with no cleanup
// whatsoever, which is what a killed process looks like from durable state:
// whatever the scheduler already wrote is still written.
type watcherStopped struct{ peer string }

// watchPeer is one watcher process's entire world.
type watchPeer struct {
	name     string
	owner    string
	store    *SQLiteOperationStore
	provider *isolatedProvider
	forge    *sharedForge
	engine   *EngineeringRuntime
	watcher  *WatchController
	// atRuntime, when set, runs where the watch cycle resolves this
	// repository's runtime - the last point before the source claim that a
	// test can reach without touching the product.
	atRuntime func()
}

// peer builds one independent watcher. An empty owner means "this process is
// the watcher": a runtime identity is minted and a REAL ownership lock is taken
// for it, so NewLockOwnerLiveness reports it alive for the same reason it would
// in production. A non-empty owner belongs to a child process that already
// holds that lock, which is how D and E get a holder whose death is real.
func (m *watchMatrix) peer(t *testing.T, name, owner string) *watchPeer {
	t.Helper()
	store, err := OpenSQLiteOperationStore(m.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	peer := &watchPeer{name: name, owner: owner, store: store}
	if owner == "" {
		peer.owner = fmt.Sprintf("%s/%d/watch-matrix-%s", ownerHost(), os.Getpid(), name)
		lock, err := AcquireOwnershipLock(m.stateDir, peer.owner)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = lock.Release() })
	}
	peer.forge = &sharedForge{mu: &m.forgeMu, forge: m.forge}
	peer.provider = newIsolatedProvider(func(dir string) error {
		return os.WriteFile(filepath.Join(dir, "candidate.go"), []byte("package candidate\n"), 0600)
	})

	deps := m.deps
	deps.Store = store
	deps.Owner = peer.owner
	deps.Liveness = NewLockOwnerLiveness(m.stateDir)
	deps.GitHub = peer.forge
	deps.Provider = peer.provider
	deps.Assurance = passingAssurance()
	peer.engine = m.newRuntime(deps)

	watcher, err := NewWatchController(WatchDependencies{
		Store:    store,
		Clock:    m.clock,
		Owner:    peer.owner,
		Liveness: NewLockOwnerLiveness(m.stateDir),
		GitHub:   peer.forge,
		Settings: WatchSettings{
			Repositories:      []GitHubRepo{repoA},
			Label:             DefaultWatchLabel,
			PollInterval:      watchPollInterval,
			MaxConcurrentRuns: 1,
		},
		Runtime: func(repo GitHubRepo) (*EngineeringRuntime, error) {
			if repo != repoA {
				return nil, fmt.Errorf("no runtime is configured for %s", repo)
			}
			if peer.atRuntime != nil {
				peer.atRuntime()
			}
			return peer.engine, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	peer.watcher = watcher
	return peer
}

// pair is the ordinary two-watcher setup: both alive, both holding their own
// ownership lock, neither able to prove the other dead.
func (m *watchMatrix) pair(t *testing.T) (*watchPeer, *watchPeer) {
	t.Helper()
	return m.peer(t, "first", ""), m.peer(t, "second", "")
}

// ---------------------------------------------------------------------------
// The forge seam
// ---------------------------------------------------------------------------

// sharedForge is one peer's handle on the SHARED forge double. before runs
// OUTSIDE the lock, so a watcher parked inside a call never blocks the watcher
// it is racing.
type sharedForge struct {
	mu    *sync.Mutex
	forge *FakeGitHubAdapter
	// before, when set, runs at the start of every call. It is how a test
	// stops a watcher's process mid-operation or parks it inside a lease.
	before func(method string)
	// after, when set, runs once the call has been PERFORMED and the lock
	// dropped. It is the only way to stop a watcher between a side effect and
	// the journal write that records it.
	after func(method string)
	// discover, when set, answers DiscoverIssues for this peer alone, which is
	// how two watchers observe genuinely different opted-in sets.
	discover func() DiscoveryResult
}

func (s *sharedForge) enter(method string) {
	if s.before != nil {
		s.before(method)
	}
	s.mu.Lock()
}

func (s *sharedForge) leave(method string) {
	s.mu.Unlock()
	if s.after != nil {
		s.after(method)
	}
}

func (s *sharedForge) Issue(ctx context.Context, repo GitHubRepo, number int) (GitHubIssue, error) {
	s.enter("Issue")
	defer s.leave("Issue")
	return s.forge.Issue(ctx, repo, number)
}
func (s *sharedForge) DiscoverIssues(ctx context.Context, query DiscoveryQuery) (DiscoveryResult, error) {
	if s.discover != nil {
		// A scripted answer touches no shared state, so it takes no lock: the
		// test must not become the thing that serializes two watchers.
		if s.before != nil {
			s.before("DiscoverIssues")
		}
		result := s.discover()
		if s.after != nil {
			s.after("DiscoverIssues")
		}
		return result, nil
	}
	s.enter("DiscoverIssues")
	defer s.leave("DiscoverIssues")
	return s.forge.DiscoverIssues(ctx, query)
}
func (s *sharedForge) FindPullRequests(ctx context.Context, repo GitHubRepo, headRef, baseRef string) ([]GitHubPullRequest, error) {
	s.enter("FindPullRequests")
	defer s.leave("FindPullRequests")
	return s.forge.FindPullRequests(ctx, repo, headRef, baseRef)
}
func (s *sharedForge) CreatePullRequest(ctx context.Context, repo GitHubRepo, request GitHubPullRequestCreate) (GitHubPullRequest, error) {
	s.enter("CreatePullRequest")
	defer s.leave("CreatePullRequest")
	return s.forge.CreatePullRequest(ctx, repo, request)
}
func (s *sharedForge) UpdatePullRequest(ctx context.Context, repo GitHubRepo, number int, update GitHubPullRequestUpdate) (GitHubPullRequest, error) {
	s.enter("UpdatePullRequest")
	defer s.leave("UpdatePullRequest")
	return s.forge.UpdatePullRequest(ctx, repo, number, update)
}
func (s *sharedForge) PullRequest(ctx context.Context, repo GitHubRepo, number int) (GitHubPullRequest, error) {
	s.enter("PullRequest")
	defer s.leave("PullRequest")
	return s.forge.PullRequest(ctx, repo, number)
}
func (s *sharedForge) Checks(ctx context.Context, repo GitHubRepo, headSHA string) (GitHubCheckObservation, error) {
	s.enter("Checks")
	defer s.leave("Checks")
	return s.forge.Checks(ctx, repo, headSHA)
}
func (s *sharedForge) Reviews(ctx context.Context, repo GitHubRepo, number int, headSHA string) (GitHubReviewObservation, error) {
	s.enter("Reviews")
	defer s.leave("Reviews")
	return s.forge.Reviews(ctx, repo, number, headSHA)
}
func (s *sharedForge) CommentOnPullRequest(ctx context.Context, repo GitHubRepo, number int, body Publication) error {
	s.enter("CommentOnPullRequest")
	defer s.leave("CommentOnPullRequest")
	return s.forge.CommentOnPullRequest(ctx, repo, number, body)
}
func (s *sharedForge) RefSHA(ctx context.Context, repo GitHubRepo, ref string) (RefObservation, error) {
	s.enter("RefSHA")
	defer s.leave("RefSHA")
	return s.forge.RefSHA(ctx, repo, ref)
}

var _ GitHubAdapter = (*sharedForge)(nil)

// ---------------------------------------------------------------------------
// Running a watcher
// ---------------------------------------------------------------------------

func tickOf(t *testing.T, p *watchPeer) TickReport {
	t.Helper()
	report, err := p.watcher.Tick(context.Background())
	if err != nil {
		t.Fatalf("%s: Tick: %v", p.name, err)
	}
	return report
}

// concurrently releases both watchers from one barrier. The reports it returns
// are evidence that both watchers ran; every correctness assertion reads the
// durable store instead.
func concurrently(t *testing.T, peers ...*watchPeer) []TickReport {
	t.Helper()
	reports := make([]TickReport, len(peers))
	errs := make([]error, len(peers))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, peer := range peers {
		wg.Add(1)
		go func(i int, peer *watchPeer) {
			defer wg.Done()
			<-start
			reports[i], errs[i] = peer.watcher.Tick(context.Background())
		}(i, peer)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("%s: Tick: %v", peers[i].name, err)
		}
	}
	return reports
}

// newMeeting is an n-party rendezvous with a bounded deadline. It exists so a
// race can be made genuinely simultaneous rather than merely started together:
// two watchers released from one barrier still drift apart across a forge call,
// and the window the source claim closes is only a few statements wide. It
// gives up rather than hanging if a party never arrives.
func newMeeting(n int) func() {
	var mu sync.Mutex
	arrived := 0
	ready := make(chan struct{})
	return func() {
		mu.Lock()
		arrived++
		if arrived == n {
			close(ready)
		}
		mu.Unlock()
		select {
		case <-ready:
		case <-time.After(matrixWindow):
		}
	}
}

// meetBeforeClaiming holds every watcher where the cycle resolves the
// repository runtime and releases them together, so the source claim really is
// entered simultaneously instead of merely started so.
func meetBeforeClaiming(peers ...*watchPeer) {
	meet := newMeeting(len(peers))
	for _, peer := range peers {
		peer.atRuntime = meet
	}
}

// crashOn stops this watcher's process immediately BEFORE the named forge call.
// Whatever lease the scheduler durably recorded to reach that call is left
// exactly as a kill would leave it.
func (p *watchPeer) crashOn(method string) {
	p.forge.before = func(call string) {
		if call == method {
			panic(watcherStopped{p.name})
		}
	}
}

// crashAfter stops this watcher's process the instant the named call has been
// PERFORMED, before the operation that made it can record anything: the side
// effect landed on the forge and the journal knows nothing about it.
func (p *watchPeer) crashAfter(method string) {
	p.forge.after = func(call string) {
		if call == method {
			panic(watcherStopped{p.name})
		}
	}
}

// stops runs one tick and requires the watcher's process to end inside it.
func (p *watchPeer) stops(t *testing.T) {
	t.Helper()
	defer func() {
		switch value := recover().(type) {
		case nil:
			t.Fatalf("%s: the watcher completed its tick instead of stopping", p.name)
		case watcherStopped:
		default:
			panic(value)
		}
	}()
	_, _ = p.watcher.Tick(context.Background())
	p.forge.before, p.forge.after = nil, nil
}

// ---------------------------------------------------------------------------
// Durable evidence
// ---------------------------------------------------------------------------

// oneRunPerSource asserts the durable identity space holds exactly one run for
// the issue: generation zero exists and no later generation was ever created.
// It is read through the caller's own store handle.
func oneRunPerSource(t *testing.T, store *SQLiteOperationStore, engine *EngineeringRuntime, issue int) string {
	t.Helper()
	runID := ""
	for generation := 0; generation < 4; generation++ {
		id, err := issueRunID(engine.deps.Repository.Identity, issue, engine.deps.ConfigDigest, generation)
		if err != nil {
			t.Fatal(err)
		}
		run, ok, err := store.Run(id)
		if err != nil {
			t.Fatal(err)
		}
		switch {
		case generation == 0 && !ok:
			t.Fatalf("issue %d produced no durable run", issue)
		case generation == 0:
			if run.Goal != issueGoal(engine.deps.Repository.Identity, issue) {
				t.Fatalf("run %s answers %q", id, run.Goal)
			}
			runID = id
		case ok:
			t.Fatalf("issue %d produced a second EngineeringRun at generation %d (%s)", issue, generation, id)
		}
	}
	return runID
}

// journalFrom reads the durable journal through the caller's own handle. Events
// only come back at all when the hash chain still verifies.
func journalFrom(t *testing.T, store *SQLiteOperationStore, runID string) []EngineeringEvent {
	t.Helper()
	events, err := store.Events(runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Replay(runID); err != nil {
		t.Fatalf("the journal of %s does not replay: %v", runID, err)
	}
	return events
}

// leaseOwners is who durably drove each attempt: the lease recorded in every
// operation.before payload, in journal order. It is the answer to "which
// process held the operation", taken from the append-only record rather than
// from anything a controller returned.
func leaseOwners(t *testing.T, events []EngineeringEvent) []string {
	t.Helper()
	owners := []string{}
	for _, event := range events {
		if event.Type != EventOperationBefore {
			continue
		}
		var op RunOperation
		if err := json.Unmarshal(event.Payload, &op); err != nil {
			t.Fatal(err)
		}
		if op.Lease == nil {
			t.Fatalf("operation.before at sequence %d records no lease", event.Sequence)
		}
		owners = append(owners, op.Lease.Owner)
	}
	return owners
}

// succeededOnce refuses a journal in which any operation committed twice. Two
// operation.after rows recording success for one operation id is what a
// duplicated side effect looks like from the record.
func succeededOnce(t *testing.T, events []EngineeringEvent) {
	t.Helper()
	commits := map[string]int{}
	for _, event := range events {
		if event.Type != EventOperationAfter {
			continue
		}
		var op RunOperation
		if err := json.Unmarshal(event.Payload, &op); err != nil {
			t.Fatal(err)
		}
		if op.State == Succeeded {
			commits[op.ID]++
		}
	}
	for id, n := range commits {
		if n > 1 {
			t.Fatalf("operation %s committed %d times", id, n)
		}
	}
}

// heldLease reports the single leased or running operation of a run, from the
// durable row rather than from the journal.
func heldLease(t *testing.T, store *SQLiteOperationStore, runID string) RunOperation {
	t.Helper()
	ops, err := store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var held []RunOperation
	for _, op := range ops {
		if (op.State == Leased || op.State == Running) && op.Lease != nil {
			held = append(held, op)
		}
	}
	if len(held) != 1 {
		t.Fatalf("run %s holds %d leases, want exactly one", runID, len(held))
	}
	return held[0]
}

// drivenRunCount is the durable global ceiling as the database sees it: how
// many distinct runs currently hold a run-driving slot.
func drivenRunCount(store *SQLiteOperationStore) int {
	ops, err := store.AllOperations()
	if err != nil {
		return 0
	}
	runs := map[string]bool{}
	for _, op := range ops {
		if (op.State == Leased || op.State == Running) && op.Lease != nil {
			runs[op.RunID] = true
		}
	}
	return len(runs)
}

func (p *watchPeer) holdsALease() bool {
	ops, err := p.store.AllOperations()
	if err != nil {
		return false
	}
	for _, op := range ops {
		if (op.State == Leased || op.State == Running) && op.Lease != nil && op.Lease.Owner == p.owner {
			return true
		}
	}
	return false
}

// ceilingProbe is the durable-ceiling assertion. Every sample is a query
// against a watcher's own SQLite handle, so what it records is the database's
// answer to "how many runs are being driven right now".
type ceilingProbe struct {
	mu  sync.Mutex
	max int
}

func (c *ceilingProbe) record(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if n > c.max {
		c.max = n
	}
}

func (c *ceilingProbe) peak() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.max
}

// park installs the probe on this peer. On the FIRST call this watcher makes
// while it durably holds a lease it parks, polling the store until a second
// driven run appears or the bounded deadline passes. That park is the window
// the racing watcher needs in order to break the ceiling if the ceiling can be
// broken; nothing asserts that the deadline is reached.
func (p *watchPeer) park(probe *ceilingProbe) {
	parked := false
	p.forge.before = func(string) {
		probe.record(drivenRunCount(p.store))
		if parked || !p.holdsALease() {
			return
		}
		parked = true
		deadline := time.Now().Add(matrixWindow)
		for time.Now().Before(deadline) {
			if n := drivenRunCount(p.store); n > 1 {
				probe.record(n)
				return
			}
			time.Sleep(matrixPoll)
		}
	}
}

func (p *watchPeer) unpark() { p.forge.before, p.forge.after = nil, nil }

// ---------------------------------------------------------------------------
// A. One source, two discoverers, one run
// ---------------------------------------------------------------------------

// TestWatchMatrixA_ConcurrentDiscoveryClaimsOneRun: two watchers observe the
// same new opted-in issue at once. The atomic source claim must make "two
// EngineeringRuns for one source" unrepresentable, and the process that LOST
// the claim must adopt the winner's row rather than replace it.
//
// A live foreign owner holds the single run-driving slot throughout, so this is
// about claiming and nothing else: both watchers observe and claim, neither
// drives.
//
// The first phase claims from durable watch state - the observation a completed
// poll leaves behind, and the state two restarted watchers come up into. It is
// the same claim() call the polling branch makes, and reaching it that way is
// what makes the race visible at all: the watch-state compare-and-set a live
// poll performs is a durable write both watchers queue on, and it hides the
// few statements the claim actually races over. The second phase then does the
// live concurrent poll, where the same one-run invariant must still hold.
func TestWatchMatrixA_ConcurrentDiscoveryClaimsOneRun(t *testing.T) {
	m := newWatchMatrix(t)
	// Time moves here on purpose. Under a frozen clock two watchers compute a
	// byte-identical run document, and "whose row is this" - the only thing the
	// claim decides - stops being observable at all.
	m.clock.step = time.Millisecond
	// Several sources, because one claim is one coin toss: the watchers race
	// each of these in turn, so an unsafe claim has six chances to show.
	sources := []int{m.issue, 101, 102, 103, 104, 105}
	for _, issue := range sources {
		optIn(m.forge, issue, time.Unix(1_700_000_000, 0).UTC())
	}
	occupyGlobalSlot(t, m.phase8Fixture)
	first, second := m.pair(t)

	seeded := m.clock.at
	if _, ok, err := first.store.PutWatchState(WatchState{
		Repository:    repoA.String(),
		Cursor:        formatWatchedIssues(sources),
		LastSuccessAt: seeded,
		NotBefore:     seeded.Add(watchPollInterval),
	}, 0); err != nil || !ok {
		t.Fatalf("seeding the watch observation: ok=%v err=%v", ok, err)
	}
	meetBeforeClaiming(first, second)

	reports := concurrently(t, first, second)
	for i, report := range reports {
		if observed := only(t, report); observed.Discovered != len(sources) {
			t.Fatalf("watcher %d observed %d issues, want %d (%q)",
				i, observed.Discovered, len(sources), observed.Detail)
		}
	}
	first.atRuntime, second.atRuntime = nil, nil

	claimed := map[string]bool{}
	for _, issue := range sources {
		runID := oneRunPerSource(t, first.store, first.engine, issue)
		if got := oneRunPerSource(t, second.store, second.engine, issue); got != runID {
			t.Fatalf("issue %d: the watchers hold different run identities: %s and %s", issue, runID, got)
		}
		if claimed[runID] {
			t.Fatalf("two sources resolved to one run %s", runID)
		}
		claimed[runID] = true
		// Each watcher resolves the same LIVE run through its own handle.
		for _, peer := range []*watchPeer{first, second} {
			got, live, _, err := issueRun(peer.engine, issue)
			if err != nil {
				t.Fatalf("%s: %v", peer.name, err)
			}
			if !live || got != runID {
				t.Fatalf("%s resolved %q (live=%v) for issue %d, want the one claimed run %s",
					peer.name, got, live, issue, runID)
			}
		}
		assertClaimedOnce(t, second.store, runID)
	}

	// The live poll, concurrently. Whether a watcher polls or reads the set the
	// other one's poll just recorded is a race the not-before decides, and both
	// answers lead to the same claim.
	optIn(m.forge, watchSecondIssue, time.Unix(1_700_000_000, 0).UTC())
	m.clock.advance(2 * watchPollInterval)
	concurrently(t, first, second)
	polled := oneRunPerSource(t, first.store, first.engine, watchSecondIssue)
	if claimed[polled] {
		t.Fatalf("the polled source resolved to an already claimed run %s", polled)
	}
	assertClaimedOnce(t, second.store, polled)
}

// assertClaimedOnce reads the durable record of a source claim: one genesis
// event, and a run row that still belongs to the process that appended it.
func assertClaimedOnce(t *testing.T, store *SQLiteOperationStore, runID string) {
	t.Helper()
	events := journalFrom(t, store, runID)
	if len(events) == 0 || events[0].Type != EventRunCreated {
		t.Fatalf("the claimed run's journal is %v, want run.created first", journalTypes(events))
	}
	if events[0].ID != runID+"-created" || events[0].Sequence != 1 {
		t.Fatalf("genesis event is %q at sequence %d", events[0].ID, events[0].Sequence)
	}
	if got := countType(events, EventRunCreated); got != 1 {
		t.Fatalf("%d genesis events were appended for one source", got)
	}
	// Only the claim winner wrote the row and hashed its genesis event against
	// it. A loser that replaced the row instead of adopting it leaves a run
	// whose creation instant is not the instant its own genesis records.
	run, ok, err := store.Run(runID)
	if err != nil || !ok {
		t.Fatalf("reading the claimed run: ok=%v err=%v", ok, err)
	}
	if !run.CreatedAt.Equal(events[0].OccurredAt) {
		t.Fatalf("run %s was created at %s but its genesis records %s: the claim loser overwrote the winner",
			runID, run.CreatedAt, events[0].OccurredAt)
	}
	if run.UpdatedAt.After(events[0].OccurredAt) {
		t.Fatalf("run %s was modified at %s, after its genesis at %s", runID, run.UpdatedAt, events[0].OccurredAt)
	}
}

// ---------------------------------------------------------------------------
// B. One run, two drivers, one lease
// ---------------------------------------------------------------------------

// TestWatchMatrixB_ConcurrentDriveTakesOneLease: both watchers drive the same
// run at once. Whichever wins parks inside its first leased operation, so the
// loser's entire tick happens while that lease is durably held. The record must
// show one driver and one of every side effect.
func TestWatchMatrixB_ConcurrentDriveTakesOneLease(t *testing.T) {
	m := newWatchMatrix(t)
	optIn(m.forge, m.issue, time.Unix(1_700_000_000, 0).UTC())
	first, second := m.pair(t)
	probe := &ceilingProbe{}
	first.park(probe)
	second.park(probe)

	concurrently(t, first, second)
	first.unpark()
	second.unpark()

	runID := oneRunPerSource(t, second.store, second.engine, m.issue)
	events := journalFrom(t, second.store, runID)
	owners := leaseOwners(t, events)
	if len(owners) == 0 {
		t.Fatalf("neither watcher drove the run: %v", journalTypes(events))
	}
	for _, owner := range owners {
		if owner != owners[0] {
			t.Fatalf("two watchers held operations of one run: %v", owners)
		}
	}
	if owners[0] != first.owner && owners[0] != second.owner {
		t.Fatalf("operations were driven by an unknown owner %q", owners[0])
	}
	if peak := probe.peak(); peak != 1 {
		t.Fatalf("%d runs were being driven at once, want exactly 1", peak)
	}
	succeededOnce(t, events)

	// One of every side effect: one genesis, one commit, one execution, one
	// push on the REAL remote, one pull request on the forge.
	if got := countType(events, EventRunCreated); got != 1 {
		t.Fatalf("%d genesis events", got)
	}
	if got := countType(events, EventCandidateCommitted); got != 1 {
		t.Fatalf("%d candidate commits: %v", got, journalTypes(events))
	}
	if got := len(first.provider.requests) + len(second.provider.requests); got != 1 {
		t.Fatalf("the execution provider was invoked %d times", got)
	}
	if got := countGitPushes(t, m.phase8Fixture, runID); got != 1 {
		t.Fatalf("the remote candidate branch received %d pushes", got)
	}
	if got := countMethod(m.forge.Calls, "CreatePullRequest"); got != 1 {
		t.Fatalf("%d pull requests were created", got)
	}
	if got := len(m.forge.PullRequests); got != 1 {
		t.Fatalf("the forge holds %d pull requests", got)
	}
}

// ---------------------------------------------------------------------------
// C. Two eligible runs, one slot
// ---------------------------------------------------------------------------

// TestWatchMatrixC_TwoEligibleRunsShareOneGlobalSlot: two opted-in issues, two
// durable runs, and a global ceiling of one. Both watchers go for both runs at
// once, and whichever takes the first slot PARKS in it, so the other has a real
// window in which to take a second one. The store must never show two runs
// holding a run-driving slot.
//
// The runs are claimed by a preceding tick that could drive nothing, so the
// concurrent phase is purely about capacity: both watchers read the same
// remembered opted-in set out of durable watch state and neither one's poll can
// change what the other sees.
func TestWatchMatrixC_TwoEligibleRunsShareOneGlobalSlot(t *testing.T) {
	m := newWatchMatrix(t)
	at := time.Unix(1_700_000_000, 0).UTC()
	optIn(m.forge, m.issue, at)
	optIn(m.forge, watchSecondIssue, at)
	first, second := m.pair(t)

	// A live foreign owner holds the only slot, so this tick claims both
	// sources and drives neither.
	occupyGlobalSlot(t, m.phase8Fixture)
	if observed := only(t, tickOf(t, first)); observed.Discovered != 2 {
		t.Fatalf("the claiming tick discovered %d issues, want 2 (%q)", observed.Discovered, observed.Detail)
	}
	runA := oneRunPerSource(t, first.store, first.engine, m.issue)
	runB := oneRunPerSource(t, second.store, second.engine, watchSecondIssue)
	if runA == runB {
		t.Fatalf("two sources resolved to one run %s", runA)
	}
	for _, runID := range []string{runA, runB} {
		if owners := leaseOwners(t, journalFrom(t, first.store, runID)); len(owners) != 0 {
			t.Fatalf("run %s was driven while the slot was taken: %v", runID, owners)
		}
	}
	releaseGlobalSlot(t, m.phase8Fixture)

	probe := &ceilingProbe{}
	first.park(probe)
	second.park(probe)
	concurrently(t, first, second)
	first.unpark()
	second.unpark()

	if peak := probe.peak(); peak != 1 {
		t.Fatalf("%d runs held a run-driving slot at once, want exactly 1", peak)
	}
	// Delayed, never starved: with one slot, both runs are still driven.
	for _, runID := range []string{runA, runB} {
		if owners := leaseOwners(t, journalFrom(t, first.store, runID)); len(owners) == 0 {
			t.Fatalf("run %s was starved rather than delayed", runID)
		}
		succeededOnce(t, journalFrom(t, second.store, runID))
	}
}

// ---------------------------------------------------------------------------
// D. Expiry is not death
// ---------------------------------------------------------------------------

// TestWatchMatrixD_ALiveWatchersExpiredLeaseIsNotStolen: the first watcher's
// process is a REAL child holding a REAL ownership lock. Its heartbeat is long
// gone; the process is not. Wall time alone must steal nothing.
func TestWatchMatrixD_ALiveWatchersExpiredLeaseIsNotStolen(t *testing.T) {
	m := newWatchMatrix(t)
	optIn(m.forge, m.issue, time.Unix(1_700_000_000, 0).UTC())
	holder := startLockHolder(t, m.stateDir, "")
	first := m.peer(t, "first", holder.owner)
	second := m.peer(t, "second", "")

	// The first watcher stops mid-operation, leaving its lease behind.
	first.crashOn("Issue")
	first.stops(t)
	runID := oneRunPerSource(t, second.store, second.engine, m.issue)
	stalled := heldLease(t, second.store, runID)
	if stalled.Lease.Owner != holder.owner {
		t.Fatalf("the stalled lease belongs to %q, want the first watcher %q", stalled.Lease.Owner, holder.owner)
	}

	m.clock.advance(matrixExpiry)
	if !m.clock.at.After(stalled.Lease.ExpiresAt) {
		t.Fatalf("the lease has not expired: now %s, expires %s", m.clock.at, stalled.Lease.ExpiresAt)
	}
	if !NewLockOwnerLiveness(m.stateDir).Alive(holder.owner) {
		t.Fatal("a process holding its ownership lock must be reported alive")
	}

	// The successor's own scheduler must refuse the operation, and its watcher
	// must not drive the run at all.
	if got, err := second.engine.scheduler.Next(runID); err != nil || got != nil {
		t.Fatalf("the expired lease of a LIVE watcher was acquired: %v %v", got, err)
	}
	report := only(t, tickOf(t, second))
	if len(report.Driven) != 0 {
		t.Fatalf("the second watcher drove %v while the first still owns the run", report.Driven)
	}
	after := heldLease(t, second.store, runID)
	if after.Lease.Owner != holder.owner || after.Attempt != stalled.Attempt {
		t.Fatalf("the lease was taken over: %+v (was owner %q attempt %d)", after.Lease, holder.owner, stalled.Attempt)
	}
	if owners := leaseOwners(t, journalFrom(t, second.store, runID)); len(owners) != 1 || owners[0] != holder.owner {
		t.Fatalf("the journal records drivers %v, want the first watcher alone", owners)
	}
}

// ---------------------------------------------------------------------------
// E. Death plus expiry is a takeover
// ---------------------------------------------------------------------------

// TestWatchMatrixE_ADeadWatchersRunIsReclaimed: the same setup, except the
// holder is killed. SIGKILL runs no cleanup, so only the kernel releases the
// ownership lock - which is exactly what makes the abandoned run reclaimable.
func TestWatchMatrixE_ADeadWatchersRunIsReclaimed(t *testing.T) {
	m := newWatchMatrix(t)
	optIn(m.forge, m.issue, time.Unix(1_700_000_000, 0).UTC())
	holder := startLockHolder(t, m.stateDir, "")
	first := m.peer(t, "first", holder.owner)
	second := m.peer(t, "second", "")

	first.crashOn("Issue")
	first.stops(t)
	runID := oneRunPerSource(t, second.store, second.engine, m.issue)
	abandoned := heldLease(t, second.store, runID)
	if abandoned.Lease.Owner != holder.owner {
		t.Fatalf("the abandoned lease belongs to %q", abandoned.Lease.Owner)
	}

	holder.kill(t)
	if NewLockOwnerLiveness(m.stateDir).Alive(holder.owner) {
		t.Fatal("a killed watcher must not be reported alive")
	}
	// Death alone is still not enough: the lease has to be expired too.
	if CanAcquire(abandoned, m.clock.at, false) {
		t.Fatal("an unexpired lease of a dead watcher was acquirable")
	}
	m.clock.advance(matrixExpiry)

	report := only(t, tickOf(t, second))
	if !containsRun(report.Driven, runID) {
		t.Fatalf("the successor drove %v, want %s (%q)", report.Driven, runID, report.Detail)
	}
	events := journalFrom(t, second.store, runID)
	owners := leaseOwners(t, events)
	if len(owners) < 2 {
		t.Fatalf("the run was not reclaimed: drivers %v, journal %v", owners, journalTypes(events))
	}
	if owners[0] != holder.owner {
		t.Fatalf("the first attempt belongs to %q, want the dead watcher", owners[0])
	}
	for _, owner := range owners[1:] {
		if owner != second.owner {
			t.Fatalf("an attempt after the takeover belongs to %q, want the successor %q", owner, second.owner)
		}
	}
	// Reclaiming is resuming: no second run, no second genesis.
	if got := oneRunPerSource(t, first.store, first.engine, m.issue); got != runID {
		t.Fatalf("the takeover produced run %s, want %s", got, runID)
	}
	if got := countType(events, EventRunCreated); got != 1 {
		t.Fatalf("%d genesis events after the takeover", got)
	}
	if got := countType(events, EventContractCompiled); got == 0 {
		t.Fatalf("the reclaimed run made no progress: %v", journalTypes(events))
	}
}

// ---------------------------------------------------------------------------
// F. A crash right after the source claim
// ---------------------------------------------------------------------------

// TestWatchMatrixF_CrashAfterTheSourceClaimResumesTheSameRun: the first
// watcher's process stops the instant its claim is durable - discovery
// answered, the run created, nothing driven. The second watcher must resume
// that run rather than claim a second one for the same source.
func TestWatchMatrixF_CrashAfterTheSourceClaimResumesTheSameRun(t *testing.T) {
	m := newWatchMatrix(t)
	optIn(m.forge, m.issue, time.Unix(1_700_000_000, 0).UTC())
	first, second := m.pair(t)

	// Cancelling at the discovery call is precisely "stopped after the claim":
	// claiming makes no forge call and holds no capacity, and driving is the
	// first thing that checks the context again.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first.forge.before = func(call string) {
		if call == "DiscoverIssues" {
			cancel()
		}
	}
	if _, err := first.watcher.Tick(ctx); err != nil {
		t.Fatalf("first: Tick: %v", err)
	}
	first.unpark()

	runID := oneRunPerSource(t, second.store, second.engine, m.issue)
	claimed := journalFrom(t, second.store, runID)
	if len(claimed) != 1 || claimed[0].Type != EventRunCreated {
		t.Fatalf("the crashed watcher left %v, want the claim alone", journalTypes(claimed))
	}

	report := only(t, tickOf(t, second))
	if !containsRun(report.Driven, runID) {
		t.Fatalf("the second watcher drove %v, want %s (%q)", report.Driven, runID, report.Detail)
	}
	// Read back through the FIRST watcher's handle: the resumed run is the
	// claimed one, whichever process asks.
	if got := oneRunPerSource(t, first.store, first.engine, m.issue); got != runID {
		t.Fatalf("resuming produced run %s, want %s", got, runID)
	}
	events := journalFrom(t, first.store, runID)
	if got := countType(events, EventRunCreated); got != 1 {
		t.Fatalf("%d genesis events for one source", got)
	}
	if events[0].ID != runID+"-created" {
		t.Fatalf("genesis event is %q", events[0].ID)
	}
	if countType(events, EventContractCompiled) == 0 {
		t.Fatalf("the resumed run made no progress: %v", journalTypes(events))
	}
	succeededOnce(t, events)
	if got := len(first.provider.requests); got != 0 {
		t.Fatalf("the crashed watcher invoked the provider %d times", got)
	}
}

// ---------------------------------------------------------------------------
// G. A crash between a GitHub observation and its record
// ---------------------------------------------------------------------------

// TestWatchMatrixG_CrashAfterPublicationPublishesOnce: the first watcher
// observes the forge for an existing pull request, finds none, publishes, and
// its process stops before one byte of that publication is recorded. The
// successor must rediscover the pull request the dead watcher created and
// publish nothing.
func TestWatchMatrixG_CrashAfterPublicationPublishesOnce(t *testing.T) {
	m := newWatchMatrix(t)
	optIn(m.forge, m.issue, time.Unix(1_700_000_000, 0).UTC())
	holder := startLockHolder(t, m.stateDir, "")
	first := m.peer(t, "first", holder.owner)
	second := m.peer(t, "second", "")

	first.crashAfter("CreatePullRequest")
	first.stops(t)

	runID := oneRunPerSource(t, second.store, second.engine, m.issue)
	crashed := journalFrom(t, second.store, runID)
	if got := countMethod(m.forge.Calls, "FindPullRequests"); got == 0 {
		t.Fatalf("the crashed watcher never observed the forge: %v", m.forge.Methods())
	}
	if got := countMethod(m.forge.Calls, "CreatePullRequest"); got != 1 {
		t.Fatalf("the crashed watcher created %d pull requests", got)
	}
	if got := countType(crashed, EventGitHubPRObserved); got != 0 {
		t.Fatalf("the publication was recorded %d times before the crash", got)
	}
	// The dead watcher still owns the publication operation, so the successor
	// can only proceed by proving that owner gone.
	abandoned := heldLease(t, second.store, runID)
	if abandoned.Kind != OpPullRequestCreate || abandoned.Lease.Owner != holder.owner {
		t.Fatalf("the crash left lease %+v on %q, want the publication owned by the first watcher",
			abandoned.Lease, abandoned.Kind)
	}
	pushes := countGitPushes(t, m.phase8Fixture, runID)

	holder.kill(t)
	m.clock.advance(matrixExpiry)
	tickOf(t, second)

	events := journalFrom(t, second.store, runID)
	if got := countMethod(m.forge.Calls, "CreatePullRequest"); got != 1 {
		t.Fatalf("%d pull requests were created in total", got)
	}
	if got := len(m.forge.PullRequests); got != 1 {
		t.Fatalf("the forge holds %d pull requests", got)
	}
	if got := countGitPushes(t, m.phase8Fixture, runID); got != pushes {
		t.Fatalf("the remote received %d extra pushes", got-pushes)
	}
	if got := len(first.provider.requests) + len(second.provider.requests); got != 1 {
		t.Fatalf("the execution provider was invoked %d times", got)
	}
	if got := countType(events, EventGitHubPRObserved); got == 0 {
		t.Fatalf("the successor never adopted the existing publication: %v", journalTypes(events))
	}
	owners := leaseOwners(t, events)
	if owners[len(owners)-1] != second.owner {
		t.Fatalf("the last attempt belongs to %q, want the successor %q", owners[len(owners)-1], second.owner)
	}
	succeededOnce(t, events)
	snapshot, err := second.store.Replay(runID)
	if err != nil {
		t.Fatal(err)
	}
	published, err := second.engine.load(runID)
	if err != nil {
		t.Fatal(err)
	}
	if published.projection.PullRequest == nil {
		t.Fatalf("the run is bound to no pull request: %v", journalTypes(events))
	}
	if _, ok := m.forge.PullRequests[published.projection.PullRequest.Number]; !ok {
		t.Fatalf("the run is bound to pull request %d, which the forge does not hold",
			published.projection.PullRequest.Number)
	}
	if terminalDisposition(snapshot.Disposition) && snapshot.Disposition != Completed {
		t.Fatalf("the resumed run settled %s: %s", snapshot.Disposition, snapshot.Reason)
	}
	if got := oneRunPerSource(t, first.store, first.engine, m.issue); got != runID {
		t.Fatalf("the resumed publication produced run %s, want %s", got, runID)
	}
}
