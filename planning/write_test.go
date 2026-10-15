package planning_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
	"github.com/bogdaniel/zenchron-engineering/planning"
)

const validProfileDocument = `{
  "execution_agent": "claude",
  "capabilities": ["repository_analysis"],
  "trust_requirement": "operator_trusted"
}`

// claudeAgents is the configured workforce matching validProfileDocument,
// reusing resolver_test.go's claudeAgent (full capabilities, operator
// trusted). The write path is fail-closed (no workforce, no match,
// refused), so every test below that expects a profile write to SUCCEED
// supplies this rather than nil.
func claudeAgents() []domain.ExecutionAgentDescriptor {
	return []domain.ExecutionAgentDescriptor{claudeAgent()}
}

// TestWriteSealsDurablyAndLoadRegistrySeesIt pins ADR-0008's one job: a
// governed write lands exactly where LoadRegistry reads, sealed the same way
// a hand-edited file would be.
func TestWriteSealsDurablyAndLoadRegistrySeesIt(t *testing.T) {
	dir := t.TempDir()
	sealed, err := planning.WriteAgentProfile(dir, "reviewer", []byte(validProfileDocument), claudeAgents())
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ID != "reviewer" || sealed.Digest == "" {
		t.Fatalf("sealed profile not identified: %+v", sealed)
	}

	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := registry.Profile("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Digest != sealed.Digest {
		t.Fatalf("loaded digest %s, written digest %s", loaded.Digest, sealed.Digest)
	}
}

// TestWriteRefusesWhatLoadWouldRefuseAndWritesNothing proves the write path
// is validated by the IDENTICAL seal function the loader uses, and that a
// refusal leaves no partial file behind.
func TestWriteRefusesWhatLoadWouldRefuseAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	if _, err := planning.WriteAgentProfile(dir, "bad", []byte(`{"unknown_field": true}`), nil); err == nil {
		t.Fatal("a profile missing required fields must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "bad.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
	}
}

// TestWriteRefusesAProfileNamingAnUninstalledPackOrContextPolicy proves a
// dangling reference is refused at the write itself - before anything is
// durable - rather than succeeding and only breaking a later
// LoadRegistry.validateReferences for every OTHER artifact too.
func TestWriteRefusesAProfileNamingAnUninstalledPackOrContextPolicy(t *testing.T) {
	dir := t.TempDir()
	if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(`{
	  "execution_agent": "claude",
	  "capabilities": ["repository_analysis"],
	  "instructions": ["does-not-exist"],
	  "trust_requirement": "operator_trusted"
	}`), nil); err == nil {
		t.Fatal("a profile naming an uninstalled instruction pack must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "reviewer.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
	}
	if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(`{
	  "execution_agent": "claude",
	  "capabilities": ["repository_analysis"],
	  "context_policy": "does-not-exist",
	  "trust_requirement": "operator_trusted"
	}`), nil); err == nil {
		t.Fatal("a profile naming an uninstalled context policy must be refused")
	}
}

// TestWriteAcceptsAProfileNamingAnInstalledPackAndContextPolicy proves the
// new check refuses a MISSING reference and nothing else: an already
// installed pack and context policy still write and reload cleanly.
func TestWriteAcceptsAProfileNamingAnInstalledPackAndContextPolicy(t *testing.T) {
	dir := t.TempDir()
	if _, err := planning.WriteInstructionPack(dir, "review-core", []byte(`{"instructions": ["Review the diff."]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := planning.WriteContextPolicy(dir, "reviewer-context", []byte(`{"exclude": ["producer_reasoning"]}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(`{
	  "execution_agent": "claude",
	  "capabilities": ["repository_analysis"],
	  "instructions": ["review-core"],
	  "context_policy": "reviewer-context",
	  "trust_requirement": "operator_trusted"
	}`), claudeAgents()); err != nil {
		t.Fatalf("a profile naming already-installed artifacts must write: %v", err)
	}
	if _, err := planning.LoadRegistry(dir); err != nil {
		t.Fatalf("reload must stay clean: %v", err)
	}
}

// TestWriteRefusesATemplateStagePreferringAnUninstalledProfile mirrors the
// profile check for EngineeringPlanTemplate, the other artifact kind that
// names other artifacts.
func TestWriteRefusesATemplateStagePreferringAnUninstalledProfile(t *testing.T) {
	dir := t.TempDir()
	if _, err := planning.WriteEngineeringPlanTemplate(dir, "two-stage", []byte(`{
	  "stages": [{"id": "a", "kind": "agent", "role": "implementer", "profile": "does-not-exist"}]
	}`)); err == nil {
		t.Fatal("a stage preferring an uninstalled profile must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "templates", "two-stage.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
	}
}

// TestWriteRejectsAnUnsafeID is the filename trust boundary: an id becomes a
// path, so a traversal segment is refused before anything is touched.
func TestWriteRejectsAnUnsafeID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../escape", "a/b", "", "."} {
		if _, err := planning.WriteAgentProfile(dir, id, []byte(validProfileDocument), nil); err == nil {
			t.Fatalf("id %q must be refused as a filename", id)
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		t.Fatalf("an unsafe id must create nothing under the planning directory: %v", entries)
	}
}

// TestWriteRefusesAnUnconfiguredOrRelativePlanningDir is the write-side half
// of config validation's "planning_dir must be an absolute path": LoadRegistry
// treats "" as a valid empty registry (no custom agents configured), which
// is fine for a READ, but a WRITE has no safe empty case - "" or a relative
// path would resolve against the server process's own working directory, not
// an operator-controlled location, and must never reach the filesystem.
func TestWriteRefusesAnUnconfiguredOrRelativePlanningDir(t *testing.T) {
	for _, dir := range []string{"", "relative/planning", "."} {
		if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(validProfileDocument), nil); err == nil {
			t.Fatalf("dir %q must be refused before anything is written", dir)
		}
	}
}

