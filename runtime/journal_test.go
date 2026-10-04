package runtime

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

func newJournalRun(id string) EngineeringRun {
	at := time.Unix(100, 0).UTC()
	return EngineeringRun{
		SchemaVersion:    SchemaVersion,
		ID:               id,
		Repository:       "github.com/example/repo",
		Goal:             "close the journal gap",
		Phase:            Execute,
		Disposition:      Active,
		Base:             Ref{ID: "base", Revision: strings.Repeat("1", 40)},
		Candidate:        Candidate{Branch: "candidate", Revision: strings.Repeat("2", 40), Tree: strings.Repeat("3", 40)},
		Contract:         Ref{ID: "contract", Revision: "1"},
		ControllerSHA256: strings.Repeat("4", 64),
		CreatedAt:        at,
		UpdatedAt:        at,
	}
}

// journalFixture exercises every recorded field: an operation lifecycle payload,
// artifact references (raw local-only alongside a sanitized candidate), and a
// run disposition the reducer must fold.
func journalFixture(t *testing.T, runID string) []EngineeringEvent {
	t.Helper()
	operation, err := json.Marshal(RunOperation{SchemaVersion: SchemaVersion, ID: "op-1", RunID: runID, Kind: "provider", IdempotencyKey: "a", State: Pending, MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := json.Marshal(map[string]string{"reason": "merged"})
	if err != nil {
		t.Fatal(err)
	}
	return []EngineeringEvent{
		{SchemaVersion: SchemaVersion, ID: "e-1", RunID: runID, Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()},
		{SchemaVersion: SchemaVersion, ID: "e-2", RunID: runID, Type: EventOperationPlanned, OperationID: "op-1", OccurredAt: time.Unix(101, 0).UTC(), Payload: operation},
		{SchemaVersion: SchemaVersion, ID: "e-3", RunID: runID, Type: EventAssuranceObserved, OccurredAt: time.Unix(102, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain", LocalOnly: true},
			{Path: "/state/verify.sanitized-candidate.log", SHA256: strings.Repeat("b", 64), MediaType: "text/plain", Sanitized: true},
		}},
		{SchemaVersion: SchemaVersion, ID: "e-4", RunID: runID, Type: EventRunCompleted, OccurredAt: time.Unix(103, 0).UTC(), Payload: completed},
	}
}

func openJournal(t *testing.T) (string, *SQLiteOperationStore) {
	t.Helper()
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if err := store.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	return dir, store
}

// rawJournalDB bypasses the store to model corruption or a foreign writer.
func rawJournalDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", sqliteFileURI(filepath.Join(dir, "runtime.db"))+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestJournalReplayIsIdenticalAfterReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	run := newJournalRun("r")
	if err := store.PutRun(run); err != nil {
		t.Fatal(err)
	}
	appended := []EngineeringEvent{}
	for _, e := range journalFixture(t, "r") {
		stored, err := store.AppendEvent(e)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Sequence != int64(len(appended)+1) {
			t.Fatalf("expected sequence %d, got %d", len(appended)+1, stored.Sequence)
		}
		if n := len(appended); n > 0 && (stored.PreviousEventID != appended[n-1].ID || stored.PreviousEventHash != appended[n-1].EventHash) {
			t.Fatalf("event %q is not linked to its predecessor: %+v", stored.ID, stored)
		}
		if n := len(appended); n > 0 && stored.StateBefore != appended[n-1].StateAfter {
			t.Fatalf("state_before of %q does not continue the previous state_after", stored.ID)
		}
		appended = append(appended, stored)
	}
	live, err := store.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if live.StateSHA256 != restored.StateSHA256 {
		t.Fatalf("state digest changed across reopen: %q vs %q", live.StateSHA256, restored.StateSHA256)
	}
	before, err := Digest(live)
	if err != nil {
		t.Fatal(err)
	}
	after, err := Digest(restored)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("replayed snapshot changed across reopen: %q vs %q", before, after)
	}
	last := appended[len(appended)-1]
	if last.StateAfter != restored.StateSHA256 {
		t.Fatalf("last event state_after %q is not the replayed state %q", last.StateAfter, restored.StateSHA256)
	}
	if restored.Cursor != (Cursor{LastSequence: int64(len(appended)), LastEventID: last.ID, LastEventHash: last.EventHash}) {
		t.Fatalf("journal cursor was not rebuilt: %+v", restored.Cursor)
	}
	if restored.Disposition != Completed || restored.Reason != "merged" {
		t.Fatalf("run disposition was not rebuilt: %q %q", restored.Disposition, restored.Reason)
	}
	if len(restored.Operations) != 1 || restored.Operations["op-1"].Kind != "provider" {
		t.Fatalf("operation state was not rebuilt: %+v", restored.Operations)
	}
	if len(restored.Artifacts) != 2 || restored.Artifacts[0].Path != "/state/verify.raw.log" {
		t.Fatalf("artifact references were not rebuilt: %+v", restored.Artifacts)
	}
	stored, ok, err := reopened.Run("r")
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if stored.Repository != run.Repository || stored.Contract != run.Contract || stored.Base != run.Base || stored.Candidate != run.Candidate {
		t.Fatalf("run subject/contract binding did not survive reopen: %+v", stored)
	}
}

