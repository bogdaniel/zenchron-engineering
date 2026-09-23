package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// generationFixture builds an activated handoff whose successor artifact really
// exists on disk, plus the self record of a process that IS that successor.
func generationFixture(t *testing.T) (root string, record ControllerHandoff, self ControllerSelfRecord) {
	t.Helper()
	root = t.TempDir()
	predecessorDir := filepath.Join(root, "main-aaaaaaaa")
	successorDir := filepath.Join(root, "main-bbbbbbbb")
	for _, dir := range []string{predecessorDir, successorDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "zenchron-engineering"), []byte(dir), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	config := ConfigDigest{Global: "config-a"}
	predecessor := HandoffParty{
		Binding:      adoptedBinding(predecessorRevision, "tree-a", config),
		ArtifactPath: filepath.Join(predecessorDir, "zenchron-engineering"),
	}
	successorBuild := attestedBuild(ControllerAdopted, successorRevision, "tree-b", strings.Repeat("cd", 32))
	successor := HandoffParty{
		Binding:      ControllerBinding{Controller: "zenchron-engineering", Build: &successorBuild, Config: config},
		ArtifactPath: filepath.Join(successorDir, "zenchron-engineering"),
	}
	now := time.Unix(1700000000, 0).UTC()
	record, err := PreflightControllerHandoff(fakeRunReader{}, HandoffPreflightInput{
		Predecessor: predecessor, Successor: successor,
		TrustedMain: RevisionRecord{Revision: successorRevision, Tree: "tree-b"},
		IsAncestor:  ancestorAlways, Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []HandoffPhase{HandoffDraining, HandoffOwnershipReleased, HandoffSuccessorAcquired, HandoffRevalidated, HandoffActivated} {
		record, err = record.Advance(phase, now)
		if err != nil {
			t.Fatal(err)
		}
	}
	self = ControllerSelfRecord{
		Build:          successorBuild,
		ExecutablePath: successor.ArtifactPath,
		Measured:       successorBuild.BinarySHA256,
	}
	return root, record, self
}

// fakeHandoffReader serves one record, or none.
type fakeHandoffReader struct {
	record ControllerHandoff
	found  bool
}

func (f fakeHandoffReader) ControllerHandoff(string) (ControllerHandoff, bool, error) {
	return f.record, f.found, nil
}

// An attested build measures the running artifact exactly once; an unattested
// one has no claim to substantiate and measures nothing.
func TestControllerIdentityMeasuresOnlyWhatItClaims(t *testing.T) {
	measured := 0
	locate := func() (string, error) { return "/controller/zenchron-engineering", nil }
	measure := func(string) (string, error) {
		measured++
		return strings.Repeat("ab", 32), nil
	}
	self, err := ControllerIdentityFrom(ControllerDeclaration{
		Kind: ControllerAdopted, Version: "main-abc", SourceRevision: predecessorRevision, SourceTree: "tree-a",
	}, locate, measure)
	if err != nil {
		t.Fatal(err)
	}
	if measured != 1 {
		t.Fatalf("the running binary was measured %d times, want exactly 1", measured)
	}
	if self.Build.BinarySHA256 != strings.Repeat("ab", 32) || self.ExecutablePath == "" {
		t.Fatalf("the identity did not carry the measurement: %+v", self)
	}

	measured = 0
	unattested, err := ControllerIdentityFrom(ControllerDeclaration{Version: "dev"}, locate, measure)
	if err != nil {
		t.Fatal(err)
	}
	if measured != 0 {
		t.Fatal("an unattested controller measured the running binary")
	}
	if !unattested.Unattested || unattested.Build.Attested() {
		t.Fatalf("an unattested build claimed provenance: %+v", unattested)
	}
}

// The proof compares the whole document. A digest that matches while the
// revision does not is two records describing different things.
func TestGenerationProofComparesEveryField(t *testing.T) {
	_, record, self := generationFixture(t)
	if err := self.ProvesGeneration(record.Successor.Binding); err != nil {
		t.Fatalf("the successor could not prove itself: %v", err)
	}
	for _, test := range []struct {
		name   string
		mutate func(*ControllerSelfRecord)
		want   string
	}{
		{"a different binary", func(s *ControllerSelfRecord) { s.Build.BinarySHA256 = strings.Repeat("ef", 32) }, "measures"},
		{"a different revision", func(s *ControllerSelfRecord) { s.Build.SourceRevision = strangerRevision }, "built from"},
		{"a different tree", func(s *ControllerSelfRecord) { s.Build.SourceTree = "tree-other" }, "tree"},
		{"a different version", func(s *ControllerSelfRecord) { s.Build.Version = "someone-elses" }, "version"},
		{"a different kind", func(s *ControllerSelfRecord) { s.Build.Kind = ControllerPreAdoptionBuild }, "controller is"},
		{"an unattested process", func(s *ControllerSelfRecord) { s.Unattested = true }, "unattested"},
	} {
		t.Run(test.name, func(t *testing.T) {
			mutated := self
			test.mutate(&mutated)
			err := mutated.ProvesGeneration(record.Successor.Binding)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
		})
	}
}

// THE DURABLE RECORD IS AUTHORITY. The pointer moves only after an activation
// exists and only for the process that is the generation it names.
func TestStableEntrypointFollowsTheDurableRecord(t *testing.T) {
	root, record, self := generationFixture(t)

	for _, test := range []struct {
		name   string
		reader fakeHandoffReader
		self   ControllerSelfRecord
		want   string
	}{
		{"no record at all", fakeHandoffReader{}, self, "no handoff"},
		{"a handoff that has not activated", func() fakeHandoffReader {
			midway := record
			midway.Phase = HandoffSuccessorAcquired
			return fakeHandoffReader{record: midway, found: true}
		}(), self, "moves only after a durable activation"},
		{"a process that is not the activated generation", fakeHandoffReader{record: record, found: true},
			func() ControllerSelfRecord {
				other := self
				other.Build.BinarySHA256 = strings.Repeat("ef", 32)
				return other
			}(), "not the activated generation"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ActivateControllerGeneration(test.reader, record.ID, test.self, root); err == nil {
				t.Fatal("the stable entrypoint moved without authority")
			} else if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want one naming %q", err, test.want)
			}
			if _, err := os.Lstat(filepath.Join(root, StableEntrypointName)); err == nil {
				t.Fatal("a refused activation created the pointer")
			}
		})
	}
}

