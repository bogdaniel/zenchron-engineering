package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Toolchain scratch is not engineering output.
//
// A brokered command used to run with GOMODCACHE, GOPATH and GOENV unnamed, so
// Go resolved them itself: onto the image's GOPATH on the read-only root, where
// `go test` failed for want of a module cache. The way around that from inside
// /candidate is to point the caches at a relative path, and that is what two
// runs did - run-0b4769171e74d5507246b589310f6500 committed three .gomodcache
// lock files, and run-fd69fe24ac1e30377d6aa0934756428c committed 412 .gocache
// files beside five real source changes.
//
// These tests pin the environment that removes the reason to do that, and the
// containment that keeps every writable Go location outside the workspace.

// brokeredGoEnvironment returns the effective environment of a brokered
// command: the LAST value of each name, because that is what Docker gives the
// process.
func brokeredGoEnvironment(t *testing.T, args []string) map[string]string {
	t.Helper()
	env := map[string]string{}
	for i, arg := range args {
		if arg != "--env" || i+1 >= len(args) {
			continue
		}
		name, value, _ := strings.Cut(args[i+1], "=")
		env[name] = value
	}
	return env
}

func brokeredMounts(args []string) []string {
	var mounts []string
	for i, arg := range args {
		if (arg == "--mount" || arg == "--tmpfs" || arg == "-v") && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
		if strings.HasPrefix(arg, "--mount=") {
			mounts = append(mounts, strings.TrimPrefix(arg, "--mount="))
		}
	}
	return mounts
}

// goWritableLocations are every variable that decides where the Go toolchain
// puts something. The list is the test's own statement of completeness: if the
// runtime stops naming one, Go starts choosing for it.
var goWritableLocations = []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "GOENV", "HOME"}

// TestBrokeredGoStateIsRuntimeOwnedAndOutsideTheCandidate is acceptance A, B, C
// and I stated once at the level that actually decides them: every writable Go
// location is named, and none resolves under /candidate.
func TestBrokeredGoStateIsRuntimeOwnedAndOutsideTheCandidate(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)
	for _, command := range [][]string{
		{"go", "test", "./..."},
		{"go", "vet", "./..."},
		{"go", "list", "-deps", "./..."},
	} {
		t.Run(strings.Join(command, " "), func(t *testing.T) {
			fake.calls = nil
			if _, err := broker.RunCommand(context.Background(), command); err != nil {
				t.Fatalf("brokered %v was refused: %v", command, err)
			}
			args := brokeredContainerArgs(t, fake)
			env := brokeredGoEnvironment(t, args)
			for _, name := range goWritableLocations {
				value, ok := env[name]
				if !ok {
					t.Fatalf("%s is unnamed, so Go resolves it for itself", name)
				}
				if !strings.HasPrefix(value, "/") {
					t.Fatalf("%s=%q is not container-absolute", name, value)
				}
				if value == "/candidate" || strings.HasPrefix(value, "/candidate/") {
					t.Fatalf("%s=%q puts toolchain state inside the engineering workspace", name, value)
				}
			}
			// The runtime-owned locations exist as tmpfs the container owns,
			// rather than as directories inside a bind mount.
			mounts := strings.Join(brokeredMounts(args), " ")
			for _, dir := range []string{sandboxBuildDir, sandboxGoPathDir, sandboxModuleCacheDir, sandboxHomeDir} {
				if !strings.Contains(mounts, dir+":") {
					t.Fatalf("%s is named in the environment but is not a runtime-owned tmpfs: %s", dir, mounts)
				}
			}
		})
	}
}