func TestJournalRefusesBrokenChain(t *testing.T) {
	for _, tamper := range []struct {
		name  string
		apply func(t *testing.T, db *sql.DB)
	}{
		{"chain column", func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE events SET previous_event_hash = ? WHERE run_id = 'r' AND sequence = 3`, strings.Repeat("0", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{"event hash column", func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE events SET event_hash = ? WHERE run_id = 'r' AND sequence = 2`, strings.Repeat("0", 64)); err != nil {
				t.Fatal(err)
			}
		}},
		{"document and column together", func(t *testing.T, db *sql.DB) {
			var document string
			if err := db.QueryRow(`SELECT document FROM events WHERE run_id = 'r' AND sequence = 3`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			var e EngineeringEvent
			if err := json.Unmarshal([]byte(document), &e); err != nil {
				t.Fatal(err)
			}
			e.PreviousEventHash = strings.Repeat("0", 64)
			tampered, err := json.Marshal(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE events SET document = ?, previous_event_hash = ? WHERE run_id = 'r' AND sequence = 3`, string(tampered), e.PreviousEventHash); err != nil {
				t.Fatal(err)
			}
		}},
		// state_after is diagnostic, never authoritative (#453), but it is
		// inside the event document its event_hash covers: rewriting it
		// without recomputing that hash is still corruption. The tip is tampered so
		// only its own event_hash, not a successor's link, can catch it.
		{"state_after without its event hash", func(t *testing.T, db *sql.DB) {
			var document string
			if err := db.QueryRow(`SELECT document FROM events WHERE run_id = 'r' AND sequence = 4`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			var e EngineeringEvent
			if err := json.Unmarshal([]byte(document), &e); err != nil {
				t.Fatal(err)
			}
			e.StateAfter = strings.Repeat("0", 64)
			tampered, err := CanonicalJSON(e)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(`UPDATE events SET document = ?, state_after = ? WHERE run_id = 'r' AND sequence = 4`, string(tampered), e.StateAfter); err != nil {
				t.Fatal(err)
			}
		}},
		{"payload body", func(t *testing.T, db *sql.DB) {
			var document string
			if err := db.QueryRow(`SELECT document FROM events WHERE run_id = 'r' AND sequence = 4`).Scan(&document); err != nil {
				t.Fatal(err)
			}
			tampered := strings.Replace(document, `"merged"`, `"forged"`, 1)
			if tampered == document {
				t.Fatal("fixture payload did not contain the expected reason")
			}
			if _, err := db.Exec(`UPDATE events SET document = ? WHERE run_id = 'r' AND sequence = 4`, tampered); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			dir := t.TempDir()
			store, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutRun(newJournalRun("r")); err != nil {
				t.Fatal(err)
			}
			for _, e := range journalFixture(t, "r") {
				if _, err := store.AppendEvent(e); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			tamper.apply(t, rawJournalDB(t, dir))

			reopened, err := OpenSQLiteOperationStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			snapshot, err := reopened.Replay("r")
			if err == nil {
				t.Fatalf("tampered journal replayed successfully: %+v", snapshot)
			}
			// A further append must not build on an unverifiable chain either.
			if _, err := reopened.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-5", RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(104, 0).UTC()}); err == nil {
				t.Fatal("append accepted a tampered journal as its predecessor")
			}
		})
	}
}

func TestJournalRefusesDuplicateSequence(t *testing.T) {
	dir, store := openJournal(t)
	first, err := store.AppendEvent(journalFixture(t, "r")[0])
	if err != nil {
		t.Fatal(err)
	}
	// The database, not the caller, is what refuses a duplicate sequence.
	_, err = rawJournalDB(t, dir).Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, '', '', '', '', '', '', '{}')`,
		"forged", first.RunID, first.Sequence, first.Type)
	if err == nil {
		t.Fatal("a duplicate (run_id, sequence) was accepted")
	}
	if !strings.Contains(strings.ToUpper(err.Error()), "UNIQUE") {
		t.Fatalf("expected a UNIQUE constraint refusal, got %v", err)
	}
	// The same row id is refused too, so an event id is never reused.
	_, err = rawJournalDB(t, dir).Exec(`INSERT INTO events (`+sqliteEventColumns+`) VALUES (?, ?, ?, ?, '', '', '', '', '', '', '{}')`,
		first.ID, first.RunID, int64(99), first.Type)
	if err == nil {
		t.Fatal("a duplicate event id was accepted")
	}
	// A caller may not choose its own sequence or chain link.
	chosen := journalFixture(t, "r")[1]
	chosen.Sequence = 1
	if _, err := store.AppendEvent(chosen); err == nil {
		t.Fatal("a caller-chosen sequence was accepted")
	}
	events, err := store.Events("r")
	if err != nil || len(events) != 1 {
		t.Fatalf("expected exactly the one appended event, got %d (%v)", len(events), err)
	}
}

func TestJournalSequenceAllocationIsMonotonicAcrossHandles(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := first.PutRun(newJournalRun("r")); err != nil {
		t.Fatal(err)
	}
	const perHandle = 8
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[int64]string{}
	for handle, store := range []*SQLiteOperationStore{first, second} {
		for i := 0; i < perHandle; i++ {
			wg.Add(1)
			go func(store *SQLiteOperationStore, id string) {
				defer wg.Done()
				stored, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: id, RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(200, 0).UTC()})
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if other, duplicate := seen[stored.Sequence]; duplicate {
					t.Errorf("sequence %d allocated to both %q and %q", stored.Sequence, other, id)
					return
				}
				seen[stored.Sequence] = id
			}(store, fmt.Sprintf("e-%d-%d", handle, i))
		}
	}
	wg.Wait()
	total := int64(2 * perHandle)
	if int64(len(seen)) != total {
		t.Fatalf("expected %d distinct sequences, got %d", total, len(seen))
	}
	for n := int64(1); n <= total; n++ {
		if seen[n] == "" {
			t.Fatalf("sequence %d was never allocated: gap in %v", n, seen)
		}
	}
	// Replaying through the reducer proves the chain survived the race.
	snapshot, err := second.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Cursor.LastSequence != total {
		t.Fatalf("expected cursor at %d, got %+v", total, snapshot.Cursor)
	}
}

