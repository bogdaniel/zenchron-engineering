package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// fakeUpHost is the machine boundary of `up`: processes, ports, the browser.
type fakeUpHost struct {
	controllerUp       bool
	alive              map[int]bool
	busyPorts          map[string]bool
	answering          map[string]bool
	spawned            [][]string
	spawnedExecutables []string
	controllerBinary   string
	exitAtOnce         bool
	terminated         []int
	opened             []string
	stateDir           string
}

func (h *fakeUpHost) ControllerAnswers(string) bool        { return h.controllerUp }
func (h *fakeUpHost) Alive(r processRecord, _ string) bool { return h.alive[r.PID] }
func (h *fakeUpHost) PortFree(a string) bool               { return !h.busyPorts[a] && !h.answering[a] }
func (h *fakeUpHost) DashboardHealthy(a, _ string) bool    { return h.answering[a] }
func (h *fakeUpHost) Open(url string) error                { h.opened = append(h.opened, url); return nil }
func (h *fakeUpHost) Terminate(pid int) error {
	h.terminated = append(h.terminated, pid)
	h.alive[pid] = false
	return nil
}

// Spawn behaves like the real services: a started process becomes ready and
// records itself, unless it is configured to die at once.
func (h *fakeUpHost) ControllerExecutable(string) string { return h.controllerBinary }

func (h *fakeUpHost) Spawn(executable, _ string, args []string) (int, <-chan struct{}, error) {
	h.spawned = append(h.spawned, args)
	h.spawnedExecutables = append(h.spawnedExecutables, executable)
	exited := make(chan struct{})
	if h.exitAtOnce {
		close(exited)
		return 100, exited, nil
	}
	pid := 100 + len(h.spawned)
	h.alive[pid] = true
	switch args[0] {
	case serviceServe:
		h.controllerUp = true
	case serviceControlPlane:
		address := args[4]
		h.answering[address] = true
		_ = writeProcessRecord(h.stateDir, serviceControlPlane, processRecord{PID: pid, Listen: address})
	}
	return pid, exited, nil
}

