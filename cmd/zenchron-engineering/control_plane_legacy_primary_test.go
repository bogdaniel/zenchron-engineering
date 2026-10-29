//go:build !windows

package main

import (
	"os"
	"strings"
	"testing"
)

// The primary lock cannot see a dashboard from a build that predates it, so the
// record is what protects it. Decided against a fake host first: only a record
// that still names the live, identity-verified process blocks a start.
func TestOnlyALiveIdentityVerifiedRecordBlocksANewPrimary(t *testing.T) {
	state := t.TempDir()
	host := &fakeUpHost{alive: map[int]bool{}}
	live := processRecord{PID: 4242, Listen: "127.0.0.1:8789"}

	if err := refuseLiveLegacyPrimary(host, state); err != nil {
		t.Fatalf("no record blocked a start: %v", err)
	}
	if err := writeProcessRecord(state, serviceControlPlane, live); err != nil {
		t.Fatal(err)
	}
	if err := refuseLiveLegacyPrimary(host, state); err != nil {
		t.Fatalf("a dead process's record blocked a start: %v", err)
	}
	host.alive[4242] = true
	err := refuseLiveLegacyPrimary(host, state)
	if err == nil || !strings.Contains(err.Error(), "4242") || !strings.Contains(err.Error(), "127.0.0.1:8789") {
		t.Fatalf("a live legacy primary was not refused with its pid and address: %v", err)
	}
	if err := os.WriteFile(processRecordPath(state, serviceControlPlane), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := refuseLiveLegacyPrimary(host, state); err != nil {
		t.Fatalf("an unreadable record blocked a start: %v", err)
	}
}

// The real thing: a live process with a recorded identity that is not the new
// primary. The new one must refuse and leave the record byte-identical; a
// record whose identity no longer matches (a recycled pid, a dead process)
// must not wedge startup, and is replaced.
func TestANewPrimaryRefusesALivePreLockDashboardAndIgnoresStaleRecords(t *testing.T) {
	state := seededState(t)
	legacy := startNamedChild(t, serviceControlPlane)
	legacy.Listen = "127.0.0.1:8789"
	if err := writeProcessRecord(state, serviceControlPlane, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(processRecordPath(state, serviceControlPlane))
	if err != nil {
		t.Fatal(err)
	}

	if cp, err := startControlPlane(t, state); err == nil {
		cp.stop()
		t.Fatal("a new primary started beside a live pre-lock dashboard")
	} else if !strings.Contains(err.Error(), "already runs") {
		t.Fatalf("refusal is not the actionable one: %v", err)
	}
	after, err := os.ReadFile(processRecordPath(state, serviceControlPlane))
	if err != nil || string(after) != string(before) {
		t.Fatalf("the refused start changed the live dashboard's record: %v\n%s\n%s", err, before, after)
	}

	// The same live pid, but not the process that wrote the record: a recycled pid.
	recycled := legacy
	recycled.Started = "Mon Jan  1 00:00:00 2001"
	if err := writeProcessRecord(state, serviceControlPlane, recycled); err != nil {
		t.Fatal(err)
	}
	cp, err := startControlPlane(t, state)
	if err != nil {
		t.Fatalf("a recycled-pid record wedged startup: %v", err)
	}
	cp.stop()

	// And a pid that is gone altogether.
	if err := writeProcessRecord(state, serviceControlPlane, processRecord{PID: 1 << 22, Listen: "127.0.0.1:8789"}); err != nil {
		t.Fatal(err)
	}
	cp, err = startControlPlane(t, state)
	if err != nil {
		t.Fatalf("a dead process's record wedged startup: %v", err)
	}
	cp.stop()
}
