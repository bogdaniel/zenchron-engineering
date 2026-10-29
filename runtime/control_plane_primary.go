package runtime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// controlPlanePrimaryLockName is the one path a state directory's primary
// dashboard (Control Plane) is claimed at. Like the controller role it is an
// exclusive advisory lock the kernel drops when its holder dies, so a crashed
// dashboard leaves no stale claim and nothing has to read a pid to decide.
const controlPlanePrimaryLockName = "control-plane-primary"

// ErrControlPlanePrimaryHeld reports that another live process is already the
// primary dashboard of this state directory.
var ErrControlPlanePrimaryHeld = errors.New("another live process is the primary control plane of this state directory")

// ControlPlanePrimary is the held claim.
type ControlPlanePrimary struct{ file *os.File }

// AcquireControlPlanePrimary claims the primary dashboard role for stateDir,
// or returns ErrControlPlanePrimaryHeld. The claim is taken BEFORE anything is
// bound or recorded, so two dashboards starting at once cannot both proceed:
// reading an existing process record first would leave exactly that race.
//
// ponytail: where the platform has no advisory lock (tryLockFile errors) the
// claim is refused rather than guessed; serve already requires the same lock
// there, so nothing that worked before stops working in practice.
func AcquireControlPlanePrimary(stateDir string) (*ControlPlanePrimary, error) {
	path := filepath.Join(stateDir, "locks", controlPlanePrimaryLockName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("claim the primary control plane: %w", err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("claim the primary control plane: %w", err)
	}
	locked, err := tryLockFile(file, true)
	if err == nil && !locked {
		err = ErrControlPlanePrimaryHeld
	}
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	return &ControlPlanePrimary{file: file}, nil
}

// Release gives the claim up. The lock file is never unlinked: authority
// attaches to the inode, exactly as for the controller role anchor.
func (p *ControlPlanePrimary) Release() error {
	if p == nil || p.file == nil {
		return nil
	}
	file := p.file
	p.file = nil
	return file.Close()
}
