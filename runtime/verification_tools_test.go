package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Each process owns a real lock; the Docker client alone is faked at I/O.
type controlledVerificationDocker struct {
	fakeCommandExecutor
	started chan struct{}
	release chan struct{}
}

func (e *controlledVerificationDocker) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	if len(args) > 0 && args[0] == "start" {
		close(e.started)
		select {
		case <-e.release:
		case <-ctx.Done():
			return CommandOutput{}, ctx.Err()
		}
	}
	return e.fakeCommandExecutor.Run(ctx, name, args, dir, env, grace)
}

func TestVerificationBrokerHelper(t *testing.T) {
	if os.Getenv("ZENCHRON_TEST_VERIFICATION") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg != "--" {
			continue
		}
		out, err := RunVerificationTool(context.Background(), os.Args[i+1], os.Args[i+2], os.Args[i+3:])
		_, _ = os.Stdout.Write(out.Stdout)
		_, _ = os.Stderr.Write(out.Stderr)
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func toolFixture(t *testing.T, ceiling int) (string, *SQLiteOperationStore, Scheduler) {
	t.Helper()
	dir, store := openJournal(t)
	s := verificationScheduler(store, NewRuntimeOwner(), 10, ceiling)
	s.Clock = RealClock{}
	s.Liveness = NewLockOwnerLiveness(dir)
	lock, err := AcquireControllerInstanceLock(dir, s.Owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := lock.Release(); err != nil {
			t.Error(err)
		}
	})
	return dir, store, s
}

func awaitVerificationWait(t *testing.T, store *SQLiteOperationStore, run string) {
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
		for _, p := range permits {
			if p.Parent.RunID == run && p.State == VerificationWaiting {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatal("tool never requested capacity")
		case <-tick.C:
		}
	}
}

func TestNestedVerificationNativeShimAndCandidateRunShareCapacity(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	protected := nestedParent(t, store, s, "protected")
	native := nestedParent(t, store, s, "native")
	ctx := withVerificationExecution(context.Background(), s, ExecutionAttemptRef{RunID: protected.RunID, OperationID: protected.ID, Attempt: protected.AttemptIdentity}, dir)
	client := &controlledVerificationDocker{fakeCommandExecutor: fakeCommandExecutor{found: true}, started: make(chan struct{}), release: make(chan struct{})}
	broker := ToolBroker{CandidateDir: t.TempDir(), Sandbox: DockerSandbox{Image: "sha256:test", StateDir: dir, Executor: client, Grace: time.Millisecond}}
	finished := make(chan error, 1)
	go func() { _, err := broker.RunCommand(ctx, []string{"probe"}); finished <- err }()
	select {
	case <-client.started:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate.run did not begin")
	}
	t.Cleanup(func() {
		select {
		case <-client.release:
		default:
			close(client.release)
		}
	})
	targetDir := t.TempDir()
	marker := filepath.Join(targetDir, "executed")
	if err := os.WriteFile(filepath.Join(targetDir, "probe"), []byte("#!/bin/sh\nprintf native\nprintf ran >> "+shellSingleQuoted(marker)+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	v := verificationExecution{s, ExecutionAttemptRef{RunID: native.RunID, OperationID: native.ID, Attempt: native.AttemptIdentity}, dir}
	shim, err := prepareVerificationTools(v, []string{"probe"}, []string{os.Args[0], "-test.run=^TestVerificationBrokerHelper$", "--"}, targetDir+string(os.PathListSeparator)+os.Getenv("PATH"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var grant verificationToolGrant
	if err := readVerificationMessage(filepath.Join(filepath.Dir(shim), "grant.json"), &grant); err != nil {
		t.Fatal(err)
	}
	_, stop, err := serveVerificationTools(context.Background(), v, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	env := append(os.Environ(), "ZENCHRON_TEST_VERIFICATION=1", "PATH="+shim+string(os.PathListSeparator)+os.Getenv("PATH"))
	waiting, cancel := context.WithCancel(context.Background())
	nativeDone := make(chan error, 1)
	go func() {
		_, err := (OSCommandExecutor{}).Run(waiting, "/bin/sh", []string{"-c", "probe"}, targetDir, env, time.Millisecond)
		nativeDone <- err
	}()
	awaitVerificationWait(t, store, native.RunID)
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("native tool bypassed the candidate.run grant")
	}
	current, _, _, err := store.Operation(native.ID)
	if err != nil || current.Attempt != native.Attempt || current.AttemptIdentity != native.AttemptIdentity || current.State != Running {
		t.Fatal("capacity wait changed the provider attempt")
	}
	cancel()
	select {
	case err := <-nativeDone:
		if err == nil {
			t.Fatal("cancelled waiter succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter did not stop")
	}
	close(client.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	// The two native-CLI-shaped calls are sequential tool executions under the
	// same physical parent, not another provider invocation or work slot.
	for range 2 {
		out, err := (OSCommandExecutor{}).Run(context.Background(), "/bin/sh", []string{"-c", "probe"}, targetDir, env, time.Millisecond)
		if err != nil || string(out.Stdout) != "native" {
			t.Fatalf("native shim: %q / %v", out.Stdout, err)
		}
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "ranran" {
		t.Fatalf("executions %q / %v", data, err)
	}
	permits, err := store.VerificationPermits()
	if err != nil {
		t.Fatal(err)
	}
	// A killed waiting broker may require death+expiry reclamation, but it
	// never owns capacity. Every completed actual tool must release its grant.
	for _, p := range permits {
		if p.State == VerificationGranted {
			t.Fatal("completed tool leaked verification capacity")
		}
	}
	if saturated, err := s.VerificationSaturated("other"); saturated || err != nil {
		t.Fatalf("capacity remained occupied: %v/%v", saturated, err)
	}
}

func TestNestedVerificationToolFailureIsAnObservationAndReleasesCapacity(t *testing.T) {
	dir, store, s := toolFixture(t, 1)
	parent := nestedParent(t, store, s, "producer")
	v := verificationExecution{s, ExecutionAttemptRef{RunID: parent.RunID, OperationID: parent.ID, Attempt: parent.AttemptIdentity}, dir}
	target := t.TempDir()
	if err := os.WriteFile(filepath.Join(target, "test-tool"), []byte("#!/bin/sh\nprintf failing-test\nexit 7\n"), 0700); err != nil {
		t.Fatal(err)
	}
	shim, err := prepareVerificationTools(v, []string{"test-tool"}, []string{os.Args[0]}, target, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var grant verificationToolGrant
	if err := readVerificationMessage(filepath.Join(filepath.Dir(shim), "grant.json"), &grant); err != nil {
		t.Fatal(err)
	}
	_, stop, err := serveVerificationTools(context.Background(), v, grant)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := stop(); err != nil {
			t.Error(err)
		}
	})
	base, _ := GitGuardDir(dir, v.Parent)
	out, err := RunVerificationTool(context.Background(), filepath.Join(base, "verification", "grant.json"), "test-tool", nil)
	if err == nil || out.ExitCode != 7 || !strings.Contains(string(out.Stdout), "failing-test") {
		t.Fatalf("test verdict changed: %+v / %v", out, err)
	}
	op, _, _, readErr := store.Operation(parent.ID)
	if readErr != nil || op.State != Running || op.Attempt != parent.Attempt {
		t.Fatal("worker observation became a parent failure")
	}
	if saturated, err := s.VerificationSaturated("other"); saturated || err != nil {
		t.Fatal(errors.New("failed test retained capacity"))
	}
}
