package runtime

import (
	"context"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/execution"
)

// holdReaper is a test seam: it runs in the reaper goroutine before it waits
// on the root, so a test can hold the reap back after the root has exited.
var holdReaper = func() {}

// terminalOwnership is the single-assignment state UNDECIDED -> exactly one
// owner, with no transition out. settle commits the first owner offered and
// returns the committed one to every caller, however many race to settle it.
type terminalOwnership struct {
	once  sync.Once
	owner TerminationOwner
}

func (o *terminalOwnership) settle(owner TerminationOwner) TerminationOwner {
	o.once.Do(func() { o.owner = owner })
	return o.owner
}

// ownerOfCancellation names the owner of a termination the executor initiated
// because ctx ended. A context keeps the FIRST cause it was cancelled with, so
// this is the event that actually fired first, not whichever one is inspected
// last.
func ownerOfCancellation(ctx context.Context) TerminationOwner {
	return execution.OwnerOfCause(context.Cause(ctx))
}
