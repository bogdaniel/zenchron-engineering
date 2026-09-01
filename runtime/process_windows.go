//go:build windows

package runtime

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// M0 has no Windows process-group primitive wired to a Job Object. Refusing
// this adapter is safer than claiming Docker-client cancellation contains a
// child group on that platform.
func runBoundedProcess(context.Context, *exec.Cmd, time.Duration) error {
	return fmt.Errorf("bounded process-group sandbox unavailable on windows")
}
