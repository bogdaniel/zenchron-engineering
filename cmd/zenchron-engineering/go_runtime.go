package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

type goRuntimeKind string

const (
	localGoRuntime  goRuntimeKind = "local"
	dockerGoRuntime goRuntimeKind = "docker"
)

// goRuntime is the single execution boundary for Go-backed bootstrap commands.
type goRuntime struct {
	kind                  goRuntimeKind
	goVersion             string
	environmentIdentifier string
	repositoryRoot        string
	commands              commandRunner
}

func (r goRuntime) String() string {
	return fmt.Sprintf("%s Go %s (%s)", r.kind, r.goVersion, r.environmentIdentifier)
}

func (r goRuntime) Run(args ...string) error {
	return r.RunTool("go", args...)
}

// RunTool executes a Go tool selected by the resolved bootstrap runtime.
// Candidate code must never execute directly on the operator host.
func (r goRuntime) RunTool(tool string, args ...string) error {
	switch r.kind {
	case localGoRuntime:
		return fmt.Errorf("local Go execution is disabled for untrusted candidate code")
	case dockerGoRuntime:
		return r.commands.Run(r.repositoryRoot, "docker", r.dockerToolArgs(tool, args...)...)
	default:
		return fmt.Errorf("unsupported Go runtime kind %q", r.kind)
	}
}

// OutputTool is RunTool for checks whose successful output is meaningful.
func (r goRuntime) OutputTool(tool string, args ...string) (string, error) {
	switch r.kind {
	case localGoRuntime:
		return "", fmt.Errorf("local Go execution is disabled for untrusted candidate code")
	case dockerGoRuntime:
		return r.commands.Output(r.repositoryRoot, "docker", r.dockerToolArgs(tool, args...)...)
	default:
		return "", fmt.Errorf("unsupported Go runtime kind %q", r.kind)
	}
}

func (r goRuntime) dockerToolArgs(tool string, args ...string) []string {
	dockerArgs := []string{
		"run", "--rm", "--network", "none",
		"--pull", "never", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
		"--tmpfs", "/tmp:rw,exec,nosuid,nodev,mode=1777",
		"--mount", "type=bind,src=" + r.repositoryRoot + ",dst=/workspace,readonly",
		"--workdir", "/workspace",
		"--env", "GOTOOLCHAIN=local",
		"--env", "GOPROXY=off",
		"--env", "GOSUMDB=off",
		"--env", "GOFLAGS=-mod=readonly",
		"--env", "HOME=/tmp/zenchron-home",
		"--env", "GOPATH=/tmp/zenchron-go",
		"--env", "GOMODCACHE=/go/pkg/mod",
		"--env", "GOCACHE=/tmp/zenchron-go-build",
		r.environmentIdentifier, tool,
	}
	return append(dockerArgs, args...)
}

// resolveGoRuntime fails closed: even a compatible host Go cannot isolate
// candidate tests from operator credentials or host filesystem access.
func resolveGoRuntime(root string, commands commandRunner) (goRuntime, error) {
	required, err := requiredGoVersion(filepath.Join(root, "go.mod"))
	if err != nil {
		return goRuntime{}, err
	}

	if err := commands.LookPath("docker"); err != nil {
		return goRuntime{}, fmt.Errorf("Docker is required to isolate candidate Go execution; install Docker")
	}
	if _, err := commands.Output(root, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return goRuntime{}, fmt.Errorf("Docker must be started and reachable: %w", err)
	}
	image := dockerGoImage(required)
	imageID, err := commands.Output(root, "docker", "image", "inspect", "--format", "{{.Id}}", image)
	if err != nil {
		return goRuntime{}, fmt.Errorf("required Docker image %q is not available locally; prepare it explicitly with `docker pull %s`", image, image)
	}
	if !strings.HasPrefix(imageID, "sha256:") {
		return goRuntime{}, fmt.Errorf("required Docker image %q did not resolve to an immutable image ID", image)
	}
	return goRuntime{dockerGoRuntime, required, imageID, root, commands}, nil
}

func requiredGoVersion(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read Go requirement from %s: %w", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "go" && validGoVersion(fields[1]) {
			return fields[1], nil
		}
	}
	return "", fmt.Errorf("go.mod does not contain a valid Go version requirement")
}

var goVersionPattern = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?$`)

func validGoVersion(version string) bool { return goVersionPattern.MatchString(version) }

func parseGoVersion(output string) (string, error) {
	for _, field := range strings.Fields(output) {
		version := strings.TrimPrefix(field, "go")
		if field != version && validGoVersion(version) {
			return version, nil
		}
	}
	return "", fmt.Errorf("cannot determine installed Go version from %q", output)
}

func compatibleGoVersion(installed, required string) bool {
	iMajor, iMinor, iPatch, ok := goVersionParts(installed)
	if !ok {
		return false
	}
	rMajor, rMinor, rPatch, ok := goVersionParts(required)
	return ok && (iMajor > rMajor ||
		iMajor == rMajor && iMinor > rMinor ||
		iMajor == rMajor && iMinor == rMinor && iPatch >= rPatch)
}

func goVersionParts(version string) (int, int, int, bool) {
	match := goVersionPattern.FindStringSubmatch(version)
	if match == nil {
		return 0, 0, 0, false
	}
	major, majorErr := strconv.Atoi(match[1])
	minor, minorErr := strconv.Atoi(match[2])
	patch := 0
	var patchErr error
	if match[3] != "" {
		patch, patchErr = strconv.Atoi(match[3])
	}
	return major, minor, patch, majorErr == nil && minorErr == nil && patchErr == nil
}

// goCompatibilityLine reduces a Go version to its major.minor compatibility
// line (e.g. "1.25.0" -> "1.25"). A patch release is not a distinct runtime
// requirement: `go 1.25` and `go 1.25.0` in go.mod name the same compatible
// toolchain, so `golang:1.25.0` is not a separate image from `golang:1.25`.
// An exact pin (a future go.mod `toolchain` directive) is a stricter,
// separate requirement and must not be derived from or folded into this line.
func goCompatibilityLine(version string) string {
	major, minor, _, ok := goVersionParts(version)
	if !ok {
		return version
	}
	return fmt.Sprintf("%d.%d", major, minor)
}

func dockerGoImage(version string) string { return "golang:" + goCompatibilityLine(version) }
