package main

import (
	"fmt"
	"io"
	"os"
	"strings"

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

// controllerDeclaration is what this build claims about itself: the link-time
// values, and nothing measured.
func controllerDeclaration() runtime.ControllerDeclaration {
	return runtime.ControllerDeclaration{
		Kind: buildKind, Version: version, SourceRevision: sourceRevision, SourceTree: sourceTree,
	}
}

// controllerSelf is this process's canonical identity. The resolution lives in
// the runtime because an activation proof needs it IN PROCESS: a controller
// cannot establish which generation it is by executing a binary it has to
// assume is itself. Everything here presents that one value.
func controllerSelf() (runtime.ControllerSelfRecord, error) {
	return runtime.CurrentControllerIdentity(controllerDeclaration())
}

// controllerBuild is the provenance document that identity carries.
func controllerBuild() (runtime.ControllerBuild, error) {
	self, err := controllerSelf()
	return self.Build, err
}

// exitUsage is the historical exit status for a top-level usage or selfhost
// failure. The autonomy subcommand reports the runtime exit codes instead.
const exitUsage = 1

func main() {
	code, err := run(os.Args[1:], osCommands{}, os.Stdout)
	if err != nil {
		// The diagnostic goes through the same terminal-safety boundary the
		// rendered views do. A refusal interpolates the material that caused it
		// - a stage id a model proposed, a message a forge returned - and this
		// is the FIRST place an operator sees it, before anyone runs a read
		// command. `planning/graph.go` joins proposed stage ids into the cycle
		// message unquoted, so the vector is real rather than hypothetical.
		fmt.Fprintln(os.Stderr, "zenchron-engineering:", terminalSafe(err.Error()))
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
	if len(args) >= 2 && args[0] == "controller" && args[1] == "status" {
		return controllerStatus(args[2:], autonomyOverrides{}, stdout)
	}
	if len(args) >= 2 && args[0] == "controller" && args[1] == "build-adopted" {
		return controllerBuildAdopted(args[2:], autonomyOverrides{}, stdout)
	}
	if len(args) >= 2 && args[0] == "controller" && args[1] == "re-adopt" {
		return controllerReadopt(args[2:], autonomyOverrides{}, stdout)
	}
	if len(args) >= 2 && args[0] == "controller" && args[1] == "install" {
		return controllerInstall(args[2:], stdout)
	}
	// The brokered Git decision of #241. It is dispatched first and separately
	// because it is not an operator command: a provider's shim execs it, its
	// exit status is the Git exit status the provider must see, and its output
	// is Git's output rather than a rendered view.
	if len(args) >= 1 && args[0] == gitBrokerSubcommand {
		return gitBroker(args[1:])
	}
	if len(args) >= 1 && args[0] == "autonomy" {
		return autonomy(args[1:], autonomyOverrides{}, stdout)
	}
	// `serve` is top level rather than under `autonomy`, because it is not one
	// more operation on a run: it is the persistent runtime the rest of the
	// commands talk to.
	if len(args) >= 1 && args[0] == "serve" {
		return serveCommand(args[1:], autonomyOverrides{}, stdout)
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
	return exitUsage, fmt.Errorf("usage: zenchron-engineering {version|serve|autonomy ...|controller inspect-self [--json]|controller status [--json] [--config <path>]|controller build-adopted [--repo owner/name] [--config <path>] [--output <dir>] [--revision <sha>]|controller re-adopt --reason <text>|controller install [--bin-dir <dir>]|selfhost issue <number> [--model <name>] [--fallback-model <name> ...]|selfhost resume issue <number>}\n\n" + serveUsage)
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
