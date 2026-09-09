package planning_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

// An operator defines a reusable agent by writing files into a directory they
// control. These tests are the trust boundary of that mechanism: what is
// accepted, what is sealed, and what is refused.

func TestRegistryLoadsAndSealsOperatorArtifacts(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "instructions/security-review-core.json", `{
	  "instructions": [
	    "Review the candidate diff for authentication and secret-handling defects.",
	    "Do not modify the candidate: this profile reviews, it does not implement."
	  ]
	}`)
	writeArtifact(t, dir, "context/independent-review.json", `{
	  "exclude": ["producer_reasoning"]
	}`)
	writeArtifact(t, dir, "profiles/zenchron-security-reviewer.json", `{
	  "version": 3,
	  "execution_agent": "claude",
	  "model": "sonnet",
	  "capabilities": ["repository_analysis", "security_review"],
	  "instructions": ["security-review-core"],
	  "context_policy": "independent-review",
	  "trust_requirement": "operator_trusted",
	  "constraints": {"deny_permission_bypass": true}
	}`)
	writeArtifact(t, dir, "templates/zenchron-feature.json", `{
	  "description": "Architecture, implementation, independent review.",
	  "stages": [
	    {"id": "architecture", "kind": "agent", "role": "system_architect"},
	    {"id": "implementation", "kind": "agent", "role": "implementer", "depends_on": ["architecture"]},
	    {"id": "review", "kind": "agent", "role": "reviewer", "depends_on": ["implementation"],
	     "independence": {"dimension": "vendor_family", "different_from": ["implementation"]},
	     "profile": "zenchron-security-reviewer"}
	  ]
	}`)

	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	profile, err := registry.Profile("zenchron-security-reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if profile.ID != "zenchron-security-reviewer" || profile.Version != 3 {
		t.Fatalf("profile identity = %q v%d", profile.ID, profile.Version)
	}
	// The loader seals: it fills the provenance it can prove and the digest it
	// computes, so an operator never hand-writes a hash and the artifact still
	// arrives content-addressed.
	if profile.Source.Type != domain.SourceOperatorFile || !strings.HasSuffix(profile.Source.Location, "profiles/zenchron-security-reviewer.json") {
		t.Fatalf("profile source = %#v", profile.Source)
	}
	computed, err := profile.ContentDigest()
	if err != nil {
		t.Fatal(err)
	}
	if profile.Digest != computed || len(profile.Digest) != 64 {
		t.Fatalf("profile digest = %q, computed %q", profile.Digest, computed)
	}

	instructions, err := registry.Instructions(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(instructions) != 2 || !strings.Contains(instructions[0], "authentication") {
		t.Fatalf("instructions = %#v", instructions)
	}
	binding, err := registry.Binding(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(binding.Instructions) != 1 || binding.Instructions[0].Digest == "" {
		t.Fatalf("binding froze no instruction digest: %#v", binding)
	}
	if binding.ContextPolicy == nil || binding.ContextPolicy.ID != "independent-review" {
		t.Fatalf("binding froze no context policy: %#v", binding)
	}
	if got := registry.TemplateIDs(); !reflect.DeepEqual(got, []string{"zenchron-feature"}) {
		t.Fatalf("templates = %v", got)
	}
}

// An absent directory is an empty registry. An operator who has defined no
// custom agents has a valid configuration.
func TestAbsentRegistryDirectoryIsEmptyRatherThanAnError(t *testing.T) {
	registry, err := planning.LoadRegistry(filepath.Join(t.TempDir(), "not-created"))
	if err != nil {
		t.Fatalf("absent directory: %v", err)
	}
	if len(registry.ProfileIDs()) != 0 || len(registry.TemplateIDs()) != 0 {
		t.Fatal("an absent directory produced artifacts")
	}
}

func TestRegistryRefusals(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		detail string
	}{
		{
			name: "document id disagrees with its file name",
			files: map[string]string{
				"profiles/reviewer.json": `{"id": "something-else", "execution_agent": "claude", "capabilities": ["repository_analysis"]}`,
			},
			detail: "the file name is the identity",
		},
		{
			name: "stated digest does not match the content",
			files: map[string]string{
				"instructions/core.json": `{"digest": "0000000000000000000000000000000000000000000000000000000000000000", "instructions": ["Review carefully."]}`,
			},
			detail: "content digests to",
		},
		{
			name: "profile names an instruction pack nobody installed",
			files: map[string]string{
				"profiles/reviewer.json": `{"execution_agent": "claude", "capabilities": ["repository_analysis"], "instructions": ["absent"]}`,
			},
			detail: `names instruction pack "absent", which is not installed`,
		},
		{
			name: "profile names a context policy nobody installed",
			files: map[string]string{
				"profiles/reviewer.json": `{"execution_agent": "claude", "capabilities": ["repository_analysis"], "context_policy": "absent"}`,
			},
			detail: `names context policy "absent", which is not installed`,
		},
		{
			// A ContextPolicy narrows. Erasing the governance envelope would
			// leave a worker unable to see the obligations it is held to.
			name: "context policy excludes a required class",
			files: map[string]string{
				"context/thin.json": `{"exclude": ["obligations"]}`,
			},
			detail: "may never remove the governance envelope",
		},
		{
			name: "context policy includes a set that omits a required class",
			files: map[string]string{
				"context/thin.json": `{"include": ["objective", "candidate_diff"]}`,
			},
			detail: "omits required context class",
		},
		{
			// One worker never inherits another's hidden reasoning: that is
			// what makes an independent review independent.
			name: "context policy asks for the producer's reasoning transcript",
			files: map[string]string{
				"context/leaky.json": `{"include": ["objective", "acceptance_criteria", "obligations", "permissions", "prohibitions", "producer_reasoning"]}`,
			},
			detail: "hidden reasoning transcript is never delivered",
		},
		{
			name: "template stage depends on a stage that does not exist",
			files: map[string]string{
				"templates/broken.json": `{"stages": [{"id": "a", "kind": "agent", "role": "implementer", "depends_on": ["ghost"]}]}`,
			},
			detail: "which is not a stage in this plan",
		},
		{
			name: "template dependencies form a cycle",
			files: map[string]string{
				"templates/loop.json": `{"stages": [
				  {"id": "a", "kind": "agent", "role": "implementer", "depends_on": ["b"]},
				  {"id": "b", "kind": "agent", "role": "reviewer", "depends_on": ["a"]}
				]}`,
			},
			detail: "form a cycle",
		},
		{
			// A gate creates no EngineeringRun. A gate with a role is a fake
			// worker run wearing a gate's name - refused by the SCHEMA, which
			// is why the detail names the document rather than the graph law
			// that says the same thing one layer further in.
			name: "gate names a role",
			files: map[string]string{
				"templates/fake-worker.json": `{"stages": [{"id": "g", "kind": "assurance_gate", "role": "reviewer", "required_claims": ["claim-x"]}]}`,
			},
			detail: "validate encoded engineering-plan-template",
		},
		{
			name: "assurance gate proves nothing",
			files: map[string]string{
				"templates/empty-gate.json": `{"stages": [{"id": "g", "kind": "assurance_gate"}]}`,
			},
			detail: "nothing would ever satisfy it",
		},
		{
			// A worker is never an independent human leg.
			name: "agent stage requires human independence",
			files: map[string]string{
				"templates/human-worker.json": `{"stages": [
				  {"id": "impl", "kind": "agent", "role": "implementer"},
				  {"id": "review", "kind": "agent", "role": "reviewer", "depends_on": ["impl"],
				   "independence": {"dimension": "human", "different_from": ["impl"]}}
				]}`,
			},
			detail: "a worker is never an independent human leg",
		},
		{
			name: "template prefers a profile nobody installed",
			files: map[string]string{
				"templates/preference.json": `{"stages": [{"id": "a", "kind": "agent", "role": "implementer", "profile": "absent"}]}`,
			},
			detail: `prefers profile "absent", which is not installed`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				writeArtifact(t, dir, name, body)
			}
			_, err := planning.LoadRegistry(dir)
			if err == nil || !strings.Contains(err.Error(), tc.detail) {
				t.Fatalf("expected refusal containing %q, got %v", tc.detail, err)
			}
		})
	}
}

