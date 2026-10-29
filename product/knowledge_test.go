package product

import (
	"testing"
	"time"
)

func TestNewKnowledgeEntryRefusesScopesAboveWorkUnit(t *testing.T) {
	_, err := NewKnowledgeEntry("product-acme", ScopeExecution, "run-7", "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("execution scope should be authorable directly: %v", err)
	}
	_, err = NewKnowledgeEntry("product-acme", ScopeWorkUnit, "run-7", "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("work_unit scope should be authorable directly: %v", err)
	}
	for _, scope := range []Scope{ScopeFeature, ScopeProduct, ScopeOrganization} {
		if _, err := NewKnowledgeEntry("product-acme", scope, "run-7", "the billing worker retries on 429",
			Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC()); err == nil {
			t.Fatalf("scope %q should require promotion, not direct authoring", scope)
		}
	}
}

func TestNewKnowledgeEntryRefusesPromotionProvenance(t *testing.T) {
	_, err := NewKnowledgeEntry("product-acme", ScopeExecution, "run-7", "claim",
		Provenance{Type: ProvenancePromotion, Producer: "someone"}, time.Unix(1700000000, 0).UTC())
	if err == nil {
		t.Fatal("a directly authored entry may not claim promotion provenance")
	}
}

func TestNewKnowledgeEntryRequiresAnOwnerReference(t *testing.T) {
	if _, err := NewKnowledgeEntry("product-acme", ScopeWorkUnit, "", "claim",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC()); err == nil {
		t.Fatal("a work_unit-scoped entry with no owner reference should be refused")
	}
}

