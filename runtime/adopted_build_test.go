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
		Git: func(dir string, args ...string) (string, error) {
			// The fetch is a no-op here: the fixture remote is the repository
			// itself, and every revision is already local.
			if len(args) > 0 && args[0] == "fetch" {
				return "", nil
			}
			return gitOutput(dir, args...)
		},
		Build: func(spec AdoptedBuildSpec) error {
			f.built = append(f.built, spec)
			return os.WriteFile(spec.Output, []byte("binary for "+spec.Revision), 0600)
		},
		Now: func() time.Time { return time.Unix(1800000000, 0).UTC() },
	}
	return f
}

func (f *adoptedFixture) request(t *testing.T) AdoptedBuildRequest {
	t.Helper()
	return AdoptedBuildRequest{
		Repository:    GitHubRepo{Owner: "acme", Name: "widgets"},
		RepositoryDir: f.dir, OutputDir: t.TempDir(),
		GOOS: "linux", GOARCH: "amd64", Policy: DefaultTrustPolicy(),
	}
}

// TestAdoptedBuildProvesBeforeItBuilds is the success path, and the assertions
// are about the PROOF: the tree is recomputed from the checkout, containment is
// re-derived from the remote-observed head, and the digest is measured from
// what was actually written.
func TestAdoptedBuildProvesBeforeItBuilds(t *testing.T) {
	f := newAdoptedFixture(t)
	got, err := BuildAdoptedController(context.Background(), f.request(t), f.deps,
		ControllerBuild{Kind: ControllerAdopted, Version: "builder-1"})
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
	if !spec.TrimPath || !spec.CGODisabled || spec.Kind != ControllerAdopted ||
		spec.Revision != f.head || spec.Tree != f.headTree {
		t.Fatalf("build spec = %+v", spec)
	}
	if got.SelfReport == "" {
		t.Fatal("provenance does not say whether the binary was probed")
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
	got, err := BuildAdoptedController(context.Background(), request, f.deps, ControllerBuild{})
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
				f.deps.Build = func(AdoptedBuildSpec) error { return fmt.Errorf("compile error") }
			}, "no binary was produced",
		},
		"digest cannot be measured": {
			func(f *adoptedFixture, _ *AdoptedBuildRequest) {
				f.deps.Build = func(AdoptedBuildSpec) error { return nil } // writes nothing
			}, "could not be measured",
		},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			f := newAdoptedFixture(t)
			request := f.request(t)
			tc.arrange(f, &request)
			got, err := BuildAdoptedController(context.Background(), request, f.deps, ControllerBuild{})
			if err == nil {
				t.Fatalf("an unprovable build succeeded: %+v", got)
			}
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("refusal does not explain %q: %v", tc.says, err)
			}
			// Nothing may be installed by a refused build.
			if entries, _ := os.ReadDir(request.OutputDir); len(entries) > 0 {
				for _, e := range entries {
					if e.Name() == "zenchron-engineering" {
						t.Fatalf("a refused build left a binary behind")
					}
				}
			}
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
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, ControllerBuild{}); err == nil ||
		!strings.Contains(err.Error(), "holds tree") {
		t.Fatalf("a checkout that is not the revision's tree was accepted: %v", err)
	}
}

// TestAdoptedBuildRefusesABinaryThatMisreportsItself proves the last gate: a
// build whose ldflags did not take effect produces a working binary that lies
// about what it is, and nothing downstream would notice.
func TestAdoptedBuildRefusesABinaryThatMisreportsItself(t *testing.T) {
	f := newAdoptedFixture(t)
	request := f.request(t)
	request.GOOS, request.GOARCH = "", "" // probe only runs for a native build
	request.DoctorConfig = filepath.Join(t.TempDir(), "config.json")

	f.deps.Probe = func(string, string) (DoctorCheck, error) {
		return DoctorCheck{ID: "controller.build", Status: DoctorPass,
			Reason: "this controller's build provenance is unattested"}, nil
	}
	_, err := BuildAdoptedController(context.Background(), request, f.deps, ControllerBuild{})
	if err == nil || !strings.Contains(err.Error(), "does not report") {
		t.Fatalf("a binary that misreports its own provenance was accepted: %v", err)
	}

	f.deps.Probe = func(string, string) (DoctorCheck, error) {
		return DoctorCheck{ID: "controller.build", Status: DoctorFail, Reason: "provenance could not be established"}, nil
	}
	if _, err := BuildAdoptedController(context.Background(), request, f.deps, ControllerBuild{}); err == nil ||
		!strings.Contains(err.Error(), "reports controller.build FAIL") {
		t.Fatalf("a binary failing its own check was accepted: %v", err)
	}
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
