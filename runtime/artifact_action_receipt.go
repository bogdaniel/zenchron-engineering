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
-- per operation, written once, read by id - exactly what OperatorActionReceipt
-- already is, for the two command kinds the journal has no stream for.
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

// PutArtifactActionReceipt durably records one receipt. The operation_id
// primary key refuses a second row for the same operation outright; the
// caller (dispatchArtifactWithReceipt) already checks for an existing
// receipt first, so a collision here means a concurrent attempt slipped
// in between, and the unique-constraint error is exactly the right refusal.
func (s *SQLiteOperationStore) PutArtifactActionReceipt(r ArtifactActionReceipt, at time.Time) error {
	document, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO artifact_action_receipts (operation_id, created_unix_nano, document) VALUES (?, ?, ?)`,
		r.OperationID, at.UnixNano(), string(document))
	return err
}

// ArtifactActionReceipt reads one receipt by operation id, or nil if none
// was ever recorded.
func (s *SQLiteOperationStore) ArtifactActionReceipt(id string) (*ArtifactActionReceipt, error) {
	var document string
	err := s.db.QueryRow(`SELECT document FROM artifact_action_receipts WHERE operation_id = ?`, id).Scan(&document)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
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
