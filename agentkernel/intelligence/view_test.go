package intelligence_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
)

func workspaceRef(ix *intelligence.Index) api.WorkspaceRef {
	return api.WorkspaceRef{ID: "ws-1", ManifestDigest: ix.Identity().ManifestDigest}
}

func TestViewItemsAreWorkspaceTrustedAndBoundToSnapshot(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	v, err := intelligence.NewView(ix, workspaceRef(ix))
	if err != nil {
		t.Fatal(err)
	}
	q := api.ContextQuery{ExecutionID: "ex-1", Objective: "fix the Total area computation", Workspace: workspaceRef(ix)}
	items, err := v.ContextItems(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) == 0 {
		t.Fatal("no items for a matching objective")
	}
	kinds := map[api.ContextKind]bool{}
	for _, it := range items {
		if err := it.Validate(); err != nil {
			t.Fatalf("item %s invalid: %v", it.ID, err)
		}
		if it.Trust != api.TrustWorkspace || it.Required || it.Revision != ix.Key() {
			t.Fatalf("item %s: trust %s required %t revision %s", it.ID, it.Trust, it.Required, it.Revision)
		}
		kinds[it.Kind] = true
	}
	for _, k := range []api.ContextKind{api.ContextSourceCode, api.ContextDependency, api.ContextTest} {
		if !kinds[k] {
			t.Errorf("no %s item", k)
		}
	}
	if !strings.Contains(items[0].Content, "Total") {
		t.Errorf("top item does not concern Total:\n%s", items[0].Content)
	}
	again, err := v.ContextItems(context.Background(), q)
	if err != nil || !reflect.DeepEqual(items, again) {
		t.Fatal("context items are not deterministic")
	}
	q.Limit = 2
	if limited, _ := v.ContextItems(context.Background(), q); len(limited) != 2 {
		t.Fatalf("limit 2 returned %d items", len(limited))
	}
}

func TestViewDependencyItemShowsIncompleteness(t *testing.T) {
	ix := build(t, workspace(t, "basic"), linux, intelligence.Scope{})
	v, err := intelligence.NewView(ix, workspaceRef(ix))
	if err != nil {
		t.Fatal(err)
	}
	items, err := v.ContextItems(context.Background(), api.ContextQuery{Objective: "Describe", Workspace: workspaceRef(ix)})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if it.Kind == api.ContextDependency && strings.Contains(it.Content, "shapes.Describe (func)") {
			for _, want := range []string{"fmt.Sprintf [unresolved", "Shape.Area [possible", "complete=false"} {
				if !strings.Contains(it.Content, want) {
					t.Errorf("dependency item lacks %q:\n%s", want, it.Content)
				}
			}
			return
		}
	}
	t.Fatal("no dependency item for Describe")
}

func TestViewRefusesStaleSnapshot(t *testing.T) {
	root := workspace(t, "basic")
	base := build(t, root, linux, intelligence.Scope{})
	ov, err := base.Overlay(context.Background(), intelligence.OverlaySpec{Root: root,
		Changes: []intelligence.Change{{Path: "shapes/reflect.go", Delete: true}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := intelligence.NewView(base, workspaceRef(ov)); !errors.Is(err, intelligence.ErrSnapshotMismatch) {
		t.Fatalf("binding the base to the changed workspace: err = %v", err)
	}
	v, err := intelligence.NewView(base, workspaceRef(base))
	if err != nil {
		t.Fatal(err)
	}
	_, err = v.ContextItems(context.Background(), api.ContextQuery{Objective: "CallByName", Workspace: workspaceRef(ov)})
	if !errors.Is(err, intelligence.ErrSnapshotMismatch) {
		t.Fatalf("query for another snapshot: err = %v, want ErrSnapshotMismatch", err)
	}
	ovView, err := intelligence.NewView(ov, workspaceRef(ov))
	if err != nil {
		t.Fatal(err)
	}
	items, err := ovView.ContextItems(context.Background(), api.ContextQuery{Objective: "CallByName reflect", Workspace: workspaceRef(ov)})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range items {
		if strings.Contains(it.Content, "CallByName") || strings.Contains(it.Content, "shapes/reflect.go") {
			t.Fatalf("overlay view served facts of a deleted file:\n%s", it.Content)
		}
	}
}
