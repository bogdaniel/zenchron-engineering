package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// latestPolicyGeneration reads the newest supervisor-start record the CLI's
// state directory holds.
func latestPolicyGeneration(t *testing.T, dir, configPath string) (runtime.SupervisorStart, bool) {
	t.Helper()
	config, err := runtime.LoadConfig(configPath, dir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runtime.OpenSQLiteOperationStore(config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	start, found, err := store.LatestSupervisorStart()
	if err != nil {
		t.Fatal(err)
	}
	return start, found
}

// TestStandaloneWatchRecordsItsPolicyGeneration: a watcher run without
// `serve` drives runs under the moved S policy, so it records the policy it
// enforces - resolved, not stated - before it polls (#346 review).
func TestStandaloneWatchRecordsItsPolicyGeneration(t *testing.T) {
	dir, configPath := watchWorkspace(t)
	if _, found := latestPolicyGeneration(t, dir, configPath); found {
		t.Fatal("setup: a policy generation exists before the watcher started")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	controller := &scriptedWatch{reports: []runtime.TickReport{{NextEligibleAt: time.Now().Add(time.Minute)}}}
	waiter := &recordingWaiter{stopAfter: 1, cancel: cancel}
	overrides := offlineOverrides()
	overrides.Watch, overrides.WatchWait = controller, waiter.Wait
	if _, err := autonomyWatch(ctx, autonomyFlags{Config: configPath}, overrides, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	start, found := latestPolicyGeneration(t, dir, configPath)
	if !found {
		t.Fatal("a standalone watcher drove runs with no durable record of the policy it enforced")
	}
	if start.Policy.PollIntervalSeconds != 60 || start.Policy.MaxConcurrentRuns < 1 || start.PolicyDigest == "" {
		t.Fatalf("recorded %+v, want the resolved policy the watcher enforces", start.Policy)
	}
}

// TestALocalDriveRecordsItsPolicyGeneration: `autonomy resume` driving a run
// in this process, with no supervisor, enforces the scheduler ceilings like
// any driver and records its policy generation first.
func TestALocalDriveRecordsItsPolicyGeneration(t *testing.T) {
	dir, configPath, runID := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	if _, found := latestPolicyGeneration(t, dir, configPath); found {
		t.Fatal("setup: a policy generation exists before anything drove")
	}
	_, _ = autonomy([]string{"resume", runID, "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if _, found := latestPolicyGeneration(t, dir, configPath); !found {
		t.Fatal("a local drive ran with no durable record of the policy it enforced")
	}
}
