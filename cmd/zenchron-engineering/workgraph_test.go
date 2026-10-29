package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const workGraphProposal = `{"name":"m2-o1","revision":1,"units":[
	{"id":"a","purpose":"land the schema","role":"implementer","issue":101},
	{"id":"b","purpose":"land the reader","role":"implementer","issue":102,"depends_on":["a"]}
]}`

func proposalFile(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestWorkGraphRefusesAMalformedRequestBeforeSendingIt: an explicit agent and a
// readable, strictly decodable proposal are required before anything reaches a
// supervisor.
func TestWorkGraphRefusesAMalformedRequestBeforeSendingIt(t *testing.T) {
	valid := proposalFile(t, workGraphProposal)
	// A proposal stating a fact the RUNTIME owns is refused, not ignored.
	forged := proposalFile(t, `{"name":"m2-o1","revision":1,"repository":"acme/repo","units":[]}`)
	twoValues := proposalFile(t, workGraphProposal+workGraphProposal)
	for name, args := range map[string][]string{
		"no agent":        {"workgraph", "adopt", valid},
		"no proposal":     {"workgraph", "adopt"},
		"no graph id":     {"workgraph", "status"},
		"unknown command": {"workgraph", "list", "x"},
		"missing file":    {"workgraph", "adopt", filepath.Join(t.TempDir(), "absent.json"), "--agent", "claude"},
		"unknown member":  {"workgraph", "adopt", forged, "--agent", "claude"},
		"trailing value":  {"workgraph", "adopt", twoValues, "--agent", "claude"},
	} {
		code, err := autonomy(args, offlineOverrides(), &bytes.Buffer{})
		if err == nil || code != runtime.ExitInvalid {
			t.Errorf("%s: code=%d err=%v, want an invalid-request refusal", name, code, err)
		}
	}
	// A flag where the subject belongs is the usage error it is, never read
	// as a graph id or a proposal path.
	for name, args := range map[string][]string{
		"flag as graph id": {"workgraph", "status", "--text"},
		"flag as proposal": {"workgraph", "adopt", "--agent", "claude", valid},
	} {
		code, err := autonomy(args, offlineOverrides(), &bytes.Buffer{})
		if err == nil || code != runtime.ExitInvalid || err.Error() != workgraphUsage {
			t.Errorf("%s: code=%d err=%v, want the usage refusal", name, code, err)
		}
	}
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	_, err := autonomy([]string{"workgraph", "adopt", valid, "--agent", "openai-responses", "--config", configPath},
		offlineOverrides(), &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("a work graph was accepted with no supervisor to own its runs: %v", err)
	}
}

// TestWorkGraphHoldAndResolveRefuseAMalformedRequestBeforeSendingIt is #508's
// CLI boundary: hold and resolve both name TWO subjects before any flag, and
// neither reaches a supervisor without one running to own the durable state.
func TestWorkGraphHoldAndResolveRefuseAMalformedRequestBeforeSendingIt(t *testing.T) {
	for name, args := range map[string][]string{
		"hold, no unit":            {"workgraph", "hold", "graph-1"},
		"hold, no graph":           {"workgraph", "hold"},
		"hold, flag as unit":       {"workgraph", "hold", "graph-1", "--note", "x"},
		"resolve, no outcome":      {"workgraph", "resolve", "message-1"},
		"resolve, no request":      {"workgraph", "resolve"},
		"resolve, flag as outcome": {"workgraph", "resolve", "message-1", "--note", "x"},
	} {
		code, err := autonomy(args, offlineOverrides(), &bytes.Buffer{})
		if err == nil || code != runtime.ExitInvalid || err.Error() != workgraphUsage {
			t.Errorf("%s: code=%d err=%v, want the usage refusal", name, code, err)
		}
	}
	dir, configPath, _ := seededWorkspace(t, "https://github.com/zenchron/seeded.git")
	t.Chdir(dir)
	if _, err := autonomy([]string{"workgraph", "hold", "graph-1", "unit-a", "--note", "sign-off", "--config", configPath},
		offlineOverrides(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("a hold was accepted with no supervisor to own the graph: %v", err)
	}
	if _, err := autonomy([]string{"workgraph", "resolve", "message-1", "allow", "--config", configPath},
		offlineOverrides(), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "serve") {
		t.Fatalf("a resolution was accepted with no supervisor to own the work it unblocks: %v", err)
	}
}
