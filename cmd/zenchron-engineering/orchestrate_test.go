package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestOrchestrateRefusesAMalformedRequestBeforeSendingIt: an explicit agent,
// and a bounded list of distinct positive issues, are required before anything
// reaches a supervisor.
func TestOrchestrateRefusesAMalformedRequestBeforeSendingIt(t *testing.T) {
	for name, args := range map[string][]string{
		"no agent":    {"orchestrate", "issues", "7", "8"},
		"duplicate":   {"orchestrate", "issues", "7", "7", "--agent", "claude"},
		"no issues":   {"orchestrate", "issues", "--agent", "claude"},
		"negative":    {"orchestrate", "issues", "-3", "--agent", "claude"},
		"no batch id": {"orchestrate", "status"},
	} {
		code, err := autonomy(args, offlineOverrides(), &bytes.Buffer{})
		if err == nil || code != runtime.ExitInvalid {
			t.Errorf("%s: code=%d err=%v, want an invalid-request refusal", name, code, err)
		}
	}
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	_, err := autonomy([]string{"orchestrate", "issues", "7", "8", "--agent", "openai-responses", "--config", configPath}, offlineOverrides(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("a batch was accepted with no supervisor to own it: %v", err)
	}
}

// TestOrchestrateGoesThroughTheRunningSupervisor drives the real CLI against a
// real composition and control endpoint: one request creates one batch whose
// children are ordinary runs, the same request again returns the same batch,
// and the aggregate status reads it back.
func TestOrchestrateGoesThroughTheRunningSupervisor(t *testing.T) {
	stateDir, err := os.MkdirTemp("/tmp", "zc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(stateDir) })
	if err := os.Chmod(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, configPath := planWorkspaceIn(t, stateDir)
	t.Chdir(dir)
	serving, err := newComposition(autonomyFlags{Config: configPath}, planOverrides(t, 41))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(serving.release)
	// The supervisor probes the agent's readiness before creating any run,
	// exactly as Submit does; the brokered agent is ready once its operator
	// credential exists and is owner-only. Nothing reads its contents.
	if err := os.WriteFile(serving.agent.CredentialPath, []byte("unused"), 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := runtime.ListenControl(serving.config.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	driver, err := serving.supervisor([]runtime.GitHubRepo{{Owner: "zenchron", Name: "seeded"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		_ = listener.Serve(func(request runtime.ControlRequest) runtime.ControlResponse {
			return serving.handleControl(context.Background(), driver, func() {}, request)
		})
	}()

	submit := func() runtime.OrchestrationView {
		t.Helper()
		var out bytes.Buffer
		code, err := autonomy([]string{"orchestrate", "issues", "41", "42", "43", "--agent", serving.agent.ID, "--config", configPath},
			planOverrides(t, 41), &out)
		if err != nil || code != runtime.ExitCompleted {
			t.Fatalf("orchestrate: code=%d err=%v\n%s", code, err, out.String())
		}
		var view runtime.OrchestrationView
		if err := json.Unmarshal(out.Bytes(), &view); err != nil {
			t.Fatalf("orchestrate printed no view: %v\n%s", err, out.String())
		}
		return view
	}
	first := submit()
	if first.Counts.Total != 3 || first.RequestedBy != "operator-1" {
		t.Fatalf("view = %+v", first)
	}
	if again := submit(); again.BatchID != first.BatchID {
		t.Fatalf("the same request created a second batch: %s then %s", first.BatchID, again.BatchID)
	}
	runs, err := serving.store.Runs()
	if err != nil || len(runs) != 3 {
		t.Fatalf("%d runs (%v), want one per named issue", len(runs), err)
	}
	var text bytes.Buffer
	if _, err := autonomy([]string{"orchestrate", "status", first.BatchID, "--text", "--config", configPath}, planOverrides(t, 41), &text); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ORCHESTRATION " + first.BatchID, "3 total", "#41", "#43"} {
		if !strings.Contains(text.String(), want) {
			t.Fatalf("status text lacks %q:\n%s", want, text.String())
		}
	}
}
