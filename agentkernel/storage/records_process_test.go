//go:build !windows

package storage

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const claimRootEnv = "AGENTKERNEL_TEST_CLAIM_ROOT"

// Exit codes the claim helper process reports back to its parent.
const (
	claimCreated = 0
	claimFailed  = 1
	claimTaken   = 3
)

// TestPutIfAbsentAcrossProcesses: separate OS processes, each with its own
// FileRecords handle on one root, race to claim one key. Exactly one may
// create it; every other must see ErrExists. This is the cross-process
// guarantee execution admission rests on.
func TestPutIfAbsentAcrossProcesses(t *testing.T) {
	root := filepath.Join(t.TempDir(), "records")
	if _, err := OpenFileRecords(root); err != nil {
		t.Fatal(err)
	}
	gate := filepath.Join(t.TempDir(), "go")
	const n = 8
	cmds := make([]*exec.Cmd, n)
	for i := range cmds {
		cmd := exec.Command(os.Args[0], "-test.run=^TestClaimHelperProcess$")
		cmd.Env = append(os.Environ(), claimRootEnv+"="+root, "AGENTKERNEL_TEST_CLAIM_GATE="+gate)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds[i] = cmd
	}
	// Release every contender at once so their claims overlap.
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	created := 0
	for _, cmd := range cmds {
		err := cmd.Wait()
		var exit *exec.ExitError
		switch {
		case err == nil:
			created++
		case errors.As(err, &exit) && exit.ExitCode() == claimTaken:
		default:
			t.Fatalf("claim helper failed: %v", err)
		}
	}
	if created != 1 {
		t.Fatalf("%d processes created the claim, want exactly 1", created)
	}
}

// TestClaimHelperProcess is the contender body run in a child process; it is
// a no-op in the normal test run.
func TestClaimHelperProcess(t *testing.T) {
	root := os.Getenv(claimRootEnv)
	if root == "" {
		return
	}
	gate := os.Getenv("AGENTKERNEL_TEST_CLAIM_GATE")
	for deadline := time.Now().Add(10 * time.Second); ; {
		if _, err := os.Stat(gate); err == nil {
			break
		}
		if time.Now().After(deadline) {
			os.Exit(claimFailed)
		}
		time.Sleep(time.Millisecond) // polling a gate file, not ordering a race
	}
	s, err := OpenFileRecords(root)
	if err != nil {
		os.Exit(claimFailed)
	}
	err = s.PutIfAbsent(context.Background(), "claims", "exec-1", []byte(os.Getenv("PPID")))
	switch {
	case err == nil:
		os.Exit(claimCreated)
	case errors.Is(err, ErrExists):
		os.Exit(claimTaken)
	default:
		os.Exit(claimFailed)
	}
}
