package planning_test

// Independence evidence when several producers share one class.
//
// `different_from` and `other_classes` answer different questions. The first is
// WHICH producer stages a stage had to be independent of, exactly as the plan
// named them. The second is WHICH comparison classes those producers actually
// occupied - a set, and agent-assignment.schema.json declares it one.
//
// Two producers can legitimately occupy one class: two codex implementers under a
// single claude reviewer is the ordinary shape of a decomposed plan, and it is
// precisely the shape the #119 planner produced the first time it stopped
// collapsing the cohort into one stage. Emitting that class once per peer claimed
// a set with a duplicate member, so the assignment failed schema validation and
// `plan show` could not render a revision that was otherwise valid and already
// stored.
//
// Eligibility is NOT what changed here. independenceViolation still evaluates
// every peer separately; these tests hold both halves at once.

import (
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// twoProducerPlan is the real #119 r3 shape: two implementers with no dependency
// between them, one reviewer that must be execution-agent independent of both,
// and the assurance gate the contract's claim requires.
func twoProducerPlan(t *testing.T, dimension domain.IndependenceDimension) domain.EngineeringPlan {
	t.Helper()
	input := planInput(t, "security-sensitive.engineering-fact.json", nil)
	input.Proposed = []domain.PlanStage{
		{ID: "producer-a", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Correct the approval surfaces.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "producer-b", Kind: domain.StageAgent, Role: domain.RoleImplementer,
			Objective:            "Harden the runtime provenance.",
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityCodeChange}},
		{ID: "reviewer", Kind: domain.StageAgent, Role: domain.RoleReviewer,
			Objective: "Verify the combined candidate.", DependsOn: []string{"producer-a", "producer-b"},
			InvocationMode:       domain.InvocationModeMutating,
			RequiresCapabilities: []domain.EngineeringCapability{domain.CapabilityVerification, domain.CapabilitySecurityReview},
			Independence: &domain.IndependenceRequirement{
				Dimension: dimension, DifferentFrom: []string{"producer-a", "producer-b"},
			}},
	}
	return compilePlan(t, input)
}

// The positive regression: two same-class producers collapse to ONE class in the
// evidence, both peers stay named, and the assignment passes the schema.
func TestTwoProducersSharingOneClassRecordThatClassOnce(t *testing.T) {
	plan := twoProducerPlan(t, domain.IndependenceExecutionAgent)
	input := resolveInput(t, plan, claudeAgent(), codexAgent())
	// codex is the operator's default, so both producers take it and the reviewer
	// has to leave it. That is the duplicate-class shape.
	input.DefaultAgent = "codex"
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(resolution.Blocked) != 0 {
		t.Fatalf("the plan blocked: %#v", resolution.Blocked)
	}
	for _, producer := range []string{"producer-a", "producer-b"} {
		assignment, ok := resolution.Assignment(producer)
		if !ok {
			t.Fatalf("no assignment for %q: %#v", producer, resolution.Assignments)
		}
		if assignment.Agent.ID != "codex" {
			t.Fatalf("the fixture does not reproduce two same-class producers: %q took %q", producer, assignment.Agent.ID)
		}
	}
	reviewer, ok := resolution.Assignment("reviewer")
	if !ok {
		t.Fatalf("no reviewer assignment: %#v", resolution.Assignments)
	}
	if reviewer.Agent.ID != "claude" {
		t.Fatalf("the reviewer is not independent of the producers: %q", reviewer.Agent.ID)
	}
	if len(reviewer.Independence) != 1 {
		t.Fatalf("the reviewer records no independence binding: %#v", reviewer.Independence)
	}
	binding := reviewer.Independence[0]

	// BOTH peers stay named: two stages are two obligations, whatever class they
	// occupied.
	if len(binding.DifferentFrom) != 2 {
		t.Fatalf("a peer stage was deduplicated away: %#v", binding.DifferentFrom)
	}
	if binding.DifferentFrom[0] != "producer-a" || binding.DifferentFrom[1] != "producer-b" {
		t.Fatalf("the peers are not the stages the plan named: %#v", binding.DifferentFrom)
	}
	// ONE class, because both peers occupied one.
	if len(binding.OtherClasses) != 1 {
		t.Fatalf("other_classes is not a set: %#v", binding.OtherClasses)
	}
	if binding.OtherClasses[0] != "codex" {
		t.Fatalf("the recorded class is wrong: %#v", binding.OtherClasses)
	}
	if binding.Class != "claude" {
		t.Fatalf("the reviewer's own class is wrong: %q", binding.Class)
	}
	if binding.SatisfiedBy != domain.IndependenceSatisfiedByWorker {
		t.Fatalf("independence satisfied by %q", binding.SatisfiedBy)
	}

	// And the assignment ENCODES: this is the boundary that refused the stored
	// #119 revision and made `plan show` fail.
	if _, err := domain.Encode(reviewer); err != nil {
		t.Fatalf("the assignment does not pass its own schema: %v", err)
	}
}