// TestBrokeredGoStateCannotBeRedirectedIntoTheCandidate is acceptance I. GOENV
// is the file Go reads settings from; pointed anywhere the candidate controls,
// a repository could set GOMODCACHE back into itself.
func TestBrokeredGoStateCannotBeRedirectedIntoTheCandidate(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)
	// A candidate that ships its own Go configuration and cache directories.
	for _, name := range []string{".gocache", ".gomodcache", "go.env"} {
		if err := os.MkdirAll(filepath.Join(broker.CandidateDir, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := broker.RunCommand(context.Background(), []string{"go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	env := brokeredGoEnvironment(t, brokeredContainerArgs(t, fake))
	if env["GOENV"] != sandboxGoEnvFile {
		t.Fatalf("GOENV=%q is not the runtime-owned file", env["GOENV"])
	}
	if strings.HasPrefix(env["GOENV"], "/candidate") {
		t.Fatal("GOENV reads configuration the candidate controls")
	}
	// Naming GOENV at a runtime path is what stops an ambient operator
	// configuration reaching the sandbox at all.
	if env["GOENV"] == "" {
		t.Fatal("GOENV unset lets Go read the operator's own environment file")
	}
}

// TestBrokeredDependencyInputIsControlledAndReadOnly is acceptance D and E.
func TestBrokeredDependencyInputIsControlledAndReadOnly(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)

	// E: with no operator cache the module cache is still runtime-owned and
	// still outside the candidate. It is simply empty, so an offline command
	// fails for want of modules rather than creating a cache in the workspace.
	if _, err := broker.RunCommand(context.Background(), []string{"go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	args := brokeredContainerArgs(t, fake)
	env := brokeredGoEnvironment(t, args)
	if env["GOMODCACHE"] != sandboxModuleCacheDir {
		t.Fatalf("without an operator cache GOMODCACHE=%q", env["GOMODCACHE"])
	}
	if env["GOPROXY"] != "off" {
		t.Fatalf("GOPROXY=%q would let a missing module become a download", env["GOPROXY"])
	}
	if !strings.Contains(strings.Join(args, " "), "--network none") {
		t.Fatal("a brokered command is not network-denied")
	}

	// D: with an operator cache, it is the module source and it is read-only.
	cache := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cache, "cache", "download"), 0o700); err != nil {
		t.Fatal(err)
	}
	withCache := broker
	withCache.DependencyCacheDir = cache
	fake.calls = nil
	if _, err := withCache.RunCommand(context.Background(), []string{"go", "list", "-deps", "./..."}); err != nil {
		t.Fatal(err)
	}
	args = brokeredContainerArgs(t, fake)
	env = brokeredGoEnvironment(t, args)
	if env["GOMODCACHE"] != "/cache" {
		t.Fatalf("the operator cache is not the module source: GOMODCACHE=%q", env["GOMODCACHE"])
	}
	mounts := strings.Join(brokeredMounts(args), " ")
	if !strings.Contains(mounts, "dst=/cache,readonly") {
		t.Fatalf("the dependency cache is not mounted read-only: %s", mounts)
	}
	if strings.Contains(strings.Join(args, " "), "--network none") != true {
		t.Fatal("networking was enabled to reach dependencies")
	}
}

// TestBrokeredDependencyCacheCannotBeTheCandidate keeps candidate-controlled
// files from arriving as trusted dependency input.
func TestBrokeredDependencyCacheCannotBeTheCandidate(t *testing.T) {
	broker, _ := brokeredCommandFixture(t)
	for name, dir := range map[string]string{
		"the candidate itself":      broker.CandidateDir,
		"inside the candidate":      filepath.Join(broker.CandidateDir, "sub"),
		"a parent of the candidate": filepath.Dir(broker.CandidateDir),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			hostile := broker
			hostile.DependencyCacheDir = dir
			if _, err := hostile.RunCommand(context.Background(), []string{"go", "list", "./..."}); err == nil {
				t.Fatal("a candidate-adjacent dependency cache was accepted")
			}
		})
	}
	missing := broker
	missing.DependencyCacheDir = filepath.Join(t.TempDir(), "absent")
	if _, err := missing.RunCommand(context.Background(), []string{"go", "list", "./..."}); err == nil {
		t.Fatal("an unreadable dependency cache was accepted")
	}
}

// TestBrokeredSandboxBoundaryIsUnchanged is acceptance H and J: the isolation
// this change must not weaken, asserted on the arguments the command actually
// runs behind.
func TestBrokeredSandboxBoundaryIsUnchanged(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)
	if _, err := broker.RunCommand(context.Background(), []string{"go", "test", "./..."}); err != nil {
		t.Fatal(err)
	}
	args := brokeredContainerArgs(t, fake)
	joined := strings.Join(args, " ")
	for _, required := range []string{
		"--network none", "--read-only", "--cap-drop ALL",
		"--security-opt no-new-privileges", "--init",
	} {
		if !strings.Contains(joined, required) {
			t.Fatalf("the brokered boundary lost %q", required)
		}
	}
	// The candidate is the only host path attached, and nothing else from the
	// host reaches the container.
	for _, mount := range brokeredMounts(args) {
		if !strings.HasPrefix(mount, "type=bind") {
			continue
		}
		if !strings.Contains(mount, "src="+broker.CandidateDir+",") {
			t.Fatalf("a host path other than the candidate is mounted: %s", mount)
		}
	}
	for _, forbidden := range []string{
		"docker.sock", "/var/run", ".zenchron-dogfood", "runtime.db",
		"/Users/", "/root", ".ssh", ".netrc", ".config/gh",
	} {
		if strings.Contains(joined, forbidden) && !strings.Contains(broker.CandidateDir, forbidden) {
			t.Fatalf("the brokered boundary exposes %q", forbidden)
		}
	}
	// #47: the container is still bound to the exact authorizing operation.
	if !strings.Contains(joined, "zenchron-") {
		t.Fatal("the brokered container lost its runtime-owned operation identity")
	}
}

