package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unicode"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

const (
	maxMediaTypeBytes = 255
	maxProducerBytes  = 1024
	maxHeaderBytes    = 4096
)

func validLabel(s string, limit int) bool {
	return s != "" && len(s) <= limit && !strings.ContainsFunc(s, unicode.IsControl)
}

func refOf(in api.ArtifactInput) (api.ArtifactRef, error) {
	if !validLabel(in.MediaType, maxMediaTypeBytes) || !validLabel(in.Producer, maxProducerBytes) {
		return api.ArtifactRef{}, fmt.Errorf("%w: artifact needs a media type and producer without control characters", ErrInvalid)
	}
	return api.ArtifactRef{
		Digest: api.Digest(in.Data), Size: int64(len(in.Data)), MediaType: in.MediaType, Producer: in.Producer,
	}, nil
}

func validRef(ref api.ArtifactRef) error {
	if !api.ValidDigest(ref.Digest) || ref.Size < 0 ||
		!validLabel(ref.MediaType, maxMediaTypeBytes) || !validLabel(ref.Producer, maxProducerBytes) {
		return fmt.Errorf("%w: artifact reference", ErrInvalid)
	}
	return nil
}

// refKey names one stored artifact by its content digest and its metadata
// binding, so the same bytes stored by two producers stay two verifiable
// records. Labels carry no control characters, so NUL separation is exact.
func refKey(ref api.ArtifactRef) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s\x00%d\x00%s\x00%s", ref.Digest, ref.Size, ref.MediaType, ref.Producer))
	return hex.EncodeToString(sum[:])
}

func verify(ref api.ArtifactRef, data []byte) ([]byte, error) {
	if int64(len(data)) != ref.Size || api.Digest(data) != ref.Digest {
		return nil, fmt.Errorf("%w: artifact %s does not match its digest or size", ErrCorrupt, ref.Digest)
	}
	return data, nil
}

// MemoryArtifacts is a process-local api.ArtifactStore bounded by total bytes.
type MemoryArtifacts struct {
	mu       sync.Mutex
	maxBytes int64
	used     int64
	items    map[api.ArtifactRef][]byte
	retained map[api.ArtifactRef]bool
}

// NewMemoryArtifacts returns a store that refuses a Put which would hold more
// than maxBytes of artifact data in total.
func NewMemoryArtifacts(maxBytes int64) (*MemoryArtifacts, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: maxBytes must be positive", ErrInvalid)
	}
	return &MemoryArtifacts{
		maxBytes: maxBytes, items: map[api.ArtifactRef][]byte{}, retained: map[api.ArtifactRef]bool{},
	}, nil
}

