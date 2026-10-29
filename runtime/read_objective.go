package runtime

import (
	"errors"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// ObjectiveGraph reads the existing explicit plan binding. Multiple matches
// are corruption, never a choice the experience layer may make.
func (s *ReadStore) ObjectiveGraph(planID string) (orchestration.WorkGraph, bool, error) {
	// ponytail: JSON binding scan is fine for hundreds of graphs; index the binding if lookups grow.
	rows, err := s.store.db.Query(`SELECT document FROM work_graph_revisions AS current
		WHERE revision = (SELECT MAX(revision) FROM work_graph_revisions WHERE graph_id = current.graph_id)
		AND json_extract(document, '$.objective_plan.id') = ? LIMIT 2`, planID)
	if err != nil {
		return orchestration.WorkGraph{}, false, err
	}
	defer rows.Close()
	var result orchestration.WorkGraph
	found := false
	for rows.Next() {
		if found {
			return orchestration.WorkGraph{}, false, errors.New("multiple graphs bind the objective plan")
		}
		var document string
		if err := rows.Scan(&document); err != nil {
			return orchestration.WorkGraph{}, false, err
		}
		result, err = decodeWorkGraph(document)
		if err != nil {
			return orchestration.WorkGraph{}, false, err
		}
		found = true
	}
	return result, found, rows.Err()
}
