package runtime

import "fmt"

// PlanIdentitiesPage reads the existing plan index, including refused attempts.
// It creates neither projections nor an authoritative objective store.
func (s *ReadStore) PlanIdentitiesPage(offset, limit int, query string) ([]PlanIdentity, bool, error) {
	if offset < 0 || limit < 1 || limit > 50 || len(query) > 200 {
		return nil, false, fmt.Errorf("invalid plan page")
	}
	// ponytail: literal text search scans the index; fine below ~10k identities;
	// add an indexed search projection if measured operator latency requires it.
	rows, err := s.store.db.Query(`SELECT id, repository, current_revision, source_issue FROM plans
		WHERE ? = '' OR instr(lower(id), lower(?)) > 0 OR instr(lower(repository), lower(?)) > 0
		OR instr(CAST(source_issue AS TEXT), ?) > 0
		ORDER BY created_unix_nano DESC, id ASC LIMIT ? OFFSET ?`, query, query, query, query, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	identities := []PlanIdentity{}
	for rows.Next() {
		var identity PlanIdentity
		if err := rows.Scan(&identity.PlanID, &identity.Repository, &identity.Revision, &identity.Issue); err != nil {
			return nil, false, err
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(identities) > limit
	if more {
		identities = identities[:limit]
	}
	return identities, more, nil
}

// PlanEventsPage uses the existing journal index, including attempt-only
// identities. It checks canonical event documents through the shared reader.
func (s *ReadStore) PlanEventsPage(id string, after int64, limit int) ([]EngineeringEvent, bool, error) {
	if after < 0 || limit < 1 || limit > 500 {
		return nil, false, fmt.Errorf("invalid event page bounds")
	}
	events, err := queryStreamEventsLimited(s.store.db, `stream_kind = ? AND run_id = '' AND plan_id = ? AND sequence > ?`,
		` LIMIT ?`, streamPlan, id, after, limit+1)
	if err != nil {
		return nil, false, err
	}
	more := len(events) > limit
	if more {
		events = events[:limit]
	}
	return events, more, nil
}

func (s *ReadStore) HasPlanIdentity(id string) (bool, error) {
	var exists bool
	err := s.store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM plans WHERE id = ?)`, id).Scan(&exists)
	return exists, err
}
