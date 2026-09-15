package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func TestReviewerResultTrailingBytes(t *testing.T) {
	for _, suffix := range []string{"", " \t\r\n", "}", "]", "\x00", "garbage", " {}", "\u00a0"} {
		t.Run(suffix, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "result.json")
			if err := os.WriteFile(path, []byte(`{"schema_version":"0.1","verdict":"accepted"}`+suffix), 0600); err != nil {
				t.Fatal(err)
			}
			_, err := ReadReviewerResult(path)
			valid := suffix == "" || suffix == " \t\r\n"
			if (err == nil) != valid {
				t.Fatalf("suffix %q: error = %v, want valid=%v", suffix, err, valid)
			}
		})
	}
}

func TestLegacyProviderReportsToolchain(t *testing.T) {
	for _, tc := range []ToolchainConfig{{}, {Path: []string{t.TempDir()}}, {Path: []string{t.TempDir()}, RequiredTools: []string{"missing-tool"}}} {
		in := DoctorInput{Toolchain: tc}
		want := doctorWorkerToolchain(in)
		count := 0
		for _, check := range doctorAgents(in) {
			if check.ID == "provider.toolchain" {
				count++
				if check != want {
					t.Fatalf("got %+v, want %+v", check, want)
				}
			}
		}
		if count != 1 {
			t.Fatalf("got %d toolchain checks", count)
		}
	}
}

func TestConfigRejectsBlankToolchainPathEntries(t *testing.T) {
	for _, entry := range []string{"", " ", "\t\r\n"} {
		dir := t.TempDir()
		var config map[string]any
		if err := json.Unmarshal([]byte(operatorConfigJSON(dir)), &config); err != nil {
			t.Fatal(err)
		}
		config["toolchain"] = map[string]any{"path": []string{dir, entry}}
		data, err := json.Marshal(config)
		if err != nil {
			t.Fatal(err)
		}
		path := writeFile(t, filepath.Join(dir, "config.json"), string(data))
		_, _, err = LoadOperatorConfig(path)
		ce := requireConfigError(t, err)
		if !strings.Contains(ce.Detail, "toolchain.path[1]") {
			t.Fatalf("wrong refusal: %v", err)
		}
	}
}

func TestLocalFetchSourceOptions(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want string
	}{
		{[]string{"fetch", "--no-tags", "/repo", "ref"}, "/repo"},
		{[]string{"fetch", "--depth", "1", "/repo", "ref"}, "/repo"},
		{[]string{"fetch", "--depth=1", "/repo"}, "/repo"},
		{[]string{"fetch", "--shallow-exclude", "old", "--filter=blob:none", "/repo"}, "/repo"},
		{[]string{"fetch", "--", "/repo"}, "/repo"},
		{[]string{"fetch", "--depth", "1", "https://example.invalid/repo"}, "https://example.invalid/repo"},
		{[]string{"fetch", "--unknown", "/repo", "https://example.invalid/repo"}, ""},
		{[]string{"fetch", "--all", "/repo"}, ""},
		{[]string{"fetch", "--depth"}, ""},
		{[]string{"fetch", "--depth", "1"}, ""},
		{[]string{"fetch", "--no-tags=value", "/repo"}, ""},
		{nil, ""},
	} {
		got, err := localTransportSource(tt.args, "fetch")
		if tt.want == "" {
			if err == nil {
				t.Fatalf("%v: expected refusal, got %q", tt.args, got)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Fatalf("%v: got %q, %v; want %q", tt.args, got, err, tt.want)
		}
	}
}

func TestBrokeredToolUsesNativeLookup(t *testing.T) {
	dir := t.TempDir()
	ambient := t.TempDir()
	name := "hardening-tool"
	filename := name
	if goruntime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE;.CMD")
		filename += ".exe"
	}
	if err := os.WriteFile(filepath.Join(ambient, filename), []byte("tool"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", ambient)
	p := CLIAgentProvider{Toolchain: ToolchainConfig{Path: []string{dir}}}
	if p.resolvesTool(name) {
		t.Fatal("resolved tool from ambient PATH")
	}
	if err := os.WriteFile(filepath.Join(dir, filename), []byte("tool"), 0700); err != nil {
		t.Fatal(err)
	}
	if !p.resolvesTool(name) {
		t.Fatal("did not resolve native executable on declared path")
	}
	if !p.resolvesTool(filename) {
		t.Fatal("did not resolve explicit executable name")
	}
}

func TestLocalFetchDepthDoesNotHideNetworkSource(t *testing.T) {
	runner := RepositoryGitRunner{Local: controlPolicy()}
	for _, source := range []string{"https://example.invalid/repo", "ssh://git@example.invalid/repo"} {
		if _, err := runner.transportFor([]string{"fetch", "--depth", "1", source, "ref"}); err == nil {
			t.Fatalf("network source %q passed local transport admission", source)
		}
	}
	source := t.TempDir()
	transport, err := runner.transportFor([]string{"fetch", "--depth", "1", source, "ref"})
	if err != nil || transport != transportFile {
		t.Fatalf("local source refused: %v, %v", transport, err)
	}
}

func TestBrokeredToolRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "tool"), 0700); err != nil {
		t.Fatal(err)
	}
	p := CLIAgentProvider{Toolchain: ToolchainConfig{Path: []string{dir}}}
	if p.resolvesTool("tool") {
		t.Fatal("directory resolved as executable")
	}
}

func TestBrokeredUnixToolFollowsExecutableSymlink(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("Unix executable mode semantics")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("tool"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "tool")); err != nil {
		t.Fatal(err)
	}
	p := CLIAgentProvider{Toolchain: ToolchainConfig{Path: []string{dir}}}
	if !p.resolvesTool("tool") {
		t.Fatal("executable symlink did not resolve")
	}
	if err := os.Chmod(target, 0600); err != nil {
		t.Fatal(err)
	}
	if p.resolvesTool("tool") {
		t.Fatal("symlink to non-executable file resolved")
	}
}
