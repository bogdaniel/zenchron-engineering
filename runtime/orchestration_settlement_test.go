package runtime

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// tickUntilReported drives one batch until its single item's latest handoff
// is journalled and not yet admitted, which is the window these tests act in.
func tickUntilReported(t *testing.T, fixture *fleetFixture, supervisor *Supervisor, batchID string) {
	t.Helper()
	for range 12 {
		if _, err := supervisor.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if fixture.status(batchID).Items[0].Handoff == orchestration.HandoffReported {
			return
		}
	}
	t.Fatal("the report was never observed before its admission")
}

// TestAnAdmittedPartialHandoffIsNotCompletedWork: admission proves the transfer
// is valid and bound; it does not turn work the worker says is unfinished into
// finished work.
func TestAnAdmittedPartialHandoffIsNotCompletedWork(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(2))
	partial := view.Items[0].RunID
	fixture.worker.set(partial, fleetPartialHandoff)
	settled := fixture.drive(supervisor, view.BatchID)
	item := itemFor(t, settled, partial)
	if item.State != orchestration.ItemPartial || item.Handoff != orchestration.HandoffAdmitted || item.HandoffID == "" {
		t.Fatalf("partial item: state %s handoff %s id %q", item.State, item.Handoff, item.HandoffID)
	}
	if !strings.Contains(item.Reason, "the migration") {
		t.Fatalf("the unresolved work is not visible: %q", item.Reason)
	}
	if settled.Counts.Partial != 1 || settled.Counts.Completed != 1 {
		t.Fatalf("counts = %+v", settled.Counts)
	}
}

// TestAHandoffIsFinalizedAfterItsAgentIsRetired: finalizing a report already
// produced needs no execution authority. A supervisor whose registry no longer
// has the batch's agent still admits it - and invokes nothing new.
func TestAHandoffIsFinalizedAfterItsAgentIsRetired(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	view := fixture.orchestrate(fixture.supervisor(), "claude", fleetIssues(1))
	tickUntilReported(t, fixture, fixture.supervisor(), view.BatchID)
	fixture.worker.mu.Lock()
	invoked := fixture.worker.invocations[view.Items[0].RunID]
	fixture.worker.mu.Unlock()

	registry, err := OperatorConfig{
		Agents:       map[string]AgentConfig{"codex": {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)}},
		DefaultAgent: "codex",
	}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	repo, err := ParseGitHubRepo("acme/repo")
	if err != nil {
		t.Fatal(err)
	}
	retired, err := NewSupervisor(SupervisorDependencies{
		Store: fixture.store, Clock: fixture.clock, Owner: "owner-1", StateDir: fixture.stateDir,
		Liveness: fixture.deps.Liveness, Repositories: []GitHubRepo{repo}, MaxConcurrentRuns: 10,
		PollInterval: time.Minute, Agents: registry,
		Runtime: func(_ GitHubRepo, agent ResolvedAgent) (*EngineeringRuntime, error) {
			deps := fixture.deps
			deps.Agent, deps.Agents = agent, registry
			return NewEngineeringRuntime(deps)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := retired.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	item := fixture.status(view.BatchID).Items[0]
	if item.State != orchestration.ItemCompleted || item.Handoff != orchestration.HandoffAdmitted {
		t.Fatalf("after the agent was retired: state %s handoff %s reason %q", item.State, item.Handoff, item.Reason)
	}
	fixture.worker.mu.Lock()
	defer fixture.worker.mu.Unlock()
	if fixture.worker.invocations[view.Items[0].RunID] != invoked {
		t.Fatal("finalizing a handoff invoked the retired agent's worker again")
	}
}

// TestAHandoffChangedAfterItWasJournalledIsRefusedOnce: admission re-reads
// the slot, and a document no longer byte-identical to the one journalled at
// completion is durably refused - once - so the item settles to
// handoff_pending with the reason instead of reading as running forever.
func TestAHandoffChangedAfterItWasJournalledIsRefusedOnce(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	supervisor := fixture.supervisor()
	view := fixture.orchestrate(supervisor, "claude", fleetIssues(1))
	runID := view.Items[0].RunID
	tickUntilReported(t, fixture, supervisor, view.BatchID)
	path := fixture.worker.request(runID).HandoffPath
	rewritten := `{"schema_version":"0.1","outcome":"completed","summary":"Something else entirely."}`
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		report, err := supervisor.Tick(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		fixture.clock.advance(61 * time.Second)
		if len(report.Orchestration) != 0 {
			t.Fatalf("a settled refusal is still reported as a problem every pass: %q", report.Orchestration)
		}
	}
	item := fixture.status(view.BatchID).Items[0]
	if item.State != orchestration.ItemHandoffPending || item.Handoff != orchestration.HandoffRefused ||
		!strings.Contains(item.Reason, "no longer the document journalled") {
		t.Fatalf("state %s handoff %s reason %q", item.State, item.Handoff, item.Reason)
	}
	if handoffs, err := fixture.store.RunHandoffs(runID); err != nil || len(handoffs) != 0 {
		t.Fatalf("%d handoffs admitted (%v)", len(handoffs), err)
	}
	if refusals, err := fixture.store.RunHandoffRefusals(runID); err != nil || len(refusals) != 1 {
		t.Fatalf("%d refusals recorded (%v), want exactly one", len(refusals), err)
	}
}

// TestOrchestrationRefusesAWorkerThatCannotWriteItsHandoff: the mandatory
// handoff is a capability precondition, asked of the provider generically.
func TestOrchestrationRefusesAWorkerThatCannotWriteItsHandoff(t *testing.T) {
	fixture := newFleetFixture(t, 10)
	fixture.deps.Provider = newIsolatedProvider(nil) // no TypedResultWriter
	_, err := fixture.supervisor().Orchestrate(context.Background(), ControlRequest{
		Repository: "acme/repo", Agent: "claude", Issues: fleetIssues(2),
	})
	if err == nil || !strings.Contains(err.Error(), "typed result directory") {
		t.Fatalf("err = %v, want a capability refusal", err)
	}
	if batches, err := fixture.store.OrchestrationBatches(); err != nil || len(batches) != 0 {
		t.Fatalf("a refused batch was written: %d (%v)", len(batches), err)
	}
	// The CLI adapters answer from the argv they would actually run.
	for kind, want := range map[string]bool{
		AgentKindClaudeCode: true, AgentKindCodexCLI: true, AgentKindGeminiCLI: false, AgentKindQwenCLI: false,
	} {
		if got := (CLIAgentProvider{Agent: ResolvedAgent{Kind: kind}}).WritesTypedResults(); got != want {
			t.Errorf("%s writes typed results = %t, want %t", kind, got, want)
		}
	}
}
