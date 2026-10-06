package runtime

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestNestedVerificationDoesNotTakeAnotherWorkSlot(t *testing.T) {
	_, store := openJournal(t)
	clock := &fakeClock{now: time.Unix(100, 0).UTC()}
	s := verificationScheduler(store, "controller", 10, 2)
	s.Clock = clock
	parents := make([]RunOperation, 10)
	for i := range parents {
		run := fmt.Sprintf("parent-%d", i)
		if err := store.PutRun(newJournalRun(run)); err != nil {
			t.Fatal(err)
		}
		planKind(t, s, run, OpExecutionInvoke)
		leased := mustNext(t, s, run)
		if leased == nil {
			t.Fatal("ten parents could not hold ten work slots")
		}
		var err error
		parents[i], err = s.Start(leased.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var held []VerificationPermit
	var requests []VerificationPermit
	for i, parent := range parents {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.RequestVerification(ExecutionAttemptRef{parent.RunID, parent.ID, parent.AttemptIdentity}, fmt.Sprintf("tool-%d", i), "tool-owner")
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			requests = append(requests, p)
			mu.Unlock()
			if acquired, err := s.AcquireVerification(p); err != nil {
				t.Error(err)
			} else if acquired {
				mu.Lock()
				held = append(held, p)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if len(held) != 2 {
		t.Fatalf("%d tools acquired capacity, want two while ten parents retain their slots", len(held))
	}
	if saturated, err := s.VerificationSaturated("another-run"); err != nil || !saturated {
		t.Fatalf("nested verification occupancy is invisible: %v / %v", saturated, err)
	}
	for _, parent := range parents {
		current, _, found, err := store.Operation(parent.ID)
		if err != nil || !found || current.State != Running || current.AttemptIdentity != parent.AttemptIdentity || current.Attempt != parent.Attempt {
			t.Fatalf("tool admission changed its parent: %+v / %v", current, err)
		}
	}
	if err := s.ReleaseVerification(held[0]); err == nil {
		t.Fatal("released a live tool without proof of death")
	}
	deadTool := s
	deadTool.Liveness = neverAlive()
	if err := deadTool.ReleaseVerification(held[0]); err != nil {
		t.Fatal(err)
	}
	admitted := 0
	for _, p := range requests {
		current, _, _, err := store.VerificationPermit(p.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State != VerificationWaiting {
			continue
		}
		ok, err := s.AcquireVerification(p)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("releasing one tool admitted %d, want one", admitted)
	}
	for _, p := range held[1:] {
		if ok, err := s.AcquireVerification(p); err == nil || ok {
			t.Fatal("same physical tool executed twice")
		}
	}
}
