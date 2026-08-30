//go:build linux

package runtime

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// On Linux /proc supplies both the parent relationship and a non-reusable
// process identity (starttime). Sampling begins as soon as Start succeeds and
// is joined before return, so cancellation cannot leave a tracker goroutine
// behind. We retain every observed descendant because a detached child can be
// reparented immediately after its parent receives SIGTERM.
type ownedProcessSet struct {
	root int
	mu   sync.Mutex
	seen map[int]string
	stop chan struct{}
	done chan struct{}
}

func newOwnedProcessSet(root int) *ownedProcessSet {
	p := &ownedProcessSet{root: root, seen: map[int]string{root: ""}, stop: make(chan struct{}), done: make(chan struct{})}
	p.capture()
	go p.track()
	return p
}

func (p *ownedProcessSet) track() {
	defer close(p.done)
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			p.capture()
		}
	}
}

func (p *ownedProcessSet) Close() {
	close(p.stop)
	<-p.done
}

func (p *ownedProcessSet) GracefulStop(grace time.Duration) {
	// Capture before touching the process group. This is the critical ordering:
	// it preserves the identities of descendants that will be reparented when
	// their intermediate shell exits. Let their parent reap a graceful child
	// before stopping that parent; otherwise a host init that does not promptly
	// reap could retain a non-mutating zombie under the detached PID.
	p.capture()
	p.signalDescendants(syscall.SIGTERM)
	p.waitForDescendants(grace / 4)
	_ = syscall.Kill(-p.root, syscall.SIGTERM)
}

func (p *ownedProcessSet) ForceKill() {
	p.capture()
	p.signalDescendants(syscall.SIGKILL)
	p.waitForDescendants(5 * time.Millisecond)
	_ = syscall.Kill(-p.root, syscall.SIGKILL)
}

func (p *ownedProcessSet) signalDescendants(signal syscall.Signal) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for pid, startTime := range p.seen {
		if pid == p.root || startTime == "" {
			continue
		}
		if current, ok := linuxProcessStartTime(pid); ok && current == startTime {
			_ = syscall.Kill(pid, signal)
		}
	}
}

func (p *ownedProcessSet) waitForDescendants(limit time.Duration) {
	if limit <= 0 {
		return
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !p.descendantsRemain() {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func (p *ownedProcessSet) descendantsRemain() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for pid, startTime := range p.seen {
		if pid != p.root && startTime != "" {
			if current, ok := linuxProcessStartTime(pid); ok && current == startTime {
				return true
			}
		}
	}
	return false
}

func (p *ownedProcessSet) capture() {
	processes := linuxProcessTable()
	owned := map[int]bool{p.root: true}
	changed := true
	for changed {
		changed = false
		for pid, process := range processes {
			if owned[process.ppid] && !owned[pid] {
				owned[pid] = true
				changed = true
			}
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for pid := range owned {
		if process, ok := processes[pid]; ok {
			p.seen[pid] = process.startTime
		}
	}
}

type linuxProcess struct {
	ppid      int
	startTime string
}

func linuxProcessTable() map[int]linuxProcess {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	processes := make(map[int]linuxProcess, len(entries))
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		if ppid, startTime, ok := linuxProcessStat(pid); ok {
			processes[pid] = linuxProcess{ppid: ppid, startTime: startTime}
		}
	}
	return processes
}

func linuxProcessStartTime(pid int) (string, bool) {
	_, startTime, ok := linuxProcessStat(pid)
	return startTime, ok
}

func linuxProcessStat(pid int) (int, string, bool) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, "", false
	}
	// comm is parenthesized and may itself contain spaces or ')'; the final ')'
	// marks the start of fixed-position fields.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return 0, "", false
	}
	fields := strings.Fields(string(data[end+1:]))
	// fields[0] is state (field 3), [1] PPID (field 4), and [19] starttime
	// (field 22) in proc(5).
	if len(fields) <= 19 {
		return 0, "", false
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, "", false
	}
	return ppid, fields[19], true
}
