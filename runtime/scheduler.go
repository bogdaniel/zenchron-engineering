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
	// writes op only when the stored revision still equals expected, the
	// operation's own run is not already terminal, AND fewer than maxRuns OTHER
	// runs currently hold a leased or running operation. All three and the
	// write must be one durable statement - checking first and writing second
	// is the read-then-act race two watcher processes both win, which is the
	// whole reason this is not just PutOperation.
	//
	// The terminal-run condition is what stops a stopped run from TAKING UP
	// more work. CancelRun writes the run document BEFORE it scans the
	// operations, so every acquisition is on one side or the other of that
	// write: one that reaches this statement first is seen by the scan and
	// finished, and one that arrives after it is refused here. Without the
	// condition a driver already inside Next - the supervisor tick and the
	// control endpoint's stop-all are different goroutines and nothing
	// serializes them - leased and executed work the operator had stopped.
	//
	// It does not reach an attempt that has already STARTED. The one started
	// attempt a stop interrupts is the provider of a running
	// execution.invoke, through the execution watcher (#213); every other
	// started operation runs to completion (#215).
	//
	// The same statement refuses a PAUSED run (#86): the run's latest
	// run.paused/run.unpaused event is read inside it, so a pause that commits
	// after a driver read the run still refuses that driver's lease. Like the
	// terminal-run condition, it is stated against SQLite only.
	//
	// The ceiling is PER CAPACITY CLASS (#85): only other runs holding an
	// active operation of the acquired operation's class are counted, against
	// maxRuns for work and maxObservations for observation. A verification
	// operation (#490) is work that ALSO needs a verification slot: it is
	// refused while maxVerifications other runs hold a verification operation,
	// in the same statement, so the ceiling is durable and race-safe exactly
	// as the class ceilings are.
	AcquireOperation(op RunOperation, expected int64, maxRuns, maxObservations, maxVerifications int) (int64, bool, error)
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
	if op.ActiveSince != nil {
		active := *op.ActiveSince
		op.ActiveSince = &active
	}
	if op.Deadline != nil {
		deadline := *op.Deadline
		op.Deadline = &deadline
	}
	if op.LastProgressAt != nil {
		progress := *op.LastProgressAt
		op.LastProgressAt = &progress
	}
	if op.InactivitySuspension != nil {
		suspension := *op.InactivitySuspension
		op.InactivitySuspension = &suspension
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
//
// The terminal-run condition is NOT mirrored, and cannot be: this double holds
// operations and nothing else, so it has no run to ask. Inventing a second
// runs table here would be a second answer to "is this run stopped" living only
// in test code, which is worth less than the rule it would imitate. The rule is
// stated against SQLite, where the run document and the acquisition are one
// statement, by TestSQLiteAStoppedRunsOperationIsNeverAcquired.
func (s *MemoryOperationStore) AcquireOperation(op RunOperation, expected int64, maxRuns, maxObservations, maxVerifications int) (int64, bool, error) {
	if op.ID == "" || expected <= 0 {
		return 0, false, fmt.Errorf("acquiring an operation needs its id and the revision it was read at")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if revision, exists := s.revisions[op.ID]; !exists || revision != expected {
		return 0, false, nil
	}
	class := OperationCapacityClass(op.Kind)
	ceiling := maxRuns
	if class == CapacityObservation {
		ceiling = maxObservations
	}
	driven, verifying := map[string]bool{}, map[string]bool{}
	for id, stored := range s.operations {
		active := stored.Lease != nil && (stored.State == Leased || stored.State == Running)
		if stored.RunID == op.RunID && id != op.ID && active {
			return 0, false, nil
		}
		if stored.RunID == op.RunID || !active {
			continue
		}
		if OperationCapacityClass(stored.Kind) == class {
			driven[stored.RunID] = true
		}
		if holdsVerificationSlot(stored) {
			verifying[stored.RunID] = true
		}
	}
	if len(driven) >= ceiling || (consumesVerification(op.Kind) && len(verifying) >= maxVerifications) {
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
	// MaxConcurrentObservations is the observation-class ceiling (#85).
	MaxConcurrentObservations int
	// MaxConcurrentVerifications is the verification ceiling (#490).
	MaxConcurrentVerifications int
	// Drivers is this process's record of the leases its drivers hold
	// (#485). Shared by every Scheduler with this Owner; nil recovers only
	// dead owners' leases.
	Drivers *DriverLeases
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
	s.MaxConcurrentObservations = resolveMaxConcurrentObservations(s.MaxConcurrentObservations)
	s.MaxConcurrentVerifications = resolveMaxConcurrentVerifications(s.MaxConcurrentVerifications)
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

// leasable is the scheduler's eligibility test, shared with the fleet's
// Runnable count so the two cannot drift: Next tries to lease exactly the
// operations for which it holds, and retires an expired one instead.
func leasable(op RunOperation, all map[string]RunOperation, now time.Time, ownerAlive bool) bool {
	return dependenciesSatisfied(op, all) && op.Attempt < op.MaxAttempts && !op.CancelRequested &&
		CanAcquire(op, now, ownerAlive)
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
	if err := s.reclaimVerificationPermits(); err != nil {
		return nil, err
	}
	if pending, err := s.VerificationCleanupPending(runID); err != nil || pending {
		return nil, err
	}
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
	now := s.Clock.Now()
	// This scan is where an ABANDONED operation is given back, because it is
	// the only place a run ever looks at a sibling's operations at all.
	//
	// It is deliberately NOT a capacity check (#85). It used to return early
	// once every slot was taken, and that early exit was blind to capacity
	// class: one long execution.invoke at a ceiling of one kept every waiting
	// run from even observing its review. The ceilings are decided per class,
	// transactionally, inside AcquireOperation below.
	for _, op := range allOperations {
		// A lease-less active row is an attempt nobody is holding - either one
		// this scan already reclaimed, or one a sibling did. It occupies
		// nothing, exactly as the durable count reads it.
		//
		// The run's OWN abandoned operations are reclaimed too: the store
		// refuses a second active operation of one run, so an abandoned lease
		// left on an exhausted operation would otherwise block the run's next
		// operation forever. reclaimAbandoned applies CanAcquire's liveness and
		// expiry rule, so a live driver's lease is never touched.
		if op.Lease == nil || (op.State != Leased && op.State != Running) {
			continue
		}
		if _, err := s.reclaimAbandoned(op, now); err != nil {
			return nil, err
		}
	}
	for _, candidate := range ops {
		op, revision, ok, err := s.Store.Operation(candidate.ID)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		alive := op.Lease != nil && s.Liveness.Alive(op.Lease.Owner)
		if !leasable(op, all, now, alive) {
			continue
		}
		// Retired on EXECUTION AUTHORITY, not on wall-clock elapsed. The
		// elapsed test charged an operation for however long the world took
		// between its attempts, so a run parked on an unavailable account could
		// be retired without ever having executed for its budget.
		if OperationExpired(op, now) {
			op.State = OperationFailed
			op.Lease = nil
			// A lost CAS here means another scheduler already retired it.
			if _, _, err := s.Store.PutOperation(op, revision); err != nil {
				return nil, err
			}
			continue
		}
		op.State = Leased
		// The grant is issued - and recorded as held - BEFORE the durable
		// acquisition (#485), so no sibling driver in this process can read
		// the new lease as one nobody here holds. The caller that receives the
		// operation owns that grant from here and ends it with Relinquish.
		grant := s.Drivers.issue()
		op.Lease = &Lease{Owner: s.Owner, HeartbeatAt: now, ExpiresAt: now.Add(s.LeaseDuration), Grant: grant}
		// Taking the lease IS taking the run-driving slot: the ceiling and the
		// compare-and-set are one durable write. A refusal here is either a
		// lost CAS or a full ceiling; both mean another driver owns the work,
		// so the scan continues past it exactly as before.
		_, acquired, err := s.Store.AcquireOperation(op, revision, s.MaxConcurrentRuns, s.MaxConcurrentObservations, s.MaxConcurrentVerifications)
		if err != nil || !acquired {
			s.Drivers.release(grant)
		}
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

// VerificationSaturated reports whether other runs hold every verification
// slot (#490). It is what lets a refused verification say it is waiting on
// capacity rather than that the operation was merely unavailable. It reads the
// same durable occupancy AcquireOperation counts, so it is true after a
// restart exactly when the acquisition would still refuse.
func (s Scheduler) VerificationSaturated(runID string) (bool, error) {
	s = s.defaults()
	all, err := s.Store.AllOperations()
	if err != nil {
		return false, err
	}
	verifying := map[string]bool{}
	for _, op := range all {
		if op.RunID != runID && holdsVerificationSlot(op) {
			verifying[op.RunID] = true
		}
	}
	nested := 0
	if store, ok := s.Store.(VerificationPermitStore); ok {
		permits, err := store.VerificationPermits()
		if err != nil {
			return false, err
		}
		for _, permit := range permits {
			if permit.State == VerificationGranted {
				nested++
			}
		}
	}
	return len(verifying)+nested >= s.MaxConcurrentVerifications, nil
}

// reclaimAbandoned drops the lease of one leased or running operation that NO
// DRIVER IS STILL HOLDING, and reports whether it is now released.
//
// The run-driving slot IS the durable lease, so it is released by whoever
// finishes the operation - and a driver that died between leasing and finishing
// never releases anything. Nothing else heals that: the collector refuses to
// collect a run holding a leased operation, and the reconciler's store-lag
// repair only acts when the journal already carries a terminal operation state.
// The slot was therefore gone until somebody edited the database by hand.
//
// The predicate is CanAcquire's, exactly: the lease has expired AND its owner
// is provably dead. It is deliberately not expiry alone. Nothing renews a lease
// during an attempt - the operation's execution authority, not its lease, is
// what bounds the work - so every live driver holds an expired lease within a
// minute of taking it, and releasing a slot on expiry would put the ceiling
// back to meaning nothing. Reusing the takeover rule is also what keeps
// exclusivity: a slot can only be reclaimed from an operation another driver
// was already permitted to take over, so reclaiming can never contradict a
// lease a living owner still holds.
//
// What is written is the smallest true thing: the lease is gone. Nothing else
// about the row is touched, because nothing else about it is known to be wrong
// - what the attempt did is the journal's to say, and the row is a cache of the
// journal. The lease is the one claim that is now provably false, so the lease
// is the one claim removed, and every counter that asks who is driving reads
// the answer from durable state rather than from an opinion held in a process.
//
// The write is a compare-and-set, so two schedulers reclaiming the same
// operation cannot both win, and a lost race simply leaves the operation
// counted - the conservative answer.
//
// A lease THIS process left behind is the other abandoned shape (#485): its
// owner is alive, so the dead-owner rule never applies, yet no driver here
// holds it - the pass that took it returned without finishing it. DriverLeases
// is what proves that, and only for a grant it issued itself; any other
// lease is still reclaimable only when its owner is dead and it has expired.
func (s Scheduler) reclaimAbandoned(candidate RunOperation, now time.Time) (bool, error) {
	abandoned := func(op RunOperation) bool {
		return op.Lease != nil && (CanAcquire(op, now, s.Liveness.Alive(op.Lease.Owner)) || s.stranded(op))
	}
	if !abandoned(candidate) {
		return false, nil
	}
	op, revision, ok, err := s.Store.Operation(candidate.ID)
	if err != nil || !ok {
		return false, err
	}
	if op.State != Leased && op.State != Running {
		return true, nil
	}
	if !abandoned(op) {
		return false, nil
	}
	// ONLY the lease is dropped. The row goes on saying the attempt was leased
	// or running, because that is what the journal says happened to it and the
	// row is a cache of the journal, not a second opinion about it.
	//
	// Writing a terminal state here would be that second opinion, and it would
	// silence the one repair that knows better. A crash between the journal
	// write and the scheduler write is the common shape of this whole defect:
	// operation.after is already journalled as succeeded and only Finish was
	// lost. reconcileStoreLag exists to copy that journalled outcome onto the
	// row, and it looks for exactly a leased or running row. A sibling that got
	// there first and stamped `unknown` on it would leave the store permanently
	// disagreeing with the journal about an operation that succeeded.
	op.Lease = nil
	_, released, err := s.Store.PutOperation(op, revision)
	return released, err
}

func (s Scheduler) Start(id string) (RunOperation, error) { return s.StartWithin(id, nil) }

// AttemptLimit caps one physical attempt below its operation's own remaining
// WallBudget. Within is the authority; Bound names which bound it is.
type AttemptLimit struct {
	Within time.Duration
	Bound  AttemptBound
}

// StartWithin is Start with the attempt's deadline additionally bounded by
// limit (#328). A nil limit is the legacy rule exactly. A non-positive Within
// is zero authority - an already-passed deadline - never "unbounded".
func (s Scheduler) StartWithin(id string, limit *AttemptLimit) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.State != Leased || op.Lease == nil || op.Lease.Owner != s.Owner {
			return fmt.Errorf("operation is not leased by scheduler")
		}
		op.State = Running
		op.Attempt++
		// The PHYSICAL attempt identity of the try about to happen, allocated
		// separately from the budget attempt above because RestoreAttempt may
		// give that one back. This one is never given back: a transcript slot
		// that has been written stays written, so the identity of the next try
		// has to be past it whatever the budget did.
		op.AttemptIdentity++
		op.StartedAt = &now
		// THE PROGRESS FINGERPRINT IS PER PHYSICAL ATTEMPT and is always
		// cleared. It is a cumulative byte count of ONE process's output, so a
		// successor's first bytes would otherwise be compared against a dead
		// process's total and read as "nothing new" - which would suppress the
		// successor's real progress until it out-talked its predecessor.
		op.NoProgressKey = ""
		// A suspension belongs to the process that opened the tool, and that
		// process is gone: no successor inherits it (#352).
		op.InactivitySuspension = nil
		// EXECUTION BEGINS HERE, so this is where authority starts being spent.
		//
		// The attempt's deadline is derived from what the operation has NOT yet
		// spent, not from the whole budget: a second attempt inherits the
		// remainder rather than a fresh envelope. And it is derived here rather
		// than at plan time because the budget bounds execution - a run parked
		// on an unavailable provider account, an operator decision or a review
		// is not executing, and an instant stamped before it started would burn
		// while nothing ran.
		// A PREVIOUS ATTEMPT THAT NEVER FINISHED still spent what it spent.
		//
		// Finish is what normally folds an attempt's elapsed execution into the
		// counter. A controller that died mid-attempt never reached it, so its
		// ActiveSince is still set here - and starting the next attempt without
		// folding it would silently return the whole budget, which is the
		// original defect in a narrower place. Charging it is the fail-safe
		// direction: an attempt that was active and cannot say how much of its
		// time was useful is charged for all of it.
		//
		// ABANDONED IS NOT THE SAME AS SETTLED, and the difference decides what
		// the successor inherits. An operation that still carries ActiveSince
		// here was never finished by anyone: its controller died mid-attempt,
		// so nothing observed how that attempt ended and nothing classified it.
		abandoned := op.ActiveSince != nil
		if abandoned {
			waiting, err := s.operationVerificationWait(*op, now)
			if err != nil {
				return err
			}
			if orphaned := now.Sub(*op.ActiveSince) - waiting; orphaned > 0 {
				op.ConsumedExecution += orphaned
				op.LastAttemptExecution = orphaned
			}
		}
		// THE INACTIVITY DATUM IS THE SECOND THING A RESTART MUST NOT REFUND,
		// and it is a different authority from the execution budget above.
		//
		// Charging the orphaned interval to ConsumedExecution proves the run's
		// wall budget did not reset. It says nothing about the no-progress
		// window, which is measured from the last moment output was actually
		// observed - so stamping this to `now` for an ABANDONED attempt hands
		// the successor a fresh full window and forgives silence the previous
		// invocation had already proven. A supervisor that bounced every few
		// minutes would reproduce #238 exactly, one clean window at a time.
		//
		// Carrying it forward is the reconstruction #238 asks for: the datum is
		// durable, it was advanced only by real observed output, and the
		// successor therefore starts with limit-minus-silence rather than
		// limit. See ProviderInactivityRemaining.
		//
		// A SETTLED attempt is the opposite case and resets. Its outcome was
		// observed, journalled and classified - a stall is recorded as
		// provider_no_progress and routed to a BOUNDED retry - and a bounded
		// retry that inherited an exhausted window would refuse before dispatch
		// forever, which is not a retry. The attempt ceiling is what bounds it,
		// exactly as it bounds every other reattemptable class.
		//
		// An inherited datum keeps ProgressRecorderOpen with it: that is the
		// evidence that the dead process's newest progress may never have
		// become durable (#352). A fresh stamp is not recorded progress and
		// has no unwritten tail.
		if !abandoned || op.LastProgressAt == nil {
			op.LastProgressAt = &now
			op.ProgressRecorderOpen = false
		}
		op.ActiveSince = &now
		if op.WallBudget > 0 {
			remaining := op.WallBudget - op.ConsumedExecution
			if remaining < 0 {
				remaining = 0
			}
			deadline := now.Add(remaining)
			op.Deadline = &deadline
		}
		op.DeadlineBound = ""
		if limit != nil {
			// The operation's own WallBudget is the run envelope it was planned
			// under, so when IT is the earlier instant the binding bound is
			// still the run's active work.
			op.DeadlineBound = BoundRunActiveWork
			within := max(limit.Within, 0)
			if deadline := now.Add(within); op.Deadline == nil || !deadline.After(*op.Deadline) {
				op.Deadline = &deadline
				op.DeadlineBound = limit.Bound
			}
		}
		return nil
	})
}

// Heartbeat renews the LEASE and nothing else.
//
// It used to take a progress value and advance NoProgressKey/LastProgressAt
// when that value changed, which made the #238 law false at this API boundary:
// a caller could refresh provider inactivity authority without the provider
// having produced anything. Liveness of the CONTROLLER and movement of the WORK
// are different claims, and letting the first stand in for the second is the
// whole defect - a supervisor that is merely still running would have kept a
// dead provider's window open indefinitely.
//
// The progress argument is REMOVED rather than ignored. An ignored parameter
// leaves the wrong call shape compiling and reads as though it still means
// something; removing it makes the invariant structural, so the mistake cannot
// be made again without changing this signature. Provider progress moves only
// through RecordProviderProgress.
func (s Scheduler) Heartbeat(id string) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.Lease == nil || op.Lease.Owner != s.Owner {
			return fmt.Errorf("operation lease is not owned")
		}
		op.Lease.HeartbeatAt = now
		op.Lease.ExpiresAt = now.Add(s.defaults().LeaseDuration)
		return nil
	})
}

