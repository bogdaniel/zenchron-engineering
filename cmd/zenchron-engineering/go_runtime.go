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
	switch r.kind {
	case localGoRuntime:
		return r.commands.RunEnv(r.repositoryRoot, []string{"GOTOOLCHAIN=local"}, "go", args...)
	case dockerGoRuntime:
		dockerArgs := []string{
			"run", "--rm", "--network", "bridge",
			"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid()),
			"--tmpfs", "/tmp:rw,nosuid,nodev,mode=1777",
			"--mount", "type=bind,src=" + r.repositoryRoot + ",dst=/workspace",
			"--workdir", "/workspace",
			"--env", "GOTOOLCHAIN=local",
			"--env", "HOME=/tmp/zenchron-home",
			"--env", "GOPATH=/tmp/zenchron-go",
			"--env", "GOMODCACHE=/tmp/zenchron-go/pkg/mod",
			"--env", "GOCACHE=/tmp/zenchron-go-build",
			r.environmentIdentifier, "go",
		}
		return r.commands.Run(r.repositoryRoot, "docker", append(dockerArgs, args...)...)
	default:
		return fmt.Errorf("unsupported Go runtime kind %q", r.kind)
	}
}

func resolveGoRuntime(root string, commands commandRunner) (goRuntime, error) {
	required, err := requiredGoVersion(filepath.Join(root, "go.mod"))
	if err != nil {
		return goRuntime{}, err
	}

	localProblem := "local Go is not installed"
	if err := commands.LookPath("go"); err == nil {
		output, versionErr := commands.OutputEnv(root, []string{"GOTOOLCHAIN=local"}, "go", "version")
		if versionErr == nil {
			installed, parseErr := parseGoVersion(output)
			if parseErr == nil && compatibleGoVersion(installed, required) {
				return goRuntime{localGoRuntime, installed, "host-go:" + installed, root, commands}, nil
			}
			if parseErr != nil {
				localProblem = parseErr.Error()
			} else {
				localProblem = fmt.Sprintf("local Go %s is incompatible with required Go %s", installed, required)
			}
		} else {
			localProblem = fmt.Sprintf("local Go is unusable: %v", versionErr)
		}
	}

	if err := commands.LookPath("docker"); err != nil {
		return goRuntime{}, fmt.Errorf("%s and Docker is not installed; install Go %s or Docker", localProblem, required)
	}
	if _, err := commands.Output(root, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		return goRuntime{}, fmt.Errorf("%s; Docker must be started and reachable: %w", localProblem, err)
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

func dockerGoImage(version string) string { return "golang:" + version }
