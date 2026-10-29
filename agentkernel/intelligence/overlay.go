package intelligence

import (
	"context"
	"fmt"
	"maps"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// Change is one workspace delta relative to the base snapshot. Content is the
// complete new file; Delete removes Path; RenamedFrom removes the old path and
// writes Content at Path.
type Change struct {
	Path        string
	Content     []byte
	Delete      bool
	RenamedFrom string
}

// OverlaySpec derives an execution-scoped index. Root holds the base content
// of unchanged files; it is read only when a re-extracted package needs an
// unchanged file, and every such read must match the base manifest digest.
type OverlaySpec struct {
	Root    string
	Changes []Change
	// Settings replaces the base settings when non-nil.
	Settings *Settings
}

// Overlay derives a new index with the changes applied. The receiver is never
// modified, and the result is private to the caller: two overlays of one base
// share only the base's read-only facts.
//
// Invalidation: changed directories and every package that (transitively)
// imports them are re-extracted; a go.mod change re-extracts every directory
// whose governing module changed; a settings change re-extracts everything.
func (ix *Index) Overlay(ctx context.Context, spec OverlaySpec) (*Index, error) {
	root, err := canonicalRoot(spec.Root)
	if err != nil {
		return nil, err
	}
	settings := ix.data.Identity.Settings
	if spec.Settings != nil {
		if settings, err = spec.Settings.canonical(); err != nil {
			return nil, err
		}
	}
	start := time.Now()
	binding := OverlayBinding{BaseKey: ix.Key(), Settings: settings, Invalidation: "files"}
	m, contents, changed, err := ix.applyChanges(spec.Changes, &binding)
	if err != nil {
		return nil, err
	}
	var stats Stats
	s := newSession(ctx, root, settings, ix.data.Identity.Scope, m, contents, &stats)
	if err := s.prepare(); err != nil {
		return nil, err
	}
	if !settings.equal(ix.data.Identity.Settings) {
		binding.Invalidation = "all"
	} else {
		ix.planReuse(s, changed, &binding)
	}
	out, err := s.build()
	if err != nil {
		return nil, err
	}
	out.stats.ExtractTime = time.Since(start)
	out.data.Overlay = &binding
	return out, nil
}

// applyChanges validates the changes and returns the overlay manifest, the
// changed contents and every path touched (both sides of a rename).
func (ix *Index) applyChanges(changes []Change, b *OverlayBinding) (Manifest, map[string][]byte, []string, error) {
	scope := ix.data.Identity.Scope
	entries := map[string]FileEntry{}
	for _, f := range ix.data.Manifest.Files {
		entries[f.Path] = f
	}
	contents, touched := map[string][]byte{}, map[string]bool{}
	for _, c := range changes {
		if err := validateChange(c, touched); err != nil {
			return Manifest{}, nil, nil, err
		}
		for _, p := range []string{c.Path, c.RenamedFrom} {
			if p != "" {
				touched[p] = true
			}
		}
		if !scope.selects(c.Path) && (c.RenamedFrom == "" || !scope.selects(c.RenamedFrom)) {
			b.Ignored = append(b.Ignored, c.Path)
			continue
		}
		if c.RenamedFrom != "" && !scope.selects(c.RenamedFrom) {
			c.RenamedFrom = "" // the source was never in this snapshot: a plain addition
		}
		if err := requireBase(c, entries); err != nil {
			return Manifest{}, nil, nil, err
		}
		if c.RenamedFrom != "" {
			delete(entries, c.RenamedFrom)
			b.Renamed = setKey(b.Renamed, c.Path, c.RenamedFrom)
		}
		if c.Delete {
			delete(entries, c.Path)
			b.Deleted = append(b.Deleted, c.Path)
			continue
		}
		if !scope.selects(c.Path) {
			continue // renamed out of scope: only the deletion applies
		}
		digest := api.Digest(c.Content)
		entries[c.Path] = FileEntry{Path: c.Path, Digest: digest, Size: int64(len(c.Content))}
		contents[c.Path] = slices.Clone(c.Content)
		b.Dirty = setKey(b.Dirty, c.Path, digest)
	}
	m := Manifest{Files: slices.SortedFunc(maps.Values(entries), func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })}
	for _, p := range ix.data.Manifest.Skipped {
		if _, replaced := entries[p]; !replaced && !touched[p] {
			m.Skipped = append(m.Skipped, p)
		}
	}
	slices.Sort(b.Deleted)
	slices.Sort(b.Ignored)
	return m, contents, slices.Sorted(maps.Keys(touched)), nil
}

