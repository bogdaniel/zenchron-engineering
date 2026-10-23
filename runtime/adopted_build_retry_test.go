package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// retryScriptedDocker is the Docker daemon boundary for the #674 tests: it fails the
// first failCreates `docker create` calls the way a real daemon does (exit 1,
// the reason only on stderr), can fail the compile step as a workload that ran,
// and records which containers the runtime removed.
type retryScriptedDocker struct {
	mu           sync.Mutex
	creates      int
	failCreates  int
	createStderr string
	running      bool // `docker inspect` says the leftover container is running
	compileFails bool
	starts       int
	workloadFail bool
	removed      []string
}

func (f *retryScriptedDocker) LookPath(string) error { return nil }

func (f *retryScriptedDocker) Run(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch args[0] {
	case "create":
		f.creates++
		if f.creates <= f.failCreates {
			return CommandOutput{Stderr: []byte(f.createStderr)}, errors.New("exit status 1")
		}
	case "start":
		f.starts++
		// Starts: the toolchain probe, then the build.
		if f.compileFails && f.starts == 2 {
			f.workloadFail = true
			return CommandOutput{Stderr: []byte("./cmd/zenchron-engineering/main.go:12:2: undefined: nope\n")}, errors.New("exit status 1")
		}
	case "wait":
		if f.workloadFail {
			return CommandOutput{Stdout: []byte("1\n")}, nil
		}
		return CommandOutput{Stdout: []byte("0\n")}, nil
	case "rm":
		f.removed = append(f.removed, args[len(args)-1])
	}
	return CommandOutput{}, nil
}

func (f *retryScriptedDocker) Output(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) >= 5 && args[len(args)-5] == "image" && args[len(args)-4] == "inspect" {
		return CommandOutput{Stdout: []byte(args[len(args)-1] + "\n")}, nil
	}
	if len(args) >= 4 && args[len(args)-4] == "inspect" && args[len(args)-3] == "--format" && args[len(args)-2] == "{{.State.Running}}" {
		return CommandOutput{Stdout: []byte(fmt.Sprintf("%v\n", f.running))}, nil
	}
	return CommandOutput{Stdout: []byte("daemon-test-id\n")}, nil
}

func retryScriptedSpec(t *testing.T, docker *retryScriptedDocker) AdoptedBuildSpec {
	t.Helper()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "module.info"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	return AdoptedBuildSpec{
		SourceDir: t.TempDir(), Output: filepath.Join(t.TempDir(), "zenchron-engineering"),
		GOOS: "linux", GOARCH: "amd64", Kind: ControllerAdopted, Version: "v",
		Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
		Sandbox:  DockerSandbox{Image: "sha256:pinned", Executor: docker, StateDir: t.TempDir()},
		CacheDir: cache,
	}
}

// noAdoptedBuildBackoff keeps the retry tests instant without changing how many retries
// the production schedule allows.
func noAdoptedBuildBackoff(t *testing.T) {
	t.Helper()
	saved := adoptedBuildBackoff
	adoptedBuildBackoff = make([]time.Duration, len(saved))
	t.Cleanup(func() { adoptedBuildBackoff = saved })
}

const createNameConflict = `Error response from daemon: Conflict. The container name "/zenchron-0123456789ab" is already in use by container "395fb605". You have to remove (or rename) that container to be able to reuse that name.`

func TestATransientCreateFailureIsRetriedAfterReleasingTheLeftoverContainer(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1, createStderr: createNameConflict}
	if _, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker)); err != nil {
		t.Fatalf("a single transient create failure refused the build: %v", err)
	}
	// Toolchain create (fails), toolchain create (retried), build create.
	if docker.creates != 3 {
		t.Fatalf("creates = %d, want 3 (one failed, one retried, one for the build)", docker.creates)
	}
	// The retry removed exactly the leftover the failed create named, and
	// nothing else: it is the deterministic per-operation name.
	if len(docker.removed) < 1 || !strings.HasPrefix(docker.removed[0], "zenchron-") {
		t.Fatalf("the leftover container was not released before the retry: %v", docker.removed)
	}
}