func newUpFixture(t *testing.T) (*fakeUpHost, []string) {
	t.Helper()
	stateDir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte(`{"state_dir": "`+stateDir+`", "project_model_path": "`+filepath.Join(stateDir, "m.json")+`", "policy_path": "`+filepath.Join(stateDir, "p.json")+`", "assurance": {"image": "sha256:`+strings.Repeat("a", 64)+`", "dependency_cache_dir": "`+stateDir+`"}, "agents": {"codex": {"kind": "codex_cli", "trust_mode": "operator_trusted"}}, "default_agent": "codex", "github": {"credential_mode": "none"}, "budgets": {"wall_limit_seconds": 1800, "max_execution_attempts": 2, "max_remediation_attempts": 2, "max_assurance_attempts": 2}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	host := &fakeUpHost{alive: map[int]bool{}, busyPorts: map[string]bool{}, answering: map[string]bool{}, stateDir: stateDir}
	return host, []string{"--config", configPath}
}

var fastTiming = upTiming{Ready: 50 * time.Millisecond, Poll: time.Millisecond}

func TestUpStartsEverythingThenReusesIt(t *testing.T) {
	host, args := newUpFixture(t)
	var out bytes.Buffer
	if code, err := upCommand(args, host, fastTiming, &out); err != nil || code != runtime.ExitCompleted {
		t.Fatalf("up = %d, %v\n%s", code, err, out.String())
	}
	if len(host.spawned) != 2 || host.spawned[0][0] != serviceServe || host.spawned[1][0] != serviceControlPlane {
		t.Fatalf("first up spawned %v, want serve then control-plane", host.spawned)
	}
	if want := "http://127.0.0.1:8787/"; len(host.opened) != 1 || host.opened[0] != want {
		t.Fatalf("opened %v, want %s", host.opened, want)
	}

	if code, err := upCommand(args, host, fastTiming, &out); err != nil || code != runtime.ExitCompleted {
		t.Fatalf("second up = %d, %v", code, err)
	}
	if len(host.spawned) != 2 {
		t.Fatalf("second up spawned again: %v", host.spawned)
	}
}

func TestUpMovesThePortOfAnUnrelatedProgramInsteadOfFailing(t *testing.T) {
	host, args := newUpFixture(t)
	host.busyPorts["127.0.0.1:8787"] = true
	var out bytes.Buffer
	if _, err := upCommand(append(args, "--no-open"), host, fastTiming, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "http://127.0.0.1:8788/") {
		t.Fatalf("dashboard should move to 8788:\n%s", out.String())
	}
}

func TestUpRefusesABusyExplicitAddress(t *testing.T) {
	host, args := newUpFixture(t)
	host.busyPorts["127.0.0.1:9000"] = true
	_, err := upCommand(append(args, "--listen", "127.0.0.1:9000"), host, fastTiming, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("err = %v, want an in-use refusal", err)
	}
}

func TestUpReportsASupervisorThatDiesAtStartup(t *testing.T) {
	host, args := newUpFixture(t)
	host.exitAtOnce = true
	_, err := upCommand(args, host, fastTiming, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "serve.log") {
		t.Fatalf("err = %v, want a pointer to the supervisor log", err)
	}
	if len(host.spawned) != 1 {
		t.Fatalf("a dead supervisor must not start the dashboard: %v", host.spawned)
	}
}

func TestUpReplacesARecordWhosePidWasRecycled(t *testing.T) {
	host, args := newUpFixture(t)
	if err := writeProcessRecord(host.stateDir, serviceControlPlane, processRecord{PID: 4242, Listen: "127.0.0.1:8787"}); err != nil {
		t.Fatal(err)
	}
	host.alive[4242] = false                // the pid now belongs to an unrelated program
	host.answering["127.0.0.1:8787"] = true // and something else now answers on its port
	if _, err := upCommand(append(args, "--no-open"), host, fastTiming, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if got := host.spawned[len(host.spawned)-1][0]; got != serviceControlPlane {
		t.Fatalf("stale dashboard record was trusted; last spawn %q", got)
	}
}

func TestDownStopsRecordedServicesAndRefusesAnUnrecordedSupervisor(t *testing.T) {
	host, args := newUpFixture(t)
	if _, err := upCommand(append(args, "--no-open"), host, fastTiming, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if err := writeProcessRecord(host.stateDir, serviceServe, processRecord{PID: 101}); err != nil {
		t.Fatal(err)
	}
	if code, err := downCommand(args, host, fastTiming, &bytes.Buffer{}); err != nil || code != runtime.ExitCompleted {
		t.Fatalf("down = %d, %v", code, err)
	}
	if len(host.terminated) != 2 {
		t.Fatalf("terminated %v, want both services", host.terminated)
	}

	hand, handArgs := newUpFixture(t)
	hand.controllerUp = true // a supervisor that never recorded itself
	_, err := downCommand(handArgs, hand, fastTiming, &bytes.Buffer{})
	if err == nil || len(hand.terminated) != 0 {
		t.Fatalf("down must not guess at an unrecorded supervisor: err=%v terminated=%v", err, hand.terminated)
	}
}

func TestProcessRecordIsClearedOnlyByItsOwner(t *testing.T) {
	dir := t.TempDir()
	if err := writeProcessRecord(dir, serviceServe, processRecord{PID: os.Getpid() + 1}); err != nil {
		t.Fatal(err)
	}
	clearProcessRecord(dir, serviceServe) // a predecessor exiting after its successor took over
	if _, ok, _ := readProcessRecord(dir, serviceServe); !ok {
		t.Fatal("a predecessor deleted its successor's record")
	}
}

func TestUpRunsTheDashboardAsTheServingControllersBinary(t *testing.T) {
	host, args := newUpFixture(t)
	host.controllerUp = true
	host.controllerBinary = "/adopted/main-1/zenchron-engineering"
	if _, err := upCommand(append(args, "--no-open"), host, fastTiming, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(host.spawnedExecutables) != 1 || host.spawnedExecutables[0] != host.controllerBinary {
		t.Fatalf("dashboard started with %v, want the controller's own binary", host.spawnedExecutables)
	}
}
