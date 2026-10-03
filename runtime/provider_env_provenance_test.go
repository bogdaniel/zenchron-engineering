package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// #391: invocation provenance records the provider-control environment the
// process ACTUALLY received, restricted to a non-secret allowlist, with an
// unpassed variable recorded as absent rather than empty.
func TestInvocationProvenanceRecordsTheEffectiveProviderEnvironment(t *testing.T) {
	common := []string{"PATH", "HOME", "USER", "GOENV", "GIT_DIR", "GOMODCACHE", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOFLAGS", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH"}
	want := map[string][]string{
		AgentKindCodexCLI:   append(slices.Clone(common), "CODEX_HOME"),
		AgentKindClaudeCode: append(slices.Clone(common), "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS", "BASH_DEFAULT_TIMEOUT_MS", "BASH_MAX_TIMEOUT_MS"),
	}
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	for _, kind := range []string{AgentKindCodexCLI, AgentKindClaudeCode} {
		for _, scratch := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "/without-scratch", true: "/with-scratch"}[scratch], func(t *testing.T) {
				// Ambient state the provider must NOT be described by: secrets,
				// a secret inside a forwarded allowlisted name, and Go
				// variables the runtime overrides or does not pass at all.
				t.Setenv("GITHUB_TOKEN", secret)
				t.Setenv("ANTHROPIC_API_KEY", secret)
				t.Setenv("GOFLAGS_TOKEN", secret)
				t.Setenv("USER", secret)
				t.Setenv("GOENV", "/operator/go/env")
				t.Setenv("GOCACHE", "/operator/cache")
				t.Setenv("GOFLAGS", "-v")
				provider, request, fake := agentFixture(t, kind)
				if scratch {
					provider.Toolchain = ToolchainConfig{RequiredTools: []string{"go"}}
					request.ScratchDir = filepath.Join(t.TempDir(), "scratch")
				}
				deadline := time.Now().Add(time.Hour)
				request.Deadline = &deadline
				result, err := provider.Execute(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				recorded := result.Invocation.ProviderEnvironment
				passed := map[string]string{}
				for _, entry := range fake.calls[len(fake.calls)-1].env {
					name, value, _ := strings.Cut(entry, "=")
					passed[name] = value
				}
				var names []string
				for _, e := range recorded {
					names = append(names, e.Name)
				}
				if !slices.Equal(names, want[kind]) {
					t.Fatalf("recorded names %v, want exactly %v", names, want[kind])
				}
				for _, e := range recorded {
					value, ok := passed[e.Name]
					switch {
					case !ok && e.Value != nil:
						t.Errorf("%s was not passed but is recorded as %q", e.Name, *e.Value)
					case ok && e.Value == nil:
						t.Errorf("%s was passed as %q but is recorded as absent", e.Name, value)
					case ok && (*e.Value != sanitizedDetail(value) || e.Bounded != (*e.Value != value)):
						// A long PATH is cut to the field bound, visibly.
						t.Errorf("%s recorded %q (bounded=%t), passed %q", e.Name, *e.Value, e.Bounded, value)
					}
				}
				byName := map[string]domain.EnvironmentEntry{}
				for _, e := range recorded {
					byName[e.Name] = e
				}
				if v := byName["GOENV"].Value; v == nil || *v != os.DevNull {
					t.Errorf("GOENV not recorded as %s: %v", os.DevNull, v)
				}
				if scratch == (byName["GOCACHE"].Value == nil) {
					t.Errorf("GOCACHE presence %v does not follow the scratch grant %v", byName["GOCACHE"].Value != nil, scratch)
				}
				if v := byName["GOFLAGS"].Value; scratch != (v != nil) || v != nil && *v != "-mod=readonly" {
					t.Errorf("GOFLAGS recorded %v; the ambient -v must never appear", v)
				}
				if kind == AgentKindClaudeCode && byName["BASH_MAX_TIMEOUT_MS"].Value == nil {
					t.Error("the deadline-derived Claude Bash timeout is not recorded")
				}
				if u := byName["USER"]; u.Value == nil || !u.Bounded {
					t.Errorf("a forwarded USER carrying a token was not recorded redacted: %+v", u)
				}
				raw, _ := json.Marshal(newExecutionAttemptProvenance(request.OperationID, 1, *result.Invocation))
				if strings.Contains(string(raw), secret) || strings.Contains(string(raw), "GITHUB_TOKEN") || strings.Contains(string(raw), "API_KEY") {
					t.Fatalf("a secret reached provenance: %s", raw)
				}
				if !appendable(EventExecutionAttemptProvenance, newExecutionAttemptProvenance(request.OperationID, 1, *result.Invocation)) {
					t.Fatal("the provenance with its environment is not appendable")
				}
			})
		}
	}
}

// The allowlist is the filter, not the trust that env() built the slice from
// scratch: a credential or unlisted name in the passed environment is never
// recorded, and set-to-empty is not absent.
func TestProviderEnvironmentRecordsOnlyAllowlistedNames(t *testing.T) {
	got := providerEnvironment([]string{
		"GITHUB_TOKEN=ghp_x", "OPENAI_API_KEY=sk-x", "SSH_AUTH_SOCK=/s", "UNLISTED=1", "GOFLAGS=", "GOENV=" + os.DevNull,
	}, cliAgentSpecs[AgentKindClaudeCode])
	for _, e := range got {
		if !slices.Contains(providerControlNames, e.Name) && e.Name != "CLAUDE_CONFIG_DIR" && !slices.Contains(cliAgentSpecs[AgentKindClaudeCode].ControlEnv, e.Name) {
			t.Errorf("recorded a non-allowlisted name %s", e.Name)
		}
		switch e.Name {
		case "GOFLAGS":
			if e.Value == nil || *e.Value != "" {
				t.Errorf("set-to-empty GOFLAGS recorded as %v", e.Value)
			}
		case "PATH":
			if e.Value != nil {
				t.Errorf("an unpassed PATH recorded as %q", *e.Value)
			}
		}
	}
}