func TestJournalRefusesRawTranscriptBody(t *testing.T) {
	_, store := openJournal(t)
	body := "github_pat_fixturesecret\n+ go test ./...\n"
	inline, err := json.Marshal(map[string]any{"provider": "codex", "transcript": body})
	if err != nil {
		t.Fatal(err)
	}
	nested, err := json.Marshal(map[string]any{"attempts": []any{map[string]any{"stderr": body}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, refused := range []struct {
		name  string
		event EngineeringEvent
	}{
		{"inline transcript", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-x", RunID: "r", Type: EventCandidateChanged, OccurredAt: time.Unix(105, 0).UTC(), Payload: inline}},
		{"nested stderr", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-y", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Payload: nested}},
		{"raw artifact that is not local-only", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-z", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain"},
		}}},
		{"raw artifact marked publishable", EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-w", RunID: "r", Type: EventAssuranceObserved, OccurredAt: time.Unix(105, 0).UTC(), Artifacts: []Artifact{
			{Path: "/state/verify.raw.log", SHA256: strings.Repeat("a", 64), MediaType: "text/plain", LocalOnly: true, Publishable: true},
		}}},
	} {
		if _, err := store.AppendEvent(refused.event); err == nil {
			t.Fatalf("%s was appended to a canonical event row", refused.name)
		}
	}
	events, err := store.Events("r")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("a refused append still wrote %d rows", len(events))
	}
	// The reference form of the same transcript is what the journal accepts.
	if _, err := store.AppendEvent(journalFixture(t, "r")[2]); err != nil {
		t.Fatalf("an artifact reference must be appendable: %v", err)
	}
}

