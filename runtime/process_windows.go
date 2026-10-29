//go:build windows

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"time"
)

// M0 has no Windows process-group primitive wired to a Job Object. Refusing
// this adapter is safer than claiming Docker-client cancellation contains a
// child group on that platform.
func runBoundedProcess(context.Context, *exec.Cmd, time.Duration) (TerminationOwner, error) {
	return OwnerUndecided, fmt.Errorf("bounded process-group sandbox unavailable on windows")
}

// releaseCandidateWriter only drops the hold: no bounded process starts here.
func releaseCandidateWriter(lock *os.File, _ int, _ time.Duration) bool {
	_ = lock.Close()
	return false
}
