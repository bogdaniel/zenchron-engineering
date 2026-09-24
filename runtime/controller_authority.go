package runtime

// WHAT GOVERNS NOW, AND HOW IT CAME TO GOVERN.
//
// For as long as the only way to establish controller authority was to activate
// a transition, "which activation governs" was the whole question, and
// controller_current_activation was a complete answer. The live configuration
// incident proved it is not.
//
// A controller-effective configuration change cannot cross an ordinary
// succession, and that is correct: the successor binding is the predecessor's
// with only the build replaced, so a process running under a changed
// configuration announces a binding the prepared record does not name, and
// evaluateConfiguration refuses for every run being carried. What followed was
// the gap - building the new controller and starting it did NOT make it the
// governing root, because only an activation could write that pointer. The old
// activation stayed current, every upgrade was refused after the point of no
// return, and the state directory sat in INVARIANT_VIOLATION with no
// in-protocol way out.
//
// RE-ADOPTION IS A SEPARATE AUTHORITY EVENT, not an activation with a different
// origin. Recording it as an activation would make the durable history lie: no
// handoff was performed, no predecessor released anything, no succession was
// admitted for any run. The handoffs that did activate remain exactly the
// history they were. What changes is that present authority can now name either.
//
// IT IS COLD AND OPERATOR-AUTHORIZED, and both halves are load-bearing. Cold,
// because an authority root that could move under a running controller would be
// the hot re-anchor this system spent itself refusing. Operator-authorized,
// because nothing in the durable state can decide that a configuration change
// was intended - only a person can, and they say so with a reason that is
// recorded beside what they sanctioned.

import (
	"database/sql"
	"fmt"
	"time"
)

// ControllerAuthorityKind is how present authority was established.
type ControllerAuthorityKind string

const (
	// AuthorityHandoffActivation is authority established by a transition that
	// activated: the ordinary path, and the only one succession produces.
	AuthorityHandoffActivation ControllerAuthorityKind = "handoff_activation"
	// AuthorityOperatorReadoption is authority established by an operator
	// sanctioning an adopted generation as the governing root, which is how a
	// deliberate configuration boundary is crossed.
	AuthorityOperatorReadoption ControllerAuthorityKind = "operator_readoption"
)

// ControllerAuthority is the governing binding and its provenance.
//
// Callers ask for the BINDING. Whether it arrived by activation or by
// re-adoption is recorded because an operator has to be able to see it, and is
// deliberately not something the succession path branches on: after either, the
// next transition succeeds from this binding in exactly the same way.
type ControllerAuthority struct {
	Kind ControllerAuthorityKind `json:"kind"`
	// Ref is the handoff id or the re-adoption id this authority came from.
	Ref     string            `json:"ref"`
	Binding ControllerBinding `json:"binding"`
	// Artifact is the executable the governing generation runs from. It is
	// recorded for the same reason a handoff records one: the stable
	// entrypoint this authority governs is a path, and a subject that named
	// only digests could say which generation governs without saying what to
	// point at.
	Artifact string `json:"artifact,omitempty"`
}

// ControllerReadoption is the immutable record of one operator re-adoption.
type ControllerReadoption struct {
	ID string `json:"id"`
	// Previous is the authority this re-adoption superseded, absent when the
	// state directory had none.
	Previous *ControllerAuthority `json:"previous,omitempty"`
	// Binding is the generation that governs from here.
	Binding ControllerBinding `json:"binding"`
	// Self is what the invoking executable measured itself to be, and
	// Provenance is the adopted build it was published as. They are recorded
	// separately because one is what is running and the other is what the
	// builder claimed, and a re-adoption is only honest when they agree.
	Self       ControllerSelfRecord   `json:"self"`
	Provenance AdoptedBuildProvenance `json:"provenance"`
	// PreviousConfig and Config are the two effective configuration digests
	// this boundary crosses. Equal digests are permitted - a re-adoption is
	// also how a new adopted root is established after an interrupted chain -
	// and an operator reading the record can see which case it was.
	PreviousConfig ConfigDigest `json:"previous_config"`
	Config         ConfigDigest `json:"config"`
	// Reason is the operator's stated reason. It is required: an authority
	// event nobody explained is one nobody can review.
	Reason string `json:"reason"`
	// Operator is who the runtime resolved as acting, with its provenance.
	Operator   RecordedOperator `json:"operator"`
	RecordedAt time.Time        `json:"recorded_at"`
}

// CurrentControllerAuthority reports the binding that presently governs.
//
// It reads the authority subject and falls back to nothing: a state directory
// with no authority has none, which is the ordinary state of one that has never
// completed a transition, and the first activation or re-adoption establishes
// it.
func (s *SQLiteOperationStore) CurrentControllerAuthority() (ControllerAuthority, bool, error) {
	var document string
	switch err := s.db.QueryRow(
		`SELECT document FROM controller_current_authority WHERE id = 'current'`).Scan(&document); err {
	case nil:
	case sql.ErrNoRows:
		return ControllerAuthority{}, false, nil
	default:
		return ControllerAuthority{}, false, err
	}
	var authority ControllerAuthority
	err := decodeJSON([]byte(document), &authority)
	return authority, err == nil, err
}

