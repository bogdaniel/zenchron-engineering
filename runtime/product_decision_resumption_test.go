package runtime

// #480 acceptance: a product-associated unit that parks on a DecisionRequest
// and is resumed after the operator's DecisionResolution is the same admitted
// worker. The resumed invocation carries the resolution AND the product
// context the unit was admitted with, however the world moved while it waited.

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

func TestADecisionResumedUnitKeepsItsAdmittedProductContext(t *testing.T) {
	fleet := newFleetFixture(t, 4)
	worker := &messagingWorker{fleetProvider: fleet.worker, documents: map[string]string{}, seeded: map[string]bool{}}
	fleet.deps.Provider = worker
	f := &productUnitFixture{fleetFixture: fleet, supervisor: fleet.supervisor(), at: time.Unix(1700000000, 0).UTC(), nextIssue: fleetFirstIssue + 20}

	owner := f.seedProduct("owner", map[string]string{"tier": "gold"})
	other := f.seedProduct("other", map[string]string{"tier": "SECRET-OTHER-TIER"})
	graphID := f.graphFor("owner-work", owner.ID, unitA())
	f.widelyPromote(owner.ID, productWideClaim)
	f.audiencedPromote(owner.ID, graphID, "a", "audienced to unit a")
	f.widelyPromote(other.ID, "SECRET-OTHER-CLAIM")

	f.admit(graphID, "a")
	runID := f.runOf(graphID, "a")
	worker.say(runID, decisionWaitDocument(t), false)
	tick := func() {
		t.Helper()
		if _, err := f.supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		f.clock.advance(61 * time.Second)
	}
	var open []orchestration.EngineeringMessage
	for range 40 {
		tick()
		open, _ = f.store.OpenDecisionRequestsForRun(runID)
		if len(open) == 1 {
			break
		}
	}
	if len(open) != 1 {
		t.Fatal("the unit never parked on its decision request")
	}
	first := productOf(t, f.worker.request(runID))
	pinned, _, err := f.store.OrchestrationBatch(f.activation(t, graphID, "a").BatchID)
	if err != nil || pinned.Product == nil {
		t.Fatalf("the admitted batch carries no product pin: %v", err)
	}

	// While the unit waits the world moves on: a newer configuration, newer
	// knowledge, and a restart of the process's view of the store.
	f.adoptConfiguration(owner.ID, 2, map[string]string{"tier": "platinum"})
	f.widelyPromote(owner.ID, "NEWER-CLAIM-AFTER-ADMISSION")
	if _, err := f.supervisor.ResolveDecision(ControlRequest{
		DecisionID: open[0].ID, DecisionOutcomeKind: orchestration.DecisionSelectedOption,
		DecisionOutcomeValue: "approach-b", Operator: "operator@example",
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(f.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.store = reopened
	f.supervisor = f.fleetFixture.supervisor()

	worker.say(runID, "", false)
	for range 20 {
		tick()
		if len(decisionResumedOperations(t, f.fleetFixture, runID)) == 1 && f.engineeringInvocations(runID) >= 2 {
			break
		}
	}
	if n := f.engineeringInvocations(runID); n != 2 {
		t.Fatalf("the unit was invoked %d times, want the first plus exactly one decision resumption", n)
	}
	resumed := productOf(t, f.worker.request(runID))
	if !reflect.DeepEqual(first, resumed) {
		t.Fatalf("the resumed worker got a different product context:\nfirst   %+v\nresumed %+v", first, resumed)
	}
	if resumed.ConfigurationRevision != 1 || resumed.Settings["tier"] != "gold" {
		t.Fatalf("the resumed worker left its admitted configuration: %+v", resumed)
	}
	prompt := providerPrompt(f.worker.request(runID))
	for _, want := range []string{productWideClaim, "audienced to unit a", "approach-b"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("the resumed prompt lacks %q", want)
		}
	}
	requireAbsent(t, prompt, "NEWER-CLAIM-AFTER-ADMISSION", "platinum", "SECRET-OTHER", productPolicyRef, productContextPolicyRef)

	after, _, err := f.store.OrchestrationBatch(pinned.ID)
	if err != nil || !reflect.DeepEqual(after.Product, pinned.Product) {
		t.Fatalf("the durable pin changed across the decision wait: %v", err)
	}
}
