package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"reflect"
	"testing"

	"github.com/bogdaniel/zenchron-engineering/runtime"
)

func TestSuccessorStateFormatCommandNeedsNoConfiguration(t *testing.T) {
	var out bytes.Buffer
	code, err := run([]string{"controller", "inspect-state-format"}, nil, &out)
	if err != nil || code != runtime.ExitCompleted {
		t.Fatal(code, err)
	}
	var got runtime.ControllerStateFormat
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	expected, err := runtime.CurrentControllerStateFormat()
	if err != nil || got != expected {
		t.Fatal(got, expected, err)
	}
}

func TestSuccessorServeArguments(t *testing.T) {
	plain, source, err := successorServeArgs([]string{"--config", "operator.json", "--follow-main", ".", "--repo", "acme/work"})
	if err != nil || source == "" || !reflect.DeepEqual(plain, []string{"--config", "operator.json", "--repo", "acme/work"}) {
		t.Fatal(plain, source, err)
	}
	for _, args := range [][]string{{"--follow-main"}, {"--follow-main", ""}, {"--follow-main", "--config"}, {"--follow-main", ".", "--follow-main", "."}} {
		if _, _, err := successorServeArgs(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
}

func TestSuccessorReadinessRequiresChildPIDAndExactBuild(t *testing.T) {
	expected := runtime.ControllerBuild{Kind: runtime.ControllerAdopted, SourceRevision: "expected", BinarySHA256: "digest"}
	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"none", "pid", "revision", "digest", "kind"} {
		t.Run(change, func(t *testing.T) {
			child := &serveChild{cmd: &exec.Cmd{Process: process}, done: make(chan struct{})}
			build, pid := expected, os.Getpid()
			switch change {
			case "pid":
				pid++
			case "revision":
				build.SourceRevision = "other"
			case "digest":
				build.BinarySHA256 = "other"
			case "kind":
				build.Kind = runtime.ControllerPreAdoptionBuild
			}
			payload, err := json.Marshal(map[string]any{"build": build, "pid": pid})
			if err != nil {
				t.Fatal(err)
			}
			err = child.readyWithProbe(context.Background(), expected, func() (runtime.ControlResponse, error) {
				return runtime.ControlResponse{OK: true, Payload: payload}, nil
			})
			if (err == nil) != (change == "none") {
				t.Fatalf("readiness for %s: %v", change, err)
			}
		})
	}
}

func TestSuccessorExitedChildCannotProveReadiness(t *testing.T) {
	child := &serveChild{done: make(chan struct{})}
	close(child.done)
	if err := child.readyWithProbe(context.Background(), runtime.ControllerBuild{}, func() (runtime.ControlResponse, error) {
		t.Fatal("exited child was probed")
		return runtime.ControlResponse{}, nil
	}); err == nil {
		t.Fatal("exited child accepted")
	}
}
