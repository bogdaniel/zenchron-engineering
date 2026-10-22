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
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	serviceServe        = "serve"
	serviceControlPlane = "control-plane"
)

// processRecord is what a service writes about itself in its state directory.
type processRecord struct {
	PID    int    `json:"pid"`
	Listen string `json:"listen,omitempty"`
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
	// Alive reports whether pid is a running process whose command line
	// names the service, so a recycled pid is never mistaken for it.
	Alive(pid int, service string) bool
	// PortFree reports whether nothing listens on address.
	PortFree(address string) bool
	// PortAnswers reports whether an HTTP server answers on address.
	PortAnswers(address string) bool
	// Spawn starts a detached child of this binary and returns a channel
	// that yields when it exits.
	Spawn(logPath string, args []string) (pid int, exited <-chan struct{}, err error)
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

func (osUpHost) Alive(pid int, service string) bool {
	process, err := os.FindProcess(pid)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		return false
	}
	// ponytail: shells out to ps (unix); a recycled pid is rejected by command
	// line. Windows reports a live pid as alive without the identity check.
	if runtime.GOOS == "windows" {
		return true
	}
	command, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return err == nil && strings.Contains(string(command), service)
}

func (osUpHost) PortFree(address string) bool {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

func (osUpHost) PortAnswers(address string) bool {
	client := http.Client{Timeout: 2 * time.Second}
	response, err := client.Get("http://" + address + "/")
	if err != nil {
		return false
	}
	_ = response.Body.Close()
	return true
}

func (osUpHost) Spawn(logPath string, args []string) (int, <-chan struct{}, error) {
	executable, err := os.Executable()
	if err != nil {
		return 0, nil, err
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
