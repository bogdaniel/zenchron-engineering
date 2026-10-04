package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// TestRecordedArgvNamesRuntimePathsByRole is the #464 privacy law: the argv
// the provider runs with keeps its real paths, and the argv provenance records
// names them only by role.
func TestRecordedArgvNamesRuntimePathsByRole(t *testing.T) {
	for kind, want := range map[string][]string{
		AgentKindCodexCLI:   {`sandbox_workspace_write.writable_roots=["$SCRATCH","$RESULT"]`, "--cd", "$CANDIDATE"},
		AgentKindClaudeCode: {"--add-dir", "$SCRATCH", "$RESULT"},
	} {
		t.Run(kind, func(t *testing.T) {
			provider, request, fake := agentFixture(t, kind)
			root := filepath.Dir(request.CandidateDir)
			state := filepath.Join(root, "state dir")
			provider.StateDir = state
			request.ScratchDir = filepath.Join(state, "runs", "run", "scratch")
			request.ReviewerResultPath = filepath.Join(state, "runs", "run", "result", "review.json")
			result, err := provider.Execute(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			// Exact elements, not a substring search: the prompt also names the
			// scratch path, and would satisfy a looser check.
			real := fake.execution(t).args
			roots, _ := json.Marshal([]string{request.ScratchDir, filepath.Dir(request.ReviewerResultPath)})
			wantReal := map[string][]string{
				AgentKindCodexCLI:   {"sandbox_workspace_write.writable_roots=" + string(roots), request.CandidateDir},
				AgentKindClaudeCode: {request.ScratchDir, filepath.Dir(request.ReviewerResultPath)},
			}[kind]
			for _, arg := range wantReal {
				if !hasArg(real, arg) {
					t.Fatalf("the provider no longer receives the real path %q: %q", arg, real)
				}
			}
			recorded := result.Invocation.Argv
			for _, arg := range want {
				if !hasArg(recorded, arg) {
					t.Fatalf("recorded argv lacks %q: %q", arg, recorded)
				}
			}
			resolved, _ := filepath.EvalSymlinks(root)
			joined := strings.Join(recorded, "\x00")
			for _, leak := range []string{root, resolved, os.TempDir(), "/private/", "/tmp/", "/Users/", "/home/", provider.OperatorHome} {
				if strings.Contains(joined, leak) {
					t.Fatalf("recorded argv leaks host path %q: %q", leak, recorded)
				}
			}
			if err := validateInvocationObservation(result.Invocation.InvocationObservation); err != nil {
				t.Fatalf("the recorded observation is not appendable: %v", err)
			}
		})
	}
}

// TestEveryPathFieldDeclaresItsRecordedForm sets every string on the
// invocation to a sentinel host path, so a path-bearing field added without a
// mapping in recorded() leaks the sentinel here and fails.
func TestEveryPathFieldDeclaresItsRecordedForm(t *testing.T) {
	const sentinel = "/Users/sentinel-464"
	var invocation cliInvocation
	fillStrings(reflect.ValueOf(&invocation).Elem(), sentinel+"/state/field")
	invocation.Prompt = "prompt"
	for kind, spec := range map[string]cliAgentSpec{"codex": codexSpec, "claude": claudeSpec, "gemini": geminiSpec, "qwen": qwenSpec} {
		builders := map[string]func(cliInvocation) []string{"ordinary": spec.Args}
		if spec.ReadOnly != nil {
			builders["read-only"] = spec.ReadOnly.Args
		}
		for mode, build := range builders {
			for _, stateDir := range []string{sentinel + "/state", ""} {
				recorded := recordedArgv(build, invocation, stateDir, spec.PromptArgFromEnd)
				if joined := strings.Join(recorded, " "); strings.Contains(joined, sentinel) {
					t.Fatalf("%s %s: a host path reached recorded argv: %s", kind, mode, joined)
				}
			}
		}
	}
}

func fillStrings(v reflect.Value, value string) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(value)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.String {
			v.Set(reflect.ValueOf([]string{value}))
		}
	case reflect.Struct:
		for n := range v.NumField() {
			if v.Field(n).CanSet() {
				fillStrings(v.Field(n), value)
			}
		}
	}
}

