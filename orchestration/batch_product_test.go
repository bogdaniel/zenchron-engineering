package orchestration

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/product"
)

func unitBatch(t *testing.T, pin *product.ContextPin) Batch {
	t.Helper()
	origin := BatchOrigin{GraphID: "graph-1", UnitID: "a"}
	id, err := WorkUnitBatchID("acme/repo", "claude", 7, origin)
	if err != nil {
		t.Fatal(err)
	}
	return Batch{SchemaVersion: BatchSchemaVersion, ID: id, Repository: "acme/repo", AgentID: "claude",
		CreatedAt: time.Unix(1700000000, 0).UTC(), Origin: &origin, Items: []BatchItem{{Issue: 7, RunID: "run-1"}}, Product: pin}
}

func TestAProductPinIsNotPartOfTheBatchIdentity(t *testing.T) {
	pin, err := product.NewContextPin(product.ProductContext{ProductID: "product-1"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, plain := unitBatch(t, &pin), unitBatch(t, nil)
	if pinned.ID != plain.ID {
		t.Fatal("a newer configuration must not name a second batch for the same unit execution")
	}
	if err := pinned.Validate(); err != nil {
		t.Fatalf("a pinned unit batch must validate: %v", err)
	}
}

func TestABatchRefusesAMalformedOrOrphanedProductPin(t *testing.T) {
	pin, err := product.NewContextPin(product.ProductContext{ProductID: "product-1"})
	if err != nil {
		t.Fatal(err)
	}
	corrupt := pin
	corrupt.ContextDigest = "not-the-digest"
	if err := unitBatch(t, &corrupt).Validate(); err == nil {
		t.Fatal("a batch carrying a corrupted pin must be refused")
	}
	direct := unitBatch(t, &pin)
	direct.Origin = nil
	direct.ID, err = BatchID("acme/repo", "claude", []int{7})
	if err != nil {
		t.Fatal(err)
	}
	if err := direct.Validate(); err == nil {
		t.Fatal("a direct batch has no graph and cannot carry a product pin")
	}
}
