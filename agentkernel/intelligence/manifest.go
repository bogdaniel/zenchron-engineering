package intelligence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// FileEntry is one regular file in a manifest.
type FileEntry struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

// Manifest is the deterministic list of selected workspace files. Skipped
// lists selected non-regular entries (symlinks, devices); they are never
// followed, so no manifest can read outside its root.
type Manifest struct {
	Files   []FileEntry `json:"files"`
	Skipped []string    `json:"skipped,omitempty"`
}

// Digest is api.Digest over the canonical JSON encoding.
func (m Manifest) Digest() string {
	b, err := json.Marshal(m)
	if err != nil {
		panic("intelligence: manifest is plain data and always encodes: " + err.Error())
	}
	return api.Digest(b)
}

// Scope selects the files a snapshot covers.
//
// Include and Exclude are path.Match patterns tested against a file's
// workspace-relative path, each of its parent directories, and its base name.
// An empty Include selects everything; Exclude always wins. Packages bounds
// extraction to the listed package directories (non-recursive) plus every
// go.mod on their ancestor chain; empty means the whole workspace.
type Scope struct {
	Include  []string `json:"include,omitempty"`
	Exclude  []string `json:"exclude,omitempty"`
	Packages []string `json:"packages,omitempty"`
}

func (s Scope) canonical() (Scope, error) {
	for _, p := range slices.Concat(s.Include, s.Exclude) {
		if _, err := path.Match(p, ""); err != nil || p == "" {
			return Scope{}, fmt.Errorf("intelligence: invalid pattern %q", p)
		}
	}
	for _, p := range s.Packages {
		if !api.ValidRelativePath(p) {
			return Scope{}, fmt.Errorf("intelligence: package dir %q must be a clean workspace-relative path", p)
		}
	}
	return Scope{Include: sortedSet(s.Include), Exclude: sortedSet(s.Exclude), Packages: sortedSet(s.Packages)}, nil
}

func (s Scope) clone() Scope {
	return Scope{Include: slices.Clone(s.Include), Exclude: slices.Clone(s.Exclude), Packages: slices.Clone(s.Packages)}
}

func sortedSet(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

// selects is the single selection rule shared by the full walk, the bounded
// walk and overlays, so an overlay can never admit a file a fresh build of the
// same tree would not.
func (s Scope) selects(p string) bool {
	if p == ".git" || strings.HasPrefix(p, ".git/") {
		return false
	}
	if len(s.Include) > 0 && !matchAny(s.Include, p) {
		return false
	}
	if matchAny(s.Exclude, p) {
		return false
	}
	if len(s.Packages) == 0 {
		return true
	}
	dir := path.Dir(p)
	if _, found := slices.BinarySearch(s.Packages, dir); found {
		return true
	}
	if path.Base(p) != "go.mod" {
		return false
	}
	return slices.ContainsFunc(s.Packages, func(pkg string) bool { return isAncestorOrEqual(dir, pkg) })
}

func matchAny(patterns []string, p string) bool {
	base := path.Base(p)
	for _, pat := range patterns {
		if ok, _ := path.Match(pat, base); ok {
			return true
		}
		for q := p; q != "." && q != "/"; q = path.Dir(q) {
			if ok, _ := path.Match(pat, q); ok {
				return true
			}
		}
	}
	return false
}

func isAncestorOrEqual(dir, p string) bool {
	return dir == "." || dir == p || strings.HasPrefix(p, dir+"/")
}

// manifestBuild hashes the selected files under root. Go sources and go.mod
// contents are kept so extraction does not read them a second time.
type manifestBuild struct {
	root     string
	scope    Scope
	manifest Manifest
	contents map[string][]byte
	stats    *Stats
}

func buildManifest(ctx context.Context, root string, scope Scope, stats *Stats) (Manifest, map[string][]byte, error) {
	mb := &manifestBuild{root: root, scope: scope, contents: map[string][]byte{}, stats: stats}
	var err error
	if len(scope.Packages) == 0 {
		err = mb.walkAll(ctx)
	} else {
		err = mb.walkBounded(ctx)
	}
	if err != nil {
		return Manifest{}, nil, err
	}
	slices.SortFunc(mb.manifest.Files, func(a, b FileEntry) int { return strings.Compare(a.Path, b.Path) })
	slices.Sort(mb.manifest.Skipped)
	return mb.manifest, mb.contents, nil
}

func (mb *manifestBuild) walkAll(ctx context.Context) error {
	return filepath.WalkDir(mb.root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("intelligence: walk %s: %w", abs, err)
		}
		rel, err := mb.rel(abs)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && (d.Name() == ".git" || matchAny(mb.scope.Exclude, rel)) {
				return filepath.SkipDir
			}
			return nil
		}
		return mb.add(ctx, rel, d.Type())
	})
}

func (mb *manifestBuild) walkBounded(ctx context.Context) error {
	seen := map[string]bool{}
	for _, dir := range mb.scope.Packages {
		entries, err := os.ReadDir(filepath.Join(mb.root, filepath.FromSlash(dir)))
		if err != nil {
			return fmt.Errorf("intelligence: package dir %s: %w", dir, err)
		}
		for _, e := range entries {
			rel := path.Join(dir, e.Name())
			if e.IsDir() || seen[rel] {
				continue
			}
			seen[rel] = true
			if err := mb.add(ctx, rel, e.Type()); err != nil {
				return err
			}
		}
		for a := dir; ; a = path.Dir(a) {
			if err := mb.addGoMod(ctx, path.Join(a, "go.mod"), seen); err != nil {
				return err
			}
			if a == "." {
				break
			}
		}
	}
	return nil
}

func (mb *manifestBuild) addGoMod(ctx context.Context, rel string, seen map[string]bool) error {
	if seen[rel] {
		return nil
	}
	seen[rel] = true
	info, err := os.Lstat(filepath.Join(mb.root, filepath.FromSlash(rel)))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("intelligence: stat %s: %w", rel, err)
	}
	return mb.add(ctx, rel, info.Mode().Type())
}

func (mb *manifestBuild) add(ctx context.Context, rel string, mode fs.FileMode) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !mb.scope.selects(rel) {
		return nil
	}
	if mode&fs.ModeType != 0 {
		mb.manifest.Skipped = append(mb.manifest.Skipped, rel)
		return nil
	}
	b, err := os.ReadFile(filepath.Join(mb.root, filepath.FromSlash(rel)))
	if err != nil {
		return fmt.Errorf("intelligence: read %s: %w", rel, err)
	}
	mb.stats.FilesHashed++
	mb.stats.BytesHashed += int64(len(b))
	mb.manifest.Files = append(mb.manifest.Files, FileEntry{Path: rel, Digest: api.Digest(b), Size: int64(len(b))})
	if strings.HasSuffix(rel, ".go") || path.Base(rel) == "go.mod" {
		mb.contents[rel] = b
	}
	return nil
}

func (mb *manifestBuild) rel(abs string) (string, error) {
	r, err := filepath.Rel(mb.root, abs)
	if err != nil {
		return "", fmt.Errorf("intelligence: %s outside root: %w", abs, err)
	}
	return filepath.ToSlash(r), nil
}
