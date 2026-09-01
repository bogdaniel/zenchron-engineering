package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// adoptedFixture builds a real little Git repository with two commits, so
// containment and tree recomputation are exercised against actual Git objects
// rather than a mock that would agree with whatever the code did.
//
// No test here touches the real trusted main: the repository is created in a
// temp directory and thrown away.
func adoptedGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(out)
}

type adoptedFixture struct {
	dir      string
	head     string
	headTree string
	orphan   string
	deps     AdoptedBuildDeps
	built    []AdoptedBuildSpec
}

func newAdoptedFixture(t *testing.T) *adoptedFixture {
	t.Helper()
	dir := t.TempDir()
	initFixtureRepo(t, dir, "README.md", "one\n")
	if err := os.WriteFile(filepath.Join(dir, "second.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "-q", "-m", "second"); err != nil {
		t.Fatal(err)
	}
	head := adoptedGit(t, dir, "rev-parse", "HEAD")
	tree := adoptedGit(t, dir, "rev-parse", "HEAD^{tree}")

	// A commit on an unrelated root: never contained in trusted main.
	if _, err := runGit(dir, "checkout", "-q", "--orphan", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "elsewhere.txt"), []byte("no\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "-q", "-m", "elsewhere"); err != nil {
		t.Fatal(err)
	}
	orphan := adoptedGit(t, dir, "rev-parse", "HEAD")
	if _, err := runGit(dir, "checkout", "-q", head); err != nil {
		t.Fatal(err)
	}

	f := &adoptedFixture{dir: dir, head: head, headTree: tree, orphan: orphan}
	f.deps = AdoptedBuildDeps{
		Rulesets: func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
			return []TrustedMainRuleset{goodRuleset()}, nil
		},
		RefSHA: func(context.Context, GitHubRepo, string) (RefObservation, error) {
			return RefObservation{Exists: true, SHA: f.head}, nil
		},
		Git: gitOutput,
		// The fixture repository is its own source of truth, so there is
		// nothing to fetch; the proof that follows is unchanged.
		Fetch: func(string, string, string) error { return nil },
		Build: func(_ context.Context, spec AdoptedBuildSpec) (BuildEnvironment, error) {
			f.built = append(f.built, spec)
			if err := os.WriteFile(spec.Output, []byte("binary for "+spec.Revision), 0600); err != nil {
				return BuildEnvironment{}, err
			}
			return BuildEnvironment{Kind: "pinned-container", Image: "sha256:fixture", Network: "none",
				SourceMount: "read-only", CacheMount: "read-only", Digest: "sha256:env"}, nil
		},
		// The fixture binary reports exactly what it was built as, so the
		// self-probe is exercised on every path rather than skipped.
		Probe: func(binary string) (ControllerBuild, error) {
			digest, err := measureExecutable(binary)
			if err != nil {
				return ControllerBuild{}, err
			}
			spec := f.built[len(f.built)-1]
			return ControllerBuild{Kind: spec.Kind, Version: spec.Version,
				SourceRevision: spec.Revision, SourceTree: spec.Tree, BinarySHA256: digest}, nil
		},
		Now: func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
	return f
}

func (f *adoptedFixture) request(t *testing.T) AdoptedBuildRequest {
	t.Helper()
	return AdoptedBuildRequest{
		Repository:    GitHubRepo{Owner: "acme", Name: "widgets"},
		RepositoryDir: f.dir, OutputRoot: t.TempDir(),
		Policy: DefaultTrustPolicy(),
	}
}

// TestAdoptedBuildProvesBeforeItBuilds is the success path, and the assertions
// are about the PROOF: the tree is recomputed from the checkout, containment is
// re-derived from the remote-observed head, and the digest is measured from
// what was actually written.
func TestAdoptedBuildProvesBeforeItBuilds(t *testing.T) {
	f := newAdoptedFixture(t)
	got, err := BuildAdoptedController(context.Background(), f.request(t), f.deps,
		BuilderRecord{Kind: ControllerUnattested})
	if err != nil {
		t.Fatalf("the intended build was refused: %v", err)
	}
	if got.Kind != ControllerAdopted {
		t.Fatalf("kind = %q", got.Kind)
	}
	if got.Source.Revision != f.head || got.Source.Tree != f.headTree {
		t.Fatalf("source = %+v, want %s / %s", got.Source, f.head, f.headTree)
	}
	if got.TrustedMain.Revision != f.head {
		t.Fatalf("trusted main = %q", got.TrustedMain.Revision)
	}
	if got.TrustRoot.RulesetID != 22043609 || !strings.HasPrefix(got.TrustRoot.Digest, "sha256:") {
		t.Fatalf("trust root = %+v", got.TrustRoot)
	}
	if got.Version != "main-"+f.head[:8] {
		t.Fatalf("version = %q", got.Version)
	}
	// The digest is of the file, not of an intention.
	measured, err := measureExecutable(got.OutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if got.BinarySHA256 != measured {
		t.Fatalf("recorded digest %s is not the file's %s", got.BinarySHA256, measured)
	}
	// The build was asked for under the controlled settings, with the proven
	// revision and tree, never the caller's word for them.
	if len(f.built) != 1 {
		t.Fatalf("built %d times", len(f.built))
	}
	spec := f.built[0]
	if spec.Kind != ControllerAdopted || spec.Revision != f.head || spec.Tree != f.headTree {
		t.Fatalf("build spec = %+v", spec)
	}
	if !got.SelfProbe.Matched || got.SelfProbe.Kind != ControllerAdopted || got.SelfProbe.BinarySHA256 != measured {
		t.Fatalf("self probe = %+v", got.SelfProbe)
	}
	// The trusted main tree is recorded, not left empty.
	if got.TrustedMain.Tree == "" || got.TrustedMain.Tree != f.headTree {
		t.Fatalf("trusted main tree = %q, want %s", got.TrustedMain.Tree, f.headTree)
	}
	if got.BuildEnv.Network != "none" || got.BuildEnv.SourceMount != "read-only" || got.BuildEnv.CacheMount != "read-only" {
		t.Fatalf("build environment = %+v", got.BuildEnv)
	}
}

// TestAdoptedBuildAcceptsAnEarlierContainedRevision: pinning an older adopted
// commit is legitimate, because containment - not recency - is what adoption
// means.
func TestAdoptedBuildAcceptsAnEarlierContainedRevision(t *testing.T) {
	f := newAdoptedFixture(t)
	parent := adoptedGit(t, f.dir, "rev-parse", f.head+"^")
	request := f.request(t)
	request.Revision = parent
	got, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
	if err != nil {
		t.Fatalf("an earlier contained revision was refused: %v", err)
	}
	if got.Source.Revision != parent {
		t.Fatalf("source = %q, want %q", got.Source.Revision, parent)
	}
	if got.TrustedMain.Revision != f.head {
		t.Fatalf("trusted main should still be the observed head, got %q", got.TrustedMain.Revision)
	}
}

func TestAdoptedBuildRefusesEverythingItCannotProve(t *testing.T) {
	for name, tc := range map[string]struct {
		arrange func(*adoptedFixture, *AdoptedBuildRequest)
		says    string
	}{
		"github unavailable": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
					return nil, fmt.Errorf("dial tcp: i/o timeout")
				}
			}, "trust root could not be observed",
		},
		"no ruleset at all": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) { return nil, nil }
			}, "no ruleset governing the trusted branch",
		},
		"ruleset governs another branch": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				r := goodRuleset()
				r.Targets = []string{"refs/heads/develop"}
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
					return []TrustedMainRuleset{r}, nil
				}
			}, "no ruleset governing the trusted branch",
		},
		"inactive ruleset": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				r := goodRuleset()
				r.Enforcement = "evaluate"
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
					return []TrustedMainRuleset{r}, nil
				}
			}, "not active",
		},
		"squash allowed": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				r := goodRuleset()
				r.PullRequest.AllowedMergeMethods = []string{"merge", "squash"}
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
					return []TrustedMainRuleset{r}, nil
				}
			}, `"squash" is allowed`,
		},
		"bypass actor": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				r := goodRuleset()
				r.BypassActors = 1
				f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
					return []TrustedMainRuleset{r}, nil
				}
			}, "gates nothing",
		},
		"trusted main not observable": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
					return RefObservation{}, fmt.Errorf("rate limited")
				}
			}, "trusted main could not be observed",
		},
		"trusted main absent": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
					return RefObservation{}, nil
				}
			}, "does not exist",
		},
		"source outside trusted main": {
			func(f *adoptedFixture, r *AdoptedBuildRequest) { r.Revision = f.orphan },
			"is not contained in trusted main",
		},
		"source is not a commit": {
			func(_ *adoptedFixture, r *AdoptedBuildRequest) { r.Revision = "not-a-revision" },
			"is not an exact commit",
		},
		"build failure": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.Build = func(context.Context, AdoptedBuildSpec) (BuildEnvironment, error) {
					return BuildEnvironment{}, fmt.Errorf("compile error")
				}
			}, "no binary was produced",
		},
		"digest cannot be measured": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.Build = func(context.Context, AdoptedBuildSpec) (BuildEnvironment, error) {
					return BuildEnvironment{}, nil // writes nothing
				}
			}, "could not be measured",
		},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			f := newAdoptedFixture(t)
			request := f.request(t)
			tc.arrange(f, &request)
			got, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
			if err == nil {
				t.Fatalf("an unprovable build succeeded: %+v", got)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("refusal does not explain %q: %v", tc.says, err)
			}
			// Nothing may be installed by a refused build.
			assertNothingInstalled(t, request.OutputRoot)
		})
	}
}

