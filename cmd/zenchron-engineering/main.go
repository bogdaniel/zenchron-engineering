package main

import (
	"fmt"
	"os"
	"strings"
)

const version = "dev"

func main() {
	if err := run(os.Args[1:], osCommands{}, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "zenchron-engineering:", err)
		os.Exit(1)
	}
}

func run(args []string, commands commandRunner, stdout *os.File) error {
	if len(args) == 1 && args[0] == "version" {
		fmt.Fprintln(stdout, version)
		return nil
	}
	if len(args) >= 3 && args[0] == "selfhost" && args[1] == "issue" {
		models, err := parseModelFlags(args[3:])
		if err != nil {
			return err
		}
		return selfhostIssueWithModels(args[2], models, commands, stdout)
	}
	if len(args) == 4 && args[0] == "selfhost" && args[1] == "resume" && args[2] == "issue" {
		return selfhostResume(args[3], commands, stdout)
	}
	return fmt.Errorf("usage: zenchron-engineering {version|selfhost issue <number> [--model <name>] [--fallback-model <name> ...]|selfhost resume issue <number>}")
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
