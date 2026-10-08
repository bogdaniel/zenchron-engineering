package orchestration

// WorkUnitHold (#508): the one way a WorkGraph unit can be held before its
// first activation, placed through the same governed, authority-checked
// control-plane action a resolution goes through - never by a worker, since
// there is no run yet for a worker to write one from.
//
// It is #472's readiness-owner seam, supplied: docs/workgraph.md is explicit
// that "the graph represents holds and resolves none" of its own, and that a
// decision_request MESSAGE (#473) is a different fact - a worker ASKING a
// question from inside its own run - which nothing wires into this hold. A
// WorkUnitHold is the durable fact an operator placed instead, and resolving
// it is the only way it is lifted.
//
// A unit held this way is, by construction, never activated while the hold
// stands: it has no child run, so it consumes no provider process and no
// scheduler slot. Its first activation after the hold lifts is therefore
// always a first activation - there is no prior provider session to
// reconstruct, only durable state to build the first one from.

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// WorkUnitHoldSchemaVersion versions the durable hold record.
const WorkUnitHoldSchemaVersion = "0.1"

const maxHoldPurposeBytes = 1 << 10

// WorkUnitHold is the immutable record that an operator placed a hold on one
// WorkGraph unit before its first activation. Its identity is deterministic
// from the graph and unit alone, so a unit can be held at most once, ever: a
// lost reply finds the hold already placed instead of a conflict, and there is
// no second hold to place once the first is resolved.
type WorkUnitHold struct {
	SchemaVersion string                      `json:"schema_version"`
	ID            string                      `json:"id"`
	GraphID       string                      `json:"graph_id"`
	UnitID        string                      `json:"unit_id"`
	Purpose       string                      `json:"purpose"`
	RequestedBy   DecisionResolutionAuthority `json:"requested_by"`
	RequestedAt   time.Time                   `json:"requested_at"`
}

// Validate refuses a hold missing any of its own identity, or authored by
// anything this build does not recognize as an authorized operator.
func (h WorkUnitHold) Validate() error {
	if h.SchemaVersion != WorkUnitHoldSchemaVersion {
		return fmt.Errorf("work unit hold schema version %q is not %q", h.SchemaVersion, WorkUnitHoldSchemaVersion)
	}
	id, err := WorkUnitHoldID(h.GraphID, h.UnitID)
	if err != nil {
		return err
	}
	if id != h.ID {
		return fmt.Errorf("work unit hold %s does not match the identity %s of its own graph and unit", h.ID, id)
	}
	if err := boundedDecisionText("work unit hold purpose", h.Purpose, maxHoldPurposeBytes); err != nil {
		return err
	}
	if err := h.RequestedBy.Validate(); err != nil {
		return err
	}
	if h.RequestedAt.IsZero() {
		return errors.New("a work unit hold needs its placement time")
	}
	return nil
}

// WorkUnitHoldID is deterministic from the graph and unit alone.
func WorkUnitHoldID(graphID, unitID string) (string, error) {
	if strings.TrimSpace(graphID) == "" || strings.TrimSpace(unitID) == "" {
		return "", errors.New("a work unit hold needs its graph and unit")
	}
	digest, err := domain.Digest(struct {
		Graph string `json:"graph"`
		Unit  string `json:"unit"`
	}{graphID, unitID})
	if err != nil {
		return "", err
	}
	return "workgraph-hold-" + digest[:32], nil
}

// Ref normalizes this hold into the request ResolveDecision validates
// against. A hold carries no exact-subject binding of its own - what it gates
// is a unit's ACTIVATION, which has no subject yet - and it is always live: it
// is placed at most once and is never superseded. ExpectedOutcomeKind is
// always allow_deny: a gate is inherently a pass/fail question, never a
// bounded-text or selected-option one, and nothing resolving it may answer
// otherwise (#508 review B2/B5).
func (h WorkUnitHold) Ref() DecisionRequestRef {
	return DecisionRequestRef{ID: h.ID, Scope: h.GraphID + ":" + h.UnitID, Live: true, ExpectedOutcomeKind: DecisionAllowDeny}
}

// DecisionWait renders this hold exactly as #472's seam expects it: an
// opaque reference the graph stores and interprets none of.
func (h WorkUnitHold) DecisionWait() DecisionWait {
	return DecisionWait{Reference: h.ID, Detail: h.Purpose}
}