// TestWriteDispatchesEveryKind proves planning.Write's kind dispatch reaches
// each of the four writers, not just AgentProfile.
func TestWriteDispatchesEveryKind(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		kind     planning.ArtifactKind
		id       string
		document string
	}{
		{planning.KindInstructionPack, "pack-1", `{"instructions": ["Review carefully."]}`},
		{planning.KindContextPolicy, "policy-1", `{"exclude": ["producer_reasoning"]}`},
		{planning.KindAgentProfile, "profile-1", validProfileDocument},
		{planning.KindEngineeringPlanTemplate, "template-1", `{"stages": [{"id": "a", "kind": "agent", "role": "implementer"}]}`},
	} {
		if _, err := planning.Write(dir, planning.ArtifactWrite{Kind: tc.kind, ID: tc.id, Document: []byte(tc.document)}, claudeAgents()); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
	}
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Pack("pack-1"); err != nil {
		t.Error(err)
	}
	if _, err := registry.ContextPolicy("policy-1"); err != nil {
		t.Error(err)
	}
	if _, err := registry.Profile("profile-1"); err != nil {
		t.Error(err)
	}
	if _, err := registry.Template("template-1"); err != nil {
		t.Error(err)
	}
}

// TestWriteRefusesAProfileWithoutAKnownWorkforce is the fail-closed
// invariant itself: a caller with no workforce to hand gets the same
// refusal an unconfigured agent would, never a bypass. There is no
// legacy/testing escape hatch here by design (independent review).
func TestWriteRefusesAProfileWithoutAKnownWorkforce(t *testing.T) {
	dir := t.TempDir()
	for _, agents := range [][]domain.ExecutionAgentDescriptor{nil, {}} {
		if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(validProfileDocument), agents); err == nil {
			t.Fatalf("agents=%#v: a profile written with no known workforce must be refused, not silently accepted", agents)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "reviewer.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
	}
}

// TestWriteRefusesAProfileRequiringMoreTrustThanItsAgentHolds is the other
// half of RefuseEscalation this write path now reaches: trust, not just
// capability.
func TestWriteRefusesAProfileRequiringMoreTrustThanItsAgentHolds(t *testing.T) {
	dir := t.TempDir()
	agents := []domain.ExecutionAgentDescriptor{{ID: "claude", TrustMode: domain.TrustRequirementOperatorTrusted}}
	if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(`{
		"execution_agent": "claude",
		"capabilities": ["repository_analysis"],
		"trust_requirement": "protected"
	}`), agents); err == nil {
		t.Fatal("a profile requiring more trust than its agent holds must be refused")
	}
}

// TestWriteRejectingAnEscalatingReplacementLeavesTheOriginalProfileUnchanged
// proves the write is refused IN PLACE: overwriting an already-installed,
// compatible profile with an escalating document must not touch the file
// that is already there.
func TestWriteRejectingAnEscalatingReplacementLeavesTheOriginalProfileUnchanged(t *testing.T) {
	dir := t.TempDir()
	// Narrower than claudeAgents(): exactly the one capability
	// validProfileDocument names, so adding a second one below is a real
	// escalation rather than something this agent already holds.
	agents := []domain.ExecutionAgentDescriptor{{
		ID: "claude", TrustMode: domain.TrustRequirementOperatorTrusted,
		Capabilities: []domain.EngineeringCapability{domain.CapabilityRepositoryAnalysis},
	}}
	original, err := planning.WriteAgentProfile(dir, "reviewer", []byte(validProfileDocument), agents)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planning.WriteAgentProfile(dir, "reviewer", []byte(`{
		"execution_agent": "claude",
		"capabilities": ["repository_analysis", "security_review"],
		"trust_requirement": "operator_trusted"
	}`), agents); err == nil {
		t.Fatal("an escalating replacement must be refused")
	}
	registry, err := planning.LoadRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	current, err := registry.Profile("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if current.Digest != original.Digest {
		t.Fatalf("a refused replacement must leave the original profile unchanged: original digest %s, now %s", original.Digest, current.Digest)
	}
}
