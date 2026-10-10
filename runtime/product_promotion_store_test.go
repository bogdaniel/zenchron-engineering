package runtime

// Knowledge entry and promotion persistence (#476), proved against the real
// SQLite store: a promoted entry is only ever written through
// RecordPromotion, together with an audit record that is cross-checked
// against it (not merely independently valid), and visibility below product
// scope is restricted to the exact audience a promotion named.

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/product"
)

func TestRecordKnowledgeEntryRefusesPromotionProvenanceAndMissingProduct(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}

	source, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	forged, _, err := product.Promote(source, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	// Writing a "promoted" entry through the DIRECT path, with no audit
	// record, is the B2 bypass this refuses.
	if _, err := first.RecordKnowledgeEntry(forged); err == nil {
		t.Fatal("RecordKnowledgeEntry should refuse promotion-provenance entries; they must go through RecordPromotion")
	}

	orphan, err := product.NewKnowledgeEntry("product-does-not-exist", product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(orphan); err == nil {
		t.Fatal("recording knowledge for a product that was never adopted should be refused")
	}
}

func TestRecordPromotionRefusesAMismatchedEntryAndRecord(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(discovered, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	record.PromotedEntryID = "knowledge-not-the-one-above"
	if _, err := first.RecordPromotion(promoted, record); err == nil {
		t.Fatal("a promotion record naming a different entry than the one given should be refused")
	}
}

// TestRecordPromotionRefusesASourceThatWasNeverRecorded is B2: a caller
// cannot fabricate a promotion whose claimed source entry does not actually
// exist in storage.
func TestRecordPromotionRefusesASourceThatWasNeverRecorded(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Never recorded through RecordKnowledgeEntry: nothing in storage backs it.
	promoted, record, err := product.Promote(discovered, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordPromotion(promoted, record); err == nil {
		t.Fatal("promoting a source entry that was never recorded should be refused")
	}
}

// TestRecordPromotionRefusesATamperedProductID is defense in depth for B2's
// cross-product case. Promote() always sets a promoted entry's ProductID from
// its real source, and content-derived ids make that binding tamper-evident:
// RecordPromotion's own product/scope/provenance cross-check against the
// stored source (product_knowledge_store.go) would also catch an untampered
// forgery naming a foreign-product source directly - this proves the id
// check alone already refuses the easier attack of editing the field in place.
func TestRecordPromotionRefusesATamperedProductID(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	productB := composeProduct(t, "checkout", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AdoptProductRevision(productB); err != nil {
		t.Fatal(err)
	}

	sourceInA, err := product.NewKnowledgeEntry(productA.ID, product.ScopeWorkUnit, "run-a", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(sourceInA); err != nil {
		t.Fatal(err)
	}

	promoted, record, err := product.Promote(sourceInA, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	promoted.ProductID = productB.ID
	if _, err := first.RecordPromotion(promoted, record); err == nil {
		t.Fatal("retargeting a promoted entry at a different product after construction should be refused")
	}
}

// TestRecordPromotionIsIdempotentOnExactReplay proves the ordinary case R1
// protects: replaying the EXACT same Promote output twice is a no-op, not a
// second row or a conflict.
func TestRecordPromotionIsIdempotentOnExactReplay(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(discovered); err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(discovered, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := first.RecordPromotion(promoted, record)
	if err != nil || !created {
		t.Fatalf("first promotion: created=%t err=%v", created, err)
	}
	replayed, err := second.RecordPromotion(promoted, record)
	if err != nil || replayed {
		t.Fatalf("exact replay should be a no-op: created=%t err=%v", replayed, err)
	}
	var rows int
	if err := first.db.QueryRow(`SELECT COUNT(1) FROM product_knowledge_promotions WHERE id = ?`, record.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("expected exactly one audit row for %s, found %d", record.ID, rows)
	}
}

// TestRecordPromotionRefusesAConflictingStoredAuditDocument is R1's
// transactional defense in depth: a stored audit row that disagrees with
// what a legitimate replay of this exact promotion id would write - whether
// from corruption or a write that bypassed RecordPromotion - is refused
// rather than silently left in place while a new, differently-audited
// promoted entry commits underneath it.
func TestRecordPromotionRefusesAConflictingStoredAuditDocument(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "run-1", "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(discovered); err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(discovered, product.ScopeProduct, "", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordPromotion(promoted, record); err != nil {
		t.Fatal(err)
	}

	if _, err := first.db.Exec(`UPDATE product_knowledge_promotions SET document = ? WHERE id = ?`,
		`{"schema_version":"0.1","id":"`+record.ID+`","tampered":true}`, record.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := first.RecordPromotion(promoted, record); err == nil {
		t.Fatal("a replay whose id matches a differently-recorded audit document should be refused, not silently ignored")
	}
}

// TestContextRestrictsAWorkUnitPromotionToItsNamedAudience is R2's real-store
// regression: execution A -> promotion to work_unit A must be invisible to
// work unit B in the same product.
func TestContextRestrictsAWorkUnitPromotionToItsNamedAudience(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	executionClaim, err := product.NewKnowledgeEntry(p.ID, product.ScopeExecution, "unit-a", "a narrow execution detail",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "unit-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(executionClaim); err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(executionClaim, product.ScopeWorkUnit, "unit-a", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordPromotion(promoted, record); err != nil {
		t.Fatal(err)
	}

	forUnitA, err := product.CompileContext(first, product.ContextRequest{ProductID: p.ID, Scope: product.ScopeWorkUnit, OwnerRef: "unit-a"})
	if err != nil {
		t.Fatal(err)
	}
	if !containsKnowledgeID(forUnitA.Knowledge, promoted.ID) {
		t.Fatal("unit A should see the promotion that named it as the audience")
	}

	forUnitB, err := product.CompileContext(first, product.ContextRequest{ProductID: p.ID, Scope: product.ScopeWorkUnit, OwnerRef: "unit-b"})
	if err != nil {
		t.Fatal(err)
	}
	if containsKnowledgeID(forUnitB.Knowledge, promoted.ID) {
		t.Fatal("unit B must not see a promotion named for unit A - a scope rank alone is not an audience")
	}
}

// TestContextRestrictsAFeaturePromotionToItsNamedAudience covers the same
// claim for two independent features.
func TestContextRestrictsAFeaturePromotionToItsNamedAudience(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	claim, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "unit-a", "a feature-relevant detail",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "unit-a"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordKnowledgeEntry(claim); err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(claim, product.ScopeFeature, "feature-checkout-redesign", "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.RecordPromotion(promoted, record); err != nil {
		t.Fatal(err)
	}

	forItsFeature, err := product.CompileContext(first, product.ContextRequest{
		ProductID: p.ID, Scope: product.ScopeFeature, FeatureRef: "feature-checkout-redesign",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !containsKnowledgeID(forItsFeature.Knowledge, promoted.ID) {
		t.Fatal("the named feature should see its own promotion")
	}

	forAnotherFeature, err := product.CompileContext(first, product.ContextRequest{
		ProductID: p.ID, Scope: product.ScopeFeature, FeatureRef: "feature-payments-refresh",
	})
	if err != nil {
		t.Fatal(err)
	}
	if containsKnowledgeID(forAnotherFeature.Knowledge, promoted.ID) {
		t.Fatal("an unrelated feature must not see a promotion named for a different feature")
	}
}
