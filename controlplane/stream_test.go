package controlplane

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	"github.com/bogdaniel/zenchron-engineering/schemas"
)

// streamFixture builds a run with three durable events and a live, running
// operation row - the same shape api_test.go's fixture builds - but keeps the
// writable store open so a test can append further events while a stream
// connection tails them, and serves the API over a real httptest.Server so
// the stream's long-lived response can be read concurrently with the
// handler's own goroutine, which httptest.NewRecorder cannot do.
func streamFixture(t *testing.T) (write *rt.SQLiteOperationStore, srv *httptest.Server, api *API) {
	t.Helper()
	dir := t.TempDir()
	write, err := rt.OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { write.Close() })
	at := time.Unix(100, 0).UTC()
	run := rt.EngineeringRun{SchemaVersion: rt.SchemaVersion, ID: "r", Repository: "example/repo", Goal: "goal", Phase: rt.Execute, Disposition: rt.Active, ControllerSHA256: strings.Repeat("4", 64), CreatedAt: at, UpdatedAt: at}
	if err := write.PutRun(run); err != nil {
		t.Fatal(err)
	}
	op := rt.RunOperation{SchemaVersion: rt.SchemaVersion, ID: "op", RunID: "r", Kind: "provider", IdempotencyKey: "key", State: rt.Running, Attempt: 1, MaxAttempts: 2, StartedAt: &at, LastProgressAt: &at}
	payload, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []rt.EngineeringEvent{
		{ID: "created", Type: rt.EventRunCreated},
		{ID: "operation", Type: rt.EventOperationPlanned, OperationID: "op", Payload: payload},
		{ID: "waiting-1", Type: rt.EventRunWaiting, Payload: json.RawMessage(`{"reason":"first"}`)},
	} {
		e.SchemaVersion = rt.SchemaVersion
		e.RunID = "r"
		e.OccurredAt = at
		if _, err := write.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := write.PutOperation(op, 0); err != nil {
		t.Fatal(err)
	}
	reader, err := rt.OpenReadStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reader.Close() })
	api = &API{
		Store: reader, Token: "test-token", Now: func() time.Time { return time.Unix(300, 0).UTC() },
		StreamPollInterval: 10 * time.Millisecond, StreamHeartbeatInterval: time.Hour,
	}
	srv = httptest.NewServer(api.Handler())
	t.Cleanup(srv.Close)
	return write, srv, api
}

// sseConn is an open per-run stream connection, read frame by frame.
type sseConn struct {
	t      *testing.T
	resp   *http.Response
	reader *bufio.Reader
}

func dialStream(t *testing.T, srv *httptest.Server, token, path, lastEventID string) *sseConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+path, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if lastEventID != "" {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	c := &sseConn{t: t, resp: resp, reader: bufio.NewReader(resp.Body)}
	t.Cleanup(func() {
		resp.Body.Close()
		cancel()
	})
	return c
}

