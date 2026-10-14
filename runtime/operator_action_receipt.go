package runtime

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

const (
	EventPlanOperatorActionReceipt = "plan.operator_action_receipt"
	EventRunOperatorActionReceipt  = "run.operator_action_receipt"
	ControlCodeOutcomeUnknown      = "action_outcome_unknown"
)

// OperatorActionReceipt observes delivery of an existing governed command.
// It grants no authority and changes no lifecycle, budget or eligibility.
type OperatorActionReceipt struct {
	OperationID       string `json:"operation_id"`
	RequestDigest     string `json:"request_digest"`
	Action            string `json:"action"`
	ControllerBinding string `json:"controller_binding"`
	PlanID            string `json:"plan_id,omitempty"`
	Revision          int    `json:"revision,omitempty"`
	PlanDigest        string `json:"plan_digest,omitempty"`
	Repository        string `json:"repository,omitempty"`
	SourceIssue       int    `json:"source_issue,omitempty"`
	RunID             string `json:"run_id,omitempty"`
	SourceRunID       string `json:"source_run_id,omitempty"`
}

func ValidActionReference(id, digest string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || len(digest) != 64 {
		return false
	}
	_, a := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	_, b := hex.DecodeString(digest)
	return a == nil && b == nil && strings.ToLower(id) == id && strings.ToLower(digest) == digest
}

func (r OperatorActionReceipt) validate() error {
	if !ValidActionReference(r.OperationID, r.RequestDigest) || r.ControllerBinding == "" {
		return errors.New("invalid operator action reference")
	}
	switch r.Action {
	case ControlPlanRevise:
		if r.PlanID == "" || r.Revision < 1 || !ValidActionReference(r.OperationID, r.PlanDigest) || r.RunID != "" || r.SourceRunID != "" {
			return errors.New("invalid planning receipt subject")
		}
		if _, err := ParseGitHubRepo(r.Repository); err != nil || r.SourceIssue < 1 {
			return errors.New("invalid planning source")
		}
	case ControlResume, ControlRefresh:
		if r.RunID == "" || r.SourceRunID == "" || r.PlanID != "" || r.Revision != 0 || r.PlanDigest != "" || r.Repository != "" || r.SourceIssue != 0 || (r.Action == ControlResume && r.RunID != r.SourceRunID) || (r.Action == ControlRefresh && r.RunID == r.SourceRunID) {
			return errors.New("invalid run receipt subject")
		}
	default:
		return errors.New("unsupported receipt action")
	}
	return nil
}

func init() {
	// Register observational events in the existing catalogue and journal only.
	for _, kind := range []string{EventPlanOperatorActionReceipt, EventRunOperatorActionReceipt} {
		eventTypes[kind] = true
		eventPayloads[kind] = payloadSchema(func(r OperatorActionReceipt) error {
			if (kind == EventPlanOperatorActionReceipt) != (r.Action == ControlPlanRevise) {
				return errors.New("receipt stream kind mismatch")
			}
			return r.validate()
		})
	}
	planEventTypes[EventPlanOperatorActionReceipt] = true
}

func (s *SQLiteOperationStore) OperatorActionReceipt(id string) (*OperatorActionReceipt, error) {
	// The existing event primary key gives an exact bounded lookup; no receipt database/index.
	events, err := queryStreamEvents(s.db, "id = ?", "operator-action-"+id)
	if err != nil || len(events) == 0 {
		return nil, err
	}
	e := events[0]
	if e.Type != EventPlanOperatorActionReceipt && e.Type != EventRunOperatorActionReceipt {
		return nil, errors.New("operator receipt event mismatch")
	}
	hash, err := EventDigest(e)
	if err != nil || hash != e.EventHash {
		return nil, errors.New("invalid operator receipt hash")
	}
	var r OperatorActionReceipt
	if err := json.Unmarshal(e.Payload, &r); err != nil {
		return nil, err
	}
	if err := r.validate(); err != nil {
		return nil, err
	}
	if r.OperationID != id || r.OperationID != e.OperationID ||
		(r.Action == ControlPlanRevise && r.PlanID != e.PlanID) ||
		(r.Action != ControlPlanRevise && r.SourceRunID != e.RunID) {
		return nil, errors.New("operator receipt subject mismatch")
	}
	return &r, nil
}

func (s *ReadStore) OperatorActionReceipt(id string) (*OperatorActionReceipt, error) {
	return s.store.OperatorActionReceipt(id)
}
