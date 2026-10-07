package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
)

func recordStores(t *testing.T) map[string]func() Records {
	root := filepath.Join(t.TempDir(), "records")
	return map[string]func() Records{
		"memory": func() Records { return NewMemoryRecords() },
		"file": func() Records {
			s, err := OpenFileRecords(root)
			if err != nil {
				t.Fatal(err)
			}
			return s
		},
	}
}

func TestRecordsContract(t *testing.T) {
	ctx := context.Background()
	for name, open := range recordStores(t) {
		t.Run(name, func(t *testing.T) {
			s := open()
			if _, err := s.Get(ctx, "p", "missing"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("absent Get = %v, want ErrNotFound", err)
			}
			for _, k := range []string{"b", "A", "a", "c:1"} {
				if err := s.Put(ctx, "p", k, []byte("v-"+k)); err != nil {
					t.Fatal(err)
				}
			}
			keys, err := s.List(ctx, "p")
			if err != nil || !slices.Equal(keys, []string{"A", "a", "b", "c:1"}) {
				t.Fatalf("List = %v, %v; want sorted keys distinct by case", keys, err)
			}
			got, err := s.Get(ctx, "p", "A")
			if err != nil || string(got) != "v-A" {
				t.Fatalf("Get A = %q, %v", got, err)
			}
			if err := s.Delete(ctx, "p", "A"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "p", "A"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("second Delete = %v, want ErrNotFound", err)
			}
			if keys, _ := s.List(ctx, "other"); len(keys) != 0 {
				t.Fatalf("partitions leak: %v", keys)
			}
		})
	}
}

func TestRecordsRefuseUnsafeNames(t *testing.T) {
	ctx := context.Background()
	bad := []string{"", "../x", "a/b", ".hidden", "a\\b", "x\x00", "/abs"}
	for name, open := range recordStores(t) {
		s := open()
		for _, n := range bad {
			if err := s.Put(ctx, n, "k", nil); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: Put partition %q = %v, want ErrInvalid", name, n, err)
			}
			if err := s.Put(ctx, "p", n, nil); !errors.Is(err, ErrInvalid) {
				t.Errorf("%s: Put key %q = %v, want ErrInvalid", name, n, err)
			}
		}
	}
}

func TestFileRecordsReopenAndDetectCorruption(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "r")
	first, err := OpenFileRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Put(ctx, "mem", "good", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	if err := first.Put(ctx, "mem", "torn", []byte("will be torn")); err != nil {
		t.Fatal(err)
	}
	// A restart is a new instance on the same root.
	second, err := OpenFileRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.Get(ctx, "mem", "good"); err != nil || string(got) != "kept" {
		t.Fatalf("reopened Get = %q, %v", got, err)
	}
	torn := second.path("mem", "torn")
	raw, err := os.ReadFile(torn)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string][]byte{
		"truncated": raw[:len(raw)-3],
		"flipped":   append(raw[:len(raw)-1:len(raw)-1], 'X'),
		"extended":  append(slices.Clone(raw), 'Y'),
		"no header": []byte("raw value"),
	}
	for name, content := range cases {
		if err := os.WriteFile(torn, content, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := second.Get(ctx, "mem", "torn"); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: Get = %v, want ErrCorrupt", name, err)
		}
	}
	if err := os.WriteFile(filepath.Join(second.partitionDir("mem"), ".tmp-123"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	keys, err := second.List(ctx, "mem")
	if err != nil || !slices.Equal(keys, []string{"good", "torn"}) {
		t.Fatalf("List = %v, %v; temp files must not appear", keys, err)
	}
}

func TestOpenFileRecordsRequiresAbsoluteRoot(t *testing.T) {
	if _, err := OpenFileRecords("relative/dir"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("relative root = %v, want ErrInvalid", err)
	}
}

func TestRecordsConcurrentReadersWriters(t *testing.T) {
	ctx := context.Background()
	for name, open := range recordStores(t) {
		t.Run(name, func(t *testing.T) {
			s := open()
			var wg sync.WaitGroup
			errs := make(chan error, 64)
			for w := range 8 {
				wg.Go(func() {
					for i := range 20 {
						key := fmt.Sprintf("k%d", i%4)
						if err := s.Put(ctx, "p", key, []byte(fmt.Sprintf("w%d-%d", w, i))); err != nil {
							errs <- err
							return
						}
						v, err := s.Get(ctx, "p", key)
						if err != nil || len(v) == 0 {
							errs <- fmt.Errorf("concurrent Get %s = %q, %v", key, v, err)
							return
						}
						if _, err := s.List(ctx, "p"); err != nil {
							errs <- err
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

// TestPutIfAbsentAdmitsExactlyOne: many writers race to create one key, each
// through its own store handle where the store has handles (two processes
// opening one FileRecords root). Exactly one succeeds, every other gets
// ErrExists, the winner's full value is what any reader ever sees, and no
// temp file is left behind.
func TestPutIfAbsentAdmitsExactlyOne(t *testing.T) {
	ctx := context.Background()
	for name, open := range recordStores(t) {
		t.Run(name, func(t *testing.T) {
			shared := open()
			handle := func() Records {
				if name == "memory" {
					return shared
				}
				return open()
			}
			const n = 16
			value := func(i int) []byte { return []byte(fmt.Sprintf("attempt-%02d:%0512d", i, i)) }
			var wg sync.WaitGroup
			wins := make(chan int, n)
			for i := range n {
				s := handle()
				wg.Go(func() {
					err := s.PutIfAbsent(ctx, "claims", "exec-1", value(i))
					if err == nil {
						wins <- i
						return
					}
					if !errors.Is(err, ErrExists) {
						t.Errorf("writer %d: %v", i, err)
					}
				})
				wg.Go(func() {
					got, err := s.Get(ctx, "claims", "exec-1")
					if err == nil && len(got) != len(value(0)) {
						t.Errorf("reader saw a partial value of %d bytes", len(got))
					}
				})
			}
			wg.Wait()
			close(wins)
			var winners []int
			for i := range wins {
				winners = append(winners, i)
			}
			if len(winners) != 1 {
				t.Fatalf("%d writers created the key, want exactly 1", len(winners))
			}
			got, err := handle().Get(ctx, "claims", "exec-1")
			if err != nil || string(got) != string(value(winners[0])) {
				t.Fatalf("stored %q, %v; want the winner's value", got, err)
			}
			if err := shared.PutIfAbsent(ctx, "claims", "exec-1", []byte("late")); !errors.Is(err, ErrExists) {
				t.Fatalf("PutIfAbsent over an existing key = %v, want ErrExists", err)
			}
			if keys, _ := shared.List(ctx, "claims"); len(keys) != 1 {
				t.Fatalf("keys %v: a second record was left", keys)
			}
			if f, ok := shared.(*FileRecords); ok {
				if entries, err := os.ReadDir(f.partitionDir("claims")); err != nil || len(entries) != 1 {
					t.Fatalf("partition holds %d entries (%v): a temp file was left", len(entries), err)
				}
			}
		})
	}
}
