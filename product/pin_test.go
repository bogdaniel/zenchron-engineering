package product

import (
	"strings"
	"testing"
	"time"
)

// pinFixture is one product with a configuration at revision 1 and one widely
// promoted claim, plus the pin of the context a unit compiles from them.
type pinFixture struct {
	store    *fakeStore
	product  Product
	request  ContextRequest
	pin      ContextPin
	promoted KnowledgeEntry
}

func newPinFixture(t *testing.T) pinFixture {
	t.Helper()
	at := time.Unix(1700000000, 0).UTC()
	store, promoted, _ := twoOverlappingProducts(t)
	id, err := ProductID("storefront")
	if err != nil {
		t.Fatal(err)
	}
	store.configurations = map[string]map[int]ProductConfiguration{
		id: {1: {SchemaVersion: ConfigurationSchemaVersion, ProductID: id, Revision: 1,
			Settings: map[string]string{"tier": "standard"}, CreatedAt: at}},
	}
	request := ContextRequest{ProductID: id, Scope: ScopeProduct, OwnerRef: WorkUnitOwnerRef("graph-1", "a")}
	compiled, err := CompileContext(store, request)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := NewContextPin(compiled)
	if err != nil {
		t.Fatal(err)
	}
	return pinFixture{store: store, product: store.products[id], request: request, pin: pin, promoted: promoted}
}

