package main

// `up`, `down` and `status`: the one-action product lifecycle.
//
// `up` makes the two processes the product needs - the `serve` supervisor and
// the `control-plane` dashboard - true, and tells the operator where the
// dashboard is. It composes the commands that already exist; it adds no
// scheduler, no authority and no credential. It runs the SAME binary the
// operator ran, so an adopted controller stays the adopted controller, and it
// reuses a healthy process instead of starting a second one.
//
// It only ever stops what it can prove is a Zenchron service of this state
// directory, and it never kills: `serve` shuts down gracefully on SIGTERM and
// leaves every run exactly as resumable as its journal says.

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const (
	defaultDashboardHost = "127.0.0.1"
	dashboardPortFirst   = 8787
	dashboardPortCount   = 10

	upUsage = "usage: zenchron-engineering up [--repo owner/name] [--config <path>] [--agent <id>] [--reviewer-agent <id>] [--listen 127.0.0.1:PORT] [--no-open]"
)

// upTiming bounds how long `up` and `down` wait for a process. Tests shrink it.
type upTiming struct {
	Ready time.Duration
	Poll  time.Duration
}

var defaultUpTiming = upTiming{Ready: 60 * time.Second, Poll: 250 * time.Millisecond}

type upFlags struct {
	autonomy autonomyFlags
	listen   string
	noOpen   bool
}

// parseUpFlags takes the two flags `up` owns and hands the rest to the parser
// `serve` uses, so `up --repo X --agent Y` means exactly what `serve` means.
func parseUpFlags(args []string) (upFlags, error) {
	var flags upFlags
	var rest []string
	for len(args) > 0 {
		switch args[0] {
		case "--no-open":
			flags.noOpen, args = true, args[1:]
		case "--listen":
			if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
				return upFlags{}, errors.New("--listen requires an address\n\n" + upUsage)
			}
			flags.listen, args = args[1], args[2:]
		default:
			rest, args = append(rest, args[0]), args[1:]
		}
	}
	parsed, err := parseAutonomyFlags(rest)
	if err != nil {
		return upFlags{}, fmt.Errorf("%w\n\n%s", err, upUsage)
	}
	flags.autonomy = parsed
	return flags, nil
}

func upStateDir(configPath string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	config, err := runtime.LoadConfig(configPath, cwd)
	if err != nil {
		return "", err
	}
	return config.StateDir, nil
}

func upCommand(args []string, host upHost, timing upTiming, stdout io.Writer) (int, error) {
	flags, err := parseUpFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir, err := upStateDir(flags.autonomy.Config)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	if err := ensureServe(host, timing, stateDir, flags.autonomy, stdout); err != nil {
		return runtime.ExitFailed, err
	}
	address, err := ensureControlPlane(host, timing, stateDir, flags.listen, stdout)
	if err != nil {
		return runtime.ExitFailed, err
	}
	url := "http://" + address + "/"
	fmt.Fprintf(stdout, "%-14s %s\n", "dashboard", url)
	fmt.Fprintf(stdout, "%-14s paste the contents of %s\n", "sign in", filepath.Join(stateDir, "control-plane.token"))
	if flags.noOpen {
		return runtime.ExitCompleted, nil
	}
	if err := host.Open(url); err != nil {
		fmt.Fprintf(stdout, "could not open a browser (%v); open the dashboard address above\n", err)
	}
	return runtime.ExitCompleted, nil
}

func ensureServe(host upHost, timing upTiming, stateDir string, flags autonomyFlags, stdout io.Writer) error {
	if host.ControllerAnswers(stateDir) {
		fmt.Fprintf(stdout, "%-14s running (reused)\n", "supervisor")
		return nil
	}
	record, recorded, err := readProcessRecord(stateDir, serviceServe)
	if err != nil {
		return err
	}
	if recorded && host.Alive(record, serviceServe) {
		// It is starting, or finishing a controller handover. Wait for it
		// rather than start a second supervisor that would only be refused
		// the controller role.
		if !pollUntil(timing, func() bool { return host.ControllerAnswers(stateDir) }) {
			return fmt.Errorf("the supervisor (pid %d) is running but does not answer; see %s, then `down` and `up` again",
				record.PID, serveLogPath(stateDir))
		}
		fmt.Fprintf(stdout, "%-14s running (pid %d)\n", "supervisor", record.PID)
		return nil
	}
	args := []string{serviceServe}
	for _, option := range []struct{ name, value string }{
		{"--repo", flags.Repo}, {"--config", flags.Config}, {"--agent", flags.Agent}, {"--reviewer-agent", flags.ReviewerAgent},
	} {
		if option.value != "" {
			args = append(args, option.name, option.value)
		}
	}
	pid, exited, err := host.Spawn("", serveLogPath(stateDir), args)
	if err != nil {
		return fmt.Errorf("start the supervisor: %w", err)
	}
	if !pollFor(timing, exited, func() bool { return host.ControllerAnswers(stateDir) }) {
		return fmt.Errorf("the supervisor did not become ready; see %s", serveLogPath(stateDir))
	}
	fmt.Fprintf(stdout, "%-14s started (pid %d), log %s\n", "supervisor", pid, serveLogPath(stateDir))
	return nil
}

