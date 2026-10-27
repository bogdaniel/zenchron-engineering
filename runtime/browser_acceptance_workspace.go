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

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
func isBrowserSurface(path string) bool {
	if strings.HasPrefix(path, browserScriptsDir+"/") {
		return true
	}
	dir, file := filepath.Split(path)
	return dir == browserTestDir+"/" && strings.HasSuffix(file, "_test.go")
}

// browserWorkspace materializes the candidate's exact tree into dest with the
// test surface replaced by the base revision's, and returns the digest of that
// surface (every path with its base blob id), so a record names exactly which
// test sources ran.
func browserWorkspace(checkout, base, tree, dest string) (string, error) {
	if err := os.MkdirAll(dest, 0700); err != nil {
		return "", err
	}
	archive := exec.Command("git", "archive", "--format=tar", tree)
	archive.Dir = checkout
	extract := exec.Command("tar", "-x", "-C", dest)
	pipe, err := archive.StdoutPipe()
	if err != nil {
		return "", err
	}
	extract.Stdin = pipe
	if err := extract.Start(); err != nil {
		return "", err
	}
	if err := archive.Run(); err != nil {
		_ = extract.Wait()
		return "", fmt.Errorf("export the candidate tree: %w", err)
	}
	if err := extract.Wait(); err != nil {
		return "", fmt.Errorf("unpack the candidate tree: %w", err)
	}
	// The sandbox masks /candidate/.git with a tmpfs, and Docker cannot create
	// that mountpoint inside a read-only bind, so the directory must exist. It
	// is empty: no repository metadata reaches the container.
	if err := os.MkdirAll(filepath.Join(dest, ".git"), 0700); err != nil {
		return "", err
	}
	// The candidate's version of the surface is discarded entirely, including
	// any test file it added.
	if err := os.RemoveAll(filepath.Join(dest, browserScriptsDir)); err != nil {
		return "", err
	}
	candidateTests, err := filepath.Glob(filepath.Join(dest, browserTestDir, "*_test.go"))
	if err != nil {
		return "", err
	}
	for _, path := range candidateTests {
		if err := os.Remove(path); err != nil {
			return "", err
		}
	}
	listing, err := gitOutput(checkout, "ls-tree", "-r", base, "--", browserTestDir, browserScriptsDir)
	if err != nil {
		return "", fmt.Errorf("read the base test surface: %w", err)
	}
	digest := sha256.New()
	var lines []string
	for _, line := range strings.Split(strings.TrimSpace(listing), "\n") {
		meta, path, found := strings.Cut(line, "\t")
		fields := strings.Fields(meta)
		if !found || len(fields) != 3 || fields[1] != "blob" || !isBrowserSurface(path) {
			continue
		}
		if fields[0] != "100644" && fields[0] != "100755" {
			return "", fmt.Errorf("base test surface entry %s is not a regular file", path)
		}
		blob, err := gitOutput(checkout, "cat-file", "blob", fields[2])
		if err != nil {
			return "", err
		}
		target := filepath.Join(dest, path)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return "", err
		}
		if err := os.WriteFile(target, []byte(blob), os.FileMode(0600)); err != nil {
			return "", err
		}
		lines = append(lines, path+" "+fields[2])
	}
	if len(lines) == 0 {
		return "", fmt.Errorf("the base revision has no browser test surface")
	}
	sort.Strings(lines)
	digest.Write([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// browserArtifacts reads what each selected test wrote under its own directory
// and returns one digest over every file (name, size-independent content
// hash). A selected test that wrote nothing is reported by name, because a
// browser test that leaves no artifact did not demonstrably drive a browser.
func browserArtifacts(root string, selection []string) (digest string, silent []string, err error) {
	var lines []string
	for _, name := range selection {
		files := 0
		walkErr := filepath.WalkDir(filepath.Join(root, name), func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("artifact %s is not a regular file", path)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, path)
			sum := sha256.Sum256(data)
			lines = append(lines, relative+" "+hex.EncodeToString(sum[:]))
			files++
			return nil
		})
		if walkErr != nil && !os.IsNotExist(walkErr) {
			return "", nil, walkErr
		}
		if files == 0 {
			silent = append(silent, name)
		}
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:]), silent, nil
}
