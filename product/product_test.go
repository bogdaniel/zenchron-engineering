package product

import (
	"testing"
	"time"
)

func composedProduct(t *testing.T, name string, revision int, repositories []string) Product {
	t.Helper()
	p, err := ProductProposal{Name: name, Revision: revision, Repositories: repositories}.
		Compose("operator@example", time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	return p
}

func TestProductIdentityIsDerivedFromName(t *testing.T) {
	first := composedProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"})
	second, err := ProductProposal{Name: "acme-storefront", Revision: 1, Repositories: []string{"acme/storefront-web"}}.
		Compose("someone-else@example", time.Unix(1800000000, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID {
		t.Fatalf("the same product name produced different identities: %s then %s", first.ID, second.ID)
	}
	other := composedProduct(t, "acme-billing", 1, []string{"acme/billing-api"})
	if other.ID == first.ID {
		t.Fatalf("two different products share identity %s", first.ID)
	}
}

func TestProductValidateRefusesMalformedData(t *testing.T) {
	base := composedProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"})
	cases := map[string]Product{
		"wrong schema version": func() Product { p := base; p.SchemaVersion = "9.9"; return p }(),
		"id does not match name": func() Product {
			p := base
			p.ID = "product-deadbeef"
			return p
		}(),
		"revision zero":   func() Product { p := base; p.Revision = 0; return p }(),
		"no created time": func() Product { p := base; p.CreatedAt = time.Time{}; return p }(),
		"no repositories": func() Product { p := base; p.Repositories = nil; return p }(),
		"duplicate repository": func() Product {
			p := base
			p.Repositories = []string{"acme/storefront-web", "acme/storefront-web"}
			return p
		}(),
		"blank repository": func() Product {
			p := base
			p.Repositories = []string{"  "}
			return p
		}(),
	}
	for name, product := range cases {
		t.Run(name, func(t *testing.T) {
			if err := product.Validate(); err == nil {
				t.Fatalf("%s: expected a validation error", name)
			}
		})
	}
}

func TestValidateProductMutationIsAppendOnly(t *testing.T) {
	current := composedProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"})
	adding, err := ProductProposal{Name: "acme-storefront", Revision: 2,
		Repositories: []string{"acme/storefront-web", "acme/storefront-mobile"}}.
		Compose("operator@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProductMutation(current, adding); err != nil {
		t.Fatalf("adding a repository should be a valid mutation: %v", err)
	}

	removing, err := ProductProposal{Name: "acme-storefront", Revision: 2,
		Repositories: []string{"acme/storefront-mobile"}}.
		Compose("operator@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProductMutation(current, removing); err == nil {
		t.Fatal("removing an already-named repository should be refused")
	}

	staleRevision, err := ProductProposal{Name: "acme-storefront", Revision: 1,
		Repositories: []string{"acme/storefront-web"}}.
		Compose("operator@example", time.Unix(1700000100, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateProductMutation(current, staleRevision); err == nil {
		t.Fatal("a stale (non-successor) revision should be refused")
	}

	differentProduct := composedProduct(t, "acme-billing", 2, []string{"acme/billing-api"})
	if err := ValidateProductMutation(current, differentProduct); err == nil {
		t.Fatal("a revision naming a different product should be refused")
	}
}
