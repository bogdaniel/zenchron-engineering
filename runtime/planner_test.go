package runtime

// The reasoning planner boundary.
//
// Every test here drives a FAKE provider. Nothing starts a coding CLI and
// nothing spends money; what is under test is the runtime's own behaviour
// around the invocation - the workspace it materializes, the verification it
// performs afterwards, and what it refuses to believe.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// fakePlanningProvider models a registered agent performing a planning
// invocation. It writes its answer as a transcript exactly as a real adapter
// does, because reading the answer back from the durable transcript is part of
// what is under test.
type fakePlanningProvider struct {
	answer string
	// writes is what a MISBEHAVING provider leaves in the workspace. A real
	// one cannot, because the provider mode forbids it - which is precisely why
	// the runtime verifies instead of trusting.
	writes map[string]string
	// failure makes the provider report a typed failure.
	failure *ProviderFailure
	// err makes the invocation itself fail.
	err error

	artifacts ArtifactStore
	requests  []ExecutionRequest
}

func (p *fakePlanningProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	p.requests = append(p.requests, request)
	for name, body := range p.writes {
		path := filepath.Join(request.CandidateDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return ExecutionResult{}, err
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			return ExecutionResult{}, err
		}
	}
	artifacts, err := p.artifacts.StoreExecutionAttemptTranscript("planner", request.AttemptRef(), []byte(p.answer), nil)
	if err != nil {
		return ExecutionResult{}, err
	}
	result := ExecutionResult{
		ProviderID: "planner", Attempt: request.Attempt, Outcome: Succeeded, Artifacts: artifacts,
		Invocation: &InvocationProvenance{PermissionMode: "read-only"},
		Failure:    p.failure,
	}
	if p.failure != nil {
		result.Outcome = OperationFailed
	}
	return result, p.err
}

// plannerFixture builds a real Git checkout to plan over, so the workspace
// measurement is measuring an actual working tree.
func plannerFixture(t *testing.T, answer string) (PlannerInput, *fakePlanningProvider) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSourceFile(t, source, "README.md", "# subject\n")
	writeSourceFile(t, source, "internal/auth/session.go", "package auth\n")
	commit, tree := commitSource(t, source)

	stateDir := filepath.Join(root, "state")
	workspace, err := CreatePlanningWorkspace(stateDir, "plan-1", source, commit, tree)
	if err != nil {
		t.Fatalf("materialize planning workspace: %v", err)
	}
	artifacts := ArtifactStore{Root: filepath.Join(stateDir, "artifacts")}
	provider := &fakePlanningProvider{answer: answer, artifacts: artifacts}
	return PlannerInput{
		PlanID: "plan-1", Revision: 1, Attempt: 1,
		Agent:          ResolvedAgent{ID: "claude", Kind: AgentKindClaudeCode, TrustMode: TrustOperatorTrusted, Model: "sonnet"},
		Provider:       provider,
		Workspace:      workspace,
		Contract:       domain.EngineeringWorkContract{ID: "contract-1", Revision: "1", AcceptanceIntent: []string{"Works."}},
		Objective:      "Add enterprise SSO.",
		Base:           Ref{ID: "main", Revision: commit},
		SourceSnapshot: Ref{ID: "issue-1", Revision: "1"},
		ControllerID:   "controller",
		AvailableRoles: domain.EngineeringRoles(), AvailableCapabilities: domain.EngineeringCapabilities(),
		Artifacts: artifacts,
	}, provider
}

func writeSourceFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commitSource(t *testing.T, dir string) (string, string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.email", "planner@test"},
		{"config", "user.name", "planner"},
		{"add", "-A"},
		{"commit", "-m", "subject"},
	} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	commit, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(commit), strings.TrimSpace(tree)
}

const goodAnswer = `I reviewed the workspace and propose the following.

` + "```json" + `
{"stages": [
  {"id": "implementation", "kind": "agent", "role": "implementer",
   "objective": "Implement the SSO exchange.", "requires_capabilities": ["code_change"],
   "rationale": "the change has to be written"},
  {"id": "review", "kind": "agent", "role": "security_reviewer", "depends_on": ["implementation"],
   "objective": "Review the session boundary.", "requires_capabilities": ["security_review"],
   "independence": {"dimension": "vendor_family", "different_from": ["implementation"]},
   "rationale": "the change crosses an authentication boundary"}
],
 "notes": "Two stages; the review must come from another vendor."}
` + "```"

