package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

type recordedCommand struct {
	name      string
	args, env []string
	dir       string
}
type fakeCommandExecutor struct {
	calls   []recordedCommand
	err     error
	outputs []CommandOutput
	found   bool
	// runningInspects counts down how many "{{.State.Running}}" inspects report
	// true before the container is modeled as stopped.
	runningInspects int
	// removed models containers that `docker rm` already deleted, so a later
	// inspect reports absence the way the real daemon does.
	removed map[string]bool
	// codexHelp is the help text the modeled Codex CLI advertises. It is what
	// the provider capability probe reads, so a fixture missing a required flag
	// models a CLI whose sandbox capability cannot be established.
	codexHelp string
}

func (f *fakeCommandExecutor) LookPath(string) error {
	if f.found {
		return nil
	}
	return errors.New("missing")
}
func (f *fakeCommandExecutor) Run(_ context.Context, name string, args []string, dir string, env []string, _ time.Duration) (CommandOutput, error) {
	f.calls = append(f.calls, recordedCommand{
		name: name,
		args: append([]string(nil), args...),
		dir:  dir,
		env:  append([]string(nil), env...),
	})
	if len(args) >= 2 && args[len(args)-2] == "rm" {
		if f.removed == nil {
			f.removed = map[string]bool{}
		}
		f.removed[args[len(args)-1]] = true
	}
	if len(f.outputs) > 0 {
		out := f.outputs[0]
		f.outputs = f.outputs[1:]
		return out, f.err
	}
	return CommandOutput{}, f.err
}
func (f *fakeCommandExecutor) Output(_ context.Context, name string, args []string, dir string, env []string, _ time.Duration) (CommandOutput, error) {
	f.calls = append(f.calls, recordedCommand{name: name, args: append([]string(nil), args...), dir: dir, env: append([]string(nil), env...)})
	if name == "codex" {
		return CommandOutput{Stdout: []byte(f.codexHelp)}, nil
	}
	if len(args) >= 3 && args[len(args)-3] == "info" && args[len(args)-2] == "--format" && args[len(args)-1] == "{{.ID}}" {
		return CommandOutput{Stdout: []byte("daemon-test-id\n")}, nil
	}
	if len(args) >= 5 && args[len(args)-5] == "image" && args[len(args)-4] == "inspect" {
		return CommandOutput{Stdout: []byte(args[len(args)-1] + "\n")}, nil
	}
	if len(args) >= 4 && args[len(args)-4] == "inspect" && args[len(args)-3] == "--format" && args[len(args)-2] == "{{.State.Running}}" && f.removed[args[len(args)-1]] {
		return CommandOutput{Stderr: []byte("Error: No such object: " + args[len(args)-1])}, errors.New("no such object")
	}
	if len(args) >= 4 && args[len(args)-4] == "inspect" && args[len(args)-3] == "--format" && args[len(args)-2] == "{{.State.Running}}" && f.runningInspects > 0 {
		f.runningInspects--
		return CommandOutput{Stdout: []byte("true\n")}, nil
	}
	// Unit command-shape tests model a created-but-not-running container. Real
	// lifecycle behavior is covered by the opt-in Docker test below.
	return CommandOutput{Stdout: []byte("false\n")}, nil
}
func commandText(calls []recordedCommand) string {
	var all []string
	for _, c := range calls {
		all = append(all, strings.Join(c.args, " "))
	}
	return strings.Join(all, "\n")
}

func hasDockerInvocation(calls []recordedCommand, args ...string) bool {
	for _, call := range calls {
		if call.name == "docker" && len(call.args) == len(args) {
			matched := true
			for i := range args {
				if call.args[i] != args[i] {
					matched = false
					break
				}
			}
			if matched {
				return true
			}
		}
	}
	return false
}

