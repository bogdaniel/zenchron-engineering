package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNestedVerificationTenNativeParentsExecuteOnlyTwoTools(t *testing.T) {
	dir, store, s := toolFixture(t, 2)
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "probe"), []byte("#!/bin/sh\nprintf started > \"$1\"\nexec cat \"$2\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	finished := make(chan error, 10)
	markers := make([]string, 10)
	fifos := make([]string, 10)
	for i := range 10 {
		parent := nestedParent(t, store, s, fmt.Sprintf("native-%d", i))
		v := verificationExecution{s, ExecutionAttemptRef{parent.RunID, parent.ID, parent.AttemptIdentity}, dir}
		descriptor, _, stop := nativeVerificationFixture(t, v, target, context.Background())
		t.Cleanup(func() {
			if err := stop(); err != nil {
				t.Error(err)
			}
		})
		markers[i] = filepath.Join(target, fmt.Sprintf("started-%d", i))
		fifos[i] = filepath.Join(target, fmt.Sprintf("block-%d", i))
		if _, err := (OSCommandExecutor{}).Run(ctx, "mkfifo", []string{fifos[i]}, target, os.Environ(), time.Second); err != nil {
			t.Fatal(err)
		}
		go func() {
			_, err := RunVerificationTool(ctx, descriptor, "probe", []string{markers[i], fifos[i]})
			finished <- err
		}()
	}
	// Cancel and join clients before stopping their controller transports.
	joined := 0
	t.Cleanup(func() {
		cancel()
		for joined < 10 {
			select {
			case <-finished:
				joined++
			case <-time.After(6 * time.Second):
				t.Error("native client failed to stop")
				return
			}
		}
	})
	started := func() []int {
		var indices []int
		for i, marker := range markers {
			if _, err := os.Stat(marker); err == nil {
				indices = append(indices, i)
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
		}
		return indices
	}
	await := func(everStarted, waiting int) {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			permits, err := store.VerificationPermits()
			if err != nil {
				t.Fatal(err)
			}
			held, pending := 0, 0
			for _, p := range permits {
				if p.State == VerificationGranted {
					held++
				}
				if p.State == VerificationWaiting {
					pending++
				}
			}
			if held > 2 || len(started()) > everStarted {
				t.Fatal("native execution exceeded the verification ceiling")
			}
			if held == 2 && pending == waiting && len(started()) == everStarted {
				return
			}
			select {
			case <-deadline.C:
				t.Fatalf("tool fleet did not reach %d started/%d waiting: %+v", everStarted, waiting, permits)
			case <-tick.C:
			}
		}
	}
	await(2, 8)
	fleet, err := FleetStatus(store, dir, 10, 2, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fleet.Working != 10 || fleet.Verifying != 2 || fleet.AwaitingVerification != 8 {
		t.Fatalf("untruthful native fleet: %+v", fleet)
	}
	first := started()[0]
	writer, err := os.OpenFile(fifos[first], os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("done\n")); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		joined++
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("released native tool did not finish")
	}
	await(3, 7)
	for i := range 10 {
		ops, err := store.Operations(fmt.Sprintf("native-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if len(ops) != 1 || ops[0].State != Running || ops[0].Attempt != 1 || ops[0].AttemptIdentity != 1 {
			t.Fatalf("tool wait consumed work or another attempt: %+v", ops)
		}
	}
	cancel()
	for joined < 10 {
		select {
		case <-finished:
			joined++
		case <-time.After(6 * time.Second):
			t.Fatal("native client failed to stop")
		}
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		t.Fatal(err)
	}
	if len(permits) != 0 {
		t.Fatalf("cancelled fleet leaked tool requests: %+v", permits)
	}
}
