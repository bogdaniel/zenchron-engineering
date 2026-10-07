package api

import (
	"context"
	"time"
)

// EventKind names one observable step of an execution.
type EventKind string

const (
	EventStarted          EventKind = "execution.started"
	EventRefused          EventKind = "execution.refused"
	EventContextCompiled  EventKind = "context.compiled"
	EventRoutingDecided   EventKind = "routing.decided"
	EventProviderRequest  EventKind = "provider.requested"
	EventProviderResponse EventKind = "provider.responded"
	EventProviderError    EventKind = "provider.failed"
	EventToolProposed     EventKind = "tool.proposed"
	EventToolRefused      EventKind = "tool.refused"
	EventToolExecuted     EventKind = "tool.executed"
	EventArtifactRecorded EventKind = "artifact.recorded"
	EventSettled          EventKind = "execution.settled"
)

// Event is one host-accountable observation. It is enough for host accounting
// and is never a second engineering journal.
type Event struct {
	Version     string       `json:"version"`
	ExecutionID string       `json:"execution_id"`
	AttemptID   string       `json:"attempt_id"`
	Seq         int64        `json:"seq"`
	Kind        EventKind    `json:"kind"`
	ObservedAt  time.Time    `json:"observed_at"`
	Source      string       `json:"source"`
	Detail      string       `json:"detail,omitempty"`
	ToolCall    string       `json:"tool_call,omitempty"`
	Grant       string       `json:"grant,omitempty"`
	Ref         *ArtifactRef `json:"ref,omitempty"`
	Usage       *TokenUsage  `json:"usage,omitempty"`
}

// EventSink records events. A returned error means the event is not recorded;
// the kernel then stops further side effects and settles recording_failed.
// Record's context ignores the execution's cancellation but carries a
// deadline (the budget deadline, or a settlement grace for terminal events);
// a call that has not returned by then is abandoned and is a recording
// failure. Record should honour its context: one that does not keeps its
// goroutine alive, though never the execution.
type EventSink interface {
	Record(ctx context.Context, event Event) error
}

// Clock is the execution's time source.
type Clock interface {
	Now() time.Time
}