// The negative regression: the dedupe must not make independence satisfiable by
// collapsing the reviewer's own class into the producers'.
//
// With codex the only registered agent, both producers and the reviewer would
// share one class. Eligibility still evaluates every peer, so the stage BLOCKS
// rather than resolving onto a one-element set that looks independent.
func TestOneClassForEveryoneStillRefusesIndependence(t *testing.T) {
	plan := twoProducerPlan(t, domain.IndependenceExecutionAgent)
	input := resolveInput(t, plan, codexAgent())
	input.DefaultAgent = "codex"
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := resolution.Assignment("reviewer"); ok {
		t.Fatal("the reviewer was assigned the producers' own agent")
	}
	if len(resolution.Blocked) == 0 {
		t.Fatal("a reviewer with no independent worker did not block")
	}
	found := false
	for _, blocked := range resolution.Blocked {
		if blocked.StageID != "reviewer" {
			continue
		}
		found = true
		if !strings.Contains(strings.ToLower(blocked.Reason), "independent") {
			t.Fatalf("the block does not name the independence shortage: %#v", blocked)
		}
	}
	if !found {
		t.Fatalf("the reviewer is neither assigned nor blocked: %#v", resolution.Blocked)
	}
}

// An unresolved peer is still unprovable rather than vacuously satisfied. The
// canonicalization drops blanks, so a peer that contributed no class must not
// look like a peer that was compared and passed.
func TestAPeerWithNoResolvedWorkerIsStillUnprovable(t *testing.T) {
	plan := twoProducerPlan(t, domain.IndependenceExecutionAgent)
	// The reviewer names a peer that is not in the plan at all, so nothing can
	// resolve it and no class can come from it.
	for i, stage := range plan.Stages {
		if stage.ID != "reviewer" {
			continue
		}
		plan.Stages[i].Independence = &domain.IndependenceRequirement{
			Dimension:     domain.IndependenceExecutionAgent,
			DifferentFrom: []string{"producer-a", "producer-that-does-not-exist"},
		}
	}
	input := resolveInput(t, plan, claudeAgent(), codexAgent())
	input.DefaultAgent = "codex"
	resolution, err := planning.Resolve(input)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, ok := resolution.Assignment("reviewer"); ok {
		t.Fatal("independence against an unresolvable peer was treated as satisfied")
	}
}

// The canonicalization is DIMENSION-INDEPENDENT: the dimension decides what a
// class is, never how a set of them is written down.
//
// Each of these puts both producers in one class of its dimension and a reviewer
// outside it, and asserts the same one-element set and a passing schema.
func TestDuplicateClassesCollapseInEveryDimension(t *testing.T) {
	for _, dimension := range []struct {
		dimension domain.IndependenceDimension
		expected  string
		reviewer  string
	}{
		{domain.IndependenceExecutionAgent, "codex", "claude"},
		{domain.IndependenceProviderKind, "codex_cli", "claude_code"},
		{domain.IndependenceVendorFamily, "openai", "anthropic"},
		// The agent_profile class carries the profile DIGEST, so it is matched by
		// prefix: what matters here is that two peers sharing one profile produce
		// one member, not how the class spells itself.
		{domain.IndependenceAgentProfile, "codex@", "claude@"},
	} {
		t.Run(string(dimension.dimension), func(t *testing.T) {
			plan := twoProducerPlan(t, dimension.dimension)
			input := resolveInput(t, plan, claudeAgent(), codexAgent())
			input.DefaultAgent = "codex"
			resolution, err := planning.Resolve(input)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if len(resolution.Blocked) != 0 {
				t.Fatalf("the plan blocked: %#v", resolution.Blocked)
			}
			reviewer, ok := resolution.Assignment("reviewer")
			if !ok {
				t.Fatalf("no reviewer assignment: %#v", resolution.Assignments)
			}
			if len(reviewer.Independence) != 1 {
				t.Fatalf("no independence binding: %#v", reviewer.Independence)
			}
			binding := reviewer.Independence[0]
			if len(binding.DifferentFrom) != 2 {
				t.Fatalf("a peer stage was deduplicated away: %#v", binding.DifferentFrom)
			}
			if len(binding.OtherClasses) != 1 || !strings.HasPrefix(binding.OtherClasses[0], dimension.expected) {
				t.Fatalf("the %s set is wrong: %#v", dimension.dimension, binding.OtherClasses)
			}
			if !strings.HasPrefix(binding.Class, dimension.reviewer) {
				t.Fatalf("the reviewer's own %s class is %q", dimension.dimension, binding.Class)
			}
			// And the two sides are genuinely different classes, whatever the
			// dimension spells them as.
			if binding.Class == binding.OtherClasses[0] {
				t.Fatalf("the %s comparison collapsed both sides into %q", dimension.dimension, binding.Class)
			}
			if _, err := domain.Encode(reviewer); err != nil {
				t.Fatalf("the assignment does not pass its own schema: %v", err)
			}
		})
	}
}
