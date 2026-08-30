package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const version = "dev"

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
	return exitUsage, fmt.Errorf("usage: zenchron-engineering {version|autonomy ...|selfhost issue <number> [--model <name>] [--fallback-model <name> ...]|selfhost resume issue <number>}")
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
