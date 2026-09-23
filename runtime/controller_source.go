package runtime

// THE CONTROLLER'S OWN SOURCE, as a durable local transport.
//
// An adopted build needs a Git clone to fetch the trusted revision through and
// to materialize the tree from. A person running `controller build-adopted`
// supplies their working copy; a daemon has no working copy and must not
// depend on the directory it happened to be started in - a controller started
// by launchd from / would silently stop being able to upgrade itself.
//
// NOTHING ABOUT THE CLONE IS TRUSTED. It is a transport: the revision comes
// from the forge, the tree is recomputed, containment is proven with git
// against the fetched objects, and the build runs from a materialized tree
// rather than from this directory. A stale or tampered clone changes what can
// be fetched, never what is believed.

import (
	"fmt"
	"os"
	"path/filepath"
)

// ControllerSourceDir is where the controller keeps that clone.
func ControllerSourceDir(stateDir string) string {
	return filepath.Join(stateDir, "controller-source")
}

// EnsureControllerSource returns a local clone of the controller's repository,
// creating it once if it is not there.
//
// It is --no-checkout: nothing reads a working tree here, and a checkout would
// be a second copy of the source for something to disagree with.
func EnsureControllerSource(stateDir string, remote RemoteIdentity, credentials CredentialProvider) (string, error) {
	dir := ControllerSourceDir(stateDir)
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0700); err != nil {
		return "", err
	}
	// A partial clone from an earlier interrupted attempt is removed rather
	// than reused: a directory that is not a clone is not a transport, and
	// discovering that during an upgrade is worse than discovering it here.
	if err := os.RemoveAll(dir); err != nil {
		return "", err
	}
	if _, err := remoteGit("", remote, credentials).run("clone", "--no-checkout", remote.URL, dir); err != nil {
		return "", fmt.Errorf("the controller's own source could not be cloned to %s: %w", dir, err)
	}
	return dir, nil
}
