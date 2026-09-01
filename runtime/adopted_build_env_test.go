package runtime

import (
	"context"
	"strings"
	"testing"
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
