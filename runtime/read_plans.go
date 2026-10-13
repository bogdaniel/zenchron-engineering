package runtime

import (
	"database/sql"
	"errors"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

type PlanPageItem struct {
	PlanSummary
	Repository string
	Issue      int
}

// PlanSummariesPage bounds document reads before replaying each visible plan.
// Lifecycle meaning stays with the same summarizer the fleet already uses.
func (s *ReadStore) PlanSummariesPage(offset, limit int) ([]PlanPageItem, bool, error) {
	if offset < 0 || limit < 1 || limit > 500 {
		return nil, false, errors.New("invalid plan page")
	}
	rows, err := s.store.db.Query(`SELECT r.document, p.id, p.current_revision, p.repository, p.source_issue FROM plans p
		LEFT JOIN plan_revisions r ON r.plan_id = p.id AND r.revision = p.current_revision
		ORDER BY p.created_unix_nano ASC, p.id ASC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var plans []domain.EngineeringPlan
	var sources []PlanPageItem
	hasMore := false
	for rows.Next() {
		var document sql.NullString
		var source PlanPageItem
		if err := rows.Scan(&document, &source.PlanID, &source.Revision, &source.Repository, &source.Issue); err != nil {
			return nil, false, err
		}
		if len(plans) == limit {
			hasMore = true
			break
		}
		if source.Issue < 0 {
			return nil, false, errors.New("invalid plan source issue")
		}
		if source.Revision == 0 && !document.Valid {
			source.Error = "no plan revision recorded"
			sources = append(sources, source)
			plans = append(plans, domain.EngineeringPlan{})
			continue
		}
		if !document.Valid {
			return nil, false, errors.New("stored plan revision is missing")
		}
		plan, err := decodePlan(document.String)
		if err != nil {
			return nil, false, err
		}
		if source.Repository != plan.Subject.Repository || source.PlanID != plan.ID || source.Revision != plan.Revision {
			return nil, false, errors.New("plan identity differs from its stored document")
		}
		plans = append(plans, plan)
		sources = append(sources, source)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	for i, plan := range plans {
		if sources[i].Revision == 0 {
			continue
		}
		sources[i].PlanSummary = summarizePlan(s.store, plan)
	}
	return sources, hasMore, nil
}

func (s *ReadStore) PlanIdentity(id string) (PlanIdentity, bool, error) {
	var identity PlanIdentity
	err := s.store.db.QueryRow(`SELECT id, repository, current_revision, source_issue FROM plans WHERE id = ?`, id).Scan(&identity.PlanID, &identity.Repository, &identity.Revision, &identity.Issue)
	if errors.Is(err, sql.ErrNoRows) {
		return PlanIdentity{}, false, nil
	}
	return identity, err == nil, err
}

func (s *ReadStore) PlanSummary(id string) (PlanSummary, error) {
	plan, found, err := s.store.Plan(id)
	if err != nil {
		return PlanSummary{}, err
	}
	if !found {
		return PlanSummary{}, errors.New("plan not found")
	}
	return summarizePlan(s.store, plan), nil
}