func TestJournalRefusesEventsForAnUnknownRun(t *testing.T) {
	_, store := openJournal(t)
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "other", Type: EventRunCreated, OccurredAt: time.Unix(100, 0).UTC()}); err == nil {
		t.Fatal("an event was appended to a run that does not exist")
	}
	if _, err := store.AppendEvent(EngineeringEvent{SchemaVersion: SchemaVersion, ID: "e-1", RunID: "r", Type: "run.invented", OccurredAt: time.Unix(100, 0).UTC()}); err == nil {
		t.Fatal("an event outside the catalogue was appended")
	}
}

// rewriteStateDigests sets every selected event's state_before/state_after to
// an arbitrary value and recomputes the hash chain over the result, as a writer
// able to rewrite a whole suffix could. It returns the rewritten events.
func rewriteStateDigests(t *testing.T, db *sql.DB, where string, args ...any) []EngineeringEvent {
	t.Helper()
	rows, err := db.Query(`SELECT document FROM events WHERE `+where+` ORDER BY sequence ASC`, args...)
	if err != nil {
		t.Fatal(err)
	}
	var events []EngineeringEvent
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			t.Fatal(err)
		}
		var e EngineeringEvent
		if err := json.Unmarshal([]byte(document), &e); err != nil {
			t.Fatal(err)
		}
		events = append(events, e)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 {
		t.Fatalf("fixture selected %d events; a chain needs at least two", len(events))
	}
	for i, e := range events {
		e.StateBefore = fmt.Sprintf("arbitrary-before-%d", i)
		e.StateAfter = strings.Repeat(fmt.Sprint(i%10), 64)
		if i > 0 {
			e.PreviousEventHash = events[i-1].EventHash
		}
		if e.EventHash, err = EventDigest(e); err != nil {
			t.Fatal(err)
		}
		events[i] = e
		canonical, err := CanonicalJSON(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE events SET document = ?, state_before = ?, state_after = ?, previous_event_hash = ?, event_hash = ? WHERE id = ?`,
			string(canonical), e.StateBefore, e.StateAfter, e.PreviousEventHash, e.EventHash, e.ID); err != nil {
			t.Fatal(err)
		}
	}
	return events
}

// TestJournalStateDigestsAreNotAuthoritative pins #453's frozen decision:
// state_before/state_after are recorded transition digests kept as diagnostic
// compatibility metadata. A journal whose state fields are arbitrary, but whose
// event documents and hash chain are internally consistent, replays to exactly
// the state its events fold to - nothing in replay consults those fields.
func TestJournalStateDigestsAreNotAuthoritative(t *testing.T) {
	dir, store := openJournal(t)
	for _, e := range journalFixture(t, "r") {
		if _, err := store.AppendEvent(e); err != nil {
			t.Fatal(err)
		}
	}
	honest, err := store.Replay("r")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	events := rewriteStateDigests(t, rawJournalDB(t, dir), `run_id = ?`, "r")
	tip := events[len(events)-1]

	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	rewritten, err := reopened.Replay("r")
	if err != nil {
		t.Fatalf("a chain-consistent journal with arbitrary state fields was refused: %v", err)
	}
	if rewritten.StateSHA256 != honest.StateSHA256 {
		t.Fatalf("replayed state depends on state_before/state_after: %s vs %s", rewritten.StateSHA256, honest.StateSHA256)
	}
	if rewritten.StateSHA256 == tip.StateAfter {
		t.Fatal("fixture did not make the recorded state_after disagree with replay")
	}
	// The cursor names the rewritten tip, because the event hash covers the
	// fields; everything else about the run is unchanged.
	if rewritten.Cursor != (Cursor{LastSequence: int64(len(events)), LastEventID: tip.ID, LastEventHash: tip.EventHash}) {
		t.Fatalf("cursor does not name the rewritten tip: %+v", rewritten.Cursor)
	}
}

// TestPlanJournalStateDigestsAreNotAuthoritative is the same contract for the
// plan stream: ReplayPlan folds the events, never the recorded state digests.
func TestPlanJournalStateDigestsAreNotAuthoritative(t *testing.T) {
	dir, store := openPlanStore(t)
	plan := planFixture(t, "plan-453", 1)
	if _, err := store.ClaimPlan(plan, time.Now()); err != nil {
		t.Fatal(err)
	}
	appendPlan(t, store, planEvent(t, plan.ID, "p453-1", EventPlanProposed, proposedPayload(plan)))
	appendPlan(t, store, planEvent(t, plan.ID, "p453-2", EventPlanValidated, PlanValidatedPayload{
		Revision: 1, Digest: plan.Digest, Status: string(domain.ProposalValid),
	}))
	appendPlan(t, store, planEvent(t, plan.ID, "p453-3", EventPlanApproved, PlanDecisionPayload{
		Revision: 1, Digest: plan.Digest, Operator: "operator-1", Note: "reviewed",
	}))
	honest, err := store.ReplayPlan(plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	events := rewriteStateDigests(t, rawJournalDB(t, dir), `plan_id = ?`, plan.ID)
	tip := events[len(events)-1]

	rewritten, err := reopenStore(t, dir).ReplayPlan(plan.ID)
	if err != nil {
		t.Fatalf("a chain-consistent plan journal with arbitrary state fields was refused: %v", err)
	}
	if rewritten.StateSHA256 != honest.StateSHA256 {
		t.Fatalf("replayed plan state depends on state_before/state_after: %s vs %s", rewritten.StateSHA256, honest.StateSHA256)
	}
	if rewritten.StateSHA256 == tip.StateAfter {
		t.Fatal("fixture did not make the recorded state_after disagree with replay")
	}
	if rewritten.Cursor != (Cursor{LastSequence: int64(len(events)), LastEventID: tip.ID, LastEventHash: tip.EventHash}) {
		t.Fatalf("cursor does not name the rewritten tip: %+v", rewritten.Cursor)
	}
}

// stateDigestReaders are the only production declarations that may touch an
// event's state_before/state_after, keyed "path:declaration". Each one writes
// the fields, checks that the indexed columns agree with the document, renders
// them, or declares their columns. Replay, ReplayPlan and the reducers are
// deliberately absent: they must decide from the events alone (#453).
var stateDigestReaders = map[string]bool{
	"runtime/journal.go:appendToStream":                true, // allocates both
	"runtime/journal.go:AppendEvent":                   true, // run row insert
	"runtime/journal.go:queryStreamEventsLimited":      true, // column/document agreement
	"runtime/journal.go:sqliteEventColumns":            true,
	"runtime/journal.go:sqlitePlanEventColumns":        true,
	"runtime/plan_store.go:AppendPlanEvent":            true, // plan row insert
	"runtime/sqlite_store.go:sqliteMigrations":         true, // DDL
	"cmd/zenchron-engineering/operator.go:renderEvent": true, // display
}

// TestNoProductionCodeReadsStateDigestsAsAuthority is #453's search guard, at
// declaration granularity: any selector .StateBefore/.StateAfter, or any string
// literal naming state_before/state_after (raw SQL), outside the declarations
// above fails. A new reader must decide from replay instead. Struct tags and
// comments are not code and are not scanned.
func TestNoProductionCodeReadsStateDigestsAsAuthority(t *testing.T) {
	column := regexp.MustCompile(`state_(before|after)`)
	root := ".."
	fset := token.NewFileSet()
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == ".claude" || d.Name() == "testdata") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range parsed.Decls {
			var names []string
			var nodes []ast.Node
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				names, nodes = []string{decl.Name.Name}, []ast.Node{decl}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.ValueSpec:
						names = append(names, spec.Names[0].Name)
					case *ast.TypeSpec:
						names = append(names, spec.Name.Name)
					default:
						names = append(names, "import")
					}
					nodes = append(nodes, spec)
				}
			}
			for i, node := range nodes {
				key := rel + ":" + names[i]
				ast.Inspect(node, func(n ast.Node) bool {
					hit := false
					switch n := n.(type) {
					case *ast.Field:
						// A declaration and its json tag name the field; they
						// do not read it.
						return false
					case *ast.SelectorExpr:
						hit = n.Sel.Name == "StateBefore" || n.Sel.Name == "StateAfter"
					case *ast.BasicLit:
						hit = n.Kind == token.STRING && column.MatchString(n.Value)
					}
					if hit {
						seen[key] = true
						if !stateDigestReaders[key] {
							t.Errorf("%s (%s) touches an event's state_before/state_after; those are diagnostic metadata (#453), decide from replay instead", fset.Position(n.Pos()), key)
						}
					}
					return true
				})
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// A stale allowlist entry means the scan is no longer seeing what it
	// guards, or a writer moved: either way the list must be corrected.
	for key := range stateDigestReaders {
		if !seen[key] {
			t.Errorf("allowlisted %s no longer touches the state digests; remove or correct it", key)
		}
	}
}