// TestBrokeredGoEnvironmentIsExhaustivelyStated is the guard that keeps this
// repair from decaying. A future variable that decides a writable Go location
// has to be added to dockerBase deliberately, because the effective environment
// is compared against a stated set rather than sampled.
func TestBrokeredGoEnvironmentIsExhaustivelyStated(t *testing.T) {
	env := brokeredGoEnvironment(t, dockerBase(t.TempDir(), false))
	stated := map[string]string{
		"HOME":       sandboxHomeDir,
		"GOTMPDIR":   sandboxBuildDir,
		"GOCACHE":    sandboxBuildDir + "/cache",
		"GOPATH":     sandboxGoPathDir,
		"GOMODCACHE": sandboxModuleCacheDir,
		"GOENV":      sandboxGoEnvFile,
	}
	for name, want := range stated {
		if env[name] != want {
			t.Fatalf("%s=%q, want %q", name, env[name], want)
		}
	}
	for _, name := range goWritableLocations {
		if _, ok := stated[name]; !ok {
			t.Fatalf("%s decides a writable Go location but dockerBase does not state it", name)
		}
	}
	// Nothing beyond PATH and the stated set is forwarded.
	for name := range env {
		if name == "PATH" {
			continue
		}
		if _, ok := stated[name]; !ok {
			t.Fatalf("dockerBase forwards %q, which is not part of the stated law", name)
		}
	}
}

// TestAssuranceAndAdoptedBuildKeepTheirOwnModuleCache proves the shared change
// did not disturb the two callers that already owned a stricter cache: Docker
// takes the LAST value of a name, and theirs is applied after dockerBase.
func TestAssuranceAndAdoptedBuildKeepTheirOwnModuleCache(t *testing.T) {
	args := append(dockerBase(t.TempDir(), true), goModuleCacheMount("/operator/cache"))
	args = append(args, envArgs(append([]string{"GOMODCACHE=/cache"}, sandboxGoEnv...)...)...)
	if env := brokeredGoEnvironment(t, args); env["GOMODCACHE"] != "/cache" {
		t.Fatalf("a caller's stricter module cache was overridden: GOMODCACHE=%q", env["GOMODCACHE"])
	}
}
