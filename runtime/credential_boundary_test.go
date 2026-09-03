package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Synthetic credential-shaped fixtures are ASSEMBLED, never written out.
//
// A test that pastes a complete realistic secret into repository source makes
// the source itself look sensitive - which is the exact defect #52 exists to
// remove, reintroduced by the tests that prove it was removed. Every fixture
// below is a harmless prefix plus deterministic filler, so the repository text
// contains vocabulary only and the VALUE exists solely at test runtime.
func githubClassicTokenValue() string {
	return "ghp_" + strings.Repeat("A", 36)
}

func githubFineGrainedTokenValue() string {
	return "github_pat_" + strings.Repeat("B", 22) + "_" + strings.Repeat("C", 59)
}

func awsSecretAssignment() string {
	return "aws_secret_access_key = " + strings.Repeat("D", 40)
}

func pemPrivateKeyBlock() string {
	return "-----BEGIN PRIVATE KEY-----\n" + strings.Repeat("QUJDRA", 20) + "\n-----END PRIVATE KEY-----"
}

// detectorVocabulary is what a scanner, a redactor, a PEM parser or a test for
// any of them contains: the NAMES of credentials, with nothing assigned.
func detectorVocabulary() string {
	return strings.Join([]string{
		"github_pat_",
		"ghp_",
		"aws_secret_access_key",
		"-----BEGIN PRIVATE KEY-----",
	}, "\n")
}

// TestCredentialDetectorSeparatesVocabularyFromValues is the core of #52: the
// old predicate could not tell the name of a secret from a secret, so the
// runtime classified its own scanner source as credential material.
func TestCredentialDetectorSeparatesVocabularyFromValues(t *testing.T) {
	for name, text := range map[string]string{
		"a bare fine-grained prefix": "const githubFineGrained = \"github_pat_\"",
		// A documentation placeholder is long enough to look like a token body
		// but has no separator. Treating it as a credential would be the same
		// false positive one layer down.
		"a documentation placeholder":  "GITHUB_TOKEN=github_pat_REPLACE_WITH_YOUR_TOKEN",
		"a near-miss without the tail": "github_pat_" + strings.Repeat("F", 22),
		"a bare classic prefix":        "if strings.HasPrefix(token, \"ghp_\") {",
		"an environment variable name": "for _, key := range []string{\"AWS_SECRET_ACCESS_KEY\", \"aws_secret_access_key\"} {",
		"a lone PEM marker":            "const pemHeader = \"-----BEGIN PRIVATE KEY-----\"",
		"a truncated PEM fixture":      "-----BEGIN PRIVATE KEY-----\nfixture-9c3\n",
		"the whole detector alphabet":  detectorVocabulary(),
	} {
		if ContainsCredentialValue([]byte(text)) {
			t.Errorf("%s was classified as a credential value: %q", name, text)
		}
	}
	for name, text := range map[string]string{
		"a classic GitHub token":       "token: " + githubClassicTokenValue(),
		"a fine-grained GitHub token":  "Authorization: bearer " + githubFineGrainedTokenValue(),
		"an assigned AWS secret":       awsSecretAssignment(),
		"a quoted AWS secret":          "aws_secret_access_key=\"" + strings.Repeat("E", 40) + "\"",
		"a complete PEM private key":   pemPrivateKeyBlock(),
		"a labelled PEM private key":   strings.Replace(pemPrivateKeyBlock(), "BEGIN PRIVATE", "BEGIN RSA PRIVATE", 1),
		"a value inside ordinary text": "deploy uses " + githubClassicTokenValue() + " today",
	} {
		if !ContainsCredentialValue([]byte(text)) {
			t.Errorf("%s was NOT detected; a missed credential shape is a security defect", name)
		}
	}
}

