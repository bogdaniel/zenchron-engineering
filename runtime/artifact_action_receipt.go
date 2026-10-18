package runtime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// artifactActionReceiptSchemaMigrations is this file's own migration,
// appended to sqliteMigrations (sqlite_store.go) the same way
// productSchemaMigrations is: one more entry in the same ordered list and
// schema version counter, kept in its own file to stay under the
// repository's file-size cap.
var artifactActionReceiptSchemaMigrations = []string{`
-- ArtifactActionReceipt's durable store. Deliberately NOT the event journal
-- (events table): that table is reduced, hash-chained lifecycle state for a
-- run or a plan, and a planning artifact or a product write is neither -
-- there is no run or plan identity to fold it against, and forcing one in
-- would duplicate this receipt's own small footprint for no benefit. One row
-- per operation, read by id - exactly what OperatorActionReceipt already is,
-- for the two command kinds the journal has no stream for.
--
-- document is empty between ClaimArtifactActionOperation (reserves the row,
-- before the effect runs) and FinalizeArtifactActionReceipt (fills it in,
-- after the effect succeeds): the PRIMARY KEY makes the claim itself atomic,
-- so a second request sharing the same operation_id cannot also dispatch.
CREATE TABLE artifact_action_receipts (
	operation_id      TEXT PRIMARY KEY,
	created_unix_nano INTEGER NOT NULL,
	document          TEXT NOT NULL
);
`}

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ArtifactActionReceipt observes delivery of a governed planning-artifact or
// product write. It is OperatorActionReceipt's sibling for the two command
// kinds that are not journaled run/plan lifecycle state - see the schema
// comment above. Same guarantee, same reason: grants no authority, changes
// no lifecycle, budget or eligibility. A durable record that an exact
// operation was delivered, nothing more.
type ArtifactActionReceipt struct {
	OperationID       string `json:"operation_id"`
	RequestDigest     string `json:"request_digest"`
	Action            string `json:"action"`
	ControllerBinding string `json:"controller_binding"`
	ArtifactKind      string `json:"artifact_kind,omitempty"`
	ArtifactID        string `json:"artifact_id,omitempty"`
	ArtifactDigest    string `json:"artifact_digest,omitempty"`
	ProductID         string `json:"product_id,omitempty"`
	ProductRevision   int    `json:"product_revision,omitempty"`
}

func (r ArtifactActionReceipt) validate() error {
	if !ValidActionReference(r.OperationID, r.RequestDigest) || r.ControllerBinding == "" {
		return errors.New("invalid artifact action reference")
	}
	switch r.Action {
	case ControlPlanningArtifactPropose:
		if r.ArtifactKind == "" || r.ArtifactID == "" || !digestPattern.MatchString(r.ArtifactDigest) || r.ProductID != "" || r.ProductRevision != 0 {
			return errors.New("invalid planning-artifact receipt subject")
		}
	case ControlProductPropose:
		if r.ProductID == "" || r.ProductRevision < 1 || r.ArtifactKind != "" || r.ArtifactID != "" || r.ArtifactDigest != "" {
			return errors.New("invalid product receipt subject")
		}
	default:
		return errors.New("unsupported artifact receipt action")
	}
	return nil
}

// ClaimArtifactActionOperation atomically reserves operation_id BEFORE the
// effect it protects runs, with an empty placeholder document - the gap a
// bare check-then-act would leave open between "no receipt yet" and "effect
// in flight," where a second request sharing the same reference could also
// see nothing recorded and dispatch a second time. The INSERT's own
// uniqueness is the lock: ON CONFLICT DO NOTHING makes a second claim
// attempt affect zero rows instead of racing past a separate read. claimed
// is false when this exact operation is already recorded or in flight; the
// caller must not dispatch again, only read the eventual result.
//
// A crash between a successful claim and FinalizeArtifactActionReceipt
// leaves the placeholder in place forever. ArtifactActionReceipt reads that
// the same way as "never attempted" - genuinely unknown, not a false
// negative claiming it never happened and not a false positive claiming it
// did. That residual window is the one this store does not close: doing so
// would mean coupling this observational table transactionally into
// AdoptProduct's or planning.Write's own commit, which is a bigger, more
// invasive change than a receipt system that grants no authority should make.
func (s *SQLiteOperationStore) ClaimArtifactActionOperation(id string, at time.Time) (bool, error) {
	result, err := s.db.Exec(`INSERT INTO artifact_action_receipts (operation_id, created_unix_nano, document) VALUES (?, ?, '') ON CONFLICT(operation_id) DO NOTHING`,
		id, at.UnixNano())
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected == 1, err
}

// ReleaseArtifactActionClaim undoes a claim whose effect was REFUSED, not
// lost - the command never durably changed anything, so there is nothing
// to hold operation_id against, and a legitimate retry (corrected content,
// an unconfigured agent now configured) must not be blocked by this one's
// placeholder forever. Only ever removes an unfinalized (still-empty)
// claim; a row that was already finalized by a true duplicate is untouched.
func (s *SQLiteOperationStore) ReleaseArtifactActionClaim(id string) error {
	_, err := s.db.Exec(`DELETE FROM artifact_action_receipts WHERE operation_id = ? AND document = ''`, id)
	return err
}

// FinalizeArtifactActionReceipt fills in the full, validated receipt for a
// claim ClaimArtifactActionOperation already reserved. It only ever UPDATEs
// an existing, still-unfinalized placeholder - never inserts - so a caller
// that skipped the claim step cannot finalize one that was never reserved.
func (s *SQLiteOperationStore) FinalizeArtifactActionReceipt(r ArtifactActionReceipt, at time.Time) error {
	if err := r.validate(); err != nil {
		return err
	}
	document, err := json.Marshal(r)
	if err != nil {
		return err
	}
	result, err := s.db.Exec(`UPDATE artifact_action_receipts SET document = ?, created_unix_nano = ? WHERE operation_id = ? AND document = ''`,
		string(document), at.UnixNano(), r.OperationID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err == nil && affected != 1 {
		err = errors.New("artifact action claim was not reserved before finalizing")
	}
	return err
}

// ArtifactActionReceipt reads one receipt by operation id. It returns nil
// for both "never attempted" and "claimed but not yet finalized" (an empty
// placeholder) - the two are indistinguishable to a reader by design: both
// mean the outcome is genuinely unknown, never a false claim either way.
func (s *SQLiteOperationStore) ArtifactActionReceipt(id string) (*ArtifactActionReceipt, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM artifact_action_receipts WHERE operation_id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if document == "" {
		return nil, nil
	}
	var r ArtifactActionReceipt
	if err := json.Unmarshal([]byte(document), &r); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	if r.OperationID != id {
		return nil, errors.New("artifact receipt subject mismatch")
	}
	return &r, nil
}

func (s *ReadStore) ArtifactActionReceipt(id string) (*ArtifactActionReceipt, error) {
	return s.store.ArtifactActionReceipt(id)
}
