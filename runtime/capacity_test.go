package runtime

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
)

// TestEveryOperationKindIsClassifiedDeliberately is #85's fail-closed rule.
// Every Op* constant in production code must have an explicit entry in the one
// vocabulary, the vocabulary may name nothing else, exactly the two read-only
// kinds are observation, and an unknown kind is work.
func TestEveryOperationKindIsClassifiedDeliberately(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	declared := map[string]bool{}
	fset := token.NewFileSet()
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				for i, name := range value.Names {
					if len(name.Name) < 3 || !strings.HasPrefix(name.Name, "Op") || !unicode.IsUpper(rune(name.Name[2])) || i >= len(value.Values) {
						continue
					}
					literal, ok := value.Values[i].(*ast.BasicLit)
					if !ok || literal.Kind != token.STRING {
						continue
					}
					kind, err := strconv.Unquote(literal.Value)
					if err != nil {
						t.Fatal(err)
					}
					declared[kind] = true
				}
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("found no Op* constants; the enumeration is broken")
	}
	for kind := range declared {
		if _, ok := operationCapacityClasses[kind]; !ok {
			t.Errorf("operation kind %q has no deliberate capacity class", kind)
		}
	}
	for kind := range operationCapacityClasses {
		if !declared[kind] {
			t.Errorf("the vocabulary classifies %q, which is not a declared operation kind", kind)
		}
	}
	for _, spec := range operationSpecs {
		if _, ok := operationCapacityClasses[spec.kind]; !ok {
			t.Errorf("planned kind %q has no deliberate capacity class", spec.kind)
		}
	}
	if got := observationKindList(); !reflect.DeepEqual(got, []string{OpGitHubObserve, OpSourceObserve}) {
		t.Fatalf("observation kinds = %v, want exactly github.observe and source.observe", got)
	}
	for _, unknown := range []string{"", "external.work", "k", "github.observe.v2", "assurance.future"} {
		if OperationCapacityClass(unknown) != CapacityWork {
			t.Fatalf("unknown kind %q was not classified as work", unknown)
		}
	}
}

// capacityPair is two runs' operations planned on one store, with a scheduler
// per run bound to the given ceilings.
func capacityScheduler(store OperationStore, owner string, runs, observations int) Scheduler {
	return Scheduler{
		Store: store, Clock: &fakeClock{now: time.Unix(100, 0)}, Owner: owner, LeaseDuration: time.Minute,
		Liveness: alwaysAlive(), MaxConcurrentRuns: runs, MaxConcurrentObservations: observations,
	}
}

func planKind(t *testing.T, s Scheduler, runID, kind string) RunOperation {
	t.Helper()
	op, created, err := s.Plan(RunOperation{RunID: runID, Kind: kind, IdempotencyKey: runID + kind})
	if err != nil || !created {
		t.Fatalf("plan %s %s: %v %v", runID, kind, created, err)
	}
	return op
}

