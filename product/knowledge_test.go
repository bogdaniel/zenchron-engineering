package product

import (
	"testing"
	"time"
)

func TestNewKnowledgeEntryRefusesScopesAboveWorkUnit(t *testing.T) {
	_, err := NewKnowledgeEntry("product-acme", ScopeExecution, "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("execution scope should be authorable directly: %v", err)
	}
	_, err = NewKnowledgeEntry("product-acme", ScopeWorkUnit, "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("work_unit scope should be authorable directly: %v", err)
	}
	for _, scope := range []Scope{ScopeFeature, ScopeProduct, ScopeOrganization} {
		if _, err := NewKnowledgeEntry("product-acme", scope, "the billing worker retries on 429",
			Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC()); err == nil {
			t.Fatalf("scope %q should require promotion, not direct authoring", scope)
		}
	}
}

func TestNewKnowledgeEntryRefusesPromotionProvenance(t *testing.T) {
	_, err := NewKnowledgeEntry("product-acme", ScopeExecution, "claim",
		Provenance{Type: ProvenancePromotion, Producer: "someone"}, time.Unix(1700000000, 0).UTC())
	if err == nil {
		t.Fatal("a directly authored entry may not claim promotion provenance")
	}
}

func discoveredEntry(t *testing.T) KnowledgeEntry {
	t.Helper()
	entry, err := NewKnowledgeEntry("product-acme", ScopeWorkUnit, "the billing worker retries on 429",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "run-7"}, time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	return entry
}

func TestPromoteRequiresAnExplicitAuthorizer(t *testing.T) {
	entry := discoveredEntry(t)
	if _, _, err := Promote(entry, ScopeProduct, "", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promotion with no authorizer should be refused as unauthorized scope widening")
	}
}

func TestPromoteOnlyMovesUpTheLadder(t *testing.T) {
	entry := discoveredEntry(t)
	if _, _, err := Promote(entry, ScopeExecution, "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to a lower or equal scope should be refused")
	}
	if _, _, err := Promote(entry, ScopeWorkUnit, "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to the same scope should be refused")
	}
	if _, _, err := Promote(entry, Scope("galaxy"), "lead@example", time.Unix(1700000100, 0).UTC()); err == nil {
		t.Fatal("promoting to an unrecognized scope should be refused")
	}
}

func TestPromotePreservesProvenanceAndLinksBack(t *testing.T) {
	entry := discoveredEntry(t)
	promoted, record, err := Promote(entry, ScopeProduct, "lead@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Provenance.Type != ProvenancePromotion || promoted.Provenance.Producer != "lead@example" {
		t.Fatalf("promoted entry should carry promotion provenance by the authorizer, got %+v", promoted.Provenance)
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
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if err := e.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", name)
			}
		})
	}
}