// ControllerReadoptions returns every re-adoption, newest first. They are
// history and are never amended.
func (s *SQLiteOperationStore) ControllerReadoptions() ([]ControllerReadoption, error) {
	rows, err := s.db.Query(
		`SELECT document FROM controller_readoptions ORDER BY recorded_unix_nano DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var readoptions []ControllerReadoption
	for rows.Next() {
		var document string
		if err := rows.Scan(&document); err != nil {
			return nil, err
		}
		var readoption ControllerReadoption
		if err := decodeJSON([]byte(document), &readoption); err != nil {
			return nil, err
		}
		readoptions = append(readoptions, readoption)
	}
	return readoptions, rows.Err()
}

// ReadoptController records a re-adoption and moves present authority to it, in
// one durable commit.
//
// THE COMMIT IS CONDITIONAL ON WHAT THE CALLER SAW. expected is the authority
// the preflight inspected, and the move happens only if that is still what
// governs - so an activation completing between the inspection and this call
// loses the race rather than being silently overwritten. A caller that observed
// no authority passes nil, which requires there to still be none.
//
// IT IS IDEMPOTENT ON RETRY. A crash after this commit and before the
// projection is repaired leaves an authority that already names this binding;
// re-running the same re-adoption recognises its own record and converges
// instead of manufacturing a second authority event.
func (s *SQLiteOperationStore) ReadoptController(readoption ControllerReadoption, expected *ControllerAuthority) (bool, error) {
	if readoption.ID == "" || readoption.Reason == "" {
		return false, fmt.Errorf("a re-adoption records an id and the operator's reason")
	}
	authority := ControllerAuthority{
		Kind: AuthorityOperatorReadoption, Ref: readoption.ID, Binding: readoption.Binding,
		Artifact: readoption.Self.ExecutablePath,
	}
	record, err := CanonicalJSON(readoption)
	if err != nil {
		return false, err
	}
	document, err := CanonicalJSON(authority)
	if err != nil {
		return false, err
	}
	stamp := readoption.RecordedAt.UnixNano()

	transaction, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = transaction.Rollback() }()

	// WHAT GOVERNS NOW MUST STILL BE WHAT THE PREFLIGHT SAW.
	//
	// The comparison is on the DECODED authority rather than on the stored
	// bytes. A row written by the migration that restated the old activation
	// pointer is the same authority as one written here and not the same text -
	// the migration builds its JSON in SQL, canonical form sorts its keys - and
	// a byte comparison would refuse every commit after an upgrade for a
	// difference that means nothing.
	var stored sql.NullString
	switch err := transaction.QueryRow(
		`SELECT document FROM controller_current_authority WHERE id = 'current'`).Scan(&stored); err {
	case nil, sql.ErrNoRows:
	default:
		return false, err
	}
	var current *ControllerAuthority
	if stored.Valid {
		var decoded ControllerAuthority
		if err := decodeJSON([]byte(stored.String), &decoded); err != nil {
			return false, err
		}
		current = &decoded
	}
	same, err := sameAuthority(current, expected)
	if err != nil {
		return false, err
	}
	if !same {
		// Either somebody else established authority in between, or this
		// process is committing against a record it did not inspect. Both are
		// the same answer: this was authorized against a world that has moved.
		return false, nil
	}

	if _, err := transaction.Exec(
		`INSERT INTO controller_readoptions (id, recorded_unix_nano, document) VALUES (?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`, readoption.ID, stamp, record); err != nil {
		return false, err
	}
	if _, err := transaction.Exec(
		`INSERT INTO controller_current_authority (id, kind, ref, updated_unix_nano, document)
		 VALUES ('current', ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, ref = excluded.ref,
		     updated_unix_nano = excluded.updated_unix_nano, document = excluded.document`,
		string(authority.Kind), authority.Ref, stamp, document); err != nil {
		return false, err
	}
	return true, transaction.Commit()
}

// sameAuthority compares two present-authority observations by what they mean:
// how authority was established, which event established it, and which binding
// it names.
func sameAuthority(left, right *ControllerAuthority) (bool, error) {
	if left == nil || right == nil {
		return left == nil && right == nil, nil
	}
	if left.Kind != right.Kind || left.Ref != right.Ref {
		return false, nil
	}
	leftBinding, err := left.Binding.Digest()
	if err != nil {
		return false, err
	}
	rightBinding, err := right.Binding.Digest()
	if err != nil {
		return false, err
	}
	return leftBinding == rightBinding, nil
}
