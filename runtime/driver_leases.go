package runtime

import "sync"

// DriverLeases is this process's own record of which operation leases a run
// driver in THIS process is holding right now (#485).
//
// The durable lease names an owner, and one owner is one process - but one
// process runs many drivers at once, and a driver can stop driving without
// releasing what it holds: a store error after the lease was taken returns
// the pass before Finish ever runs. The owner is still alive, so the
// dead-owner reclaim never applies, and the lease would claim forever that a
// live supervisor is driving work nobody is driving. Only the process itself
// can tell a lease one of its drivers holds from one it left behind, and this
// is where it records that.
//
// It is injected, one per process, into every Scheduler that leases under the
// process's owner identity. A Scheduler with none recovers nothing beyond the
// dead-owner rule: without this record, an own-owner lease cannot be told
// apart from a concurrent driver's, and taking it would steal live work.
type DriverLeases struct {
	mu sync.Mutex
	// held COUNTS drivers per operation. Two drivers of one run in this
	// process can overlap on one operation - one returning after finishing
	// it retryably while the other has already leased it again - and the
	// first one's release must not erase the second one's hold.
	held map[string]int
}

// NewDriverLeases is the record for one process.
func NewDriverLeases() *DriverLeases { return &DriverLeases{held: map[string]int{}} }

// hold records that a driver here holds, or is about to take, a lease. It is
// called BEFORE the durable acquisition so no other driver in this process
// can read the freshly written lease as one nobody holds.
func (d *DriverLeases) hold(id string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.held[id]++
}

// release records that no driver here holds the lease any more.
func (d *DriverLeases) release(id string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.held[id] <= 1 {
		delete(d.held, id)
		return
	}
	d.held[id]--
}

// stranded reports whether op carries a lease of THIS process that no driver
// here holds. Without a record it is never true: unknown is not stranded.
func (s Scheduler) stranded(op RunOperation) bool {
	if s.Drivers == nil || op.Lease == nil || op.Lease.Owner != s.Owner || (op.State != Leased && op.State != Running) {
		return false
	}
	s.Drivers.mu.Lock()
	defer s.Drivers.mu.Unlock()
	return s.Drivers.held[op.ID] == 0
}

// Relinquish ends one driver's hold on the operation it was granted. If the
// row still carries EXACTLY the lease that driver was granted - its pass
// returned, failed or aborted before finishing it - that lease is removed by
// compare-and-set, and NOTHING ELSE on the row changes: its state, attempt,
// physical identity, execution start, result and retry history are the
// journal's to settle, exactly as after a dead owner's lease is reclaimed.
//
// Matching the exact grant, not merely this process's owner, is what keeps a
// driver from dropping a lease another driver here took after it finished.
// The in-process hold is released either way, so a write that fails here is
// recovered by the next scan instead.
func (s Scheduler) Relinquish(id string, granted Lease) error {
	defer s.Drivers.release(id)
	s = s.defaults()
	op, revision, ok, err := s.Store.Operation(id)
	if err != nil || !ok {
		return err
	}
	if op.Lease == nil || !sameLease(*op.Lease, granted) || (op.State != Leased && op.State != Running) {
		return nil
	}
	op.Lease = nil
	_, _, err = s.Store.PutOperation(op, revision)
	return err
}

func sameLease(a, b Lease) bool {
	return a.Owner == b.Owner && a.HeartbeatAt.Equal(b.HeartbeatAt) && a.ExpiresAt.Equal(b.ExpiresAt)
}