// The pointer is replaced by rename, never rewritten, and running the
// activation again is repair rather than a decision.
func TestStableEntrypointIsReplacedAtomicallyAndIdempotently(t *testing.T) {
	root, record, self := generationFixture(t)
	reader := fakeHandoffReader{record: record, found: true}

	// An operator already pointed at the PREVIOUS generation.
	pointer := filepath.Join(root, StableEntrypointName)
	if err := os.Symlink(filepath.Dir(record.Predecessor.ArtifactPath), pointer); err != nil {
		t.Fatal(err)
	}

	stable, err := ActivateControllerGeneration(reader, record.ID, self, root)
	if err != nil {
		t.Fatal(err)
	}
	target, err := os.Readlink(pointer)
	if err != nil {
		t.Fatal(err)
	}
	if target != filepath.Dir(record.Successor.ArtifactPath) {
		t.Fatalf("the pointer names %s, want the activated generation %s",
			target, filepath.Dir(record.Successor.ArtifactPath))
	}
	// The stable path is what an operator types, and it resolves to the
	// artifact without naming a revision.
	resolved, err := filepath.EvalSymlinks(stable)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != mustEval(t, record.Successor.ArtifactPath) {
		t.Fatalf("the stable path resolves to %s, want %s", resolved, record.Successor.ArtifactPath)
	}

	// Repair: running it again reaches the same state and leaves no staging
	// directory behind.
	if _, err := ActivateControllerGeneration(reader, record.ID, self, root); err != nil {
		t.Fatalf("a repeated activation refused: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".entrypoint-") {
			t.Fatalf("a staging directory survived: %s", entry.Name())
		}
	}

	// The previous generation is still addressable - it is provenance and
	// recovery material - and it is no longer what the pointer names.
	if _, err := os.Stat(record.Predecessor.ArtifactPath); err != nil {
		t.Fatalf("the predecessor artifact stopped being addressable: %v", err)
	}
}