// TestAdoptedBuildRefusesATreeThatIsNotTheRevisionsTree covers the one check
// nothing else would catch: an export that silently produced different content
// than the commit names. The build directory is corrupted between export and
// comparison by making the export write the wrong revision.
func TestAdoptedBuildRefusesATreeThatIsNotTheRevisionsTree(t *testing.T) {
	f := newAdoptedFixture(t)
	request := f.request(t)
	// Report the head's tree for a revision whose content differs, which is
	// exactly the shape of a mis-exported checkout.
	realGit := f.deps.Git
	f.deps.Git = func(dir string, args ...string) (string, error) {
		if len(args) == 2 && args[0] == "rev-parse" && strings.HasSuffix(args[1], "^{tree}") {
			return strings.Repeat("0", 40), nil
		}
		return realGit(dir, args...)
	}
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil ||
		!strings.Contains(err.Error(), "holds tree") {
		t.Fatalf("a checkout that is not the revision's tree was accepted: %v", err)
	}
}

// assertNothingInstalled proves a refused build published nothing and left no
// staging debris behind.
func assertNothingInstalled(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".staging-") {
			t.Fatalf("a refused build published %q", entry.Name())
		}
		t.Fatalf("a refused build left staging debris %q", entry.Name())
	}
}

// TestAdoptedBuildRefusesABinaryThatMisreportsItself proves the last gate: a
// build whose ldflags did not take effect produces a working binary that lies
// about what it is, and nothing downstream would notice.
func TestAdoptedBuildRefusesABinaryThatMisreportsItself(t *testing.T) {
	for name, probe := range map[string]func(string) (ControllerBuild, error){
		"claims to be unattested": func(string) (ControllerBuild, error) {
			return ControllerBuild{Kind: ControllerUnattested}, nil
		},
		"names another revision": func(binary string) (ControllerBuild, error) {
			digest, _ := measureExecutable(binary)
			return ControllerBuild{Kind: ControllerAdopted, Version: "main-deadbeef",
				SourceRevision: strings.Repeat("d", 40), SourceTree: strings.Repeat("e", 40), BinarySHA256: digest}, nil
		},
		"reports a digest that is not the file": func(string) (ControllerBuild, error) {
			return ControllerBuild{Kind: ControllerAdopted, BinarySHA256: strings.Repeat("f", 64)}, nil
		},
		"cannot report at all": func(string) (ControllerBuild, error) {
			return ControllerBuild{}, fmt.Errorf("exec format error")
		},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			f := newAdoptedFixture(t)
			request := f.request(t)
			f.deps.Probe = probe
			if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil {
				t.Fatal("a binary that misreports its own provenance was accepted")
			}
			assertNothingInstalled(t, request.OutputRoot)
		})
	}
}

