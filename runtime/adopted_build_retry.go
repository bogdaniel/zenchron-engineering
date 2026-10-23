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
// The caller holds the build's exclusive claim (claimAdoptedBuild), which is
// what makes everything below safe.
//
// Two different failures need two different responses:
//   - A failure BEFORE a container exists (the daemon is not ready, its
//     identity is unavailable) created nothing, so there is nothing to
//     reconcile: it backs off and tries again, with no cleanup.
//   - A Docker step failing AFTER the operation began (create, start, ...) may
//     have left this operation's container behind. Before the step runs again
//     that container is reconciled: removed if it exists and is not running.
//     If the daemon cannot answer for the reconciliation, the container's state
//     is ambiguous, so the step is NOT run again; the reconciliation itself is
//     what is retried, within the same bound, and exhausting the bound refuses
//     the build rather than guessing.
//
// Nothing else changes between attempts: same image, same arguments, same
// pinned inputs, so the trust verification around the build is exactly what a
// first-try success would have had.
func runAdoptedStep(ctx context.Context, sandbox DockerSandbox, step string, run func() (CommandOutput, error)) (CommandOutput, error) {
	needsReconcile := false
	for attempt := 1; ; attempt++ {
		var out CommandOutput
		var err error
		ready := true
		if needsReconcile {
			if err = sandbox.releaseStaleContainer(ctx); err != nil {
				err = fmt.Errorf("the previous attempt's container could not be reconciled: %w", err)
				ready = false
			} else {
				needsReconcile = false
			}
		}
		if ready {
			out, err = run()
			if err == nil {
				return out, nil
			}
			needsReconcile = containerMayExist(err)
		}
		transient := transientSandboxFailure(err)
		if !transient || attempt > len(adoptedBuildBackoff) || ctx.Err() != nil {
			return out, newAdoptedBuildFailure(step, attempt, transient, sandbox.OperationID, out, err)
		}
		timer := time.NewTimer(adoptedBuildBackoff[attempt-1])
		select {
		case <-ctx.Done():
			timer.Stop()
			return out, newAdoptedBuildFailure(step, attempt, transient, sandbox.OperationID, out, errors.Join(err, ctx.Err()))
		case <-timer.C:
		}
	}
}

// containerMayExist reports whether a failed step happened after the Docker
// operation began, so its container may exist. A daemon that was never ready
// produced a plain ErrSandboxUnavailable and created nothing.
func containerMayExist(err error) bool {
	var step *SandboxStepError
	return errors.As(err, &step)
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
//
// It is only ever called by the holder of the build's exclusive claim, so a
// container that exists and is not running is the leftover of an attempt that
// is no longer alive; without the claim it could be a concurrent build's
// container that was created and not yet started.
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
