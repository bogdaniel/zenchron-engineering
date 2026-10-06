package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

type verificationExecution struct {
	Scheduler Scheduler
	Parent    ExecutionAttemptRef
	StateDir  string
}

type verificationExecutionKey struct{}
type verificationOwnerFileKey struct{}

func withVerificationExecution(ctx context.Context, s Scheduler, parent ExecutionAttemptRef, stateDir string) context.Context {
	return context.WithValue(ctx, verificationExecutionKey{}, verificationExecution{s, parent, stateDir})
}

func verificationExecutionFrom(ctx context.Context) (verificationExecution, bool) {
	v, ok := ctx.Value(verificationExecutionKey{}).(verificationExecution)
	return v, ok
}

// begin waits inside the physical provider attempt. It does not plan, start,
// restore or refund an engineering operation. Cancellation ends this wait.
func (v verificationExecution) begin(ctx context.Context, sandbox *VerificationSandbox) (VerificationPermit, *ControllerInstanceLock, error) {
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		return VerificationPermit{}, nil, err
	}
	id := hex.EncodeToString(identity[:])
	owner := NewRuntimeOwner() + "-tool-" + id
	lock, err := AcquireControllerInstanceLock(v.StateDir, owner)
	if err != nil {
		return VerificationPermit{}, nil, err
	}
	p, err := v.Scheduler.requestVerification(v.Parent, id, owner, sandbox, v.StateDir)
	if err != nil {
		return p, nil, errors.Join(err, lock.Release())
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var acquired bool
		acquired, err = v.Scheduler.AcquireVerification(p)
		if err != nil {
			break
		}
		if acquired {
			return p, lock, nil
		}
		select {
		case <-ctx.Done():
			err = ctx.Err()
		case <-ticker.C:
			continue
		}
		break
	}
	return p, nil, errors.Join(err, lock.Release(), v.Scheduler.ReleaseVerification(p))
}

func (v verificationExecution) finish(p VerificationPermit, lock *ControllerInstanceLock, sandbox *DockerSandbox) error {
	// Never unlink the ownership path: descendants can still hold the inherited
	// file descriptor after their wrapper dies or returns.
	err := lock.file.Close()
	lock.file = nil
	if NewLockOwnerLiveness(v.StateDir).Alive(p.ToolOwner) {
		return errors.Join(err, fmt.Errorf("verification execution still owns its process lock"))
	}
	if sandbox != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, cleanupErr := sandbox.ReconcileDockerOperation(ctx); cleanupErr != nil {
			return errors.Join(err, cleanupErr)
		}
	}
	return errors.Join(err, v.Scheduler.ReleaseVerification(p))
}