func mustNext(t *testing.T, s Scheduler, runID string) *RunOperation {
	t.Helper()
	op, err := s.Next(runID)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

// TestTheObservationCeilingBoundsWaitingRuns: with max_concurrent_observations
// of one, two waiting runs never observe at once, and the second observes as
// soon as the first finishes. A work slot held elsewhere changes none of it.
func TestTheObservationCeilingBoundsWaitingRuns(t *testing.T) {
	store, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := capacityScheduler(store, "owner", 1, 1)
	planKind(t, s, "worker", OpExecutionInvoke)
	if mustNext(t, s, "worker") == nil {
		t.Fatal("the work slot was not granted")
	}
	planKind(t, s, "waiting-a", OpGitHubObserve)
	planKind(t, s, "waiting-b", OpGitHubObserve)
	first := mustNext(t, s, "waiting-a")
	if first == nil {
		t.Fatal("an observation was refused while only WORK capacity was full")
	}
	if second := mustNext(t, s, "waiting-b"); second != nil {
		t.Fatal("two runs observed at once under max_concurrent_observations=1")
	}
	if _, err := s.Finish(first.ID, Succeeded); err != nil {
		t.Fatal(err)
	}
	if mustNext(t, s, "waiting-b") == nil {
		t.Fatal("the freed observation slot was not handed on")
	}
}

// TestTheKindColumnDoesNotDecideCapacity is amendment C: the denormalized
// kind column is not verified against the document, so rewriting ONLY the
// column of an active work operation to github.observe must not free the work
// slot. Classifying on the bare column fails this test.
func TestTheKindColumnDoesNotDecideCapacity(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	s := capacityScheduler(store, "owner", 1, 2)
	held := planKind(t, s, "run-a", OpExecutionInvoke)
	if mustNext(t, s, "run-a") == nil {
		t.Fatal("the work slot was not granted")
	}
	result, err := rawJournalDB(t, dir).Exec(`UPDATE run_operations SET kind = ? WHERE id = ?`, OpGitHubObserve, held.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		t.Fatalf("corrupted %d rows, want 1", n)
	}
	planKind(t, s, "run-b", OpExecutionInvoke)
	if mustNext(t, s, "run-b") != nil {
		t.Fatal("rewriting the kind column of an active work operation freed the work slot")
	}
}

// TestCapacityStateSurvivesARestart: the classification, the per-run counts
// and the acquisition decision are rebuilt from durable rows alone.
func TestCapacityStateSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	s := capacityScheduler(store, "owner", 1, 1)
	planKind(t, s, "working", OpExecutionInvoke)
	planKind(t, s, "observing", OpSourceObserve)
	planKind(t, s, "runnable", OpCandidateCommit)
	for _, run := range []string{"working", "observing"} {
		if mustNext(t, s, run) == nil {
			t.Fatalf("%s was not granted its slot", run)
		}
	}
	now := time.Unix(100, 0)
	states := func(store *SQLiteOperationStore) map[string]CapacityClass {
		ops, err := store.AllOperations()
		if err != nil {
			t.Fatal(err)
		}
		byRun := map[string]map[string]RunOperation{"waiting": nil}
		for _, op := range ops {
			if byRun[op.RunID] == nil {
				byRun[op.RunID] = map[string]RunOperation{}
			}
			byRun[op.RunID][op.ID] = op
		}
		out := map[string]CapacityClass{}
		for run, ops := range byRun {
			out[run] = capacityState(ops, now)
		}
		return out
	}
	want := map[string]CapacityClass{
		"working": CapacityWork, "observing": CapacityObservation,
		"runnable": capacityRunnable, "waiting": capacityWaiting,
	}
	if got := states(store); !reflect.DeepEqual(got, want) {
		t.Fatalf("before restart: %v, want %v", got, want)
	}
	store.Close()
	reopened := reopenStore(t, dir)
	if got := states(reopened); !reflect.DeepEqual(got, want) {
		t.Fatalf("after restart: %v, want %v", got, want)
	}
	s = capacityScheduler(reopened, "owner", 1, 1)
	if mustNext(t, s, "runnable") != nil {
		t.Fatal("a reopened store forgot the work slot is held")
	}
}

// heldProvider holds the first execution in one candidate directory open
// until released: the deterministic stand-in for a provider that runs for half
// an hour. Every other execution passes straight through.
type heldProvider struct {
	*isolatedProvider
	dir     string
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *heldProvider) Execute(ctx context.Context, request ExecutionRequest) (ExecutionResult, error) {
	if request.CandidateDir == p.dir {
		held := false
		p.once.Do(func() { held = true; close(p.entered) })
		if held {
			<-p.release
		}
	}
	return p.isolatedProvider.Execute(ctx, request)
}

