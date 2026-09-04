package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// attemptRecordingProvider reports the identity the RUNTIME handed it, so a
// test can compare what the provider was told against what the scheduler
// durably recorded. It never manufactures an attempt of its own - that is the
// behaviour being removed.
type attemptRecordingProvider struct {
	seen  []ExecutionAttemptRef
	stops []ProviderStop
}

func (p *attemptRecordingProvider) Isolation() ProviderIsolation {
	return ProviderIsolation{
		FilesystemRead: IsolationProven, FilesystemWrite: IsolationProven,
		NetworkDenied: IsolationProven, CredentialScope: IsolationProven,
	}
}

func (p *attemptRecordingProvider) Execute(_ context.Context, request ExecutionRequest) (ExecutionResult, error) {
	index := len(p.seen)
	p.seen = append(p.seen, request.AttemptRef())
	stop := StopCompleted
	if index < len(p.stops) {
		stop = p.stops[index]
	}
	if stop == StopCompleted {
		return ExecutionResult{ProviderID: "attempt-recorder", Attempt: request.Attempt, Outcome: Succeeded}, nil
	}
	return ExecutionResult{ProviderID: "attempt-recorder", Attempt: request.Attempt, Outcome: OperationFailed},
		&ProviderStopError{Reason: stop, Detail: "reasoning iterations exceeded 16"}
}

// TestTheRuntimeHandsTheProviderItsRealSchedulerAttempt is acceptance C and the
// production half of H: the attempt a provider is told is the attempt the
// scheduler durably recorded, on the first invocation and on every retry.
func TestTheRuntimeHandsTheProviderItsRealSchedulerAttempt(t *testing.T) {
	fixture := newPhase8Fixture(t)
	provider := &attemptRecordingProvider{stops: []ProviderStop{StopIterationBudget, StopIterationBudget}}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	for pass := 0; pass < 8; pass++ {
		fixture.reconcile(runID)
		if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
			break
		}
	}
	if len(provider.seen) < 2 {
		t.Fatalf("expected at least two invocations to compare, got %d", len(provider.seen))
	}
	// Retries of one binding must count 1, 2, 3 - never a provider-local 1, 1, 1.
	byBinding := map[string][]int{}
	for _, ref := range provider.seen {
		if ref.RunID != runID {
			t.Fatalf("a provider was told the wrong run: %q", ref.RunID)
		}
		if ref.OperationID == "" || ref.Attempt < 1 {
			t.Fatalf("the runtime handed the provider an incomplete identity: %#v", ref)
		}
		byBinding[ref.OperationID] = append(byBinding[ref.OperationID], ref.Attempt)
	}
	for operation, attempts := range byBinding {
		for i, attempt := range attempts {
			if attempt != i+1 {
				t.Fatalf("operation %s saw attempts %v, want 1..n in order", operation, attempts)
			}
		}
		// And the durable operation agrees with what the provider was told.
		durable, ok := fixture.state(runID).snapshot.Operations[operation]
		if !ok {
			for _, op := range fixture.state(runID).snapshot.Operations {
				if op.ID == operation {
					durable, ok = op, true
				}
			}
		}
		if ok && durable.Attempt != attempts[len(attempts)-1] {
			t.Fatalf("scheduler recorded attempt %d, provider was told %d", durable.Attempt, attempts[len(attempts)-1])
		}
	}
}

// TestEveryAttemptIsRedactedIndependently is acceptance E. Splitting one
// transcript per run into one per attempt must not turn one redaction pass
// into a shared one, and no credential may reach an artifact PATH either.
func TestEveryAttemptIsRedactedIndependently(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	operation := executionOperationID("run-e2e", "continuation|abc")
	secret := "ghp_" + strings.Repeat("A", 36)

	var sanitized []string
	for attempt := 1; attempt <= 2; attempt++ {
		artifacts, err := store.StoreExecutionAttemptTranscript("openai-responses",
			attemptRef("run-e2e", operation, attempt), []byte("authorization: bearer "+secret+"\n"), nil)
		if err != nil {
			t.Fatal(err)
		}
		path := sanitizedPath(t, artifacts)
		sanitized = append(sanitized, path)
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), secret) {
			t.Fatalf("attempt %d's sanitized transcript kept the credential", attempt)
		}
		if !strings.Contains(string(body), "[REDACTED]") {
			t.Fatalf("attempt %d was not redacted at all: %q", attempt, body)
		}
		for _, artifact := range artifacts {
			if strings.Contains(artifact.Path, secret) {
				t.Fatalf("a credential reached an artifact path: %s", artifact.Path)
			}
		}
	}
	if sanitized[0] == sanitized[1] {
		t.Fatal("both attempts shared one sanitized transcript, so one redaction covered two invocations")
	}
}

