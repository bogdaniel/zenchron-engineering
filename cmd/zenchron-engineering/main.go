package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// Build identity. These are variables rather than constants because a constant
// cannot be injected: the controlled build sets them with -ldflags -X from the
// exact source checkout that produced the binary. See controllerBuild.
//
//	go build -trimpath -ldflags "\
//	  -X main.buildKind=pre_adoption_build \
//	  -X main.version=$(git rev-parse --short HEAD) \
//	  -X main.sourceRevision=$(git rev-parse HEAD) \
//	  -X main.sourceTree=$(git rev-parse HEAD^{tree})" \
//	  -o bin/zenchron-engineering ./cmd/zenchron-engineering
//
// A build that injects nothing is unattested: it still runs, but it records no
// provenance claim and can never be mistaken for an adopted controller.
var (
	version        = "dev"
	buildKind      = runtime.ControllerUnattested
	sourceRevision string
	sourceTree     string
)

// controllerBuild is this process's provenance, resolved once from the injected
// metadata plus a measurement of the executable that is actually running.
func controllerBuild() (runtime.ControllerBuild, error) {
	return buildProvenance(buildKind, version, sourceRevision, sourceTree, runningBinaryDigest)
}

// buildProvenance takes its inputs instead of reading the package variables, so
// a test asserts the exact same resolution without a real -ldflags build and
// without hashing whatever binary happens to be running.
func buildProvenance(kind, version, revision, tree string, digest func() (string, error)) (runtime.ControllerBuild, error) {
	if kind == "" || kind == runtime.ControllerUnattested {
		return runtime.ControllerBuild{Kind: runtime.ControllerUnattested}, nil
	}
	binary, err := digest()
	if err != nil {
		return runtime.ControllerBuild{}, fmt.Errorf("cannot measure the running controller binary: %w", err)
	}
	return runtime.ControllerBuild{
		Kind: kind, Version: version, SourceRevision: revision, SourceTree: tree, BinarySHA256: binary,
	}, nil
}

// runningBinaryDigest hashes the executable this process was started from. A
// binary cannot contain its own final digest, and an adjacent metadata file is
// a claim rather than a measurement, so the only honest answer is to read the
// running artifact back and hash it. It is computed at most once: the file
// cannot change identity under a running process.
var runningBinaryDigest = sync.OnceValues(func() (string, error) {
	path, err := os.Executable()
	if err != nil {
		return "", err
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
})

// exitUsage is the historical exit status for a top-level usage or selfhost
// failure. The autonomy subcommand reports the runtime exit codes instead.
const exitUsage = 1

func main() {
	code, err := run(os.Args[1:], osCommands{}, os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "zenchron-engineering:", err)
	}
	os.Exit(code)
}

// run returns the process exit status alongside the diagnostic error. main
// passes the status to os.Exit unchanged, so the code a handler returns is the
// code the process reports.
func run(args []string, commands commandRunner, stdout io.Writer) (int, error) {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return runtime.ExitCompleted, nil
	}
	if len(args) >= 2 && args[0] == "controller" && args[1] == "inspect-self" {
		return controllerInspectSelf(args[2:], stdout)
	}
	if len(args) >= 2 && args[0] == "controller" && args[1] == "build-adopted" {
		return controllerBuildAdopted(args[2:], autonomyOverrides{}, stdout)
	}
	if len(args) >= 1 && args[0] == "autonomy" {
		return autonomy(args[1:], autonomyOverrides{}, stdout)
	}
	if len(args) >= 3 && args[0] == "selfhost" && args[1] == "issue" {
		models, err := parseModelFlags(args[3:])
		if err != nil {
			return exitUsage, err
		}
		if err := selfhostIssueWithModels(args[2], models, commands, stdout); err != nil {
			return exitUsage, err
		}
		return runtime.ExitCompleted, nil
	}
	if len(args) == 4 && args[0] == "selfhost" && args[1] == "resume" && args[2] == "issue" {
		if err := selfhostResume(args[3], commands, stdout); err != nil {
			return exitUsage, err
		}
		return runtime.ExitCompleted, nil
	}
	return exitUsage, fmt.Errorf("usage: zenchron-engineering {version|controller inspect-self [--json]|controller build-adopted [--repo owner/name] [--config <path>] [--output <dir>] [--revision <sha>]|autonomy ...|selfhost issue <number> [--model <name>] [--fallback-model <name> ...]|selfhost resume issue <number>}")
}

func parseModelFlags(args []string) ([]string, error) {
	models := make([]string, 0, 3)
	for len(args) > 0 {
		if (args[0] != "--model" && args[0] != "--fallback-model") || len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return nil, fmt.Errorf("model selection requires --model <name> followed by optional --fallback-model <name>")
		}
		if args[0] == "--fallback-model" && len(models) == 0 {
			return nil, fmt.Errorf("--fallback-model requires a preceding --model")
		}
		models = append(models, args[1])
		args = args[2:]
	}
	if len(models) > 3 {
		return nil, fmt.Errorf("at most 3 Codex model attempts may be configured")
	}
	return models, nil
}
