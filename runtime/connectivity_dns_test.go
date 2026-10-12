package runtime

import (
	"net"
	"testing"
)

func TestDNSNotFoundSourceObservationUsesBoundedBackoff(t *testing.T) {
	f := newPhase8Fixture(t)
	runID := f.start()
	f.clock.step = 0
	f.inject(func(GitHubCall) error { return &net.DNSError{Name: "api.github.com", IsNotFound: true} })
	first := f.reconcile(runID)
	if first.Disposition != Waiting || first.Reason != ReasonConnectivityBackoff {
		t.Fatalf("DNS lookup outage: %+v", first)
	}
	ops, err := f.store.Operations(runID)
	if err != nil {
		t.Fatal(err)
	}
	var waiting RunOperation
	for _, op := range ops {
		if op.Kind == OpSourceObserve {
			waiting = op
		}
	}
	if waiting.Attempt != 1 || !waiting.RetryNotBefore.After(f.clock.at) {
		t.Fatalf("DNS lookup did not retain bounded retry authority: %+v", waiting)
	}
	f.inject(func(GitHubCall) error { return nil })
	f.clock.at = waiting.RetryNotBefore
	f.reconcile(runID)
	recovered, _, _, err := f.store.Operation(waiting.ID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != Succeeded || recovered.Attempt != 2 {
		t.Fatalf("DNS lookup recovery changed operation or budget: %+v", recovered)
	}
}
