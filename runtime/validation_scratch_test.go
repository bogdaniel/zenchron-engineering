package runtime

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidationScratchGrants(t *testing.T) {
	scratch := filepath.Join(t.TempDir(), "build space")
	invocation := cliInvocation{CandidateDir: t.TempDir(), ScratchDir: scratch}
	for name, spec := range map[string]cliAgentSpec{"codex": codexSpec, "claude": claudeSpec} {
		t.Run(name, func(t *testing.T) {
			args := strings.Join(spec.Args(invocation), " ")
			if !strings.Contains(args, scratch) {
				t.Fatalf("scratch is not granted: %s", args)
			}
			if strings.Contains(strings.Join(spec.ReadOnly.Args(invocation), " "), scratch) {
				t.Fatal("planning received a write grant")
			}
		})
	}
	provider := CLIAgentProvider{Toolchain: ToolchainConfig{RequiredTools: []string{"go"}}, ExecScratchDir: scratch}
	env := strings.Join(provider.toolchainEnv(), "\n")
	for _, entry := range []string{"TMPDIR=" + scratch, "GOTMPDIR=" + scratch, "GOCACHE=" + filepath.Join(scratch, "cache"), "GOPATH=" + filepath.Join(scratch, "gopath"), "GOENV=off"} {
		if !strings.Contains(env, entry) {
			t.Fatalf("missing %q in %s", entry, env)
		}
	}
}

func TestValidationScratchAdmissionSubject(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOTMPDIR", root)
	attempt := ExecutionAttemptRef{RunID: "run-cache", OperationID: "run-cache:execution.invoke:x", Attempt: 1}
	scratch, err := ExecutionScratchDir(root, attempt)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareValidationScratch(candidate, scratch); err != nil {
		t.Fatal(err)
	}
	replay, err := ExecutionScratchDir(root, attempt)
	if err != nil || replay != scratch {
		t.Fatalf("replay scratch: %q, %v", replay, err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "source.go"), []byte("package source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, shape := range []string{".test-cache/38/3836bcb5017074e3b9d9e26170671e008544641fe4552451a3736b807d22cf4b-d", ".work-cache/build/4d/4d6e240d199dcaafed1d7ddf853b224f41112a0efa40ade9ad60f83b72cfd0b6-d"} {
		t.Run(shape, func(t *testing.T) {
			cache := filepath.Join(scratch, shape)
			if err := os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
				t.Fatal(err)
			}
			f, err := os.Create(cache)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(credentialScanFileLimit + 1); err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
			if err := ScanCandidateForCredentialValues(candidate); err != nil {
				t.Fatalf("runtime cache entered admission: %v", err)
			}
			source := filepath.Join(candidate, shape)
			if err := os.MkdirAll(filepath.Dir(source), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(cache, source); err != nil {
				t.Fatal(err)
			}
			assertCredentialKind(t, ScanCandidateForCredentialValues(candidate), CredentialMaterialInconclusive)
			assertCredentialKind(t, scanPathsForCredentialValues(candidate, []string{shape}), CredentialMaterialInconclusive)
			if err := os.WriteFile(source, []byte("ghp_"+strings.Repeat("a", 36)), 0600); err != nil {
				t.Fatal(err)
			}
			assertCredentialKind(t, ScanCandidateForCredentialValues(candidate), CredentialMaterialValue)
			assertCredentialKind(t, scanPathsForCredentialValues(candidate, []string{shape}), CredentialMaterialValue)
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertCredentialKind(t *testing.T, err error, kind CredentialMaterialKind) {
	t.Helper()
	var refusal *CredentialMaterialError
	if !errors.As(err, &refusal) || refusal.Kind != kind {
		t.Fatalf("want %s refusal, got %v", kind, err)
	}
}

func TestValidationScratchCannotOverlapCandidate(t *testing.T) {
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	if err := os.Mkdir(candidate, 0700); err != nil {
		t.Fatal(err)
	}
	for _, scratch := range []string{root, candidate, filepath.Join(candidate, "cache")} {
		if err := prepareValidationScratch(candidate, scratch); err == nil {
			t.Fatalf("allowed overlap: %s", scratch)
		}
	}
	link := filepath.Join(root, "alias")
	if err := os.Symlink(candidate, link); err != nil {
		t.Fatal(err)
	}
	if err := prepareValidationScratch(candidate, link); err == nil {
		t.Fatal("allowed symlink into candidate")
	}
}

// Exercise Go itself: its build cache and testing.T temporary directories must
// leave the candidate source inventory unchanged.
func TestGoValidationLeavesOnlySourceInCandidate(t *testing.T) {
	workspace := commitGateWorkspace(t)
	candidate := workspace.Dir
	// The verifier's default temporary directory may be mounted noexec.
	// Use the runtime's published executable scratch for Go's test binary,
	// while retaining an isolated directory and test-owned cleanup.
	scratch, err := os.MkdirTemp(ExecCapableScratchBase(t.TempDir()), "validation-scratch-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(scratch); err != nil {
			t.Errorf("remove validation scratch: %v", err)
		}
	})
	if err := prepareValidationScratch(candidate, scratch); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"go.mod":    "module validationfixture\n\ngo 1.25.0\n",
		"source.go": "package validationfixture\nfunc Value() int { return 2 }\n",
		"source_test.go": `package validationfixture
import ("os"; "path/filepath"; "strings"; "testing")
func TestValue(t *testing.T) {
 if Value() != 2 { t.Fatal("wrong source") }
 rel, err := filepath.Rel(os.Getenv("TMPDIR"), t.TempDir())
 if err != nil || strings.HasPrefix(rel, "..") { t.Fatal("test scratch escaped") }
}
`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(candidate, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	provider := CLIAgentProvider{Toolchain: ToolchainConfig{RequiredTools: []string{"go"}}, ExecScratchDir: scratch}
	cmd := exec.Command("go", "test", "./...")
	cmd.Dir = candidate
	cmd.Env = append(os.Environ(), provider.toolchainEnv()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go validation: %v\n%s", err, out)
	}
	entries, err := os.ReadDir(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(files)+2 {
		t.Fatalf("validation added candidate material: %v", entries)
	}
	cache, err := os.ReadDir(filepath.Join(scratch, "cache"))
	if err != nil || len(cache) == 0 {
		t.Fatalf("Go did not populate runtime scratch: %v", err)
	}
	if err := ScanCandidateForCredentialValues(candidate); err != nil {
		t.Fatal(err)
	}
	result, err := workspace.Commit("validated source", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Paths, ",") != "go.mod,source.go,source_test.go" {
		t.Fatalf("scratch entered changed-path evidence: %v", result.Paths)
	}
	observed, err := workspace.ObservedChange(result)
	if err != nil || strings.Join(observed.Paths, ",") != "go.mod,source.go,source_test.go" {
		t.Fatalf("reassessment subject: %v, %v", observed, err)
	}
	tree, err := gitOutput(candidate, "ls-tree", "-r", "--name-only", "HEAD")
	if err != nil || strings.TrimSpace(tree) != "README.md\ngo.mod\nsource.go\nsource_test.go" {
		t.Fatalf("commit subject: %q, %v", tree, err)
	}
}

func TestCommitCredentialScanRefusesStatFailure(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "file"), []byte("source"), 0600); err != nil {
		t.Fatal(err)
	}
	assertCredentialKind(t, scanPathsForCredentialValues(root, []string{"file/child"}), CredentialMaterialInconclusive)
	if err := scanPathsForCredentialValues(root, []string{"deleted"}); err != nil {
		t.Fatalf("deletion refused: %v", err)
	}
}