// TestTheCredentialDetectorDoesNotFlagItsOwnSource is the regression that #52
// actually is. The detector's definition necessarily contains every prefix and
// marker it looks for; if that makes the file credential material, the runtime
// cannot maintain its own security code.
func TestTheCredentialDetectorDoesNotFlagItsOwnSource(t *testing.T) {
	for _, name := range []string{"credential_boundary.go", "sandbox.go", "adapters.go", "tool_broker.go", "tool_surface.go"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if ContainsCredentialValue(data) {
			t.Errorf("runtime/%s is classified as credential material by its own detector", name)
		}
	}
}

func TestRedactionRemovesValuesAndKeepsVocabulary(t *testing.T) {
	token := githubClassicTokenValue()
	redacted := RedactCredentialValues("stdout:\n" + token + "\ndone\n")
	if strings.Contains(redacted, token) {
		t.Fatalf("a token survived redaction: %q", redacted)
	}
	if !strings.Contains(redacted, credentialRedaction) {
		t.Fatalf("redaction produced no marker: %q", redacted)
	}
	// The identifier survives; only what was assigned to it does not.
	aws := RedactCredentialValues(awsSecretAssignment())
	if strings.Contains(aws, strings.Repeat("D", 40)) {
		t.Fatalf("an AWS secret survived redaction: %q", aws)
	}
	if !strings.Contains(aws, "aws_secret_access_key") {
		t.Fatalf("redaction destroyed the identifier the model needs: %q", aws)
	}
	pem := RedactCredentialValues("key:\n" + pemPrivateKeyBlock() + "\n")
	if strings.Contains(pem, "QUJDRA") || strings.Contains(pem, "BEGIN PRIVATE KEY") {
		t.Fatalf("a PEM block survived redaction: %q", pem)
	}
	// Vocabulary is left completely alone, so scanner source stays readable.
	vocabulary := detectorVocabulary()
	if got := RedactCredentialValues(vocabulary); got != vocabulary {
		t.Fatalf("redaction damaged detector vocabulary:\nwant %q\ngot  %q", vocabulary, got)
	}
}

// TestPathGateAcceptsSecurityCodeAndRefusesCredentialFiles is matrix D and E.
func TestPathGateAcceptsSecurityCodeAndRefusesCredentialFiles(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{
		"secret_scanner.go", "secret_detection_test.go", "private_key_parser.go",
		"credential_policy.go", "sandbox.go", "adapters.go",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(detectorVocabulary()), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := GuardCandidate(root, []string{name}, 1<<20); err != nil {
			t.Errorf("the path gate refused security source %q: %v", name, err)
		}
	}
	for _, name := range []string{".env", ".env.local", "deploy.env", "id_rsa", "id_ed25519", "server.pem", "store.p12", "credentials", ".netrc"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := GuardCandidate(root, []string{name}, 1<<20); err == nil {
			t.Errorf("the path gate accepted credential file %q", name)
		}
	}
}

// countingProvider proves matrix R: a refusal that happens BEFORE inference
// costs zero reasoning iterations, which is only observable as "Execute was
// never called".
type countingProvider struct{ calls int }

func (p *countingProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *countingProvider) Execute(context.Context, ExecutionRequest) (ExecutionResult, error) {
	p.calls++
	return ExecutionResult{ProviderID: "counting", Outcome: Succeeded}, nil
}

