package execution

import (
	"context"
	"errors"
	"testing"
)

// The writer is the fallible view; the recorder derived from it writes
// through the same path and drops the error, so recorder-only providers keep
// their behaviour.
func TestProgressWriterInstallsTheBestEffortRecorder(t *testing.T) {
	var writes []Progress
	failure := errors.New("store unavailable")
	ctx := WithProgressWriter(context.Background(), func(p Progress) error {
		writes = append(writes, p)
		return failure
	})
	if err := ProgressWriterFrom(ctx)(Progress{Key: "1"}); !errors.Is(err, failure) {
		t.Errorf("writer error %v, want the write's own error", err)
	}
	record := ProgressRecorder(ctx)
	if record == nil {
		t.Fatal("installing a writer left no best-effort recorder")
	}
	record(Progress{Key: "2"})
	if len(writes) != 2 || writes[1].Key != "2" {
		t.Errorf("writes %+v: the recorder must write through the writer", writes)
	}
}

func TestProgressWriterAbsentOrNil(t *testing.T) {
	ctx := WithProgressRecorder(context.Background(), func(Progress) {})
	if ProgressWriterFrom(ctx) != nil {
		t.Error("a recorder alone must not appear as a fallible writer")
	}
	if WithProgressWriter(ctx, nil) != ctx {
		t.Error("a nil writer must leave the context unchanged")
	}
}
