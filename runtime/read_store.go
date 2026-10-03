package runtime

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"
)

// ReadStore exposes projections only. Its private SQLite handle is opened in
// mode=ro with query_only on every connection; no migrations or repairs run.
type ReadStore struct {
	store    *SQLiteOperationStore
	stateDir string
}

func OpenReadStore(stateDir string) (*ReadStore, error) {
	if stateDir == "" {
		return nil, fmt.Errorf("state directory is required")
	}
	path, err := filepath.Abs(filepath.Join(stateDir, "runtime.db"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", sqliteFileURI(path)+"?mode=ro&_pragma=query_only(on)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	var version int
	if err = db.QueryRow(`PRAGMA user_version`).Scan(&version); err == nil && version != sqliteSchemaVersion {
		err = fmt.Errorf("read store requires schema %d; found %d", sqliteSchemaVersion, version)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &ReadStore{store: &SQLiteOperationStore{db: db}, stateDir: stateDir}, nil
}
func (s *ReadStore) Close() error { return s.store.Close() }
func (s *ReadStore) Fleet(now time.Time) (Fleet, error) {
	f, err := FleetStatus(s.store, s.stateDir, 0, now)
	if f.Runs == nil {
		f.Runs = []RunSummary{}
	}
	return f, err
}

type readClock struct{ at time.Time }

func (c readClock) Now() time.Time { return c.at }
func (s *ReadStore) Status(id string, now time.Time) (StatusReport, error) {
	// No configured provider, scheduler, artifacts, credentials, or mutation
	// ports are created to obtain a status projection.
	r := &EngineeringRuntime{deps: Dependencies{Store: s.store, StateDir: s.stateDir, Clock: readClock{now}}}
	return r.Status(id)
}
func (s *ReadStore) HasRun(id string) (bool, error) { _, ok, err := s.store.Run(id); return ok, err }
func (s *ReadStore) EventsPage(id string, after int64, limit int) ([]EngineeringEvent, bool, error) {
	return s.store.EventsPage(id, after, limit)
}
func (s *ReadStore) LatestSequence(id string) (int64, error) { return s.store.LatestSequence(id) }
func (s *ReadStore) Controller(root string, observe func() (LiveControllerSnapshot, error), now time.Time) (ControllerStatus, error) {
	return DescribeControllerStatus(s.store, root, observe, now)
}