// TestEngineAWaitingRunObservesWhileAnotherHoldsTheOnlyWorkSlot is #85's
// decisive case, driven through the engine every supervisor turn calls
// (Reconcile), so B's pass is deterministic rather than timed. max_concurrent_runs is one. Run A holds a long execution.invoke. Run B
// is waiting on review: during A's operation it performs github.observe,
// discovers the requested change and becomes Runnable - and it does NOT
// acquire work until A releases the slot. Then it does.
//
// Counting observation against max_concurrent_runs in AcquireOperation, or
// any class-blind early exit in front of it, fails this test: B then never
// observes the review while A works.
func TestEngineAWaitingRunObservesWhileAnotherHoldsTheOnlyWorkSlot(t *testing.T) {
	fixture := newPhase8Fixture(t)
	fixture.distinctMutations()
	fixture.trackPullRequestHeads()
	// Every owner is alive, so A's lease cannot be reclaimed as abandoned by
	// the stepping clock: the slot is held for exactly as long as A holds it.
	fixture.deps.Liveness = alwaysAlive()
	gate := &heldProvider{isolatedProvider: fixture.provider, entered: make(chan struct{}), release: make(chan struct{})}
	fixture.deps.Provider = gate
	fixture.runtime = fixture.newRuntime(fixture.deps)
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(gate.release) }) }
	t.Cleanup(release)

	// B publishes and parks on review.
	b := fixture.start()
	fixture.reconcile(b)
	published := fixture.state(b)
	if published.projection.PullRequest == nil || published.snapshot.Disposition != Waiting {
		t.Fatalf("run B did not park on review: %s %v", published.snapshot.Disposition, journalTypes(published.events))
	}

	fixture.issue = phase8Issue + 1
	fixture.forge.Issues[fixture.issue] = GitHubIssue{
		Number: fixture.issue, URL: "https://github.com/acme/repo/issues/43",
		Title: "a long task", Body: "body", State: GitHubOpen, UpdatedAt: fixture.clock.Now(),
	}
	a := fixture.start()
	gate.dir = candidateDir(fixture.stateDir, a)
	aDone := make(chan error, 1)
	go func() {
		_, err := fixture.runtime.Reconcile(context.Background(), a)
		aDone <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(60 * time.Second):
		t.Fatal("run A never reached its execution.invoke")
	}

	// A reviewer requests changes on B's head while A is executing.
	head := published.projection.CandidateRevision
	fixture.forge.ReviewsByHead[head] = GitHubReviewObservation{Reviews: []GitHubReview{{
		ID: 1, Author: GitHubActor{Login: "reviewer", ID: 9},
		State: GitHubReviewChangesRequested, Body: "rename the helper", CommitSHA: head,
	}}}
	invocations := len(fixture.provider.requests)
	fixture.reconcile(b)

	observed := fixture.state(b)
	if review := observed.projection.Review; review == nil || review.State != string(GitHubReviewChangesRequested) {
		t.Fatalf("run B did not observe its review while A held the only work slot: %v", journalTypes(observed.events))
	}
	if len(fixture.provider.requests) != invocations {
		t.Fatal("run B executed work while A held the only work slot")
	}
	fleet, err := FleetStatus(fixture.store, fixture.stateDir, 1, 2, fixture.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Working != 1 || fleet.Runnable != 1 || fleet.Observing != 0 || fleet.Waiting != 0 || fleet.Unavailable != 0 {
		t.Fatalf("counts = working %d observing %d runnable %d waiting %d unavailable %d, want A working and B runnable",
			fleet.Working, fleet.Observing, fleet.Runnable, fleet.Waiting, fleet.Unavailable)
	}
	if sum := fleet.Working + fleet.Observing + fleet.Runnable + fleet.Waiting + fleet.Unavailable; sum != fleet.Active {
		t.Fatalf("the counts sum to %d over %d nonterminal runs", sum, fleet.Active)
	}

	release()
	select {
	case err := <-aDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(120 * time.Second):
		t.Fatal("run A never finished after its slot was released")
	}
	before := len(fixture.provider.requests)
	fixture.reconcile(b)
	if len(fixture.provider.requests) <= before {
		t.Fatal("run B did not acquire work once A released the slot")
	}
	remediation := fixture.provider.requests[before]
	if remediation.Purpose != InvocationRemediation || remediation.CandidateDir != candidateDir(fixture.stateDir, b) {
		t.Fatalf("B's next invocation = %q in %s, want its review remediation", remediation.Purpose, remediation.CandidateDir)
	}
}

