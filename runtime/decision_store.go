package runtime

// Durable, authorized DecisionResolution persistence (#508).
//
// Both tables are insert-only, exactly like the handoff and message tables
// beside them: a hold is placed once, keyed by its graph and unit alone, and
// a resolution is written once, keyed by the request it resolves alone. A
// second writer racing the first - two operators, or a retried request after
// a lost reply - finds the row already there rather than a free slot to
// write into; ResolveDecision (orchestration package) then decides whether
// that is an idempotent replay or a conflict to refuse.

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// PlaceWorkUnitHold writes one hold ONCE. A hold already stored for this
// graph and unit is not rewritten: it is returned with placed=false, which
// makes resubmitting a request whose reply was lost find the hold already
// there. A DIFFERENT hold under the same identity - the graph and unit are
// the whole identity, so this can only mean a second, disagreeing placement
// attempt - is a conflict, never an overwrite.
func (s *SQLiteOperationStore) PlaceWorkUnitHold(hold orchestration.WorkUnitHold) (orchestration.WorkUnitHold, bool, error) {
	if err := hold.Validate(); err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	document, err := CanonicalJSON(hold)
	if err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO work_unit_holds (id, graph_id, unit_id, requested_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		hold.ID, hold.GraphID, hold.UnitID, hold.RequestedAt.UnixNano(), string(document))
	if err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	if inserted == 1 {
		return hold, true, nil
	}
	stored, found, err := s.WorkUnitHoldByID(hold.ID)
	if err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	if !found {
		return orchestration.WorkUnitHold{}, false, fmt.Errorf("work unit hold %s was neither inserted nor found", hold.ID)
	}
	// Compared by IDENTITY CONTENT alone - Purpose and RequestedBy, both
	// plain strings - never by RequestedAt or a canonical-document byte
	// comparison that would include it (#508 review B4). RequestedAt is
	// stamped fresh by the clock on every call, including an identical
	// retry after a lost reply; comparing it would make that retry its own
	// conflict rather than the idempotent replay it is. Purpose and
	// RequestedBy are the only caller-stated content a hold has, and the
	// graph/unit/id are already proven equal by the lookup itself.
	if stored.Purpose != hold.Purpose || stored.RequestedBy != hold.RequestedBy {
		return orchestration.WorkUnitHold{}, false, fmt.Errorf(
			"work unit %s of graph %s already has a hold placed with different content; a hold is placed at most once", hold.UnitID, hold.GraphID)
	}
	return stored, false, nil
}

// WorkUnitHoldByID reads one hold by its own identity.
func (s *SQLiteOperationStore) WorkUnitHoldByID(id string) (orchestration.WorkUnitHold, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM work_unit_holds WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.WorkUnitHold{}, false, nil
	}
	if err != nil {
		return orchestration.WorkUnitHold{}, false, err
	}
	return decodeWorkUnitHold(document)
}