// TestProviderAdmissionRefusesCredentialMaterialBeforeInference is matrix F, G,
// H and R. The gate is the mount: once a producer holds this workspace,
// candidate.run reads anything in it, so the decision has to be made here.
func TestProviderAdmissionRefusesCredentialMaterialBeforeInference(t *testing.T) {
	for name, body := range map[string]string{
		"a GitHub token":     githubFineGrainedTokenValue(),
		"an AWS secret":      awsSecretAssignment(),
		"a PEM private key":  pemPrivateKeyBlock(),
		"a classic PAT":      githubClassicTokenValue(),
		"a value in a patch": "+++ b/config.yaml\n+token: " + githubClassicTokenValue() + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			// The material is in the BASE, so the candidate workspace the
			// runtime clones contains it before any producer could be admitted
			// to it. That is exactly the state a producer would be handed.
			fixture := newPhase8Fixture(t, func(origin string) {
				if err := os.WriteFile(filepath.Join(origin, "planted.txt"), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			})
			provider := &countingProvider{}
			fixture.deps.Provider = provider
			fixture.runtime = fixture.newRuntime(fixture.deps)
			runID := fixture.start()
			for pass := 0; pass < 10; pass++ {
				fixture.reconcile(runID)
				if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
					break
				}
			}
			if provider.calls != 0 {
				t.Fatalf("provider was invoked %d time(s) despite credential material in the candidate", provider.calls)
			}
			class, found := executionFailureClass(t, fixture.state(runID))
			if !found || class != FailureCandidateCredentialMaterial {
				t.Fatalf("execution failure class = %q found=%v, want %q", class, found, FailureCandidateCredentialMaterial)
			}
			if disposition := fixture.state(runID).snapshot.Disposition; !terminalDisposition(disposition) {
				t.Fatalf("a local prerequisite refusal left the run %q", disposition)
			}
		})
	}
}

// TestAdmissionRefusesCredentialFilesWhateverTheyContain is the second half of
// the bypass. A .env holding an opaque value matches no detector, the path gate
// refuses to BROKER it, and candidate.run reads it anyway from the bind mount -
// so admission is the only layer where refusing it means anything.
func TestAdmissionRefusesCredentialFilesWhateverTheyContain(t *testing.T) {
	for name, body := range map[string]string{
		".env":        "TOKEN=hunter2\n",
		"id_rsa":      "opaque key material\n",
		"store.p12":   "binary-ish\n",
		"deploy.env":  "API_KEY=abc\n",
		"server.pem":  "not even a PEM\n",
		"credentials": "[default]\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			err := ScanCandidateForCredentialValues(root)
			if err == nil {
				t.Fatalf("admission accepted a candidate containing %q", name)
			}
			var material *CredentialMaterialError
			if !asCredentialMaterial(err, &material) || material.Kind != CredentialMaterialFile {
				t.Fatalf("admission refusal was not a credential-file finding: %#v", err)
			}
		})
	}
	// Committed documentation is not a credential, and refusing it would make a
	// very large share of repositories unworkable.
	for _, name := range []string{".env.example", ".env.sample", ".env.template"} {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, name), []byte("TOKEN=changeme\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := ScanCandidateForCredentialValues(root); err != nil {
			t.Errorf("admission refused committed documentation %q: %v", name, err)
		}
	}
}

