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
