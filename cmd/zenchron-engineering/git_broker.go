package main

// The #241 broker entry point.
//
// A provider's `git` resolves to a runtime-owned shim, and the shim execs this
// subcommand. It is deliberately a hidden command rather than part of the
// operator surface: nobody types it, it takes only runtime-owned arguments, and
// the one thing it must do is be the same binary the operator is running.
//
// The decision itself is runtime.BrokerGitCommand. Nothing is decided here.

import (
	"fmt"
	"os"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// gitBrokerSubcommand is the argv the generated shim invokes. It is prefixed so
// it cannot collide with an operator command, and so a reader of a process
// listing can see what it is.
const gitBrokerSubcommand = "__git-broker"

// gitBrokerCommand is the broker argv the composition root hands the provider
// boundary: this controller's own executable, and the hidden subcommand.
//
// An executable that cannot be resolved yields NO broker, which the provider
// boundary reports as an unguarded invocation. That is the honest failure: a
// guard whose enforcer could not be named is not a guard, and claiming one
// would be worse than recording its absence.
func gitBrokerCommand() []string {
	executable, err := os.Executable()
	if err != nil {
		return nil
	}
	return []string{executable, gitBrokerSubcommand}
}

// gitBroker parses the shim's argv and performs the decision.
//
// The flags are runtime-owned and the provider's own argv is everything after
// the `--` separator, so no provider-chosen string can be read as a flag to
// this command.
func gitBroker(args []string) (int, error) {
	var candidate, scratch, refusalLog string
	for len(args) > 0 {
		switch args[0] {
		case "--candidate":
			if len(args) < 2 {
				return exitUsage, fmt.Errorf("--candidate requires the candidate workspace")
			}
			candidate, args = args[1], args[2:]
		case "--scratch":
			if len(args) < 2 {
				return exitUsage, fmt.Errorf("--scratch requires the attempt's runtime-owned temp root")
			}
			scratch, args = args[1], args[2:]
		case "--refusal-log":
			if len(args) < 2 {
				return exitUsage, fmt.Errorf("--refusal-log requires a path")
			}
			refusalLog, args = args[1], args[2:]
		case "--":
			// Everything after this is the provider's Git argv.
			code, err := runtime.BrokerGitCommand(candidate, scratch, refusalLog, args[1:], os.Stdout, os.Stderr)
			if err != nil {
				return code, err
			}
			return code, nil
		default:
			return exitUsage, fmt.Errorf("unexpected brokered Git argument")
		}
	}
	return exitUsage, fmt.Errorf("brokered Git requires the provider argv after --")
}