func TestLogicalPathMatchesWholeComponentsAndTheMostSpecificRole(t *testing.T) {
	roots := []pathRoot{
		{"/var/z/state/runs/r1/scratch", "$SCRATCH"}, {"/var/z/state/runs/r1/result", "$RESULT"},
		{"/var/z/candidate", "$CANDIDATE"}, {"/var/z/state", "$STATE"},
		{`C:\Users\alice\state`, "$STATE"},
	}
	for path, want := range map[string]string{
		"/var/z/state/runs/r1/scratch":       "$SCRATCH",
		"/var/z/state/runs/r1/scratch/go":    "$SCRATCH/go",
		"/var/z/state/runs/r1/result":        "$RESULT",
		"/var/z/state/runs/r1/other":         "$STATE/runs/r1/other",
		"/var/z/state/runs/r1/scratchy":      "$STATE/runs/r1/scratchy",
		"/var/z/candidate/./pkg/../main.go":  "$CANDIDATE/main.go",
		"/var/z/candidateX":                  hostPathMarker,
		"/var/z/stat":                        hostPathMarker,
		"/var/z":                             hostPathMarker,
		`c:\Users\alice\state\runs\r1`:       "$STATE/runs/r1",
		`C:/Users/alice/state`:               "$STATE",
		`C:\Users\alice\stateX`:              hostPathMarker,
		`D:\Users\alice\state`:               hostPathMarker,
		`\\fileserver\share\state`:           hostPathMarker,
		"/Users/alice/with space/and\"quote": hostPathMarker,
	} {
		if got := logicalPath(path, roots); got != want {
			t.Errorf("logicalPath(%q) = %q, want %q", path, got, want)
		}
	}
	if got := logicalPath("", roots); got != "" {
		t.Errorf("an absent path must stay absent so a spec's grant test is unchanged, got %q", got)
	}
}

