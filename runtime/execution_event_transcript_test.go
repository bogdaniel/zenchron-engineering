package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The method set PR #529's agentkernel adapter consumes as TranscriptWriter.
var _ interface {
	WriteEvent(context.Context, ExecutionAttemptRef, []byte) error
} = (*ExecutionEventTranscript)(nil)

const eventTranscriptProvider = "agentkernel"

var eventTranscriptAttempt = ExecutionAttemptRef{RunID: "run-1", OperationID: "op:1#a", Attempt: 2}

func openEventTranscript(t *testing.T, root string) *ExecutionEventTranscript {
	t.Helper()
	w, err := ArtifactStore{Root: root}.OpenExecutionEventTranscript(eventTranscriptProvider, eventTranscriptAttempt)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return w
}

func eventTranscriptPaths(t *testing.T, root string) (staging, raw, sanitized string) {
	t.Helper()
	prefix, err := attemptTranscriptPrefix(eventTranscriptProvider, eventTranscriptAttempt)
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(root, prefix)
	return base + eventTranscriptStagingSuffix, base + ".raw.log", base + ".sanitized-candidate.log"
}

func writeEvents(t *testing.T, w *ExecutionEventTranscript, events ...string) {
	t.Helper()
	for _, event := range events {
		if err := w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte(event)); err != nil {
			t.Fatalf("write %s: %v", event, err)
		}
	}
}

func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s must not exist, stat err = %v", path, err)
		}
	}
}

func readTranscriptFile(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestEventTranscriptFinalizePublishesOrdinaryTranscript(t *testing.T) {
	root := t.TempDir()
	events := []string{`{"kind":"started"}`, `{"kind":"tool","out":"ok"}`, `{"kind":"settled"}`}
	w := openEventTranscript(t, root)
	writeEvents(t, w, events...)
	staging, raw, sanitized := eventTranscriptPaths(t, root)
	assertAbsent(t, raw, sanitized)

	artifacts, err := w.Finalize()
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts = %+v", artifacts)
	}
	assertAbsent(t, staging)

	// The same content through the existing one-shot writer is the format.
	other := t.TempDir()
	if _, err := (ArtifactStore{Root: other}).StoreExecutionAttemptTranscript(eventTranscriptProvider, eventTranscriptAttempt, []byte(strings.Join(events, "\n")+"\n"), nil); err != nil {
		t.Fatal(err)
	}
	_, wantRaw, wantSanitized := eventTranscriptPaths(t, other)
	if got, want := readTranscriptFile(t, raw), readTranscriptFile(t, wantRaw); !bytes.Equal(got, want) {
		t.Fatalf("raw = %q, want %q", got, want)
	}
	if got, want := readTranscriptFile(t, sanitized), readTranscriptFile(t, wantSanitized); !bytes.Equal(got, want) {
		t.Fatalf("sanitized = %q, want %q", got, want)
	}
}

func TestEventTranscriptCrashLeavesOnlyMarkedPartialEvidence(t *testing.T) {
	root := t.TempDir()
	w := openEventTranscript(t, root)
	writeEvents(t, w, `{"kind":"started"}`)
	// A crash: the process stops here and Finalize never runs.
	staging, raw, sanitized := eventTranscriptPaths(t, root)
	if got := readTranscriptFile(t, staging); string(got) != "{\"kind\":\"started\"}\n" {
		t.Fatalf("staged = %q", got)
	}
	assertAbsent(t, raw, sanitized)
	// The crashed attempt's partial evidence is never reopened for appending.
	if _, err := (ArtifactStore{Root: root}).OpenExecutionEventTranscript(eventTranscriptProvider, eventTranscriptAttempt); !errors.Is(err, os.ErrExist) {
		t.Fatalf("reopen over staged evidence: err = %v, want ErrExist", err)
	}
}

func TestEventTranscriptRefusedEventFailsClosed(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := map[string]func(w *ExecutionEventTranscript) error{
		"not json": func(w *ExecutionEventTranscript) error {
			return w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte("{"))
		},
		"multi-line": func(w *ExecutionEventTranscript) error {
			return w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte("{\n}"))
		},
		"cancelled": func(w *ExecutionEventTranscript) error {
			return w.WriteEvent(cancelled, eventTranscriptAttempt, []byte("{}"))
		},
		"other attempt": func(w *ExecutionEventTranscript) error {
			other := eventTranscriptAttempt
			other.Attempt++
			return w.WriteEvent(context.Background(), other, []byte("{}"))
		},
		"oversized event": func(w *ExecutionEventTranscript) error {
			big := `"` + strings.Repeat("a", maxExecutionEventBytes) + `"`
			return w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte(big))
		},
	}
	for name, refuse := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			w := openEventTranscript(t, root)
			writeEvents(t, w, `{"kind":"started"}`)
			if err := refuse(w); !errors.Is(err, ErrEventTranscriptIncomplete) {
				t.Fatalf("refused write: err = %v", err)
			}
			if err := w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte("{}")); !errors.Is(err, ErrEventTranscriptIncomplete) {
				t.Fatalf("write after failure: err = %v", err)
			}
			for range 2 {
				if _, err := w.Finalize(); !errors.Is(err, ErrEventTranscriptIncomplete) {
					t.Fatalf("finalize after failure: err = %v", err)
				}
			}
			staging, raw, sanitized := eventTranscriptPaths(t, root)
			assertAbsent(t, raw, sanitized)
			if _, err := os.Stat(staging); err != nil {
				t.Fatalf("partial evidence must remain: %v", err)
			}
		})
	}
}

