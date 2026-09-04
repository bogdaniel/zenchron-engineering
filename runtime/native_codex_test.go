package runtime

// Every test here drives NativeCodexProvider through the fake CommandExecutor.
// No real Codex process is started and no paid model call is ever made. A test
// that needed a real provider would have to gate itself on an environment
// variable the way TestDockerSandboxOwnsExactContainerLifecycleWhenConfigured
// does.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// codexCapableHelp models an installed CLI that advertises the sandbox
// capability the runtime depends on.
const codexCapableHelp = `Options:
  -c, --config <key=value>
  -s, --sandbox <SANDBOX_MODE>  [possible values: read-only, workspace-write, danger-full-access]
  -a, --ask-for-approval <APPROVAL_POLICY>
      --ignore-user-config
  -C, --cd <DIR>
`

func nativeCodexFixture(t *testing.T) (NativeCodexProvider, ExecutionRequest, *fakeCommandExecutor) {
	t.Helper()
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	home := filepath.Join(root, "codex-home")
	for _, dir := range []string{candidate, home} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeCommandExecutor{found: true, codexHelp: codexCapableHelp}
	provider := NativeCodexProvider{ArtifactStore: ArtifactStore{Root: filepath.Join(root, "artifacts")}, Model: "model", AuthMode: "chatgpt", CodexHome: home, Executor: fake}
	request := ExecutionRequest{RunID: "run", OperationID: "run:execution.invoke:execution.invoke#initial|1|base-sha", Attempt: 1, SourceSnapshot: Ref{ID: "issue-snapshot", Revision: "1"}, ControllerID: "controller", Base: Ref{ID: "base", Revision: "base-sha"}, Candidate: Candidate{Revision: "candidate-sha", Tree: "tree-sha"}, CandidateDir: candidate, Contract: Ref{ID: "contract", Revision: "4"}, Objective: "fix", AcceptanceObligations: []string{"test"}, Constraints: []string{"bounded"}, Prohibitions: []string{"no publish"}, Permissions: []string{"edit"}, TrustedInstructions: "trusted-only", Purpose: InvocationInitial}
	return provider, request, fake
}

// codexInvocation returns the single recorded execution of the Codex CLI, which
// is the only place candidate work can be requested.
func codexInvocation(t *testing.T, fake *fakeCommandExecutor) recordedCommand {
	t.Helper()
	for _, call := range fake.calls {
		if call.name == "codex" && len(call.args) > 2 && call.args[len(call.args)-1] != "--help" {
			return call
		}
	}
	t.Fatalf("no Codex execution was recorded: %#v", fake.calls)
	return recordedCommand{}
}

// recordedText is every argument and every environment entry of every recorded
// command, which is the whole surface a candidate or ambient secret could
// travel on.
func recordedText(fake *fakeCommandExecutor) string {
	var all []string
	for _, call := range fake.calls {
		all = append(all, call.name, call.dir, strings.Join(call.args, " "), strings.Join(call.env, " "))
	}
	return strings.Join(all, "\n")
}

func TestNativeCodexBindsExactCandidateWorkspace(t *testing.T) {
	provider, request, fake := nativeCodexFixture(t)
	result, err := provider.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProviderID != "native-codex" || result.Outcome != Succeeded || result.Attempt != 1 {
		t.Fatalf("provider observation lost: %#v", result)
	}
	call := codexInvocation(t, fake)
	if call.dir != request.CandidateDir {
		t.Fatalf("working directory %q is not the candidate workspace %q", call.dir, request.CandidateDir)
	}
	if !strings.Contains(strings.Join(call.args, " "), "--cd "+request.CandidateDir) {
		t.Fatalf("candidate workspace was not bound exactly: %#v", call.args)
	}
	// No second workspace is granted: --add-dir would widen the writable set.
	if strings.Contains(strings.Join(call.args, " "), "--add-dir") {
		t.Fatalf("provider widened the writable workspace: %#v", call.args)
	}
	if len(result.Artifacts) != 2 || result.Artifacts[0].Publishable || result.Artifacts[1].Publishable || !result.Artifacts[1].Sanitized {
		t.Fatalf("artifact separation lost: %#v", result.Artifacts)
	}
}

func TestNativeCodexRequestsSandboxConstraintsExplicitly(t *testing.T) {
	provider, request, fake := nativeCodexFixture(t)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(codexInvocation(t, fake).args, " ")
	for _, want := range []string{"--ask-for-approval never", "--sandbox workspace-write", "exec", "--ignore-user-config", "-c sandbox_workspace_write.network_access=false", "-c project_doc_max_bytes=0"} {
		if !strings.Contains(args, want) {
			t.Errorf("runtime-owned sandbox constraint %q was not requested: %s", want, args)
		}
	}
	for _, forbidden := range []string{"danger-full-access", "--dangerously-bypass-approvals-and-sandbox", "--search", "--profile"} {
		if strings.Contains(args, forbidden) {
			t.Errorf("provider relaxed the sandbox with %q: %s", forbidden, args)
		}
	}
}

func TestNativeCodexFailsClosedWithoutProvenSandboxCapability(t *testing.T) {
	for name, degrade := range map[string]func(*fakeCommandExecutor){
		"cli absent":                func(f *fakeCommandExecutor) { f.found = false },
		"sandbox flag unadvertised": func(f *fakeCommandExecutor) { f.codexHelp = "Options:\n  -C, --cd <DIR>\n" },
		"approval flag unadvertised": func(f *fakeCommandExecutor) {
			f.codexHelp = strings.ReplaceAll(codexCapableHelp, "--ask-for-approval", "--approve-for-me")
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, request, fake := nativeCodexFixture(t)
			degrade(fake)
			if _, err := provider.Execute(context.Background(), request); !errors.Is(err, ErrSandboxUnavailable) {
				t.Fatalf("want fail closed sandbox error, got %v", err)
			}
			if strings.Contains(recordedText(fake), "--sandbox") {
				t.Fatalf("provider executed without a proven sandbox: %#v", fake.calls)
			}
		})
	}
}