func TestPinnedConfigurationRevisionSurvivesANewerRevision(t *testing.T) {
	f := newPinFixture(t)
	f.store.configurations[f.product.ID][2] = ProductConfiguration{SchemaVersion: ConfigurationSchemaVersion,
		ProductID: f.product.ID, Revision: 2, Settings: map[string]string{"tier": "gold"}, CreatedAt: time.Unix(1700000100, 0).UTC()}

	current, err := CompileContext(f.store, f.request)
	if err != nil || current.Configuration.Revision != 2 {
		t.Fatalf("an unpinned request should see the current revision 2: %v %+v", err, current.Configuration)
	}
	resolved, err := f.pin.Resolve(f.store, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Configuration == nil || resolved.Configuration.Revision != 1 || resolved.Configuration.Settings["tier"] != "standard" {
		t.Fatalf("the pinned unit must keep revision 1, got %+v", resolved.Configuration)
	}
}

func TestAPinWithNoConfigurationStaysWithoutOne(t *testing.T) {
	f := newPinFixture(t)
	delete(f.store.configurations, f.product.ID)
	compiled, err := CompileContext(f.store, f.request)
	if err != nil {
		t.Fatal(err)
	}
	pin, err := NewContextPin(compiled)
	if err != nil || pin.ConfigurationRevision != 0 {
		t.Fatalf("pin of a product with no configuration: %v %+v", err, pin)
	}
	f.store.configurations[f.product.ID] = map[int]ProductConfiguration{1: {SchemaVersion: ConfigurationSchemaVersion,
		ProductID: f.product.ID, Revision: 1, Settings: map[string]string{"late": "arrival"}, CreatedAt: time.Unix(1700000100, 0).UTC()}}
	resolved, err := pin.Resolve(f.store, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Configuration != nil {
		t.Fatalf("a configuration adopted after admission must not reach the unit: %+v", resolved.Configuration)
	}
}

func TestKnowledgePromotedAfterThePinIsNotDelivered(t *testing.T) {
	f := newPinFixture(t)
	later, err := NewKnowledgeEntry(f.product.ID, ScopeWorkUnit, "graph-9/z", "a claim recorded later",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "graph-9/z"}, time.Unix(1700000200, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	promoted, _, err := Promote(later, ScopeProduct, "", "lead@example", time.Unix(1700000200, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.store.knowledge[f.product.ID] = append(f.store.knowledge[f.product.ID], promoted)

	resolved, err := f.pin.Resolve(f.store, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Knowledge) != 1 || resolved.Knowledge[0].ID != f.promoted.ID {
		t.Fatalf("only the pinned claim may be delivered, got %+v", resolved.Knowledge)
	}
}

func TestResolveRefusesWhatNoLongerReproducesThePin(t *testing.T) {
	cases := map[string]func(f pinFixture) (ContextPin, ContextRequest){
		"a pinned knowledge entry disappeared": func(f pinFixture) (ContextPin, ContextRequest) {
			f.store.knowledge[f.product.ID] = nil
			return f.pin, f.request
		},
		"the pinned configuration revision disappeared": func(f pinFixture) (ContextPin, ContextRequest) {
			delete(f.store.configurations[f.product.ID], 1)
			return f.pin, f.request
		},
		"the pinned configuration content changed": func(f pinFixture) (ContextPin, ContextRequest) {
			changed := f.store.configurations[f.product.ID][1]
			changed.Settings = map[string]string{"tier": "tampered"}
			f.store.configurations[f.product.ID][1] = changed
			return f.pin, f.request
		},
		"the pin names another product": func(f pinFixture) (ContextPin, ContextRequest) {
			other := f.request
			other.ProductID = "product-someone-else"
			return f.pin, other
		},
		"the pin's digest was edited": func(f pinFixture) (ContextPin, ContextRequest) {
			tampered := f.pin
			tampered.ContextDigest = strings.Repeat("0", len(f.pin.ContextDigest))
			return tampered, f.request
		},
		"the pin's knowledge set was edited": func(f pinFixture) (ContextPin, ContextRequest) {
			tampered := f.pin
			tampered.KnowledgeIDs = append(append([]string(nil), f.pin.KnowledgeIDs...), "zzz-injected")
			return tampered, f.request
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newPinFixture(t)
			pin, request := mutate(f)
			if _, err := pin.Resolve(f.store, request); err == nil {
				t.Fatal("a context that no longer reproduces its pin must be refused, not delivered emptier or different")
			}
		})
	}
}

func TestAnotherUnitsAudiencedPromotionIsNeverInThisUnitsPin(t *testing.T) {
	f := newPinFixture(t)
	raw, err := NewKnowledgeEntry(f.product.ID, ScopeExecution, "graph-1/b", "meant for unit b only",
		Provenance{Type: ProvenanceAgentDiscovery, Producer: "graph-1/b"}, time.Unix(1700000300, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	forB, _, err := Promote(raw, ScopeWorkUnit, "graph-1/b", "lead@example", time.Unix(1700000300, 0).UTC())
	if err != nil {
		t.Fatal(err)
	}
	f.store.knowledge[f.product.ID] = append(f.store.knowledge[f.product.ID], raw, forB)

	unitA, err := CompileContext(f.store, f.request)
	if err != nil {
		t.Fatal(err)
	}
	unitB := f.request
	unitB.OwnerRef = WorkUnitOwnerRef("graph-1", "b")
	compiledB, err := CompileContext(f.store, unitB)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range unitA.Knowledge {
		if entry.ID == forB.ID || entry.ID == raw.ID {
			t.Fatalf("unit a received unit b's private knowledge %s", entry.ID)
		}
	}
	found := false
	for _, entry := range compiledB.Knowledge {
		found = found || entry.ID == forB.ID
		if entry.ID == raw.ID {
			t.Fatal("a raw discovery must not reach even its own owner at product scope; it re-enters only by promotion")
		}
	}
	if !found {
		t.Fatal("unit b should receive the promotion audienced to it")
	}
}

func TestPinValidateRefusesMalformedPins(t *testing.T) {
	f := newPinFixture(t)
	for name, mutate := range map[string]func(*ContextPin){
		"no product":                 func(p *ContextPin) { p.ProductID = "" },
		"negative revision":          func(p *ContextPin) { p.ConfigurationRevision = -1 },
		"revision without a digest":  func(p *ContextPin) { p.ConfigurationDigest = "" },
		"unsorted knowledge ids":     func(p *ContextPin) { p.KnowledgeIDs = []string{"b", "a"} },
		"duplicate knowledge ids":    func(p *ContextPin) { p.KnowledgeIDs = []string{"a", "a"} },
		"too many knowledge ids":     func(p *ContextPin) { p.KnowledgeIDs = make([]string, maxPinnedKnowledgeEntries+1) },
		"digest not of the contents": func(p *ContextPin) { p.ContextDigest = "x" },
	} {
		t.Run(name, func(t *testing.T) {
			pin := f.pin
			mutate(&pin)
			if err := pin.Validate(); err == nil {
				t.Fatal("a malformed pin must not validate")
			}
		})
	}
	if err := f.pin.Validate(); err != nil {
		t.Fatalf("the untouched pin must validate: %v", err)
	}
}
