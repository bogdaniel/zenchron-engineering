package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	common := []string{"PATH", "HOME", "GOENV", "GIT_DIR", "GOMODCACHE", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOFLAGS", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH"}
	want := map[string][]string{
		AgentKindCodexCLI:   append(slices.Clone(common), "CODEX_HOME"),
		AgentKindClaudeCode: append(slices.Clone(common), "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_PRINT_BG_WAIT_CEILING_MS", "BASH_DEFAULT_TIMEOUT_MS", "BASH_MAX_TIMEOUT_MS"),
	}
	hashed := []string{"PATH", "HOME", "GIT_DIR", "TMPDIR", "GOTMPDIR", "GOCACHE", "GOPATH", "GOMODCACHE", "CODEX_HOME", "CLAUDE_CONFIG_DIR"}
	// Assembled at run time so this source file is not itself credential
	// material to candidate admission.
	secret := "gh" + "p_" + strings.Repeat("0a", 20)
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
				t.Setenv("GOPROXY", "https://user:"+secret+"@proxy.example")
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
				// The allowlist and env() are maintained apart: a variable
				// env() starts passing must be recorded or deliberately
				// omitted, or this goes red.
				for name := range passed {
					if !slices.Contains(names, name) && !slices.Contains(providerEnvNotRecorded, name) {
						t.Errorf("%s is passed to the provider but not recorded", name)
					}
				}
				if _, ok := passed["USER"]; !ok {
					t.Fatal("the fixture no longer passes USER, so its omission is untested")
				}
				if !slices.Equal(names, want[kind]) {
					t.Fatalf("recorded names %v, want exactly %v", names, want[kind])
				}
				for _, e := range recorded {
					value, ok := passed[e.Name]
					if slices.Contains(hashed, e.Name) {
						switch {
						case e.Value != nil:
							t.Errorf("host path %s recorded literally", e.Name)
						case ok != (e.SHA256 != ""):
							t.Errorf("%s passed=%t but identity %q", e.Name, ok, e.SHA256)
						case ok && e.SHA256 != testEnvIdentity(e.Name, value):
							t.Errorf("%s identity %s is not the domain-separated SHA-256 of the passed value", e.Name, e.SHA256)
						}
						continue
					}
					switch {
					case e.SHA256 != "":
						t.Errorf("%s is not a host path but carries an identity", e.Name)
					case !ok && e.Value != nil:
						t.Errorf("%s was not passed but is recorded as %q", e.Name, *e.Value)
					case ok && e.Value == nil:
						t.Errorf("%s was passed as %q but is recorded as absent", e.Name, value)
					case ok && (*e.Value != sanitizedDetail(value) || e.Bounded != (*e.Value != value)):
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
				if scratch == (byName["GOCACHE"].SHA256 == "") {
					t.Errorf("GOCACHE presence %v does not follow the scratch grant %v", byName["GOCACHE"].SHA256 != "", scratch)
				}
				if v := byName["GOFLAGS"].Value; scratch != (v != nil) || v != nil && *v != "-mod=readonly" {
					t.Errorf("GOFLAGS recorded %v; the ambient -v must never appear", v)
				}
				if v := byName["GOPROXY"].Value; scratch != (v != nil) || v != nil && *v != "off" {
					t.Errorf("GOPROXY recorded %v; the ambient proxy must never appear", v)
				}
				if kind == AgentKindClaudeCode && byName["BASH_MAX_TIMEOUT_MS"].Value == nil {
					t.Error("the deadline-derived Claude Bash timeout is not recorded")
				}
				if byName["HOME"].SHA256 == "" || byName["PATH"].SHA256 == "" {
					t.Error("HOME and PATH were passed but carry no identity")
				}
				// NO RAW HOST PATH in the recorded environment: the only literal
				// path allowed is the null device GOENV names.
				environment, _ := json.Marshal(recorded)
				for _, path := range []string{provider.OperatorHome, request.ScratchDir, os.TempDir()} {
					if path != "" && strings.Contains(string(environment), path) {
						t.Errorf("raw path %s reached the recorded environment: %s", path, environment)
					}
				}
				for _, e := range recorded {
					if e.Value != nil && strings.Contains(*e.Value, "/") && *e.Value != os.DevNull {
						t.Errorf("%s recorded a literal path %q", e.Name, *e.Value)
					}
				}
				raw, _ := json.Marshal(newExecutionAttemptProvenance(request.OperationID, 1, *result.Invocation))
				if strings.Contains(string(raw), provider.OperatorHome) || strings.Contains(string(raw), `"USER"`) {
					t.Fatalf("a host path or USER reached provenance: %s", raw)
				}
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

// testEnvIdentity restates the construction independently of the producer:
// lowercase hex SHA-256 of "zenchron/provider-env/<NAME>/v1" NUL value.
func testEnvIdentity(name, value string) string {
	sum := sha256.Sum256([]byte("zenchron/provider-env/" + name + "/v1\x00" + value))
	return hex.EncodeToString(sum[:])
}

// Absent records nothing, empty records the identity of "", and the tag
// separates domains: the same path under HOME and CODEX_HOME, or bare, is
// three different identities.
func TestProviderEnvironmentHostPathIdentity(t *testing.T) {
	got := map[string]domain.EnvironmentEntry{}
	for _, e := range providerEnvironment([]string{"HOME=", "CODEX_HOME=/h"}, cliAgentSpecs[AgentKindCodexCLI]) {
		got[e.Name] = e
	}
	if p := got["PATH"]; p.SHA256 != "" || p.Value != nil {
		t.Errorf("an absent PATH recorded %+v", p)
	}
	if h := got["HOME"]; h.Value != nil || h.SHA256 != testEnvIdentity("HOME", "") {
		t.Errorf("an empty HOME recorded %+v, want the identity of \"\"", h)
	}
	plain := sha256.Sum256([]byte("/h"))
	if c := got["CODEX_HOME"].SHA256; c != testEnvIdentity("CODEX_HOME", "/h") || c == testEnvIdentity("HOME", "/h") || c == hex.EncodeToString(plain[:]) {
		t.Errorf("CODEX_HOME identity %s is not domain-separated", c)
	}
}

// The schema's hash-only name set is the runtime's, so neither can gain a
// host path the other records literally.
func TestSchemaEnvironmentNamesMatchTheRuntime(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "schemas", "planning-vocabulary.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Defs struct {
			Observation struct {
				Properties struct {
					Environment struct {
						Items struct {
							Properties struct {
								Name struct{ Enum []string }
							}
							If struct {
								Properties struct {
									Name struct{ Enum []string }
								}
							}
						}
					} `json:"provider_environment"`
				}
			} `json:"invocation_observation"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(body, &schema); err != nil {
		t.Fatal(err)
	}
	items := schema.Defs.Observation.Properties.Environment.Items
	var all, hashed []string
	for _, spec := range cliAgentSpecs {
		for _, name := range recordedEnvNames(spec) {
			if !slices.Contains(all, name) {
				all = append(all, name)
				if hashedEnvName(name) {
					hashed = append(hashed, name)
				}
			}
		}
	}
	for label, pair := range map[string][2][]string{
		"recorded names":  {items.Properties.Name.Enum, all},
		"hash-only names": {items.If.Properties.Name.Enum, hashed},
	} {
		inSchema, runtime := slices.Sorted(slices.Values(pair[0])), slices.Sorted(slices.Values(pair[1]))
		if !slices.Equal(inSchema, runtime) {
			t.Errorf("schema %s %v, runtime %v", label, inSchema, runtime)
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

// os/exec keeps the LAST duplicate, so provenance must too; a URL's userinfo
// is a credential even inside a non-secret control value.
func TestProviderEnvironmentLastWinsAndRedactsUserinfo(t *testing.T) {
	token := "tok" + strings.Repeat("9", 20)
	got := map[string]domain.EnvironmentEntry{}
	for _, e := range providerEnvironment([]string{
		"GOFLAGS=first", "GOFLAGS=last", "GOPROXY=https://user:" + token + "@proxy.example,direct",
	}, cliAgentSpecs[AgentKindCodexCLI]) {
		got[e.Name] = e
	}
	if v := got["GOFLAGS"].Value; v == nil || *v != "last" {
		t.Errorf("duplicate GOFLAGS recorded as %v, want the last one", v)
	}
	proxy := got["GOPROXY"]
	if proxy.Value == nil || strings.Contains(*proxy.Value, token) || strings.Contains(*proxy.Value, "user") || !proxy.Bounded {
		t.Errorf("GOPROXY userinfo not redacted: %+v", proxy)
	}
}

// The userinfo ends at the LAST @ before the first /: a raw @ inside the
// password must not leave its tail behind, and an @ in a path is not userinfo.
func TestURLUserinfoRedaction(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@ss@host/p":                          "https://[REDACTED]@host/p",
		"https://a:x@h1/p,https://b:y@h2|https://c:z@h3": "https://[REDACTED]@h1/p,https://[REDACTED]@h2|https://[REDACTED]@h3",
		"https://host/a@b":                               "https://host/a@b",
		"off":                                            "off",
	} {
		if got := urlUserinfo.ReplaceAllString(in, "${1}[REDACTED]@"); got != want {
			t.Errorf("%q redacted to %q, want %q", in, got, want)
		}
	}
}

// Every spec's allowlist fits the bound validation enforces at append.
func TestEverySpecProviderEnvironmentFitsTheBound(t *testing.T) {
	for kind, spec := range cliAgentSpecs {
		if n := len(providerEnvironment(nil, spec)); n > domain.MaxProviderEnvironment {
			t.Errorf("%s records %d environment names, above %d", kind, n, domain.MaxProviderEnvironment)
		}
		// Every name the spec can record, passed and absent, validates.
		var env []string
		for _, name := range recordedEnvNames(spec) {
			env = append(env, name+"=/x")
		}
		for _, passed := range [][]string{env, nil} {
			if err := validateInvocationObservation(domain.InvocationObservation{Executable: "x", ProviderEnvironment: providerEnvironment(passed, spec)}); err != nil {
				t.Errorf("%s: its own recorded environment is refused: %v", kind, err)
			}
		}
	}
}

// The append-time check refuses what the producer never writes.
func TestValidateRefusesMalformedProviderEnvironment(t *testing.T) {
	long, ok := strings.Repeat("x", 201), "v"
	many := make([]domain.EnvironmentEntry, domain.MaxProviderEnvironment+1)
	for i := range many {
		many[i] = domain.EnvironmentEntry{Name: "GOFLAGS"} // an allowlisted name, so only the cap refuses it
	}
	for name, env := range map[string][]domain.EnvironmentEntry{
		"bad identifier":   {{Name: "NOT A NAME"}},
		"case variant":     {{Name: "home"}},
		"unlisted name":    {{Name: "XDG_CONFIG_HOME"}},
		"credential name":  {{Name: "GITHUB_TOKEN"}},
		"too many entries": many,
		"oversized value":  {{Name: "GOFLAGS", Value: &long}},
		"literal HOME":     {{Name: "HOME", Value: &ok}},
		"value and hash":   {{Name: "GOFLAGS", Value: &ok, SHA256: testEnvIdentity("GOFLAGS", "v")}},
		"malformed hash":   {{Name: "PATH", SHA256: "not-a-digest"}},
		"literal GOCACHE":  {{Name: "GOCACHE", Value: &ok}},
		"hash on literal":  {{Name: "GOFLAGS", SHA256: testEnvIdentity("GOFLAGS", "v")}},
	} {
		if validateInvocationObservation(domain.InvocationObservation{Executable: "x", ProviderEnvironment: env}) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := validateInvocationObservation(domain.InvocationObservation{Executable: "x", ProviderEnvironment: []domain.EnvironmentEntry{{Name: "GOFLAGS", Value: &ok}, {Name: "GOCACHE"}}}); err != nil {
		t.Errorf("a well-formed environment was refused: %v", err)
	}
}
