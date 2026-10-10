package runtime

// The durable half of #476, proved the same way #472's graph store is: a
// revision is immutable, "current" is the highest one adopted, and every
// claim holds across two independent handles on one database file, so no
// process-local state is what makes these pass.

import (
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/product"
)

func composeProduct(t *testing.T, name string, revision int, repositories []string, at time.Time) product.Product {
	t.Helper()
	p, err := product.ProductProposal{Name: name, Revision: revision, Repositories: repositories}.
		Compose("operator@example", at)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestProductRevisionsAreAppendOnlyAcrossRestart(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()

	one := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	adopted, created, err := first.AdoptProductRevision(one)
	if err != nil || !created {
		t.Fatalf("adopt revision 1: created=%t err=%v", created, err)
	}

	// A second handle on the SAME file is a restart: the revision survives.
	restarted, found, err := second.CurrentProduct(adopted.ID)
	if err != nil || !found || restarted.Revision != 1 {
		t.Fatalf("restart read: found=%t revision=%d err=%v", found, restarted.Revision, err)
	}

	// Resubmitting revision 1 unchanged, through the OTHER handle, is found,
	// not rewritten.
	again, created, err := second.AdoptProductRevision(one)
	if err != nil || created || again.ID != adopted.ID {
		t.Fatalf("resubmission: created=%t err=%v", created, err)
	}

	// A DIFFERENT document under revision 1 is a conflict, never an overwrite.
	altered := one
	altered.Repositories = []string{"acme/storefront-web", "acme/storefront-mobile"}
	if _, _, err := second.AdoptProductRevision(altered); err == nil || !strings.Contains(err.Error(), "a revision is immutable") {
		t.Fatalf("err = %v, want a refusal to rewrite an adopted revision", err)
	}

	// Revision 2 (the real successor) becomes current, and revision 1 is kept.
	two := composeProduct(t, "acme-storefront", 2,
		[]string{"acme/storefront-web", "acme/storefront-mobile"}, now.Add(time.Hour))
	if _, created, err := first.AdoptProductRevision(two); err != nil || !created {
		t.Fatalf("adopt revision 2: created=%t err=%v", created, err)
	}
	current, found, err := second.CurrentProduct(adopted.ID)
	if err != nil || !found || current.Revision != 2 {
		t.Fatalf("current revision = %d (found=%t err=%v)", current.Revision, found, err)
	}
	historical, found, err := first.ProductRevision(adopted.ID, 1)
	if err != nil || !found || len(historical.Repositories) != 1 {
		t.Fatalf("revision 1 was rewritten: found=%t repositories=%v err=%v", found, historical.Repositories, err)
	}
}

func baseConfigurationRevision(t *testing.T, productID string, revision int, at time.Time) product.ProductConfiguration {
	t.Helper()
	c := product.ProductConfiguration{
		SchemaVersion: product.ConfigurationSchemaVersion, ProductID: productID, Revision: revision,
		Settings: map[string]string{"tier": "standard"}, CreatedAt: at,
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestProductConfigurationHistoryIsPreservedAndStaleRevisionsConflict(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}

	one := baseConfigurationRevision(t, p.ID, 1, now)
	if _, created, err := first.AdoptProductConfigurationRevision(one); err != nil || !created {
		t.Fatalf("adopt configuration revision 1: created=%t err=%v", created, err)
	}
	two := baseConfigurationRevision(t, p.ID, 2, now.Add(time.Hour))
	two.Settings = map[string]string{"tier": "premium"}
	if _, created, err := second.AdoptProductConfigurationRevision(two); err != nil || !created {
		t.Fatalf("adopt configuration revision 2: created=%t err=%v", created, err)
	}

	// Historical revision 1 is unchanged: changing current configuration does
	// not rewrite a run's frozen history.
	historical, found, err := first.ProductConfigurationRevision(p.ID, 1)
	if err != nil || !found || historical.Settings["tier"] != "standard" {
		t.Fatalf("configuration revision 1 was rewritten: found=%t settings=%v err=%v", found, historical.Settings, err)
	}
	current, found, err := second.CurrentConfiguration(p.ID)
	if err != nil || !found || current.Settings["tier"] != "premium" {
		t.Fatalf("current configuration = %v (found=%t err=%v)", current.Settings, found, err)
	}

	// Resubmitting the CURRENT revision number with different content is a
	// stale-revision conflict, not an overwrite.
	stale := baseConfigurationRevision(t, p.ID, 2, now.Add(time.Hour))
	stale.Settings = map[string]string{"tier": "basic"}
	if _, _, err := first.AdoptProductConfigurationRevision(stale); err == nil || !strings.Contains(err.Error(), "already adopted with different contents") {
		t.Fatalf("err = %v, want a stale revision conflict", err)
	}
}

func TestAssociateWorkGraphOneProductPerGraph(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	productA := composeProduct(t, "storefront", 1, []string{"acme/shared-lib"}, now)
	productB := composeProduct(t, "checkout", 1, []string{"acme/shared-lib"}, now)
	if _, _, err := first.AdoptProductRevision(productA); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AdoptProductRevision(productB); err != nil {
		t.Fatal(err)
	}

	association, err := product.NewGraphAssociation(productA.ID, "graph-launch", now)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := first.AssociateWorkGraph(association); err != nil || !created {
		t.Fatalf("associate: created=%t err=%v", created, err)
	}
	// The SAME association, replayed through another handle, is a no-op.
	if created, err := second.AssociateWorkGraph(association); err != nil || created {
		t.Fatalf("idempotent re-association: created=%t err=%v", created, err)
	}
	// A DIFFERENT product claiming the same graph is refused, never a silent
	// takeover of which product's context the graph's units now resolve.
	conflicting, err := product.NewGraphAssociation(productB.ID, "graph-launch", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.AssociateWorkGraph(conflicting); err == nil {
		t.Fatal("associating an already-claimed graph with a different product should be refused")
	}
	owner, found, err := first.AssociatedProduct("graph-launch")
	if err != nil || !found || owner != productA.ID {
		t.Fatalf("associated product = %q (found=%t err=%v), want %q", owner, found, err, productA.ID)
	}
}

func TestCompileContextThroughTheRealStoreFailsClosedOnAMissingProduct(t *testing.T) {
	_, first, _ := openPair(t)
	if _, err := product.CompileContext(first, product.ContextRequest{
		ProductID: "product-does-not-exist", Scope: product.ScopeProduct,
	}); err == nil {
		t.Fatal("compiling context for a nonexistent product should fail closed")
	}
}

func TestStoredProductIsRefusedWhenCorrupt(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	// A row written by something other than AdoptProductRevision - or corrupted
	// at rest - must be refused on read, never silently accepted as a product
	// with whatever fields happened to decode.
	if _, err := first.db.Exec(`UPDATE product_revisions SET document = ? WHERE product_id = ? AND revision = 1`,
		`{"schema_version":"0.1","id":"product-wrong-id","name":"acme-storefront"}`, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.CurrentProduct(p.ID); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want a refusal of corrupt stored data", err)
	}
}

func TestRecordPromotionRefusesAMismatchedEntryAndRecord(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	p := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(p); err != nil {
		t.Fatal(err)
	}
	discovered, err := product.NewKnowledgeEntry(p.ID, product.ScopeWorkUnit, "claim",
		product.Provenance{Type: product.ProvenanceAgentDiscovery, Producer: "run-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	promoted, record, err := product.Promote(discovered, product.ScopeProduct, "lead@example", now)
	if err != nil {
		t.Fatal(err)
	}
	record.PromotedEntryID = "knowledge-not-the-one-above"
	if _, err := first.RecordPromotion(promoted, record); err == nil {
		t.Fatal("a promotion record naming a different entry than the one given should be refused")
	}
}
