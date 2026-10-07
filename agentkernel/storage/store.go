// Package storage holds the module-owned artifact and record stores: in-memory
// and file-backed. Storage roots are always supplied explicitly.
package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ErrNotFound reports an absent record or artifact.
var ErrNotFound = errors.New("storage: not found")

// ErrCorrupt reports bytes that fail integrity verification.
var ErrCorrupt = errors.New("storage: corrupt")

// ErrInvalid reports a malformed partition, key, artifact input or reference.
var ErrInvalid = errors.New("storage: invalid")

// ErrCapacity reports a write refused because it would exceed the store's
// configured bound. Stores refuse; they never silently evict to make room.
var ErrCapacity = errors.New("storage: capacity exceeded")

// ErrRetained reports a delete refused because the artifact is retained.
var ErrRetained = errors.New("storage: artifact is retained")

// Records is a partitioned key/value store for derived state (memory records,
// index snapshots). Partition and key are validated identifiers; a value is
// written atomically and verified on read.
type Records interface {
	Put(ctx context.Context, partition, key string, value []byte) error
	Get(ctx context.Context, partition, key string) ([]byte, error)
	List(ctx context.Context, partition string) ([]string, error)
	Delete(ctx context.Context, partition, key string) error
}

// prepareRoot validates an explicitly supplied storage root and creates it.
// Windows is refused rather than half-supported: directory fsync, which the
// atomic-rename durability argument relies on, is not available there.
func prepareRoot(root string) error {
	if runtime.GOOS == "windows" {
		return fmt.Errorf("%w: file-backed stores are not supported on windows in Gate A", ErrInvalid)
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("%w: storage root %q must be a clean absolute path", ErrInvalid, root)
	}
	return os.MkdirAll(root, 0o700)
}

// writeAtomic replaces name in dir with data: temp file, fsync, rename, then
// fsync the directory so the rename itself survives a crash.
func writeAtomic(dir, name string, data []byte) (err error) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfExists(tmp.Name()))
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err = tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(dir, name)); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(d.Sync(), d.Close())
}

func removeIfExists(name string) error {
	if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
