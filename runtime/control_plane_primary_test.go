package runtime

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnlyOneControlPlanePrimaryPerStateDirectory(t *testing.T) {
	state := t.TempDir()
	first, err := AcquireControlPlanePrimary(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireControlPlanePrimary(state); !errors.Is(err, ErrControlPlanePrimaryHeld) {
		t.Fatalf("a second primary was not refused: %v", err)
	}
	// Another state directory is another claim.
	other, err := AcquireControlPlanePrimary(t.TempDir())
	if err != nil {
		t.Fatalf("an unrelated state directory was refused: %v", err)
	}
	_ = other.Release()
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	again, err := AcquireControlPlanePrimary(state)
	if err != nil {
		t.Fatalf("the claim was not released: %v", err)
	}
	_ = again.Release()
}

// Starting together must still yield exactly one primary: the claim is the
// atomic step, not a record read before it.
func TestConcurrentControlPlaneStartsYieldExactlyOnePrimary(t *testing.T) {
	state := t.TempDir()
	const starters = 16
	var winners atomic.Int32
	var held []*ControlPlanePrimary
	var mu sync.Mutex
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range starters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			primary, err := AcquireControlPlanePrimary(state)
			if err != nil {
				if !errors.Is(err, ErrControlPlanePrimaryHeld) {
					t.Errorf("unexpected refusal: %v", err)
				}
				return
			}
			winners.Add(1)
			mu.Lock()
			held = append(held, primary)
			mu.Unlock()
		}()
	}
	close(start)
	wg.Wait()
	for _, primary := range held {
		_ = primary.Release()
	}
	if winners.Load() != 1 {
		t.Fatalf("%d of %d simultaneous starts became primary, want exactly 1", winners.Load(), starters)
	}
}