func TestProfilesMayNarrowButNeverEscalate(t *testing.T) {
	claude := domain.ExecutionAgentDescriptor{
		ID: "claude", ProviderKind: "claude_code", VendorFamily: "anthropic",
		TrustMode:    domain.TrustRequirementOperatorTrusted,
		Capabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis, domain.CapabilitySecurityReview, domain.CapabilityCodeChange},
		Available:    true,
	}
	narrowing := domain.AgentProfile{
		ID: "reviewer", Version: 1, ExecutionAgent: "claude",
		Capabilities:     []domain.EngineeringCapability{domain.CapabilitySecurityReview},
		TrustRequirement: domain.TrustRequirementOperatorTrusted,
		Source:           domain.ArtifactSource{Type: domain.SourceOperatorFile, Location: "/operator/profiles/reviewer.json"},
	}
	if err := planning.RefuseEscalation(narrowing, claude); err != nil {
		t.Fatalf("narrowing profile refused: %v", err)
	}

	escalations := []struct {
		name    string
		mutate  func(*domain.AgentProfile)
		refusal string
	}{
		{
			name:    "raises the trust mode",
			mutate:  func(p *domain.AgentProfile) { p.TrustRequirement = domain.TrustRequirementProtected },
			refusal: "no configuration can raise it",
		},
		{
			name: "advertises a capability the worker does not hold",
			mutate: func(p *domain.AgentProfile) {
				p.Capabilities = append(p.Capabilities, domain.CapabilityVerification)
			},
			refusal: "may narrow the agent's capability set and never extend it",
		},
		{
			name:    "advertises nothing at all",
			mutate:  func(p *domain.AgentProfile) { p.Capabilities = nil },
			refusal: "no stage could ever be eligible",
		},
		{
			name:    "claims a source that is not operator-owned",
			mutate:  func(p *domain.AgentProfile) { p.Source.Type = "repository" },
			refusal: "not operator-owned",
		},
	}
	for _, tc := range escalations {
		t.Run(tc.name, func(t *testing.T) {
			profile := narrowing
			profile.Capabilities = append([]domain.EngineeringCapability{}, narrowing.Capabilities...)
			tc.mutate(&profile)
			err := planning.RefuseEscalation(profile, claude)
			if err == nil || !strings.Contains(err.Error(), tc.refusal) {
				t.Fatalf("expected refusal containing %q, got %v", tc.refusal, err)
			}
		})
	}
}

