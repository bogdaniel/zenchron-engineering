// Command standalone runs one bounded execution with nothing but this module
// and the standard library: a scripted provider reads a file and writes a
// summary into a temporary workspace, with in-memory stores, and the settled
// result is printed as JSON. No Zenchron object, configuration or database is
// involved (#446 A01).
//
//	go run ./examples/standalone
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

const notes = "kernel: bounded execution\nhost: owns authority\n"

func main() {
	dir, err := os.MkdirTemp("", "agentkernel-standalone-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	res, err := run(context.Background(), dir)
	if err != nil {
		fail(err)
	}
	out, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		fail(err)
	}
	fmt.Println(string(out))
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "standalone:", err)
	os.Exit(1)
}

// run executes the scripted two-step task in dir, an empty directory.
func run(ctx context.Context, dir string) (api.ExecutionResult, error) {
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte(notes), 0o600); err != nil {
		return api.ExecutionResult{}, err
	}
	ws, err := tools.NewWorkspace(dir, nil)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	broker, err := tools.NewBroker(ws.ReadFile(), ws.WriteFile())
	if err != nil {
		return api.ExecutionResult{}, err
	}
	artifacts, err := storage.NewMemoryArtifacts(1 << 20)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	eng, err := engine.New(engine.Config{
		Providers: map[string]api.Provider{"scripted": script()},
		Broker:    broker, Artifacts: artifacts, Events: &events{}, Clock: systemClock{}, OutputLimit: 4096,
	})
	if err != nil {
		return api.ExecutionResult{}, err
	}
	return eng.Execute(ctx, request(time.Now()))
}

// script is the "model": read notes.txt, create summary.txt, stop. It reports
// no token usage, so the result states reported usage as unknown.
func script() *scripted.Provider {
	read := api.ToolCall{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"notes.txt"}`)}
	write := api.ToolCall{ID: "c2", Name: "write_file", Arguments: json.RawMessage(
		`{"path":"summary.txt","content":"2 lines read\n","expected_sha256":"absent"}`)}
	return scripted.New(
		scripted.Step{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{read}, Stop: api.StopToolUse}},
		scripted.Step{Response: api.ProviderResponse{ToolCalls: []api.ToolCall{write}, Stop: api.StopToolUse}},
		scripted.Step{Response: api.ProviderResponse{Text: "wrote summary.txt", Stop: api.StopEnd}},
	)
}

func request(now time.Time) api.ExecutionRequest {
	return api.ExecutionRequest{
		Version: api.ExecutionVersion, ExecutionID: "standalone-1", AttemptID: "attempt-1",
		Objective: "Summarize notes.txt into summary.txt.",
		Mode:      api.ModeReadWrite,
		// The host owns workspace identity; here the manifest is the one file it wrote.
		Workspace: api.WorkspaceRef{ID: "temp-workspace", ManifestDigest: api.Digest([]byte(notes))},
		Constraints: api.Constraints{
			RequiredFeatures:  []string{api.FeatureTools},
			InstructionDigest: api.Digest([]byte("standalone example")),
		},
		Grants: []api.Capability{
			{Handle: "read", Kind: api.CapabilityFileRead, Roots: []string{"."}},
			{Handle: "write", Kind: api.CapabilityFileWrite, Roots: []string{"."}},
		},
		Budget: api.Budget{
			Deadline: now.Add(time.Minute), MaxIterations: 5, MaxToolCalls: 5, MaxInputTokens: 50000,
			MaxOutputTokens: 4000, MaxArtifactBytes: 1 << 20,
		},
		Providers: []api.ProviderBinding{{
			ID: "scripted", Kind: "scripted", Model: "fixture", ModelVersion: api.UnknownVersion, Features: []string{api.FeatureTools},
			ConfigFingerprint: api.Digest([]byte("scripted")), Eligible: true, Isolation: api.IsolationUnproven,
			ContextWindow: 32000, MaxOutputTokens: 1000,
		}},
	}
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// events keeps every event in memory; a real host persists them.
type events struct {
	mu   sync.Mutex
	list []api.Event
}

func (e *events) Record(_ context.Context, ev api.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.list = append(e.list, ev)
	return nil
}
