package main

import (
	"bytes"
	"encoding/json"
	"io"
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
	portOf             map[int]string
	slowToAnswer       int // health probes that miss before the dashboard answers
	healthChecks       int
	exitAtOnce         bool
	terminated         []int
	opened             []string
	stateDir           string
}

func (h *fakeUpHost) ControllerAnswers(string) bool        { return h.controllerUp }
func (h *fakeUpHost) Alive(r processRecord, _ string) bool { return h.alive[r.PID] }
func (h *fakeUpHost) PortFree(a string) bool               { return !h.busyPorts[a] && !h.answering[a] }
func (h *fakeUpHost) DashboardHealthy(a, _ string) bool {
	h.healthChecks++
	return h.answering[a] && h.healthChecks > h.slowToAnswer
}
func (h *fakeUpHost) Open(url string) error { h.opened = append(h.opened, url); return nil }
func (h *fakeUpHost) Terminate(pid int) error {
	h.terminated = append(h.terminated, pid)
	delete(h.answering, h.portOf[pid]) // a stopped dashboard frees its port
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

func TestUpReplacesADashboardFromAnEarlierControllerGeneration(t *testing.T) {
	host, args := newUpFixture(t)
	host.controllerUp = true
	host.controllerBinary = "/adopted/main-2/zenchron-engineering"
	if err := writeProcessRecord(host.stateDir, serviceControlPlane, processRecord{PID: 4242, Listen: "127.0.0.1:8789", Executable: "/adopted/main-1/zenchron-engineering"}); err != nil {
		t.Fatal(err)
	}
	host.alive[4242] = true
	host.answering["127.0.0.1:8789"] = true
	host.portOf = map[int]string{4242: "127.0.0.1:8789"}
	var out bytes.Buffer
	if _, err := upCommand(append(args, "--no-open"), host, fastTiming, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(host.terminated) != 1 || host.terminated[0] != 4242 {
		t.Fatalf("the old-generation dashboard was not stopped: %v", host.terminated)
	}
	if len(host.spawnedExecutables) != 1 || host.spawnedExecutables[0] != host.controllerBinary {
		t.Fatalf("replacement ran %v, want the new controller's binary", host.spawnedExecutables)
	}
	if !strings.Contains(out.String(), "127.0.0.1:8789") {
		t.Fatalf("the replacement should keep the operator's address:\n%s", out.String())
	}
}

// A live recorded dashboard that misses a health probe (a loaded machine, a slow
// start) is waited for, never replaced by a second instance on another port.
func TestUpWaitsForALiveDashboardThatIsSlowToAnswer(t *testing.T) {
	host, args := newUpFixture(t)
	host.controllerUp = true
	if err := writeProcessRecord(host.stateDir, serviceControlPlane, processRecord{PID: 4242, Listen: "127.0.0.1:8789"}); err != nil {
		t.Fatal(err)
	}
	host.alive[4242], host.answering["127.0.0.1:8789"], host.slowToAnswer = true, true, 3
	var out bytes.Buffer
	if _, err := upCommand(append(args, "--no-open"), host, upTiming{Ready: time.Second, Poll: time.Millisecond}, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if len(host.spawned) != 0 || len(host.terminated) != 0 {
		t.Fatalf("a slow but live dashboard was replaced: spawned=%v terminated=%v", host.spawned, host.terminated)
	}
	if !strings.Contains(out.String(), "127.0.0.1:8789") {
		t.Fatalf("the operator must be told the existing address:\n%s", out.String())
	}
}

func TestUpNamesALiveDashboardThatNeverAnswersInsteadOfStartingASecond(t *testing.T) {
	host, args := newUpFixture(t)
	host.controllerUp = true
	if err := writeProcessRecord(host.stateDir, serviceControlPlane, processRecord{PID: 4242, Listen: "127.0.0.1:8789"}); err != nil {
		t.Fatal(err)
	}
	host.alive[4242], host.answering["127.0.0.1:8789"] = true, true
	host.slowToAnswer = 1 << 30
	_, err := upCommand(append(args, "--no-open"), host, fastTiming, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "pid 4242") || !strings.Contains(err.Error(), "does not answer") {
		t.Fatalf("err = %v, want one naming the live dashboard that does not answer", err)
	}
	if len(host.spawned) != 0 {
		t.Fatalf("a second dashboard was started beside a live one: %v", host.spawned)
	}
}

func TestStatusCommandExecutableGeneration(t *testing.T) {
	for _, tt := range []struct {
		name       string
		controller string
		dashboard  string
		answers    bool
		wantOlder  bool
	}{
		{"same generation", "/build/current", "/build/current", true, false},
		{"different generation", "/build/current", "/build/previous", true, true},
		{"unknown controller", "", "/build/previous", true, false},
		{"unknown dashboard", "/build/current", "", true, false},
		{"both unknown", "", "", true, false},
		{"controller unavailable", "/build/current", "/build/previous", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, args := newUpFixture(t)
			host.controllerUp = tt.answers
			host.controllerBinary = tt.controller
			host.alive[42] = true
			host.answering["127.0.0.1:8787"] = true
			if err := writeProcessRecord(host.stateDir, serviceControlPlane, processRecord{
				PID: 42, Listen: "127.0.0.1:8787", Executable: tt.dashboard,
			}); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			if code, err := statusCommand(args, host, &out); err != nil || code != runtime.ExitCompleted {
				t.Fatalf("status = %d, %v\n%s", code, err, out.String())
			}
			if got := strings.Contains(out.String(), "(executable "); got != (tt.answers && tt.controller != "") {
				t.Fatalf("controller generation visibility = %v:\n%s", got, out.String())
			}
			if tt.answers && tt.controller != "" && !strings.Contains(out.String(), "(executable "+tt.controller+")") {
				t.Fatalf("missing serving controller executable:\n%s", out.String())
			}
			if got := strings.Contains(out.String(), "built for an earlier generation; `up` will replace it"); got != tt.wantOlder {
				t.Fatalf("earlier generation claim = %v, want %v:\n%s", got, tt.wantOlder, out.String())
			}
			if len(host.spawned) != 0 || len(host.terminated) != 0 || len(host.opened) != 0 {
				t.Fatalf("status changed the process lifecycle: %+v", host)
			}
		})
	}
}

func TestStatusJSON(t *testing.T) {
	for _, tt := range []struct {
		name, controller, dashboard, generation string
		running, recorded, answers              bool
	}{
		{"current", "/current", "/current", "current", true, true, true},
		{"earlier", "/current", "/old", "earlier", true, true, true},
		{"unknown controller", "", "/old", "unknown", true, true, true},
		{"unknown dashboard", "/current", "", "unknown", true, true, true},
		{"both unknown", "", "", "unknown", true, true, true},
		{"unavailable controller", "/current", "/old", "unknown", true, true, false},
		{"not running", "", "", "unknown", false, false, false},
		{"stale records", "/current", "/old", "unknown", false, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			host, args := newUpFixture(t)
			host.controllerUp, host.controllerBinary = tt.answers, tt.controller
			if tt.recorded {
				for service, record := range map[string]processRecord{
					serviceServe:        {PID: 41, Executable: "/stale"},
					serviceControlPlane: {PID: 42, Listen: "127.0.0.1:8787", Executable: tt.dashboard},
				} {
					if err := writeProcessRecord(host.stateDir, service, record); err != nil {
						t.Fatal(err)
					}
				}
			}
			host.alive[41] = tt.answers && tt.recorded // the recorded supervisor is the live one
			host.alive[42], host.answering["127.0.0.1:8787"] = tt.running, tt.running
			var out bytes.Buffer
			code, err := statusCommand(append(args, "--json"), host, &out)
			if err != nil || code != runtime.ExitCompleted {
				t.Fatalf("status = %d, %v", code, err)
			}
			decoder := json.NewDecoder(&out)
			var got map[string]map[string]any
			if err := decoder.Decode(&got); err != nil {
				t.Fatal(err)
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				t.Fatalf("extra output: %v", err)
			}
			supervisor, dashboard := got["supervisor"], got["dashboard"]
			if supervisor["running"] != tt.answers || dashboard["running"] != tt.running || dashboard["generation"] != tt.generation {
				t.Fatalf("status: %#v", got)
			}
			var pid, executable, address, dashboardPID any
			if tt.answers && tt.recorded {
				pid = float64(41)
			}
			if tt.answers && tt.controller != "" {
				executable = tt.controller
			}
			if tt.running {
				address, dashboardPID = "127.0.0.1:8787", float64(42)
			}
			for _, check := range []struct {
				object map[string]any
				key    string
				want   any
			}{
				{supervisor, "pid", pid}, {supervisor, "executable", executable},
				{dashboard, "address", address}, {dashboard, "pid", dashboardPID},
			} {
				value, exists := check.object[check.key]
				if !exists || value != check.want {
					t.Errorf("%s = %#v (exists %v), want %#v", check.key, value, exists, check.want)
				}
			}
			if len(host.spawned)+len(host.terminated)+len(host.opened) != 0 {
				t.Fatal("status changed lifecycle")
			}
		})
	}
}

func TestJSONFlagIsStatusOnly(t *testing.T) {
	host, args := newUpFixture(t)
	if code, err := upCommand(append(args, "--json"), host, fastTiming, io.Discard); err == nil || code != runtime.ExitInvalid {
		t.Fatalf("up: %d, %v", code, err)
	}
	if code, err := downCommand(append(args, "--json"), host, fastTiming, io.Discard); err == nil || code != runtime.ExitInvalid {
		t.Fatalf("down: %d, %v", code, err)
	}
}

func TestStatusJSONRunningSupervisorWithoutRecord(t *testing.T) {
	host, args := newUpFixture(t)
	host.controllerUp = true
	var out bytes.Buffer
	if code, err := statusCommand(append(args, "--json"), host, &out); err != nil || code != runtime.ExitCompleted {
		t.Fatalf("status = %d, %v", code, err)
	}
	var got map[string]map[string]any
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["supervisor"]["running"] != true || got["supervisor"]["pid"] != nil || got["supervisor"]["executable"] != nil {
		t.Fatalf("unknown supervisor details must be null: %s", out.String())
	}
}

// A controller that answers does not prove the recorded pid is the one answering.
// A stale record is reported as stale (pid null), never presented as the supervisor.
func TestStatusDoesNotClaimAStaleRecordedPidForTheAnsweringSupervisor(t *testing.T) {
	for name, alive := range map[string]bool{"stale record": false, "live record": true} {
		t.Run(name, func(t *testing.T) {
			host, args := newUpFixture(t)
			host.controllerUp = true
			if err := writeProcessRecord(host.stateDir, serviceServe, processRecord{PID: 41}); err != nil {
				t.Fatal(err)
			}
			host.alive[41] = alive
			var text, jsonOut bytes.Buffer
			if _, err := statusCommand(args, host, &text); err != nil {
				t.Fatal(err)
			}
			if _, err := statusCommand(append(args, "--json"), host, &jsonOut); err != nil {
				t.Fatal(err)
			}
			var got map[string]map[string]any
			if err := json.Unmarshal(jsonOut.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			claimed := strings.Contains(text.String(), "(pid 41)")
			if claimed != alive || (got["supervisor"]["pid"] != nil) != alive {
				t.Fatalf("alive=%v: text claims pid=%v, json pid=%v\n%s", alive, claimed, got["supervisor"]["pid"], text.String())
			}
			if !alive && !strings.Contains(text.String(), "pid unknown") {
				t.Fatalf("a stale record must be named as such:\n%s", text.String())
			}
		})
	}
}
