package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestResolveGoRuntime(t *testing.T) {
	tests := []struct {
		name        string
		commands    *runtimeCommands
		wantKind    goRuntimeKind
		wantError   string
		wantVersion string
	}{
		{"compatible local Go", &runtimeCommands{goVersion: "go version go1.25.3 linux/amd64", dockerAvailable: true, dockerRunning: true, imageAvailable: true}, localGoRuntime, "", "1.25.3"},
		{"local absent Docker running", &runtimeCommands{dockerAvailable: true, dockerRunning: true, imageAvailable: true}, dockerGoRuntime, "", "1.25"},
		{"local incompatible Docker running", &runtimeCommands{goVersion: "go version go1.24.9 linux/amd64", dockerAvailable: true, dockerRunning: true, imageAvailable: true}, dockerGoRuntime, "", "1.25"},
		{"neither available", &runtimeCommands{}, "", "install Go 1.25 or Docker", ""},
		{"Docker stopped", &runtimeCommands{dockerAvailable: true}, "", "Docker must be started", ""},
		{"image absent", &runtimeCommands{dockerAvailable: true, dockerRunning: true}, "", "docker pull golang:1.25", ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := writeGoMod(t, "module example.test/runtime\n\ngo 1.25\n")
			runtime, err := resolveGoRuntime(root, test.commands)
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("error = %v, want containing %q", err, test.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if runtime.kind != test.wantKind || runtime.goVersion != test.wantVersion {
				t.Fatalf("runtime kind/version = %q/%q, want %q/%q", runtime.kind, runtime.goVersion, test.wantKind, test.wantVersion)
			}
		})
	}
}

func TestResolveGoRuntimePrefersLocalGo(t *testing.T) {
	commands := &runtimeCommands{goVersion: "go version go1.26.0 linux/amd64", dockerAvailable: true, dockerRunning: true, imageAvailable: true}
	runtime, err := resolveGoRuntime(writeGoMod(t, "module example.test/runtime\n\ngo 1.25\n"), commands)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.kind != localGoRuntime {
		t.Fatalf("kind = %q, want local", runtime.kind)
	}
	if slices.Contains(commands.calls, "docker info --format {{.ServerVersion}}") {
		t.Fatal("Docker must not be inspected when local Go is compatible")
	}
}

func TestGoRuntimeRunsDockerWithBoundedDerivedCommand(t *testing.T) {
	root := writeGoMod(t, "module example.test/runtime\n\ngo 1.25\n")
	commands := &runtimeCommands{dockerAvailable: true, dockerRunning: true, imageAvailable: true}
	runtime, err := resolveGoRuntime(root, commands)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Run("test", "./..."); err != nil {
		t.Fatal(err)
	}
	call := commands.calls[len(commands.calls)-1]
	for _, want := range []string{
		"docker run --rm",
		"--network bridge",
		"--tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777",
		"--mount type=bind,src=" + root + ",dst=/workspace",
		"--workdir /workspace",
		"--env HOME=/tmp/zenchron-home",
		"--env GOPATH=/tmp/zenchron-go",
		"--env GOMODCACHE=/tmp/zenchron-go/pkg/mod",
		"--env GOCACHE=/tmp/zenchron-go-build",
		"sha256:test-image go test ./...",
	} {
		if !strings.Contains(call, want) {
			t.Errorf("Docker command missing %q:\n%s", want, call)
		}
	}
	for _, forbidden := range []string{"--privileged", "/var/run/docker.sock", "--network none"} {
		if strings.Contains(call, forbidden) {
			t.Errorf("Docker command contains forbidden %q:\n%s", forbidden, call)
		}
	}
}

func TestDockerGoRuntimeGoTestUsesExecutableEphemeralTemporaryDirectory(t *testing.T) {
	commands := &runtimeCommands{}
	runtime := goRuntime{
		kind:                  dockerGoRuntime,
		goVersion:             "1.25",
		environmentIdentifier: "sha256:test-image",
		repositoryRoot:        "/workspace-source",
		commands:              commands,
	}

	if err := runtime.Run("test", "./..."); err != nil {
		t.Fatal(err)
	}

	call := commands.calls[len(commands.calls)-1]
	if !strings.Contains(call, "--tmpfs /tmp:rw,exec,nosuid,nodev,mode=1777") {
		t.Fatalf("go test temporary binaries require an executable /tmp tmpfs:\n%s", call)
	}
	if strings.Contains(call, "GOCACHE=/workspace") || strings.Contains(call, "GOPATH=/workspace") {
		t.Fatalf("Go temporary build state must remain outside the repository mount:\n%s", call)
	}
}

func TestVerifyBootstrapChecksUsesResolvedDockerRuntime(t *testing.T) {
	root := writeGoMod(t, "module example.test/runtime\n\ngo 1.25\n")
	commands := &runtimeCommands{dockerAvailable: true, dockerRunning: true, imageAvailable: true}
	runtime, err := resolveGoRuntime(root, commands)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := verifyBootstrapChecks(runtime)
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 3 {
		t.Fatalf("verified checks = %d, want 3", len(checks))
	}
	got := strings.Join(commands.calls, "\n")
	for _, want := range []string{"sha256:test-image gofmt -l", "sha256:test-image go vet ./...", "sha256:test-image go test ./..."} {
		if !strings.Contains(got, want) {
			t.Errorf("Docker verification missing %q:\n%s", want, got)
		}
	}
}

