package runtime

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Clock is injected so scheduling, expiry, and budgets are deterministic in tests.
type Clock interface{ Now() time.Time }
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now().UTC() }

// OperationStore is deliberately small, but its write is compare-and-set: a
// durable implementation must be able to refuse a write that raced another
// process without a shared in-process lock.
type OperationStore interface {
	Operations(runID string) ([]RunOperation, error)
	AllOperations() ([]RunOperation, error)
	// Operation returns the stored operation and the row revision observed with it.
	Operation(id string) (RunOperation, int64, bool, error)
	// OperationByIdempotencyKey resolves the durable operation for a run-scoped key.
	OperationByIdempotencyKey(runID, key string) (RunOperation, bool, error)
	// PutOperation writes op only when the stored row revision still equals
	// expected; expected 0 means the row must not yet exist. It reports false
	// when another writer won the race, without modifying stored state.
	PutOperation(op RunOperation, expected int64) (int64, bool, error)
	// AcquireOperation is PutOperation guarded by the global run ceiling: it
	// writes op only when the stored revision still equals expected AND fewer
	// than maxRuns OTHER runs currently hold a leased or running operation.
	// The count and the write must be one durable statement - counting first
	// and writing second is the read-then-act race two watcher processes both
	// win, which is the whole reason this is not just PutOperation.
	AcquireOperation(op RunOperation, expected int64, maxRuns int) (int64, bool, error)
}

// MemoryOperationStore is the in-process test double for OperationStore. It
// holds revisions beside operations so it enforces the same CAS contract the
// durable store does; it provides no cross-process guarantee.
type MemoryOperationStore struct {
	mu         sync.Mutex
	operations map[string]RunOperation
	revisions  map[string]int64
}

func NewMemoryOperationStore() *MemoryOperationStore {
	return &MemoryOperationStore{operations: map[string]RunOperation{}, revisions: map[string]int64{}}
}

// copyOperation detaches the mutable pointer fields so a caller that mutates a
// returned operation cannot alter stored state before winning its CAS.
func copyOperation(op RunOperation) RunOperation {
	if op.Lease != nil {
		lease := *op.Lease
		op.Lease = &lease
	}
	if op.StartedAt != nil {
		started := *op.StartedAt
		op.StartedAt = &started
	}
	if op.LastProgressAt != nil {
		progress := *op.LastProgressAt
		op.LastProgressAt = &progress
	}
	return op
}
func (s *MemoryOperationStore) Operations(runID string) ([]RunOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []RunOperation{}
	for _, op := range s.operations {
		if op.RunID == runID {
			out = append(out, copyOperation(op))
		}
	}
	return sortOperations(out), nil
}
func (s *MemoryOperationStore) AllOperations() ([]RunOperation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RunOperation, 0, len(s.operations))
	for _, op := range s.operations {
		out = append(out, copyOperation(op))
	}
	return sortOperations(out), nil
}
func (s *MemoryOperationStore) Operation(id string) (RunOperation, int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[id]
	return copyOperation(op), s.revisions[id], ok, nil
}
func (s *MemoryOperationStore) OperationByIdempotencyKey(runID, key string) (RunOperation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, op := range s.operations {
		if op.RunID == runID && op.IdempotencyKey == key {
			return copyOperation(op), true, nil
		}
	}
	return RunOperation{}, false, nil
}
func (s *MemoryOperationStore) PutOperation(op RunOperation, expected int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision, exists := s.revisions[op.ID]
	if expected == 0 {
		if exists {
			return 0, false, nil
		}
		// Mirrors the durable UNIQUE(run_id, idempotency_key) constraint.
		for _, prior := range s.operations {
			if prior.RunID == op.RunID && prior.IdempotencyKey == op.IdempotencyKey {
				return 0, false, nil
			}
		}
	} else if !exists || revision != expected {
		return 0, false, nil
	}
	s.operations[op.ID] = copyOperation(op)
	s.revisions[op.ID] = revision + 1
	return revision + 1, true, nil
}