func validateChange(c Change, touched map[string]bool) error {
	if !api.ValidRelativePath(c.Path) || c.Path == "." {
		return fmt.Errorf("intelligence: change path %q must be a clean workspace-relative file path", c.Path)
	}
	if touched[c.Path] || (c.RenamedFrom != "" && touched[c.RenamedFrom]) {
		return fmt.Errorf("intelligence: path %q changed more than once", c.Path)
	}
	if c.Delete && (c.Content != nil || c.RenamedFrom != "") {
		return fmt.Errorf("intelligence: delete of %q carries content or a rename", c.Path)
	}
	if c.RenamedFrom != "" && (!api.ValidRelativePath(c.RenamedFrom) || c.RenamedFrom == c.Path || c.RenamedFrom == ".") {
		return fmt.Errorf("intelligence: invalid rename source %q", c.RenamedFrom)
	}
	return nil
}

// requireBase refuses a delete or rename of a file the base never had: such a
// change describes some other workspace, not a delta from this snapshot.
func requireBase(c Change, entries map[string]FileEntry) error {
	if _, ok := entries[c.Path]; c.Delete && !ok {
		return fmt.Errorf("intelligence: delete of %q, which is not in the base manifest", c.Path)
	}
	if _, ok := entries[c.RenamedFrom]; c.RenamedFrom != "" && !ok {
		return fmt.Errorf("intelligence: rename source %q is not in the base manifest", c.RenamedFrom)
	}
	return nil
}

func setKey(m map[string]string, k, v string) map[string]string {
	if m == nil {
		m = map[string]string{}
	}
	m[k] = v
	return m
}

// planReuse marks which base directories the overlay may reuse unchanged: a
// directory is reusable only when none of its files changed, its governing
// module is identical, and nothing it imports (transitively) was re-extracted.
func (ix *Index) planReuse(s *session, touched []string, b *OverlayBinding) {
	affected := map[string]bool{}
	for _, p := range touched {
		if strings.HasSuffix(p, ".go") {
			affected[path.Dir(p)] = true
		}
	}
	for _, df := range ix.data.Dirs {
		if moduleKey(ix.data.Modules, df.Dir) != moduleKey(s.modules, df.Dir) {
			affected[df.Dir] = true
			b.Invalidation = "module"
		}
	}
	ix.closeOverImporters(s, affected)
	for _, df := range ix.data.Dirs {
		if _, live := s.goDirs[df.Dir]; live && !affected[df.Dir] {
			s.reuse[df.Dir] = df
		}
	}
	s.baseTypes = ix.types
}

// closeOverImporters adds every base directory that imports an affected
// package, under either its old or its new import path, until a fixpoint.
func (ix *Index) closeOverImporters(s *session, affected map[string]bool) {
	paths := map[string]bool{}
	for {
		for dir := range affected {
			paths[s.pathOf[dir]] = true
			if old, ok := importPathFor(ix.data.Modules, dir); ok {
				paths[old] = true
			}
		}
		grew := false
		for _, df := range ix.data.Dirs {
			if affected[df.Dir] {
				continue
			}
			if slices.ContainsFunc(df.Imports, func(im Import) bool { return paths[im.Path] }) {
				affected[df.Dir], grew = true, true
			}
		}
		if !grew {
			return
		}
	}
}