func TestDockerGoRuntimeCanResolveRepositoryModules(t *testing.T) {
	goMod, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(goMod), "github.com/santhosh-tekuri/jsonschema/v6") {
		t.Fatal("regression requires this repository to declare a non-vendored external module")
	}
	if _, err := os.Stat(filepath.Join("..", "..", "vendor")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("regression requires no vendor directory; stat error = %v", err)
	}

	commands := &runtimeCommands{}
	runtime := goRuntime{dockerGoRuntime, "1.25", "sha256:test-image", filepath.Join("..", ".."), commands}
	if err := runtime.Run("vet", "./..."); err != nil {
		t.Fatal(err)
	}
	call := commands.calls[len(commands.calls)-1]
	if !strings.Contains(call, "--network bridge") {
		t.Fatalf("non-vendored module resolution requires network access:\n%s", call)
	}
	if !strings.Contains(call, "--env GOMODCACHE=/tmp/zenchron-go/pkg/mod") {
		t.Fatalf("module resolution requires a writable module cache:\n%s", call)
	}
}

func TestGoRuntimeDisablesAutomaticLocalToolchainDownload(t *testing.T) {
	commands := &runtimeCommands{goVersion: "go version go1.25.1 test/arch"}
	runtime, err := resolveGoRuntime(writeGoMod(t, "module example.test/runtime\n\ngo 1.25\n"), commands)
	if err != nil {
		t.Fatal(err)
	}
	if err := runtime.Run("test", "./..."); err != nil {
		t.Fatal(err)
	}
	if got := commands.calls[len(commands.calls)-1]; got != "GOTOOLCHAIN=local go test ./..." {
		t.Fatalf("local Go command = %q", got)
	}
}

func TestRequiredGoVersionComesFromGoMod(t *testing.T) {
	root := writeGoMod(t, "module example.test/runtime\n\ngo 1.27.2\n")
	version, err := requiredGoVersion(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	if version != "1.27.2" || dockerGoImage(version) != "golang:1.27.2" {
		t.Fatalf("version/image = %q/%q", version, dockerGoImage(version))
	}
}

func TestCompatibleGoVersionHonorsPatchRequirement(t *testing.T) {
	if compatibleGoVersion("1.25.1", "1.25.2") {
		t.Fatal("older patch release must not satisfy the go.mod requirement")
	}
	if !compatibleGoVersion("1.25.2", "1.25.2") || !compatibleGoVersion("1.26.0", "1.25.2") {
		t.Fatal("equal or newer Go release must satisfy the go.mod requirement")
	}
}

func TestSelfhostResolvesRuntimeBeforeMutation(t *testing.T) {
	commands := newFakeCommands(t)
	commands.goUnavailable = true
	commands.dockerUnavailable = true
	err := selfhostIssue("4", commands, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "install Go 1.25 or Docker") {
		t.Fatalf("error = %v", err)
	}
	for _, call := range commands.calls {
		if strings.HasPrefix(call, "git fetch ") || strings.HasPrefix(call, "git switch ") {
			t.Fatalf("repository mutation attempted before runtime resolution: %s", call)
		}
	}
}

func writeGoMod(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

type runtimeCommands struct {
	goVersion       string
	dockerAvailable bool
	dockerRunning   bool
	imageAvailable  bool
	calls           []string
}

func (c *runtimeCommands) LookPath(name string) error {
	c.calls = append(c.calls, "lookpath "+name)
	if name == "go" && c.goVersion != "" || name == "docker" && c.dockerAvailable {
		return nil
	}
	return errors.New("missing")
}

func (c *runtimeCommands) Output(_ string, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	switch {
	case call == "git ls-files -z -- *.go":
		return "cmd/zenchron-engineering/main.go", nil
	case call == "docker info --format {{.ServerVersion}}" && c.dockerRunning:
		return "28.0.0", nil
	case call == "docker image inspect --format {{.Id}} golang:1.25" && c.imageAvailable:
		return "sha256:test-image", nil
	case strings.Contains(call, "sha256:test-image gofmt -l"):
		return "", nil
	default:
		return "", errors.New("unavailable")
	}
}

func (c *runtimeCommands) OutputEnv(_ string, _ []string, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	c.calls = append(c.calls, call)
	if call == "go version" && c.goVersion != "" {
		return c.goVersion, nil
	}
	return "", errors.New("unavailable")
}

func (c *runtimeCommands) Run(_ string, name string, args ...string) error {
	c.calls = append(c.calls, name+" "+strings.Join(args, " "))
	return nil
}

func (c *runtimeCommands) RunEnv(_ string, env []string, name string, args ...string) error {
	c.calls = append(c.calls, strings.Join(env, " ")+" "+name+" "+strings.Join(args, " "))
	return nil
}
