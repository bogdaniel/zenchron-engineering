package runtime

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// adoptedBuildBackoff is the wait before each RETRY of a sandbox step that
// failed for an infrastructure reason; its length is the number of retries, so
// a step runs at most len+1 times. Small on purpose: this absorbs a daemon
// hiccup or a leftover container, not an outage, and the controller already
// waits updateRetryInterval before it rebuilds a refused subject. A variable
// only so a test need not sleep.
var adoptedBuildBackoff = []time.Duration{2 * time.Second, 8 * time.Second}

// AdoptedBuildFailure says WHICH step of an adopted build failed, how many
// times it was tried, which runtime-owned Docker operation it was, and what
// Docker said - the account an operator needs and a bare "exit status 1" never
// gave (#674). Err is the original error and stays reachable through Unwrap.
//
// It describes a refusal. It never makes a build succeed, never relaxes a
// trust check and is not consulted by one.
type AdoptedBuildFailure struct {
	// Step is "toolchain" (the pinned image's `go version` probe) or "build"
	// (the compile container).
	Step     string
	Attempts int
	// Transient is whether the LAST failure was an infrastructure failure, so
	// a refusal with Attempts>1 and Transient=true means the bounded retry
	// was spent, and Transient=false means it was never eligible.
	Transient   bool
	OperationID string
	Container   string
	Stderr      string
	Err         error
}

func (e *AdoptedBuildFailure) Error() string {
	what := "the adopted build container failed"
	if e.Step == "toolchain" {
		what = "the pinned build toolchain could not be identified"
	}
	text := fmt.Sprintf("%s (%s step, %d attempt(s), operation %s): %v", what, e.Step, e.Attempts, e.OperationID, e.Err)
	// A Docker step error already carries its own stderr; a workload's output
	// (a compile error) arrives separately.
	var step *SandboxStepError
	if e.Stderr != "" && !errors.As(e.Err, &step) {
		text += ": " + e.Stderr
	}
	return text
}

func (e *AdoptedBuildFailure) Unwrap() error { return e.Err }

// runAdoptedStep runs one sandbox step, retrying it a bounded number of times
// ONLY when transientSandboxFailure says the Docker lifecycle itself failed.
//
// Idempotence: before a retry the exact container this operation names is
// removed if (and only if) it exists and is not running - the leftover of an
// interrupted attempt, which is what a `docker create` name conflict is. A
// container that is still running belongs to a live attempt and is never
// touched. Nothing else changes between attempts: same image, same arguments,
// same pinned inputs, so the trust verification around the build is exactly
// what a first-try success would have had.
func runAdoptedStep(ctx context.Context, sandbox DockerSandbox, step string, run func() (CommandOutput, error)) (CommandOutput, error) {
	for attempt := 1; ; attempt++ {
		out, err := run()
		if err == nil {
			return out, nil
		}
		transient := transientSandboxFailure(err)
		failure := func(cause error) error {
			return newAdoptedBuildFailure(step, attempt, transient, sandbox.OperationID, out, cause)
		}
		if !transient || attempt > len(adoptedBuildBackoff) || ctx.Err() != nil {
			return out, failure(err)
		}
		if cleanupErr := sandbox.releaseStaleContainer(ctx); cleanupErr != nil {
			return out, failure(errors.Join(err, fmt.Errorf("the previous attempt's container could not be reconciled: %w", cleanupErr)))
		}
		timer := time.NewTimer(adoptedBuildBackoff[attempt-1])
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, failure(errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
	}
}

func newAdoptedBuildFailure(step string, attempts int, transient bool, operationID string, out CommandOutput, cause error) *AdoptedBuildFailure {
	failure := &AdoptedBuildFailure{Step: step, Attempts: attempts, Transient: transient, OperationID: operationID, Err: cause}
	var docker *SandboxStepError
	if errors.As(cause, &docker) {
		failure.Container, failure.Stderr = docker.Container, docker.Stderr
		return failure
	}
	failure.Stderr = sandboxStepDetail(out)
	return failure
}

// releaseStaleContainer removes this operation's exact container when it
// exists and is not running. Its name is the deterministic one the sandbox
// derives from the operation, image, endpoint and daemon - never a pattern -
// so it cannot reach a container that is not this operation's.
func (s DockerSandbox) releaseStaleContainer(ctx context.Context) error {
	record, _, err := s.operationRecord(ctx)
	if err != nil {
		return err
	}
	running, exists, err := s.inspectRunning(record.ContainerName)
	if err != nil || !exists || running {
		return err
	}
	return s.removeExact(record.ContainerName)
}
