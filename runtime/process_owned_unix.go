//go:build darwin || dragonfly || freebsd || netbsd || openbsd || solaris

package runtime

import (
	"syscall"
	"time"
)

// ownedProcessSet is deliberately scoped to one direct child created with a
// fresh process group. Platforms without Linux's reliable descendant identity
// source retain process-group containment.
type ownedProcessSet struct{ root int }

func newOwnedProcessSet(root int) *ownedProcessSet { return &ownedProcessSet{root: root} }
func (p *ownedProcessSet) Close()                  {}
func (p *ownedProcessSet) GracefulStop(time.Duration) {
	_ = syscall.Kill(-p.root, syscall.SIGTERM)
}
func (p *ownedProcessSet) ForceKill() {
	_ = syscall.Kill(-p.root, syscall.SIGKILL)
}
