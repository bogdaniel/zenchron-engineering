package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// AdmitMessages persists one invocation's admitted messages all-or-nothing.
// It is insert-only: a message already admitted under its identity is left
// exactly as it was, so replaying an admission pass duplicates nothing. The
// schema refuses a second correction of one record, so two passes racing to
// supersede it cannot both succeed.
func (s *SQLiteOperationStore) AdmitMessages(messages []orchestration.EngineeringMessage) error {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, message := range messages {
		if err := message.Validate(); err != nil {
			return err
		}
		document, err := CanonicalJSON(message)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO orchestration_messages
			(id, scope, run_id, operation_id, attempt, supersedes, admitted_unix_nano, document)
			VALUES (?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?) ON CONFLICT(id) DO NOTHING`,
			message.ID, message.Scope, message.Source.RunID, message.Source.OperationID, message.Source.Attempt,
			message.Supersedes, message.AdmittedAt.UnixNano(), string(document)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ScopeMessages reads one scope's admitted messages in admission order.
func (s *SQLiteOperationStore) ScopeMessages(scope string) ([]orchestration.EngineeringMessage, error) {
	return queryScopeMessages(s.db, scope)
}

// MessageByID reads one admitted message by its own identity, regardless of
// scope. It is how a decision resolution (#508) finds the exact
// worker-authored decision_request it is asked to resolve, without the
// resolver needing to already know which scope it lives in.
func (s *SQLiteOperationStore) MessageByID(id string) (orchestration.EngineeringMessage, bool, error) {
	return queryMessageByID(s.db, id)
}

// queryMessageByID is the sqlExecutor-generic read ResolveDecisionRequest
// (decision_store.go) pins inside its own linearized transaction, exactly as
// the standalone MessageByID does outside one.
func queryMessageByID(q sqlExecutor, id string) (orchestration.EngineeringMessage, bool, error) {
	var document string
	err := q.QueryRow(`SELECT document FROM orchestration_messages WHERE id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return orchestration.EngineeringMessage{}, false, nil
	}
	if err != nil {
		return orchestration.EngineeringMessage{}, false, err
	}
	var message orchestration.EngineeringMessage
	if err := strictJSON([]byte(document), &message); err != nil {
		return orchestration.EngineeringMessage{}, false, fmt.Errorf("stored message is unreadable: %w", err)
	}
	if err := message.Validate(); err != nil {
		return orchestration.EngineeringMessage{}, false, fmt.Errorf("stored message is corrupt: %w", err)
	}
	return message, true, nil
}

func queryScopeMessages(q eventQuerier, scope string) ([]orchestration.EngineeringMessage, error) {
	rows, err := q.Query(`SELECT document FROM orchestration_messages WHERE scope = ? ORDER BY seq ASC`, scope)
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
	return messages, rows.Err()
}

// RefuseMessages durably records that one invocation's message report will
// never be admitted, and why. Insert-only: the first decision stands.
func (s *SQLiteOperationStore) RefuseMessages(key, scope, runID, reason string, at time.Time) error {
	if key == "" || scope == "" || runID == "" || strings.TrimSpace(reason) == "" {
		return errors.New("a message refusal needs its invocation, scope, run and reason")
	}
	_, err := s.db.Exec(`INSERT INTO orchestration_message_refusals (id, scope, run_id, refused_unix_nano, reason)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`, key, scope, runID, at.UnixNano(), boundedDetail(reason))
	return err
}

// ScopeMessageRefusals maps every refused invocation key of one scope to its
// recorded reason.
func (s *SQLiteOperationStore) ScopeMessageRefusals(scope string) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT id, reason FROM orchestration_message_refusals WHERE scope = ?`, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refused := map[string]string{}
	for rows.Next() {
		var key, reason string
		if err := rows.Scan(&key, &reason); err != nil {
			return nil, err
		}
		refused[key] = reason
	}
	return refused, rows.Err()
}
