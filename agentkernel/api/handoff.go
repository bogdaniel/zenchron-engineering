package api

import "context"

// Call is one request the kernel hands to a host-owned worker. The kernel
// never runs host code on its own goroutines: every host port (event sink,
// context source, provider, command runner, host tool) is a channel of Calls
// that host-started workers read. The kernel waits a bounded time to hand a
// Call over and a bounded time for its reply, then stops waiting; a stuck
// worker strands only the host's goroutine.
//
// Context is the call's context, like http.Request's: it carries the bound
// the kernel waits for and is cancelled once the kernel stops waiting, so a
// worker can stop work nobody will read.
//
// ID is stable and unique per request within one execution attempt
// (execution, attempt and a sequence or call id). The kernel never sends the
// same ID twice; a worker that sees one again (its own retry, a replayed
// queue) must treat it as the same request, so delayed or duplicate
// processing is idempotent.
//
// Reply is created by the kernel for this call alone with capacity 1. A
// worker sends exactly one value on it and never closes it; because it is
// buffered the send never blocks, even after the kernel stopped waiting, and
// the kernel discards a reply that arrives late.
type Call[Q, R any] struct {
	ID      string
	Context context.Context
	Request Q
	Reply   chan<- R
}

// Reply is a worker's answer: a value, or the error that replaced it.
type Reply[T any] struct {
	Value T
	Err   error
}

// EventDelivery hands one event to the host. The reply is nil once the event
// is durable as the host's contract requires, or the error that stopped it.
type EventDelivery = Call[Event, error]

// ContextRequest asks a host-served context source for optional items.
type ContextRequest = Call[ContextQuery, Reply[[]ContextItem]]

// ProviderCall hands one model call to a host-served provider adapter.
type ProviderCall = Call[ProviderRequest, Reply[ProviderResponse]]

// CommandCall hands one granted command to the host's process boundary.
type CommandCall = Call[CommandRequest, Reply[CommandResult]]

// Serve starts a host-owned worker that answers calls with handle, one at a
// time, until ctx ends, and returns the unbuffered channel to give the
// kernel. The goroutine belongs to the host: cancelling ctx stops it between
// calls, and a handle that ignores its context strands this goroutine, never
// the kernel's. A host wanting parallel answers starts its own workers on
// its own channel instead.
func Serve[Q, R any](ctx context.Context, handle func(context.Context, Q) R) chan<- Call[Q, R] {
	calls := make(chan Call[Q, R])
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case c := <-calls:
				c.Reply <- handle(c.Context, c.Request)
			}
		}
	}()
	return calls
}

// ServeEvents serves an EventSink (see Serve).
func ServeEvents(ctx context.Context, sink EventSink) chan<- EventDelivery {
	return Serve(ctx, sink.Record)
}

// ServeContext serves a ContextSource (see Serve).
func ServeContext(ctx context.Context, src ContextSource) chan<- ContextRequest {
	return Serve(ctx, func(ctx context.Context, q ContextQuery) Reply[[]ContextItem] {
		items, err := src.ContextItems(ctx, q)
		return Reply[[]ContextItem]{Value: items, Err: err}
	})
}

// ServeProvider serves a Provider, a kernel adapter or a host one alike (see
// Serve). Adapters call host code (credential sources, HTTP doers), so no
// provider is ever called on a kernel goroutine.
func ServeProvider(ctx context.Context, p Provider) chan<- ProviderCall {
	return Serve(ctx, func(ctx context.Context, req ProviderRequest) Reply[ProviderResponse] {
		resp, err := p.Complete(ctx, req)
		return Reply[ProviderResponse]{Value: resp, Err: err}
	})
}

// ServeCommands serves a CommandRunner (see Serve).
func ServeCommands(ctx context.Context, runner CommandRunner) chan<- CommandCall {
	return Serve(ctx, func(ctx context.Context, req CommandRequest) Reply[CommandResult] {
		res, err := runner.Run(ctx, req)
		return Reply[CommandResult]{Value: res, Err: err}
	})
}