// ensureControlPlane returns the address the dashboard answers on.
func ensureControlPlane(host upHost, timing upTiming, stateDir, requested string, stdout io.Writer) (string, error) {
	record, recorded, err := readProcessRecord(stateDir, serviceControlPlane)
	if err != nil {
		return "", err
	}
	if recorded && host.Alive(record, serviceControlPlane) {
		// A live recorded dashboard that does not answer yet is starting or busy
		// (a single probe is only seconds long and a loaded machine misses it).
		// Starting a second one beside it would only be refused by its primary
		// lock and moved to another port, so wait for it instead.
		if !pollUntil(timing, func() bool { return host.DashboardHealthy(record.Listen, stateDir) }) {
			return "", fmt.Errorf("the dashboard (pid %d) is running on %s but does not answer; see %s, then `down` and `up` again",
				record.PID, record.Listen, controlPlaneLogPath(stateDir))
		}
		// A controller handover changes the generation the dashboard must
		// match. One built from the previous generation still answers, but the
		// controller would refuse everything it tried to do, so it is replaced
		// on the same address rather than reused.
		if serving := host.ControllerExecutable(stateDir); serving != "" && record.Executable != "" && record.Executable != serving {
			if err := stopService(host, timing, stateDir, serviceControlPlane, stdout); err != nil {
				return "", err
			}
			if requested == "" {
				requested = record.Listen
			}
			return startDashboard(host, timing, stateDir, requested, stdout)
		}
		if requested != "" && requested != record.Listen {
			return "", fmt.Errorf("the dashboard already runs on %s; run `down` first to move it to %s", record.Listen, requested)
		}
		fmt.Fprintf(stdout, "%-14s running (reused, pid %d)\n", "dashboard host", record.PID)
		return record.Listen, nil
	}
	return startDashboard(host, timing, stateDir, requested, stdout)
}

func startDashboard(host upHost, timing upTiming, stateDir, requested string, stdout io.Writer) (string, error) {
	address, err := chooseDashboardAddress(host, requested)
	if err != nil {
		return "", err
	}
	// The dashboard acts on the controller's behalf, and the controller refuses
	// a caller whose build differs from its own generation. So it runs the
	// serving controller's binary, not whichever one happened to run `up`.
	pid, exited, err := host.Spawn(host.ControllerExecutable(stateDir), controlPlaneLogPath(stateDir),
		[]string{serviceControlPlane, "--state-dir", stateDir, "--listen", address})
	if err != nil {
		return "", fmt.Errorf("start the dashboard: %w", err)
	}
	if !pollFor(timing, exited, func() bool { return host.DashboardHealthy(address, stateDir) }) {
		return "", fmt.Errorf("the dashboard did not become ready on %s; see %s", address, controlPlaneLogPath(stateDir))
	}
	fmt.Fprintf(stdout, "%-14s started (pid %d), log %s\n", "dashboard host", pid, controlPlaneLogPath(stateDir))
	return address, nil
}

// chooseDashboardAddress honours an explicit request exactly and otherwise
// takes the first free port, so a port held by something unrelated moves the
// dashboard instead of failing the start.
func chooseDashboardAddress(host upHost, requested string) (string, error) {
	if requested != "" {
		if !host.PortFree(requested) {
			return "", fmt.Errorf("%s is already in use by another program; choose another --listen address", requested)
		}
		return requested, nil
	}
	for offset := 0; offset < dashboardPortCount; offset++ {
		address := net.JoinHostPort(defaultDashboardHost, strconv.Itoa(dashboardPortFirst+offset))
		if host.PortFree(address) {
			return address, nil
		}
	}
	return "", fmt.Errorf("ports %d-%d on %s are all in use; free one or pass --listen", dashboardPortFirst, dashboardPortFirst+dashboardPortCount-1, defaultDashboardHost)
}

