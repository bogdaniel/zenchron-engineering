package runtime

// #63 ships the REFUSAL side of provider handoff, so these tests prove the
// refusal is a governed transition rather than an error string: it records
// everything a successor would have needed, it changes nothing, and it says
// what the operator should do instead.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordedAgentOf reads the durable binding and fails the test if it cannot be
// read, so every assertion below is about the identity itself.
func recordedAgentOf(t *testing.T, state *runState) AgentIdentity {
	t.Helper()
	agent, err := state.recordedAgent()
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func handoffRegistry(t *testing.T) AgentRegistry {
	t.Helper()
	registry, err := OperatorConfig{
		Agents: map[string]AgentConfig{
			"codex":  {Kind: AgentKindCodexCLI, TrustMode: string(TrustOperatorTrusted)},
			"claude": {Kind: AgentKindClaudeCode, TrustMode: string(TrustOperatorTrusted)},
			"openai": {Kind: AgentKindOpenAIResponses, TrustMode: string(TrustProtected), Model: "m", CredentialPath: "/operator/key"},
		},
		DefaultAgent: "codex",
	}.AgentRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

// handoffFixture drives a run to publication under a named agent, with a
// registry that also knows the agent an operator might try to move it to.
func handoffFixture(t *testing.T) (*phase8Fixture, string) {
	t.Helper()
	fixture := newPhase8Fixture(t)
	registry := handoffRegistry(t)
	agent, err := registry.Agent("codex")
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.Agent = agent
	fixture.deps.Agents = registry
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	if outcome := fixture.reconcile(runID); outcome.Disposition == Failed {
		t.Fatalf("run failed before publication: %#v", outcome)
	}
	return fixture, runID
}

// An assignment that cannot be read is not an unbound run.
//
// The zero identity is a PERMISSION here, not an absence: repairAgentBinding
// backfills a binding onto an unbound run from whichever agent is running now,
// and TrustDowngradeRefused compares a zero From against TrustProtected and
// finds no downgrade. Answering an unreadable assignment as "unbound" would
// therefore let a corrupt event rebind a protected run to an operator-trusted
// worker, and journal the rebinding as legitimate. The absence keeps its
// documented legacy meaning; only the unreadable case becomes an error.
func TestAnUnreadableAgentAssignmentIsNotReadAsUnbound(t *testing.T) {
	assigned := func(payload string) *runState {
		return &runState{
			run:    EngineeringRun{ID: "run-1"},
			events: []EngineeringEvent{{Type: EventRunAgentAssigned, Payload: []byte(payload)}},
		}
	}

	unreadable, err := assigned("[]").recordedAgent()
	if err == nil {
		t.Fatalf("an unreadable agent assignment read back as the identity %#v", unreadable)
	}
	if !strings.Contains(err.Error(), "run-1") {
		t.Fatalf("the refusal does not name the run it is about: %q", err.Error())
	}

	// An assignment that IS readable still answers, and a run with none at all
	// still reads back as the legacy zero identity rather than an error.
	readable := recordedAgentOf(t, assigned(`{"agent_id":"codex","provider_kind":"codex_cli","trust_mode":"operator_trusted"}`))
	if readable.AgentID != "codex" || readable.TrustMode != TrustOperatorTrusted {
		t.Fatalf("a readable assignment did not answer: %#v", readable)
	}
	if legacy := recordedAgentOf(t, &runState{run: EngineeringRun{ID: "run-1"}}); legacy.AgentID != "" {
		t.Fatalf("a run with no assignment invented one: %#v", legacy)
	}
	// A journal that recorded the event with NO CONTENT keeps the legacy
	// meaning the previous code gave it. Failing every adoption pass over such
	// a journal would be worse than the answer it already had.
	if empty := recordedAgentOf(t, assigned("")); empty.AgentID != "" {
		t.Fatalf("a zero-length assignment payload was read as an identity: %#v", empty)
	}
}

// TestRunIsBoundToItsAgentInTheJournal is the binding law: which worker a run
// is worked by is answerable from the append-only log alone.
func TestRunIsBoundToItsAgentInTheJournal(t *testing.T) {
	fixture, runID := handoffFixture(t)
	state := fixture.state(runID)
	if countType(state.events, EventRunAgentAssigned) != 1 {
		t.Fatalf("the agent binding was not journalled exactly once: %v", journalTypes(state.events))
	}
	recorded := recordedAgentOf(t, state)
	if recorded.AgentID != "codex" || recorded.Kind != AgentKindCodexCLI || recorded.TrustMode != TrustOperatorTrusted {
		t.Fatalf("the journal does not name the agent this run was created with: %#v", recorded)
	}
	if state.run.AgentID != "codex" {
		t.Fatalf("the run row projection lost the agent: %q", state.run.AgentID)
	}
}

// TestHandoffRefusalCarriesTheWholeTransition is the conformance case the
// normative clarification asks for when in-run handoff is not implemented: the
// typed record must carry the predecessor, the exact candidate, both agents,
// both trust modes, the reason, the budgets and the feedback continuity state.
func TestHandoffRefusalCarriesTheWholeTransition(t *testing.T) {
	fixture, runID := handoffFixture(t)
	before := fixture.state(runID)

	record, err := fixture.runtime.RequestAgentHandoff(runID, "claude", "the codex session was lost")
	var refused *AgentHandoffRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("an agent change was not refused as a typed transition: %v", err)
	}
	if !record.Refused || record.RefusalCode != HandoffNotImplemented {
		t.Fatalf("the record does not state the refusal: %#v", record)
	}
	if record.RunID != runID {
		t.Fatalf("record names run %q, want %q", record.RunID, runID)
	}
	if record.From.AgentID != "codex" || record.To.AgentID != "claude" {
		t.Fatalf("the record does not name both agents: %#v", record)
	}
	if record.From.TrustMode != TrustOperatorTrusted || record.To.TrustMode != TrustOperatorTrusted {
		t.Fatalf("the record does not name both trust modes: %#v", record)
	}
	if record.CandidateRevision != before.projection.CandidateRevision || record.CandidateTree != before.projection.CandidateTree {
		t.Fatalf("the record does not bind the exact candidate: %#v", record)
	}
	if record.Contract != before.projection.Contract {
		t.Fatalf("the record does not bind the governing contract: %#v", record)
	}
	if !strings.Contains(record.Reason, "codex session") {
		t.Fatalf("the operator's stated reason was lost: %q", record.Reason)
	}
	if !strings.Contains(refused.Error(), "new generation") {
		t.Fatalf("the refusal does not say what to do instead: %q", refused.Error())
	}
	if !record.ProviderSessionRetired {
		t.Fatal("the record does not state that the predecessor's provider session is retired")
	}

	// Budgets are carried, and the dimensions no configured worker reports stay
	// UNKNOWN rather than being flattened to zero.
	if !record.Budgets.ExecutionAttempts.Known || !record.Budgets.WallSeconds.Known {
		t.Fatalf("runtime-owned budgets were not carried: %#v", record.Budgets)
	}
	if record.Budgets.Tokens.Known || record.Budgets.Cost.Known {
		t.Fatalf("an unreported budget dimension was invented as a number: %#v", record.Budgets)
	}
	// The RUN TOTAL is one of the carried dimensions. It is a different bound
	// from the per-binding retry allowance beside it, and it is the one a
	// successor cannot recover by starting a fresh binding - a record without
	// it described itself as complete while omitting the only ceiling that
	// spans bindings. This run states none, so it is UNKNOWN rather than zero.
	if record.Budgets.ProviderInvocations.Known {
		t.Fatalf("a run with no stated invocation total reported one: %#v", record.Budgets)
	}
	bounded := fixture.state(runID)
	bounded.run.Budgets = &RunBudgets{MaxProviderInvocations: 5}
	if got := fixture.runtime.remainingBudgets(bounded).ProviderInvocations; !got.Known || got.Remaining != int64(5-bounded.projection.Attempts[OpExecutionInvoke]) {
		t.Fatalf("the stated run total was not carried: %#v, after %d invocations",
			got, bounded.projection.Attempts[OpExecutionInvoke])
	}

	// The refusal mutates nothing.
	after := fixture.state(runID)
	if after.projection.CandidateRevision != before.projection.CandidateRevision ||
		after.projection.CandidateTree != before.projection.CandidateTree {
		t.Fatal("a refused handoff moved the candidate")
	}
	if after.snapshot.Disposition != before.snapshot.Disposition {
		t.Fatalf("a refused handoff changed the disposition: %q -> %q", before.snapshot.Disposition, after.snapshot.Disposition)
	}
	if len(after.projection.EvidenceBundles) != len(before.projection.EvidenceBundles) {
		t.Fatal("a refused handoff changed the evidence")
	}
	if recordedAgentOf(t, after).AgentID != "codex" {
		t.Fatalf("a refused handoff moved the agent binding: %#v", recordedAgentOf(t, after))
	}
	if countType(after.events, EventRunAgentHandoffRefused) != 1 {
		t.Fatalf("the attempted transition was not journalled exactly once: %v", journalTypes(after.events))
	}
}

// TestHandoffNeverLowersTheTrustARunWasGovernedUnder is the privilege law: a
// run created under protected execution is not continued by an operator_trusted
// worker, and the refusal says so distinctly.
func TestHandoffNeverLowersTheTrustARunWasGovernedUnder(t *testing.T) {
	fixture := newPhase8Fixture(t)
	registry := handoffRegistry(t)
	protected, err := registry.Agent("openai")
	if err != nil {
		t.Fatal(err)
	}
	fixture.deps.Agent = protected
	fixture.deps.Agents = registry
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	fixture.reconcile(runID)

	record, err := fixture.runtime.RequestAgentHandoff(runID, "codex", "try a local CLI instead")
	var refused *AgentHandoffRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a trust downgrade was not refused as a typed transition: %v", err)
	}
	if !record.TrustDowngradeRefused() {
		t.Fatalf("the record does not classify the refusal as a trust downgrade: %#v", record)
	}
	if record.RefusalCode != HandoffTrustDowngrade || !strings.Contains(refused.Error(), "protected") {
		t.Fatalf("the refusal does not name the trust requirement: %#v / %q", record, refused.Error())
	}
}

