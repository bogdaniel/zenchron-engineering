package main

import (
	"context"
	"sync"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestPlanningAdmissionRefusesDuplicateAndReleases(t *testing.T) {
	c := &composition{}
	release, err := c.admitBrowserPlanning("p")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	for range 2 {
		go func() {
			defer wg.Done()
			if again, err := c.admitBrowserPlanning("p"); err == nil {
				again()
				t.Error("duplicate provider invocation admitted")
			}
		}()
	}
	wg.Wait()
	release()
	again, err := c.admitBrowserPlanning("p")
	if err != nil {
		t.Fatal("completed invocation retained admission", err)
	}
	again()
}

func TestRefreshRefusesBeforeCancellingWhenPolicyGenerationCannotBeRecorded(t *testing.T) {
	dir, configPath := planWorkspace(t)
	stateDir := activeRun(t, configPath, dir, "refresh-run")
	t.Chdir(dir)
	built, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(built.release)
	self, err := controllerSelf()
	if err != nil {
		t.Fatal(err)
	}
	governs(t, built, self)
	built.config.Digest.Global += "-stale"

	response := built.controlRunLifecycle(context.Background(), nil, runtime.ControlRequest{Command: runtime.ControlRefresh, RunID: "refresh-run"})
	if response.OK {
		t.Fatal("refresh proceeded under a configuration the governing authority does not accept")
	}
	if got := cancellations(t, stateDir, "refresh-run"); got != 0 {
		t.Fatalf("refused refresh appended %d cancellation events", got)
	}
}
