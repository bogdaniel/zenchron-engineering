package runtime

// The durable half of #476, proved the same way #472's graph store is: a
// revision is immutable, "current" is the highest one adopted, the FULL
// revision-transition invariant is enforced at the write boundary itself (not
// merely available for a caller to call), and every claim holds across two
// independent handles on one database file, so no process-local state is what
// makes these pass.

import (
	"strings"
	"sync"
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

// TestAdoptProductRevisionEnforcesTheFullTransitionInvariant is B1: the write
// boundary itself refuses a non-initial first revision, a gap, and a revision
// that would remove a repository an earlier one already named - not merely a
// caller that remembered to call product.ValidateProductMutation first.
func TestAdoptProductRevisionEnforcesTheFullTransitionInvariant(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()

	notOne := composeProduct(t, "acme-storefront", 2, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(notOne); err == nil {
		t.Fatal("a product's first-ever revision must be 1")
	}

	one := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, created, err := first.AdoptProductRevision(one); err != nil || !created {
		t.Fatalf("adopt revision 1: created=%t err=%v", created, err)
	}

	gap := composeProduct(t, "acme-storefront", 3, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(gap); err == nil {
		t.Fatal("skipping from revision 1 straight to revision 3 should be refused")
	}

	removing := composeProduct(t, "acme-storefront", 2, []string{"acme/storefront-mobile"}, now)
	if _, _, err := first.AdoptProductRevision(removing); err == nil {
		t.Fatal("revision 2 removing revision 1's repository should be refused, even with no caller-side mutation check")
	}
}

// TestAdoptProductRevisionSerializesConcurrentCompetingWriters is B1's
// concurrency claim: two writers racing to adopt "the next revision" can
// never both succeed with disagreeing content, across two independent store
// handles, with no in-process lock making it work.
func TestAdoptProductRevisionSerializesConcurrentCompetingWriters(t *testing.T) {
	_, first, second := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	base := composeProduct(t, "acme-storefront", 1, []string{"acme/storefront-web"}, now)
	if _, _, err := first.AdoptProductRevision(base); err != nil {
		t.Fatal(err)
	}

	byFirst := composeProduct(t, "acme-storefront", 2, []string{"acme/storefront-web", "acme/storefront-mobile"}, now)
	bySecond := composeProduct(t, "acme-storefront", 2, []string{"acme/storefront-web", "acme/storefront-api"}, now)

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); _, _, results[0] = first.AdoptProductRevision(byFirst) }()
	go func() { defer wg.Done(); _, _, results[1] = second.AdoptProductRevision(bySecond) }()
	wg.Wait()

	succeeded := 0
	for _, err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("exactly one of two competing revision-2 proposals should succeed, got %d", succeeded)
	}
	current, found, err := first.CurrentProduct(base.ID)
	if err != nil || !found || current.Revision != 2 {
		t.Fatalf("current revision = %d (found=%t err=%v)", current.Revision, found, err)
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
	historical, found, err := first.ConfigurationRevision(p.ID, 1)
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

	// A gap (revision 4 when current is 2) is refused, not adopted as "next".
	gap := baseConfigurationRevision(t, p.ID, 4, now.Add(2*time.Hour))
	if _, _, err := first.AdoptProductConfigurationRevision(gap); err == nil {
		t.Fatal("a configuration revision gap should be refused")
	}
}

// TestAdoptProductConfigurationRevisionRequiresAnExistingProduct is part of
// B5: the configuration adopter establishes product existence itself, rather
// than trusting a caller to have checked.
func TestAdoptProductConfigurationRevisionRequiresAnExistingProduct(t *testing.T) {
	_, first, _ := openPair(t)
	now := time.Unix(1700000000, 0).UTC()
	orphan := baseConfigurationRevision(t, "product-does-not-exist", 1, now)
	if _, _, err := first.AdoptProductConfigurationRevision(orphan); err == nil {
		t.Fatal("adopting configuration for a product that was never adopted should be refused")
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