// AcquireOperation mirrors the durable guard so the double keeps the same
// contract. Its atomicity comes from a process-local mutex, which is exactly
// why it is a test double and never the proof of anything cross-process.
func (s *MemoryOperationStore) AcquireOperation(op RunOperation, expected int64, maxRuns int) (int64, bool, error) {
	if op.ID == "" || expected <= 0 {
		return 0, false, fmt.Errorf("acquiring an operation needs its id and the revision it was read at")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision, exists := s.revisions[op.ID]; !exists || revision != expected {
		return 0, false, nil
	}
	driven := map[string]bool{}
	for _, stored := range s.operations {
		if stored.RunID != op.RunID && (stored.State == Leased || stored.State == Running) {
			driven[stored.RunID] = true
		}
	}
	if len(driven) >= maxRuns {
		return 0, false, nil
	}
	s.operations[op.ID] = copyOperation(op)
	s.revisions[op.ID] = expected + 1
	return expected + 1, true, nil
}

// sortOperations is the single queue order shared by every store: creation time
// first, then ID, so eligibility scanning is deterministic across processes.
func sortOperations(operations []RunOperation) []RunOperation {
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].CreatedAt.Equal(operations[j].CreatedAt) {
			return operations[i].ID < operations[j].ID
		}
		return operations[i].CreatedAt.Before(operations[j].CreatedAt)
	})
	return operations
}

// OwnerLiveness distinguishes an expired but healthy owner from a crash. A
// replacement scheduler must not steal a healthy owner's operation.
type OwnerLiveness interface{ Alive(owner string) bool }
type OwnerLivenessFunc func(string) bool

func (f OwnerLivenessFunc) Alive(owner string) bool { return f(owner) }

type Scheduler struct {
	Store             OperationStore
	Clock             Clock
	Owner             string
	Liveness          OwnerLiveness
	LeaseDuration     time.Duration
	MaxConcurrentRuns int
}

func (s Scheduler) defaults() Scheduler {
	if s.Clock == nil {
		s.Clock = RealClock{}
	}
	if s.Liveness == nil {
		// An absent liveness source must never read as "the owner is dead":
		// that would let wall-clock expiry alone authorize a takeover. The
		// process liveness policy is conservative and refuses to steal a lease
		// it cannot prove is abandoned.
		s.Liveness = NewProcessOwnerLiveness()
	}
	if s.LeaseDuration <= 0 {
		s.LeaseDuration = time.Minute
	}
	if s.MaxConcurrentRuns <= 0 {
		s.MaxConcurrentRuns = defaultMaxConcurrentRuns
	}
	return s
}

// defaultMaxConcurrentRuns is the M0 global ceiling on concurrently driven
// runs. It is a constant rather than a setting because raising it is an
// operator decision; there is no path from repository configuration to it.
const defaultMaxConcurrentRuns = 1

// resolveMaxConcurrentRuns applies the ceiling rule in one place: the operator
// ceiling defaults to one, and a requested value - from the CLI or from
// repository configuration, which are indistinguishable here on purpose - may
// only LOWER it. Raising the ceiling takes the separate operator-authorized
// value, which nothing in a repository can set.
func resolveMaxConcurrentRuns(requested, operatorAuthorized int) int {
	ceiling := operatorAuthorized
	if ceiling <= 0 {
		ceiling = defaultMaxConcurrentRuns
	}
	if requested <= 0 || requested > ceiling {
		return ceiling
	}
	return requested
}

