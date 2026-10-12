package main

import (
	"sync"
	"testing"
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