// TestAdoptedBuildRevalidatesTrustBeforePublishing closes the window between
// proving the gate and publishing under it. A trust root that changed while
// the build ran is not the gate that was proven, and a source that is no
// longer contained is not adopted however it started.
func TestAdoptedBuildRevalidatesTrustBeforePublishing(t *testing.T) {
	t.Run("refuse a trust root that changed mid-build", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		calls := 0
		f.deps.Rulesets = func(context.Context, GitHubRepo) ([]TrustedMainRuleset, error) {
			calls++
			r := goodRuleset()
			if calls > 1 {
				r.Name = "loosened-after-the-build-started"
			}
			return []TrustedMainRuleset{r}, nil
		}
		if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil ||
			!strings.Contains(err.Error(), "trust root changed while the build ran") {
			t.Fatalf("a changed trust root was published under: %v", err)
		}
		assertNothingInstalled(t, request.OutputRoot)
	})

	t.Run("refuse a source that left trusted main", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		calls := 0
		f.deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
			calls++
			if calls > 1 {
				// main is now an unrelated history; the source is not in it.
				return RefObservation{Exists: true, SHA: f.orphan}, nil
			}
			return RefObservation{Exists: true, SHA: f.head}, nil
		}
		if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil ||
			!strings.Contains(err.Error(), "is not contained in trusted main") {
			t.Fatalf("a source that left trusted main was published: %v", err)
		}
		assertNothingInstalled(t, request.OutputRoot)
	})

	// A concurrent legitimate merge is NOT a failure: the source is still
	// contained, and the provenance simply states the trust state at
	// publication rather than the one the build started under.
	t.Run("accept a main that legitimately advanced", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		advanced := adoptedGit(t, f.dir, "commit-tree", f.headTree, "-p", f.head, "-m", "later")
		calls := 0
		f.deps.RefSHA = func(context.Context, GitHubRepo, string) (RefObservation, error) {
			calls++
			if calls > 1 {
				return RefObservation{Exists: true, SHA: advanced}, nil
			}
			return RefObservation{Exists: true, SHA: f.head}, nil
		}
		got, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
		if err != nil {
			t.Fatalf("a legitimate concurrent merge was treated as a failure: %v", err)
		}
		if got.TrustedMain.Revision != advanced {
			t.Fatalf("provenance names trusted main %s, want the observation at publication %s", got.TrustedMain.Revision, advanced)
		}
	})
}

