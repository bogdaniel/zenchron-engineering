package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestObjectiveIntakeRejectsMalformedIssue(t *testing.T) {
	for _, issue := range []string{"", "0", "-1", "objective text", "--agent"} {
		code, err := autonomyOrchestrate([]string{"objective", issue}, &bytes.Buffer{})
		if code != runtime.ExitInvalid || err == nil {
			t.Fatalf("issue %q: code=%d err=%v", issue, code, err)
		}
	}
}

func TestObjectiveDelegationKeepsExplicitAgent(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	serving, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serving.release)
	listener, err := runtime.ListenControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	requests := make(chan runtime.ControlRequest, 1)
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			requests <- request
			return controlOK(runtime.PlanView{})
		})
	}()
	_, err = autonomyPlan(context.Background(), []string{"issue", "41", "--agent", "claude", "--repo", "bogdaniel/zenchron-engineering", "--text", "--config", configPath}, planOverrides(t, 41), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("canonical plan issue intake: %v", err)
	}
	request := <-requests
	if request.Agent != "claude" || !request.ObjectiveWorkflow || request.Issue != 41 {
		t.Fatalf("objective request lost its worker or workflow binding: %+v", request)
	}
}

func TestObjectivePlanShowsDurableGraph(t *testing.T) {
	var out bytes.Buffer
	_, err := renderObjectivePlan(autonomyFlags{Text: true}, runtime.PlanView{WorkGraphID: "graph-objective"}, &out, "proposed")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "work graph: graph-objective (activation awaits plan approval)") {
		t.Fatalf("graph approval boundary missing from output: %s", out.String())
	}
}
