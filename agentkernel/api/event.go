package api

import (
	"context"
	"sync"
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

// EventSink records events. A returned error means the event is not
// recorded; the kernel then stops further side effects and settles
// recording_failed. The kernel never calls Record itself: a host serves its
// sink to the kernel with ServeEvents (or its own worker on an
// EventDelivery channel). Record's context ignores the execution's
// cancellation but carries the bound the kernel waits for (the budget
// deadline, or a settlement grace for terminal events) and is cancelled when
// the kernel stops waiting; an event not acknowledged by then is a recording
// failure. Each event is identified by execution_id, attempt_id and seq.
type EventSink interface {
	Record(ctx context.Context, event Event) error
}

// Clock is the execution's time source. The kernel reads it synchronously,
// so engine.New accepts only the kernel's own clocks, by exact type:
// SystemClock, or *ManualClock for tests and deterministic replay. Any other
// implementation, one embedding these included, is refused.
type Clock interface {
	Now() time.Time
}

// SystemClock is the wall clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// ManualClock is a clock that moves only when told to.
type ManualClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewManualClock returns a clock reading now.
func NewManualClock(now time.Time) *ManualClock { return &ManualClock{now: now} }

func (c *ManualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to now.
func (c *ManualClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}

// Advance moves the clock forward by d.
func (c *ManualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
