package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// The child is the existing composition and serve handler, never a test
// authority implementation. External forge/provider boundaries stay offline;
// no scheduler or worker runs. It reopens the parent's exact durable state.
func TestEngineeringOSControllerProcess(t *testing.T) {
	config := os.Getenv("ZENCHRON_TEST_CONTROLLER_CONFIG")
	if config == "" {
		t.Skip("subprocess helper")
	}
	attestedBuild(t)
	built, err := newComposition(autonomyFlags{Config: config}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	defer built.release()
	role, err := runtime.AcquireControllerRole(built.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer role.Release()
	built.role = role
	driver, err := built.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := runtime.ListenControl(built.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		done <- listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return built.handleControl(context.Background(), driver, func() {}, request)
		})
	}()
	fmt.Println("ZENCHRON_CONTROLLER_READY")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A real OS-process boundary: crash/exit releases the role; a new process
// measures the same binary, reacquires it and reads the preserved journal.
func controllerProcess(t *testing.T, config string) func() {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestEngineeringOSControllerProcess$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(), "ZENCHRON_TEST_CONTROLLER_CONFIG="+config)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		if err := cmd.Process.Kill(); err != nil {
			t.Error(err)
		}
		// Killing is the deliberate fault. Wait reaps the owned child.
		if err := cmd.Wait(); err == nil {
			t.Error("controller process exited without the deliberate crash")
		}
	}
	t.Cleanup(stop)
	ready := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ZENCHRON_CONTROLLER_READY" {
				ready <- true
				return
			}
		}
		ready <- false
	}()
	select {
	case ok := <-ready:
		if !ok {
			t.Fatal("controller subprocess exited before readiness")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("controller subprocess did not become ready")
	}
	return stop
}
