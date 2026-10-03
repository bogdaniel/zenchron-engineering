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
		Store: reader, Token: "test-token",
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

	e := rt.EngineeringEvent{ID: "waiting-2", Type: rt.EventRunWaiting, Payload: json.RawMessage(`{"reason":"second"}`), SchemaVersion: rt.SchemaVersion, RunID: "r", OccurredAt: time.Unix(200, 0).UTC()}
	if _, err := write.AppendEvent(e); err != nil {
		t.Fatal(err)
	}

	id, msg = conn.next()
	if id != "4" || msg.Type != "event" || msg.Event == nil || msg.Event.Sequence != 4 {
		t.Fatalf("expected live-tailed event at id 4, got id=%s msg=%+v", id, msg)
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
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Fatalf("%+v: status %d", tc, resp.StatusCode)
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
