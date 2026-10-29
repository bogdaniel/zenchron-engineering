package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
)

func TestStandaloneSettlesAndWrites(t *testing.T) {
	dir := t.TempDir()
	res, err := run(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if res.Termination.Outcome != api.OutcomeCompleted || res.Termination.Cause != api.CauseLoopCompleted {
		t.Fatalf("termination %+v", res.Termination)
	}
	if res.Usage.ToolCalls != 2 || res.Usage.Reported.Input != nil {
		t.Fatalf("usage %+v: want 2 tool calls and unknown reported input", res.Usage)
	}
	got, err := os.ReadFile(filepath.Join(dir, "summary.txt"))
	if err != nil || string(got) != "2 lines read\n" {
		t.Fatalf("summary.txt = %q, %v", got, err)
	}
}
