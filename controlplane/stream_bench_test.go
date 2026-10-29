package controlplane

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	rt "github.com/bogdaniel/zenchron-engineering/runtime"
	_ "modernc.org/sqlite"
)

// Scale of the real-shaped journal BenchmarkStreamReadCost measures against.
const (
	benchRunEvents    = 100_000
	benchAppendPrefix = 400 // big-run events written through the real AppendEvent path
	benchFleetRuns    = 100
	benchFleetEvents  = 40
	benchResumeFrom   = benchRunEvents / 2
)

var benchStart = time.Unix(1_700_000_000, 0).UTC()

// benchEvent is event n (1-based) of a long-lived run, in the shape the
// runtime itself journals: a repeating operation lifecycle (planned, before,
// after - each carrying the full RunOperation payload Reduce decodes) over a
// pool of 50 operations, followed by the supervisor's run.waiting.
func benchEvent(runID string, n int) rt.EngineeringEvent {
	cycle, step := (n-1)/4, (n-1)%4
	at := benchStart.Add(time.Duration(n) * time.Second)
	e := rt.EngineeringEvent{SchemaVersion: rt.SchemaVersion, ID: fmt.Sprintf("%s-e%d", runID, n), RunID: runID, OccurredAt: at}
	if step == 3 {
		e.Type = rt.EventRunWaiting
		e.Payload = json.RawMessage(fmt.Sprintf(`{"reason":"waiting for provider capacity (pass %d)"}`, cycle))
		return e
	}
	started := at.Add(-time.Minute)
	op := rt.RunOperation{
		SchemaVersion: rt.SchemaVersion, ID: fmt.Sprintf("%s-op%d", runID, cycle%50), RunID: runID, Kind: "provider",
		IdempotencyKey: rt.StableOperationKey(runID, "provider", fmt.Sprintf("op%d", cycle%50)),
		Attempt:        cycle/50 + 1, AttemptIdentity: cycle/50 + 1, MaxAttempts: 1_000_000,
		InputStateSHA256: strings.Repeat("a", 64), CreatedAt: benchStart, WallBudget: time.Hour,
	}
	switch step {
	case 0:
		e.Type, op.State = rt.EventOperationPlanned, rt.Pending
	case 1:
		e.Type, op.State = rt.EventOperationBefore, rt.Running
		op.StartedAt, op.LastProgressAt, op.ActiveSince = &started, &at, &started
		op.Lease = &rt.Lease{Owner: "supervisor-1", HeartbeatAt: at, ExpiresAt: at.Add(time.Minute)}
	case 2:
		e.Type, op.State = rt.EventOperationAfter, rt.Succeeded
		op.StartedAt, op.LastProgressAt = &started, &at
		op.ConsumedExecution, op.LastAttemptExecution = time.Minute, time.Minute
		op.Result = json.RawMessage(`{"outcome":"succeeded","summary_sha256":"` + strings.Repeat("b", 64) + `"}`)
	}
	e.OperationID = op.ID
	payload, err := json.Marshal(op)
	if err != nil {
		panic(err)
	}
	e.Payload = payload
	return e
}

func benchFatal(b *testing.B, err error) {
	b.Helper()
	if err != nil {
		b.Fatal(err)
	}
}

