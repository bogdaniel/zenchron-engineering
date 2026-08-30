package runtime

// Durable watch observation state.
//
// Two rules make this layer safe to build a WatchController on:
//
//   - Global/operator configuration is the SOLE authority for which
//     repositories are watched. This table answers "what did the last poll of
//     this repository observe", never "which repositories are watched". There
//     is deliberately no accessor that enumerates it, so a repository the
//     operator dropped from configuration cannot resurrect itself from a
//     leftover row: nothing ever asks for it again.
//   - No credential, token, header, or secret is ever stored here. The fields
//     below are observations - a cursor, an ETag, timings, a rate-limit reading
//     and an error class - and there is no field one could be smuggled into.
//
// Every timestamp is a value the caller supplies and gets back. This layer does
// not read a clock; the caller injects one, so a watcher's timing is testable
// and two watchers never disagree because one of them looked at wall time.

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// WatchErrorClass is why the last poll of a repository failed. It is a small
// watch-specific vocabulary rather than FailureClass, which classifies what a
// candidate's verification did and has no meaning for a forge poll.
type WatchErrorClass string

const (
	// WatchErrorNone is a repository whose last poll succeeded.
	WatchErrorNone WatchErrorClass = ""
	// WatchErrorTransient is a network or 5xx failure: retry later.
	WatchErrorTransient WatchErrorClass = "transient"
	// WatchErrorRateLimited is the forge asking to be left alone until reset.
	WatchErrorRateLimited WatchErrorClass = "rate_limited"
	// WatchErrorAuth is a credential the operator must fix. Backing off is
	// correct; retrying harder is not.
	WatchErrorAuth WatchErrorClass = "auth"
	// WatchErrorPermanent is a repository that will not start answering by
	// itself - removed, renamed, or never visible to this credential.
	WatchErrorPermanent WatchErrorClass = "permanent"
)

// WatchRateLimit is the last rate-limit reading the forge reported. It is an
// observation, not a budget: nothing here authorizes a call.
type WatchRateLimit struct {
	Limit      int       `json:"limit,omitempty"`
	Remaining  int       `json:"remaining,omitempty"`
	ResetAt    time.Time `json:"reset_at,omitempty"`
	ObservedAt time.Time `json:"observed_at,omitempty"`
}

// WatchState is everything a watcher needs to resume polling one repository
// safely after a restart, and nothing else.
type WatchState struct {
	// Repository is the "owner/name" identity, which is also the row key.
	Repository string `json:"repository"`
	// Cursor is the discovery position the adapter defines - for the GitHub
	// issue listing, the "updated since" boundary already consumed.
	Cursor string `json:"cursor,omitempty"`
	// ETag is the conditional-request validator, where the endpoint supports
	// one. It is what turns a poll into a cheap 304.
	ETag string `json:"etag,omitempty"`
	// LastSuccessAt is the last poll that actually completed.
	LastSuccessAt time.Time `json:"last_success_at,omitempty"`
	// NotBefore is the backoff: no poll of this repository before this instant.
	// It is stored rather than recomputed so a restart does not reset a backoff
	// the forge asked for.
	NotBefore time.Time `json:"not_before,omitempty"`
	// ConsecutiveFailures carries the backoff's growth across restarts.
	ConsecutiveFailures int `json:"consecutive_failures,omitempty"`
	// RateLimit is the last reading; ObservedAt says how stale it is.
	RateLimit WatchRateLimit `json:"rate_limit,omitempty"`
	// LastErrorClass, LastErrorDetail and LastErrorAt describe the last
	// failure. Detail is a bounded runtime-authored summary; it is never forge
	// text treated as anything but data.
	LastErrorClass  WatchErrorClass `json:"last_error_class,omitempty"`
	LastErrorDetail string          `json:"last_error_detail,omitempty"`
	LastErrorAt     time.Time       `json:"last_error_at,omitempty"`
}

// WatchStateFor reads the observation state for one repository and the revision
// it was read at. An unwatched-so-far repository reports ok=false at revision 0,
// which is exactly the revision PutWatchState treats as "create".
//
// The caller passes an identity it read from CONFIGURATION. There is no
// "all watch states" query on purpose: see the note at the top of this file.
func (s *SQLiteOperationStore) WatchStateFor(repository string) (WatchState, int64, bool, error) {
	var document string
	var revision int64
	err := s.db.QueryRow(`SELECT document, revision FROM watch_state WHERE repository = ?`, repository).Scan(&document, &revision)
	if err == sql.ErrNoRows {
		return WatchState{}, 0, false, nil
	}
	if err != nil {
		return WatchState{}, 0, false, err
	}
	var state WatchState
	if err := json.Unmarshal([]byte(document), &state); err != nil {
		return WatchState{}, 0, false, fmt.Errorf("decode durable watch state: %w", err)
	}
	return state, revision, true, nil
}

// PutWatchState is the compare-and-set write, following PutOperation exactly:
// one conditional statement, which SQLite executes as one transaction. expected
// is the revision the state was read at; 0 means "create". A create that loses
// the race and an update whose expected revision is stale both report false and
// leave the stored row untouched, so the loser must re-read and re-decide
// rather than overwrite the winner.
func (s *SQLiteOperationStore) PutWatchState(state WatchState, expected int64) (int64, bool, error) {
	if state.Repository == "" {
		return 0, false, fmt.Errorf("watch state repository is required")
	}
	// The durable document is canonical (RFC 8785) so equal state is byte-equal.
	document, err := CanonicalJSON(state)
	if err != nil {
		return 0, false, err
	}
	if expected == 0 {
		result, err := s.db.Exec(`INSERT INTO watch_state (repository, revision, document)
			SELECT ?, 1, ? WHERE NOT EXISTS (SELECT 1 FROM watch_state WHERE repository = ?)`,
			state.Repository, string(document), state.Repository)
		if err != nil {
			return 0, false, err
		}
		affected, err := result.RowsAffected()
		if err != nil || affected == 0 {
			return 0, false, err
		}
		return 1, true, nil
	}
	result, err := s.db.Exec(`UPDATE watch_state SET revision = revision + 1, document = ? WHERE repository = ? AND revision = ?`,
		string(document), state.Repository, expected)
	if err != nil {
		return 0, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil || affected == 0 {
		return 0, false, err
	}
	return expected + 1, true, nil
}
