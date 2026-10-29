//go:build !windows

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/controlplane"
)

// A real child whose command line names the service, for the real Alive.
func startNamedChild(t *testing.T, service string) processRecord {
	t.Helper()
	child := exec.Command("/bin/sh", "-c", "sleep 30; true", service)
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	return processRecord{PID: child.Process.Pid, Executable: "/bin/sh", Started: processStartMarker(child.Process.Pid)}
}

func TestAliveBindsARecordToTheProcessThatWroteIt(t *testing.T) {
	host := osUpHost{}
	record := startNamedChild(t, serviceServe)
	if record.Started == "" || !host.Alive(record, serviceServe) {
		t.Fatalf("the process a record names must be alive: %+v", record)
	}
	for name, mutated := range map[string]processRecord{
		"another start time": {PID: record.PID, Executable: record.Executable, Started: "Mon Jan  1 00:00:00 2001"},
		"another executable": {PID: record.PID, Executable: "/usr/bin/zenchron-engineering", Started: record.Started},
	} {
		if host.Alive(mutated, serviceServe) {
			t.Errorf("a recycled pid with %s was trusted", name)
		}
	}
	if host.Alive(record, serviceControlPlane) {
		t.Error("a process running a different service was trusted")
	}
}

func TestDashboardHealthyDemandsTheStatesOwnAuthGate(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, controlplane.TokenFile), []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer gated.Close()
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer foreign.Close()
	otherState := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnauthorized)
	}))
	defer otherState.Close()

	host := osUpHost{}
	address := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	if !host.DashboardHealthy(address(gated), stateDir) {
		t.Error("this state's Control Plane was not recognised")
	}
	if host.DashboardHealthy(address(foreign), stateDir) {
		t.Error("a program that answers 200 to everything was taken for the Control Plane")
	}
	if host.DashboardHealthy(address(otherState), stateDir) {
		t.Error("a Control Plane gated by another token was taken for this state's")
	}
}
