package intelligence_test

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/intelligence"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

var linux = intelligence.Settings{GoVersion: "go1.25.0", GOOS: "linux", GOARCH: "amd64"}

// brokenGo is written at test time so no unparseable file is committed where
// gofmt walks.
const brokenGo = "package broken\n\nfunc Good() {}\n\nfunc Bad( {\n"

// workspace copies a testdata module to a fresh directory, adds the broken
// package for the basic fixture, and applies changes on disk.
func workspace(t *testing.T, fixture string, changes ...intelligence.Change) string {
	t.Helper()
	dst := t.TempDir()
	src := filepath.Join("testdata", fixture)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return writeFile(dst, rel, b)
	})
	if err != nil {
		t.Fatal(err)
	}
	if fixture == "basic" {
		if err := writeFile(dst, "broken/broken.go", []byte(brokenGo)); err != nil {
			t.Fatal(err)
		}
	}
	applyOnDisk(t, dst, changes)
	return dst
}

func applyOnDisk(t *testing.T, root string, changes []intelligence.Change) {
	t.Helper()
	for _, c := range changes {
		if c.RenamedFrom != "" || c.Delete {
			old := c.RenamedFrom
			if c.Delete {
				old = c.Path
			}
			if err := os.Remove(filepath.Join(root, filepath.FromSlash(old))); err != nil {
				t.Fatal(err)
			}
		}
		if !c.Delete {
			if err := writeFile(root, c.Path, c.Content); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func writeFile(root, rel string, b []byte) error {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

func build(t *testing.T, root string, settings intelligence.Settings, scope intelligence.Scope) *intelligence.Index {
	t.Helper()
	ix, err := intelligence.Build(context.Background(), intelligence.BuildConfig{Root: root, Settings: settings, Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// snapshotDigest digests every fact except the overlay binding, so an overlay
// can be compared with a fresh build of the same tree.
func snapshotDigest(t *testing.T, ix *intelligence.Index) string {
	t.Helper()
	s := ix.Snapshot()
	s.Overlay = nil
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return api.Digest(b)
}

func fullDigest(t *testing.T, ix *intelligence.Index) string {
	t.Helper()
	b, err := json.Marshal(ix.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	return api.Digest(b)
}

func symbols(ix *intelligence.Index) map[string]intelligence.Symbol {
	out := map[string]intelligence.Symbol{}
	for _, df := range ix.Snapshot().Dirs {
		for _, s := range df.Symbols {
			out[s.ID] = s
		}
	}
	return out
}

func calls(ix *intelligence.Index) []intelligence.Call {
	var out []intelligence.Call
	for _, df := range ix.Snapshot().Dirs {
		out = append(out, df.Calls...)
	}
	return out
}

func findCall(cs []intelligence.Call, caller, calleeSubstr string) (intelligence.Call, bool) {
	for _, c := range cs {
		if c.Caller == caller && strings.Contains(c.Callee, calleeSubstr) {
			return c, true
		}
	}
	return intelligence.Call{}, false
}

func mentionsFile(t *testing.T, ix *intelligence.Index, file string) bool {
	t.Helper()
	b, err := json.Marshal(ix.Snapshot().Dirs)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Contains(string(b), `"`+file+`"`)
}

// memRecords is a minimal storage.Records for tests. Its map can be shared by
// two instances to model a process restart over the same durable state.
type memRecords struct {
	mu      sync.Mutex
	data    map[string][]byte
	failPut bool
}

func newRecords() *memRecords { return &memRecords{data: map[string][]byte{}} }

func (m *memRecords) reopen() *memRecords { return &memRecords{data: m.data} }

func (m *memRecords) Put(_ context.Context, partition, key string, value []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failPut {
		return os.ErrPermission
	}
	m.data[partition+"/"+key] = append([]byte(nil), value...)
	return nil
}

func (m *memRecords) PutIfAbsent(ctx context.Context, partition, key string, value []byte) error {
	if _, err := m.Get(ctx, partition, key); err == nil {
		return storage.ErrExists
	}
	return m.Put(ctx, partition, key, value)
}

func (m *memRecords) Get(_ context.Context, partition, key string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.data[partition+"/"+key]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return append([]byte(nil), b...), nil
}

func (m *memRecords) List(_ context.Context, partition string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var keys []string
	for k := range m.data {
		if rest, ok := strings.CutPrefix(k, partition+"/"); ok {
			keys = append(keys, rest)
		}
	}
	return keys, nil
}

func (m *memRecords) Delete(_ context.Context, partition, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, partition+"/"+key)
	return nil
}