// TestConsumedFeedbackIsCarriedIntoTheTransition proves the continuity fact a
// successor would need: feedback the predecessor already consumed is bound to
// the record, so it could never be replayed as new input.
func TestConsumedFeedbackIsCarriedIntoTheTransition(t *testing.T) {
	fixture, runID := handoffFixture(t)
	// The runtime resolves its own publishing account per observation, so the
	// fake has to have one: without it the guard cannot tell the runtime's
	// comments from anyone else's and correctly admits nothing.
	fixture.forge.ViewerActor = GitHubActor{Login: "zenchron-runtime", ID: 99}
	fixture.deps.Feedback = FeedbackPolicy{}
	fixture.runtime = fixture.newRuntime(fixture.deps)
	fixture.forge.Permissions["maintainer"] = PermissionWrite
	number := fixture.state(runID).projection.PullRequest.Number
	fixture.forge.ConversationComments[number] = []GitHubComment{{
		ID: 1101, Author: GitHubActor{Login: "maintainer", ID: 7},
		Body: UntrustedText("please rename the helper"), CreatedAt: fixture.clock.Now(),
	}}
	if _, err := fixture.runtime.ObserveFeedback(context.Background(), runID); err != nil {
		t.Fatal(err)
	}
	fixture.reconcile(runID)

	state := fixture.state(runID)
	if state.consumedFeedbackCount() == 0 {
		t.Fatalf("the feedback was never delivered, so there is nothing to carry: %v", journalTypes(state.events))
	}
	record, err := fixture.runtime.RequestAgentHandoff(runID, "claude", "operator preference")
	var refused *AgentHandoffRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("the transition was not refused: %v", err)
	}
	if record.FeedbackConsumed != state.consumedFeedbackCount() || record.FeedbackConsumedDigest == "" {
		t.Fatalf("consumed feedback was not bound to the transition: %#v", record)
	}
}

