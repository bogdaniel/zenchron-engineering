// Command hostport shows a host supplying the kernel's ports itself: its own
// CommandRunner, EventSink and CredentialSource, a custom document-analysis
// tool, and a provider reached through the providers/local adapter. Every
// port is served by a worker goroutine the host starts and stops; the kernel
// only hands calls over and waits a bounded time. The task is not about a
// repository at all (#446 A02).
//
//	go run ./examples/hostport
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/local"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

var documents = map[string]string{
	"contracts/lease.txt": "1. Parties. Landlord and Tenant.\n\n2. Rent is due monthly.\n\n" +
		"3. Repairs are the Landlord's duty.\n\n4. Termination requires 60 days written notice.",
	"hr/salaries.txt": "confidential",
}

func main() {
	dir, err := os.MkdirTemp("", "agentkernel-hostport-")
	if err != nil {
		fail(err)
	}
	defer os.RemoveAll(dir)
	res, err := run(context.Background(), dir, os.Stderr)
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
	fmt.Fprintln(os.Stderr, "hostport:", err)
	os.Exit(1)
}

// run executes the document task; dir is the command working directory and
// events receives the host's JSON-lines event log.
func run(ctx context.Context, dir string, events io.Writer) (api.ExecutionResult, error) {
	workers, stop := context.WithCancel(context.Background())
	defer stop()
	command, err := tools.NewCommand(api.ServeCommands(workers, hostRunner{documents: documents}), dir)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	clause, err := findClause{documents: documents}.tool(workers)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	broker, err := tools.NewBroker(clause, command)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	model, err := local.New(local.Config{
		Endpoint: "http://model.host.invalid/v1/complete", MaxResponseBytes: 1 << 20,
		Doer:        modelServer{token: "host-secret", script: script()},
		Credentials: vault{"doc-model": "host-secret"},
	})
	if err != nil {
		return api.ExecutionResult{}, err
	}
	artifacts, err := storage.NewMemoryArtifacts(1 << 20)
	if err != nil {
		return api.ExecutionResult{}, err
	}
	eng, err := engine.New(engine.Config{
		Providers: map[string]chan<- api.ProviderCall{"doc-model": api.ServeProvider(workers, model)},
		Broker:    broker, Artifacts: artifacts,
		Events: api.ServeEvents(workers, &jsonLines{w: events}), Clock: api.SystemClock{}, OutputLimit: 4096,
	})
	if err != nil {
		return api.ExecutionResult{}, err
	}
	return eng.Execute(ctx, request(time.Now()))
}

// script lists the documents, asks for the termination clause in the lease
// and (refused: not granted) in the salary file, then answers.
func script() *scripted.Provider {
	call := func(id, name, args string) api.ToolCall {
		return api.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(args)}
	}
	return scripted.New(
		scripted.Step{Response: api.ProviderResponse{Stop: api.StopToolUse, ToolCalls: []api.ToolCall{
			call("c1", "run_command", `{"command":"list_documents"}`)}}},
		scripted.Step{Response: api.ProviderResponse{Stop: api.StopToolUse, ToolCalls: []api.ToolCall{
			call("c2", "find_clause", `{"document":"contracts/lease.txt","term":"termination"}`),
			call("c3", "find_clause", `{"document":"hr/salaries.txt","term":"termination"}`)}}},
		scripted.Step{Response: api.ProviderResponse{Stop: api.StopEnd, Text: "Paragraph 4: 60 days written notice."}},
	)
}

func request(now time.Time) api.ExecutionRequest {
	return api.ExecutionRequest{
		Version: api.ExecutionVersion, ExecutionID: "lease-review-1", AttemptID: "attempt-1",
		Objective: "Find the termination clause in the lease.",
		// read_write only because a command grant is offered; nothing here writes.
		Mode:      api.ModeReadWrite,
		Workspace: api.WorkspaceRef{ID: "document-set", ManifestDigest: api.Digest([]byte(documents["contracts/lease.txt"]))},
		Constraints: api.Constraints{
			RequiredFeatures: []string{api.FeatureTools}, InstructionDigest: api.Digest([]byte("hostport example")),
		},
		Grants: []api.Capability{
			{Handle: "contracts-search", Kind: api.CapabilityFileSearch, Roots: []string{"contracts"}},
			{Handle: "list", Kind: api.CapabilityCommand, Commands: []api.CommandGrant{
				{Name: "list_documents", Argv: []string{"list-documents"}, TimeoutSeconds: 5}}},
		},
		Budget: api.Budget{
			Deadline: now.Add(time.Minute), MaxIterations: 5, MaxToolCalls: 5, MaxInputTokens: 50000,
			MaxOutputTokens: 4000, MaxArtifactBytes: 1 << 20,
		},
		Providers: []api.ProviderBinding{{
			ID: "doc-model", Kind: "local", Model: "fixture", ModelVersion: api.UnknownVersion,
			Features: []string{api.FeatureTools}, ConfigFingerprint: api.Digest([]byte("local fixture")),
			Eligible: true, Isolation: api.IsolationUnproven, ContextWindow: 32000, MaxOutputTokens: 1000,
			CredentialHandle: "doc-model",
		}},
	}
}