func TestBindRefusesAProfileWhoseWorkerIsNotConfigured(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "profiles/ghost-backed.json", `{"execution_agent": "not-configured", "capabilities": ["code_change"]}`)
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = registry.Bind([]domain.ExecutionAgentDescriptor{{ID: "claude", TrustMode: domain.TrustRequirementOperatorTrusted}})
	if err == nil || !strings.Contains(err.Error(), "can never introduce one") {
		t.Fatalf("expected an unknown-agent refusal, got %v", err)
	}
}

// Two profiles over ONE worker, used differently, with no core change: this is
// the composition #64 asks for, and the assertion is that the registry treats
// them as two independent identities with two digests.
func TestOneExecutionAgentBacksSeveralProfiles(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "instructions/architecture.json", `{"instructions": ["State the integration boundary before writing code."]}`)
	writeArtifact(t, dir, "instructions/review.json", `{"instructions": ["Review the diff; do not implement."]}`)
	writeArtifact(t, dir, "profiles/zenchron-architect.json", `{
	  "execution_agent": "claude", "capabilities": ["architecture_reasoning", "repository_analysis"],
	  "instructions": ["architecture"]
	}`)
	writeArtifact(t, dir, "profiles/zenchron-reviewer.json", `{
	  "execution_agent": "claude", "capabilities": ["repository_analysis", "security_review"],
	  "instructions": ["review"]
	}`)
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	claude := domain.ExecutionAgentDescriptor{
		ID: "claude", ProviderKind: "claude_code", VendorFamily: "anthropic",
		TrustMode: domain.TrustRequirementOperatorTrusted,
		Capabilities: []domain.EngineeringCapability{
			domain.CapabilityArchitectureReasoning, domain.CapabilityRepositoryAnalysis, domain.CapabilitySecurityReview,
		},
		Available: true,
	}
	if err := registry.Bind([]domain.ExecutionAgentDescriptor{claude}); err != nil {
		t.Fatalf("two profiles over one worker were refused: %v", err)
	}
	architect, _ := registry.Profile("zenchron-architect")
	reviewer, _ := registry.Profile("zenchron-reviewer")
	if architect.Digest == reviewer.Digest {
		t.Fatal("two differently specialized profiles share one identity")
	}
	architectInstructions, _ := registry.Instructions(architect)
	reviewerInstructions, _ := registry.Instructions(reviewer)
	if reflect.DeepEqual(architectInstructions, reviewerInstructions) {
		t.Fatal("two profiles over one worker received the same instructions")
	}
}

