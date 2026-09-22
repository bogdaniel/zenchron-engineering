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

// TestAdoptedBuildEnvironmentIsStatedNotInherited is defect W.
//
// The previous builder ran `go build` with os.Environ() appended, so ambient
// GOFLAGS, GOWORK, GOENV, GOPROXY, GOTOOLCHAIN and PATH could change what was
// compiled AFTER the source tree had already been approved.
// GOFLAGS=-overlay=/outside/overlay.json alone is enough: the binary ends up
// containing code from outside the recomputed tree while its embedded metadata
// still names that tree, and every later check - the self-probe included -
// passes.
//
// The fix is not scrubbing a list of variable names, which is a race against
// whatever the toolchain learns to read next. It is refusing to inherit an
// environment at all: Docker's --env is an allowlist, so the compiler sees
// exactly what is named here and nothing else.
//
// This test sets each hostile value in the TEST PROCESS and proves none of it
// reaches the container arguments.
func TestAdoptedBuildEnvironmentIsStatedNotInherited(t *testing.T) {
	hostile := map[string]string{
		"GOFLAGS":     "-overlay=/outside/overlay.json",
		"GOWORK":      "/outside/go.work",
		"GOENV":       "/outside/goenv",
		"GOPROXY":     "https://attacker.example",
		"GOTOOLCHAIN": "go1.99.0",
		"GONOSUMDB":   "*",
		"GOPRIVATE":   "*",
		"CGO_ENABLED": "1",
	}
	for name, value := range hostile {
		t.Setenv(name, value)
	}
	fakePATH := t.TempDir() // a directory that could hold a fake `go`
	t.Setenv("PATH", fakePATH)

	spec := AdoptedBuildSpec{
		SourceDir: t.TempDir(), Output: t.TempDir() + "/zenchron-engineering",
		GOOS: "darwin", GOARCH: "arm64", Kind: ControllerAdopted, Version: "v",
		Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
		Sandbox: DockerSandbox{Image: "sha256:pinned"}, CacheDir: t.TempDir(),
	}

	// The container argument vector is the whole surface: whatever is not in
	// it cannot reach the compiler.
	args := adoptedBuildArgs(spec)
	rendered := strings.Join(args, " ")
	// The assertion is on NAME=value pairs: a bare value like "1" would match
	// half a path and prove nothing.
	for name, value := range hostile {
		if strings.Contains(rendered, name+"="+value) {
			t.Fatalf("ambient %s=%q reached the build: %s", name, value, rendered)
		}
	}
	// The ambient PATH is replaced outright, not extended.
	if strings.Contains(rendered, "PATH="+fakePATH) {
		t.Fatalf("the ambient PATH reached the build: %s", rendered)
	}

	// And the settings that must be stated ARE stated.
	for _, want := range []string{
		"--network none", "--read-only", "--cap-drop ALL",
		"GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"GOFLAGS=-mod=readonly -buildvcs=false",
		"GOWORK=off", "GOENV=off", "CGO_ENABLED=0",
		"GOOS=darwin", "GOARCH=arm64",
		"GOMODCACHE=/cache", "-trimpath",
		"dst=/cache,readonly",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("the controlled build does not state %q: %s", want, rendered)
		}
	}
	// Source read-only, output the only writable product.
	if !strings.Contains(rendered, "src="+spec.SourceDir+",dst=/candidate,readonly") {
		t.Fatalf("the source is not mounted read-only: %s", rendered)
	}
	if !strings.Contains(rendered, "dst=/out") {
		t.Fatalf("the output directory is not mounted: %s", rendered)
	}
	// The pinned image, not a PATH lookup.
	if !strings.Contains(rendered, "sha256:pinned") {
		t.Fatalf("the build does not use the pinned image: %s", rendered)
	}
}

