//go:build unix

package tools

import (
	"os"
	"path/filepath"
	"testing"
)

// A hard link aliases .git/config with no symlink anywhere in its path; a
// regular file with more than one name is neither read, written nor searched.
func TestHardLinkAliasToGitMetadataIsRefused(t *testing.T) {
	f := aliasFixture(t)
	if err := os.Link(filepath.Join(f.root, ".git/config"), filepath.Join(f.root, "src/h")); err != nil {
		t.Fatal(err)
	}
	assertAliasesRefused(t, f, true, []string{"src/h"})
}