// TestLogicalPathSeesThroughSymlinkedRoots is macOS's /tmp -> /private/tmp in
// both directions: whichever spelling the root and the path use, the role is
// recognized and neither host spelling is recorded.
func TestLogicalPathSeesThroughSymlinkedRoots(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.MkdirAll(filepath.Join(real, "scratch"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable:", err)
	}
	if got := logicalPath(filepath.Join(real, "scratch"), []pathRoot{{link, "$STATE"}}); got != "$STATE/scratch" {
		t.Errorf("a real path under a symlinked root: %q", got)
	}
	if got := logicalPath(filepath.Join(link, "scratch"), []pathRoot{{real, "$STATE"}}); got != "$STATE/scratch" {
		t.Errorf("a symlinked path under a real root: %q", got)
	}
}

// TestAHostPathUnderNoRuntimeRootIsMarkedNotRecorded is the unknown-root
// decision: the value is replaced by <host-path>, never persisted raw, and
// the invocation itself is not refused for it.
func TestAHostPathUnderNoRuntimeRootIsMarkedNotRecorded(t *testing.T) {
	invocation := cliInvocation{
		Agent: ResolvedAgent{Model: "/opt/models/local.gguf"}, Prompt: "p",
		ScratchDir: "/srv/state/scratch", RequiredTools: []string{"go", `C:\tools\gofmt.exe`},
	}
	recorded := recordedArgv(claudeSpec.Args, invocation, "/srv/state", claudeSpec.PromptArgFromEnd)
	joined := strings.Join(recorded, " ")
	for _, want := range []string{"--model " + hostPathMarker, "Bash(go *) Bash(" + hostPathMarker + " *)", "--add-dir $SCRATCH"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("recorded argv lacks %q: %s", want, joined)
		}
	}
	if strings.Contains(joined, "/opt/") || strings.Contains(joined, `C:\`) {
		t.Fatalf("an unknown host path was recorded raw: %s", joined)
	}
	// A spec that spelled an absolute path literally is caught too.
	literal := recordedArgv(func(cliInvocation) []string { return []string{"--config", "/etc/x.toml", "p"} }, invocation, "", 0)
	if !reflect.DeepEqual(literal, []string{"--config", hostPathMarker, "[prompt]"}) {
		t.Fatalf("a literal host path was recorded raw: %q", literal)
	}
}

func TestTheJournalRefusesARawHostPathInRecordedArgv(t *testing.T) {
	for _, arg := range []string{"/Users/alice/scratch", `C:\Users\alice`, `\\server\share`} {
		if validateInvocationObservation(domain.InvocationObservation{Argv: []string{"--add-dir", arg}}) == nil {
			t.Errorf("a raw host path %q was appendable", arg)
		}
	}
	ok := domain.InvocationObservation{Argv: []string{"--add-dir", "$SCRATCH", "--allowedTools", "Write Bash(go *)", hostPathMarker, "[prompt]"}}
	if err := validateInvocationObservation(ok); err != nil {
		t.Fatalf("a logical argv was refused: %v", err)
	}
}

// TestAModelPathUnderTheStateDirIsRecordedByRole pins the per-field mappings
// in recorded(): the bare post-pass alone would only yield <host-path> here.
func TestAModelPathUnderTheStateDirIsRecordedByRole(t *testing.T) {
	const state = "/Users/sentinel-464/state"
	for name, tc := range map[string]struct {
		invocation cliInvocation
		want       string
	}{
		"profile preference": {cliInvocation{ModelPreference: state + "/models/pref.gguf", Agent: ResolvedAgent{Model: "default"}}, "$STATE/models/pref.gguf"},
		"agent default":      {cliInvocation{Agent: ResolvedAgent{Model: state + "/models/agent.gguf"}}, "$STATE/models/agent.gguf"},
		"leading space":      {cliInvocation{Agent: ResolvedAgent{Model: " " + state + "/models/agent.gguf"}}, "$STATE/models/agent.gguf"},
	} {
		tc.invocation.Prompt = "p"
		recorded := recordedArgv(claudeSpec.Args, tc.invocation, state, claudeSpec.PromptArgFromEnd)
		if !strings.Contains(strings.Join(recorded, " "), "--model "+tc.want) {
			t.Errorf("%s: recorded argv lacks --model %s: %q", name, tc.want, recorded)
		}
	}
}

func TestALeadingSpaceDoesNotHideAHostPath(t *testing.T) {
	invocation := cliInvocation{Agent: ResolvedAgent{Model: " /opt/models/x.gguf"}, Prompt: "p"}
	recorded := recordedArgv(claudeSpec.Args, invocation, "", claudeSpec.PromptArgFromEnd)
	if joined := strings.Join(recorded, " "); strings.Contains(joined, "/opt/") {
		t.Fatalf("a space-prefixed host path was recorded raw: %q", recorded)
	}
	literal := recordedArgv(func(cliInvocation) []string { return []string{"\t/etc/x", "p"} }, invocation, "", 0)
	if literal[0] != hostPathMarker {
		t.Fatalf("the post-pass missed a space-prefixed host path: %q", literal)
	}
	if validateInvocationObservation(domain.InvocationObservation{Argv: []string{" /Users/alice"}}) == nil {
		t.Fatal("the journal accepted a space-prefixed host path")
	}
}

// TestAFilesystemRootIsNeverARoleRoot: a StateDir misconfigured as `/` or a
// drive root must not turn every host path into `$STATE/<host path>`.
func TestAFilesystemRootIsNeverARoleRoot(t *testing.T) {
	for root, path := range map[string]string{
		"/":                   "/Users/alice/x",
		`C:\`:                 `C:\Users\alice\x`,
		"c:/":                 "c:/Users/alice/x",
		`\\fileserver\share`:  `\\fileserver\share\alice\x`,
		"//fileserver/share/": "//fileserver/share/alice/x",
	} {
		if got := logicalPath(path, []pathRoot{{root, "$STATE"}}); got != hostPathMarker {
			t.Errorf("root %q claimed %q as %q", root, path, got)
		}
	}
	// A real directory on a share is still a role root.
	if got := logicalPath(`\\fileserver\share\state\runs`, []pathRoot{{`\\fileserver\share\state`, "$STATE"}}); got != "$STATE/runs" {
		t.Errorf("a share-hosted state dir lost its role: %q", got)
	}
}
