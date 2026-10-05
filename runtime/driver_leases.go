package runtime

import (
	"crypto/rand"
	"strconv"
	"strings"
	"sync"
)

// DriverLeases is this process's record of the lease GRANTS it issued and
// which of them a run driver here is still holding (#485).
//
// The durable lease names an owner, and one owner is one process - but one
// process runs many drivers at once, and a driver can stop driving without
// releasing what it holds: a store error after the lease was taken returns
// the pass before Finish ever runs. The owner is still alive, so the
// dead-owner reclaim never applies, and the lease would claim forever that a
// live supervisor is driving work nobody is driving.
//
// Every acquisition gets a grant this record mints - its own random prefix
// and a counter - so it can answer exactly one question: "is this a grant I
// issued that none of my drivers holds?" A grant it did not issue (another
// record's, even under the same owner identity, or a lease from before grants
// existed) is not one it can vouch for, and is never treated as stranded.
//
// One is injected per process into every Scheduler. A Scheduler with none
// mints unprefixed grants and recovers nothing beyond the dead-owner rule.
type DriverLeases struct {
	prefix string
	mu     sync.Mutex
	next   uint64
	held   map[string]bool
}

// NewDriverLeases is the record for one process.
func NewDriverLeases() *DriverLeases {
	return &DriverLeases{prefix: rand.Text() + "-", held: map[string]bool{}}
}

// issue mints a grant and records it as held, in one step, before the lease
// carrying it is written. Without a record the grant is still unique - so
// Relinquish can still match it exactly - but no record vouches for it.
func (d *DriverLeases) issue() string {
	if d == nil {
		return rand.Text()
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.next++
	grant := d.prefix + strconv.FormatUint(d.next, 10)
	d.held[grant] = true
	return grant
}

// release records that no driver here holds this grant any more.
func (d *DriverLeases) release(grant string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.held, grant)
}

// abandoned reports whether grant is one this record issued and no driver
// here holds. Unknown is never abandoned.
func (d *DriverLeases) abandoned(grant string) bool {
	if d == nil || !strings.HasPrefix(grant, d.prefix) {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return !d.held[grant]
}

// stranded reports whether op carries a lease of THIS scheduler's owner whose
// grant this process issued and no driver here holds.
func (s Scheduler) stranded(op RunOperation) bool {
	if op.Lease == nil || op.Lease.Owner != s.Owner || (op.State != Leased && op.State != Running) {
		return false
	}
	return s.Drivers.abandoned(op.Lease.Grant)
}

// Relinquish ends one driver's hold on the lease it was granted. If the row
// still carries THAT grant - its pass returned, failed or aborted before
// finishing the operation - the lease is removed by compare-and-set, and
// NOTHING ELSE on the row changes: its state, attempt, physical identity,
// execution start, result and retry history are the journal's to settle,
// exactly as after a dead owner's lease is reclaimed.
//
// Matching the grant, which every acquisition mints afresh, is what keeps a
// driver that returns late from dropping a lease another driver here took
// after it - even one written in the same clock instant. The hold is released
// either way, so a write that fails here is recovered by the next scan.
func (s Scheduler) Relinquish(id string, granted Lease) error {
	defer s.Drivers.release(granted.Grant)
	s = s.defaults()
	op, revision, ok, err := s.Store.Operation(id)
	if err != nil || !ok {
		return err
	}
	if granted.Grant == "" || op.Lease == nil || op.Lease.Grant != granted.Grant || (op.State != Leased && op.State != Running) {
		return nil
	}
	op.Lease = nil
	_, _, err = s.Store.PutOperation(op, revision)
	return err
}
