package main

import (
	"bytes"
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
