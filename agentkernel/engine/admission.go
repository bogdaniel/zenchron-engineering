package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// admissionPartition is the Records partition holding one admission record
// per execution_id.
const admissionPartition = "agentkernel.admissions"

// admissionRecord is what the kernel remembers about one execution_id: the
// envelope its first attempt was admitted with, which every later attempt
// shares, and each attempt's state.
type admissionRecord struct {
	Budget   api.Budget      `json:"budget"`
	Attempts []attemptRecord `json:"attempts"`
}

// attemptRecord is one admitted attempt. Until Settled, its consumption is
// unknown; Consumed is the ledger charge it settled with.
type attemptRecord struct {
	AttemptID string                        `json:"attempt_id"`
	Settled   bool                          `json:"settled"`
	Consumed  map[api.BudgetDimension]int64 `json:"consumed,omitempty"`
}

// admit decides, before any side effect, whether this attempt may run, and
// if so records it as in flight and charges the ledger with what settled
// earlier attempts consumed. Budget is the execution-wide envelope: an
// attempt can never renew or widen it.
//
// ponytail: admission is serialized by the Engine's mutex, so it is exact
// within one Engine. Several Engines or processes sharing one Records store
// can race between Get and Put; that needs a Records with an atomic
// compare-and-put, which Gate A does not have, or host serialization.
func (r *run) admit(ctx context.Context) (api.Termination, bool) {
	r.e.admitMu.Lock()
	defer r.e.admitMu.Unlock()
	var rec admissionRecord
	err := r.bounded(ctx, false, "admission store", func(ctx context.Context) error {
		var err error
		rec, err = r.e.loadAdmission(ctx, r.req.ExecutionID)
		return err
	})
	if err != nil {
		return r.admissionNotRecorded(err), false
	}
	if reason := admissionRefusal(rec, r.req); reason != "" {
		return r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, reason), false
	}
	prior := map[api.BudgetDimension]int64{}
	for _, a := range rec.Attempts {
		for d, n := range a.Consumed {
			prior[d] += n
		}
	}
	if len(rec.Attempts) == 0 {
		rec.Budget = r.req.Budget
	}
	rec.Attempts = append(rec.Attempts, attemptRecord{AttemptID: r.req.AttemptID})
	// An abandoned write may still land later; the attempt then reads as
	// admitted and unsettled, which refuses later attempts: fail closed.
	save := func(ctx context.Context) error { return r.e.saveAdmission(ctx, r.req.ExecutionID, rec) }
	if err := r.bounded(ctx, false, "admission store", save); err != nil {
		return r.admissionNotRecorded(err), false
	}
	r.ledger.restore(prior)
	r.admitted = true
	return api.Termination{}, true
}

// admissionNotRecorded refuses an attempt whose admission could not be read
// or written: without it, consumption could not be carried to a later
// attempt. Nothing has run.
func (r *run) admissionNotRecorded(err error) api.Termination {
	return r.termination(api.OutcomeIncomplete, api.CauseRecordingFailed,
		"admission not recorded, nothing ran: "+err.Error())
}

// admissionRefusal names why req may not start, or returns "".
func admissionRefusal(rec admissionRecord, req api.ExecutionRequest) string {
	for _, a := range rec.Attempts {
		if a.AttemptID == req.AttemptID {
			return fmt.Sprintf("attempt %q already admitted for execution %q", req.AttemptID, req.ExecutionID)
		}
	}
	if len(rec.Attempts) == 0 {
		return ""
	}
	if bound := widenedBound(rec.Budget, req.Budget); bound != "" {
		return fmt.Sprintf("budget widens %s beyond the envelope of execution %q", bound, req.ExecutionID)
	}
	for _, a := range rec.Attempts {
		if !a.Settled {
			return fmt.Sprintf("prior attempt %q unsettled; consumption unknown", a.AttemptID)
		}
	}
	return ""
}

// widenedBound names the first bound of next that is wider than first's, or
// returns "". Any change to the money ceiling other than lowering it in the
// same currency is a widening: without the same ceiling, earlier money
// consumption is not comparable.
func widenedBound(first, next api.Budget) string {
	switch {
	case next.Deadline.After(first.Deadline):
		return "deadline"
	case next.MaxIterations > first.MaxIterations:
		return "max_iterations"
	case next.MaxToolCalls > first.MaxToolCalls:
		return "max_tool_calls"
	case next.MaxInputTokens > first.MaxInputTokens:
		return "max_input_tokens"
	case next.MaxOutputTokens > first.MaxOutputTokens:
		return "max_output_tokens"
	case next.MaxArtifactBytes > first.MaxArtifactBytes:
		return "max_artifact_bytes"
	case next.MaxProviderRetries > first.MaxProviderRetries:
		return "max_provider_retries"
	case (first.Money == nil) != (next.Money == nil):
		return "money"
	case first.Money != nil && (next.Money.Currency != first.Money.Currency || next.Money.MaxMicros > first.Money.MaxMicros):
		return "money"
	}
	return ""
}

// settleAdmission records this attempt's consumption so a later attempt
// starts from it. Like terminal recording it ignores the caller's
// cancellation and is abandoned at the settlement deadline (run.bounded); an
// abandoned write leaves the attempt unsettled, which refuses later attempts.
func (r *run) settleAdmission(ctx context.Context) error {
	r.e.admitMu.Lock()
	defer r.e.admitMu.Unlock()
	consumed := r.ledger.consumed()
	err := r.bounded(ctx, true, "admission store", func(ctx context.Context) error {
		rec, err := r.e.loadAdmission(ctx, r.req.ExecutionID)
		if err != nil {
			return err
		}
		if err := settleAttempt(&rec, r.req.AttemptID, consumed); err != nil {
			return err
		}
		return r.e.saveAdmission(ctx, r.req.ExecutionID, rec)
	})
	if err != nil {
		return fmt.Errorf("consumption of attempt %q not recorded: %w", r.req.AttemptID, err)
	}
	return nil
}

func settleAttempt(rec *admissionRecord, attemptID string, consumed map[api.BudgetDimension]int64) error {
	for i := range rec.Attempts {
		if rec.Attempts[i].AttemptID == attemptID {
			rec.Attempts[i].Settled, rec.Attempts[i].Consumed = true, consumed
			return nil
		}
	}
	return errors.New("admission record lost the attempt")
}

// loadAdmission returns the execution's record; an absent one is empty.
func (e *Engine) loadAdmission(ctx context.Context, executionID string) (admissionRecord, error) {
	var rec admissionRecord
	data, err := e.admissions.Get(ctx, admissionPartition, executionID)
	if errors.Is(err, storage.ErrNotFound) {
		return rec, nil
	}
	if err != nil {
		return rec, err
	}
	if err := json.Unmarshal(data, &rec); err != nil {
		return rec, fmt.Errorf("admission record unreadable: %w", err)
	}
	return rec, nil
}

func (e *Engine) saveAdmission(ctx context.Context, executionID string, rec admissionRecord) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode admission record: %w", err)
	}
	return e.admissions.Put(ctx, admissionPartition, executionID, data)
}
