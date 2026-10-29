package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// MaxRecordBytes bounds one record value.
const MaxRecordBytes = 16 << 20

func validName(partition, key string) error {
	if !api.ValidIdentifier(partition) {
		return fmt.Errorf("%w: partition %q", ErrInvalid, partition)
	}
	if key != "" && !api.ValidIdentifier(key) {
		return fmt.Errorf("%w: key %q", ErrInvalid, key)
	}
	return nil
}

func validRecord(partition, key string, value []byte) error {
	if key == "" {
		return fmt.Errorf("%w: empty key", ErrInvalid)
	}
	if err := validName(partition, key); err != nil {
		return err
	}
	if len(value) > MaxRecordBytes {
		return fmt.Errorf("%w: value of %d bytes exceeds %d", ErrInvalid, len(value), MaxRecordBytes)
	}
	return nil
}

// MemoryRecords is a process-local Records implementation.
type MemoryRecords struct {
	mu    sync.RWMutex
	parts map[string]map[string][]byte
}

// NewMemoryRecords returns an empty in-memory store.
func NewMemoryRecords() *MemoryRecords {
	return &MemoryRecords{parts: map[string]map[string][]byte{}}
}

func (m *MemoryRecords) Put(_ context.Context, partition, key string, value []byte) error {
	if err := validRecord(partition, key, value); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.parts[partition] == nil {
		m.parts[partition] = map[string][]byte{}
	}
	m.parts[partition][key] = bytes.Clone(value)
	return nil
}

func (m *MemoryRecords) PutIfAbsent(_ context.Context, partition, key string, value []byte) error {
	if err := validRecord(partition, key, value); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, taken := m.parts[partition][key]; taken {
		return ErrExists
	}
	if m.parts[partition] == nil {
		m.parts[partition] = map[string][]byte{}
	}
	m.parts[partition][key] = bytes.Clone(value)
	return nil
}

func (m *MemoryRecords) Get(_ context.Context, partition, key string) ([]byte, error) {
	if err := validRecord(partition, key, nil); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.parts[partition][key]
	if !ok {
		return nil, ErrNotFound
	}
	return bytes.Clone(v), nil
}

func (m *MemoryRecords) List(_ context.Context, partition string) ([]string, error) {
	if err := validName(partition, ""); err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0, len(m.parts[partition]))
	for k := range m.parts[partition] {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys, nil
}

func (m *MemoryRecords) Delete(_ context.Context, partition, key string) error {
	if err := validRecord(partition, key, nil); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.parts[partition][key]; !ok {
		return ErrNotFound
	}
	delete(m.parts[partition], key)
	return nil
}

// nameEncoding maps identifiers to file names. Identifiers may differ only in
// case or contain ':', which collide or misbehave on common filesystems; the
// single-case base32hex alphabet avoids both and never starts with '.', so
// temp files can never be mistaken for records.
var nameEncoding = base32.HexEncoding.WithPadding(base32.NoPadding)

const recordMagic = "zkrec1"

// FileRecords is a Records implementation rooted at an explicit directory.
// Each record is one file framed with its length and sha256, replaced
// atomically, so a torn or altered file reads as ErrCorrupt, never as data.
type FileRecords struct{ root string }

// OpenFileRecords opens (creating if needed) a store at root, an absolute path.
// Reopening the same root after a restart sees every record written before.
func OpenFileRecords(root string) (*FileRecords, error) {
	if err := prepareRoot(root); err != nil {
		return nil, err
	}
	return &FileRecords{root: root}, nil
}

func (f *FileRecords) partitionDir(partition string) string {
	return filepath.Join(f.root, nameEncoding.EncodeToString([]byte(partition)))
}

func (f *FileRecords) path(partition, key string) string {
	return filepath.Join(f.partitionDir(partition), nameEncoding.EncodeToString([]byte(key)))
}

func (f *FileRecords) Put(ctx context.Context, partition, key string, value []byte) error {
	if err := validRecord(partition, key, value); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := f.partitionDir(partition)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return writeAtomic(dir, nameEncoding.EncodeToString([]byte(key)), frameRecord(value))
}

// PutIfAbsent writes the framed value to a synced temp file and links it to
// the record's name. link(2) creates the name only if it does not exist, as
// one atomic step, and the name appears with its full content, so neither a
// racing writer in another process nor a crash can expose a partial value or
// let two writers both succeed.
func (f *FileRecords) PutIfAbsent(ctx context.Context, partition, key string, value []byte) (err error) {
	if err := validRecord(partition, key, value); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := f.partitionDir(partition)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := writeTemp(dir, frameRecord(value))
	if err != nil {
		return err
	}
	defer func() {
		if rerr := removeIfExists(tmp); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}()
	err = os.Link(tmp, f.path(partition, key))
	if errors.Is(err, os.ErrExist) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	return syncDir(dir)
}

func (f *FileRecords) Get(ctx context.Context, partition, key string) ([]byte, error) {
	if err := validRecord(partition, key, nil); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(f.path(partition, key))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	// The +128 admits the frame header; anything longer is corrupt by definition.
	raw, err := io.ReadAll(io.LimitReader(file, MaxRecordBytes+128))
	if err != nil {
		return nil, err
	}
	return unframeRecord(raw)
}

func (f *FileRecords) List(ctx context.Context, partition string) ([]string, error) {
	if err := validName(partition, ""); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(f.partitionDir(partition))
	if errors.Is(err, os.ErrNotExist) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(entries))
	for _, e := range entries {
		raw, err := nameEncoding.DecodeString(e.Name())
		if err != nil || !e.Type().IsRegular() || !api.ValidIdentifier(string(raw)) {
			continue // temp files and foreign entries are not records
		}
		keys = append(keys, string(raw))
	}
	slices.Sort(keys)
	return keys, nil
}

func (f *FileRecords) Delete(ctx context.Context, partition, key string) error {
	if err := validRecord(partition, key, nil); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := os.Remove(f.path(partition, key))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return syncDir(f.partitionDir(partition))
}

func frameRecord(value []byte) []byte {
	sum := sha256.Sum256(value)
	header := fmt.Sprintf("%s %s %d\n", recordMagic, hex.EncodeToString(sum[:]), len(value))
	return append([]byte(header), value...)
}

func unframeRecord(raw []byte) ([]byte, error) {
	header, value, ok := bytes.Cut(raw, []byte("\n"))
	if !ok {
		return nil, fmt.Errorf("%w: missing record header", ErrCorrupt)
	}
	fields := bytes.Fields(header)
	if len(fields) != 3 || string(fields[0]) != recordMagic {
		return nil, fmt.Errorf("%w: malformed record header", ErrCorrupt)
	}
	size, err := strconv.Atoi(string(fields[2]))
	if err != nil || size != len(value) {
		return nil, fmt.Errorf("%w: record length mismatch", ErrCorrupt)
	}
	sum := sha256.Sum256(value)
	if hex.EncodeToString(sum[:]) != string(fields[1]) {
		return nil, fmt.Errorf("%w: record checksum mismatch", ErrCorrupt)
	}
	return value, nil
}