// TestARunHoldsAtMostOneActiveOperation is frozen invariant 4 made durable:
// while one driver holds X's execution.invoke, no driver - not even with every
// ceiling free - may lease X's github.observe beside it. Both stores.
func TestARunHoldsAtMostOneActiveOperation(t *testing.T) {
	sqlite, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close() })
	for name, store := range map[string]OperationStore{"sqlite": sqlite, "memory": NewMemoryOperationStore()} {
		first := capacityScheduler(store, "w1", 5, 5)
		second := capacityScheduler(store, "w2", 5, 5)
		planKind(t, first, "x", OpExecutionInvoke)
		if mustNext(t, first, "x") == nil {
			t.Fatalf("%s: W1 was not granted X's work", name)
		}
		planKind(t, second, "x", OpGitHubObserve)
		if got := mustNext(t, second, "x"); got != nil {
			t.Fatalf("%s: W2 leased X's %s while W1 holds X's execution.invoke", name, got.Kind)
		}
	}
}

// TestTheTwoCeilingsAreIndependent pins that each class is measured against
// ITS OWN ceiling: with one work slot and two observation slots, two runs
// observe at once and a third does not. Equal ceilings would hide a swap.
func TestTheTwoCeilingsAreIndependent(t *testing.T) {
	sqlite, err := OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlite.Close() })
	for name, store := range map[string]OperationStore{"sqlite": sqlite, "memory": NewMemoryOperationStore()} {
		s := capacityScheduler(store, "owner", 1, 2)
		planKind(t, s, "worker", OpExecutionInvoke)
		if mustNext(t, s, "worker") == nil {
			t.Fatalf("%s: the work slot was not granted", name)
		}
		for _, run := range []string{"observer-a", "observer-b"} {
			planKind(t, s, run, OpSourceObserve)
			if mustNext(t, s, run) == nil {
				t.Fatalf("%s: %s was refused one of two observation slots", name, run)
			}
		}
		planKind(t, s, "observer-c", OpSourceObserve)
		if mustNext(t, s, "observer-c") != nil {
			t.Fatalf("%s: a third run observed under an observation ceiling of two", name)
		}
		planKind(t, s, "second-worker", OpCandidateCommit)
		if mustNext(t, s, "second-worker") != nil {
			t.Fatalf("%s: a second run worked under a work ceiling of one", name)
		}
	}
}

func TestTheDefaultObservationCeilingIsTwo(t *testing.T) {
	if DefaultMaxConcurrentObservations != 2 || resolveMaxConcurrentObservations(0) != 2 {
		t.Fatalf("the frozen default observation ceiling is 2, got %d", resolveMaxConcurrentObservations(0))
	}
}

// TestAnUnreadableRunIsUnavailableNotWaiting: a nonterminal run whose journal
// cannot be replayed is counted Unavailable and never folded into another
// count.
func TestAnUnreadableRunIsUnavailableNotWaiting(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Unix(100, 0).UTC()
	if err := store.PutRun(EngineeringRun{SchemaVersion: SchemaVersion, ID: "r", Repository: "example/repo", Phase: Execute, Disposition: Active, CreatedAt: at, UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []EngineeringEvent{{ID: "created", Type: EventRunCreated}, {ID: "waiting", Type: EventRunWaiting, Payload: []byte(`{"reason":"external_review"}`)}} {
		e.SchemaVersion, e.RunID, e.OccurredAt = SchemaVersion, "r", at
		if _, err := store.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := rawJournalDB(t, dir).Exec(`UPDATE events SET event_hash = ? WHERE run_id = 'r' AND sequence = 2`, strings.Repeat("0", 64)); err != nil {
		t.Fatal(err)
	}
	fleet, err := FleetStatus(store, dir, 1, 2, at)
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Runs[0].Error == "" {
		t.Fatal("fixture: the tampered journal must not replay")
	}
	if fleet.Unavailable != 1 || fleet.Waiting != 0 || fleet.Active != 1 {
		t.Fatalf("unavailable %d waiting %d active %d, want the unreadable run counted as unavailable", fleet.Unavailable, fleet.Waiting, fleet.Active)
	}
}
