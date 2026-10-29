package runtime

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func TestPlanSummariesPageBoundsReadsAndPreservesLifecycleOwner(t *testing.T) {
	dir := t.TempDir()
	writer, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for i := range 3 {
		plan := domain.EngineeringPlan{SchemaVersion: domain.SchemaVersion, ID: fmt.Sprintf("plan-%d", i), Revision: 1, Objective: "bounded work", Subject: domain.Subject{Repository: "acme/repo"}}
		plan.Digest, err = plan.ContentDigest()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := writer.ClaimPlan(plan, time.Unix(int64(i+1), 0).UTC()); err != nil {
			t.Fatal(err)
		}
	}
	reader, err := OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	first, more, err := reader.PlanSummariesPage(0, 2)
	if err != nil || !more || len(first) != 2 || first[0].PlanID != "plan-0" || first[1].PlanID != "plan-1" {
		t.Fatalf("first page: %+v more=%t err=%v", first, more, err)
	}
	last, more, err := reader.PlanSummariesPage(2, 2)
	if err != nil || more || len(last) != 1 || last[0].PlanID != "plan-2" {
		t.Fatalf("last page: %+v more=%t err=%v", last, more, err)
	}
	for _, summary := range append(first, last...) {
		one, err := reader.PlanSummary(summary.PlanID)
		if err != nil || one.State != PlanStateAwaitingApproval || one.State != summary.State {
			t.Fatalf("lifecycle differs: %+v / %+v err=%v", summary, one, err)
		}
	}
	if _, _, err := reader.PlanSummariesPage(-1, 2); err == nil {
		t.Fatal("negative page accepted")
	}
	if _, _, err := reader.PlanSummariesPage(0, 501); err == nil {
		t.Fatal("unbounded page accepted")
	}
	third, found, err := writer.Plan("plan-2")
	if err != nil || !found {
		t.Fatalf("third plan missing before corruption: found=%t err=%v", found, err)
	}
	validDocument, err := json.Marshal(third)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.db.Exec(`UPDATE plan_revisions SET document = 'malformed' WHERE plan_id = 'plan-2'`); err != nil {
		t.Fatal(err)
	}
	if first, more, err := reader.PlanSummariesPage(0, 1); err != nil || !more || len(first) != 1 {
		t.Fatalf("off-page document read: %+v more=%t err=%v", first, more, err)
	}
	if _, _, err := reader.PlanSummariesPage(2, 1); err == nil {
		t.Fatal("visible corrupt plan silently omitted")
	}
	if _, err := writer.db.Exec(`UPDATE plan_revisions SET document = ? WHERE plan_id = 'plan-2'`, string(validDocument)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.db.Exec(`UPDATE plans SET current_revision = 2 WHERE id = 'plan-1'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reader.PlanSummariesPage(1, 1); err == nil {
		t.Fatal("missing revision silently omitted")
	}
}
