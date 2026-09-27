package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// #326: the lease line of a running operation is the live row's lease that
// status matched to this attempt, not the one the journal stamped at start.
func TestLeaseLineOfARunningOperationIsTheRowLease(t *testing.T) {
	started := time.Unix(1_800_000_000, 0).UTC()
	beat := started.Add(20 * time.Minute)
	journalled, err := json.Marshal(runtime.RunOperation{ID: "op-1", Lease: &runtime.Lease{Owner: "journal-owner", HeartbeatAt: started, ExpiresAt: started}})
	if err != nil {
		t.Fatal(err)
	}
	events := []runtime.EngineeringEvent{{Type: runtime.EventOperationBefore, OperationID: "op-1", Payload: journalled}}
	report := runtime.StatusReport{Operation: &runtime.OperationStatus{
		ID: "op-1", State: runtime.Running, ProgressSource: "row",
		Lease: &runtime.Lease{Owner: "row-owner", HeartbeatAt: beat, ExpiresAt: beat.Add(time.Minute)},
	}}
	lease := leaseOf(events, report)
	if lease == nil || lease.Owner != "row-owner" || !lease.HeartbeatAt.Equal(beat) {
		t.Fatalf("lease = %#v, want the row's", lease)
	}
	// A journal fallback still reads the journal, and the text says so.
	report.Operation.ProgressSource, report.Operation.Lease = "journal", nil
	report.Operation.InactivityLimit = 10 * time.Minute
	lease = leaseOf(events, report)
	if lease == nil || lease.Owner != "journal-owner" {
		t.Fatalf("fallback lease = %#v, want the journal's", lease)
	}
	var out bytes.Buffer
	if err := renderStatusText(&out, statusView{StatusReport: report, Lease: lease}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"progress (journal):", "lease (journal):"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("text does not mark the journal source %q:\n%s", want, out.String())
		}
	}
}
