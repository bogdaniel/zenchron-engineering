package main

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

// TestInspectSelfReportsOnlyItsOwnProvenance covers defect Y's mechanism: the
// adopted builder must be able to ask a freshly built binary what it thinks it
// is WITHOUT an operator configuration, a Docker daemon or any credential.
// Requiring the full doctor is what turned a missing config into a skipped
// probe, and a skipped probe into an adopted artifact nobody had checked.
func TestInspectSelfReportsOnlyItsOwnProvenance(t *testing.T) {
	var out bytes.Buffer
	code, err := controllerInspectSelf(nil, &out)
	if err != nil {
		t.Fatalf("a binary could not report its own provenance: %v", err)
	}
	if code != runtime.ExitCompleted {
		t.Fatalf("exit = %d", code)
	}
	var reported runtime.ControllerBuild
	if err := json.Unmarshal(out.Bytes(), &reported); err != nil {
		t.Fatalf("inspect-self did not emit a controller provenance document: %s", out.String())
	}
	// The test binary is an ordinary unattested build, which is a truthful
	// answer and exactly what an unproven binary must say about itself.
	if reported.Kind != runtime.ControllerUnattested {
		t.Fatalf("kind = %q, want %q", reported.Kind, runtime.ControllerUnattested)
	}
	// An unattested build claims nothing, so it reports no digest either.
	// Measuring one would be manufacturing a provenance field for a binary
	// that deliberately has none.
	if reported.Attested() || reported.BinarySHA256 != "" {
		t.Fatalf("an unattested build reported a provenance claim: %+v", reported)
	}

	// An attested build reports the injected identity together with a digest
	// measured from the running executable - which is what the adopted builder
	// compares against what it asked for.
	attested, err := buildProvenance(runtime.ControllerAdopted, "main-abc12345",
		"abc1234500000000000000000000000000000000", "def4567800000000000000000000000000000000",
		func() (string, error) { return "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil })
	if err != nil {
		t.Fatal(err)
	}
	if attested.Kind != runtime.ControllerAdopted || attested.BinarySHA256 == "" {
		t.Fatalf("an attested build did not report a measured identity: %+v", attested)
	}
	// It reports provenance and nothing else: no configuration, no
	// credentials, no environment.
	for _, forbidden := range []string{"config", "credential", "token", "endpoint", "state_dir"} {
		if bytes.Contains(bytes.ToLower(out.Bytes()), []byte(forbidden)) {
			t.Fatalf("inspect-self reported %q, which is not its business: %s", forbidden, out.String())
		}
	}
}

// TestControllerCommandsRefuseArgumentsTheyDoNotOwn covers the CLI findings.
// A build command that silently accepts a run flag suggests it did something,
// and a blank flag value silently selects a default the operator did not ask
// for.
func TestControllerCommandsRefuseArgumentsTheyDoNotOwn(t *testing.T) {
	for name, args := range map[string][]string{
		"an autonomy run flag": {"--new-generation"},
		"a dry-run flag":       {"--dry-run"},
		"a watch flag":         {"--follow"},
		"an unknown flag":      {"--adopted"},
		"a bare positional":    {"main"},
		"a blank output":       {"--output", "   "},
		"a blank revision":     {"--revision", ""},
		"a blank repo":         {"--repo", " "},
		"a value-less output":  {"--output"},
		"a repeated flag":      {"--repo", "a/b", "--repo", "c/d"},
	} {
		t.Run("refuse "+name, func(t *testing.T) {
			if _, err := parseControllerFlags(args); err == nil {
				t.Fatalf("controller build-adopted accepted %v", args)
			}
		})
	}

	flags, err := parseControllerFlags([]string{"--repo", "acme/widgets", "--config", "/tmp/c.json",
		"--output", " /tmp/out ", "--revision", " abc123 "})
	if err != nil {
		t.Fatalf("the intended arguments were refused: %v", err)
	}
	if flags.Repo != "acme/widgets" || flags.Config != "/tmp/c.json" ||
		flags.Output != "/tmp/out" || flags.Revision != "abc123" {
		t.Fatalf("flags = %+v", flags)
	}

	// inspect-self has one optional flag and refuses anything else rather than
	// letting a typo look like it worked.
	var out bytes.Buffer
	if _, err := controllerInspectSelf([]string{"--verbose"}, &out); err == nil {
		t.Fatal("inspect-self accepted an unknown argument")
	}
	if _, err := controllerInspectSelf([]string{"--json", "--json"}, &out); err == nil {
		t.Fatal("inspect-self accepted a repeated flag")
	}
	if _, err := controllerInspectSelf([]string{"--json"}, &out); err != nil {
		t.Fatalf("inspect-self refused its own flag: %v", err)
	}
}

// TestUsageNamesTheControllerCommands: a command a user cannot discover is a
// command they will not use.
func TestUsageNamesTheControllerCommands(t *testing.T) {
	var out bytes.Buffer
	_, err := run([]string{"nonsense"}, nil, &out)
	if err == nil {
		t.Fatal("an unknown command was accepted")
	}
	for _, want := range []string{"controller inspect-self", "controller build-adopted"} {
		if !bytes.Contains([]byte(err.Error()), []byte(want)) {
			t.Fatalf("usage does not mention %q: %v", want, err)
		}
	}
}