// TestAdoptedBuildPublishesAtomicallyAndNeverReplaces covers defect X: a
// failure after the binary is in place must not leave an installed controller
// with incomplete provenance, and an existing version directory is the
// provenance of every run it has already governed.
func TestAdoptedBuildPublishesAtomicallyAndNeverReplaces(t *testing.T) {
	// Permission-based failures are not used here: these tests run as root in
	// the verification sandbox, where a mode bit stops nothing. These two
	// force the failure structurally instead, which is true everywhere.
	t.Run("an unusable output root installs nothing", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		blocked := filepath.Join(t.TempDir(), "root")
		if err := os.WriteFile(blocked, []byte("not a directory"), 0600); err != nil {
			t.Fatal(err)
		}
		request.OutputRoot = blocked
		if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil {
			t.Fatal("a build that could not stage its artifact reported success")
		}
		data, err := os.ReadFile(blocked)
		if err != nil || string(data) != "not a directory" {
			t.Fatalf("the unusable output root was disturbed: %q / %v", data, err)
		}
	})

	t.Run("a publish failure leaves no partial final directory", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		// A regular file where the version directory belongs: the rename
		// cannot succeed, and nothing may be left half-installed.
		occupied := filepath.Join(request.OutputRoot, "main-"+f.head[:8])
		if err := os.WriteFile(occupied, []byte("occupied"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
		if err == nil {
			t.Fatal("a build published over an occupied path")
		}
		data, readErr := os.ReadFile(occupied)
		if readErr != nil || string(data) != "occupied" {
			t.Fatalf("the occupied path was replaced: %q / %v", data, readErr)
		}
		// No staging debris either.
		entries, _ := os.ReadDir(request.OutputRoot)
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), ".staging-") {
				t.Fatalf("a failed publish left staging debris %q", entry.Name())
			}
		}
	})

	t.Run("an identical existing version is idempotent", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		first, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
		if err != nil {
			t.Fatal(err)
		}
		before, err := measureExecutable(first.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err != nil {
			t.Fatalf("rebuilding the same artifact was refused: %v", err)
		}
		after, err := measureExecutable(first.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if before != after {
			t.Fatal("an existing controller binary was replaced")
		}
	})

	t.Run("a different artifact for an existing version is refused", func(t *testing.T) {
		f := newAdoptedFixture(t)
		request := f.request(t)
		first, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
		if err != nil {
			t.Fatal(err)
		}
		installedBefore, err := measureExecutable(first.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		provenanceBefore, err := os.ReadFile(filepath.Join(filepath.Dir(first.OutputPath), "provenance.json"))
		if err != nil {
			t.Fatal(err)
		}
		// The same version, different content: exactly what must never
		// silently replace an installed controller.
		f.deps.Build = func(_ context.Context, spec AdoptedBuildSpec) (BuildEnvironment, error) {
			f.built = append(f.built, spec)
			return BuildEnvironment{}, os.WriteFile(spec.Output, []byte("a different binary"), 0600)
		}
		_, err = BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{})
		if err == nil || !strings.Contains(err.Error(), "version directories are immutable") {
			t.Fatalf("an installed controller was replaced: %v", err)
		}
		installedAfter, err := measureExecutable(first.OutputPath)
		if err != nil {
			t.Fatal(err)
		}
		if installedAfter != installedBefore {
			t.Fatal("the installed binary changed")
		}
		provenanceAfter, err := os.ReadFile(filepath.Join(filepath.Dir(first.OutputPath), "provenance.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(provenanceAfter) != string(provenanceBefore) {
			t.Fatal("the installed provenance changed")
		}
	})
}

// TestAdoptedBuildRefusesMissingProductionDependencies proves the API fails
// closed rather than panicking: there is no honest default for "which forge
// tells me what the trust root is".
func TestAdoptedBuildRefusesMissingProductionDependencies(t *testing.T) {
	f := newAdoptedFixture(t)
	for name, mutate := range map[string]func(*AdoptedBuildDeps){
		"no ruleset reader": func(d *AdoptedBuildDeps) { d.Rulesets = nil },
		"no ref observer":   func(d *AdoptedBuildDeps) { d.RefSHA = nil },
	} {
		t.Run(name, func(t *testing.T) {
			deps := f.deps
			mutate(&deps)
			if _, err := BuildAdoptedController(context.Background(), f.request(t), deps, BuilderRecord{}); err == nil ||
				!strings.Contains(err.Error(), "no way to observe") {
				t.Fatalf("a builder with no trust source did not fail closed: %v", err)
			}
		})
	}
}

// TestAdoptedBuildRefusesCrossTargetAdoption: a binary that cannot run here
// cannot be asked what it thinks it is, and an unprobed adopted artifact is
// exactly what this command exists to refuse. Cross-compiling stays available
// for unattested builds, which is where it belongs.
func TestAdoptedBuildRefusesCrossTargetAdoption(t *testing.T) {
	f := newAdoptedFixture(t)
	request := f.request(t)
	request.GOOS, request.GOARCH = "plan9", "mips"
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, BuilderRecord{}); err == nil ||
		!strings.Contains(err.Error(), "cross-target adoption is refused") {
		t.Fatalf("a cross-target adopted build was accepted: %v", err)
	}
	assertNothingInstalled(t, request.OutputRoot)
}

// TestAdoptedBuildProvenanceIsWrittenOwnerOnly: the record is not a secret, but
// it is operator material and it is written like it.
func TestAdoptedBuildProvenanceIsWrittenOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provenance.json")
	digest, err := WriteAdoptedBuildProvenance(path, AdoptedBuildProvenance{
		SchemaVersion: adoptedBuildSchemaVersion, Kind: ControllerAdopted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(digest) != 64 {
		t.Fatalf("digest = %q", digest)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("provenance mode = %#o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"token", "Authorization", "api_key", "secret"} {
		if strings.Contains(strings.ToLower(string(data)), strings.ToLower(forbidden)) {
			t.Fatalf("the provenance artifact names %q", forbidden)
		}
	}
}