// next reads one SSE frame (id + data lines up to the blank separator),
// decodes its data as a StreamMessage and checks the decoded value against
// the wire schema - the same discipline api_test.go's wire() applies to the
// plain JSON endpoints, now applied to the SSE DTO.
func (c *sseConn) next() (id string, msg StreamMessage) {
	c.t.Helper()
	var dataLine string
	for {
		line, err := c.reader.ReadString('\n')
		if err != nil {
			c.t.Fatalf("read SSE frame: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case line == "":
			if dataLine == "" {
				continue
			}
			var raw any
			if err := json.Unmarshal([]byte(dataLine), &raw); err != nil {
				c.t.Fatalf("decode frame: %v: %s", err, dataLine)
			}
			if err := schemas.Validate("control-plane-run-stream", raw); err != nil {
				c.t.Fatalf("schema: %v: %s", err, dataLine)
			}
			if err := json.Unmarshal([]byte(dataLine), &msg); err != nil {
				c.t.Fatal(err)
			}
			return id, msg
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			dataLine = strings.TrimPrefix(line, "data: ")
		}
	}
}

func TestStreamSnapshotThenLiveTail(t *testing.T) {
	write, srv, api := streamFixture(t)
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream", "")
	if ct := conn.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type: %s", ct)
	}

	id, msg := conn.next()
	if id != "3" || msg.Type != "snapshot" || msg.Run == nil {
		t.Fatalf("expected baseline snapshot at id 3, got id=%s msg=%+v", id, msg)
	}
	if msg.Run.ID != "r" || msg.Run.Operation == nil || msg.Run.Operation.ProgressSource != "row" {
		t.Fatalf("snapshot lost canonical run state: %+v", msg.Run)
	}
	// The snapshot is GET /v1/runs/{id}'s own runDetailProjection, byte for
	// byte, so the two views of one run can never drift (#418 review).
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/runs/r", nil)
	req.Header.Set("Authorization", "Bearer "+api.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var got RunDetail
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if a, b := mustJSON(t, got), mustJSON(t, *msg.Run); a != b {
		t.Fatalf("SSE snapshot drifted from GET /v1/runs/r:\nGET:  %s\nSSE:  %s", a, b)
	}

	e := rt.EngineeringEvent{ID: "waiting-2", Type: rt.EventRunWaiting, Payload: json.RawMessage(`{"reason":"second"}`), SchemaVersion: rt.SchemaVersion, RunID: "r", OccurredAt: time.Unix(200, 0).UTC()}
	if _, err := write.AppendEvent(e); err != nil {
		t.Fatal(err)
	}

	id, msg = conn.next()
	if id != "4" || msg.Type != "event" || msg.Event == nil || msg.Event.Sequence != 4 {
		t.Fatalf("expected live-tailed event at id 4, got id=%s msg=%+v", id, msg)
	}
}

func appendRaceEvent(t *testing.T, write *rt.SQLiteOperationStore) func() {
	return func() {
		e := rt.EngineeringEvent{ID: "race", Type: rt.EventRunWaiting, Payload: json.RawMessage(`{"reason":"race"}`), SchemaVersion: rt.SchemaVersion, RunID: "r", OccurredAt: time.Unix(150, 0).UTC()}
		if _, err := write.AppendEvent(e); err != nil {
			t.Error(err)
		}
	}
}

// TestStreamFreshConnectEventAfterSnapshotReadIsNeverSkipped commits event 4
// right after the snapshot's Status read, so it is in neither the snapshot
// nor (with the cursor read first) the cursor. It must be the next frame. If
// the cursor were read after the snapshot - the original bug - the snapshot
// id would already be 4, drain would start after it, and event 4 would never
// arrive: the next frame would be the 2s heartbeat instead.
func TestStreamFreshConnectEventAfterSnapshotReadIsNeverSkipped(t *testing.T) {
	write, srv, api := streamFixture(t)
	api.StreamHeartbeatInterval = 2 * time.Second
	api.afterSnapshotRead = appendRaceEvent(t, write)
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream", "")

	if _, msg := conn.next(); msg.Type != "snapshot" {
		t.Fatalf("expected snapshot first, got %+v", msg)
	}
	id, msg := conn.next()
	if msg.Type != "event" || msg.Event == nil || msg.Event.Sequence != 4 {
		t.Fatalf("event 4 committed after the snapshot read was never delivered; next frame id=%s msg=%+v", id, msg)
	}
}

// TestStreamFreshConnectEventBeforeSnapshotReadMayDuplicate commits event 4
// between the cursor read and the snapshot's Status read. The snapshot may
// already reflect it; it is still replayed as an incremental frame - the
// conservative duplicate the S2 contract permits.
func TestStreamFreshConnectEventBeforeSnapshotReadMayDuplicate(t *testing.T) {
	write, srv, api := streamFixture(t)
	api.afterCursorRead = appendRaceEvent(t, write)
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream", "")

	if _, msg := conn.next(); msg.Type != "snapshot" {
		t.Fatalf("expected snapshot first, got %+v", msg)
	}
	id, msg := conn.next()
	if id != "4" || msg.Type != "event" || msg.Event == nil || msg.Event.Sequence != 4 {
		t.Fatalf("event committed between cursor read and snapshot must still be delivered, got id=%s msg=%+v", id, msg)
	}
}

func TestStreamHeartbeatFrame(t *testing.T) {
	_, srv, api := streamFixture(t)
	api.StreamHeartbeatInterval = 10 * time.Millisecond
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream", "3")
	if id, msg := conn.next(); id != "3" || msg.Type != "heartbeat" {
		t.Fatalf("expected heartbeat at the resume cursor, got id=%s msg=%+v", id, msg)
	}
}

func TestStreamResumeFromLastEventIDSkipsSnapshotAndPaginatesBacklog(t *testing.T) {
	_, srv, api := streamFixture(t)
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream?limit=1", "1")

	id, msg := conn.next()
	if msg.Type == "snapshot" {
		t.Fatalf("resume must not re-send the canonical snapshot, got %+v", msg)
	}
	if id != "2" || msg.Event == nil || msg.Event.Sequence != 2 {
		t.Fatalf("expected first backlog event (sequence 2), got id=%s msg=%+v", id, msg)
	}

	id, msg = conn.next()
	if id != "3" || msg.Type != "event" || msg.Event == nil || msg.Event.Sequence != 3 {
		t.Fatalf("expected bounded backlog to continue to sequence 3, got id=%s msg=%+v", id, msg)
	}
}

func TestStreamUnknownRunIsOrdinary404(t *testing.T) {
	_, srv, api := streamFixture(t)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/runs/missing/stream", nil)
	req.Header.Set("Authorization", "Bearer "+api.Token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("a missing run must fail before any SSE header is written: %s", ct)
	}
}

func TestStreamRejectsInvalidPageParameters(t *testing.T) {
	_, srv, api := streamFixture(t)
	for _, tc := range []struct{ path, lastEventID string }{
		{"/v1/runs/r/stream?after=-1", ""},
		{"/v1/runs/r/stream?limit=0", ""},
		{"/v1/runs/r/stream?limit=501", ""},
		{"/v1/runs/r/stream", "not-a-number"},
		// Resume positions beyond the run's latest sequence (3).
		{"/v1/runs/r/stream?after=4", ""},
		{"/v1/runs/r/stream", "99"},
	} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
		req.Header.Set("Authorization", "Bearer "+api.Token)
		if tc.lastEventID != "" {
			req.Header.Set("Last-Event-ID", tc.lastEventID)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body any
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if resp.StatusCode != 400 || err != nil {
			t.Fatalf("%+v: status %d: %v", tc, resp.StatusCode, err)
		}
		if err := schemas.Validate("control-plane-error", body); err != nil || body.(map[string]any)["error"] != "invalid_page" {
			t.Fatalf("%+v: body %v: %v", tc, body, err)
		}
	}
}

func TestStreamUnauthorizedNeverOpensTheConnection(t *testing.T) {
	_, srv, _ := streamFixture(t)
	resp, err := http.Get(srv.URL + "/v1/runs/r/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("status: %d", resp.StatusCode)
	}
}

func TestStreamReadFailureEmitsExplicitStaleFrameAndCloses(t *testing.T) {
	_, srv, api := streamFixture(t)
	conn := dialStream(t, srv, api.Token, "/v1/runs/r/stream", "")
	if _, msg := conn.next(); msg.Type != "snapshot" {
		t.Fatalf("expected snapshot first, got %+v", msg)
	}

	if err := api.Store.Close(); err != nil {
		t.Fatal(err)
	}

	_, msg := conn.next()
	if msg.Type != "stale" || msg.Reason != "read_failed" {
		t.Fatalf("expected an explicit stale frame, got %+v", msg)
	}
	if _, err := conn.reader.ReadByte(); err == nil {
		t.Fatal("expected the stream to close after reporting stale")
	}
}

func TestStreamRequiresAResponseFlusher(t *testing.T) {
	a := &API{}
	rec := &noFlushRecorder{rec: httptest.NewRecorder()}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/r/stream", nil)
	req.SetPathValue("id", "r")
	a.stream(rec, req)
	if rec.rec.Code != 500 {
		t.Fatalf("status: %d", rec.rec.Code)
	}
	var out Error
	if err := json.Unmarshal(rec.rec.Body.Bytes(), &out); err != nil || out.Code != "stream_unsupported" {
		t.Fatalf("body: %s", rec.rec.Body.String())
	}
	var raw any
	if err := json.Unmarshal(rec.rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if err := schemas.Validate("control-plane-error", raw); err != nil {
		t.Fatalf("schema: %v", err)
	}
}

// noFlushRecorder is an http.ResponseWriter that is deliberately NOT an
// http.Flusher. It holds httptest.ResponseRecorder as a named field rather
// than embedding it, because embedding would promote Flush and defeat the
// one thing this type exists to test.
type noFlushRecorder struct {
	rec *httptest.ResponseRecorder
}

func (r *noFlushRecorder) Header() http.Header         { return r.rec.Header() }
func (r *noFlushRecorder) Write(b []byte) (int, error) { return r.rec.Write(b) }
func (r *noFlushRecorder) WriteHeader(status int)      { r.rec.WriteHeader(status) }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