// RecordProviderProgress makes one observation of provider activity DURABLE.
//
// It is deliberately separate from Heartbeat, which renews the lease and
// touches nothing else. Renewing a lease is a claim about the CONTROLLER being
// alive; this is a claim about the WORK moving, and #238 is precisely the
// defect of letting the first stand in for the second. Merging them would mean
// an inactivity window could be refreshed by a supervisor that is merely still
// running.
//
// The key is a progress FINGERPRINT, and the durable instant advances only when
// it changes - so re-observing the same output is not progress. An unowned or
// finished operation is not an error: the process this records for may outlive
// the lease it was started under, and losing a progress note is not a reason to
// fail an invocation that is working.
//
// It is BOUND to the physical attempt that observed it: the write lands only
// while the row is running THAT attempt. A write of attempt N can still land
// after N settled, or after N+1 started on the same row, and either would
// durably claim progress - or a suspension - a different process made. (An
// operation leased before the identity counter existed carries 0 here while
// its dispatch was clamped to 1, so it misses its durable stamps until it
// settles; that is the only cost.)
//
// The instant recorded is when the progress was OBSERVED - now less its Age -
// not when the coalescing recorder got round to writing it (#352), and it
// never moves backwards. The suspension and the recorder-open flag are the
// attempt's current state, replaced whole on every write.
func (s Scheduler) RecordProviderProgress(id string, attempt int, progress ProviderProgress) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.State != Running || op.AttemptIdentity != attempt {
			return nil
		}
		if progress.Key != "" && progress.Key != op.NoProgressKey {
			at := now.Add(-max(progress.Age, 0))
			if op.LastProgressAt != nil && at.Before(*op.LastProgressAt) {
				at = *op.LastProgressAt
			}
			op.NoProgressKey, op.LastProgressAt = progress.Key, &at
		}
		op.ProgressRecorderOpen = !progress.Final
		op.InactivitySuspension = nil
		if progress.Suspended {
			op.InactivitySuspension = &InactivitySuspension{
				Since: now.Add(-max(progress.SuspendedAge, 0)), AttemptIdentity: attempt, Owner: s.Owner,
			}
		}
		return nil
	})
}

