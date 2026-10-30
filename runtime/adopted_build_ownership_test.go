package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockstepDocker is ONE daemon shared by two builders. Containers are real as
// far as naming goes: a second `create` of a name that exists is refused with
// the daemon's conflict error, `rm` removes, and the first `start` of the whole
// run blocks until the test lets it go, so a build can be held exactly between
// `docker create` and `docker start`.
type lockstepDocker struct {
	mu           sync.Mutex
	created      map[string]bool
	creates      int
	firstCreated string // the name of the first container the daemon created
	starts       int
	removed      []string
	started      chan struct{} // closed when the first start is reached
	gate         chan struct{} // the first start waits for this
}

func newLockstepDocker() *lockstepDocker {
	return &lockstepDocker{created: map[string]bool{}, started: make(chan struct{}), gate: make(chan struct{})}
}

func (f *lockstepDocker) LookPath(string) error { return nil }

func (f *lockstepDocker) Run(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	f.mu.Lock()
	switch args[0] {
	case "create":
		name := ""
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		if f.created[name] {
			f.mu.Unlock()
			return CommandOutput{Stderr: []byte(createNameConflict)}, errors.New("exit status 1")
		}
		f.created[name] = true
		if f.creates == 0 {
			f.firstCreated = name
		}
		f.creates++
	case "start":
		f.starts++
		first := f.starts == 1
		f.mu.Unlock()
		if first {
			close(f.started)
			<-f.gate
		}
		return CommandOutput{}, nil
	case "wait":
		f.mu.Unlock()
		return CommandOutput{Stdout: []byte("0\n")}, nil
	case "rm":
		name := args[len(args)-1]
		f.removed = append(f.removed, name)
		delete(f.created, name)
	}
	f.mu.Unlock()
	return CommandOutput{}, nil
}

func (f *lockstepDocker) Output(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) >= 5 && args[len(args)-5] == "image" && args[len(args)-4] == "inspect" {
		return CommandOutput{Stdout: []byte(args[len(args)-1] + "\n")}, nil
	}
	if len(args) >= 4 && args[len(args)-4] == "inspect" && args[len(args)-3] == "--format" && args[len(args)-2] == "{{.State.Running}}" {
		if !f.created[args[len(args)-1]] {
			return CommandOutput{Stderr: []byte("Error: No such object: " + args[len(args)-1])}, errors.New("exit status 1")
		}
		return CommandOutput{Stdout: []byte("false\n")}, nil // created, not started: NOT running
	}
	return CommandOutput{Stdout: []byte("daemon-test-id\n")}, nil
}

func lockstepSpec(t *testing.T, docker *lockstepDocker, state string) AdoptedBuildSpec {
	t.Helper()
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "module.info"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	return AdoptedBuildSpec{
		SourceDir: t.TempDir(), Output: filepath.Join(t.TempDir(), "zenchron-engineering"),
		GOOS: "linux", GOARCH: "amd64", Kind: ControllerAdopted, Version: "v",
		Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
		Sandbox:  DockerSandbox{Image: "sha256:pinned", Executor: docker, StateDir: state},
		CacheDir: cache,
	}
}

// THE CREATE-BEFORE-START BOUNDARY. Build A has created its container and not
// yet started it, so the container exists and is NOT running - which a retry
// cleanup would read as abandoned. Build B (the updater racing a manual
// `controller build-adopted`, or the reverse) must report that A owns the
// revision and must not remove, or even create, anything. Without the claim B
// meets the name conflict, retries, "releases" A's container and A's build
// dies underneath it.
func TestASecondBuildOfARevisionNeverTouchesTheFirstsCreatedButNotStartedContainer(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := newLockstepDocker()
	state := t.TempDir() // the one operation directory both builders configure
	done := make(chan error, 1)
	go func() {
		_, err := runAdoptedBuild(context.Background(), lockstepSpec(t, docker, state))
		done <- err
	}()
	<-docker.started // A is held between create and start

	_, err := runAdoptedBuild(context.Background(), lockstepSpec(t, docker, state))
	if !errors.Is(err, ErrAdoptedBuildInProgress) {
		t.Fatalf("the second build was not told another build owns the revision: %v", err)
	}
	docker.mu.Lock()
	removed, creates := append([]string(nil), docker.removed...), docker.creates
	docker.mu.Unlock()
	if len(removed) != 0 {
		t.Fatalf("the first build's container was removed underneath it: %v", removed)
	}
	if creates != 1 {
		t.Fatalf("the second build created a container (%d creates in all); it must touch nothing", creates)
	}

	close(docker.gate)
	if err := <-done; err != nil {
		t.Fatalf("the first build did not finish after the second was refused: %v", err)
	}
}

// Ownership is per revision and ends with the build.
func TestTheBuildClaimIsPerRevisionAndEndsWithTheBuild(t *testing.T) {
	state := t.TempDir()
	first, err := claimAdoptedBuild(state, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := claimAdoptedBuild(state, strings.Repeat("a", 40)); !errors.Is(err, ErrAdoptedBuildInProgress) {
		t.Fatalf("the same revision was claimed twice: %v", err)
	}
	other, err := claimAdoptedBuild(state, strings.Repeat("c", 40))
	if err != nil {
		t.Fatalf("an unrelated revision was blocked: %v", err)
	}
	other.Release()
	first.Release()
	again, err := claimAdoptedBuild(state, strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("a released claim could not be taken again: %v", err)
	}
	again.Release()
}

// A daemon that is not ready has created nothing: the retry backs off and
// tries again with NO cleanup, and a recovered daemon produces the build.
func TestADaemonThatIsNotReadyIsRetriedWithoutAnyCleanupAndRecovery(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{infoFailures: 2}
	if _, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker)); err != nil {
		t.Fatalf("a daemon that recovered within the bound refused the build: %v", err)
	}
	if docker.infoCalls < 3 {
		t.Fatalf("readiness was probed %d times, want the retries to have happened", docker.infoCalls)
	}
	// Two containers ran (toolchain, build) and each is removed once after it
	// ran. Any removal beyond that would be a cleanup of something never created.
	if len(docker.removed) != 2 || docker.creates != 2 {
		t.Fatalf("an unready daemon caused cleanup or extra containers: removed=%v creates=%d", docker.removed, docker.creates)
	}
}

