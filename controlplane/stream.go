package controlplane

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// StreamMessage is the one typed envelope that crosses the per-run SSE wire.
// Exactly one of Run, Event or Reason is populated, selected by Type: a
// reader resuming from a durable sequence, replaying a page of backlog, or
// just confirming the connection is alive never receives a shape it has to
// guess at. It carries no workspace path, payload or diagnostic prose -
// Run and Event are the same sanitized DTOs GET /v1/runs/{id} and
// GET /v1/runs/{id}/events already return.
type StreamMessage struct {
	Type   string     `json:"type"`
	Run    *RunDetail `json:"run,omitempty"`
	Event  *Event     `json:"event,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

const (
	streamDefaultPage      = 100
	streamMaxPage          = 500
	defaultStreamPoll      = time.Second
	defaultStreamHeartbeat = 15 * time.Second
)

func (a *API) pollInterval() time.Duration {
	if a.StreamPollInterval > 0 {
		return a.StreamPollInterval
	}
	return defaultStreamPoll
}
func (a *API) heartbeatInterval() time.Duration {
	if a.StreamHeartbeatInterval > 0 {
		return a.StreamHeartbeatInterval
	}
	return defaultStreamHeartbeat
}

// streamCursor reads the resume position a client offers. Last-Event-ID (the
// SSE reconnect header) takes precedence over an explicit ?after=. /v1 is
// Bearer-only, which a browser EventSource cannot send, so the client here is
// a Bearer-capable reader that tracks the last id itself. Either one present means this is a resume, not a fresh connect, so
// the handler skips the canonical snapshot and goes straight to replaying
// backlog after that exact position - never a second, different reduction of
// the same run.
func (a *API) streamCursor(r *http.Request) (cursor int64, resuming, ok bool) {
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		n, err := strconv.ParseInt(last, 10, 64)
		return n, true, err == nil && n >= 0
	}
	if _, present := r.URL.Query()["after"]; present {
		n, pok := pageNumber(r, "after", 0, 1<<63-1)
		return n, true, pok
	}
	return 0, false, true
}

func writeSSE(w http.ResponseWriter, flusher http.Flusher, id int64, msg StreamMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", id, body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// drain replays every event after *cursor in bounded, LIMIT-sized pages -
// EventsPage, never EventsAfter - advancing *cursor only once a page's events
// are confirmed written. A write failure or read failure leaves *cursor at
// the last event the client actually received, so a reconnect resumes there
// and never skips or re-fabricates state; a read failure is reported as an
// explicit stale frame before the stream closes. It returns false exactly
// when the caller must stop serving this connection.
func (a *API) drain(w http.ResponseWriter, flusher http.Flusher, id string, limit int, cursor *int64) bool {
	for {
		events, more, err := a.Store.EventsPage(id, *cursor, limit)
		if err != nil {
			_ = writeSSE(w, flusher, *cursor, StreamMessage{Type: "stale", Reason: "read_failed"})
			return false
		}
		for _, e := range events {
			ev := eventProjection(e)
			if err := writeSSE(w, flusher, ev.Sequence, StreamMessage{Type: "event", Event: &ev}); err != nil {
				return false
			}
			*cursor = ev.Sequence
		}
		if !more {
			return true
		}
	}
}

// stream serves the per-run SSE boundary: one canonical snapshot on a fresh
// connect, then an ordered incremental tail, resumable by Last-Event-ID
// against the durable run-local sequence - never a fleet-wide cursor that
// does not exist. It is read-only and SQL-bounded throughout: every replay
// read is a LIMIT-ed EventsPage call, so a large journal is paginated through
// rather than loaded for the connection to hold.
func (a *API) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "stream_unsupported")
		return
	}
	id := r.PathValue("id")
	if !a.exists(w, id) {
		return
	}
	cursor, resuming, ok := a.streamCursor(r)
	limit, lok := pageNumber(r, "limit", streamDefaultPage, streamMaxPage)
	if !ok || !lok || limit == 0 {
		fail(w, 400, "invalid_page")
		return
	}

	// Every read the first frame depends on happens BEFORE the SSE headers, so
	// a failure there is an ordinary JSON error, never a pre-snapshot stale
	// frame whose id a client would resume from without ever seeing a
	// snapshot.
	//
	// The durable cursor is read BEFORE the point-in-time snapshot is built,
	// never after: if an event committed between the two reads, a cursor taken
	// afterward would advance past state the snapshot never observed, and a
	// client resuming from it would silently skip that event. Reading the
	// cursor first means the worst case is the conservative direction - the
	// snapshot may already reflect the event and drain() replays it anyway, a
	// harmless duplicate the client dedupes by sequence.
	seq, err := a.Store.LatestSequence(id)
	if err != nil {
		fail(w, 500, "read_failed")
		return
	}
	// A resume position beyond anything this run has committed names no
	// durable event; it is refused rather than tailed forever.
	if resuming && cursor > seq {
		fail(w, 400, "invalid_page")
		return
	}
	var detail RunDetail
	if !resuming {
		if a.afterCursorRead != nil {
			a.afterCursorRead()
		}
		s, err := a.Store.Status(id, a.now())
		if err != nil {
			fail(w, 500, "read_failed")
			return
		}
		if a.afterSnapshotRead != nil {
			a.afterSnapshotRead()
		}
		detail = runDetailProjection(s)
	}

	// The server's WriteTimeout bounds one request's write; an SSE stream is
	// deliberately long-lived, so this connection's deadline is cleared here
	// rather than widening the timeout for every other, ordinary response.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	if !resuming {
		if err := writeSSE(w, flusher, seq, StreamMessage{Type: "snapshot", Run: &detail}); err != nil {
			return
		}
		cursor = seq
	}

	if !a.drain(w, flusher, id, int(limit), &cursor) {
		return
	}

	ctx := r.Context()
	poll := time.NewTicker(a.pollInterval())
	defer poll.Stop()
	heartbeat := time.NewTicker(a.heartbeatInterval())
	defer heartbeat.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if err := writeSSE(w, flusher, cursor, StreamMessage{Type: "heartbeat"}); err != nil {
				return
			}
		case <-poll.C:
			if !a.drain(w, flusher, id, int(limit), &cursor) {
				return
			}
		}
	}
}
