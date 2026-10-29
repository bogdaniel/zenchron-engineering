package product

import (
	"testing"
	"time"
)

func TestNewGraphAssociationRefusesMissingFields(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	if _, err := NewGraphAssociation("", "graph-1", at); err == nil {
		t.Fatal("a missing product id should be refused")
	}
	if _, err := NewGraphAssociation("product-acme", "", at); err == nil {
		t.Fatal("a missing graph id should be refused")
	}
	if _, err := NewGraphAssociation("product-acme", "graph-1", time.Time{}); err == nil {
		t.Fatal("a missing association time should be refused")
	}
	association, err := NewGraphAssociation("product-acme", "graph-1", at)
	if err != nil {
		t.Fatal(err)
	}
	if association.ProductID != "product-acme" || association.GraphID != "graph-1" {
		t.Fatalf("unexpected association: %+v", association)
	}
}