func TestPlanningInvocationVerifiesTheWorkspaceItself(t *testing.T) {
	input, provider := plannerFixture(t, goodAnswer)
	output, err := InvokePlanner(context.Background(), input)
	if err != nil {
		t.Fatalf("planning invocation: %v", err)
	}
	if !output.Reasoning.WorkspaceUnchanged {
		t.Fatal("an unchanged workspace was not recorded as unchanged")
	}
	if output.Reasoning.WorkspaceDigestBefore != output.Reasoning.WorkspaceDigestAfter {
		t.Fatal("the two measurements disagree while the verification passed")
	}
	if len(output.Reasoning.WorkspaceDigestBefore) != 64 {
		t.Fatalf("workspace digest is not a sha256: %q", output.Reasoning.WorkspaceDigestBefore)
	}
	if output.Reasoning.InvocationMode != domain.InvocationModeNonMutatingPlanning {
		t.Fatalf("recorded invocation mode = %q", output.Reasoning.InvocationMode)
	}
	if output.Reasoning.ProviderMode != "read-only" {
		t.Fatalf("the provider's own mode was not recorded: %q", output.Reasoning.ProviderMode)
	}
	if output.Reasoning.VendorFamily != "anthropic" {
		t.Fatalf("vendor family = %q", output.Reasoning.VendorFamily)
	}
	// The request the provider received IS a planning request, and it carries
	// no permission to change anything.
	if len(provider.requests) != 1 {
		t.Fatalf("expected one invocation, got %d", len(provider.requests))
	}
	request := provider.requests[0]
	if request.Purpose != InvocationPlanning || request.Mode != domain.InvocationModeNonMutatingPlanning {
		t.Fatalf("request purpose/mode = %q/%q", request.Purpose, request.Mode)
	}
	if request.CandidateDir != input.Workspace.Dir {
		t.Fatalf("the planner was given %q rather than the runtime-owned planning workspace", request.CandidateDir)
	}
}

// A provider that wrote to the workspace broke the boundary its mode promised.
// Its answer is refused rather than parsed - the verification exists precisely
// because a provider's claim about itself is not evidence.
func TestAProviderThatWritesDuringPlanningIsRefused(t *testing.T) {
	input, provider := plannerFixture(t, goodAnswer)
	provider.writes = map[string]string{"internal/auth/session.go": "package auth // edited\n"}

	output, err := InvokePlanner(context.Background(), input)
	var workspaceErr *PlanningWorkspaceError
	if !errors.As(err, &workspaceErr) {
		t.Fatalf("expected a typed workspace refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "did not hold") {
		t.Fatalf("the refusal does not name what failed: %v", err)
	}
	if output.Reasoning.WorkspaceUnchanged {
		t.Fatal("a changed workspace was recorded as unchanged")
	}
	if len(output.Stages) != 0 {
		t.Fatal("the proposal of a provider that broke the boundary was returned anyway")
	}
}

// An untracked file is a change too. A Git-only check would miss it, which is
// why the measurement walks the tree.
func TestAnUntrackedFileCountsAsAChangedWorkspace(t *testing.T) {
	input, provider := plannerFixture(t, goodAnswer)
	provider.writes = map[string]string{"notes/scratch.md": "thinking out loud\n"}
	if _, err := InvokePlanner(context.Background(), input); err == nil || !strings.Contains(err.Error(), "changed during a non-mutating invocation") {
		t.Fatalf("expected an untracked write to be detected, got %v", err)
	}
}

func TestPlannerProposalIsTranslatedIntoTheRuntimeVocabulary(t *testing.T) {
	input, _ := plannerFixture(t, goodAnswer)
	output, err := InvokePlanner(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Stages) != 2 {
		t.Fatalf("stages = %#v", output.Stages)
	}
	if output.Stages[0].Role != domain.RoleImplementer || output.Stages[1].Role != domain.RoleSecurityReviewer {
		t.Fatalf("roles = %q, %q", output.Stages[0].Role, output.Stages[1].Role)
	}
	if output.Stages[1].Independence == nil || output.Stages[1].Independence.Dimension != domain.IndependenceVendorFamily {
		t.Fatalf("independence = %#v", output.Stages[1].Independence)
	}
	if output.Notes == "" {
		t.Fatal("the model's operator-facing note was dropped")
	}
}

// Out-of-vocabulary answers are refused HERE, where an operator can act on
// them, rather than becoming a plan nothing can resolve.
func TestPlannerAnswersOutsideTheVocabularyAreRefused(t *testing.T) {
	cases := []struct {
		name   string
		answer string
		detail string
	}{
		{
			name:   "no JSON at all",
			answer: "I think we should probably split this into a few parts.",
			detail: "no JSON object with a stages member",
		},
		{
			name:   "unknown role",
			answer: "```json\n{\"stages\":[{\"id\":\"a\",\"kind\":\"agent\",\"role\":\"release_manager\"}]}\n```",
			detail: "not in the role catalogue",
		},
		{
			name:   "unknown capability",
			answer: "```json\n{\"stages\":[{\"id\":\"a\",\"kind\":\"agent\",\"role\":\"implementer\",\"requires_capabilities\":[\"deploy_production\"]}]}\n```",
			detail: "not in the v0 ontology",
		},
		{
			name:   "a stage kind that would nest a plan",
			answer: "```json\n{\"stages\":[{\"id\":\"a\",\"kind\":\"plan\"}]}\n```",
			detail: "not a stage kind",
		},
		{
			name:   "an unknown member smuggled into a stage",
			answer: "```json\n{\"stages\":[{\"id\":\"a\",\"kind\":\"agent\",\"role\":\"implementer\",\"permissions\":[\"secret.read\"]}]}\n```",
			detail: "not the stated JSON object",
		},
		{
			name:   "no stages",
			answer: "```json\n{\"stages\":[]}\n```",
			detail: "proposed no stages",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input, _ := plannerFixture(t, tc.answer)
			_, err := InvokePlanner(context.Background(), input)
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("expected a refusal containing %q, got %v", tc.detail, err)
			}
		})
	}
}

