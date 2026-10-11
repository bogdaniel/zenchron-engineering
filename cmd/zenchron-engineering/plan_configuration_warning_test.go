package main

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestPlanConfigurationWarning(t *testing.T) {
	store, err := runtime.OpenSQLiteOperationStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	local := runtime.ConfigDigest{Global: strings.Repeat("a", 64), Repository: strings.Repeat("b", 64)}
	var out bytes.Buffer
	if err := warnPlanConfiguration(store, local, &out); err != nil || out.Len() != 0 {
		t.Fatalf("no authority: %q, %v", out.String(), err)
	}
	governing := local
	governing.Repository = strings.Repeat("c", 64)
	if _, err := store.ReadoptController(runtime.ControllerReadoption{ID: "warning-test", Reason: "test", Binding: runtime.ControllerBinding{Config: governing}, RecordedAt: time.Unix(1, 0)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := warnPlanConfiguration(store, governing, &out); err != nil || out.Len() != 0 {
		t.Fatalf("matching authority: %q, %v", out.String(), err)
	}
	if err := warnPlanConfiguration(store, local, &out); err != nil {
		t.Fatal(err)
	}
	want := "warning: repository configuration differs (CLI bbbbbbbbbbbb, governing controller cccccccccccc); the governing controller will hold this plan as configuration_changed: it was proposed under a different controller-effective configuration; it is not carried across that boundary. Run from the directory `serve` uses or restore the configuration.\n"
	if out.String() != want {
		t.Fatalf("warning = %q; want %q", out.String(), want)
	}
	authority, ok, err := store.CurrentControllerAuthority()
	if err != nil || !ok || authority.Binding.Config != governing {
		t.Fatalf("warning changed authority: %+v %v", authority, err)
	}
}

// Exercise the public command routing with real configuration and SQLite state.
// Neither a forge nor a provider may be contacted outside the test fixtures.
func TestPlanCommandsWarnAboutGoverningConfiguration(t *testing.T) {
	for _, verb := range []string{"issue", "approve", "revise", "reject"} {
		for _, member := range []string{"global", "repository", "identical", "no authority"} {
			for _, format := range []string{"json", "text"} {
				t.Run(verb+"/"+member+"/"+format, func(t *testing.T) {
					runPlanConfigurationWarningCase(t, verb, member, format, false)
				})
			}
		}
	}
}

// RESTORED DEFECT: local planning and approval previously proceeded silently
// when the CLI configuration differed from the recorded controller authority.
// A helper-only test would miss a removed call at either command boundary.
// Here the digests even share their displayed prefix: comparing shortened
// diagnostics instead of full digests would restore the same silent failure.
func TestPlanConfigurationWarningRestoredDefect(t *testing.T) {
	for _, verb := range []string{"issue", "approve", "revise"} {
		t.Run(verb, func(t *testing.T) {
			runPlanConfigurationWarningCase(t, verb, "global", "json", true)
		})
	}
}

func runPlanConfigurationWarningCase(t *testing.T, verb, member, format string, samePrefix bool) {
	t.Helper()
	dir, configPath := planWorkspace(t)
	t.Chdir(dir)
	overrides := planOverrides(t, 41)
	args := []string{"plan", verb}
	wantCode := runtime.ExitWaiting
	if verb == "issue" {
		args = append(args, "41", "--deterministic")
	} else {
		planID := proposePlan(t, configPath, 41)
		args = append(args, planID)
		if verb == "revise" {
			args = append(args, "--deterministic")
		} else {
			revision, digest, assignments := pendingDecision(t, configPath, planID, 41)
			args = append(args, "--revision", strconv.Itoa(revision), "--digest", digest)
			// Only approval binds an assignment set; rejection must omit it.
			if verb == "approve" {
				args = append(args, "--assignments", assignments)
			}
			wantCode = runtime.ExitCompleted
		}
	}
	args = append(args, "--config", configPath)
	if format == "text" {
		args = append(args, "--text")
	}

	composed, err := buildPlanComposition(autonomyFlags{Config: configPath}, overrides)
	if err != nil {
		t.Fatal(err)
	}
	// Record config X as governing while subsequent CLI compositions load Y.
	// The plan itself stays bound to Y so revising it is not a configuration hold.
	local := composed.built.config.Digest
	governing := local
	change := func(digest string) string {
		if samePrefix {
			if len(digest) <= 12 {
				t.Fatalf("expected full digest, got %q", digest)
			}
			last := "0"
			if strings.HasSuffix(digest, last) {
				last = "1"
			}
			return digest[:len(digest)-1] + last
		}
		changed := strings.Repeat("a", 64)
		if digest == changed {
			changed = strings.Repeat("b", 64)
		}
		return changed
	}
	switch member {
	case "global":
		governing.Global = change(local.Global)
	case "repository":
		governing.Repository = change(local.Repository)
	}
	if member != "no authority" {
		_, err = composed.built.store.ReadoptController(runtime.ControllerReadoption{
			ID: "cli-warning-test", Reason: "test governing config X",
			Binding: runtime.ControllerBinding{Config: governing}, RecordedAt: time.Unix(1, 0),
		}, nil)
		if err != nil {
			composed.release()
			t.Fatal(err)
		}
	}
	before, existed, err := composed.built.store.CurrentControllerAuthority()
	composed.release()
	if err != nil {
		t.Fatal(err)
	}

	// os.Stderr is process-global: these tests deliberately do not run in parallel.
	stderr, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	original := os.Stderr
	t.Cleanup(func() { os.Stderr = original })
	os.Stderr = stderr
	var stdout bytes.Buffer
	code, commandErr := autonomy(args, overrides, &stdout)
	os.Stderr = original
	warning, err := os.ReadFile(stderr.Name())
	if err != nil {
		t.Fatal(err)
	}
	if commandErr != nil || code != wantCode {
		t.Fatalf("%s: code=%d err=%v stdout=%s stderr=%s", verb, code, commandErr, stdout.String(), warning)
	}
	wantWarning := verb != "reject" && (member == "global" || member == "repository")
	if wantWarning {
		for _, want := range []string{"warning: " + member + " configuration differs", "CLI ", "governing controller ", "configuration_changed", "Run from the directory `serve` uses or restore the configuration."} {
			if !strings.Contains(string(warning), want) {
				t.Fatalf("stderr lacks %q: %q", want, warning)
			}
		}
		if strings.Count(string(warning), "warning:") != 1 {
			t.Fatalf("expected one warning: %q", warning)
		}
	} else if len(warning) != 0 {
		t.Fatalf("unexpected stderr: %q", warning)
	}
	if strings.Contains(stdout.String(), "warning:") {
		t.Fatalf("warning leaked into stdout: %s", stdout.String())
	}
	if format == "json" && !json.Valid(stdout.Bytes()) {
		t.Fatalf("invalid JSON stdout: %s", stdout.String())
	}
	if format == "text" && !strings.Contains(stdout.String(), "plan ") {
		t.Fatalf("missing text plan output: %s", stdout.String())
	}

	composed, err = buildPlanComposition(autonomyFlags{Config: configPath}, overrides)
	if err != nil {
		t.Fatal(err)
	}
	defer composed.release()
	after, exists, err := composed.built.store.CurrentControllerAuthority()
	if err != nil || exists != existed || !reflect.DeepEqual(before, after) {
		t.Fatalf("command changed governing authority: before=%+v after=%+v exists=%v err=%v", before, after, exists, err)
	}
}
