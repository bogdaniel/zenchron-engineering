package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// goEnvWritingExecutor is a fake provider that does what run-2217ae2b's worker
// did (#430): it runs a REAL `go env -w` with exactly the environment the
// adapter handed it, and records the GOENV that child received.
type goEnvWritingExecutor struct {
	*fakeAgentExecutor
	goBinary    string
	sawGOENV    string
	writeOutput string
}

func (g *goEnvWritingExecutor) Run(ctx context.Context, name string, args []string, dir string, env []string, grace time.Duration) (CommandOutput, error) {
	write := exec.Command(g.goBinary, "env", "-w", "GOCACHE=/x/zenchron-escape")
	write.Env = env
	out, werr := write.CombinedOutput()
	g.writeOutput = fmt.Sprint(string(out), werr)
	// What the child process sees: exec keeps the last duplicate.
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GOENV="); ok {
			g.sawGOENV = value
		}
	}
	return g.fakeAgentExecutor.Run(ctx, name, args, dir, env, grace)
}

// TestProviderGoEnvWriteCannotReachOperatorConfig is the #430 regression: a
// worker's `go env -w` must never persist into the operator's Go env file,
// whether or not the attempt has a validation scratch, and an inherited GOENV
// naming the operator's file must not win.
func TestProviderGoEnvWriteCannotReachOperatorConfig(t *testing.T) {
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH")
	}
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode} {
		for _, scratch := range []bool{false, true} {
			name := kind + "/without-scratch"
			if scratch {
				name = kind + "/with-scratch"
			}
			t.Run(name, func(t *testing.T) {
				provider, request, fake := agentFixture(t, kind)
				operatorHome := provider.OperatorHome
				t.Setenv("HOME", operatorHome)
				t.Setenv("XDG_CONFIG_HOME", filepath.Join(operatorHome, ".config"))
				operatorFile := filepath.Join(operatorHome, "operator-go-env")
				const original = "GOFLAGS=-v\n"
				if err := os.WriteFile(operatorFile, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GOENV", operatorFile)
				// Go telemetry writes into HOME from a lingering child; turned
				// off here only so TempDir cleanup is not racing it.
				for _, dir := range []string{filepath.Join(operatorHome, "Library", "Application Support"), filepath.Join(operatorHome, ".config")} {
					mode := filepath.Join(dir, "go", "telemetry", "mode")
					if err := os.MkdirAll(filepath.Dir(mode), 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(mode, []byte("off"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if scratch {
					provider.Toolchain = ToolchainConfig{RequiredTools: []string{"go"}}
					request.ScratchDir = filepath.Join(t.TempDir(), "scratch")
				}
				writer := &goEnvWritingExecutor{fakeAgentExecutor: fake, goBinary: goBinary}
				provider.Executor = writer
				if _, err := provider.Execute(context.Background(), request); err != nil {
					t.Fatal(err)
				}
				if got, _ := os.ReadFile(operatorFile); string(got) != original {
					t.Errorf("operator GOENV file changed: %q", got)
				}
				for _, defaultFile := range []string{
					filepath.Join(operatorHome, "Library", "Application Support", "go", "env"),
					filepath.Join(operatorHome, ".config", "go", "env"),
				} {
					if _, err := os.Stat(defaultFile); err == nil {
						t.Errorf("worker wrote the operator's default Go env file %s (go env -w said %q)", defaultFile, writer.writeOutput)
					}
				}
				if writer.sawGOENV != os.DevNull {
					t.Errorf("worker child received GOENV=%q, want %s", writer.sawGOENV, os.DevNull)
				}
			})
		}
	}
}
