// Package storage holds the module-owned artifact and record stores: in-memory
// and file-backed. Storage roots are always supplied explicitly.
package storage

import (
	"context"
	"errors"
)

// ErrNotFound reports an absent record or artifact.
var ErrNotFound = errors.New("storage: not found")

// ErrCorrupt reports bytes that fail integrity verification.
var ErrCorrupt = errors.New("storage: corrupt")

// Records is a partitioned key/value store for derived state (memory records,
// index snapshots). Partition and key are validated identifiers; a value is
// written atomically and verified on read.
type Records interface {
	Put(ctx context.Context, partition, key string, value []byte) error
	Get(ctx context.Context, partition, key string) ([]byte, error)
	List(ctx context.Context, partition string) ([]string, error)
	Delete(ctx context.Context, partition, key string) error
}