// The strongest form of "may not escalate" is being unable to write it down.
// These assertions are on the TYPES: a member that raised a ceiling or granted
// a bypass would have to be added here first, and adding one fails this test.
func TestEscalatingCustomizationIsUnrepresentable(t *testing.T) {
	constraints := reflect.TypeFor[domain.ProfileConstraints]()
	// MaxProviderInvocations is a CEILING on a run total, reviewed against the
	// law when it was added: it can lower what a stage may spend in total and
	// there is no value of it that raises anything.
	allowed := map[string]bool{
		"MaxWallSeconds": true, "MaxExecutionAttempts": true,
		"MaxProviderInvocations": true, "DenyPermissionBypass": true,
	}
	for i := range constraints.NumField() {
		name := constraints.Field(i).Name
		if !allowed[name] {
			t.Fatalf("ProfileConstraints gained member %q: every constraint must NARROW, and a new one has to be reviewed against that law", name)
		}
	}
	// There is no repository provenance class. Instruction content that a
	// candidate could author would be a candidate writing its own governance.
	for _, class := range []string{domain.SourceOperatorConfig, domain.SourceOperatorFile} {
		if !strings.HasPrefix(class, "operator_") {
			t.Fatalf("artifact source class %q is not operator-owned", class)
		}
	}
}

func writeArtifact(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// An operator artifact is read within a bound, and an oversized one is refused
// by name rather than read whole. These are small documents; the bound only
// ever refuses a mistake, and every other file this runtime reads whose size it
// does not control is bounded the same way.
func TestAnOversizedOperatorArtifactIsRefusedByName(t *testing.T) {
	dir := t.TempDir()
	writeArtifact(t, dir, "profiles/enormous.json", `{"execution_agent": "codex", "capabilities": ["code_change"]}`)
	path := filepath.Join(dir, "profiles", "enormous.json")
	padding := make([]byte, 2<<20)
	for i := range padding {
		padding[i] = ' '
	}
	if err := os.WriteFile(path, append(padding, []byte(`{"execution_agent": "codex", "capabilities": ["code_change"]}`)...), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := planning.LoadRegistry(dir)
	if err == nil {
		t.Fatal("an artifact larger than the bound was read whole")
	}
	if !strings.Contains(err.Error(), "enormous.json") || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("the refusal does not name the file and the bound: %v", err)
	}
}
