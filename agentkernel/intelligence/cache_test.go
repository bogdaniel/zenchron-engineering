package intelligence_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
)

func open(t *testing.T, rec *memRecords, root string) *intelligence.Index {
	t.Helper()
	cfg := intelligence.BuildConfig{Root: root, Settings: linux}
	var ix *intelligence.Index
	var err error
	if rec == nil {
		ix, err = intelligence.Open(context.Background(), nil, cfg)
	} else {
		ix, err = intelligence.Open(context.Background(), rec, cfg)
	}
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// TestCacheReopenAfterRestart models a restart: a new store instance over the
// same durable records yields the identical index without re-extraction.
func TestCacheReopenAfterRestart(t *testing.T) {
	root := workspace(t, "basic")
	rec := newRecords()
	first := open(t, rec, root)
	if first.Stats().CacheHit || first.Stats().CacheDegraded != "" {
		t.Fatalf("cold open stats %+v", first.Stats())
	}
	second := open(t, rec.reopen(), root)
	st := second.Stats()
	if !st.CacheHit || st.FilesParsed != 0 || st.PackagesChecked != 0 {
		t.Fatalf("warm open stats %+v: want a cache hit with no extraction", st)
	}
	if fullDigest(t, first) != fullDigest(t, second) || first.Key() != second.Key() {
		t.Fatal("reopened index differs from the saved one")
	}
}

func TestCacheFailuresDegradeToCorrectRebuild(t *testing.T) {
	root := workspace(t, "basic")
	want := fullDigest(t, build(t, root, linux, intelligence.Scope{}))
	key := build(t, root, linux, intelligence.Scope{}).Key()
	other := build(t, workspace(t, "pure"), linux, intelligence.Scope{})

	corrupt := newRecords()
	corrupt.data[intelligence.CachePartition+"/"+key] = []byte(`{"format":1,"checksum":"x","snapshot":{}}`)
	garbage := newRecords()
	garbage.data[intelligence.CachePartition+"/"+key] = []byte("not json")
	mismatched := newRecords()
	if err := intelligence.Save(context.Background(), mismatched, other); err != nil {
		t.Fatal(err)
	}
	mismatched.data[intelligence.CachePartition+"/"+key] = mismatched.data[intelligence.CachePartition+"/"+other.Key()]
	if _, err := intelligence.Load(context.Background(), mismatched, key); !errors.Is(err, intelligence.ErrCacheInvalid) {
		t.Fatalf("mismatched identity: err = %v, want ErrCacheInvalid", err)
	}
	failing := newRecords()
	failing.failPut = true

	for name, rec := range map[string]*memRecords{"corrupt": corrupt, "garbage": garbage, "mismatched": mismatched, "failing": failing, "disabled": nil} {
		ix := open(t, rec, root)
		if ix.Stats().CacheHit || ix.Stats().CacheDegraded == "" {
			t.Errorf("%s: stats %+v, want explicit degradation", name, ix.Stats())
		}
		if fullDigest(t, ix) != want {
			t.Errorf("%s: degraded open produced different facts", name)
		}
	}
}

func TestCacheMissesWhenWorkspaceChanges(t *testing.T) {
	root := workspace(t, "basic")
	rec := newRecords()
	open(t, rec, root)
	if err := writeFile(root, "shapes/reflect.go", []byte("package shapes\n\nfunc Replaced() {}\n")); err != nil {
		t.Fatal(err)
	}
	ix := open(t, rec, root)
	if ix.Stats().CacheHit {
		t.Fatal("cache served a snapshot for different workspace bytes")
	}
	syms := symbols(ix)
	if _, ok := syms["example.com/basic/shapes.CallByName"]; ok {
		t.Fatal("stale symbol served after the workspace changed")
	}
	if _, ok := syms["example.com/basic/shapes.Replaced"]; !ok {
		t.Fatal("new content not extracted")
	}
}

// TestOverlayOfLoadedIndex: a cache-loaded base has no in-memory types, so
// its overlays must re-check dependencies from verified bytes and still match
// a fresh build.
func TestOverlayOfLoadedIndex(t *testing.T) {
	root := workspace(t, "basic")
	rec := newRecords()
	open(t, rec, root)
	loaded := open(t, rec.reopen(), root)
	if !loaded.Stats().CacheHit {
		t.Fatal("precondition: loaded from cache")
	}
	change := intelligence.Change{Path: "report/extra.go", Content: []byte(
		"package report\n\nimport \"example.com/basic/shapes\"\n\nfunc Sum() float64 { return shapes.NewSquare(1).Area() }\n")}
	ov, err := loaded.Overlay(context.Background(), intelligence.OverlaySpec{Root: root, Changes: []intelligence.Change{change}})
	if err != nil {
		t.Fatal(err)
	}
	fresh := build(t, workspace(t, "basic", change), linux, intelligence.Scope{})
	if snapshotDigest(t, ov) != snapshotDigest(t, fresh) {
		t.Fatal("overlay of a loaded index differs from a fresh build")
	}
	if err := intelligence.Save(context.Background(), rec, ov); err == nil || !strings.Contains(err.Error(), "overlay") {
		t.Fatalf("saving an overlay: err = %v, want refusal", err)
	}
}
