package main

// `controller status`, which RENDERS and decides nothing.
//
// Every classification in the output comes from runtime.DescribeControllerStatus.
// The one thing this file owns is the exit-code contract, and it exists because
// automation needs to tell three different situations apart that a naive
// "healthy / not healthy" split would merge:
//
//	0  the durable state is consistent, whatever is or is not serving
//	1  an invariant violation: two authorities where there may be one
//	2  the status could not be collected at all
//	3  the observation was torn by a concurrent transition
//
// NOT SERVING IS NOT AN ERROR. A controller mid-upgrade, or activated and not
// yet admitting, is protocol-correct and temporarily unavailable; reporting it
// as 1 would make automation treat zero availability and split-brain authority
// as the same severity, and they are not remotely the same severity.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func controllerStatus(args []string, overrides autonomyOverrides, stdout io.Writer) (int, error) {
	asJSON := false
	var config string
	for len(args) > 0 {
		switch args[0] {
		case "--json":
			asJSON, args = true, args[1:]
		case "--config":
			if len(args) < 2 {
				return runtime.ExitInvalid, fmt.Errorf("--config requires a path")
			}
			config, args = args[1], args[2:]
		default:
			return runtime.ExitInvalid, fmt.Errorf("controller status does not accept %q; it takes --json and --config", args[0])
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 2, err
	}
	loaded, err := runtime.LoadConfig(config, cwd)
	if err != nil {
		return 2, err
	}
	store, err := runtime.OpenSQLiteOperationStore(loaded.StateDir)
	if err != nil {
		return 2, err
	}
	defer store.Close()

	status, err := runtime.DescribeControllerStatus(store, controllerRoot(), observeLiveController(loaded.StateDir), time.Now().UTC())
	if err != nil {
		return 2, err
	}
	if asJSON {
		encoded, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return 2, err
		}
		fmt.Fprintln(stdout, string(encoded))
	} else {
		renderControllerStatus(stdout, status)
	}
	return statusExitCode(status), nil
}

// statusExitCode is the contract automation reads. It is a function so it can
// be asserted directly: the one thing this command decides should not be
// reachable only by running it.
func statusExitCode(status runtime.ControllerStatus) int {
	switch status.DurableConsistency {
	case runtime.DurableViolation:
		return 1
	case runtime.DurableUnstable:
		return 3
	}
	// NOT SERVING IS NOT AN ERROR, and neither is a stale pointer. A controller
	// mid-upgrade is protocol-correct and temporarily unavailable; reporting
	// that as a failure would make automation treat zero availability and
	// split-brain authority as the same severity.
	return runtime.ExitCompleted
}

// observeLiveController asks a running controller about itself. An endpoint
// that is not there yields an error, which the status model turns into UNKNOWN
// rather than into a claim that nobody is serving.
func observeLiveController(stateDir string) func() (runtime.LiveControllerSnapshot, error) {
	return func() (runtime.LiveControllerSnapshot, error) {
		response, err := runtime.SendControl(stateDir, runtime.ControlRequest{Command: runtime.ControlCommandControllerSnapshot})
		if err != nil {
			return runtime.LiveControllerSnapshot{}, err
		}
		if !response.OK {
			return runtime.LiveControllerSnapshot{}, fmt.Errorf("%s", response.Error)
		}
		var snapshot runtime.LiveControllerSnapshot
		if err := json.Unmarshal(response.Payload, &snapshot); err != nil {
			return runtime.LiveControllerSnapshot{}, fmt.Errorf("the controller's snapshot could not be read: %w", err)
		}
		return snapshot, nil
	}
}

// controllerRoot is where adopted generations and the stable entrypoint live.
func controllerRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".zenchron-adopted-controller"
	}
	return filepath.Join(home, ".zenchron-adopted-controller")
}

// renderControllerStatus prints the dimensions. It flattens nothing: the reader
// sees which facts were observed, which were not, and where each came from.
func renderControllerStatus(stdout io.Writer, status runtime.ControllerStatus) {
	line := func(label, value string) { fmt.Fprintf(stdout, "%-22s %s\n", label, value) }

	durable := "none recorded"
	if status.Durable.Generation != nil {
		durable = fmt.Sprintf("%s (%s)", status.Durable.Generation.Version, shortVersion(status.Durable.Generation.SourceRevision))
	}
	line("durable active", durable)
	if status.Durable.HandoffID != "" {
		line("transition", fmt.Sprintf("%s %s", status.Durable.HandoffID, status.Durable.Phase))
	}
	if snapshot := status.Live.Snapshot; snapshot != nil {
		line("running generation", snapshot.Identity.Build.Version)
		line("role ownership", observed(string(snapshot.Role)))
		line("work admission", observed(string(snapshot.WorkAdmission)))
	} else {
		line("live controller", "unreachable: "+status.Live.Detail)
		line("role ownership", "UNKNOWN")
		line("work admission", "UNKNOWN")
	}
	projection := string(status.Projection.State)
	if status.Projection.Target != "" {
		projection += " -> " + status.Projection.Target
	}
	line("projection", projection)
	line("durable consistency", strings.ToUpper(string(status.DurableConsistency)))
	line("serving", strings.ToUpper(string(status.Serving)))
	for _, finding := range status.Findings {
		fmt.Fprintf(stdout, "  - %s\n", finding)
	}
}

// observed renders an unobserved fact as UNKNOWN rather than as a value nobody
// reported. The empty string is the model's unknown, and printing it as blank
// would read as "no role" to a human.
func observed(value string) string {
	if strings.TrimSpace(value) == "" {
		return "UNKNOWN"
	}
	return value
}