func serveLogPath(stateDir string) string { return filepath.Join(stateDir, "logs", "serve.log") }
func controlPlaneLogPath(stateDir string) string {
	return filepath.Join(stateDir, "logs", "control-plane.log")
}

// pollUntil polls condition until it holds or the ready window closes.
func pollUntil(timing upTiming, condition func() bool) bool {
	return pollFor(timing, nil, condition)
}

// pollFor is pollUntil that also gives up the moment the child exits: a child
// that died will never become ready, and waiting out the window would only
// delay the operator reading why.
func pollFor(timing upTiming, exited <-chan struct{}, condition func() bool) bool {
	deadline := time.Now().Add(timing.Ready)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-exited:
			return condition()
		case <-time.After(timing.Poll):
		}
	}
}

func downCommand(args []string, host upHost, timing upTiming, stdout io.Writer) (int, error) {
	flags, err := parseUpFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir, err := upStateDir(flags.autonomy.Config)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	var failures []string
	// The dashboard first: it only reads, so nothing waits on it.
	for _, service := range []string{serviceControlPlane, serviceServe} {
		if err := stopService(host, timing, stateDir, service, stdout); err != nil {
			failures = append(failures, err.Error())
		}
	}
	if len(failures) > 0 {
		return runtime.ExitFailed, errors.New(strings.Join(failures, "\n"))
	}
	return runtime.ExitCompleted, nil
}

func stopService(host upHost, timing upTiming, stateDir, service string, stdout io.Writer) error {
	record, recorded, err := readProcessRecord(stateDir, service)
	if err != nil {
		return err
	}
	if !recorded || !host.Alive(record, service) {
		if recorded {
			_ = os.Remove(processRecordPath(stateDir, service))
		}
		if service == serviceServe && host.ControllerAnswers(stateDir) {
			return fmt.Errorf("a supervisor answers on %s but did not record itself, so it was started by an older build; stop it with Ctrl-C in its terminal", stateDir)
		}
		fmt.Fprintf(stdout, "%-14s not running\n", service)
		return nil
	}
	if err := host.Terminate(record.PID); err != nil {
		return fmt.Errorf("stop %s (pid %d): %w", service, record.PID, err)
	}
	if !pollUntil(timing, func() bool { return !host.Alive(record, service) }) {
		return fmt.Errorf("%s (pid %d) is still shutting down; it is finishing in-flight work, run `down` again shortly", service, record.PID)
	}
	fmt.Fprintf(stdout, "%-14s stopped (pid %d)\n", service, record.PID)
	return nil
}

func statusCommand(args []string, host upHost, stdout io.Writer) (int, error) {
	flags, err := parseUpFlags(args)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	stateDir, err := upStateDir(flags.autonomy.Config)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	supervisor := "not running"
	controllerExecutable := ""
	if host.ControllerAnswers(stateDir) {
		supervisor = "running"
		controllerExecutable = host.ControllerExecutable(stateDir)
	}
	if record, ok, err := readProcessRecord(stateDir, serviceServe); err != nil {
		return runtime.ExitFailed, err
	} else if ok && supervisor == "running" {
		supervisor += fmt.Sprintf(" (pid %d)", record.PID)
	} else if ok && !host.Alive(record, serviceServe) {
		supervisor += " (stale record from pid " + strconv.Itoa(record.PID) + "; `up` will replace it)"
	}
	if controllerExecutable != "" {
		supervisor += fmt.Sprintf(" (executable %s)", controllerExecutable)
	}
	fmt.Fprintf(stdout, "%-14s %s\n", "supervisor", supervisor)

	dashboard := "not running"
	record, ok, err := readProcessRecord(stateDir, serviceControlPlane)
	if err != nil {
		return runtime.ExitFailed, err
	}
	switch {
	case ok && host.Alive(record, serviceControlPlane) && host.DashboardHealthy(record.Listen, stateDir):
		dashboard = fmt.Sprintf("http://%s/ (pid %d)", record.Listen, record.PID)
		if controllerExecutable != "" && record.Executable != "" && record.Executable != controllerExecutable {
			dashboard += " (built for an earlier generation; `up` will replace it)"
		}
	case ok:
		dashboard = "not running (stale record; `up` will replace it)"
	}
	fmt.Fprintf(stdout, "%-14s %s\n", "dashboard", dashboard)
	return runtime.ExitCompleted, nil
}
