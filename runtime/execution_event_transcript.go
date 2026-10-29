package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// eventTranscriptStagingSuffix names the per-attempt file events stream into
// before settlement. "partial" is in the name on purpose: whatever a crash
// leaves under it is evidence of an attempt that never finished recording, and
// nothing reading the ordinary .raw.log / .sanitized-candidate.log names can
// mistake it for a complete transcript.
const eventTranscriptStagingSuffix = ".events.partial.log"

// maxExecutionEventBytes bounds one event. The stream as a whole is bounded by
// maxCapturedProcessBytes, the same ceiling a provider subprocess's captured
// output has, because the finalized transcript is read whole by the next
// attempt exactly as a captured one is.
const maxExecutionEventBytes = 1 << 20

// ErrEventTranscriptIncomplete is the refusal to finalize, or to keep
// recording into, a transcript that already failed to record an event. Its
// staging file is left in place as partial evidence.
var ErrEventTranscriptIncomplete = errors.New("execution event transcript is incomplete: an event was not recorded")

// ExecutionEventTranscript streams one execution attempt's events durably and,
// on settlement, publishes them as that attempt's ordinary create-once
// transcript.
//
// Every refused WriteEvent is sticky. An error means an event was not recorded,
// so the caller settles recording_failed; a transcript missing that event must
// never be published under a name that claims completeness.
type ExecutionEventTranscript struct {
	store   ArtifactStore
	attempt ExecutionAttemptRef
	prefix  string
	staging string

	mu       sync.Mutex
	file     *os.File
	written  int
	failed   error
	sealed   []byte
	isSealed bool
}

// OpenExecutionEventTranscript creates the staging file for one attempt. It
// refuses an attempt that already has staged or published evidence: a
// physical attempt records once, and a leftover staging file is a crashed
// attempt's partial evidence, never something to append to.
func (s ArtifactStore) OpenExecutionEventTranscript(providerID string, attempt ExecutionAttemptRef) (*ExecutionEventTranscript, error) {
	if s.Root == "" {
		return nil, fmt.Errorf("artifact root required")
	}
	prefix, err := attemptTranscriptPrefix(providerID, attempt)
	if err != nil {
		return nil, err
	}
	base := filepath.Join(s.Root, prefix)
	if _, err := os.Lstat(base + ".raw.log"); err == nil {
		return nil, &TranscriptConflictError{Path: base + ".raw.log"}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(base), 0700); err != nil {
		return nil, err
	}
	staging := base + eventTranscriptStagingSuffix
	file, err := os.OpenFile(staging, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("open execution event transcript: %w", err)
	}
	if err := syncDirectory(filepath.Dir(staging)); err != nil {
		file.Close()
		return nil, err
	}
	return &ExecutionEventTranscript{store: s, attempt: attempt, prefix: prefix, staging: staging, file: file}, nil
}

// WriteEvent appends one encoded event as one redacted line and returns only
// after it is fsynced. The event must be single-line JSON for its own attempt.
func (t *ExecutionEventTranscript) WriteEvent(ctx context.Context, attempt ExecutionAttemptRef, event []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return t.failed
	}
	if t.file == nil {
		return fmt.Errorf("execution event transcript is settled; no further events are accepted")
	}
	if err := t.appendLocked(ctx, attempt, event); err != nil {
		t.file.Close()
		t.file = nil
		t.failed = fmt.Errorf("%w: %w", ErrEventTranscriptIncomplete, err)
		return t.failed
	}
	return nil
}

func (t *ExecutionEventTranscript) appendLocked(ctx context.Context, attempt ExecutionAttemptRef, event []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if attempt != t.attempt {
		return fmt.Errorf("event for attempt %+v offered to the transcript of attempt %+v", attempt, t.attempt)
	}
	if len(event) > maxExecutionEventBytes {
		return fmt.Errorf("event of %d bytes exceeds the %d-byte event bound", len(event), maxExecutionEventBytes)
	}
	// Single-line JSON is what makes per-line redaction equal to redacting the
	// whole stream: no secret pattern can span a record boundary.
	if !json.Valid(event) || bytes.IndexByte(event, '\n') >= 0 {
		return fmt.Errorf("event is not single-line JSON")
	}
	line := append(redactTranscript(event), '\n')
	if t.written+len(line) > maxCapturedProcessBytes {
		return fmt.Errorf("event would raise the transcript past its %d-byte bound", maxCapturedProcessBytes)
	}
	if _, err := t.file.Write(line); err != nil {
		return err
	}
	if err := t.file.Sync(); err != nil {
		return err
	}
	t.written += len(line)
	return nil
}

// Finalize stops accepting events and publishes the staged records as the
// attempt's ordinary transcript through the create-once path, then removes
// the staging file.
//
// It refuses with ErrEventTranscriptIncomplete after any failed WriteEvent.
// It is idempotent: a retry after a failed publish or removal republishes the
// same sealed bytes, which create-once accepts, so a call after success
// returns the same artifacts. A crash after publish and before removal leaves
// both files; the published transcript is authoritative and the staging copy
// is redundant.
func (t *ExecutionEventTranscript) Finalize() ([]Artifact, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failed != nil {
		return nil, t.failed
	}
	if !t.isSealed {
		if err := t.sealLocked(); err != nil {
			return nil, err
		}
	}
	artifacts, err := t.store.writeTranscript(t.prefix, t.sealed, nil, true)
	if err != nil {
		return nil, err
	}
	if err := os.Remove(t.staging); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err := syncDirectory(filepath.Dir(t.staging)); err != nil {
		return nil, err
	}
	return artifacts, nil
}

// sealLocked closes the staging file and reads back what is durable there,
// which is what gets published: the file, not a memory copy, is the record.
func (t *ExecutionEventTranscript) sealLocked() error {
	if t.file != nil {
		err := t.file.Close()
		t.file = nil
		if err != nil {
			t.failed = fmt.Errorf("%w: %w", ErrEventTranscriptIncomplete, err)
			return t.failed
		}
	}
	body, err := os.ReadFile(t.staging)
	if err != nil {
		return fmt.Errorf("read staged execution events: %w", err)
	}
	if len(body) != t.written {
		t.failed = fmt.Errorf("%w: staged %d bytes, recorded %d", ErrEventTranscriptIncomplete, len(body), t.written)
		return t.failed
	}
	t.sealed, t.isSealed = body, true
	return nil
}
