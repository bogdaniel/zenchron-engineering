package runtime

// Internal candidate transport: a downstream stage receives the EXACT upstream
// candidate, or it does not run.
//
// The #119 reviewer was handed the trusted base and told it was reviewing a
// candidate. It caught that only because it hashed its own tree rather than
// trusting its brief, and the fallback that produced it was justified by a
// comment claiming the diff arrived "as context" - which is not a mechanism
// that exists. These tests hold both halves: the transfer works without
// publishing anything, and a subject that cannot be proven is an error rather
// than a workspace somebody executes against.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// producerWorkspace is a runtime-owned workspace holding one real commit, which
// is what a producer run leaves behind when its candidate is not published.
func producerWorkspace(t *testing.T, dir, content string) CandidateRef {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--initial-branch=main"},
		{"config", "user.name", "Producer"},
		{"config", "user.email", "producer@zenchron.invalid"},
	} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "candidate.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "candidate"}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	head, err := gitOutput(dir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(dir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return CandidateRef{
		RunID: "run-producer", StageID: "implementation",
		Revision: strings.TrimSpace(head), Tree: strings.TrimSpace(tree),
	}
}

// consumerWorkspace is a clone of the producer at its FIRST commit, standing in
// for a downstream run cloned at the trusted base.
func consumerWorkspace(t *testing.T, producerDir, dir string) {
	t.Helper()
	if _, err := runGit("", "clone", "--no-tags", producerDir, dir); err != nil {
		t.Fatalf("clone: %v", err)
	}
}

// The transfer moves the exact commit between two runtime-owned workspaces,
// with no remote and no credential involved - which is the whole point:
// publication is authority, internal consumption is plumbing.
func TestAnUnpublishedCandidateIsTransferredBetweenWorkspaces(t *testing.T) {
	root := t.TempDir()
	producerDir := filepath.Join(root, "producer")
	ref := producerWorkspace(t, producerDir, "first")

	consumerDir := filepath.Join(root, "consumer")
	consumerWorkspace(t, producerDir, consumerDir)

	// The producer moves on to work that was never pushed anywhere.
	if err := os.WriteFile(filepath.Join(producerDir, "candidate.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"commit", "-m", "unpublished candidate"}} {
		if _, err := runGit(producerDir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	head, err := gitOutput(producerDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	tree, err := gitOutput(producerDir, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	unpublished := CandidateRef{
		RunID: ref.RunID, StageID: ref.StageID,
		Revision: strings.TrimSpace(head), Tree: strings.TrimSpace(tree),
	}

	// The consumer does not have it yet. If it did, this test would prove
	// nothing about the transfer.
	if _, err := gitOutput(consumerDir, "cat-file", "-t", unpublished.Revision); err == nil {
		t.Fatal("the consumer already held the unpublished candidate")
	}
	if err := MaterializeCandidate(consumerDir, unpublished, producerDir); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	// And it is PROVEN, not assumed.
	if err := AssertCandidateSubject(consumerDir, unpublished); err != nil {
		t.Fatalf("the workspace was not proven to be the candidate: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(consumerDir, "candidate.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "second" {
		t.Fatalf("the consumer holds %q, which is not the unpublished candidate's content", content)
	}
}

// A subject that does not match is an ERROR, never a workspace the caller keeps
// going with. This is the check that would have caught #119 at the source: the
// reviewer's workspace was the base, and every surface said it was fine.
func TestAWorkspaceThatIsNotTheCandidateIsRefused(t *testing.T) {
	root := t.TempDir()
	producerDir := filepath.Join(root, "producer")
	ref := producerWorkspace(t, producerDir, "first")

	consumerDir := filepath.Join(root, "consumer")
	consumerWorkspace(t, producerDir, consumerDir)

	// The same workspace, asked about a DIFFERENT tree.
	wrongTree := ref
	wrongTree.Tree = strings.Repeat("0", 40)
	if err := AssertCandidateSubject(consumerDir, wrongTree); err == nil {
		t.Fatal("a workspace whose tree is not the candidate's was accepted")
	}
	wrongCommit := ref
	wrongCommit.Revision = strings.Repeat("1", 40)
	if err := AssertCandidateSubject(consumerDir, wrongCommit); err == nil {
		t.Fatal("a workspace whose head is not the candidate was accepted")
	}
}

// An incomplete reference is refused rather than partially trusted: "some of
// the identity matched" is not the question a downstream stage is asking.
func TestAnIncompleteCandidateReferenceIsRefused(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "consumer")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []CandidateRef{
		{Revision: "abc", Tree: "def"},
		{RunID: "run-1", Tree: "def"},
		{RunID: "run-1", Revision: "abc"},
	} {
		if ref.Materializable() {
			t.Fatalf("an incomplete reference reported materializable: %#v", ref)
		}
		if err := MaterializeCandidate(dir, ref, root); err == nil {
			t.Fatalf("an incomplete reference was materialized: %#v", ref)
		}
	}
}

// A producer whose workspace is gone cannot hand anything over, and that is an
// error rather than a silent fallback to the base.
func TestAMissingProducerWorkspaceIsAnError(t *testing.T) {
	root := t.TempDir()
	consumerDir := filepath.Join(root, "consumer")
	if err := os.MkdirAll(consumerDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ref := CandidateRef{RunID: "run-gone", Revision: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)}
	if err := MaterializeCandidate(consumerDir, ref, filepath.Join(root, "not-a-workspace")); err == nil {
		t.Fatal("a missing producer workspace was treated as a successful transfer")
	}
}
