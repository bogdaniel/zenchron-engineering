package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

// retainingStore is what both artifact stores offer beyond api.ArtifactStore.
type retainingStore interface {
	api.ArtifactStore
	Retain(ctx context.Context, ref api.ArtifactRef) error
	Release(ctx context.Context, ref api.ArtifactRef) error
	Delete(ctx context.Context, ref api.ArtifactRef) error
}

func artifactStores(t *testing.T, maxBytes int64) map[string]retainingStore {
	mem, err := NewMemoryArtifacts(maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	file, err := OpenFileArtifacts(filepath.Join(t.TempDir(), "artifacts"), maxBytes)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]retainingStore{"memory": mem, "file": file}
}

func input(data string) api.ArtifactInput {
	return api.ArtifactInput{MediaType: "text/plain", Producer: "exec/attempt/call-1", Data: []byte(data)}
}

func TestArtifactRoundTripAndBinding(t *testing.T) {
	ctx := context.Background()
	for name, s := range artifactStores(t, 1<<20) {
		t.Run(name, func(t *testing.T) {
			ref, err := s.Put(ctx, input("full output"))
			if err != nil {
				t.Fatal(err)
			}
			if ref.Digest != api.Digest([]byte("full output")) || ref.Size != 11 || ref.Producer != "exec/attempt/call-1" {
				t.Fatalf("ref = %+v", ref)
			}
			got, err := s.Get(ctx, ref)
			if err != nil || string(got) != "full output" {
				t.Fatalf("Get = %q, %v", got, err)
			}
			other := ref
			other.Producer = "someone-else"
			if _, err := s.Get(ctx, other); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get with foreign producer = %v, want ErrNotFound", err)
			}
			if _, err := s.Put(ctx, api.ArtifactInput{MediaType: "", Producer: "p", Data: nil}); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Put without media type = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestArtifactCapacityRefusesInsteadOfEvicting(t *testing.T) {
	ctx := context.Background()
	for name, s := range artifactStores(t, 200) {
		t.Run(name, func(t *testing.T) {
			first, err := s.Put(ctx, input(string(bytes.Repeat([]byte("a"), 60))))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Put(ctx, input(string(bytes.Repeat([]byte("b"), 150)))); !errors.Is(err, ErrCapacity) {
				t.Fatalf("over-bound Put = %v, want ErrCapacity", err)
			}
			if _, err := s.Get(ctx, first); err != nil {
				t.Fatalf("existing artifact evicted: %v", err)
			}
			if err := s.Retain(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, first); !errors.Is(err, ErrRetained) {
				t.Fatalf("Delete of retained = %v, want ErrRetained", err)
			}
			if err := s.Release(ctx, first); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, first); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Get(ctx, first); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get after Delete = %v", err)
			}
			if _, err := s.Put(ctx, input(string(bytes.Repeat([]byte("b"), 150)))); err != nil {
				t.Fatalf("Put after freeing bytes = %v", err)
			}
		})
	}
}

func TestFileArtifactsDetectCorruptionAndReopen(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "a")
	s, err := OpenFileArtifacts(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(ctx, input("exact bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, ref); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileArtifacts(root, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Get(ctx, ref); err != nil || string(got) != "exact bytes" {
		t.Fatalf("reopened Get = %q, %v", got, err)
	}
	if reopened.used != s.used {
		t.Fatalf("reopened accounting %d != %d", reopened.used, s.used)
	}
	if err := reopened.Delete(ctx, ref); !errors.Is(err, ErrRetained) {
		t.Fatalf("retain mark lost across restart: %v", err)
	}
	raw, err := os.ReadFile(s.path(ref))
	if err != nil {
		t.Fatal(err)
	}
	header, _, _ := bytes.Cut(raw, []byte("\n"))
	cases := map[string][]byte{
		"data flipped":   bytes.Replace(raw, []byte("exact"), []byte("EXACT"), 1),
		"truncated":      raw[:len(raw)-2],
		"trailing bytes": append(bytes.Clone(raw), '!'),
		"media swapped":  bytes.Replace(raw, []byte("text/plain"), []byte("text/html!"), 1),
		"no header":      []byte("exact bytes"),
		"header only":    header,
	}
	for name, content := range cases {
		if err := os.WriteFile(s.path(ref), content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := reopened.Get(ctx, ref); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Get = %v, want ErrCorrupt", name, err)
		}
	}
}

func TestArtifactsConcurrentPutGet(t *testing.T) {
	ctx := context.Background()
	for name, s := range artifactStores(t, 1<<20) {
		t.Run(name, func(t *testing.T) {
			var wg sync.WaitGroup
			errs := make(chan error, 32)
			for w := range 8 {
				wg.Go(func() {
					for i := range 10 {
						data := fmt.Sprintf("payload-%d", (w+i)%5)
						ref, err := s.Put(ctx, input(data))
						if err != nil {
							errs <- err
							return
						}
						got, err := s.Get(ctx, ref)
						if err != nil || string(got) != data {
							errs <- fmt.Errorf("Get = %q, %v", got, err)
							return
						}
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// A Put over a corrupt existing copy must repair it, not report it stored.
func TestFileArtifactsPutRepairsCorruptCopy(t *testing.T) {
	ctx := context.Background()
	s, err := OpenFileArtifacts(filepath.Join(t.TempDir(), "a"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Put(ctx, input("exact bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(ref), []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	used := s.used
	if _, err := s.Put(ctx, input("exact bytes")); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Get(ctx, ref); err != nil || string(got) != "exact bytes" {
		t.Fatalf("Get after repairing Put = %q, %v", got, err)
	}
	if s.used != used {
		t.Fatalf("repair changed accounting %d -> %d", used, s.used)
	}
}
