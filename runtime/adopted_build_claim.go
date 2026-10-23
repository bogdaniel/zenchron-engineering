package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrAdoptedBuildInProgress reports that another process holds the build of
// this exact revision, so this one must not touch its container.
var ErrAdoptedBuildInProgress = errors.New("another build of this revision is in progress")

// adoptedBuildClaim is the EXCLUSIVE OWNERSHIP of one revision's build, held
// for the whole of runAdoptedBuild.
//
// Why a lock and not a look at the container (#674 review): the container name
// is deterministic per operation, so the controller's updater and a
// simultaneous `controller build-adopted` of the same revision use the same
// name. A container that exists and is not running is NOT thereby abandoned -
// it may be the other build's, created and about to be started - and the
// updater's in-process mutex cannot see another process. An advisory lock is
// dropped by the kernel when its holder dies, so "the owner holds the lock"
// and "the attempt is live" are the same fact, and a leftover container can
// only be released by whoever holds the claim.
//
// Both entrypoints reach runAdoptedBuild through AdoptedBuildDeps.Build, and
// both configure the same Docker operation directory, so they contend on the
// same file.
type adoptedBuildClaim struct{ file *os.File }

func claimAdoptedBuild(stateDir, revision string) (*adoptedBuildClaim, error) {
	if stateDir == "" || revision == "" {
		return nil, fmt.Errorf("an adopted build needs its runtime-owned state directory and a revision to claim it")
	}
	// Named by a digest, never by the revision text, so no spelling of a
	// revision can address another path.
	name := sha256.Sum256([]byte("adopted-build\x00" + revision))
	path := filepath.Join(stateDir, "adopted-build-"+hex.EncodeToString(name[:16])+".lock")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("claim the adopted build of %s: %w", shortSHA(revision), err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claim the adopted build of %s: %w", shortSHA(revision), err)
	}
	locked, err := tryLockFile(file, true)
	if err == nil && !locked {
		err = fmt.Errorf("%w (%s); its container is left alone", ErrAdoptedBuildInProgress, shortSHA(revision))
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &adoptedBuildClaim{file: file}, nil
}

// Release gives the claim up. The lock file is never unlinked: authority
// attaches to the inode.
func (c *adoptedBuildClaim) Release() {
	if c != nil && c.file != nil {
		_ = c.file.Close()
		c.file = nil
	}
}
