package controlplane

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestPlanIndexIsBoundedSearchableAndKeepsAttemptsNonPending(t *testing.T) {
	f := newActionFixture(t)
	for i := range 101 {
		if _, err := f.store.ClaimPlanAttempt(fmt.Sprintf("attempt-%03d", i), "other/repo", time.Unix(int64(i+200), 0)); err != nil {
			t.Fatal(err)
		}
	}
	var index PlanIndex
	decode := func(path string) {
		t.Helper()
		if err := json.Unmarshal(wire(t, f.api, path, "GET", f.api.Token, "plan-index", 200), &index); err != nil {
			t.Fatal(err)
		}
	}
	decode("/v1/plans?limit=50")
	if len(index.Plans) != 50 || !index.HasMore || index.Plans[0].PlanID != "attempt-100" {
		t.Fatalf("bounded ordered page: %+v", index)
	}
	for _, row := range index.Plans {
		if row.Pending || row.Shown != "attempt_only" {
			t.Fatalf("attempts cannot request a plan decision: %+v", row)
		}
	}
	decode("/v1/plans?offset=100&limit=50")
	if len(index.Plans) != 2 || index.HasMore || index.Plans[1].PlanID != "p" || !index.Plans[1].Pending {
		t.Fatalf("last page: %+v", index)
	}
	if index.Plans[1].Lifecycle == nil || index.Plans[1].Lifecycle.State != rt.PlanStateAwaitingApproval {
		t.Fatal("canonical plan lifecycle was lost from the portfolio")
	}
	decode("/v1/plans?q=example%2Frepo")
	if len(index.Plans) != 1 || index.Plans[0].PlanID != "p" {
		t.Fatalf("search must query persisted index, not first page: %+v", index)
	}
	for _, path := range []string{"/v1/plans?limit=51", "/v1/plans?limit=0", "/v1/plans?offset=-1", "/v1/plans?q=x&q=y"} {
		wire(t, f.api, path, "GET", f.api.Token, "error", 400)
	}
	wire(t, f.api, "/v1/plans", "GET", "", "error", 401)
}

func TestPlanContextReceiptsAreHistoricalExactAndPrivate(t *testing.T) {
	f := newActionFixture(t)
	var context PlanContext
	read := func() {
		t.Helper()
		if err := json.Unmarshal(wire(t, f.api, "/v1/plans/p/context?revision=1", "GET", f.api.Token, "plan-context", 200), &context); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if !context.Pending || context.Receipt != nil || context.Plan.Digest != f.digest {
		t.Fatalf("pending subject: %+v", context)
	}
	if _, err := (rt.PlanService{Store: f.store}).Reject("p", 1, f.digest, "", "SECRET_OPERATOR", "SECRET_NOTE"); err != nil {
		t.Fatal(err)
	}
	read()
	if context.Pending || context.Receipt == nil || context.Receipt.Status != "rejected" || context.Receipt.Revision != 1 || context.Receipt.Digest != f.digest || context.Receipt.Sequence < 1 {
		t.Fatalf("exact historical receipt: %+v", context)
	}
	// Objective prose, notes, operator identities, payloads and local paths
	// retain the existing sanitized wire boundary.
	private := fixture(t, true)
	wire(t, private, "/v1/plans", "GET", private.Token, "plan-index", 200)
	wire(t, private, "/v1/plans/p/context", "GET", private.Token, "plan-context", 200)
	wire(t, f.api, "/v1/plans/p/context?revision=9", "GET", f.api.Token, "error", 404)
	wire(t, f.api, "/v1/plans/p/context?revision=-1", "GET", f.api.Token, "error", 400)
	wire(t, f.api, "/v1/plans/p/context", "GET", "", "error", 401)
}