func TestADeterministicCompileFailureIsNeverRetriedAndSurfacesItsStepAndStderr(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{compileFails: true}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		t.Fatalf("the refusal carries no step account: %v", err)
	}
	if failure.Step != "build" || failure.Attempts != 1 || failure.Transient {
		t.Fatalf("a compile error was treated as retryable: %+v", failure)
	}
	if !strings.Contains(failure.Error(), "undefined: nope") || !strings.Contains(failure.OperationID, "adopted-build-") {
		t.Fatalf("the refusal lost the compiler's account or the operation: %v", failure)
	}
	// Toolchain probe + one build container: no retry of the compile.
	if docker.creates != 2 {
		t.Fatalf("a deterministic failure created %d containers, want 2", docker.creates)
	}
}

func TestInfrastructureRetriesAreBoundedAndTheRefusalNamesTheDockerStep(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1000, createStderr: createNameConflict}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no step account: %v", err)
	}
	if want := len(adoptedBuildBackoff) + 1; failure.Attempts != want || docker.creates != want {
		t.Fatalf("attempts = %d, creates = %d, want exactly %d", failure.Attempts, docker.creates, want)
	}
	if !failure.Transient || failure.Step != "toolchain" {
		t.Fatalf("a spent infrastructure retry is not described as such: %+v", failure)
	}
	var step *SandboxStepError
	if !errors.As(err, &step) || step.Step != "create" || step.Container == "" {
		t.Fatalf("the failing Docker step and container were dropped: %v", err)
	}
	for _, want := range []string{"docker create failed", "adopted-build-toolchain-", "Conflict", "already in use"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal omits %q: %v", want, err)
		}
	}
}

func TestARetryNeverRemovesAContainerThatIsStillRunning(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1000, createStderr: createNameConflict, running: true}
	if _, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker)); err == nil {
		t.Fatal("the build succeeded while its container name was held")
	}
	if len(docker.removed) != 0 {
		t.Fatalf("a live attempt's container was removed: %v", docker.removed)
	}
}

func TestACancelledBuildIsNotRetried(t *testing.T) {
	noAdoptedBuildBackoff(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	docker := &retryScriptedDocker{failCreates: 1000, createStderr: createNameConflict}
	_, err := runAdoptedBuild(ctx, retryScriptedSpec(t, docker))
	if err == nil {
		t.Fatal("a cancelled build succeeded")
	}
	if docker.creates > 1 {
		t.Fatalf("a cancelled build was retried: %d creates", docker.creates)
	}
}

func TestHostileDockerStderrIsSanitisedRedactedAndBounded(t *testing.T) {
	noAdoptedBuildBackoff(t)
	// Control sequences and a secret sit in the TAIL (what survives the cut),
	// and a run of tokens the redactor replaces with a LONGER marker precedes
	// them, so a bound applied before redaction would push the last sentence out.
	hostile := strings.Repeat("A", 50000) + strings.Repeat(" ghp_a", 300) +
		" \x1b[31mred\x1b[0m\x1b]0;pwned\x07\x00 authorization: bearer s3cr3t-token-value ghp_ABCDEF1234567890 the daemon's last sentence"
	docker := &retryScriptedDocker{failCreates: 1000, createStderr: hostile}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no step account: %v", err)
	}
	text := failure.Error()
	for _, bad := range []string{"\x1b", "\x07", "\x00", "s3cr3t-token-value", "ghp_ABCDEF"} {
		if strings.Contains(text, bad) {
			t.Fatalf("the refusal carries %q", bad)
		}
	}
	if len(failure.Stderr) > sandboxStderrTailBytes {
		t.Fatalf("stderr tail is %d bytes, bound is %d", len(failure.Stderr), sandboxStderrTailBytes)
	}
	if !strings.Contains(failure.Stderr, "the daemon's last sentence") {
		t.Fatalf("the tail lost the daemon's last sentence: %q", failure.Stderr)
	}
}

func TestOnlyDockerLifecycleFailuresAreEligibleForRetry(t *testing.T) {
	step := &SandboxStepError{Step: "create", Err: errors.New("exit status 1")}
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"docker create failed":    {step, true},
		"daemon unavailable":      {fmt.Errorf("probe: %w", ErrSandboxUnavailable), true},
		"workload exited nonzero": {&containerExitError{Code: 1, err: errors.New("exit status 1")}, false},
		"cancelled":               {fmt.Errorf("x: %w", context.Canceled), false},
		"deadline":                {fmt.Errorf("x: %w", context.DeadlineExceeded), false},
		"plain error":             {errors.New("the trusted module cache is empty"), false},
		"nil":                     {nil, false},
	} {
		if got := transientSandboxFailure(tc.err); got != tc.want {
			t.Errorf("%s: transient = %v, want %v", name, got, tc.want)
		}
	}
}
