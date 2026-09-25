package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

// entrypointFakeAuthority serves the one fact entrypointAuthorityConsistency
// asks for: what durable controller authority presently governs. found=false
// models a state directory that has never completed a transition.
type entrypointFakeAuthority struct {
	authority ControllerAuthority
	found     bool
	err       error
}

func (f entrypointFakeAuthority) CurrentControllerAuthority() (ControllerAuthority, bool, error) {
	return f.authority, f.found, f.err
}

// entrypointFixture builds a real adopted generation under root, with
// "current" pointing at it - the same shape `controller build-adopted` and
// succession leave behind - so tests exercise the actual filesystem chain
// DiagnoseEntrypoint reads rather than a description of it.
func entrypointFixture(t *testing.T, root string) (generation, canonicalTarget string) {
	t.Helper()
	generation = filepath.Join(root, "main-aaaaaaaa")
	if err := os.MkdirAll(generation, 0o700); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(generation, EntrypointExecutableName)
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(root, StableEntrypointName)
	if err := os.Symlink(generation, current); err != nil {
		t.Fatal(err)
	}
	return generation, filepath.Join(current, EntrypointExecutableName)
}

func TestDiscoverEntrypointCandidatesFindsEachPathDirAtMostOnce(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	c := filepath.Join(root, "c")
	for _, dir := range []string{a, b, c} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{a, b} {
		if err := os.WriteFile(filepath.Join(dir, EntrypointExecutableName), []byte("x"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	// c has no executable, and a repeats - a shell never revisits a directory
	// it already searched.
	pathValue := a + string(os.PathListSeparator) + c + string(os.PathListSeparator) + b + string(os.PathListSeparator) + a

	candidates := DiscoverEntrypointCandidates(pathValue)
	if len(candidates) != 2 {
		t.Fatalf("want 2 candidates, got %d: %+v", len(candidates), candidates)
	}
	if candidates[0].Dir != a || candidates[1].Dir != b {
		t.Fatalf("candidates out of PATH order: %+v", candidates)
	}
}

func TestDiagnoseEntrypointRecognizesTheCanonicalChain(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	binDir := t.TempDir()
	entry := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.Symlink(canonicalTarget, entry); err != nil {
		t.Fatal(err)
	}

	diagnosis := DiagnoseEntrypoint(binDir, root, entrypointFakeAuthority{found: false})
	if diagnosis.Winner == nil || diagnosis.Winner.Path != entry {
		t.Fatalf("winner = %+v, want %s", diagnosis.Winner, entry)
	}
	if !diagnosis.Canonical {
		t.Fatalf("Canonical = false: %s", diagnosis.Detail)
	}
	if !diagnosis.ReachesAdopted {
		t.Fatalf("ReachesAdopted = false: %s", diagnosis.Detail)
	}
	if !diagnosis.AuthorityConsistent {
		t.Fatalf("AuthorityConsistent = false: %s", diagnosis.Detail)
	}
	if len(diagnosis.Shadowed()) != 0 {
		t.Fatalf("Shadowed() = %+v, want none", diagnosis.Shadowed())
	}
}

func TestDiagnoseEntrypointReportsADetachedCopyAsNotCanonical(t *testing.T) {
	root := t.TempDir()
	entrypointFixture(t, root)
	binDir := t.TempDir()
	entry := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.WriteFile(entry, []byte("stale binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	diagnosis := DiagnoseEntrypoint(binDir, root, entrypointFakeAuthority{found: false})
	if diagnosis.Canonical {
		t.Fatalf("a detached copy must not diagnose as canonical")
	}
	if diagnosis.ReachesAdopted {
		t.Fatalf("a detached copy with different content must not reach the adopted target")
	}
	if diagnosis.Detail == "" {
		t.Fatalf("a false Canonical must explain itself")
	}
}

func TestDiagnoseEntrypointReportsDriftFromDurableAuthority(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	binDir := t.TempDir()
	entry := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.Symlink(canonicalTarget, entry); err != nil {
		t.Fatal(err)
	}
	// Durable authority names a DIFFERENT generation than "current" points at -
	// the shape of a projection that has drifted from the fact that governs.
	other := filepath.Join(root, "main-bbbbbbbb")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	authority := entrypointFakeAuthority{found: true, authority: ControllerAuthority{
		Kind: AuthorityHandoffActivation, Ref: "handoff-1",
		Artifact: filepath.Join(other, EntrypointExecutableName),
	}}

	diagnosis := DiagnoseEntrypoint(binDir, root, authority)
	if diagnosis.AuthorityConsistent {
		t.Fatalf("a \"current\" that names a different generation than durable authority must not be consistent")
	}
	if diagnosis.Detail == "" {
		t.Fatalf("drift must be explained")
	}
}

func TestDiagnoseEntrypointWithNoAuthorityReaderFailsClosed(t *testing.T) {
	root := t.TempDir()
	entrypointFixture(t, root)
	diagnosis := DiagnoseEntrypoint("", root, nil)
	if diagnosis.AuthorityConsistent {
		t.Fatalf("a nil authority reader must not be treated as consistent")
	}
}

func TestEntrypointDiagnosisShadowedNamesEveryOtherCandidate(t *testing.T) {
	winner := EntrypointCandidate{Dir: "/a", Path: "/a/zenchron-engineering"}
	other := EntrypointCandidate{Dir: "/b", Path: "/b/zenchron-engineering"}
	diagnosis := EntrypointDiagnosis{Winner: &winner, Candidates: []EntrypointCandidate{winner, other}}
	shadowed := diagnosis.Shadowed()
	if len(shadowed) != 1 || shadowed[0].Path != other.Path {
		t.Fatalf("Shadowed() = %+v, want [%+v]", shadowed, other)
	}

	solo := EntrypointDiagnosis{Winner: &winner, Candidates: []EntrypointCandidate{winner}}
	if got := solo.Shadowed(); got != nil {
		t.Fatalf("a single candidate must not shadow itself: %+v", got)
	}

	empty := EntrypointDiagnosis{}
	if got := empty.Shadowed(); got != nil {
		t.Fatalf("no winner must report no shadowing: %+v", got)
	}
}

func TestInstallCanonicalEntrypointCreatesAFreshLinkWhenNothingIsOnPath(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	binDir := filepath.Join(t.TempDir(), "bin")

	result, err := InstallCanonicalEntrypoint("", binDir, root, entrypointFakeAuthority{found: false})
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(binDir, EntrypointExecutableName)
	if result.Path != wantPath {
		t.Fatalf("Path = %s, want %s", result.Path, wantPath)
	}
	if result.AlreadyCanonical || result.RetiredPath != "" {
		t.Fatalf("a fresh install has nothing to retire and was not already canonical: %+v", result)
	}
	if result.OnPath {
		t.Fatalf("an empty PATH does not contain binDir")
	}
	link, err := os.Readlink(result.Path)
	if err != nil || link != canonicalTarget {
		t.Fatalf("readlink = %q, %v; want %s", link, err, canonicalTarget)
	}
}

func TestInstallCanonicalEntrypointMigratesADetachedStaleBinaryInPlace(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	binDir := t.TempDir()
	stale := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.WriteFile(stale, []byte("stale binary"), 0o700); err != nil {
		t.Fatal(err)
	}

	result, err := InstallCanonicalEntrypoint(binDir, "/unused-bin-dir", root, entrypointFakeAuthority{found: false})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != stale {
		t.Fatalf("the winning PATH entry must be converted in place: Path = %s, want %s", result.Path, stale)
	}
	if result.RetiredPath == "" {
		t.Fatalf("the stale occupant must be retired, not deleted")
	}
	retired, err := os.ReadFile(result.RetiredPath)
	if err != nil || string(retired) != "stale binary" {
		t.Fatalf("retired content = %q, %v; want the original stale binary preserved", retired, err)
	}
	link, err := os.Readlink(stale)
	if err != nil || link != canonicalTarget {
		t.Fatalf("readlink(%s) = %q, %v; want %s", stale, link, err, canonicalTarget)
	}
	if !result.OnPath {
		t.Fatalf("binDir is on the PATH value passed in")
	}
}

func TestInstallCanonicalEntrypointIsANoopWhenAlreadyCanonical(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	binDir := t.TempDir()
	entry := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.Symlink(canonicalTarget, entry); err != nil {
		t.Fatal(err)
	}

	result, err := InstallCanonicalEntrypoint(binDir, "", root, entrypointFakeAuthority{found: false})
	if err != nil {
		t.Fatal(err)
	}
	if !result.AlreadyCanonical {
		t.Fatalf("an existing canonical link must be reported as already canonical, not rewritten")
	}
	if result.RetiredPath != "" {
		t.Fatalf("nothing should have been retired: %+v", result)
	}
}

func TestInstallCanonicalEntrypointRefusesWhenAuthorityHasDrifted(t *testing.T) {
	root := t.TempDir()
	entrypointFixture(t, root)
	other := filepath.Join(root, "main-bbbbbbbb")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	authority := entrypointFakeAuthority{found: true, authority: ControllerAuthority{
		Kind: AuthorityHandoffActivation, Ref: "handoff-1",
		Artifact: filepath.Join(other, EntrypointExecutableName),
	}}

	_, err := InstallCanonicalEntrypoint("", t.TempDir(), root, authority)
	if err == nil {
		t.Fatalf("installing over a drifted \"current\" projection must be refused")
	}
}

func TestInstallCanonicalEntrypointRefusesWhenNoGenerationIsAdopted(t *testing.T) {
	root := t.TempDir() // no "current" pointer, nothing adopted
	_, err := InstallCanonicalEntrypoint("", t.TempDir(), root, entrypointFakeAuthority{found: false})
	if err == nil {
		t.Fatalf("installing before anything is adopted must be refused")
	}
}

func TestInstallCanonicalEntrypointNeverModifiesShadowedEntries(t *testing.T) {
	root := t.TempDir()
	_, canonicalTarget := entrypointFixture(t, root)
	winningDir := t.TempDir()
	shadowedDir := t.TempDir()
	stale := filepath.Join(winningDir, EntrypointExecutableName)
	if err := os.WriteFile(stale, []byte("stale winner"), 0o700); err != nil {
		t.Fatal(err)
	}
	shadowed := filepath.Join(shadowedDir, EntrypointExecutableName)
	if err := os.WriteFile(shadowed, []byte("shadowed copy"), 0o700); err != nil {
		t.Fatal(err)
	}
	pathValue := winningDir + string(os.PathListSeparator) + shadowedDir

	if _, err := InstallCanonicalEntrypoint(pathValue, "", root, entrypointFakeAuthority{found: false}); err != nil {
		t.Fatal(err)
	}
	link, err := os.Readlink(stale)
	if err != nil || link != canonicalTarget {
		t.Fatalf("the winning entry must become canonical: readlink = %q, %v", link, err)
	}
	content, err := os.ReadFile(shadowed)
	if err != nil || string(content) != "shadowed copy" {
		t.Fatalf("a shadowed entry other than the winner must never be modified: %q, %v", content, err)
	}
}

// A FAILED INSTALL MUST NOT DELETE THE OPERATOR'S COMMAND.
//
// The migration used to rename the existing entry away and only then install
// the replacement, so a failure in the second step left the public PATH
// location empty: an install that breaks the very command it was asked to fix.
// replaceSymlinkAtomically guarantees a reader sees old-or-new, and that is
// only worth something while the old one is still there.
func TestAFailedInstallLeavesTheExistingEntrypointInPlace(t *testing.T) {
	root := t.TempDir()
	entrypointFixture(t, root)
	binDir := t.TempDir()

	// A stale detached copy already on PATH: exactly what #319 migrates.
	link := filepath.Join(binDir, EntrypointExecutableName)
	if err := os.WriteFile(link, []byte("stale copy"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The install cannot complete: the directory admits no new entries, so
	// staging the replacement link fails.
	if err := os.Chmod(binDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(binDir, 0o700) })

	if _, err := InstallCanonicalEntrypoint(binDir, "", root, entrypointFakeAuthority{found: false}); err == nil {
		t.Fatal("the install reported success against a directory it cannot write")
	}

	// THE COMMAND IS STILL THERE, and it is still the operator's original.
	_ = os.Chmod(binDir, 0o700)
	body, err := os.ReadFile(link)
	if err != nil {
		t.Fatalf("a failed install removed the operator's command: %v", err)
	}
	if string(body) != "stale copy" {
		t.Fatalf("the surviving command is %q, not what was there before", body)
	}
	// And no half-finished migration is left beside it for the next attempt
	// to trip over.
	if _, err := os.Lstat(link + ".pre-canonical"); err == nil {
		t.Fatal("a failed install left a retirement copy behind")
	}
}