func mustEval(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

// Drift is reported rather than repaired silently: the three things that can
// disagree are the record, the pointer and the running process.
func TestGenerationStatusNamesDrift(t *testing.T) {
	root, record, self := generationFixture(t)
	now := time.Unix(1700000000, 0).UTC()

	if status := DescribeControllerGeneration(self, root, &record, now); !status.Drifted {
		t.Fatal("a missing pointer is not drift")
	}
	if _, err := ActivateControllerGeneration(fakeHandoffReader{record: record, found: true}, record.ID, self, root); err != nil {
		t.Fatal(err)
	}
	status := DescribeControllerGeneration(self, root, &record, now)
	if status.Drifted {
		t.Fatalf("an activated, correctly pointed generation reported drift: %+v", status)
	}
	if status.PointsAt != filepath.Dir(record.Successor.ArtifactPath) {
		t.Fatalf("status names %s, want %s", status.PointsAt, filepath.Dir(record.Successor.ArtifactPath))
	}

	// A process running the PREVIOUS artifact while the pointer names the new
	// one is exactly the drift #234 was filed about.
	stale := self
	stale.ExecutablePath = record.Predecessor.ArtifactPath
	if !DescribeControllerGeneration(stale, root, &record, now).Drifted {
		t.Fatal("a process running an artifact the pointer does not name reported no drift")
	}
}

// THE POINTER IS NEVER AN INPUT. Whatever occupies the stable path - a stale
// link, a dangling one, a regular file, nothing at all - the repair is computed
// from the activated record and the proven running identity alone, and every
// starting state converges on the same answer.
func TestActivationRepairsAnyPointerWithoutReadingIt(t *testing.T) {
	for _, test := range []struct {
		name    string
		occupy  func(t *testing.T, root, pointer string)
		refused string
	}{
		{"nothing at all", func(*testing.T, string, string) {}, ""},
		{"a stale link to the predecessor", func(t *testing.T, root, pointer string) {
			if err := os.Symlink(filepath.Join(root, "main-aaaaaaaa"), pointer); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a dangling link to a generation that was deleted", func(t *testing.T, root, pointer string) {
			if err := os.Symlink(filepath.Join(root, "main-deleted"), pointer); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a link to something outside the controller root", func(t *testing.T, root, pointer string) {
			if err := os.Symlink(filepath.Join(t.TempDir(), "somewhere-else"), pointer); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"a regular file somebody copied over it", func(t *testing.T, root, pointer string) {
			if err := os.WriteFile(pointer, []byte("not a link"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, ""},
		// The single exception, and it is a refusal rather than a deletion:
		// removing a directory to make room for a link would destroy whatever
		// an operator put there.
		{"a directory occupying the path", func(t *testing.T, root, pointer string) {
			if err := os.MkdirAll(filepath.Join(pointer, "someone-elses-files"), 0o700); err != nil {
				t.Fatal(err)
			}
		}, "is a directory"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, record, self := generationFixture(t)
			pointer := filepath.Join(root, StableEntrypointName)
			test.occupy(t, root, pointer)

			stable, err := ActivateControllerGeneration(
				fakeHandoffReader{record: record, found: true}, record.ID, self, root)
			if test.refused != "" {
				if err == nil || !strings.Contains(err.Error(), test.refused) {
					t.Fatalf("error = %v, want one naming %q", err, test.refused)
				}
				return
			}
			if err != nil {
				t.Fatalf("repair refused: %v", err)
			}
			// Same answer from every starting state, taken from the record.
			target, err := os.Readlink(pointer)
			if err != nil {
				t.Fatal(err)
			}
			want := filepath.Dir(record.Successor.ArtifactPath)
			if target != want {
				t.Fatalf("the pointer names %s, want %s", target, want)
			}
			if resolved := mustEval(t, stable); resolved != mustEval(t, record.Successor.ArtifactPath) {
				t.Fatalf("the stable path resolves to %s, want the activated artifact", resolved)
			}
		})
	}
}
