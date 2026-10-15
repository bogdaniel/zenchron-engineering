package planning_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/planning"
)

const validProfileDocument = `{
  "execution_agent": "claude",
  "capabilities": ["repository_analysis"],
  "trust_requirement": "operator_trusted"
}`

// TestWriteSealsDurablyAndLoadRegistrySeesIt pins ADR-0008's one job: a
// governed write lands exactly where LoadRegistry reads, sealed the same way
// a hand-edited file would be.
func TestWriteSealsDurablyAndLoadRegistrySeesIt(t *testing.T) {
	dir := t.TempDir()
	sealed, err := planning.WriteAgentProfile(dir, "reviewer", []byte(validProfileDocument))
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
	if _, err := planning.WriteAgentProfile(dir, "bad", []byte(`{"unknown_field": true}`)); err == nil {
		t.Fatal("a profile missing required fields must be refused")
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles", "bad.json")); !os.IsNotExist(err) {
		t.Fatalf("a refused write must leave no file: stat err = %v", err)
	}
}

// TestWriteRejectsAnUnsafeID is the filename trust boundary: an id becomes a
// path, so a traversal segment is refused before anything is touched.
func TestWriteRejectsAnUnsafeID(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"../escape", "a/b", "", "."} {
		if _, err := planning.WriteAgentProfile(dir, id, []byte(validProfileDocument)); err == nil {
			t.Fatalf("id %q must be refused as a filename", id)
		}
	}
	entries, err := os.ReadDir(dir)
	if err == nil && len(entries) > 0 {
		t.Fatalf("an unsafe id must create nothing under the planning directory: %v", entries)
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
		if _, err := planning.Write(dir, planning.ArtifactWrite{Kind: tc.kind, ID: tc.id, Document: []byte(tc.document)}); err != nil {
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