// The last complete answer wins, because a model that restates its proposal
// ends with the one it means.
func TestTheLastStatedProposalIsTheOneRead(t *testing.T) {
	answer := "```json\n{\"stages\":[{\"id\":\"first\",\"kind\":\"agent\",\"role\":\"implementer\"}]}\n```\n" +
		"On reflection:\n```json\n{\"stages\":[{\"id\":\"second\",\"kind\":\"agent\",\"role\":\"implementer\"}]}\n```"
	input, _ := plannerFixture(t, answer)
	output, err := InvokePlanner(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if len(output.Stages) != 1 || output.Stages[0].ID != "second" {
		t.Fatalf("stages = %#v", output.Stages)
	}
}

// A provider failure is reported with its provenance, and the workspace
// verification still happened: a failed invocation that wrote is still a broken
// boundary.
func TestAFailedPlanningInvocationStillReportsItsVerification(t *testing.T) {
	input, provider := plannerFixture(t, goodAnswer)
	provider.failure = &ProviderFailure{Classification: FailureProviderQuota}
	output, err := InvokePlanner(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), string(FailureProviderQuota)) {
		t.Fatalf("expected the typed provider failure, got %v", err)
	}
	if !output.Reasoning.WorkspaceUnchanged || output.Reasoning.WorkspaceDigestAfter == "" {
		t.Fatalf("the verification did not run for a failed invocation: %#v", output.Reasoning)
	}
}

// The planning workspace is materialized at the EXACT trusted snapshot, and it
// is not the controller checkout.
func TestPlanningWorkspaceIsTheExactTrustedSnapshot(t *testing.T) {
	input, _ := plannerFixture(t, goodAnswer)
	commit, err := gitOutput(input.Workspace.Dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(commit) != input.Workspace.Commit {
		t.Fatalf("workspace is at %q, want %q", strings.TrimSpace(commit), input.Workspace.Commit)
	}
	if _, err := os.Stat(filepath.Join(input.Workspace.Dir, "internal", "auth", "session.go")); err != nil {
		t.Fatalf("the trusted source is not present in the workspace: %v", err)
	}
	// Two measurements of an untouched workspace agree, which is what makes a
	// difference meaningful.
	first, err := input.Workspace.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := input.Workspace.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("measuring an unchanged workspace twice produced two digests")
	}
}

// The answer is located in a single pass. The earlier scanner restarted at
// every unclosed brace, which is quadratic in exactly the input a coding CLI
// produces - echoed source code - so an ordinary transcript could stall
// planning for minutes before anything was parsed.
func TestThePlannerAnswerIsLocatedInOnePass(t *testing.T) {
	answer := `{"stages": [{"id": "implementation", "kind": "agent", "role": "implementer", "objective": "do it"}]}`
	// Half a megabyte of unclosed braces, as a CLI echoing code produces.
	noise := strings.Repeat("if x { log(\"a\n", 40000)

	started := time.Now()
	found, err := extractJSONObject(noise + "\n" + answer)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("the answer was not found after %d bytes of noise: %v", len(noise), err)
	}
	if found != answer {
		t.Fatalf("located %q, want the answer itself", found)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("locating the answer took %s: the scan is not linear", elapsed)
	}

	// A candidate closed inside an unbalanced outer region still counts: the
	// surrounding noise is the CLI's, not the model's.
	inner := `{"stages": []}`
	if found, err := extractJSONObject(`{ source ` + inner); err != nil || found != inner {
		t.Fatalf("an answer inside an unclosed region was missed: %q %v", found, err)
	}
}