// Plan is idempotent by key across processes: the durable unique constraint,
// not a prior read, decides which planner created the operation.
func (s Scheduler) Plan(op RunOperation) (RunOperation, bool, error) {
	s = s.defaults()
	if s.Store == nil || op.RunID == "" || op.Kind == "" || op.IdempotencyKey == "" {
		return RunOperation{}, false, fmt.Errorf("operation store, run, kind, and idempotency key are required")
	}
	prior, ok, err := s.Store.OperationByIdempotencyKey(op.RunID, op.IdempotencyKey)
	if err != nil {
		return RunOperation{}, false, err
	}
	if ok {
		return adoptPlanned(prior, op.Kind)
	}
	if op.ID == "" {
		op.ID = StableOperationKey(op.RunID, op.Kind, op.IdempotencyKey)
	}
	op.SchemaVersion = SchemaVersion
	op.State = Pending
	op.CreatedAt = s.Clock.Now()
	if op.MaxAttempts <= 0 {
		op.MaxAttempts = 1
	}
	_, created, err := s.Store.PutOperation(op, 0)
	if err != nil {
		return RunOperation{}, false, err
	}
	if created {
		return op, true, nil
	}
	// Another process planned the same key first; adopt its durable operation.
	prior, ok, err = s.Store.OperationByIdempotencyKey(op.RunID, op.IdempotencyKey)
	if err != nil {
		return RunOperation{}, false, err
	}
	if !ok {
		return RunOperation{}, false, fmt.Errorf("operation %q already exists under another identity", op.ID)
	}
	return adoptPlanned(prior, op.Kind)
}
func adoptPlanned(prior RunOperation, kind string) (RunOperation, bool, error) {
	if prior.Kind != kind {
		return RunOperation{}, false, fmt.Errorf("idempotency key belongs to %q", prior.Kind)
	}
	return prior, false, nil
}

func dependenciesSatisfied(op RunOperation, all map[string]RunOperation) bool {
	for _, id := range op.DependsOn {
		if all[id].State != Succeeded {
			return false
		}
	}
	return true
}

// Next leases one eligible operation. Ordering is stable by creation time then
// ID, and acquisition is a compare-and-set: losing the race means another
// scheduler owns the operation, so the scan continues past it.
func (s Scheduler) Next(runID string) (*RunOperation, error) {
	s = s.defaults()
	ops, err := s.Store.Operations(runID)
	if err != nil {
		return nil, err
	}
	all := map[string]RunOperation{}
	for _, op := range ops {
		all[op.ID] = op
	}
	allOperations, err := s.Store.AllOperations()
	if err != nil {
		return nil, err
	}
	// This scan is a cheap early exit only. On its own it is a read-then-act
	// race that two watcher processes both win, so the ceiling is re-checked
	// inside the durable acquisition below; that check, not this one, is what
	// makes max=1 hold across processes.
	activeRuns := map[string]bool{}
	for _, op := range allOperations {
		if (op.State == Leased || op.State == Running) && op.RunID != runID {
			activeRuns[op.RunID] = true
		}
	}
	if len(activeRuns) >= s.MaxConcurrentRuns {
		return nil, nil
	}
	now := s.Clock.Now()
	for _, candidate := range ops {
		op, revision, ok, err := s.Store.Operation(candidate.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if !dependenciesSatisfied(op, all) || op.Attempt >= op.MaxAttempts || op.CancelRequested {
			continue
		}
		alive := op.Lease != nil && s.Liveness.Alive(op.Lease.Owner)
		if !CanAcquire(op, now, alive) {
			continue
		}
		if op.WallBudget > 0 && OperationElapsed(op, now) > op.WallBudget {
			op.State = OperationFailed
			op.Lease = nil
			// A lost CAS here means another scheduler already retired it.
			if _, _, err := s.Store.PutOperation(op, revision); err != nil {
				return nil, err
			}
			continue
		}
		op.State = Leased
		op.Lease = &Lease{Owner: s.Owner, HeartbeatAt: now, ExpiresAt: now.Add(s.LeaseDuration)}
		// Taking the lease IS taking the run-driving slot: the ceiling and the
		// compare-and-set are one durable write. A refusal here is either a
		// lost CAS or a full ceiling; both mean another driver owns the work,
		// so the scan continues past it exactly as before.
		_, acquired, err := s.Store.AcquireOperation(op, revision, s.MaxConcurrentRuns)
		if err != nil {
			return nil, err
		}
		if !acquired {
			continue
		}
		return &op, nil
	}
	return nil, nil
}

func (s Scheduler) Start(id string) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.State != Leased || op.Lease == nil || op.Lease.Owner != s.Owner {
			return fmt.Errorf("operation is not leased by scheduler")
		}
		op.State = Running
		op.Attempt++
		op.StartedAt = &now
		op.LastProgressAt = &now
		return nil
	})
}
func (s Scheduler) Heartbeat(id string, progress string) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.Lease == nil || op.Lease.Owner != s.Owner {
			return fmt.Errorf("operation lease is not owned")
		}
		op.Lease.HeartbeatAt = now
		op.Lease.ExpiresAt = now.Add(s.defaults().LeaseDuration)
		if progress != op.NoProgressKey {
			op.NoProgressKey = progress
			op.LastProgressAt = &now
		}
		return nil
	})
}
func (s Scheduler) Finish(id string, state OperationState) (RunOperation, error) {
	if state != Succeeded && state != OperationFailed && state != OperationCancelled && state != Unknown {
		return RunOperation{}, fmt.Errorf("not a terminal operation state")
	}
	return s.transition(id, func(op *RunOperation, _ time.Time) error {
		if op.State != Leased && op.State != Running {
			return fmt.Errorf("operation is not active")
		}
		op.State = state
		op.Lease = nil
		return nil
	})
}
func (s Scheduler) RequestCancel(id string) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, _ time.Time) error { op.CancelRequested = true; return nil })
}