// TestAdoptingALiveGenerationWithAnotherAgentIsRefused is the same law reached
// the way an operator would actually reach it: by running the issue again with
// a different --agent.
func TestAdoptingALiveGenerationWithAnotherAgentIsRefused(t *testing.T) {
	fixture, runID := handoffFixture(t)
	registry := handoffRegistry(t)
	other := fixture.deps
	agent, err := registry.Agent("claude")
	if err != nil {
		t.Fatal(err)
	}
	other.Agent = agent
	successor := fixture.newRuntime(other)

	_, err = successor.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	var refused *AgentHandoffRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("a live generation was silently adopted by a different agent: %v", err)
	}
	if recordedAgentOf(t, fixture.state(runID)).AgentID != "codex" {
		t.Fatal("the refused adoption changed the run's agent binding")
	}
}

// TestALegacyRunIsAdoptedRatherThanRefused keeps the upgrade path open: a run
// created before the agent registry existed has no recorded agent, and that
// absence is its documented legacy meaning rather than a mismatch.
func TestALegacyRunIsAdoptedRatherThanRefused(t *testing.T) {
	fixture := newPhase8Fixture(t)
	runID := fixture.start() // created with no named agent
	fixture.reconcile(runID)

	registry := handoffRegistry(t)
	agent, err := registry.Agent("codex")
	if err != nil {
		t.Fatal(err)
	}
	upgraded := fixture.deps
	upgraded.Agent = agent
	upgraded.Agents = registry
	successor := fixture.newRuntime(upgraded)

	outcome, err := successor.StartIssueRun(context.Background(), fixture.issue, AdoptCompatibleGeneration)
	if err != nil {
		t.Fatalf("a pre-registry run was stranded by the upgrade: %v", err)
	}
	if !outcome.Adopted || outcome.RunID != runID {
		t.Fatalf("the existing run was not adopted: %#v", outcome)
	}
	// The absence is not backfilled with an invented identity.
	if recorded := recordedAgentOf(t, fixture.state(runID)); recorded.AgentID != "" {
		t.Fatalf("an agent identity was invented for a legacy run: %#v", recorded)
	}
}