func (s Scheduler) Finish(id string, state OperationState) (RunOperation, error) {
	return s.finishAt(id, state, time.Time{}, "", time.Time{})
}

// finishAt also records retryAt (purely "not eligible before") with the
// disposition that accounts for it, so the store row and the journal agree,
// and, for store-lag recovery, when the attempt actually ended; zero is now.
func (s Scheduler) finishAt(id string, state OperationState, retryAt time.Time, disposition RetryDisposition, endedAt time.Time) (RunOperation, error) {
	if state != Succeeded && state != OperationFailed && state != OperationCancelled && state != Unknown {
		return RunOperation{}, fmt.Errorf("not a terminal operation state")
	}
	return s.transition(id, func(op *RunOperation, now time.Time) error {
		if op.State != Leased && op.State != Running {
			return fmt.Errorf("operation is not active")
		}
		op.State = state
		op.RetryNotBefore, op.RetryDisposition = retryAt, disposition
		op.Lease = nil
		// What this attempt ACTUALLY executed joins the durable counter, and
		// the operation stops being active. Anything that happens between now
		// and the next attempt - an operator decision, a funded account, a
		// human review - costs nothing, because nothing is executing.
		if op.ActiveSince != nil {
			ended := now
			if !endedAt.IsZero() && endedAt.Before(ended) {
				ended = endedAt
			}
			wait, err := s.operationVerificationWait(*op, ended)
			if err != nil {
				return err
			}
			spent := ended.Sub(*op.ActiveSince) - wait
			if spent < 0 {
				spent = 0
			}
			op.ConsumedExecution += spent
			op.LastAttemptExecution = spent
			op.ActiveSince = nil
		}
		op.Deadline, op.DeadlineBound = nil, ""
		// The attempt's process is over, so neither can still be true of it.
		op.InactivitySuspension, op.ProgressRecorderOpen = nil, false
		return nil
	})
}