func TestNativeCodexReceivesNoAmbientHostEnvironment(t *testing.T) {
	t.Setenv("ZENCHRON_AMBIENT_SECRET", "fixture-ambient-secret-9c3")
	provider, request, fake := nativeCodexFixture(t)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	text := recordedText(fake)
	for _, forbidden := range []string{"ZENCHRON_AMBIENT_SECRET", "fixture-ambient-secret-9c3"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("ambient host environment reached the provider: %q in %s", forbidden, text)
		}
	}
	// The allowlist is constructed from scratch, so it stays small.
	for _, call := range fake.calls {
		if len(call.env) > 3 {
			t.Fatalf("environment is not an explicit allowlist: %#v", call.env)
		}
	}
}

func TestNativeCodexForwardsNoPublicationOrCloudCredentials(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "github_pat_fixture9c3")
	t.Setenv("SSH_AUTH_SOCK", "/fixture/ssh-agent-9c3.sock")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "aws-fixture-secret-9c3")
	provider, request, fake := nativeCodexFixture(t)
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	text := recordedText(fake)
	for _, forbidden := range []string{"GITHUB_TOKEN", "github_pat_fixture9c3", "SSH_AUTH_SOCK", "ssh-agent-9c3", "AWS_SECRET_ACCESS_KEY", "aws-fixture-secret-9c3"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("publication or cloud credential reached the provider: %q in %s", forbidden, text)
		}
	}
}

func TestNativeCodexDoesNotPromoteCandidateInstructions(t *testing.T) {
	provider, request, fake := nativeCodexFixture(t)
	agents := filepath.Join(request.CandidateDir, "AGENTS.md")
	candidateOwned := []byte("candidate-controlled-instruction-9c3\n")
	if err := os.WriteFile(agents, candidateOwned, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(recordedText(fake), "candidate-controlled-instruction-9c3") {
		t.Fatal("candidate-modified AGENTS.md promoted itself into trusted instructions")
	}
	args := strings.Join(codexInvocation(t, fake).args, " ")
	if !strings.Contains(args, "-c project_doc_max_bytes=0") {
		t.Fatalf("workspace instruction files were not refused: %s", args)
	}
	if !strings.Contains(args, request.TrustedInstructions) {
		t.Fatalf("pinned trusted instructions did not reach the provider: %s", args)
	}
	after, err := os.ReadFile(agents)
	if err != nil || string(after) != string(candidateOwned) {
		t.Fatalf("candidate AGENTS.md was not left byte-identical: %v %q", err, after)
	}
}

func TestNativeCodexAuthenticationStaysControlPlane(t *testing.T) {
	provider, request, fake := nativeCodexFixture(t)
	if err := os.WriteFile(filepath.Join(provider.CodexHome, "auth.json"), []byte(`{"token":"provider-credential-9c3"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Execute(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	call := codexInvocation(t, fake)
	for _, want := range []string{"CODEX_HOME=" + provider.CodexHome, "HOME=" + provider.CodexHome} {
		if !strings.Contains(strings.Join(call.env, " "), want) {
			t.Fatalf("provider auth path %q missing: %#v", want, call.env)
		}
	}
	// Auth is a runtime-owned path, so the credential itself is never anywhere
	// a candidate command could read it out of the environment or the arguments.
	if strings.Contains(recordedText(fake), "provider-credential-9c3") {
		t.Fatal("provider credential became candidate-visible configuration")
	}
	for _, call := range fake.calls {
		for _, entry := range call.env {
			if strings.HasPrefix(entry, "OPENAI_API_KEY=") || strings.HasPrefix(entry, "CODEX_API_KEY=") {
				t.Fatalf("raw provider token injected into the environment: %#v", call.env)
			}
		}
	}
	// A configuration that is not a runtime-owned directory is refused outright.
	inline := provider
	inline.CodexHome = "sk-inline-credential"
	if _, err := inline.Execute(context.Background(), request); err == nil {
		t.Fatal("inline provider credential was accepted as a Codex home")
	}
}

func TestNativeCodexRetriesOnlyRecognizedTransientCapacity(t *testing.T) {
	for _, tc := range []struct {
		diagnostic string
		want       FailureClass
	}{
		{"selected model is at capacity github_pat_fixturesecret", FailureTransientProvider},
		{"permission denied", FailureUnknown},
	} {
		provider, request, fake := nativeCodexFixture(t)
		fake.err = errors.New("codex exited non-zero")
		fake.outputs = []CommandOutput{{Stdout: []byte(tc.diagnostic), ExitCode: 1}}
		result, err := provider.Execute(context.Background(), request)
		if err == nil {
			t.Fatalf("provider failure was not surfaced for %q", tc.diagnostic)
		}
		if result.Outcome != OperationFailed || result.Failure == nil || result.Failure.Classification != tc.want {
			t.Fatalf("failure classification for %q = %#v, want %q", tc.diagnostic, result.Failure, tc.want)
		}
		if len(result.Artifacts) != 2 {
			t.Fatalf("failed run lost its diagnostic artifacts: %#v", result.Artifacts)
		}
		sanitized, err := os.ReadFile(result.Artifacts[1].Path)
		if err != nil || strings.Contains(string(sanitized), "github_pat_fixturesecret") {
			t.Fatalf("sanitized candidate retained fixture secret: %v %q", err, sanitized)
		}
	}
}
