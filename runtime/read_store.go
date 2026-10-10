package runtime

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/product"
	"github.com/bogdaniel/zenchron-engineering/review"
)

// ReadStore exposes projections only. Its private SQLite handle is opened in
// mode=ro with query_only on every connection; no migrations or repairs run.
type ReadStore struct {
	store    *SQLiteOperationStore
	stateDir string
	fleet    summaryCache
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
	f, err := fleetStatus(s.store, s.stateDir, 0, now, &s.fleet)
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

// PlanView is the durable plan view - no assignment resolution; see
// PlanService.DurableView.
func (s *ReadStore) PlanView(id string, revision int) (PlanView, error) {
	return PlanService{Store: s.store}.DurableView(id, revision)
}
func (s *ReadStore) PlanEvents(id string) ([]EngineeringEvent, error) {
	return s.store.PlanEvents(id)
}
func (s *ReadStore) HasRun(id string) (bool, error) { _, ok, err := s.store.Run(id); return ok, err }
func (s *ReadStore) EventsPage(id string, after int64, limit int) ([]EngineeringEvent, bool, error) {
	return s.store.EventsPage(id, after, limit)
}
func (s *ReadStore) LatestSequence(id string) (int64, error) { return s.store.LatestSequence(id) }
func (s *ReadStore) Controller(root string, observe func() (LiveControllerSnapshot, error), now time.Time) (ControllerStatus, error) {
	return DescribeControllerStatus(s.store, root, observe, now)
}

// WorkGraphs lists one bounded, database-paged slice of every WorkGraph's
// current revision (#472). hasMore is true when a page beyond this one
// exists.
func (s *ReadStore) WorkGraphs(offset, limit int) ([]orchestration.WorkGraph, bool, error) {
	return s.store.WorkGraphsPage(offset, limit)
}

// WorkGraph reads one WorkGraph's current revision (#472).
func (s *ReadStore) WorkGraph(graphID string) (orchestration.WorkGraph, bool, error) {
	return s.store.WorkGraph(graphID)
}

// WorkGraphStatus projects one WorkGraph (#472), including every unit hold
// #508 currently has open - the same WorkGraphHolds read the CLI's own
// `autonomy workgraph status` already uses, so a not-yet-activated unit
// gated by an unresolved operator hold reads identically here.
func (s *ReadStore) WorkGraphStatus(graphID string, now time.Time) (WorkGraphView, error) {
	holds, err := s.store.WorkGraphHolds(graphID)
	if err != nil {
		return WorkGraphView{}, err
	}
	return WorkGraphStatus(s.store, s.stateDir, graphID, now, holds)
}

// OrchestrationStatus projects one batch's open decisions and other #473
// typed messages (#470/#473).
func (s *ReadStore) OrchestrationStatus(batchID string, now time.Time) (OrchestrationView, error) {
	return OrchestrationStatus(s.store, s.stateDir, batchID, now)
}

// DecisionResolution reads the durable #508 resolution of one decision
// request by the request's own id, when one has been written. A #473
// DecisionRequest message has no resolved/open flag of its own - this is the
// one place that fact actually lives.
func (s *ReadStore) DecisionResolution(requestID string) (orchestration.DecisionResolution, bool, error) {
	return s.store.DecisionResolutionByRequestID(requestID)
}

// Products lists one bounded, database-paged slice of every Product's
// current revision (#476). hasMore is true when a page beyond this one
// exists.
func (s *ReadStore) Products(offset, limit int) ([]product.Product, bool, error) {
	return s.store.Products(offset, limit)
}

// CurrentProduct reads a Product's current revision (#476).
func (s *ReadStore) CurrentProduct(productID string) (product.Product, bool, error) {
	return s.store.CurrentProduct(productID)
}

// CurrentConfiguration reads a Product's current configuration revision
// (#476). found is false for a product with no configuration adopted yet.
func (s *ReadStore) CurrentConfiguration(productID string) (product.ProductConfiguration, bool, error) {
	return s.store.CurrentConfiguration(productID)
}

// AssociatedProduct reads which Product, if any, a WorkGraph is associated
// with (#476).
func (s *ReadStore) AssociatedProduct(graphID string) (string, bool, error) {
	return s.store.AssociatedProduct(graphID)
}

// AssociatedGraphs reads one bounded, database-paged slice of WorkGraph ids
// associated with a Product (#476). hasMore is true when a page beyond this
// one exists - a 201st association is reported, never silently dropped.
func (s *ReadStore) AssociatedGraphs(productID string, offset, limit int) ([]string, bool, error) {
	return s.store.AssociatedGraphs(productID, offset, limit)
}

// LatestReviewDecision reads the latest independent review decision for one
// pull request (#474), the same durable fact WorkGraphStatus's own
// ReviewApproved predicate already reads.
func (s *ReadStore) LatestReviewDecision(repository string, prNumber int) (review.Decision, bool, error) {
	return s.store.LatestReviewDecision(repository, prNumber)
}
