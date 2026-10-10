package product

import (
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func baseConfiguration(t *testing.T, productID string, revision int) ProductConfiguration {
	t.Helper()
	c := ProductConfiguration{
		SchemaVersion: ConfigurationSchemaVersion, ProductID: productID, Revision: revision,
		Policy:    &domain.ObjectRevision{ID: "policy-acme", Revision: "3"},
		CreatedAt: time.Unix(1700000000, 0).UTC(),
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("base configuration should be valid: %v", err)
	}
	return c
}

func TestProductConfigurationValidateRefusesMalformedData(t *testing.T) {
	base := baseConfiguration(t, "product-acme", 1)
	cases := map[string]ProductConfiguration{
		"wrong schema version": func() ProductConfiguration { c := base; c.SchemaVersion = "9.9"; return c }(),
		"no product id":        func() ProductConfiguration { c := base; c.ProductID = ""; return c }(),
		"revision zero":        func() ProductConfiguration { c := base; c.Revision = 0; return c }(),
		"no created time":      func() ProductConfiguration { c := base; c.CreatedAt = time.Time{}; return c }(),
		"policy missing revision": func() ProductConfiguration {
			c := base
			c.Policy = &domain.ObjectRevision{ID: "policy-acme"}
			return c
		}(),
		"too many settings": func() ProductConfiguration {
			c := base
			c.Settings = map[string]string{}
			for i := 0; i < MaxConfigurationSettings+1; i++ {
				c.Settings[string(rune('a'+i))] = "v"
			}
			return c
		}(),
	}
	for name, configuration := range cases {
		t.Run(name, func(t *testing.T) {
			if err := configuration.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", name)
			}
		})
	}
}

func TestValidateConfigurationMutationDetectsStaleRevisions(t *testing.T) {
	current := baseConfiguration(t, "product-acme", 2)
	next := baseConfiguration(t, "product-acme", 3)
	if err := ValidateConfigurationMutation(current, next); err != nil {
		t.Fatalf("the immediate successor revision should be valid: %v", err)
	}

	stale := baseConfiguration(t, "product-acme", 2)
	if err := ValidateConfigurationMutation(current, stale); err == nil {
		t.Fatal("resubmitting the current revision number should be refused as stale")
	}

	skipped := baseConfiguration(t, "product-acme", 4)
	if err := ValidateConfigurationMutation(current, skipped); err == nil {
		t.Fatal("skipping a revision number should be refused")
	}

	otherProduct := baseConfiguration(t, "product-other", 3)
	if err := ValidateConfigurationMutation(current, otherProduct); err == nil {
		t.Fatal("a configuration naming a different product should be refused")
	}
}

func TestProductConfigurationRevisionDigestIsStableUnderFieldOrder(t *testing.T) {
	a := baseConfiguration(t, "product-acme", 1)
	a.Settings = map[string]string{"x": "1", "y": "2"}
	b := a
	b.Settings = map[string]string{"y": "2", "x": "1"}
	digestA, err := a.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	digestB, err := b.RevisionDigest()
	if err != nil {
		t.Fatal(err)
	}
	if digestA != digestB {
		t.Fatal("map iteration order should not change the revision digest")
	}
}
