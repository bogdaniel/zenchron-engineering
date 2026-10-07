package runtime

// WorkGraph persistence (#472). It is deliberately isolated in its own file and
// its own two tables: a graph revision document and a unit activation. Nothing
// here rewrites an existing migration, and nothing here is a second run
// database - the child runs, their journal, their operations and their leases
// stay exactly where #63 and #470 already keep them.

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// WorkUnitActivation is the durable claim that one unit's child run exists, and
// the exact upstream outputs it was created against. The second half is what
// makes stale downstream satisfaction detectable instead of permanent.
type WorkUnitActivation struct {
	GraphID      string
	UnitID       string
	BatchID      string
	RunID        string
	InputsDigest string
	ActivatedAt  time.Time
}

// AdoptWorkGraphRevision writes one revision ONCE. A revision already stored is
// not rewritten: the stored document is returned with adopted=false, which makes
// resubmitting a request whose reply was lost find the revision it already
// adopted. A different document under the same revision is a conflict, never an
// overwrite.
func (s *SQLiteOperationStore) AdoptWorkGraphRevision(graph orchestration.WorkGraph) (orchestration.WorkGraph, bool, error) {
	if err := graph.Validate(); err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	document, err := CanonicalJSON(graph)
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO work_graph_revisions (graph_id, revision, repository, created_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(graph_id, revision) DO NOTHING`,
		graph.ID, graph.Revision, graph.Repository, graph.CreatedAt.UnixNano(), string(document))
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	if inserted == 1 {
		return graph, true, nil
	}
	stored, found, err := s.WorkGraphRevision(graph.ID, graph.Revision)
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	if !found {
		return orchestration.WorkGraph{}, false, fmt.Errorf("work graph %s revision %d was neither inserted nor found", graph.ID, graph.Revision)
	}
	storedDigest, err := stored.RevisionDigest()
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	proposedDigest, err := graph.RevisionDigest()
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	if storedDigest != proposedDigest {
		return orchestration.WorkGraph{}, false, fmt.Errorf(
			"work graph %s revision %d is already adopted with different contents; a revision is immutable, so propose revision %d instead",
			graph.ID, graph.Revision, graph.Revision+1)
	}
	return stored, false, nil
}

// WorkGraphRevision reads one exact revision.
func (s *SQLiteOperationStore) WorkGraphRevision(graphID string, revision int) (orchestration.WorkGraph, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM work_graph_revisions WHERE graph_id = ? AND revision = ?`,
		graphID, revision).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WorkGraph{}, false, nil
	}
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	graph, err := decodeWorkGraph(document)
	return graph, err == nil, err
}

// WorkGraph reads a graph's CURRENT revision: the highest one adopted.
func (s *SQLiteOperationStore) WorkGraph(graphID string) (orchestration.WorkGraph, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM work_graph_revisions WHERE graph_id = ?
		ORDER BY revision DESC LIMIT 1`, graphID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WorkGraph{}, false, nil
	}
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	graph, err := decodeWorkGraph(document)
	return graph, err == nil, err
}

// WorkGraphs reads every graph's current revision, oldest graph first.
//
// ponytail: reads every graph's current revision each supervisor pass; fine for
// an operator's tens of graphs, index by "has an unactivated unit" if graphs ever
// number in the thousands.
func (s *SQLiteOperationStore) WorkGraphs() ([]orchestration.WorkGraph, error) {
	rows, err := s.db.Query(`SELECT document FROM work_graph_revisions AS current
		WHERE revision = (SELECT MAX(revision) FROM work_graph_revisions WHERE graph_id = current.graph_id)
		ORDER BY created_unix_nano ASC, graph_id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var graphs []orchestration.WorkGraph
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		graph, err := decodeWorkGraph(document)
		if err != nil {
			return nil, err
		}
		graphs = append(graphs, graph)
	}
	return graphs, rows.Err()
}

func decodeWorkGraph(document string) (orchestration.WorkGraph, error) {
	var graph orchestration.WorkGraph
	if err := strictJSON([]byte(document), &graph); err != nil {
		return orchestration.WorkGraph{}, fmt.Errorf("stored work graph is unreadable: %w", err)
	}
	if err := graph.Validate(); err != nil {
		return orchestration.WorkGraph{}, fmt.Errorf("stored work graph is corrupt: %w", err)
	}
	return graph, nil
}

// ActivateWorkUnit claims one unit's child run. It is insert-only, and the
// primary key is the whole idempotency story: the first activation stands, a
// second attempt returns false, and no replay or recovery can produce a second
// child run for a unit.
func (s *SQLiteOperationStore) ActivateWorkUnit(activation WorkUnitActivation) (bool, error) {
	for name, value := range map[string]string{
		"graph_id": activation.GraphID, "unit_id": activation.UnitID,
		"batch_id": activation.BatchID, "run_id": activation.RunID,
		"inputs_digest": activation.InputsDigest,
	} {
		if strings.TrimSpace(value) == "" {
			return false, fmt.Errorf("a work unit activation needs its %s", name)
		}
	}
	if activation.ActivatedAt.IsZero() {
		return false, errors.New("a work unit activation needs its activation time")
	}
	result, err := s.db.Exec(`INSERT INTO work_graph_activations
		(graph_id, unit_id, batch_id, run_id, inputs_digest, activated_unix_nano)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(graph_id, unit_id) DO NOTHING`,
		activation.GraphID, activation.UnitID, activation.BatchID, activation.RunID,
		activation.InputsDigest, activation.ActivatedAt.UnixNano())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	return inserted == 1, err
}

// WorkUnitActivations reads one graph's activations by unit id.
func (s *SQLiteOperationStore) WorkUnitActivations(graphID string) (map[string]WorkUnitActivation, error) {
	return queryWorkUnitActivations(s.db, graphID)
}

func queryWorkUnitActivations(q eventQuerier, graphID string) (map[string]WorkUnitActivation, error) {
	rows, err := q.Query(`SELECT unit_id, batch_id, run_id, inputs_digest, activated_unix_nano
		FROM work_graph_activations WHERE graph_id = ?`, graphID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	activations := map[string]WorkUnitActivation{}
	for rows.Next() {
		activation := WorkUnitActivation{GraphID: graphID}
		var activated int64
		if err := rows.Scan(&activation.UnitID, &activation.BatchID, &activation.RunID,
			&activation.InputsDigest, &activated); err != nil {
			return nil, err
		}
		activation.ActivatedAt = time.Unix(0, activated).UTC()
		activations[activation.UnitID] = activation
	}
	return activations, rows.Err()
}
