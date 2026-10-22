package main

// The process half of `up`, `down` and `status`: what a running service
// writes about itself, and the real host operations `up` performs.
//
// A service records its own pid, not its launcher, because the launcher is not
// the only thing that starts one. A hand-started `serve`, and a successor
// controller that took the role over from its predecessor, are both real
// controllers whose pid the launcher never saw. When the process names itself,
// there is no such thing as an unrecorded one.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
)

const (
	serviceServe        = "serve"
	serviceControlPlane = "control-plane"
)

// processRecord is what a service writes about itself in its state directory.
// Executable and Started pin the identity of the process, not just its pid: a
// recycled pid, even one now running another Zenchron service, matches neither.
type processRecord struct {
	PID        int    `json:"pid"`
	Listen     string `json:"listen,omitempty"`
	Executable string `json:"executable,omitempty"`
	Started    string `json:"started,omitempty"`
}

// selfProcessRecord is the record of this process, with its identity filled in
// as far as the platform can establish it.
func selfProcessRecord(listen string) processRecord {
	record := processRecord{PID: os.Getpid(), Listen: listen}
	record.Executable, _ = os.Executable()
	record.Started = processStartMarker(record.PID)
	return record
}

// processStartMarker is the process's start time as `ps` reports it, or ""
// where the platform cannot say. ponytail: shells out to ps (unix); Windows has
// no marker, so there a recycled pid is caught only by the Alive check.
func processStartMarker(pid int) string {
	if runtime.GOOS == "windows" {
		return ""
	}
	started, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(started))
}

func processRecordPath(stateDir, service string) string {
	return filepath.Join(stateDir, service+".process.json")
}

// writeProcessRecord replaces the record atomically, so a reader never sees a
// torn file.
func writeProcessRecord(stateDir, service string, record processRecord) error {
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := processRecordPath(stateDir, service)
	temporary := path + "." + strconv.Itoa(record.PID) + ".tmp"
	if err := os.WriteFile(temporary, encoded, 0o600); err != nil {
		return fmt.Errorf("record the %s process: %w", service, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("record the %s process: %w", service, err)
	}
	return nil
}

// readProcessRecord reports ok=false when no service has recorded itself. A
// record that exists but cannot be read is an error, never "not running".
func readProcessRecord(stateDir, service string) (processRecord, bool, error) {
	encoded, err := os.ReadFile(processRecordPath(stateDir, service))
	if errors.Is(err, fs.ErrNotExist) {
		return processRecord{}, false, nil
	}
	if err != nil {
		return processRecord{}, false, err
	}
	var record processRecord
	if err := json.Unmarshal(encoded, &record); err != nil || record.PID <= 0 {
		return processRecord{}, false, fmt.Errorf("%s is unreadable; remove it if no %s is running", processRecordPath(stateDir, service), service)
	}
	return record, true, nil
}

// clearProcessRecord removes the record only while it is still this process's.
// A successor controller overwrites the predecessor's record, and the
// predecessor's exit must not delete the successor's.
func clearProcessRecord(stateDir, service string) {
	record, ok, err := readProcessRecord(stateDir, service)
	if err != nil || !ok || record.PID != os.Getpid() {
		return
	}
	_ = os.Remove(processRecordPath(stateDir, service))
}

// upHost is the machine `up` acts on: processes, ports and the browser. Tests
// fake it; the real one is osUpHost.
type upHost interface {
	// ControllerAnswers reports whether a live controller answers on the
	// control endpoint of stateDir.
	ControllerAnswers(stateDir string) bool
	// Alive reports whether record still names the running process it was
	// written by: same pid, same start time, same executable running service.
	Alive(record processRecord, service string) bool
	// PortFree reports whether nothing listens on address.
	PortFree(address string) bool
	// DashboardHealthy reports whether the Control Plane of stateDir serves
	// authenticated reads on address. Any other program answering there, or a
	// Control Plane of another state directory, is not it.
	DashboardHealthy(address, stateDir string) bool
	// ControllerExecutable is the binary the serving controller runs, or ""
	// when none answers or it does not say.
	ControllerExecutable(stateDir string) string
	// Spawn starts a detached child and returns a channel that yields when
	// it exits. An empty executable means this binary.
	Spawn(executable, logPath string, args []string) (pid int, exited <-chan struct{}, err error)
	// Terminate asks pid to shut down gracefully.
	Terminate(pid int) error
	// Open shows url in the operator's browser.
	Open(url string) error
}

type osUpHost struct{}

func (osUpHost) ControllerAnswers(stateDir string) bool {
	_, err := observeLiveController(stateDir)()
	return err == nil
}

func (osUpHost) Alive(record processRecord, service string) bool {
	process, err := os.FindProcess(record.PID)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	command, err := exec.Command("ps", "-p", strconv.Itoa(record.PID), "-o", "command=").Output()
	if err != nil {
		return false
	}
	if record.Executable != "" && !strings.HasPrefix(string(command), record.Executable+" ") {
		return false
	}
	if !slices.Contains(strings.Fields(string(command)), service) {
		return false
	}
	return record.Started == "" || processStartMarker(record.PID) == record.Started
}

func (osUpHost) PortFree(address string) bool {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (osUpHost) DashboardHealthy(address, stateDir string) bool {
	token, err := os.ReadFile(filepath.Join(stateDir, controlplane.TokenFile))
	if err != nil {
		return false
	}
	// A Control Plane refuses a wrong credential and accepts its own. A program
	// that answers 200 to everything, or one gated by another state
	// directory's token, fails one of the two.
	return dashboardStatus(address, "not-the-token") == http.StatusUnauthorized &&
		dashboardStatus(address, strings.TrimSpace(string(token))) == http.StatusOK
}

func dashboardStatus(address, token string) int {
	request, err := http.NewRequest(http.MethodGet, "http://"+address+"/v1/runs?limit=1", nil)
	if err != nil {
		return 0
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		return 0
	}
	_ = response.Body.Close()
	return response.StatusCode
}

func (osUpHost) ControllerExecutable(stateDir string) string {
	snapshot, err := observeLiveController(stateDir)()
	if err != nil {
		return ""
	}
	return snapshot.Identity.ExecutablePath
}

func (osUpHost) Spawn(executable, logPath string, args []string) (int, <-chan struct{}, error) {
	if executable == "" {
		var err error
		if executable, err = os.Executable(); err != nil {
			return 0, nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return 0, nil, err
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return 0, nil, err
	}
	defer log.Close()
	command := exec.Command(executable, args...)
	command.Stdout, command.Stderr = log, log
	// Its own process group: closing the operator's terminal or pressing
	// Ctrl-C there must not stop the services `up` just made durable.
	configureSuccessorProcess(command)
	if err := command.Start(); err != nil {
		return 0, nil, err
	}
	exited := make(chan struct{})
	go func() { _ = command.Wait(); close(exited) }()
	return command.Process.Pid, exited, nil
}

func (osUpHost) Terminate(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

func (osUpHost) Open(url string) error {
	var command *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		command = exec.Command("open", url)
	case "windows":
		command = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		command = exec.Command("xdg-open", url)
	}
	return command.Start()
}
