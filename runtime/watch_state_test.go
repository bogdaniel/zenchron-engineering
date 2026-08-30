package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// sampleWatchState is one fully populated observation: every field a watcher
// needs to resume, and no credential, because there is nowhere to put one.
func sampleWatchState(repository string, at time.Time) WatchState {
	return WatchState{
		Repository:          repository,
		Cursor:              at.Format(time.RFC3339Nano),
		ETag:                `W/"e0f1"`,
		LastSuccessAt:       at,
		NotBefore:           at.Add(2 * time.Minute),
		ConsecutiveFailures: 3,
		RateLimit: WatchRateLimit{
			Limit:      5000,
			Remaining:  4987,
			ResetAt:    at.Add(time.Hour),
			ObservedAt: at,
		},
		LastErrorClass:  WatchErrorRateLimited,
		LastErrorDetail: "secondary rate limit",
		LastErrorAt:     at.Add(-time.Minute),
	}
}

func requireWatchState(t *testing.T, store *SQLiteOperationStore, repository string) (WatchState, int64) {
	t.Helper()
	state, revision, ok, err := store.WatchStateFor(repository)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected durable watch state for %q", repository)
	}
	return state, revision
}

func assertWatchStateEqual(t *testing.T, got, want WatchState) {
	t.Helper()
	if got.Repository != want.Repository || got.Cursor != want.Cursor || got.ETag != want.ETag ||
		got.ConsecutiveFailures != want.ConsecutiveFailures || got.RateLimit.Limit != want.RateLimit.Limit ||
		got.RateLimit.Remaining != want.RateLimit.Remaining || got.LastErrorClass != want.LastErrorClass ||
		got.LastErrorDetail != want.LastErrorDetail {
		t.Fatalf("watch state did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	for _, pair := range []struct {
		name      string
		got, want time.Time
	}{
		{"last_success_at", got.LastSuccessAt, want.LastSuccessAt},
		{"not_before", got.NotBefore, want.NotBefore},
		{"rate_limit.reset_at", got.RateLimit.ResetAt, want.RateLimit.ResetAt},
		{"rate_limit.observed_at", got.RateLimit.ObservedAt, want.RateLimit.ObservedAt},
		{"last_error_at", got.LastErrorAt, want.LastErrorAt},
	} {
		if !pair.got.Equal(pair.want) {
			t.Fatalf("%s did not round-trip: got %s want %s", pair.name, pair.got, pair.want)
		}
	}
}

func TestWatchStateSurvivesCloseAndIndependentReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	written := sampleWatchState("owner/name", time.Unix(1700, 0).UTC())
	revision, ok, err := store.PutWatchState(written, 0)
	if err != nil || !ok || revision != 1 {
		t.Fatalf("create: %d %v %v", revision, ok, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, restoredRevision := requireWatchState(t, reopened, "owner/name")
	if restoredRevision != 1 {
		t.Fatalf("revision changed across reopen: %d", restoredRevision)
	}
	assertWatchStateEqual(t, restored, written)
	// A repository never polled has no state, and that is not an error: it is
	// the revision a create is expected at.
	_, revision, ok, err = reopened.WatchStateFor("owner/never-polled")
	if err != nil || ok || revision != 0 {
		t.Fatalf("expected absent state at revision 0, got ok=%v revision=%d err=%v", ok, revision, err)
	}
}

func TestWatchStateUpdatesAreNotLostAcrossHandles(t *testing.T) {
	_, storeA, storeB := openPair(t)
	at := time.Unix(1700, 0).UTC()
	// Two watchers both find the repository unpolled; exactly one creates it.
	firstRevision, createdByA, err := storeA.PutWatchState(sampleWatchState("owner/name", at), 0)
	if err != nil {
		t.Fatal(err)
	}
	_, createdByB, err := storeB.PutWatchState(sampleWatchState("owner/name", at.Add(time.Second)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !createdByA || createdByB {
		t.Fatalf("exactly one create must win: A=%v B=%v", createdByA, createdByB)
	}

	// Both now read the same revision and both decide to write.
	_, readByA := requireWatchState(t, storeA, "owner/name")
	stale, readByB := requireWatchState(t, storeB, "owner/name")
	if readByA != firstRevision || readByB != firstRevision {
		t.Fatalf("both handles must read revision %d, got %d and %d", firstRevision, readByA, readByB)
	}
	winner := sampleWatchState("owner/name", at.Add(time.Hour))
	winner.Cursor = "winner"
	winnerRevision, ok, err := storeA.PutWatchState(winner, readByA)
	if err != nil || !ok || winnerRevision != firstRevision+1 {
		t.Fatalf("the first writer must win: %d %v %v", winnerRevision, ok, err)
	}
	stale.Cursor = "loser"
	if _, ok, err := storeB.PutWatchState(stale, readByB); err != nil || ok {
		t.Fatalf("a stale update must be refused: ok=%v err=%v", ok, err)
	}
	// The loser overwrote nothing, and re-reading gives it the winner's state
	// at the revision it must now build on.
	after, afterRevision := requireWatchState(t, storeB, "owner/name")
	if after.Cursor != "winner" || afterRevision != winnerRevision {
		t.Fatalf("the winner's state was corrupted: %+v at revision %d", after, afterRevision)
	}
	assertWatchStateEqual(t, after, winner)

	// A repository identity is required; nothing writes an anonymous row.
	if _, _, err := storeA.PutWatchState(WatchState{}, 0); err == nil {
		t.Fatal("expected a refusal for watch state with no repository")
	}
}

// Configuration is the authority for WHICH repositories are watched. Leftover
// observation state for a repository the operator dropped is inert: the watch
// set comes from configuration, and there is no accessor that enumerates the
// table back into one.
func TestWatchStateDoesNotResurrectARepositoryDroppedFromConfig(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	at := time.Unix(1700, 0).UTC()
	for _, repository := range []string{"owner/kept", "owner/dropped"} {
		if _, ok, err := store.PutWatchState(sampleWatchState(repository, at), 0); err != nil || !ok {
			t.Fatalf("%s: %v %v", repository, ok, err)
		}
	}
	// The operator now enrols only one of them.
	config, _, err := LoadOperatorConfig(operatorConfigWithWatch(t, dir, `{"repositories": ["owner/kept"]}`))
	if err != nil {
		t.Fatal(err)
	}
	settings, err := config.WatchSettings()
	if err != nil {
		t.Fatal(err)
	}
	observed := []string{}
	for _, repo := range settings.Repositories {
		if _, _, ok, err := store.WatchStateFor(repo.String()); err != nil || !ok {
			t.Fatalf("%s: %v %v", repo, ok, err)
		}
		observed = append(observed, repo.String())
	}
	if fmt.Sprint(observed) != "[owner/kept]" {
		t.Fatalf("watch observed %v; configuration enrolled only owner/kept", observed)
	}
	// The dropped repository's row is still there and still inert: reaching it
	// takes an identity no configuration hands out any more.
	if _, _, ok, err := store.WatchStateFor("owner/dropped"); err != nil || !ok {
		t.Fatalf("the dropped row was expected to survive untouched: %v %v", ok, err)
	}
}

func TestWatchStateSchemaIsCoveredByTheNewerSchemaRefusal(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenSQLiteOperationStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != sqliteSchemaVersion || version != len(sqliteMigrations) {
		t.Fatalf("the watch migration must advance the schema version: %d of %d", version, len(sqliteMigrations))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Join(dir, "runtime.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, sqliteSchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = OpenSQLiteOperationStore(dir)
	var unsupported UnsupportedSchemaError
	if !errors.As(err, &unsupported) {
		t.Fatalf("a database one version newer must be refused, got %v", err)
	}
	if unsupported.Found != sqliteSchemaVersion+1 || unsupported.Supported != sqliteSchemaVersion {
		t.Fatalf("unexpected schema error detail: %+v", unsupported)
	}
}