// TestAdoptedBuildRefusesAnUnpinnedEnvironment: without a pinned image or a
// provisioned offline cache there is no controlled environment, and an
// arbitrary `go` from an ambient PATH is not a trust boundary.
func TestAdoptedBuildRefusesAnUnpinnedEnvironment(t *testing.T) {
	base := AdoptedBuildSpec{
		SourceDir: t.TempDir(), Output: t.TempDir() + "/out",
		Sandbox: DockerSandbox{Image: "sha256:pinned"}, CacheDir: t.TempDir(),
	}
	for name, tc := range map[string]struct {
		mutate func(*AdoptedBuildSpec)
		says   string
	}{
		"no pinned image":  {func(s *AdoptedBuildSpec) { s.Sandbox.Image = "" }, "not a reproducible trust boundary"},
		"no trusted cache": {func(s *AdoptedBuildSpec) { s.CacheDir = "" }, "never downloads"},
		"empty cache":      {func(s *AdoptedBuildSpec) {}, "empty or unreadable"},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			spec := base
			tc.mutate(&spec)
			_, err := runAdoptedBuild(context.Background(), spec)
			if err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("refusal does not explain %q: %v", tc.says, err)
			}
		})
	}
}

// dockerLifecycleExecutor models a well-behaved daemon through the whole
// create/start/inspect/wait/rm sequence runContainer drives, and fails only
// the Nth `docker start --attach`, with the compiler's own stderr attached -
// exactly what a real `go build` failure looks like once the container has
// actually run. failOnStart counts container starts across the WHOLE build,
// which includes the toolchain probe runAdoptedBuild issues before compiling.
type dockerLifecycleExecutor struct {
	failOnStart int
	starts      int
	stderr      string
}

func (f *dockerLifecycleExecutor) LookPath(string) error { return nil }

func (f *dockerLifecycleExecutor) Run(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	if len(args) >= 2 && args[0] == "start" && args[1] == "--attach" {
		f.starts++
		if f.starts == f.failOnStart {
			return CommandOutput{Stderr: []byte(f.stderr)}, errors.New("exit status 1")
		}
	}
	return CommandOutput{}, nil
}

func (f *dockerLifecycleExecutor) Output(_ context.Context, _ string, args []string, _ string, _ []string, _ time.Duration) (CommandOutput, error) {
	if len(args) >= 5 && args[len(args)-5] == "image" && args[len(args)-4] == "inspect" {
		return CommandOutput{Stdout: []byte(args[len(args)-1] + "\n")}, nil
	}
	if len(args) >= 4 && args[len(args)-4] == "inspect" && args[len(args)-3] == "--format" && args[len(args)-2] == "{{.State.Running}}" {
		return CommandOutput{Stdout: []byte("false\n")}, nil
	}
	return CommandOutput{Stdout: []byte("daemon-test-id\n")}, nil
}

// TestRunAdoptedBuildSurfacesCompilerStderr is the fix for the deferred
// finding: a deterministic build failure used to collapse to the bare
// `exit status 1` runContainer's own error carries, with the compiler's own
// explanation - captured in CommandOutput.Stderr all along - discarded. An
// operator refused with no cause guesses instead of reading the reason.
func TestRunAdoptedBuildSurfacesCompilerStderr(t *testing.T) {
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, "module.info"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	const compilerStderr = "./cmd/zenchron-engineering/main.go:12:2: undefined: doesNotExist\n"
	fake := &dockerLifecycleExecutor{
		// The toolchain probe (`go version`) is the first container started;
		// the build itself is the second, and that is the one made to fail.
		failOnStart: 2,
		stderr:      compilerStderr,
	}
	spec := AdoptedBuildSpec{
		SourceDir: t.TempDir(), Output: filepath.Join(t.TempDir(), "zenchron-engineering"),
		GOOS: "linux", GOARCH: "amd64", Kind: ControllerAdopted, Version: "v",
		Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40),
		Sandbox:  DockerSandbox{Image: "sha256:pinned", Executor: fake, StateDir: t.TempDir()},
		CacheDir: cache,
	}
	_, err := runAdoptedBuild(context.Background(), spec)
	if err == nil {
		t.Fatal("a failed compile was reported as a success")
	}
	if !strings.Contains(err.Error(), "undefined: doesNotExist") {
		t.Fatalf("the compiler's own stderr did not reach the refusal: %v", err)
	}
	if !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("the underlying process error was dropped from the refusal: %v", err)
	}
}
