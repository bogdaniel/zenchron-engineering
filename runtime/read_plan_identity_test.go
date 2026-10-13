package runtime

import (
	"testing"
	"time"
)

func TestPlanPageRetainsAttemptIdentityWithoutInventingARevision(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.ClaimPlanAttempt("planning-only", "acme/repo", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	page, more, err := reader.PlanSummariesPage(0, 1)
	if err != nil || more || len(page) != 1 || page[0].PlanID != "planning-only" || page[0].Revision != 0 || page[0].Error == "" {
		t.Fatalf("attempt identity lost or fabricated: %+v more=%t err=%v", page, more, err)
	}
	identity, found, err := reader.PlanIdentity("planning-only")
	if err != nil || !found || identity.Revision != 0 || identity.Repository != "acme/repo" {
		t.Fatalf("identity: %+v found=%t err=%v", identity, found, err)
	}
}
