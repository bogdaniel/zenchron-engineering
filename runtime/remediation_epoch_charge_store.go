package runtime

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
)

// A remediation epoch of a plan-bound unit is paid for out of the plan's child-run
// budget (#480). The charge is a plan-journal event and the epoch is a batch plus
// an epoch row; these are committed in ONE transaction, so there is no state in
// which the epoch exists unpaid or the budget is spent on an epoch that was never
// admitted. The ceiling is checked against the events this very transaction
// appends after, so two admissions racing for the last slot cannot both have it.

// EpochCharge is one epoch's child-run charge against its plan.
type EpochCharge struct {
	PlanID  string
	StageID string
	RunID   string
	At      time.Time
	// MaxChildRuns is the approved ceiling. EnforceCeiling is false only for an
	// epoch run that already exists: it has already spent the child run, and a
	// ceiling cannot un-spend it, so the charge records the fact.
	MaxChildRuns   int
	EnforceCeiling bool
}

var errEpochAlreadyCharged = errors.New("the remediation epoch's child run is already charged to the plan")

// AdmitChargedRemediationEpoch commits the child-run charge, the batch and the
// epoch claim together or not at all. A repeat of an admitted epoch changes
// nothing. A charge already present without its batch - left by an earlier
// revision of this path - is not charged again; the batch is admitted alone.
func (s *SQLiteOperationStore) AdmitChargedRemediationEpoch(batch orchestration.Batch, epoch RemediationEpoch, charge EpochCharge) (orchestration.Batch, bool, error) {
	if err := validateEpochAdmission(batch, epoch); err != nil {
		return orchestration.Batch{}, false, err
	}
	var stored orchestration.Batch
	var created bool
	err := s.commitEpochCharge(charge, func(tx *sql.Tx) (err error) {
		stored, created, err = admitRemediationEpochTx(tx, batch, epoch)
		return err
	})
	if errors.Is(err, errEpochAlreadyCharged) {
		return s.AdmitRemediationEpochBatch(batch, epoch)
	}
	return stored, created, err
}

// ChargeRemediationEpoch records the charge alone, once, for an epoch batch that
// is already stored. It is a no-op when the epoch is already charged.
func (s *SQLiteOperationStore) ChargeRemediationEpoch(charge EpochCharge) error {
	err := s.commitEpochCharge(charge, nil)
	if errors.Is(err, errEpochAlreadyCharged) {
		return nil
	}
	return err
}

func (s *SQLiteOperationStore) commitEpochCharge(charge EpochCharge, alongside func(*sql.Tx) error) error {
	payload, err := marshalPayloadJSON(PlanBudgetConsumedPayload{
		Key: "child_run:" + charge.RunID, StageID: charge.StageID, RunID: charge.RunID, ChildRuns: 1,
	})
	if err != nil {
		return err
	}
	event := EngineeringEvent{
		SchemaVersion: SchemaVersion, ID: newEventID(charge.PlanID), PlanID: charge.PlanID,
		Type: EventPlanBudgetConsumed, OccurredAt: charge.At, Payload: payload,
	}
	stream := planStream(event)
	stream.allocate = func(existing []EngineeringEvent, e EngineeringEvent) (EngineeringEvent, error) {
		snapshot, err := ReducePlan(charge.PlanID, existing)
		if err != nil {
			return EngineeringEvent{}, err
		}
		if snapshot.RunConsumed[charge.RunID].ChildRuns > 0 {
			return EngineeringEvent{}, errEpochAlreadyCharged
		}
		if charge.EnforceCeiling && snapshot.Consumed.ChildRuns >= charge.MaxChildRuns {
			return EngineeringEvent{}, fmt.Errorf("plan %s allows %d child runs and %d have been created, so stage %s cannot start a remediation epoch",
				charge.PlanID, charge.MaxChildRuns, snapshot.Consumed.ChildRuns, charge.StageID)
		}
		return e, nil
	}
	stream.mutate = alongside
	_, err = s.appendToStream(event, stream)
	return err
}
