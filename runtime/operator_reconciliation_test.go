package runtime

import (
	"context"
	"testing"
)

func TestRefreshReusesExactSuccessorAndCannotRefreshBoundWork(t *testing.T) {
	fixture := newFleetFixture(t, 2)
	supervisor := fixture.supervisor()
	started, err := supervisor.Submit(context.Background(), ControlRequest{Repository: "acme/repo", Issue: fleetFirstIssue, Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := supervisor.RefreshIssueRun(context.Background(), started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.RunID == started.RunID {
		t.Fatal("refresh reused original generation")
	}
	original, _, err := fixture.store.Run(started.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if original.Disposition != Cancelled || original.Reason != ReasonOperatorSourceRefresh {
		t.Fatal("refresh did not preserve distinct durable intent")
	}
	retry, err := supervisor.RefreshIssueRun(context.Background(), started.RunID)
	if err != nil || retry.RunID != refreshed.RunID {
		t.Fatalf("refresh retry created new work: %+v %v", retry, err)
	}
	if err := supervisor.RequestRunReconciliation(started.RunID); err == nil {
		t.Fatal("canceled run accepted resume")
	}
}
