package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/engine"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/providers/scripted"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// The persisted admission partitions (docs/spec/execution-v0.2.md §8).
const (
	claimPartition     = "agentkernel.admission_claims"
	admissionPartition = "agentkernel.admissions"
)

func openRecords(t *testing.T, root string) *storage.FileRecords {
	t.Helper()
	rec, err := storage.OpenFileRecords(root)
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func withAdmissions(r *storage.FileRecords) option {
	return func(cfg *engine.Config) { cfg.Admissions = r }
}

// breakRoot makes root unusable (a file where the directory was) and
// returns the function that restores it.
func breakRoot(t *testing.T, root string) func() {
	t.Helper()
	aside := root + ".aside"
	if err := os.Rename(root, aside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(root); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(aside, root); err != nil {
			t.Fatal(err)
		}
	}
}

// settledAttempts reads the execution's admission record from disk.
func settledAttempts(t *testing.T, rec *storage.FileRecords) []string {
	t.Helper()
	data, err := rec.Get(context.Background(), admissionPartition, "exec-1")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Attempts []struct {
			AttemptID string `json:"attempt_id"`
		} `json:"attempts"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, a := range r.Attempts {
		ids = append(ids, a.AttemptID)
	}
	return ids
}

// TestTwoEnginesOneRootAdmitExactlyOne is A05 across processes: two Engines
// over two separately opened FileRecords handles on one root (what two
// processes have) race to admit attempts of one execution. Exactly one is
// admitted; every loser is refused before reserving budget or calling its
// provider, and only the winner's consumption is recorded. Run with -race.
func TestTwoEnginesOneRootAdmitExactlyOne(t *testing.T) {
	root := t.TempDir()
	gates := []*gate{{release: make(chan struct{})}, {release: make(chan struct{})}}
	var engines []*fixture
	for _, g := range gates {
		engines = append(engines, newFixture(t, nil, withProvider(t, g), withAdmissions(openRecords(t, root))))
	}
	const n = 8
	results := make(chan api.ExecutionResult, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			res, _ := engines[i%2].engine.Execute(context.Background(), attempt(fmt.Sprintf("att-%d", i)))
			results <- res
		})
	}
	var admitted []api.ExecutionResult
	for range n - 1 {
		select {
		case res := <-results:
			want(t, res, api.OutcomeBlocked, api.CauseInvalidRequest)
			if u := res.Usage; u.ProviderCalls != 0 || u.Iterations != 0 || u.ToolCalls != 0 {
				t.Fatalf("refused contender reserved budget: %+v", u)
			}
		case <-time.After(5 * time.Second):
			for _, g := range gates {
				close(g.release)
			}
			t.Fatal("more than one contender was admitted")
		}
	}
	for _, g := range gates {
		close(g.release)
	}
	wg.Wait()
	admitted = append(admitted, <-results)
	want(t, admitted[0], api.OutcomeCompleted, api.CauseLoopCompleted)
	if calls := gates[0].calls + gates[1].calls; calls != 1 {
		t.Fatalf("providers called %d times, want 1", calls)
	}
	if ids := settledAttempts(t, openRecords(t, root)); len(ids) != 1 || ids[0] != admitted[0].AttemptID {
		t.Fatalf("recorded attempts %v, want only %s", ids, admitted[0].AttemptID)
	}
}

// TestCrashedAttemptBlocksTheNext: a claim left by an attempt that never
// settled (the process died) refuses every later attempt, from any Engine.
// Nothing expires it: its consumption is unknown.
func TestCrashedAttemptBlocksTheNext(t *testing.T) {
	root := t.TempDir()
	if err := openRecords(t, root).PutIfAbsent(context.Background(), claimPartition, "exec-1", []byte("att-1")); err != nil {
		t.Fatal(err)
	}
	f := newFixture(t, []scripted.Step{end("never")}, withAdmissions(openRecords(t, root)))
	wantRefused(t, f.next(t, attempt("att-2")), `prior attempt "att-1" in flight or unsettled`)
	wantRefused(t, f.next(t, attempt("att-1")), "already admitted")
	if len(f.provider.Requests()) != 0 {
		t.Fatal("an attempt after a crashed one reached the provider")
	}
}

// TestSettlementReleasesTheClaim: a settled attempt releases its claim, and
// a fresh Engine on the same root admits the next attempt starting from the
// recorded consumption.
func TestSettlementReleasesTheClaim(t *testing.T) {
	root := t.TempDir()
	first := newFixture(t, []scripted.Step{toolUse(readCall("a", "a.txt")), end("one")}, withAdmissions(openRecords(t, root)))
	req := attempt("att-1")
	req.Budget.MaxIterations = 3
	want(t, first.next(t, req), api.OutcomeCompleted, api.CauseLoopCompleted)
	if _, err := openRecords(t, root).Get(context.Background(), claimPartition, "exec-1"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("claim after settlement: %v, want released", err)
	}
	second := newFixture(t, []scripted.Step{toolUse(readCall("b", "a.txt")), end("never")}, withAdmissions(openRecords(t, root)))
	req = attempt("att-2")
	req.Budget.MaxIterations = 3
	res := second.next(t, req)
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionIterations || len(second.provider.Requests()) != 1 {
		t.Fatalf("dimension %q after %d calls: the restarted attempt renewed iterations",
			res.Termination.Dimension, len(second.provider.Requests()))
	}
}

// TestAdmissionWriteFailureRefusesBeforeSideEffects: if the claim cannot be
// written, nothing runs.
func TestAdmissionWriteFailureRefusesBeforeSideEffects(t *testing.T) {
	root := filepath.Join(t.TempDir(), "admissions")
	f := newFixture(t, []scripted.Step{end("never")}, withAdmissions(openRecords(t, root)))
	defer breakRoot(t, root)()
	res := f.next(t, request())
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if len(f.provider.Requests()) != 0 || !strings.Contains(res.Termination.Detail, "nothing ran") {
		t.Fatalf("%d provider calls, detail %q", len(f.provider.Requests()), res.Termination.Detail)
	}
}

// TestUnsettledAttemptBlocksTheNext: an attempt whose consumption could not
// be recorded keeps its claim, so every later attempt is refused.
func TestUnsettledAttemptBlocksTheNext(t *testing.T) {
	root := filepath.Join(t.TempDir(), "admissions")
	var restore func()
	step := end("one")
	step.Before = func() { restore = breakRoot(t, root) }
	f := newFixture(t, []scripted.Step{step, end("two")}, withAdmissions(openRecords(t, root)))
	res := f.next(t, attempt("att-1"))
	restore()
	want(t, res, api.OutcomeIncomplete, api.CauseRecordingFailed)
	if !strings.Contains(res.Termination.Detail, "completed/loop_completed") {
		t.Fatalf("observed outcome lost: %q", res.Termination.Detail)
	}
	wantRefused(t, f.next(t, attempt("att-2")), "unsettled")
	if len(f.provider.Requests()) != 1 {
		t.Fatal("attempt after an unsettled one reached the provider")
	}
}

// putRecord writes a raw admission record for exec-1, as an earlier kernel
// would have: att-1 settled with 2 iterations consumed.
func putRecord(t *testing.T, root string, version string, budget api.Budget) []byte {
	t.Helper()
	rec := map[string]any{
		"budget":   budget,
		"attempts": []map[string]any{{"attempt_id": "att-1", "consumed": map[string]int64{"iterations": 2}}},
	}
	if version != "" {
		rec["version"] = version
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	return putRaw(t, root, data)
}

// putRaw stores data as exec-1's admission record.
func putRaw(t *testing.T, root string, data []byte) []byte {
	t.Helper()
	if err := openRecords(t, root).Put(context.Background(), admissionPartition, "exec-1", data); err != nil {
		t.Fatal(err)
	}
	return data
}

// TestUnreadableAdmissionVersionFailsClosed: a present legacy unversioned
// (v0.1) record, whatever it contains (no attempts, an empty object), or one
// of an unknown version, is never reinterpreted. A new attempt
// is refused before any side effect and before any claim, the record is left
// as it was, and no claim exists afterwards, so it repeats as the same
// refusal.
func TestUnreadableAdmissionVersionFailsClosed(t *testing.T) {
	const legacy = "legacy unversioned (v0.1) admission state; explicit recovery or migration is required"
	for name, tc := range map[string]struct {
		put    func(t *testing.T, root string) []byte
		reason string
	}{
		"legacy_v0.1": {func(t *testing.T, root string) []byte { return putRecord(t, root, "", attempt("att-1").Budget) }, legacy},
		"legacy_zero_attempts": {func(t *testing.T, root string) []byte {
			return putRaw(t, root, []byte(`{"budget":{},"attempts":[]}`))
		}, legacy},
		"legacy_empty_object": {func(t *testing.T, root string) []byte { return putRaw(t, root, []byte(`{}`)) }, legacy},
		"unknown": {func(t *testing.T, root string) []byte {
			return putRecord(t, root, "agentkernel.admission/v9", attempt("att-1").Budget)
		}, `unknown version "agentkernel.admission/v9"`},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			before := tc.put(t, root)
			records := openRecords(t, root)
			f := newFixture(t, []scripted.Step{end("never")}, withAdmissions(records))
			for range 2 {
				res := f.next(t, attempt("att-2"))
				wantRefused(t, res, tc.reason)
				if u := res.Usage; u.ProviderCalls != 0 || u.Iterations != 0 || u.ToolCalls != 0 || len(f.provider.Requests()) != 0 {
					t.Fatalf("refused attempt had side effects: usage %+v, %d provider calls", u, len(f.provider.Requests()))
				}
			}
			if _, err := openRecords(t, root).Get(context.Background(), claimPartition, "exec-1"); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("claim after refusal: %v, want none (read through a second store handle)", err)
			}
			after, err := records.Get(context.Background(), admissionPartition, "exec-1")
			if err != nil || string(after) != string(before) {
				t.Fatalf("record changed by a refused attempt: %s (%v)", after, err)
			}
		})
	}
}

// TestVersionedAdmissionRecordAdmits: a v0.2 record admits a new attempt with
// a later deadline, which starts from the recorded consumption; the kernel
// writes the version on the record it extends.
func TestVersionedAdmissionRecordAdmits(t *testing.T) {
	root := t.TempDir()
	first := attempt("att-1")
	first.Budget.MaxIterations = 3
	putRecord(t, root, "agentkernel.admission/v0.2", first.Budget)
	records := openRecords(t, root)
	f := newFixture(t, []scripted.Step{toolUse(readCall("b", "a.txt")), end("never")}, withAdmissions(records))
	second := attempt("att-2")
	second.Budget.MaxIterations = 3
	second.Budget.Deadline = first.Budget.Deadline.Add(time.Hour)
	res := f.next(t, second)
	want(t, res, api.OutcomeExhausted, api.CauseBudgetExhausted)
	if res.Termination.Dimension != api.DimensionIterations || len(f.provider.Requests()) != 1 {
		t.Fatalf("dimension %q after %d calls: want admitted with 2 of 3 iterations already spent",
			res.Termination.Dimension, len(f.provider.Requests()))
	}
	data, err := records.Get(context.Background(), admissionPartition, "exec-1")
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &rec); err != nil || rec.Version != "agentkernel.admission/v0.2" {
		t.Fatalf("record version %q (%v), want agentkernel.admission/v0.2", rec.Version, err)
	}
	if ids := settledAttempts(t, records); len(ids) != 2 || ids[1] != "att-2" {
		t.Fatalf("recorded attempts %v, want [att-1 att-2]", ids)
	}
}
