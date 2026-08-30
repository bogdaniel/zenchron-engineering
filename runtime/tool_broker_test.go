package runtime

// Every test here drives ToolBroker against a real temporary workspace and a
// fake CommandExecutor. No model is called and no container is started.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// toolBrokerFixture builds a candidate git workspace plus an out-of-workspace
// tree reachable only through a symlink, which is the escape a read or search
// must refuse.
func toolBrokerFixture(t *testing.T) (ToolBroker, string) {
	t.Helper()
	root := t.TempDir()
	candidate := filepath.Join(root, "candidate")
	outside := filepath.Join(root, "outside")
	for _, dir := range []string{candidate, outside, filepath.Join(candidate, "sub")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "data.txt"), []byte("runtime-state-9c3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"hello.txt":     "candidate-content-9c3\n",
		"deploy.env":    "TOKEN=fixture-9c3\n",
		"sub/notes.txt": "-----BEGIN PRIVATE KEY-----\nfixture-9c3\n",
	} {
		if err := os.WriteFile(filepath.Join(candidate, filepath.FromSlash(name)), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(candidate, "escape")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "-A"}, {"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-m", "base"}} {
		cmd := exec.Command("git", append([]string{"-C", candidate}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return ToolBroker{CandidateDir: candidate}, outside
}

func TestToolBrokerConfinesEveryPathCapabilityToTheCandidateWorkspace(t *testing.T) {
	broker, _ := toolBrokerFixture(t)
	for _, unsafe := range []string{
		"../outside/data.txt",
		"sub/../../outside/data.txt",
		"/etc/passwd",
		"escape/data.txt",
		"escape",
		".git/config",
		"sub\\..\\..\\outside",
	} {
		if data, err := broker.ReadFile(unsafe); err == nil {
			t.Fatalf("brokered read escaped the workspace via %q: %q", unsafe, data)
		}
		if hits, err := broker.Search("runtime-state-9c3", []string{unsafe}); err == nil {
			t.Fatalf("brokered search escaped the workspace via %q: %#v", unsafe, hits)
		}
		if diff, err := broker.Diff([]string{unsafe}); err == nil {
			t.Fatalf("brokered diff escaped the workspace via %q: %q", unsafe, diff)
		}
	}
	// Credential-shaped names and contents are refused even inside the
	// workspace, so a brokered read cannot surface them.
	for _, sensitive := range []string{"deploy.env", "sub/notes.txt"} {
		if data, err := broker.ReadFile(sensitive); err == nil {
			t.Fatalf("brokered read exposed credential-shaped path %q: %q", sensitive, data)
		}
	}
	if hits, err := broker.Search("fixture-9c3", nil); err != nil || len(hits) != 0 {
		t.Fatalf("brokered search exposed credential-shaped content: %v %#v", err, hits)
	}
	// The rule is confinement, not refusal: in-workspace paths still work.
	data, err := broker.ReadFile("hello.txt")
	if err != nil || string(data) != "candidate-content-9c3\n" {
		t.Fatalf("brokered read of an in-workspace file failed: %v %q", err, data)
	}
	hits, err := broker.Search("candidate-content-9c3", nil)
	if err != nil || len(hits) != 1 || hits[0].Path != "hello.txt" {
		t.Fatalf("brokered search of the workspace failed: %v %#v", err, hits)
	}
	// A whole-workspace search must not surface anything the symlink points at.
	escaped, err := broker.Search("runtime-state-9c3", nil)
	if err != nil || len(escaped) != 0 {
		t.Fatalf("brokered search followed a symlink out of the workspace: %v %#v", err, escaped)
	}
	if _, err := broker.Diff([]string{"hello.txt"}); err != nil {
		t.Fatalf("brokered diff of an in-workspace path failed: %v", err)
	}
}

func TestToolBrokerApplyPatchStaysInsideTheCandidateWorkspace(t *testing.T) {
	broker, outside := toolBrokerFixture(t)
	escape := "diff --git a/escape/pwned.txt b/escape/pwned.txt\nnew file mode 100644\n--- /dev/null\n+++ b/escape/pwned.txt\n@@ -0,0 +1 @@\n+pwned-9c3\n"
	if err := broker.ApplyPatch([]byte(escape)); err == nil {
		t.Fatal("brokered patch wrote through a symlink out of the candidate workspace")
	}
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Fatal("brokered patch created a file outside the candidate workspace")
	}
	traversal := "diff --git a/../pwned.txt b/../pwned.txt\nnew file mode 100644\n--- /dev/null\n+++ b/../pwned.txt\n@@ -0,0 +1 @@\n+pwned-9c3\n"
	if err := broker.ApplyPatch([]byte(traversal)); err == nil {
		t.Fatal("brokered patch accepted a traversal path")
	}
	// An in-workspace path git itself would happily patch is still refused when
	// it is credential-shaped, which is the broker's own validation gate.
	sensitive := "diff --git a/deploy.env b/deploy.env\n--- a/deploy.env\n+++ b/deploy.env\n@@ -1 +1 @@\n-TOKEN=fixture-9c3\n+TOKEN=pwned-9c3\n"
	if err := broker.ApplyPatch([]byte(sensitive)); err == nil {
		t.Fatal("brokered patch was applied to a credential-shaped path")
	}
	unchanged, err := os.ReadFile(filepath.Join(broker.CandidateDir, "deploy.env"))
	if err != nil || string(unchanged) != "TOKEN=fixture-9c3\n" {
		t.Fatalf("refused brokered patch still modified the workspace: %v %q", err, unchanged)
	}
	inside := "diff --git a/added.txt b/added.txt\nnew file mode 100644\n--- /dev/null\n+++ b/added.txt\n@@ -0,0 +1 @@\n+added-9c3\n"
	if err := broker.ApplyPatch([]byte(inside)); err != nil {
		t.Fatalf("brokered patch of an in-workspace path failed: %v", err)
	}
	added, err := os.ReadFile(filepath.Join(broker.CandidateDir, "added.txt"))
	if err != nil || string(added) != "added-9c3\n" {
		t.Fatalf("in-workspace patch was not applied: %v %q", err, added)
	}
}

// brokeredContainerArgs returns the docker create arguments, which are the
// complete boundary a brokered command actually runs behind.
func brokeredContainerArgs(t *testing.T, fake *fakeCommandExecutor) []string {
	t.Helper()
	for _, call := range fake.calls {
		if call.name == "docker" && len(call.args) > 0 && call.args[0] == "create" {
			return call.args
		}
	}
	t.Fatalf("no brokered container was created: %#v", fake.calls)
	return nil
}

func brokeredCommandFixture(t *testing.T) (ToolBroker, *fakeCommandExecutor) {
	t.Helper()
	broker, _ := toolBrokerFixture(t)
	fake := &fakeCommandExecutor{found: true}
	broker.Sandbox = DockerSandbox{Image: "sha256:image", Executor: fake, OperationID: "broker-operation", StateDir: t.TempDir()}
	return broker, fake
}

func TestBrokeredCommandCarriesNoProviderCredentialOrAmbientSecret(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "github_pat_broker9c3")
	t.Setenv("OPENAI_API_KEY", "sk-broker-provider-credential-9c3")
	t.Setenv("CODEX_HOME", "/fixture/codex-home-9c3")
	t.Setenv("SSH_AUTH_SOCK", "/fixture/ssh-agent-9c3.sock")
	t.Setenv("ZENCHRON_AMBIENT_SECRET", "fixture-ambient-secret-9c3")
	broker, fake := brokeredCommandFixture(t)
	if _, err := broker.RunCommand(context.Background(), []string{"sh", "-ec", "true"}); err != nil {
		t.Fatal(err)
	}
	var text []string
	for _, call := range fake.calls {
		text = append(text, strings.Join(call.args, " "), strings.Join(call.env, " "))
	}
	recorded := strings.Join(text, "\n")
	for _, forbidden := range []string{"GITHUB_TOKEN", "github_pat_broker9c3", "OPENAI_API_KEY", "sk-broker-provider-credential-9c3", "CODEX_HOME", "codex-home-9c3", "SSH_AUTH_SOCK", "ssh-agent-9c3", "ZENCHRON_AMBIENT_SECRET", "fixture-ambient-secret-9c3"} {
		if strings.Contains(recorded, forbidden) {
			t.Fatalf("provider credential or ambient secret reached a brokered command: %q in %s", forbidden, recorded)
		}
	}
	// The container environment is an explicit allowlist, not an inherited one.
	args := brokeredContainerArgs(t, fake)
	var env []string
	for i, arg := range args {
		if arg == "--env" && i+1 < len(args) {
			env = append(env, args[i+1])
		}
	}
	if len(env) != 2 || env[0] != "HOME=/home" || !strings.HasPrefix(env[1], "PATH=") {
		t.Fatalf("brokered command environment is not an explicit allowlist: %#v", env)
	}
}

func TestBrokeredCommandIsNetworkDisabledAndMountsOnlyTheCandidate(t *testing.T) {
	broker, fake := brokeredCommandFixture(t)
	if _, err := broker.RunCommand(context.Background(), []string{"sh", "-ec", "true"}); err != nil {
		t.Fatal(err)
	}
	args := brokeredContainerArgs(t, fake)
	var networks, mounts []string
	for i, arg := range args {
		if arg == "--network" && i+1 < len(args) {
			networks = append(networks, args[i+1])
		}
		if arg == "--mount" && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
	}
	if len(networks) != 1 || networks[0] != "none" {
		t.Fatalf("brokered command was not network-denied: %#v", networks)
	}
	// Runtime state, the controller checkout, and other runs are absent because
	// the candidate workspace is the only thing bound into the container.
	candidate, err := filepath.EvalSymlinks(broker.CandidateDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mounts) != 1 || mounts[0] != "type=bind,src="+candidate+",dst=/candidate" {
		t.Fatalf("brokered command received more than the candidate workspace: %#v", mounts)
	}
	if _, err := broker.RunCommand(context.Background(), nil); err == nil {
		t.Fatal("an empty brokered command was accepted")
	}
}