func TestEventTranscriptTotalBound(t *testing.T) {
	previous := maxCapturedProcessBytes
	maxCapturedProcessBytes = 64
	defer func() { maxCapturedProcessBytes = previous }()
	root := t.TempDir()
	w := openEventTranscript(t, root)
	event := `{"k":"` + strings.Repeat("a", 20) + `"}` // 29 bytes with newline
	writeEvents(t, w, event, event)
	if err := w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte(event)); !errors.Is(err, ErrEventTranscriptIncomplete) {
		t.Fatalf("write past bound: err = %v", err)
	}
	staging, _, _ := eventTranscriptPaths(t, root)
	if got := len(readTranscriptFile(t, staging)); got != 58 {
		t.Fatalf("staged %d bytes, want the 58 accepted", got)
	}
}

func TestEventTranscriptRedactsEveryFile(t *testing.T) {
	root := t.TempDir()
	secrets := []string{"ghp_abc123secret", "github_pat_11abc_def", "Authorization: Bearer tok3n"}
	w := openEventTranscript(t, root)
	for _, secret := range secrets {
		event, _ := json.Marshal(map[string]string{"out": secret})
		writeEvents(t, w, string(event))
	}
	staging, raw, sanitized := eventTranscriptPaths(t, root)
	check := func(path string) {
		body := readTranscriptFile(t, path)
		for _, secret := range []string{"ghp_abc123secret", "github_pat_11abc_def", "tok3n"} {
			if bytes.Contains(body, []byte(secret)) {
				t.Fatalf("%s holds %q: %q", path, secret, body)
			}
		}
		if !bytes.Contains(body, []byte("[REDACTED]")) {
			t.Fatalf("%s carries no redaction marker: %q", path, body)
		}
	}
	check(staging)
	if _, err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	check(raw)
	check(sanitized)
}

func TestEventTranscriptDoubleFinalizeIsIdempotent(t *testing.T) {
	root := t.TempDir()
	w := openEventTranscript(t, root)
	writeEvents(t, w, `{"kind":"settled"}`)
	first, err := w.Finalize()
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.Finalize()
	if err != nil || fmt.Sprint(first) != fmt.Sprint(second) {
		t.Fatalf("second finalize = %+v, %v; want %+v", second, err, first)
	}
	if err := w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte("{}")); err == nil {
		t.Fatal("a settled transcript accepted an event")
	}
	var conflict *TranscriptConflictError
	if _, err := (ArtifactStore{Root: root}).OpenExecutionEventTranscript(eventTranscriptProvider, eventTranscriptAttempt); !errors.As(err, &conflict) {
		t.Fatalf("reopen over published transcript: err = %v", err)
	}
}

func TestEventTranscriptFinalizeRefusesConflictAndKeepsStaging(t *testing.T) {
	root := t.TempDir()
	w := openEventTranscript(t, root)
	writeEvents(t, w, `{"kind":"settled"}`)
	staging, raw, _ := eventTranscriptPaths(t, root)
	if err := os.WriteFile(raw, []byte("someone else's bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	var conflict *TranscriptConflictError
	for range 2 {
		if _, err := w.Finalize(); !errors.As(err, &conflict) {
			t.Fatalf("finalize over conflicting transcript: err = %v", err)
		}
	}
	if _, err := os.Stat(staging); err != nil {
		t.Fatalf("staged evidence must survive a refused publish: %v", err)
	}
}

func TestEventTranscriptConcurrentWrites(t *testing.T) {
	root := t.TempDir()
	w := openEventTranscript(t, root)
	const writers, each = 16, 20
	var wg sync.WaitGroup
	for g := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				event := fmt.Sprintf(`{"g":%d,"i":%d}`, g, i)
				if err := w.WriteEvent(context.Background(), eventTranscriptAttempt, []byte(event)); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if _, err := w.Finalize(); err != nil {
		t.Fatal(err)
	}
	_, raw, _ := eventTranscriptPaths(t, root)
	lines := strings.Split(strings.TrimSuffix(string(readTranscriptFile(t, raw)), "\n"), "\n")
	seen := map[string]bool{}
	for _, line := range lines {
		if !json.Valid([]byte(line)) {
			t.Fatalf("interleaved record %q", line)
		}
		seen[line] = true
	}
	if len(lines) != writers*each || len(seen) != writers*each {
		t.Fatalf("got %d lines (%d distinct), want %d", len(lines), len(seen), writers*each)
	}
}

func TestEventTranscriptFinalizeRefusesTamperedStaging(t *testing.T) {
	root := t.TempDir()
	w := openEventTranscript(t, root)
	writeEvents(t, w, `{"kind":"settled"}`)
	staging, raw, sanitized := eventTranscriptPaths(t, root)
	f, err := os.OpenFile(staging, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"forged\":true}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := w.Finalize(); !errors.Is(err, ErrEventTranscriptIncomplete) {
		t.Fatalf("finalize over tampered staging: err = %v", err)
	}
	assertAbsent(t, raw, sanitized)
}