// BudgetState reports a durable terminal-safe condition without guessing how a
// caller should remediate it. The reconciler records the chosen outcome.
func (s Scheduler) BudgetState(op RunOperation) OperationState {
	now := s.defaults().Clock.Now()
	if (op.WallBudget > 0 && OperationElapsed(op, now) > op.WallBudget) || NoProgressExceeded(op, now) {
		return Unknown
	}
	return op.State
}

const transitionAttempts = 3

// transition re-reads the operation and re-applies change on every attempt, so
// a lost CAS never writes a decision taken against stale durable state.
func (s Scheduler) transition(id string, change func(*RunOperation, time.Time) error) (RunOperation, error) {
	s = s.defaults()
	for attempt := 0; attempt < transitionAttempts; attempt++ {
		op, revision, ok, err := s.Store.Operation(id)
		if err != nil {
			return RunOperation{}, err
		}
		if !ok {
			return RunOperation{}, fmt.Errorf("operation %q not found", id)
		}
		if err := change(&op, s.Clock.Now()); err != nil {
			return RunOperation{}, err
		}
		_, written, err := s.Store.PutOperation(op, revision)
		if err != nil {
			return RunOperation{}, err
		}
		if written {
			return op, nil
		}
	}
	return RunOperation{}, fmt.Errorf("operation %q was changed concurrently by another writer", id)
}

// Reconciler answers whether a side effect happened before a crash. Unknown is
// preserved rather than replayed; later adapters can make operation-specific proofs.
type Reconciler interface {
	Reconcile(context.Context, RunOperation) (OperationState, error)
}

func (s Scheduler) Reconcile(ctx context.Context, op RunOperation, r Reconciler) (RunOperation, error) {
	if r == nil {
		return s.Finish(op.ID, Unknown)
	}
	state, err := r.Reconcile(ctx, op)
	if err != nil {
		return s.Finish(op.ID, Unknown)
	}
	return s.Finish(op.ID, state)
}

// ProcessController is shared by future providers and verifiers. It carries no
// provider-specific process implementation.
type ProcessController interface {
	RequestStop(context.Context, time.Duration) error
}
