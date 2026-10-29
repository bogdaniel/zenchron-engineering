package runtime

// The browser run executes tests, and the tests are part of the candidate. A
// candidate that could change them could weaken or empty them and still be
// "verified", so the test SURFACE is not the candidate's: it is the base
// revision's. The workspace the browser container sees is the candidate's exact
// tree with the test surface replaced by the base revision's, as the file-size
// rules come from the base and never from the candidate.
//
// The surface is the browser-driving Go tests and every script they run:
// cmd/zenchron-engineering/*_test.go and scripts/. The application under test
// (the Go packages, the embedded frontend bundle) stays the candidate's, so the
// candidate is exactly what is exercised. A base test that no longer compiles
// against the candidate is a judged failure: update the tests in a separate
// reviewed change first, then the change under test.
//
// A candidate's own new browser tests therefore never count toward the claim.
// They count once they are on a base revision AND listed in the base's
// browser-acceptance-tests.txt, which is how a feature-specific acceptance
// check becomes required (docs/browser-acceptance.md).
//
// The workspace is built on the controller host from candidate-controlled
// content, so it is built the way the adopted builder builds a tree
// (materializeTree): from Git OBJECTS, never from a checkout or an archive.
// Nothing consults .gitattributes (no export-ignore, no export-subst), no
// filter runs, and each blob is verified against the object id the tree named.
// The final file set is composed FIRST - the candidate's entries minus the
// surface and minus every entry that is an ancestor of a surface path, plus the
// base's surface entries - and written ONCE into an empty directory, with every
// parent directory checked to be a real directory before anything is created
// under it. A candidate symlink or file standing where a surface directory
// belongs is therefore dropped, and nothing written can pass through a link.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

const (
	browserTestDir    = "cmd/zenchron-engineering"
	browserScriptsDir = "scripts"
)

// isBrowserSurface reports whether a repository path belongs to the test
// surface taken from the base revision.
func isBrowserSurface(p string) bool {
	if strings.HasPrefix(p, browserScriptsDir+"/") {
		return true
	}
	dir, file := path.Split(p)
	return dir == browserTestDir+"/" && strings.HasSuffix(file, "_test.go")
}

type browserTreeEntry struct{ mode, object, path string }

// browserTreeEntries lists every blob-like entry of a revision, exactly as Git
// names them, refusing paths the adopted builder refuses and any submodule.
func browserTreeEntries(checkout, revision string) ([]browserTreeEntry, error) {
	listing, err := gitOutput(checkout, "ls-tree", "-r", "-z", "--full-tree", revision)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", revision, err)
	}
	var entries []browserTreeEntry
	for _, raw := range strings.Split(strings.TrimRight(listing, "\x00"), "\x00") {
		if raw == "" {
			continue
		}
		meta, p, found := strings.Cut(raw, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 {
			return nil, errors.New("the tree listing is malformed")
		}
		if err := safeTreePath(p); err != nil {
			return nil, err
		}
		switch fields[0] {
		case "100644", "100755", "120000":
			entries = append(entries, browserTreeEntry{mode: fields[0], object: fields[2], path: p})
		default:
			return nil, fmt.Errorf("%s contains %q with mode %s, which a browser workspace does not carry", revision, p, fields[0])
		}
	}
	return entries, nil
}

// browserWorkspace materializes the candidate's exact tree into dest with the
// test surface replaced by the base revision's, and returns the digest of that
// surface (every path with its base blob id), so a record names exactly which
// test sources ran.
func browserWorkspace(checkout, base, tree, dest string) (string, error) {
	if format, err := gitOutput(checkout, "rev-parse", "--show-object-format"); err != nil || strings.TrimSpace(format) != "sha1" {
		return "", errors.New("the repository object format could not be established as sha1")
	}
	candidate, err := browserTreeEntries(checkout, tree)
	if err != nil {
		return "", err
	}
	baseEntries, err := browserTreeEntries(checkout, base)
	if err != nil {
		return "", err
	}
	var surface []browserTreeEntry
	ancestors := map[string]bool{}
	for _, entry := range baseEntries {
		if !isBrowserSurface(entry.path) {
			continue
		}
		if entry.mode == "120000" {
			return "", fmt.Errorf("base test surface entry %s is a symbolic link", entry.path)
		}
		surface = append(surface, entry)
		for dir := path.Dir(entry.path); dir != "."; dir = path.Dir(dir) {
			ancestors[dir] = true
		}
	}
	if len(surface) == 0 {
		return "", errors.New("the base revision has no browser test surface")
	}
	composed := make([]browserTreeEntry, 0, len(candidate)+len(surface))
	for _, entry := range candidate {
		if !isBrowserSurface(entry.path) && !ancestors[entry.path] {
			composed = append(composed, entry)
		}
	}
	composed = append(composed, surface...)
	if err := materializeEntries(checkout, composed, dest); err != nil {
		return "", err
	}
	lines := make([]string, 0, len(surface))
	for _, entry := range surface {
		lines = append(lines, entry.path+" "+entry.object)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

// materializeEntries writes the composed entries into dest, which must not
// exist or be empty. Every blob is read from the Git object store and verified
// against its object id. No path component under dest is ever followed through
// a symbolic link, and no existing file is ever replaced.
func materializeEntries(checkout string, entries []browserTreeEntry, dest string) error {
	if err := os.MkdirAll(dest, 0700); err != nil {
		return err
	}
	if existing, err := os.ReadDir(dest); err != nil || len(existing) != 0 {
		return errors.New("the browser workspace is not empty")
	}
	for _, entry := range entries {
		content, err := gitOutput(checkout, "cat-file", "blob", entry.object)
		if err != nil {
			return fmt.Errorf("object %s could not be read: %w", entry.object, err)
		}
		if got := blobID(content); got != entry.object {
			return fmt.Errorf("object %s materialized as %s; the checkout is not the tree it claims", entry.object, got)
		}
		parent, err := realDirectory(dest, path.Dir(entry.path))
		if err != nil {
			return fmt.Errorf("%s: %w", entry.path, err)
		}
		target := filepath.Join(parent, path.Base(entry.path))
		if entry.mode == "120000" {
			if strings.ContainsRune(content, 0) {
				return fmt.Errorf("symbolic link %s has an invalid target", entry.path)
			}
			if err := os.Symlink(content, target); err != nil {
				return err
			}
			continue
		}
		perm := os.FileMode(0600)
		if entry.mode == "100755" {
			perm = 0700
		}
		file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(content); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	// The sandbox masks /candidate/.git with a tmpfs, and Docker cannot create
	// that mountpoint inside a read-only bind, so the directory must exist. It
	// is empty: no repository metadata reaches the container.
	return os.MkdirAll(filepath.Join(dest, ".git"), 0700)
}

// realDirectory returns the path of the directory dir (slash-separated, relative
// to root), creating missing components, and fails if any existing component is
// not a real directory. Resolving component by component is what stops a
// symbolic link or file from redirecting a write out of root.
func realDirectory(root, dir string) (string, error) {
	current := root
	if dir == "." {
		return current, nil
	}
	for _, element := range strings.Split(dir, "/") {
		current = filepath.Join(current, element)
		info, err := os.Lstat(current)
		switch {
		case os.IsNotExist(err):
			if err := os.Mkdir(current, 0700); err != nil {
				return "", err
			}
		case err != nil:
			return "", err
		case !info.IsDir():
			return "", fmt.Errorf("%s is not a directory", element)
		}
	}
	return current, nil
}