// buildBenchJournal writes a fleet of ordinary runs and the first
// benchAppendPrefix events of run "big" through the store's real AppendEvent
// path, then extends "big" to benchRunEvents rows in one transaction.
//
// The extension exists because AppendEvent re-reads and reduces the whole run
// twice per append (its own ponytail note), so 100k appends are quadratic -
// the per-append cost at the prefix is logged as evidence. Extended rows carry
// the real sequence, hash chain (previous_event_id/hash), EventDigest and
// canonical document, so Status's full Reduce verifies them exactly as it
// would appended rows. Only state_before/state_after are format-real
// (sha256 hex) rather than recomputed: recomputing them is the same
// quadratic reduce, and no read path the stream uses re-derives them.
func buildBenchJournal(b *testing.B, dir string) {
	store, err := rt.OpenSQLiteOperationStore(dir)
	benchFatal(b, err)
	putRun := func(id string) {
		benchFatal(b, store.PutRun(rt.EngineeringRun{SchemaVersion: rt.SchemaVersion, ID: id, Repository: "example/repo", Goal: "goal", Phase: rt.Execute, Disposition: rt.Active, ControllerSHA256: strings.Repeat("4", 64), CreatedAt: benchStart, UpdatedAt: benchStart}))
	}
	for r := 0; r < benchFleetRuns; r++ {
		id := fmt.Sprintf("fleet-%03d", r)
		putRun(id)
		for n := 1; n <= benchFleetEvents; n++ {
			_, err := store.AppendEvent(benchEvent(id, n))
			benchFatal(b, err)
		}
	}
	putRun("big")
	var last rt.EngineeringEvent
	var lastAppend time.Duration
	for n := 1; n <= benchAppendPrefix; n++ {
		start := time.Now()
		last, err = store.AppendEvent(benchEvent("big", n))
		lastAppend = time.Since(start)
		benchFatal(b, err)
	}
	b.Logf("real AppendEvent cost at run length %d: %s per append (grows linearly with run length, so appending all %d is quadratic)", benchAppendPrefix, lastAppend, benchRunEvents)
	benchFatal(b, store.Close())

	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db")+"?_pragma=busy_timeout(5000)")
	benchFatal(b, err)
	defer db.Close()
	var global int64
	benchFatal(b, db.QueryRow(`SELECT MAX(global_sequence) FROM events`).Scan(&global))
	tx, err := db.Begin()
	benchFatal(b, err)
	stmt, err := tx.Prepare(`INSERT INTO events (id, run_id, sequence, type, operation_id, previous_event_id, previous_event_hash, state_before, state_after, event_hash, document, global_sequence) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	benchFatal(b, err)
	for n := benchAppendPrefix + 1; n <= benchRunEvents; n++ {
		e := benchEvent("big", n)
		e.Sequence, e.PreviousEventID, e.PreviousEventHash = int64(n), last.ID, last.EventHash
		e.StateBefore = last.StateAfter
		sum := sha256.Sum256([]byte(strconv.Itoa(n)))
		e.StateAfter = hex.EncodeToString(sum[:])
		e.EventHash, err = rt.EventDigest(e)
		benchFatal(b, err)
		doc, err := rt.CanonicalJSON(e)
		benchFatal(b, err)
		global++
		_, err = stmt.Exec(e.ID, e.RunID, e.Sequence, e.Type, e.OperationID, e.PreviousEventID, e.PreviousEventHash, e.StateBefore, e.StateAfter, e.EventHash, string(doc), global)
		benchFatal(b, err)
		last = e
	}
	benchFatal(b, stmt.Close())
	benchFatal(b, tx.Commit())
}

// discardFlusher is a ResponseWriter+Flusher that keeps only what a test
// asserts on: the number of SSE frames written.
type discardFlusher struct {
	header http.Header
	frames int
}

func (d *discardFlusher) Header() http.Header { return d.header }
func (d *discardFlusher) Write(p []byte) (int, error) {
	d.frames += strings.Count(string(p), "\n\n")
	return len(p), nil
}
func (d *discardFlusher) WriteHeader(int) {}
func (d *discardFlusher) Flush()          {}

// BenchmarkStreamReadCost measures the per-run SSE boundary against a
// real-shaped journal: one run of benchRunEvents events beside a fleet of
// benchFleetRuns ordinary runs (#396 acceptance). Timings are evidence, not
// assertions; the bounded properties are asserted deterministically. Run:
//
//	go test ./controlplane -run '^$' -bench BenchmarkStreamReadCost -benchmem -benchtime 5x
func BenchmarkStreamReadCost(b *testing.B) {
	dir := b.TempDir()
	start := time.Now()
	buildBenchJournal(b, dir)
	b.Logf("built %d-event run + %d runs x %d events in %s", benchRunEvents, benchFleetRuns, benchFleetEvents, time.Since(start))
	reader, err := rt.OpenReadStore(dir)
	benchFatal(b, err)
	defer reader.Close()
	api := &API{Store: reader, Token: "t", StreamHeartbeatInterval: time.Hour, StreamPollInterval: time.Hour}
	h := api.Handler()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel() // the handler serves its first frames, then returns at the tail loop

	serve := func(lastEventID string) *discardFlusher {
		req := httptest.NewRequest(http.MethodGet, "/v1/runs/big/stream?limit=500", nil).WithContext(cancelled)
		req.Header.Set("Authorization", "Bearer t")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		w := &discardFlusher{header: http.Header{}}
		h.ServeHTTP(w, req)
		return w
	}

	// Bounded properties, asserted once, deterministically.
	if latest, err := reader.LatestSequence("big"); err != nil || latest != benchRunEvents {
		b.Fatalf("latest sequence %d, %v", latest, err)
	}
	if _, _, err := reader.EventsPage("big", 0, 501); err == nil {
		b.Fatal("a replay page above 500 rows was accepted")
	}
	if w := serve(""); w.frames != 1 {
		b.Fatalf("fresh connect at the head must be exactly one snapshot frame, got %d", w.frames)
	}
	if w := serve(strconv.Itoa(benchResumeFrom)); w.frames != benchRunEvents-benchResumeFrom {
		b.Fatalf("resume replayed %d frames, want %d", w.frames, benchRunEvents-benchResumeFrom)
	}
	poll := func() {
		cursor := int64(benchRunEvents)
		w := &discardFlusher{header: http.Header{}}
		if !api.drain(w, w, "big", 500, &cursor) || w.frames != 0 {
			b.Fatalf("steady-state poll wrote %d frames", w.frames)
		}
	}
	// A steady-state poll is one LIMIT-ed EventsPage after the head. Loading
	// the journal would cost at least one allocation per row (100k+); a
	// bounded poll costs a fixed handful.
	if allocs := testing.AllocsPerRun(10, poll); allocs > 1000 {
		b.Fatalf("steady-state poll allocated %.0f times; it is reading history, not one bounded page", allocs)
	}

	b.Run("fresh_connect", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			serve("")
		}
	})
	b.Run("fresh_connect_status_only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, err := reader.Status("big", time.Now())
			benchFatal(b, err)
		}
	})
	b.Run("fresh_connect_latest_sequence_only", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			_, err := reader.LatestSequence("big")
			benchFatal(b, err)
		}
	})
	b.Run("resume_mid_journal_50k_events", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			serve(strconv.Itoa(benchResumeFrom))
		}
	})
	b.Run("steady_state_poll", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			poll()
		}
	})
}