// RestoreAttempt gives back the attempt an operation consumed when that attempt
// did not exercise the run's work at all.
//
// It exists for exactly one situation: a wait-routed failure, where the
// external world refused before any work happened - the execution provider
// declined at its account boundary, so no reasoning ran, no candidate was
// touched, and nothing about the run changed. Charging the run's execution
// budget for that would mean an operator who corrects the external condition
// finds the budget already spent by the passes that were only waiting, and a
// watch loop polling a waiting run would exhaust it without ever doing work.
//
// The operation stays FAILED and its diagnostic stays in the journal: what the
// attempt observed is still true and still readable. Only the budget counter
// and the elapsed-time origin are given back, so the wall budget measures the
// next real attempt rather than however long a human took to restore an
// account.
// ReserveAttemptIdentity raises an operation's allocated attempt identity and
// persists it, so nothing can write evidence under an identity this runtime has
// not already committed to owning.
//
// It exists because the identity is decided by two durable facts and the later
// one wins: what the scheduler allocated, and what the evidence store says is
// still free. When the evidence is ahead - a record written before the identity
// was tracked, or an operation already stranded on an occupied slot - the
// selected identity is higher than the one on the record, and dispatching a
// provider under it without writing it down first is a crash away from handing
// the same identity out twice: the counter would still be behind, the slot
// would still look free, and the second invocation would claim it.
//
// It only ever raises. A caller asking for an identity at or below the one
// already allocated is not an error and changes nothing: the operation has
// already committed to owning at least that far.
func (s Scheduler) ReserveAttemptIdentity(id string, identity int) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, _ time.Time) error {
		if identity > op.AttemptIdentity {
			op.AttemptIdentity = identity
		}
		return nil
	})
}

