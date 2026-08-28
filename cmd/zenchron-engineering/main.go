package main

import (
	"fmt"
	"os"
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
	if len(args) == 3 && args[0] == "selfhost" && args[1] == "issue" {
		return selfhostIssue(args[2], commands, stdout)
	}
	if len(args) == 4 && args[0] == "selfhost" && args[1] == "resume" && args[2] == "issue" {
		return selfhostResume(args[3], commands, stdout)
	}
	return fmt.Errorf("usage: zenchron-engineering {version|selfhost issue <number>|selfhost resume issue <number>}")
}
