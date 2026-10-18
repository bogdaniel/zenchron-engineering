package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func benchClock(minutes *int) func() time.Time {
	base := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	return func() time.Time { return base.Add(time.Duration(*minutes) * time.Minute) }
}

func runBenchmark(t *testing.T, at *int, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	_, err := benchmarkCommand(args, benchClock(at), &out)
	return out.String(), err
}

func TestBenchmarkLogStartStopRecordSummaryAndEmit(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "activity.jsonl")
	at := 0
	common := []string{"--log", log, "--arm", "direct_agent", "--operator", "op-1"}

	if _, err := runBenchmark(t, &at, append([]string{"log", "start", "--kind", "task_preparation"}, common...)...); err != nil {
		t.Fatal(err)
	}
	at = 5
	if _, err := runBenchmark(t, &at, "log", "stop", "--log", log); err != nil {
		t.Fatal(err)
	}
	if _, err := runBenchmark(t, &at, append([]string{"log", "record", "--kind", "manual_relay", "--relay-kind", "finding",
		"--start", "2026-10-20T09:05:00Z", "--end", "2026-10-20T09:07:00Z"}, common...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runBenchmark(t, &at, append([]string{"log", "record", "--kind", "unattended_wait",
		"--start", "2026-10-20T09:07:00Z", "--end", "2026-10-20T09:37:00Z"}, common...)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runBenchmark(t, &at, append([]string{"log", "start", "--kind", "bogus"}, common...)...); err == nil {
		t.Fatal("an invalid kind must be refused by the CLI")
	}

	summary, err := runBenchmark(t, &at, "log", "summary", "--log", log)
	if err != nil {
		t.Fatalf("summary: %v\n%s", err, summary)
	}
	var parsed map[string]struct {
		ActiveHumanSeconds    float64 `json:"active_human_seconds"`
		UnattendedWaitSeconds float64 `json:"unattended_wait_seconds"`
	}
	if err := json.Unmarshal([]byte(summary), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed["direct_agent"]; got.ActiveHumanSeconds != 7*60 || got.UnattendedWaitSeconds != 30*60 {
		t.Fatalf("summary = %+v", got)
	}

	at = 120
	out := filepath.Join(dir, "out")
	emitted, err := runBenchmark(t, &at, "emit", "--log", log, "--arm", "direct_agent", "--case-id", "issue-344",
		"--repo", "o/r", "--base-sha", strings.Repeat("a", 40), "--outcome", "rejected", "--out", out)
	if err != nil {
		t.Fatalf("emit: %v\n%s", err, emitted)
	}
	if !strings.Contains(emitted, `"valid": true`) {
		t.Fatalf("a rejected outcome is still a valid measurement:\n%s", emitted)
	}
	if _, err := os.Stat(filepath.Join(out, "direct_agent", "2026-10-20-issue-344.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := runBenchmark(t, &at, "emit", "--log", log, "--arm", "direct_agent", "--case-id", "issue-344",
		"--repo", "o/r", "--base-sha", strings.Repeat("a", 40), "--outcome", "accepted", "--out", out); err == nil {
		t.Fatal("re-emitting over an existing result must be refused")
	}
}

func TestBenchmarkUsageErrors(t *testing.T) {
	at := 0
	for _, args := range [][]string{{}, {"log"}, {"log", "nope", "--log", "x"}, {"emit"}, {"wat"}} {
		if _, err := runBenchmark(t, &at, args...); err == nil {
			t.Fatalf("%v must be a usage error", args)
		}
	}
}
