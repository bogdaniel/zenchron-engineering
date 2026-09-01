package runtime

// These tests drive ToolSurface against a real temporary workspace and a fake
// CommandExecutor. No model is called and no container is started.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// surfaceWorkspace builds a candidate git workspace plus an out-of-workspace
// tree reachable only through a symlink, which is the escape a brokered read
// must refuse. It is self-contained so these tests do not depend on fixtures
// owned by other files.
func surfaceWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{candidate, outside} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "data.txt"), []byte("runtime-state-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(candidate, "hello.txt"), []byte("candidate-content-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(candidate, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "-A"}, {"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "base"}} {
		if out, err := exec.Command("git", append([]string{"-C", candidate}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return candidate
}

func toolSurfaceFixture(t *testing.T) (ToolSurface, *fakeCommandExecutor) {
	t.Helper()
	candidate := surfaceWorkspace(t)
	fake := &fakeCommandExecutor{found: true}
	broker := ToolBroker{CandidateDir: candidate, Sandbox: DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "surface-operation", StateDir: t.TempDir()}}
	return ToolSurface{Broker: broker}, fake
}

// createdContainerArgs returns the docker create arguments, which are the
// complete boundary a brokered command actually runs behind.
func createdContainerArgs(t *testing.T, fake *fakeCommandExecutor) []string {
	t.Helper()
	for _, call := range fake.calls {
		if call.name == "docker" && len(call.args) > 0 && call.args[0] == "create" {
			return call.args
		}
	}
	t.Fatalf("no brokered container was created: %#v", fake.calls)
	return nil
}

// TestToolSurfaceRoundTripsEveryBrokeredCapability proves each of the five M0
// tools reaches its ToolBroker method and returns a usable model-facing result.
func TestToolSurfaceRoundTripsEveryBrokeredCapability(t *testing.T) {
	surface, fake := toolSurfaceFixture(t)
	ctx := context.Background()

	if out, failed := surface.Invoke(ctx, ToolRepoRead, []byte(`{"path":"hello.txt"}`)); failed || out != "candidate-content-9c3\n" {
		t.Fatalf("repo.read round trip failed: %t %q", failed, out)
	}
	if out, failed := surface.Invoke(ctx, ToolRepoSearch, []byte(`{"pattern":"candidate-content-9c3","scope":[]}`)); failed || !strings.Contains(out, "hello.txt:1: candidate-content-9c3") {
		t.Fatalf("repo.search round trip failed: %t %q", failed, out)
	}
	patch := "diff --git a/added.txt b/added.txt\nnew file mode 100644\n--- /dev/null\n+++ b/added.txt\n@@ -0,0 +1 @@\n+added-9c3\n"
	if out, failed := surface.Invoke(ctx, ToolCandidateApplyPatch, jsonArgs(t, map[string]any{"patch": patch})); failed || out != "patch applied" {
		t.Fatalf("candidate.apply_patch round trip failed: %t %q", failed, out)
	}
	// The applied patch is now visible through the diff capability, which is
	// how the loop observes its own effect.
	if out, failed := surface.Invoke(ctx, ToolCandidateDiff, []byte(`{"paths":[]}`)); failed {
		t.Fatalf("candidate.diff round trip failed: %q", out)
	}
	if out, failed := surface.Invoke(ctx, ToolCandidateRun, []byte(`{"command":["go","test","./..."]}`)); failed || !strings.Contains(out, "exit=0") {
		t.Fatalf("candidate.run round trip failed: %t %q", failed, out)
	}
	args := createdContainerArgs(t, fake)
	if strings.Join(args, " ") == "" || args[len(args)-3] != "go" {
		t.Fatalf("candidate.run did not pass the argv through the sandbox: %#v", args)
	}
	// candidate.run is an argv through the sandbox, never a host shell.
	if !strings.Contains(strings.Join(args, " "), "--network none") {
		t.Fatalf("candidate.run was not network-denied: %#v", args)
	}
}

// TestToolSurfaceRefusesAnythingOutsideTheDeclaredSurface covers the three ways
// a request can be inadmissible: an unknown name, malformed JSON, and an
// argument field no tool declares. Each must come back as a normal tool result
// and must not execute a neighbouring capability instead.
func TestToolSurfaceRefusesAnythingOutsideTheDeclaredSurface(t *testing.T) {
	surface, fake := toolSurfaceFixture(t)
	ctx := context.Background()
	for name, args := range map[string]string{
		"shell":                  `{"command":["sh","-c","cat /etc/passwd"]}`,
		"bash":                   `{"command":["bash"]}`,
		"container.exec":         `{"command":["sh"]}`,
		"repo.read.unrestricted": `{"path":"hello.txt"}`,
		"":                       `{}`,
	} {
		out, failed := surface.Invoke(ctx, name, []byte(args))
		if !failed || !strings.Contains(out, "unknown tool") {
			t.Fatalf("unknown tool %q was not refused: %t %q", name, failed, out)
		}
	}
	for _, malformed := range []string{
		``,
		`{`,
		`{"path":"hello.txt"} {"path":"hello.txt"}`,
		`{"path":"hello.txt","follow_symlinks":true}`,
		`{"path":"hello.txt","sudo":true}`,
		`{"paths":["hello.txt"]}`,
		`{"path":123}`,
		`{"path":""}`,
	} {
		out, failed := surface.Invoke(ctx, ToolRepoRead, []byte(malformed))
		if !failed || !strings.HasPrefix(out, "tool error:") {
			t.Fatalf("repo.read accepted malformed arguments %q: %t %q", malformed, failed, out)
		}
		if strings.Contains(out, "candidate-content-9c3") {
			t.Fatalf("a refused repo.read still returned file content: %q", out)
		}
	}
	for _, malformed := range []string{`{"command":"sh -c whoami"}`, `{"command":[]}`, `{"command":["sh"],"privileged":true}`, `{"argv":["sh"]}`} {
		if out, failed := surface.Invoke(ctx, ToolCandidateRun, []byte(malformed)); !failed || !strings.HasPrefix(out, "tool error:") {
			t.Fatalf("candidate.run accepted malformed arguments %q: %t %q", malformed, failed, out)
		}
	}
	// Nothing above was allowed to execute: no container was ever created.
	for _, call := range fake.calls {
		if call.name == "docker" && len(call.args) > 0 && call.args[0] == "create" {
			t.Fatalf("a refused tool request still executed a command: %#v", call.args)
		}
	}
	// Path confinement still applies through the surface, not only the broker.
	for _, escape := range []string{"../outside/data.txt", "/etc/passwd", ".git/config", "escape/data.txt"} {
		if out, failed := surface.Invoke(ctx, ToolRepoRead, jsonArgs(t, map[string]any{"path": escape})); !failed {
			t.Fatalf("repo.read escaped the workspace via %q: %q", escape, out)
		}
	}
}

// TestToolSurfaceBoundsOneToolResult keeps a single capability from flooding
// the reasoning context.
func TestToolSurfaceBoundsOneToolResult(t *testing.T) {
	surface, _ := toolSurfaceFixture(t)
	surface.MaxResultBytes = 8
	out, failed := surface.Invoke(context.Background(), ToolRepoRead, []byte(`{"path":"hello.txt"}`))
	if failed || !strings.Contains(out, "[truncated by Zenchron") {
		t.Fatalf("an oversized tool result was not bounded: %t %q", failed, out)
	}
}

// TestToolSurfaceDeclaresExactlyTheM0Tools locks the advertised surface: an
// added capability has to be a deliberate edit here, not a silent widening.
func TestToolSurfaceDeclaresExactlyTheM0Tools(t *testing.T) {
	want := map[string]bool{ToolRepoRead: true, ToolRepoSearch: true, ToolCandidateDiff: true, ToolCandidateApplyPatch: true, ToolCandidateRun: true}
	definitions := toolDefinitions()
	if len(definitions) != len(want) {
		t.Fatalf("advertised tool surface changed size: %d", len(definitions))
	}
	for _, definition := range definitions {
		if !want[definition.Name] {
			t.Fatalf("undeclared tool advertised to the model: %q", definition.Name)
		}
		if definition.Type != "function" || !definition.Strict {
			t.Fatalf("tool %q is not a strict function tool: %#v", definition.Name, definition)
		}
		if definition.Parameters["additionalProperties"] != false {
			t.Fatalf("tool %q accepts unknown argument fields: %#v", definition.Name, definition.Parameters)
		}
		delete(want, definition.Name)
	}
	if len(want) != 0 {
		t.Fatalf("declared tools missing from the advertised surface: %v", want)
	}
}
