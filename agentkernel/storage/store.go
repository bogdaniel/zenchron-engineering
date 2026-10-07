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

// ErrExists reports a PutIfAbsent refused because the key already holds a
// value.
var ErrExists = errors.New("storage: already exists")

// ErrRetained reports a delete refused because the artifact is retained.
var ErrRetained = errors.New("storage: artifact is retained")

// Records is a partitioned key/value store for derived state (memory records,
// index snapshots, admission claims). Partition and key are validated
// identifiers; a value is written atomically and verified on read.
//
// PutIfAbsent writes value only if key holds none, in one atomic step: it
// returns ErrExists, and changes nothing, when the key is taken. It is never
// a check followed by a write, so of any number of concurrent writers of one
// key exactly one succeeds; FileRecords keeps that across processes sharing
// a root on one local filesystem.
type Records interface {
	Put(ctx context.Context, partition, key string, value []byte) error
	PutIfAbsent(ctx context.Context, partition, key string, value []byte) error
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

// writeAtomic replaces name in dir with data: a synced temp file renamed
// over it, then a directory fsync so the rename itself survives a crash.
func writeAtomic(dir, name string, data []byte) error {
	tmp, err := writeTemp(dir, data)
	if err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		return errors.Join(err, removeIfExists(tmp))
	}
	return syncDir(dir)
}

// writeTemp writes data to a new synced temp file in dir and returns its
// path; on failure no temp file is left behind.
func writeTemp(dir string, data []byte) (name string, err error) {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, removeIfExists(tmp.Name()))
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return "", errors.Join(err, tmp.Close())
	}
	if err = tmp.Sync(); err != nil {
		return "", errors.Join(err, tmp.Close())
	}
	if err = tmp.Close(); err != nil {
		return "", err
	}
	return tmp.Name(), nil
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
