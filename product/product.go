package product

// Product (#476): the durable identity of one ProductEngineeringEnvironment.
//
// A Product names which repositories belong to it and nothing else about how
// work against them proceeds - WorkGraphs, plans and runs are associated with
// a Product (association.go) rather than embedded in it, so claiming a graph
// never requires a new product revision and a product revision never has to
// wait on execution state.
//
// Revisioning mirrors the WorkGraph document (orchestration/workgraph.go): a
// revision is written once and kept, identity is content-derived from the
// product's name, and a mutation may only ever ADD a repository, never remove
// or rebind one a graph may already be associated against.

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// ProductSchemaVersion versions the durable product revision document.
const ProductSchemaVersion = "0.1"

// MaxProductRepositories bounds one product. A product is an operator-composed
// boundary, not a discovery result, so it is bounded like any other proposal
// a control request carries.
const MaxProductRepositories = 64

const (
	maxProductNameBytes = 200
	maxRepositoryBytes  = 200
)

// Product is one revision of a product's identity.
type Product struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	// Name is the operator's stable name for this product. Identity is derived
	// from it, mirroring WorkGraphID, so resubmitting the same product finds
	// the one already adopted rather than creating a second.
	Name         string    `json:"name"`
	Revision     int       `json:"revision"`
	Repositories []string  `json:"repositories"`
	RequestedBy  string    `json:"requested_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ProductID is the deterministic identity of a product: the same name always
// names the same product across revisions.
func ProductID(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", errors.New("a product needs a name")
	}
	if len(name) > maxProductNameBytes {
		return "", fmt.Errorf("product name is %d bytes, above the %d byte bound", len(name), maxProductNameBytes)
	}
	digest, err := domain.Digest(struct {
		Name string `json:"name"`
	}{name})
	if err != nil {
		return "", err
	}
	return "product-" + digest[:32], nil
}

// RevisionDigest is the CONTENT identity of one revision: its product and
// revision number and its repository list, canonically ordered.
func (p Product) RevisionDigest() (string, error) {
	return domain.Digest(struct {
		ID           string   `json:"id"`
		Revision     int      `json:"revision"`
		Repositories []string `json:"repositories"`
	}{p.ID, p.Revision, sortedCopy(p.Repositories)})
}

// ProductProposal is a proposed revision as an operator states it. It becomes
// a Product only through Compose, and is adopted only after Validate and, for
// a mutation, ValidateProductMutation.
type ProductProposal struct {
	Name         string   `json:"name"`
	Revision     int      `json:"revision"`
	Repositories []string `json:"repositories"`
}

// Compose turns a proposal into a candidate revision under the requester and
// clock the runtime owns.
func (p ProductProposal) Compose(requestedBy string, at time.Time) (Product, error) {
	id, err := ProductID(p.Name)
	if err != nil {
		return Product{}, err
	}
	product := Product{
		SchemaVersion: ProductSchemaVersion, ID: id, Name: p.Name, Revision: p.Revision,
		Repositories: p.Repositories, RequestedBy: requestedBy, CreatedAt: at,
	}
	if err := product.Validate(); err != nil {
		return Product{}, err
	}
	return product, nil
}

// Validate is the complete deterministic admission check for one revision
// document, independent of any other revision.
func (p Product) Validate() error {
	if p.SchemaVersion != ProductSchemaVersion {
		return fmt.Errorf("product schema version %q is not %q", p.SchemaVersion, ProductSchemaVersion)
	}
	if err := boundedField("product name", p.Name, maxProductNameBytes); err != nil {
		return err
	}
	id, err := ProductID(p.Name)
	if err != nil {
		return err
	}
	if id != p.ID {
		return fmt.Errorf("product %s does not match the identity %s of its own name", p.ID, id)
	}
	if p.Revision < 1 {
		return fmt.Errorf("product revision %d is not a revision", p.Revision)
	}
	if p.CreatedAt.IsZero() {
		return errors.New("product creation time is required")
	}
	if len(p.Repositories) == 0 {
		return errors.New("a product names at least one repository")
	}
	if len(p.Repositories) > MaxProductRepositories {
		return fmt.Errorf("product names %d repositories, above the %d repository bound", len(p.Repositories), MaxProductRepositories)
	}
	seen := make(map[string]bool, len(p.Repositories))
	for _, repository := range p.Repositories {
		if err := boundedField("product repository", repository, maxRepositoryBytes); err != nil {
			return err
		}
		if seen[repository] {
			return fmt.Errorf("product repository %q is named more than once", repository)
		}
		seen[repository] = true
	}
	return nil
}

// ValidateProductMutation is the deterministic gate a proposed next revision
// passes before anything adopts it.
//
// A revision may only ADD repositories. Removing or rebinding one a graph may
// already be associated against would silently change which product's
// context that association resolves through - exactly the cross-product
// leakage #476 exists to rule out.
func ValidateProductMutation(current, next Product) error {
	if err := next.Validate(); err != nil {
		return err
	}
	if next.ID != current.ID || next.Name != current.Name {
		return fmt.Errorf("product revision %d describes a different product than %s", next.Revision, current.ID)
	}
	if next.Revision != current.Revision+1 {
		return fmt.Errorf("product %s is at revision %d; the next revision is %d, not %d",
			current.ID, current.Revision, current.Revision+1, next.Revision)
	}
	have := make(map[string]bool, len(next.Repositories))
	for _, repository := range next.Repositories {
		have[repository] = true
	}
	for _, repository := range current.Repositories {
		if !have[repository] {
			return fmt.Errorf("repository %q is already part of product %s and may not be removed by revision %d",
				repository, current.ID, next.Revision)
		}
	}
	return nil
}

func boundedField(name, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s is %d bytes, above the %d byte bound", name, len(value), limit)
	}
	return nil
}

func sortedCopy(values []string) []string {
	out := append([]string(nil), values...)
	sort.Strings(out)
	return out
}
