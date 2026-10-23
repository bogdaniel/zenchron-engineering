package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// sandboxStderrTailBytes bounds how much of a docker step's own output an
// operator is shown. The tail, not the head, because a daemon states its
// refusal last.
const sandboxStderrTailBytes = 1000

// SandboxStepError is one Docker lifecycle step failing: create, start,
// inspect, wait or remove.
//
// It exists because the Docker CLI reports almost every infrastructure
// refusal as a bare "exit status 1" and puts the reason on stderr. A name
// conflict on `docker create` is exactly that, and runContainer used to drop
// the output, so a controller's refused upgrade read only "exit status 1" and
// the operator had to repeat the build by hand to learn anything (#674).
//
// Step, OperationID and Container say WHERE it failed and WHICH runtime-owned
// operation it belongs to; Stderr is the sanitised, bounded tail of what
// Docker said. The wrapped error is unchanged, so errors.Is/As behave as they
// did before.
type SandboxStepError struct {
	Step        string
	OperationID string
	Container   string
	Stderr      string
	Err         error
}

func (e *SandboxStepError) Error() string {
	text := fmt.Sprintf("docker %s failed for operation %s (container %s): %v", e.Step, e.OperationID, e.Container, e.Err)
	if e.Stderr != "" {
		text += ": " + e.Stderr
	}
	return text
}

func (e *SandboxStepError) Unwrap() error { return e.Err }

// stepError labels a failed Docker step of the operation `record` names, or
// returns nil for a successful one so a call site can return it unconditionally.
func (s DockerSandbox) stepError(step string, record dockerOperationRecord, out CommandOutput, err error) error {
	return s.dockerStepError(step, record.ContainerName, out, err)
}

// dockerStepError is the same for a step that knows only the container's name
// (inspect, remove): the output Docker produced for THAT command is kept, so a
// failing `docker inspect` or `docker rm` says why instead of "exit status 1".
func (s DockerSandbox) dockerStepError(step, container string, out CommandOutput, err error) error {
	if err == nil {
		return nil
	}
	return &SandboxStepError{Step: step, OperationID: s.OperationID, Container: container, Stderr: sandboxStepDetail(out), Err: err}
}

// sandboxStepDetail is the operator-facing account of one Docker step's
// output (stdout when stderr is empty): every control character removed so a
// hostile or merely colourful message cannot drive a terminal, whitespace
// collapsed, the transcript redactor applied, and only THEN the last
// sandboxStderrTailBytes kept. The order matters twice over: redaction
// replaces a secret with a longer marker, so cutting first and redacting after
// could push the daemon's actionable last sentence out of the bound; and a
// secret split by the cut could survive as an unrecognisable half.
func sandboxStepDetail(out CommandOutput) string {
	text := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(compilerFailureDetail(out), ""))
	text = string(redactTranscript([]byte(strings.Join(strings.Fields(text), " "))))
	if len(text) > sandboxStderrTailBytes {
		text = strings.ToValidUTF8(text[len(text)-sandboxStderrTailBytes:], "")
	}
	return text
}

// transientSandboxFailure reports whether a failed sandbox run is worth one
// more bounded attempt.
//
// ONE RULE, ONE PLACE. The workload having run and failed (a compile error, a
// test failure) is a verdict about the code and is never retried; the
// caller's own cancellation is never retried. What remains is the Docker
// lifecycle itself failing before or around the workload - the daemon not
// answering, a create/start/inspect/wait/remove refused - which is
// infrastructure. Docker types none of those refusals, so the classification
// is by STEP, not by message text. A deterministic infrastructure error (an
// invalid mount, a missing image) is retried too, but only within the small
// bound below and with its stderr shown every time, so the cost of that
// coarseness is seconds, not a hidden failure.
func transientSandboxFailure(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var workload *containerExitError
	if errors.As(err, &workload) {
		return false
	}
	if errors.Is(err, ErrSandboxUnavailable) {
		return true
	}
	var step *SandboxStepError
	return errors.As(err, &step)
}
