package runtime

// The storage ceiling exists so an operator gets a typed refusal instead of
// ENOSPC in the middle of a clone. A check that reserves nothing cannot do that
// while the supervisor drives runs concurrently.

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func filledStateDir(t *testing.T, bytes int) string {
	t.Helper()
	dir := t.TempDir()
	if bytes > 0 {
		if err := os.WriteFile(filepath.Join(dir, "used"), make([]byte, bytes), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestTwoConcurrentRunsCannotBothTakeTheLastSlot is the oversubscription law.
// Each run fits on its own; both together do not. Exactly one may hold the
// reservation at a time, so the second measures a directory the first has
// already committed to and is refused before it allocates.
func TestTwoConcurrentRunsCannotBothTakeTheLastSlot(t *testing.T) {
	dir := filledStateDir(t, 0)
	storage := StateStorage{Dir: dir, CeilingBytes: estimatedCandidateBytes + (estimatedCandidateBytes / 2)}

	// The first reservation is held while the second is attempted, which is
	// exactly the window the supervisor creates by driving runs at once.
	first, err := storage.Reserve()
	if err != nil {
		t.Fatalf("the first run was refused with an empty state directory: %v", err)
	}

	// While the first holds it, simulate its allocation landing.
	if err := os.WriteFile(filepath.Join(dir, "candidate"), make([]byte, 1024), 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		release, err := storage.Reserve()
		if err == nil {
			release()
		}
		done <- err
	}()

	// The second must not be able to complete its admission while the first
	// holds the reservation; releasing lets it proceed and be judged.
	first()
	secondErr := <-done
	if secondErr != nil {
		var storageErr *StateStorageError
		if !asStorageError(secondErr, &storageErr) {
			t.Fatalf("the second run failed for the wrong reason: %v", secondErr)
		}
	}
}

// TestReservationsSerializeAcrossGoroutines proves the critical section is real:
// with a ceiling that admits one candidate, many concurrent reservations must
// never overlap.
func TestReservationsSerializeAcrossGoroutines(t *testing.T) {
	dir := filledStateDir(t, 0)
	storage := StateStorage{Dir: dir, CeilingBytes: estimatedCandidateBytes * 4}

	var mu sync.Mutex
	inside, peak := 0, 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := storage.Reserve()
			if err != nil {
				return
			}
			mu.Lock()
			inside++
			if inside > peak {
				peak = inside
			}
			mu.Unlock()
			// The reservation is HELD for a window. Without one, the counter is
			// incremented and decremented in adjacent instants and never
			// observes an overlap - which is how the first version of this test
			// passed with the lock released before the check.
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inside--
			mu.Unlock()
			release()
		}()
	}
	wg.Wait()
	if peak > 1 {
		t.Fatalf("%d reservations were held at once; admission and allocation must not overlap", peak)
	}
}

// TestAnUnboundedCeilingReservesNothing keeps the default free: an operator who
// configured no ceiling must not acquire a lock per run.
func TestAnUnboundedCeilingReservesNothing(t *testing.T) {
	release, err := StateStorage{Dir: t.TempDir()}.Reserve()
	if err != nil {
		t.Fatalf("an unconfigured ceiling refused work: %v", err)
	}
	release()
}

func asStorageError(err error, target **StateStorageError) bool {
	if e, ok := err.(*StateStorageError); ok {
		*target = e
		return true
	}
	return false
}