// TestProviderAdmissionRefusesCredentialFilesBeforeInference proves the same
// through the runtime, with the provider counting its own invocations.
func TestProviderAdmissionRefusesCredentialFilesBeforeInference(t *testing.T) {
	fixture := newPhase8Fixture(t, func(origin string) {
		if err := os.WriteFile(filepath.Join(origin, ".env"), []byte("TOKEN=hunter2\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	})
	provider := &countingProvider{}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	for pass := 0; pass < 10; pass++ {
		fixture.reconcile(runID)
		if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
			break
		}
	}
	if provider.calls != 0 {
		t.Fatalf("provider was invoked %d time(s) despite a .env in the candidate", provider.calls)
	}
	class, found := executionFailureClass(t, fixture.state(runID))
	if !found || class != FailureCandidateCredentialMaterial {
		t.Fatalf("execution failure class = %q found=%v, want %q", class, found, FailureCandidateCredentialMaterial)
	}
}

// TestProviderAdmissionAcceptsDetectorVocabulary is the other half: a candidate
// full of scanner source is admitted normally.
func TestProviderAdmissionAcceptsDetectorVocabulary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "secret_scanner.go"), []byte(detectorVocabulary()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ScanCandidateForCredentialValues(root); err != nil {
		t.Fatalf("admission refused a candidate that contains only vocabulary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "leaked.txt"), []byte(githubClassicTokenValue()), 0o600); err != nil {
		t.Fatal(err)
	}
	err := ScanCandidateForCredentialValues(root)
	if err == nil {
		t.Fatal("admission accepted a candidate containing a token value")
	}
	var material *CredentialMaterialError
	if !asCredentialMaterial(err, &material) || material.Path != "leaked.txt" || material.Kind != CredentialMaterialValue {
		t.Fatalf("admission refusal did not name the path and kind: %#v", err)
	}
	// The diagnostic must not quote the secret it is refusing.
	if strings.Contains(err.Error(), githubClassicTokenValue()) {
		t.Fatal("the admission diagnostic reproduced the credential value")
	}
}

// TestAdmissionIgnoresRuntimeOwnedGitMetadata keeps the scan to candidate
// content: .git is masked inside the producer sandbox and is runtime state.
func TestAdmissionIgnoresRuntimeOwnedGitMetadata(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "config"), []byte(githubClassicTokenValue()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ScanCandidateForCredentialValues(root); err != nil {
		t.Fatalf("admission read runtime-owned Git metadata: %v", err)
	}
}

// TestCommitGateRefusesIntroducedCredentialValues is matrix M and N. Admission
// protects the input; this is the separate fact about the output.
func TestCommitGateRefusesIntroducedCredentialValues(t *testing.T) {
	workspace := commitGateWorkspace(t)
	if err := os.WriteFile(filepath.Join(workspace.Dir, "ordinary.go"), []byte("package main\n\n// "+detectorVocabulary()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Commit("ordinary change", 1<<20); err != nil {
		t.Fatalf("the commit gate refused an ordinary change: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace.Dir, "leaked.go"), []byte("package main\n\nconst t = \""+githubClassicTokenValue()+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := workspace.Commit("introduces a credential", 1<<20)
	if err == nil {
		t.Fatal("the commit gate committed an introduced credential value")
	}
	var material *CredentialMaterialError
	if !asCredentialMaterial(err, &material) || material.Path != "leaked.go" {
		t.Fatalf("the commit refusal did not name the introduced path: %#v", err)
	}
}

// TestBrokerReadsSearchesAndPatchesSecuritySource is matrix A, B, C, O and P:
// the capability set that #50 needed and could not have.
func TestBrokerReadsSearchesAndPatchesSecuritySource(t *testing.T) {
	root := t.TempDir()
	broker := ToolBroker{CandidateDir: root, MaxBytes: 1 << 20}
	sources := map[string]string{
		"sandbox.go":         "package runtime\n\nvar transcriptSecrets = regexp.MustCompile(`github_pat_[a-z0-9_]+|aws_secret_access_key`)\n",
		"adapters.go":        "package runtime\n\n// contains(\"github_pat_\") used to refuse this file.\n",
		"pem_parser.go":      "package runtime\n\nconst header = \"-----BEGIN PRIVATE KEY-----\"\n",
		"secret_scanner.go":  "package runtime\n\nconst prefix = \"ghp_\"\n",
		"aws_credentials.go": "package runtime\n\nconst awsKey = \"aws_secret_access_key\"\n",
	}
	for name, body := range sources {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	initCommitGateRepo(t, root)
	for name, body := range sources {
		data, err := broker.ReadFile(name)
		if err != nil {
			t.Fatalf("brokered read of security source %q was refused: %v", name, err)
		}
		if string(data) != body {
			t.Fatalf("brokered read of %q returned different bytes", name)
		}
		hits, err := broker.Search("regexp|const|contains", []string{name})
		if err != nil {
			t.Fatalf("brokered search scoped to %q was refused: %v", name, err)
		}
		if len(hits) == 0 {
			t.Fatalf("brokered search of %q found nothing", name)
		}
	}
	// Matrix O and P: the file the #50 repair has to change is patchable.
	patch := "diff --git a/sandbox.go b/sandbox.go\n--- a/sandbox.go\n+++ b/sandbox.go\n@@ -1,3 +1,4 @@\n package runtime\n \n var transcriptSecrets = regexp.MustCompile(`github_pat_[a-z0-9_]+|aws_secret_access_key`)\n+// GOMODCACHE would be established here.\n"
	if err := broker.ApplyPatch([]byte(patch)); err != nil {
		t.Fatalf("brokered patch of the sandbox source was refused: %v", err)
	}
	patched, err := os.ReadFile(filepath.Join(root, "sandbox.go"))
	if err != nil || !strings.Contains(string(patched), "GOMODCACHE would be established") {
		t.Fatalf("the patch did not reach the workspace: %v %q", err, patched)
	}
}

// TestToolResultsRedactCredentialValues is matrix I, J, K and L. Every
// model-visible path is checked through the same surface the model uses.
func TestToolResultsRedactCredentialValues(t *testing.T) {
	root := t.TempDir()
	token := githubClassicTokenValue()
	if err := os.WriteFile(filepath.Join(root, "leaked.txt"), []byte("token: "+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "vocabulary.go"), []byte(detectorVocabulary()), 0o600); err != nil {
		t.Fatal(err)
	}
	initCommitGateRepo(t, root)
	if err := os.WriteFile(filepath.Join(root, "leaked.txt"), []byte("token: "+token+"\nchanged\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := ToolSurface{Broker: ToolBroker{CandidateDir: root, MaxBytes: 1 << 20}}
	ctx := context.Background()
	for name, call := range map[string]struct{ tool, args string }{
		"repo.read":      {ToolRepoRead, `{"path":"leaked.txt"}`},
		"repo.search":    {ToolRepoSearch, `{"pattern":"token","scope":[]}`},
		"candidate.diff": {ToolCandidateDiff, `{"paths":[]}`},
	} {
		result, failed := surface.Invoke(ctx, call.tool, []byte(call.args))
		if failed {
			t.Fatalf("%s failed: %s", name, result)
		}
		if strings.Contains(result, token) {
			t.Fatalf("%s returned an unredacted credential value: %q", name, result)
		}
		if !strings.Contains(result, credentialRedaction) {
			t.Fatalf("%s returned no redaction marker: %q", name, result)
		}
	}
	// Matrix J: vocabulary stays intelligible, so scanner source is usable.
	result, failed := surface.Invoke(ctx, ToolRepoRead, []byte(`{"path":"vocabulary.go"}`))
	if failed || result != detectorVocabulary() {
		t.Fatalf("detector vocabulary was damaged in a tool result: failed=%v %q", failed, result)
	}
}

// TestCandidateRunOutputIsRedacted is matrix I for the capability that was the
// actual bypass: candidate.run could print a file repo.read refused.
func TestCandidateRunOutputIsRedacted(t *testing.T) {
	token := githubClassicTokenValue()
	// candidate.run needs only the workspace, not a repository: it is an argv
	// through the sandbox and asks Git nothing.
	root := t.TempDir()
	// The container's output is what `docker start --attach` returns: the third
	// Run the sandbox issues, after the readiness probe and create.
	fake := &fakeCommandExecutor{found: true, outputs: []CommandOutput{{}, {}, {
		Stdout: []byte("found: " + token + "\n"),
		Stderr: []byte("context: " + token + "\n"),
	}}}
	surface := ToolSurface{Broker: ToolBroker{
		CandidateDir: root,
		Sandbox:      DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "redaction-operation", StateDir: t.TempDir()},
	}}
	result, failed := surface.Invoke(context.Background(), ToolCandidateRun, []byte(`{"command":["cat","leaked.txt"]}`))
	if failed {
		t.Fatalf("candidate.run failed: %s", result)
	}
	if strings.Contains(result, token) {
		t.Fatalf("candidate.run returned an unredacted credential value: %q", result)
	}
	if strings.Count(result, credentialRedaction) != 2 {
		t.Fatalf("candidate.run redacted %d of 2 streams: %q", strings.Count(result, credentialRedaction), result)
	}
	// Matrix J on the same capability: vocabulary-only output is untouched.
	plainFake := &fakeCommandExecutor{found: true, outputs: []CommandOutput{{}, {}, {Stdout: []byte(detectorVocabulary() + "\n")}}}
	plainSurface := ToolSurface{Broker: ToolBroker{
		CandidateDir: root,
		Sandbox:      DockerSandbox{Image: "sha256:image", Executor: plainFake, OperationID: "vocabulary-operation", StateDir: t.TempDir()},
	}}
	plain, failed := plainSurface.Invoke(context.Background(), ToolCandidateRun, []byte(`{"command":["cat","scanner.go"]}`))
	if failed || !strings.Contains(plain, "aws_secret_access_key") || strings.Contains(plain, credentialRedaction) {
		t.Fatalf("candidate.run damaged detector vocabulary: failed=%v %q", failed, plain)
	}
}

// TestToolFailuresAreRedactedToo closes the quieter channel: a diagnostic is
// still text the model reads.
func TestToolFailuresAreRedactedToo(t *testing.T) {
	token := githubClassicTokenValue()
	root := t.TempDir()
	surface := ToolSurface{Broker: ToolBroker{CandidateDir: root, MaxBytes: 1 << 20}}
	result, failed := surface.Invoke(context.Background(), ToolRepoRead, []byte(`{"path":"`+token+`.txt"}`))
	if !failed {
		t.Fatal("reading a missing path succeeded")
	}
	if strings.Contains(result, token) {
		t.Fatalf("a tool error echoed a credential value back to the model: %q", result)
	}
}

// TestRedactionPrecedesBounding proves the ordering. Truncating first would
// make confidentiality depend on where in the output a secret happened to sit.
func TestRedactionPrecedesBounding(t *testing.T) {
	token := githubClassicTokenValue()
	root := t.TempDir()
	body := strings.Repeat("filler line\n", 200) + token + "\n"
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	surface := ToolSurface{Broker: ToolBroker{CandidateDir: root, MaxBytes: 1 << 20}, MaxResultBytes: len(body) + 32}
	result, failed := surface.Invoke(context.Background(), ToolRepoRead, []byte(`{"path":"big.txt"}`))
	if failed {
		t.Fatalf("read failed: %s", result)
	}
	if strings.Contains(result, token) {
		t.Fatalf("a credential value survived because bounding ran first: %q", result)
	}
}

// initCommitGateRepo makes the directory a real repository, because both the
// commit gate and the diff capability answer with Git rather than about it.
func initCommitGateRepo(t *testing.T, dir string) {
	t.Helper()
	if _, err := runGit("", "init", dir); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"config", "user.name", "fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if _, err := runGit(dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runGit(dir, "add", "-A"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(dir, "commit", "--no-gpg-sign", "-m", "fixture base"); err != nil {
		t.Fatal(err)
	}
}

func commitGateWorkspace(t *testing.T) *CandidateWorkspace {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	initCommitGateRepo(t, dir)
	metadata, err := gitMetadataDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return &CandidateWorkspace{Dir: dir, TrustedMetadata: metadata}
}

// executionFailureClass reads what the producer operation itself recorded. The
// credential refusal is an EXECUTION fact, not an assurance or CI verdict, so
// it is read where the runtime wrote it.
func executionFailureClass(t *testing.T, state *runState) (FailureClass, bool) {
	t.Helper()
	for _, operation := range state.snapshot.Operations {
		if operation.Kind != OpExecutionInvoke || len(operation.Result) == 0 {
			continue
		}
		var record executionRecord
		if err := json.Unmarshal(operation.Result, &record); err != nil {
			t.Fatalf("unmarshalling the execution record: %v", err)
		}
		if record.FailureClass != "" {
			return record.FailureClass, true
		}
	}
	return "", false
}

func asCredentialMaterial(err error, target **CredentialMaterialError) bool {
	for err != nil {
		if material, ok := err.(*CredentialMaterialError); ok {
			*target = material
			return true
		}
		unwrapper, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = unwrapper.Unwrap()
	}
	return false
}