func TestADaemonThatNeverBecomesReadySettlesBoundedWithAClearRefusal(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{infoFailures: 1000}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no step account: %v", err)
	}
	if want := len(adoptedBuildBackoff) + 1; failure.Attempts != want || docker.infoCalls != want {
		t.Fatalf("attempts = %d, readiness probes = %d, want exactly %d", failure.Attempts, docker.infoCalls, want)
	}
	if failure.Step != "toolchain" || !failure.Transient || !errors.Is(err, ErrSandboxUnavailable) {
		t.Fatalf("the refusal does not say a ready daemon was never reached: %+v / %v", failure, err)
	}
	if docker.creates != 0 || len(docker.removed) != 0 {
		t.Fatalf("something was created or removed for a daemon that never answered: creates=%d removed=%v", docker.creates, docker.removed)
	}
}

// Container state AMBIGUOUS (the daemon cannot give its identity right after a
// failed create): the step is not run again on a guess. The RECONCILIATION is
// what is retried, inside the same bound, and once it succeeds the step runs.
func TestAmbiguousContainerStateRetriesTheReconciliationBeforeTheStepRunsAgain(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1, createStderr: createNameConflict, identityFailures: 1}
	if _, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker)); err != nil {
		t.Fatalf("a reconciliation that recovered within the bound refused the build: %v", err)
	}
	// Toolchain create (conflict), reconcile (fails once, not a create), reconcile
	// (ok, removes), toolchain create (ok), build create.
	// Removals: the leftover (once, by the reconciliation), then the toolchain
	// container and the build container after they ran.
	if docker.creates != 3 || len(docker.removed) != 3 {
		t.Fatalf("creates = %d, removed = %v; the step must wait for the reconciliation", docker.creates, docker.removed)
	}
}

func TestContainerStateThatStaysAmbiguousRefusesInsteadOfGuessing(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1, createStderr: createNameConflict, identityFailures: 1000}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	var failure *AdoptedBuildFailure
	if !errors.As(err, &failure) {
		t.Fatalf("no step account: %v", err)
	}
	if want := len(adoptedBuildBackoff) + 1; failure.Attempts != want {
		t.Fatalf("attempts = %d, want the bound %d", failure.Attempts, want)
	}
	if !strings.Contains(err.Error(), "could not be reconciled") {
		t.Fatalf("the refusal does not say why the step never ran again: %v", err)
	}
	if docker.creates != 1 || len(docker.removed) != 0 {
		t.Fatalf("the step ran again, or something was removed, on ambiguous state: creates=%d removed=%v", docker.creates, docker.removed)
	}
}

// `docker inspect` and `docker rm` keep their own output, sanitised and
// bounded by the same path as `docker create`, never a bare "exit status 1".
func TestInspectAndRemoveFailuresKeepTheirOwnSanitisedStderr(t *testing.T) {
	hostile := strings.Repeat("A", 50000) + " \x1b[31mred\x1b[0m\x1b]0;pwned\x07 authorization: bearer s3cr3t-token-value the daemon's last sentence"
	docker := &retryScriptedDocker{inspectFails: true, inspectStderr: hostile, rmFails: true, rmStderr: hostile}
	sandbox := DockerSandbox{Image: "sha256:pinned", Executor: docker, StateDir: t.TempDir(), OperationID: "adopted-build-x"}

	_, _, inspectErr := sandbox.inspectRunning("zenchron-0123456789ab")
	removeErr := sandbox.removeExact("zenchron-0123456789ab")
	for name, err := range map[string]error{"inspect": inspectErr, "remove": removeErr} {
		var step *SandboxStepError
		if !errors.As(err, &step) || step.Step != name || step.OperationID != "adopted-build-x" || step.Container != "zenchron-0123456789ab" {
			t.Fatalf("%s failure is not a typed step error naming its operation and container: %v", name, err)
		}
		if !strings.Contains(step.Stderr, "the daemon's last sentence") || len(step.Stderr) > sandboxStderrTailBytes {
			t.Fatalf("%s stderr is not the bounded tail: len=%d %q", name, len(step.Stderr), step.Stderr)
		}
		for _, bad := range []string{"\x1b", "\x07", "s3cr3t-token-value"} {
			if strings.Contains(err.Error(), bad) {
				t.Fatalf("%s refusal carries %q", name, bad)
			}
		}
	}
}

// ... and a reconciliation that keeps failing says WHY in the refusal.
func TestAFailingRemoveDuringReconciliationIsNamedInTheRefusal(t *testing.T) {
	noAdoptedBuildBackoff(t)
	docker := &retryScriptedDocker{failCreates: 1, createStderr: createNameConflict, rmFails: true, rmStderr: "Error response from daemon: removal of container is already in progress"}
	_, err := runAdoptedBuild(context.Background(), retryScriptedSpec(t, docker))
	if err == nil || !strings.Contains(err.Error(), "docker remove failed") || !strings.Contains(err.Error(), "removal of container is already in progress") {
		t.Fatalf("a failing docker rm surfaced as a bare error: %v", err)
	}
	if docker.creates != 1 {
		t.Fatalf("the step ran again over an unremoved container: creates=%d", docker.creates)
	}
}