func hasDockerPrefix(calls []recordedCommand, args ...string) bool {
	for _, call := range calls {
		if call.name != "docker" || len(call.args) < len(args) {
			continue
		}
		matched := true
		for i := range args {
			if call.args[i] != args[i] {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func TestDockerSandboxBindsTrustedEndpointToEveryControlPlaneCall(t *testing.T) {
	root := t.TempDir()
	fake := &fakeCommandExecutor{found: true}
	s := DockerSandbox{Image: "sha256:image", Endpoint: DockerEndpoint{Host: "tcp://dind:2375"}, Executor: fake, OperationID: "operation", StateDir: root}
	args := dockerBase(root, false)
	args = append(args, "sha256:image", "sh", "-ec", "true")
	if _, err := s.run(context.Background(), args); err != nil {
		t.Fatal(err)
	}
	for _, call := range fake.calls {
		if call.name != "docker" || len(call.args) < 3 || call.args[0] != "--host" || call.args[1] != "tcp://dind:2375" {
			t.Fatalf("control-plane command is not bound to trusted endpoint: %#v", call)
		}
		for _, value := range call.env {
			if strings.HasPrefix(value, "DOCKER_") {
				t.Fatalf("control-plane endpoint leaked through command environment: %#v", call)
			}
		}
	}
}

func TestDockerSandboxRejectsAmbiguousTrustedEndpoint(t *testing.T) {
	s := DockerSandbox{Image: "sha256:image", Endpoint: DockerEndpoint{Host: "tcp://user:secret@dind:2375"}, Executor: &fakeCommandExecutor{found: true}}
	if !errors.Is(s.ready(), ErrSandboxUnavailable) {
		t.Fatal("credential-bearing endpoint was accepted")
	}
}

func TestArtifactReadErrorIsNotRecordedAsValidDigest(t *testing.T) {
	got, err := artifact(filepath.Join(t.TempDir(), "missing.log"), false, false)
	if err == nil {
		t.Fatalf("unreadable artifact was recorded as a valid digest: %#v", got)
	}
	if got.SHA256 != "" {
		t.Fatalf("unreadable artifact produced a digest: %#v", got)
	}
}

func TestAssuranceCheckoutBindsExactTree(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin")
	if _, err := runGit("", "init", origin); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "test"}, {"config", "user.email", "test@example.invalid"}} {
		if _, err := runGit(origin, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(origin, "a.go"), []byte("package a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "add", "."); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(origin, "commit", "-m", "base"); err != nil {
		t.Fatal(err)
	}
	commit, _ := gitOutput(origin, "rev-parse", "HEAD")
	tree, _ := gitOutput(origin, "rev-parse", "HEAD^{tree}")
	checkout := filepath.Join(root, "checkout")
	if err := CreateAssuranceCheckout(origin, checkout, strings.TrimSpace(commit), strings.TrimSpace(tree)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(origin, "a.go"), []byte("package changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, _ := gitOutput(checkout, "rev-parse", "HEAD^{tree}")
	if strings.TrimSpace(got) != strings.TrimSpace(tree) {
		t.Fatal("verification input followed mutable provider workspace")
	}
}

func TestVerifierUsesSeparateOfflinePreparationAndReadonlyCandidate(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	cache := filepath.Join(root, "cache")
	_ = os.Mkdir(checkout, 0700)
	_ = os.Mkdir(cache, 0700)
	// The git binding is deliberately allowed to fail after preparation in this
	// command-shape test; direct preparation still proves its isolated command.
	_ = os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module x\ngo 1.25\n"), 0600)
	fake := &fakeCommandExecutor{found: true}
	v := BaselineGoVerifier{Sandbox: DockerSandbox{Image: "sha256:image", Executor: fake}, ArtifactStore: ArtifactStore{Root: filepath.Join(root, "artifacts")}, DependencyCacheDir: cache}
	if err := v.prepare(context.Background(), checkout); err != nil {
		t.Fatal(err)
	}
	text := commandText(fake.calls)
	for _, want := range []string{"go mod download", "--network none", "src=" + checkout + ",dst=/candidate,readonly", "src=" + cache + ",dst=/cache", "GOPROXY=off", "GOFLAGS=-mod=readonly"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing verifier isolation %q: %s", want, text)
		}
	}
	if strings.Contains(text, "GITHUB_TOKEN") || strings.Contains(text, "runtime.db") {
		t.Fatal("verifier received ambient secret/runtime")
	}
}

func TestDockerReconciliationTargetsOnlyRecordedExactContainer(t *testing.T) {
	root := t.TempDir()
	fake := &fakeCommandExecutor{found: true}
	s := DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "operation", StateDir: root}
	state, err := s.ReconcileDockerOperation()
	if err != nil || state != DockerNoContainer {
		t.Fatalf("unrecorded reconciliation = %q, %v", state, err)
	}
	if len(fake.calls) != 0 {
		t.Fatal("unrecorded reconciliation must not enumerate or target containers")
	}
	record, path, err := s.operationRecord()
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = "created"
	if err := writeDockerOperation(path, record); err != nil {
		t.Fatal(err)
	}
	state, err = s.ReconcileDockerOperation()
	if err != nil || state != DockerRemoved {
		t.Fatalf("recorded exited reconciliation = %q, %v", state, err)
	}
	text := commandText(fake.calls)
	for _, want := range []string{"inspect --format {{.State.Running}} " + record.ContainerName, "wait " + record.ContainerName, "rm " + record.ContainerName} {
		if !strings.Contains(text, want) {
			t.Fatalf("exact reconciliation missing %q: %s", want, text)
		}
	}
}

// Reconciliation never goes through run(), which is the only place that
// otherwise defaults Grace. Without a default on this path, terminateExact's
// deadline timer fires immediately and force-kills instead of attempting a
// graceful stop.
func TestReconciliationDefaultsGraceAndTriesGracefulStopBeforeForceKill(t *testing.T) {
	root := t.TempDir()
	// Two running inspects: one consumed by the reconcile-path check, one by
	// terminateExact's first loop pass, so the deadline/poll race is real.
	fake := &fakeCommandExecutor{found: true, runningInspects: 2}
	s := DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "operation", StateDir: root}
	record, path, err := s.operationRecord()
	if err != nil {
		t.Fatal(err)
	}
	record.Phase = "created"
	if err := writeDockerOperation(path, record); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReconcileDockerOperation(); err != nil {
		t.Fatal(err)
	}
	if !hasDockerInvocation(fake.calls, "kill", "--signal", "TERM", record.ContainerName) {
		t.Fatal("graceful stop was not attempted on the reconciliation path")
	}
	if hasDockerInvocation(fake.calls, "kill", "--signal", "KILL", record.ContainerName) {
		t.Fatal("reconciliation force-killed without waiting: Grace was not defaulted on the reconciliation path")
	}
}

func TestBoundedProcessCancellationKillsControlledGroup(t *testing.T) {
	requireBoundedProcess(t)
	if runtime.GOOS != "linux" {
		t.Skip("detached-session containment requires Linux /proc process identities")
	}
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := (OSCommandExecutor{}).Run(ctx, "sh", []string{"-c", "setsid sleep 30 & child=$!; echo $child > " + pidFile + "; wait"}, "", os.Environ(), 50*time.Millisecond)
		done <- err
	}()
	if err := waitForFile(pidFile, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("controlled child group survived cancellation")
	}
	if processFromFileAlive(pidFile) {
		t.Fatal("detached controlled child remained alive")
	}
}

func TestBoundedProcessCancellationKillsImmediateChild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	done, cancel := runControlledShell(t, "sleep 30 & child=$!; echo $child > "+pidFile+"; wait", 50*time.Millisecond)
	defer cancel()
	if err := waitForFile(pidFile, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	awaitBoundedProcess(t, done)
	if processFromFileAlive(pidFile) {
		t.Fatal("immediate controlled child remained alive")
	}
}

func TestBoundedProcessNaturalExitDuringGrace(t *testing.T) {
	start := time.Now()
	done, cancel := runControlledShell(t, "exit 0", time.Second)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("natural exit returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("natural exit was not reaped")
	}
	if elapsed := time.Since(start); elapsed >= 200*time.Millisecond {
		t.Fatalf("natural exit was artificially delayed by graceful period: %v", elapsed)
	}
	// Context cancellation after a completed Wait must be idempotent.
	cancel()
	cancel()
}

func TestBoundedProcessEscalatesAfterGrace(t *testing.T) {
	// The readiness file is written only after the shell has installed TERM
	// ignore. The child sleep inherits that disposition, so this is a controlled
	// proof of the force path rather than a race with process startup.
	rootPID := filepath.Join(t.TempDir(), "root.pid")
	ready := filepath.Join(t.TempDir(), "ready")
	grace := 50 * time.Millisecond
	done, cancel := runControlledShell(t, "trap '' TERM; echo $$ > "+rootPID+"; : > "+ready+"; while :; do sleep 30; done", grace)
	defer cancel()
	if err := waitForFile(ready, time.Second); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	cancel()
	select {
	case <-time.After(grace / 2):
		if !processFromFileAlive(rootPID) {
			t.Fatal("TERM-ignoring controlled workload exited during grace")
		}
	case <-done:
		t.Fatal("TERM-ignoring controlled workload returned before grace elapsed")
	}
	awaitBoundedProcess(t, done)
	if elapsed := time.Since(start); elapsed < grace-10*time.Millisecond {
		t.Fatalf("process returned before bounded graceful period: %v", elapsed)
	}
	if processFromFileAlive(rootPID) {
		t.Fatal("force-killed controlled workload was not reaped")
	}
}

func TestBoundedProcessRepeatedCancellationDoesNotTargetUnrelatedProcess(t *testing.T) {
	requireBoundedProcess(t)
	unrelated := exec.Command("sh", "-c", "sleep 30")
	if err := unrelated.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unrelated.Process.Kill()
		_, _ = unrelated.Process.Wait()
	}()
	done, cancel := runControlledShell(t, "sleep 30", 25*time.Millisecond)
	cancel()
	cancel()
	cancel()
	awaitBoundedProcess(t, done)
	if !processAlive(strconv.Itoa(unrelated.Process.Pid)) {
		t.Fatal("unrelated process was targeted")
	}
}

func runControlledShell(t *testing.T, script string, grace time.Duration) (<-chan error, context.CancelFunc) {
	t.Helper()
	requireBoundedProcess(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := (OSCommandExecutor{}).Run(ctx, "sh", []string{"-c", script}, "", os.Environ(), grace)
		done <- err
	}()
	return done, cancel
}

func requireBoundedProcess(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("bounded process containment is unavailable on windows")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
}

func awaitBoundedProcess(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("bounded process was not reaped")
	}
}

func waitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func processFromFileAlive(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return processAlive(strings.TrimSpace(string(data)))
}

func processAlive(pid string) bool {
	return exec.Command("sh", "-c", "kill -0 "+pid).Run() == nil
}

// This is deliberately an execution proof rather than a flag-string test. A
// host that provisions a pinned shell-capable test image can opt in without
// making ordinary unit tests require Docker. The image value must be its local
// immutable sha256 image ID.
func TestDockerSandboxBlocksHostFixturesWhenConfigured(t *testing.T) {
	image := os.Getenv("ZENCHRON_SANDBOX_TEST_IMAGE")
	if image == "" {
		t.Skip("set ZENCHRON_SANDBOX_TEST_IMAGE to an immutable local test image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable")
	}
	endpoint := DockerEndpoint{Host: os.Getenv("ZENCHRON_SANDBOX_TEST_DOCKER_HOST")}
	if _, err := endpoint.identity(); err != nil {
		t.Fatalf("invalid ZENCHRON_SANDBOX_TEST_DOCKER_HOST: %v", err)
	}
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	runtimeState := filepath.Join(root, "runtime")
	if err := os.MkdirAll(filepath.Join(candidate, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(runtimeState, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, ".git", "config"), []byte("github-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeState, "secret"), []byte("runtime-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ZENCHRON_AMBIENT_SECRET", "host-secret")
	s := DockerSandbox{Image: image, Endpoint: endpoint, Grace: time.Second, OperationID: "isolation-" + strconv.FormatInt(time.Now().UnixNano(), 10), StateDir: runtimeState}
	args := dockerBase(candidate, false)
	args = append(args, "--workdir", "/candidate", image, "sh", "-ec", "test ! -e /candidate/.git/config; test ! -e /runtime/secret; test -z \"${ZENCHRON_AMBIENT_SECRET:-}\"")
	if _, err := s.run(context.Background(), args); err != nil {
		t.Fatalf("sandbox boundary escaped: %v", err)
	}
}

func TestDockerSandboxOwnsExactContainerLifecycleWhenConfigured(t *testing.T) {
	image := os.Getenv("ZENCHRON_SANDBOX_TEST_IMAGE")
	if image == "" {
		t.Skip("set ZENCHRON_SANDBOX_TEST_IMAGE to an immutable local test image")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker unavailable")
	}
	endpoint := DockerEndpoint{Host: os.Getenv("ZENCHRON_SANDBOX_TEST_DOCKER_HOST")}
	if _, err := endpoint.identity(); err != nil {
		t.Fatalf("invalid ZENCHRON_SANDBOX_TEST_DOCKER_HOST: %v", err)
	}
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	op := "lifecycle-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	s := DockerSandbox{Image: image, Endpoint: endpoint, Grace: 250 * time.Millisecond, OperationID: op, StateDir: filepath.Join(root, "runtime")}
	record, _, err := s.operationRecord()
	if err != nil {
		t.Fatal(err)
	}
	unrelatedName := "zenchron-unrelated-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	if err := dockerTestCommand(endpoint, "run", "-d", "--name", unrelatedName, image, "sh", "-ec", "sleep 30").Run(); err != nil {
		t.Fatalf("start unrelated container: %v", err)
	}
	defer dockerTestCommand(endpoint, "rm", "-f", unrelatedName).Run()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	args := dockerBase(candidate, false)
	args = append(args, image, "sh", "-ec", "trap '' TERM; while :; do sleep 30; done")
	done := make(chan error, 1)
	go func() { _, err := s.run(ctx, args); done <- err }()
	if err := waitForDockerRunning(endpoint, record.ContainerName, done, time.Second); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
		// A non-nil error here is expected and is not a cleanup failure: the
		// attached `docker start` exits non-zero precisely because the runtime
		// force-killed the workload it owns. Cancellation is proven by the
		// lifecycle assertions below, never by the CLI's exit status.
	case <-time.After(3 * time.Second):
		t.Fatal("runtime-owned Docker workload was not reaped")
	}
	if dockerContainerExists(endpoint, record.ContainerName) {
		t.Fatal("exact runtime-owned Docker container was not removed")
	}
	if !dockerContainerRunning(endpoint, unrelatedName) {
		t.Fatal("unrelated container was targeted")
	}
}

func dockerTestCommand(endpoint DockerEndpoint, args ...string) *exec.Cmd {
	bound, err := endpoint.args(args)
	if err != nil {
		return exec.Command("false")
	}
	return exec.Command("docker", bound...)
}
func waitForDockerRunning(endpoint DockerEndpoint, name string, done <-chan error, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if dockerContainerRunning(endpoint, name) {
			return nil
		}
		select {
		case err := <-done:
			return fmt.Errorf("DockerSandbox.Run returned before container became ready: %w", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	return fmt.Errorf("timed out waiting for Docker container %q", name)
}
func dockerContainerRunning(endpoint DockerEndpoint, name string) bool {
	out, err := dockerTestCommand(endpoint, "inspect", "--format", "{{.State.Running}}", name).Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
func dockerContainerExists(endpoint DockerEndpoint, name string) bool {
	return dockerTestCommand(endpoint, "inspect", name).Run() == nil
}