// WorkGraphHolds reports, for one graph, every unit a hold has been placed on
// that no durable resolution has yet lifted. It is the function wired as
// SupervisorDependencies.WorkUnitHolds: the ONLY source #508 supplies for
// #472's readiness-owner seam. A unit with no row here is simply not held;
// nothing else in this build can hold one.
func (s *SQLiteOperationStore) WorkGraphHolds(graphID string) (map[string]orchestration.DecisionWait, error) {
	rows, err := s.db.Query(`SELECT document FROM work_unit_holds WHERE graph_id = ?`, graphID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var holds []orchestration.WorkUnitHold
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		hold, _, err := decodeWorkUnitHold(document)
		if err != nil {
			return nil, err
		}
		holds = append(holds, hold)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	waits := map[string]orchestration.DecisionWait{}
	for _, hold := range holds {
		resolution, resolved, err := s.DecisionResolutionByRequestID(hold.ID)
		if err != nil {
			return nil, err
		}
		// ONLY an explicit allow lifts a hold (#508 review B2). A hold's own
		// Ref always prescribes an allow_deny outcome (orchestration.WorkUnitHold.Ref),
		// so "resolved but not allow" means exactly one thing: an authorized
		// deny. The unit stays held - never auto-run denied work - and an
		// unknown/malformed stored outcome fails the same way: closed, not
		// treated as permission.
		if resolved && resolution.Outcome.Kind == orchestration.DecisionAllowDeny && resolution.Outcome.Value == orchestration.DecisionAllow {
			continue
		}
		waits[hold.UnitID] = hold.DecisionWait()
	}
	return waits, nil
}

func decodeWorkUnitHold(document string) (orchestration.WorkUnitHold, bool, error) {
	var hold orchestration.WorkUnitHold
	if err := strictJSON([]byte(document), &hold); err != nil {
		return orchestration.WorkUnitHold{}, false, fmt.Errorf("stored work unit hold is unreadable: %w", err)
	}
	if err := hold.Validate(); err != nil {
		return orchestration.WorkUnitHold{}, false, fmt.Errorf("stored work unit hold is corrupt: %w", err)
	}
	return hold, true, nil
}

// InsertDecisionResolution writes one resolution ONCE, keyed by the request it
// resolves. It always returns the row that is durably stored for that request
// afterwards - inserted=true only when THIS call wrote it - so a caller can
// tell an idempotent replay (the stored row matches what it proposed) from a
// race it lost to a conflicting answer (it does not), exactly as
// orchestration.ResolveDecision decides which one happened.
func (s *SQLiteOperationStore) InsertDecisionResolution(resolution orchestration.DecisionResolution) (orchestration.DecisionResolution, bool, error) {
	if err := resolution.Validate(); err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	document, err := CanonicalJSON(resolution)
	if err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	result, err := s.db.Exec(`INSERT INTO decision_resolutions (id, request_id, scope, resolved_unix_nano, document)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		resolution.ID, resolution.RequestID, resolution.Scope, resolution.ResolvedAt.UnixNano(), string(document))
	if err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	if inserted == 1 {
		return resolution, true, nil
	}
	stored, found, err := s.DecisionResolutionByRequestID(resolution.RequestID)
	if err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	if !found {
		return orchestration.DecisionResolution{}, false, fmt.Errorf("decision resolution for request %s was neither inserted nor found", resolution.RequestID)
	}
	return stored, false, nil
}

// DecisionResolutionByRequestID reads the resolution already durably stored
// for one request, if any.
func (s *SQLiteOperationStore) DecisionResolutionByRequestID(requestID string) (orchestration.DecisionResolution, bool, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM decision_resolutions WHERE request_id = ?`, requestID).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.DecisionResolution{}, false, nil
	}
	if err != nil {
		return orchestration.DecisionResolution{}, false, err
	}
	var resolution orchestration.DecisionResolution
	if err := strictJSON([]byte(document), &resolution); err != nil {
		return orchestration.DecisionResolution{}, false, fmt.Errorf("stored decision resolution is unreadable: %w", err)
	}
	if err := resolution.Validate(); err != nil {
		return orchestration.DecisionResolution{}, false, fmt.Errorf("stored decision resolution is corrupt: %w", err)
	}
	return resolution, true, nil
}

// OpenDecisionRequestsForRun is #508's run-level wait eligibility: every live
// (not superseded), not yet resolved #473 decision_request message THIS run
// itself admitted. Reconcile holds a run with any open here - plans no
// further operation, so it spends no provider process or scheduler slot -
// until each one resolves. A worker cannot shorten this list: nothing it can
// write resolves a request, and the set is read fresh every pass from durable
// state alone.
func (s *SQLiteOperationStore) OpenDecisionRequestsForRun(runID string) ([]orchestration.EngineeringMessage, error) {
	rows, err := s.db.Query(`SELECT document FROM orchestration_messages WHERE run_id = ?`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var messages []orchestration.EngineeringMessage
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var message orchestration.EngineeringMessage
		if err := strictJSON([]byte(document), &message); err != nil {
			return nil, fmt.Errorf("stored message is unreadable: %w", err)
		}
		if err := message.Validate(); err != nil {
			return nil, fmt.Errorf("stored message is corrupt: %w", err)
		}
		messages = append(messages, message)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Live is computed over THIS run's own full message set: a draft can only
	// supersede a prior one from the same unit of the same scope, and a
	// run's items are all one unit, so every superseding reference is already
	// present in what was just read.
	var open []orchestration.EngineeringMessage
	for _, message := range orchestration.Live(messages) {
		if message.Kind != orchestration.KindDecisionRequest {
			continue
		}
		if _, resolved, err := s.DecisionResolutionByRequestID(message.ID); err != nil {
			return nil, err
		} else if !resolved {
			open = append(open, message)
		}
	}
	return open, nil
}