func (m *MemoryArtifacts) Put(_ context.Context, in api.ArtifactInput) (api.ArtifactRef, error) {
	ref, err := refOf(in)
	if err != nil {
		return api.ArtifactRef{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[ref]; ok {
		return ref, nil
	}
	if m.used+ref.Size > m.maxBytes {
		return api.ArtifactRef{}, fmt.Errorf("%w: %d + %d > %d bytes", ErrCapacity, m.used, ref.Size, m.maxBytes)
	}
	m.items[ref] = bytes.Clone(in.Data)
	m.used += ref.Size
	return ref, nil
}

func (m *MemoryArtifacts) Get(_ context.Context, ref api.ArtifactRef) ([]byte, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	m.mu.Lock()
	data, ok := m.items[ref]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return verify(ref, bytes.Clone(data))
}

// Retain marks ref as referenced by live work; Delete then refuses it.
func (m *MemoryArtifacts) Retain(_ context.Context, ref api.ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[ref]; !ok {
		return ErrNotFound
	}
	m.retained[ref] = true
	return nil
}

// Release clears a Retain mark.
func (m *MemoryArtifacts) Release(_ context.Context, ref api.ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[ref]; !ok {
		return ErrNotFound
	}
	delete(m.retained, ref)
	return nil
}

// Delete removes an unretained artifact and frees its bytes.
func (m *MemoryArtifacts) Delete(_ context.Context, ref api.ArtifactRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.items[ref]; !ok {
		return ErrNotFound
	}
	if m.retained[ref] {
		return ErrRetained
	}
	delete(m.items, ref)
	m.used -= ref.Size
	return nil
}

// FileArtifacts is a file-backed api.ArtifactStore rooted at an explicit
// directory. Each artifact is one file: a JSON header line binding digest,
// size, media type and producer, then the exact bytes. A Retain mark is a
// sibling ".pin" file so it survives a restart.
type FileArtifacts struct {
	root     string
	maxBytes int64
	mu       sync.Mutex // serializes Put/Delete/Retain so accounting stays exact
	used     int64
}

type artifactHeader struct {
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	Producer  string `json:"producer"`
}

// OpenFileArtifacts opens (creating if needed) a store at root, an absolute
// path. maxBytes bounds the artifact data bytes held, as in MemoryArtifacts.
func OpenFileArtifacts(root string, maxBytes int64) (*FileArtifacts, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("%w: maxBytes must be positive", ErrInvalid)
	}
	if err := prepareRoot(root); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	s := &FileArtifacts{root: root, maxBytes: maxBytes}
	for _, e := range entries {
		if len(e.Name()) != sha256.Size*2 || !e.Type().IsRegular() {
			continue // pins and leftover temp files hold no artifact bytes
		}
		size, err := storedSize(filepath.Join(root, e.Name()))
		if err != nil {
			return nil, err
		}
		s.used += size
	}
	return s, nil
}

// storedSize is the data size an artifact file accounts for. A file whose
// header is unreadable counts in full: corruption must not free capacity.
func storedSize(name string) (int64, error) {
	f, err := os.Open(name)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head, err := io.ReadAll(io.LimitReader(f, maxHeaderBytes))
	if err != nil {
		return 0, err
	}
	line, _, ok := bytes.Cut(head, []byte("\n"))
	var h artifactHeader
	if ok && json.Unmarshal(line, &h) == nil && h.Size >= 0 {
		return h.Size, nil
	}
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (s *FileArtifacts) path(ref api.ArtifactRef) string { return filepath.Join(s.root, refKey(ref)) }

func (s *FileArtifacts) Put(ctx context.Context, in api.ArtifactInput) (api.ArtifactRef, error) {
	ref, err := refOf(in)
	if err != nil {
		return api.ArtifactRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return api.ArtifactRef{}, err
	}
	header, err := json.Marshal(artifactHeader(ref))
	if err != nil {
		return api.ArtifactRef{}, err
	}
	framed := append(append(header, '\n'), in.Data...)
	s.mu.Lock()
	defer s.mu.Unlock()
	// An existing copy counts as stored only if it verifies; a corrupt one is
	// rewritten in place (its key is already in the accounting).
	existing := false
	if _, err := os.Lstat(s.path(ref)); err == nil {
		if _, err := s.Get(ctx, ref); err == nil {
			return ref, nil
		}
		existing = true
	}
	if !existing && s.used+ref.Size > s.maxBytes {
		return api.ArtifactRef{}, fmt.Errorf("%w: %d + %d > %d bytes", ErrCapacity, s.used, ref.Size, s.maxBytes)
	}
	if err := writeAtomic(s.root, refKey(ref), framed); err != nil {
		return api.ArtifactRef{}, err
	}
	if !existing {
		s.used += ref.Size
	}
	return ref, nil
}

func (s *FileArtifacts) Get(ctx context.Context, ref api.ArtifactRef) ([]byte, error) {
	if err := validRef(ref); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(s.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	// One byte past the expected length so trailing garbage is detected.
	raw, err := io.ReadAll(io.LimitReader(f, maxHeaderBytes+ref.Size+1))
	if err != nil {
		return nil, err
	}
	line, data, ok := bytes.Cut(raw, []byte("\n"))
	var h artifactHeader
	if !ok || json.Unmarshal(line, &h) != nil || api.ArtifactRef(h) != ref {
		return nil, fmt.Errorf("%w: artifact %s header does not match its reference", ErrCorrupt, ref.Digest)
	}
	return verify(ref, data)
}

// Retain marks ref as referenced by live work; Delete then refuses it, also
// after a restart.
func (s *FileArtifacts) Retain(ctx context.Context, ref api.ArtifactRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.present(ctx, ref); err != nil {
		return err
	}
	return writeAtomic(s.root, refKey(ref)+".pin", nil)
}

// Release clears a Retain mark.
func (s *FileArtifacts) Release(ctx context.Context, ref api.ArtifactRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.present(ctx, ref); err != nil {
		return err
	}
	if err := removeIfExists(s.path(ref) + ".pin"); err != nil {
		return err
	}
	return syncDir(s.root)
}

// Delete removes an unretained artifact and frees its bytes.
func (s *FileArtifacts) Delete(ctx context.Context, ref api.ArtifactRef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.present(ctx, ref); err != nil {
		return err
	}
	_, err := os.Lstat(s.path(ref) + ".pin")
	if err == nil {
		return ErrRetained
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	size, err := storedSize(s.path(ref))
	if err != nil {
		return err
	}
	if err := os.Remove(s.path(ref)); err != nil {
		return err
	}
	s.used -= size
	return syncDir(s.root)
}

func (s *FileArtifacts) present(ctx context.Context, ref api.ArtifactRef) error {
	if err := validRef(ref); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := os.Lstat(s.path(ref))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}