func discoveredEntry(t *testing.T) KnowledgeEntry {
	t.Helper()
	entry, err := NewKnowledgeEntry("product-acme", ScopeWorkUnit, "run-7", "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

// executionScopedEntry is a valid Promote SOURCE for a work_unit-scope
// target: discoveredEntry is already at work_unit scope, one rung too high.
func executionScopedEntry(t *testing.T) KnowledgeEntry {
	t.Helper()
	entry, err := NewKnowledgeEntry("product-acme", ScopeExecution, "run-7", "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestPromoteRequiresAnExplicitAuthorizer(t *testing.T) {
	entry := discoveredEntry(t)
	if _, _, err := Promote(entry, ScopeProduct, "", "", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promotion with no authorizer should be refused as unauthorized scope widening")
	}
}

func TestPromoteOnlyMovesUpTheLadder(t *testing.T) {
	entry := discoveredEntry(t)
	if _, _, err := Promote(entry, ScopeExecution, "", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to a lower or equal scope should be refused")
	}
	if _, _, err := Promote(entry, ScopeWorkUnit, "unit-a", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to the same scope should be refused")
	}
	if _, _, err := Promote(entry, Scope("galaxy"), "", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to an unrecognized scope should be refused")
	}
}

// TestPromoteRequiresAnAudienceBelowProductScope is R2: a scope rank alone is
// not an audience. work_unit and feature promotions must name who they are
// for; product and organization promotions must not, since they have no
// narrower audience to restrict to.
func TestPromoteRequiresAnAudienceBelowProductScope(t *testing.T) {
	entry := discoveredEntry(t)
	if _, _, err := Promote(entry, ScopeWorkUnit, "", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to work_unit scope with no audience reference should be refused")
	}
	if _, _, err := Promote(entry, ScopeFeature, "", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to feature scope with no audience reference should be refused")
	}
	if _, _, err := Promote(entry, ScopeProduct, "unit-b", "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to product scope with an audience reference should be refused; it has no narrower audience")
	}
}

func TestPromotePreservesProvenanceAndLinksBack(t *testing.T) {
	entry := discoveredEntry(t)
	promoted, record, err := Promote(entry, ScopeProduct, "", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Provenance.Type != ProvenancePromotion || promoted.Provenance.Producer != "lead@example" {
		t.Fatalf("promoted entry should carry promotion provenance by the authorizer, got %+v", promoted.Provenance)
	}
	if promoted.OwnerRef != "" {
		t.Fatalf("a promoted entry should carry no owner reference, got %q", promoted.OwnerRef)
	}
	if promoted.PromotedFrom == nil || *promoted.PromotedFrom != entry.ID {
		t.Fatalf("promoted entry should link back to its source, got %+v", promoted.PromotedFrom)
	}
	if promoted.Statement != entry.Statement {
		t.Fatal("promotion must not alter the claim's content")
	}
	if record.SourceProvenance.Type != ProvenanceAgentDiscovery || record.SourceProvenance.Producer != "run-7" {
		t.Fatalf("the promotion record should preserve the ORIGINAL provenance, got %+v", record.SourceProvenance)
	}
	if record.SourceEntryID != entry.ID || record.PromotedEntryID != promoted.ID {
		t.Fatal("the promotion record should bind the exact source and resulting entry")
	}
	if err := promoted.Validate(); err != nil {
		t.Fatalf("promoted entry should validate: %v", err)
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("promotion record should validate: %v", err)
	}
}

// TestPromoteToWorkUnitScopeNamesOneAudience proves a work_unit-scope
// promotion carries the exact audience it was promoted for, on both the
// entry and its audit record - what makes it possible for a store to refuse
// handing it to any OTHER work unit.
func TestPromoteToWorkUnitScopeNamesOneAudience(t *testing.T) {
	entry := executionScopedEntry(t)
	promoted, record, err := Promote(entry, ScopeWorkUnit, "unit-a", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if promoted.AudienceRef != "unit-a" || record.AudienceRef != "unit-a" {
		t.Fatalf("promoted entry and record should both name the audience, got entry=%q record=%q",
			promoted.AudienceRef, record.AudienceRef)
	}
}

func TestKnowledgeEntryValidateRefusesMalformedData(t *testing.T) {
	entry := discoveredEntry(t)
	cases := map[string]KnowledgeEntry{
		"unknown scope": func() KnowledgeEntry { e := entry; e.Scope = "galaxy"; return e }(),
		"unknown provenance type": func() KnowledgeEntry {
			e := entry
			e.Provenance.Type = "vibes"
			return e
		}(),
		"no created time": func() KnowledgeEntry { e := entry; e.CreatedAt = time.Time{}; return e }(),
		"promotion without source": func() KnowledgeEntry {
			e := entry
			e.Provenance.Type = ProvenancePromotion
			return e
		}(),
		"source on a non-promoted entry": func() KnowledgeEntry {
			e := entry
			id := "knowledge-other"
			e.PromotedFrom = &id
			return e
		}(),
		"no owner reference at work_unit scope": func() KnowledgeEntry { e := entry; e.OwnerRef = ""; return e }(),
		"owner reference on a feature-scoped entry": func() KnowledgeEntry {
			e := entry
			e.Scope = ScopeFeature
			return e
		}(),
		"audience reference on a directly authored entry": func() KnowledgeEntry {
			e := entry
			e.AudienceRef = "unit-a"
			return e
		}(),
		"forged id": func() KnowledgeEntry { e := entry; e.ID = "knowledge-forged"; return e }(),
		"tampered statement after id was computed": func() KnowledgeEntry {
			e := entry
			e.Statement = "the billing worker retries on 500"
			return e
		}(),
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", name)
			}
		})
	}
}

func TestPromotedEntryValidateRefusesMalformedAudience(t *testing.T) {
	entry := executionScopedEntry(t)
	promoted, _, err := Promote(entry, ScopeWorkUnit, "unit-a", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	noAudience := promoted
	noAudience.AudienceRef = ""
	if err := noAudience.Validate(); err == nil {
		t.Fatal("a work_unit-scope promoted entry with no audience reference should be refused")
	}

	widePromotion, _, err := Promote(entry, ScopeProduct, "", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	withAudience := widePromotion
	withAudience.AudienceRef = "unit-a"
	if err := withAudience.Validate(); err == nil {
		t.Fatal("a product-scope promoted entry with an audience reference should be refused")
	}
}

func TestKnowledgePromotionValidateRefusesAForgedID(t *testing.T) {
	entry := discoveredEntry(t)
	_, record, err := Promote(entry, ScopeProduct, "", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	record.ID = "promotion-forged"
	if err := record.Validate(); err == nil {
		t.Fatal("a promotion record with a tampered id should be refused")
	}
}

// forgedPromotion re-identifies a tampered KnowledgePromotion by recomputing
// its OWN content id for whatever fields the caller just mutated, so
// Validate() alone cannot catch the tamper - only cross-checking it against
// the promoted entry it claims to audit (ValidatePromotionConsistency) can.
// This is what makes the adversarial tests below a real test of R1's causal
// cross-check rather than a restatement of the id-integrity check.
func forgedPromotion(t *testing.T, r KnowledgePromotion) KnowledgePromotion {
	t.Helper()
	id, err := r.contentID()
	if err != nil {
		t.Fatal(err)
	}
	r.ID = id
	return r
}

func TestValidatePromotionConsistencyRefusesCausallyMismatchedDocuments(t *testing.T) {
	entry := discoveredEntry(t)
	promoted, record, err := Promote(entry, ScopeProduct, "", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidatePromotionConsistency(promoted, record); err != nil {
		t.Fatalf("an untampered pair should be consistent: %v", err)
	}

	mismatchedScope := record
	mismatchedScope.TargetScope = ScopeOrganization
	mismatchedScope = forgedPromotion(t, mismatchedScope)
	if err := mismatchedScope.Validate(); err != nil {
		t.Fatalf("the forged record should independently validate: %v", err)
	}
	if err := ValidatePromotionConsistency(promoted, mismatchedScope); err == nil {
		t.Fatal("a record whose target scope disagrees with the promoted entry's own scope should be refused")
	}

	mismatchedAuthorizer := record
	mismatchedAuthorizer.AuthorizedBy = "someone-else@example"
	mismatchedAuthorizer = forgedPromotion(t, mismatchedAuthorizer)
	if err := ValidatePromotionConsistency(promoted, mismatchedAuthorizer); err == nil {
		t.Fatal("a record whose authorizer disagrees with the promoted entry's producer should be refused")
	}

	mismatchedTime := record
	mismatchedTime.PromotedAt = record.PromotedAt.Add(time.Minute)
	mismatchedTime = forgedPromotion(t, mismatchedTime)
	if err := ValidatePromotionConsistency(promoted, mismatchedTime); err == nil {
		t.Fatal("a record whose promoted-at time disagrees with the promoted entry's creation time should be refused")
	}
}

func TestValidatePromotionConsistencyRefusesMismatchedAudience(t *testing.T) {
	entry := executionScopedEntry(t)
	promoted, record, err := Promote(entry, ScopeWorkUnit, "unit-a", "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	mismatchedAudience := record
	mismatchedAudience.AudienceRef = "unit-b"
	mismatchedAudience = forgedPromotion(t, mismatchedAudience)
	if err := ValidatePromotionConsistency(promoted, mismatchedAudience); err == nil {
		t.Fatal("a record whose audience disagrees with the promoted entry's own audience should be refused")
	}
}
