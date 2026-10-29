package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func verificationBrokerCommand() []string {
	path, err := os.Executable()
	if err != nil {
		return nil
	}
	return []string{path, "__verification-tool"}
}

func verificationTool(args []string) (int, error) {
	if len(args) < 2 {
		return exitUsage, fmt.Errorf("verification tool requires its runtime grant and executable")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	out, err := runtime.RunVerificationTool(ctx, args[0], args[1], args[2:])
	_, stdoutErr := os.Stdout.Write(out.Stdout)
	_, stderrErr := os.Stderr.Write(out.Stderr)
	if out.ExitCode != 0 {
		return out.ExitCode, errors.Join(stdoutErr, stderrErr)
	}
	if err != nil {
		return 1, errors.Join(err, stdoutErr, stderrErr)
	}
	return 0, errors.Join(stdoutErr, stderrErr)
}
