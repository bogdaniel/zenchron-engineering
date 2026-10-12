package runtime

// Integration conflict delivery (#475 B1): the last governed step connecting
// a WorkGraph integration unit's locally-admitted conflict Finding to the
// upstream producer it names.
//
// refuseIntegrationConflict (integration_dispatch.go) already admits a typed
// #473 Finding, correctly sourced and routed to the producer's unit name -
// but into the INTEGRATION UNIT'S OWN single-item batch scope, which the
// producer's own inbox is never projected from. deliverIntegrationConflicts
// is the explicit delivery of that SAME admitted observation into the
// producer's OWN batch scope, using orchestration.DeliverFinding - never a
// second message bus, never a graph-wide scope, never every unit's messages.
import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// deliverIntegrationConflicts finds every integration_compose batch's live,
// unit-routed Findings and delivers a copy into the producer unit's own
// batch, resolved from the SAME graph's durable WorkUnitActivations - never
// a live WorkGraph projection, which can change under this function's feet,
// and never a caller-supplied map, which could name a batch this function
// never itself verified owns that unit.
//
// It is unconditionally safe to run every reconcile pass: delivery is
// idempotent (orchestration.DeliverFinding's id is a pure function of the
// original message and the destination scope), and a Finding already
// delivered re-derives the identical row, which the store's insert-only
// admission leaves untouched. A conflict whose Finding has not landed yet,
// or whose producer has no recorded activation yet, is simply retried next
// pass - never a terminal error for this batch or any other.
//
// A conflict already delivered on an earlier pass is recognized by a bare
// existence check on its own deterministic destination id (#475 review P2)
// BEFORE this function pays for the heavier per-conflict work (reading the
// producer's whole RunHandoffs and writing a transaction) - every one of
// which otherwise repeats, unchanged, on every single supervisor tick for
// the rest of that batch's durable history.
func deliverIntegrationConflicts(store *SQLiteOperationStore, batches []orchestration.Batch) []string {
	var problems []string
	byID := make(map[string]orchestration.Batch, len(batches))
	for _, batch := range batches {
		byID[batch.ID] = batch
	}
	activationsByGraph := map[string]map[string]WorkUnitActivation{}
	for _, batch := range batches {
		if batch.Origin == nil || batch.Origin.ExecutionKind != orchestration.ExecutionKindIntegrationCompose {
			continue
		}
		activations, ok := activationsByGraph[batch.Origin.GraphID]
		if !ok {
			var err error
			if activations, err = store.WorkUnitActivations(batch.Origin.GraphID); err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
				continue
			}
			activationsByGraph[batch.Origin.GraphID] = activations
		}
		messages, err := store.ScopeMessages(batch.ID)
		if err != nil {
			problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
			continue
		}
		for _, message := range orchestration.Live(messages) {
			if message.Kind != orchestration.KindFinding || message.Route.Audience != orchestration.AudienceUnit {
				continue
			}
			activation, found := activations[message.Route.Unit]
			if !found {
				problems = append(problems, boundedDetail(fmt.Sprintf(
					"%s conflict delivery: producer unit %s of graph %s has no recorded activation yet",
					batch.ID, message.Route.Unit, batch.Origin.GraphID)))
				continue
			}
			destination, found := byID[activation.BatchID]
			if !found || len(destination.Items) != 1 {
				problems = append(problems, boundedDetail(fmt.Sprintf(
					"%s conflict delivery: producer unit %s's own batch %s is not a readable one-issue batch",
					batch.ID, message.Route.Unit, activation.BatchID)))
				continue
			}
			destinationUnit := orchestration.BatchItemUnit(destination.Items[0].Issue)
			deliveredID, err := orchestration.DeliveredFindingID(message.ID, destination.ID)
			if err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
				continue
			}
			if already, err := store.MessageExists(deliveredID); err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
				continue
			} else if already {
				if integrationConflictDeliverySkipped != nil {
					integrationConflictDeliverySkipped(deliveredID)
				}
				continue
			}
			// The consuming unit's own scope can only reconstruct a foreign
			// input's subject from WorkUnitInputs, which carries no
			// BaseRevision (queryBatchMessageScope) - so message.Subject.Revision
			// is incomplete. The destination's OWN durable handoff record, read
			// by the exact handoff id this Finding already names, is the real,
			// complete one: looked up by identity, never by "whichever is
			// latest", so a handoff since superseded by a newer one still
			// resolves to ITS OWN original subject, exactly as the conflict was
			// actually about.
			revision, found := handoffSubject(store, activation.RunID, message.Subject.Handoff)
			if !found {
				problems = append(problems, boundedDetail(fmt.Sprintf(
					"%s conflict delivery: producer unit %s's exact handoff %s is no longer readable",
					batch.ID, message.Route.Unit, message.Subject.Handoff)))
				continue
			}
			delivered, err := orchestration.DeliverFinding(message, destination.ID, destinationUnit, revision)
			if err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
				continue
			}
			if _, err := store.AdmitDeliveredFinding(delivered); err != nil {
				problems = append(problems, boundedDetail(fmt.Sprintf("%s conflict delivery: %v", batch.ID, err)))
			}
		}
	}
	return problems
}

// integrationConflictDeliverySkipped is a test seam: it runs whenever the
// already-delivered fast path short-circuits a conflict this function would
// otherwise redo the full delivery work for, the same pattern
// afterMergeBeforeHeadRead (integration.go) and afterCommitGates (git.go)
// already use to let a test observe a code path without instrumenting
// SQLite itself.
var integrationConflictDeliverySkipped func(deliveredID string)

// handoffSubject reads one run's exact admitted handoff by its own
// immutable id, never by recency: a handoff since superseded by a newer one
// is still readable under its own id, with its own original subject.
func handoffSubject(store *SQLiteOperationStore, runID, handoffID string) (orchestration.HandoffSubject, bool) {
	handoffs, err := store.RunHandoffs(runID)
	if err != nil {
		return orchestration.HandoffSubject{}, false
	}
	for _, handoff := range handoffs {
		if handoff.ID == handoffID {
			return handoff.Subject, true
		}
	}
	return orchestration.HandoffSubject{}, false
}