// TestDurableStateExplainsEveryTranscript is acceptance F: from one artifact
// reference, durable state names the run, the operation and the attempt -
// without asking the provider anything.
func TestDurableStateExplainsEveryTranscript(t *testing.T) {
	fixture := newPhase8Fixture(t)
	provider := &attemptRecordingProvider{stops: []ProviderStop{StopIterationBudget}}
	fixture.deps.Provider = provider
	fixture.runtime = fixture.newRuntime(fixture.deps)
	runID := fixture.start()
	for pass := 0; pass < 8; pass++ {
		fixture.reconcile(runID)
		if terminalDisposition(fixture.state(runID).snapshot.Disposition) {
			break
		}
	}
	state := fixture.state(runID)
	// Every execution operation's durable record carries the attempt, and the
	// journal binds each event to the operation that produced it. That chain is
	// the association: artifact -> event.operation_id -> operation.Attempt -> run.
	found := 0
	for _, event := range state.events {
		if event.OperationID == "" {
			continue
		}
		operation, ok := operationByID(state, event.OperationID)
		if !ok || operation.Kind != OpExecutionInvoke {
			continue
		}
		if operation.Attempt < 1 {
			t.Fatalf("an execution operation has no durable attempt: %#v", operation)
		}
		if event.RunID != runID {
			t.Fatalf("an event names a different run: %q", event.RunID)
		}
		found++
	}
	if found == 0 {
		t.Fatal("no journalled event could be traced to an execution operation")
	}
}

func operationByID(state *runState, id string) (RunOperation, bool) {
	for _, operation := range state.snapshot.Operations {
		if operation.ID == id {
			return operation, true
		}
	}
	return RunOperation{}, false
}

// TestLegacyTranscriptNamingIsUnchanged is acceptance I. Assurance, semantic
// assurance and every historical artifact keep the naming and the overwrite
// behaviour they had, because renaming durable history is not a migration -
// it is a rewrite of evidence.
func TestLegacyTranscriptNamingIsUnchanged(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	first, err := store.StoreTranscript("assurance-run-legacy", []byte("first\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store.Root, "assurance-run-legacy.raw.log")
	if got := rawPath(t, first); got != want {
		t.Fatalf("legacy transcript path changed: got %s want %s", got, want)
	}
	// The legacy path is deliberately still last-write-wins: a per-run
	// assurance transcript is not an execution attempt and nothing in #55 gives
	// it a new identity.
	second, err := store.StoreTranscript("assurance-run-legacy", []byte("second\n"), nil)
	if err != nil {
		t.Fatalf("the legacy path became create-once: %v", err)
	}
	body, err := os.ReadFile(rawPath(t, second))
	if err != nil || string(body) != "second\n" {
		t.Fatalf("legacy overwrite semantics changed: %v %q", err, body)
	}
}

// TestRetentionStillOwnsPerAttemptTranscripts is acceptance J. GC proves
// ownership from the JOURNAL - a raw artifact this run recorded - so the nested
// per-attempt layout is governed by exactly the same retention law, and an
// unjournalled file at the same depth is still refused.
func TestRetentionStillOwnsPerAttemptTranscripts(t *testing.T) {
	stateDir := t.TempDir()
	store := ArtifactStore{Root: filepath.Join(stateDir, "artifacts")}
	operation := executionOperationID("run-gc", "continuation|abc")
	artifacts, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-gc", operation, 2), []byte("bounded\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	raw := rawPath(t, artifacts)

	snapshot := RunSnapshot{
		EngineeringRun: EngineeringRun{ID: "run-gc", Disposition: Failed},
		Artifacts:      artifacts,
	}
	if !rawArtifactOf(snapshot, raw) {
		t.Fatal("GC cannot prove ownership of a journalled per-attempt transcript")
	}
	// The sanitized half is deliberately retained, exactly as before.
	if rawArtifactOf(snapshot, sanitizedPath(t, artifacts)) {
		t.Fatal("GC treated a sanitized per-attempt transcript as collectable raw output")
	}
	// A file the journal never recorded stays unprovable, however plausible its
	// path looks.
	unrecorded := filepath.Join(filepath.Dir(raw), "attempt-99.raw.log")
	if err := os.WriteFile(unrecorded, []byte("not journalled\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if rawArtifactOf(snapshot, unrecorded) {
		t.Fatal("GC proved ownership of a transcript no journal event records")
	}
}

// TestAttemptTranscriptsAreStableUnderRepetition runs the create-once path
// enough times to expose an accidental truncate-on-open, which a single write
// would hide.
func TestAttemptTranscriptsAreStableUnderRepetition(t *testing.T) {
	store := ArtifactStore{Root: t.TempDir()}
	operation := executionOperationID("run-stable", "initial|1|base")
	body := []byte("one invocation\n")
	first, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-stable", operation, 1), body, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := rawPath(t, first)
	for i := 0; i < 25; i++ {
		if _, err := store.StoreExecutionAttemptTranscript("openai-responses", attemptRef("run-stable", operation, 1), body, nil); err != nil {
			t.Fatalf("repetition %d was refused: %v", i, err)
		}
		got, err := os.ReadFile(path)
		if err != nil || string(got) != string(body) {
			t.Fatalf("repetition %d damaged the transcript: %v %q", i, err, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != int64(len(body)) {
		t.Fatalf("transcript size drifted: %v %d", err, info.Size())
	}
	_ = time.Now
}
