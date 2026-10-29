package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// A revision of a plan held for a configuration change is refused before
// anything is spent on it. The composition here carries no engine, so reaching
// the forge read or the planning invocation would panic rather than refuse.
func TestRevisingAHeldPlanSpendsNothing(t *testing.T) {
	store, err := runtime.OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.ClaimPlanAttempt("plan-x", "acme/repo", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := store.BindPlanSource("plan-x", 7); err != nil {
		t.Fatal(err)
	}
	if err := store.BindPlanConfig("plan-x", runtime.ConfigDigest{Global: "config-x"}); err != nil {
		t.Fatal(err)
	}
	composed := &planComposition{service: runtime.PlanService{Store: store, Config: runtime.ConfigDigest{Global: "config-y"}}}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("the revision proceeded past the hold: %v", recovered)
		}
	}()
	_, err = proposeSerialized(context.Background(), composed, autonomyFlags{}, 7, "plan-x", io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "configuration_changed") {
		t.Fatalf("revising a held plan was not refused: %v", err)
	}
}