func (s Scheduler) RestoreAttempt(id string, refundExecution bool) (RunOperation, error) {
	return s.transition(id, func(op *RunOperation, _ time.Time) error {
		if op.State != OperationFailed {
			return fmt.Errorf("only a failed operation can have an attempt restored")
		}
		if op.Attempt > 0 {
			op.Attempt--
		}
		// AttemptIdentity is deliberately NOT restored. The attempt ceiling is a
		// budget and may be refunded; the try that just happened is a fact, and
		// its evidence is filed under an identity no later try may reuse.
		op.Lease = nil
		op.StartedAt = nil
		// The execution budget is given back ONLY when no execution happened.
		//
		// Elapsed time is not proof that a provider did nothing. A worker can
		// reason, call tools and edit for twenty minutes and only then meet a
		// rate limit, a quota or an account condition that routes to a wait.
		// Refunding that would make real work free every time it ends in the
		// same wall, and a cumulative budget that can be replenished by hitting
		// an external limit is not a budget.
		//
		// What is always given back is the ATTEMPT: observing an external
		// refusal is not work the run's attempt ceiling should pay for.
		if refundExecution {
			op.ConsumedExecution -= op.LastAttemptExecution
			if op.ConsumedExecution < 0 {
				op.ConsumedExecution = 0
			}
		}
		op.LastAttemptExecution = 0
		op.ActiveSince = nil
		op.Deadline, op.DeadlineBound = nil, ""
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
	if OperationExpired(op, now) || NoProgressExceeded(op, now) {
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
