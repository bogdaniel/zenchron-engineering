package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/storage"
)

// admissionPartition holds one admission record per execution_id: the
// envelope and every settled attempt's consumption. claimPartition holds
// the execution's in-flight claim, whose value is the claiming attempt id.
const (
	admissionPartition = "agentkernel.admissions"
	claimPartition     = "agentkernel.admission_claims"
)

// admissionVersion is written on every admission record. A record of any
// other version, an unversioned (v0.1) one included, is not reinterpreted:
// admission refuses until an explicit recovery or migration exists.
const admissionVersion = "agentkernel.admission/v0.2"

// admissionRecord is what the kernel remembers about one execution_id: the
// envelope its first attempt was admitted with, which every later attempt
// shares, and what each settled attempt consumed. Budget.Deadline is stored
// but never compared: the deadline is attempt-scoped (execution spec §11.1).
type admissionRecord struct {
	Version  string          `json:"version,omitempty"`
	Budget   api.Budget      `json:"budget"`
	Attempts []attemptRecord `json:"attempts"`
}

// attemptRecord is one settled attempt and the ledger charge it settled
// with. Consumed is cumulative: the execution's whole charge through this
// attempt, earlier attempts included (the ledger starts from them).
type attemptRecord struct {
	AttemptID string                        `json:"attempt_id"`
	Consumed  map[api.BudgetDimension]int64 `json:"consumed,omitempty"`
}

// consumed is what the execution has consumed so far: per dimension, the
// largest cumulative charge any attempt settled with. Records are never
// added together; each already includes the ones before it. A negative
// charge is corrupt and stays, so ledger.restore treats it as exhausted.
func (rec admissionRecord) consumed() map[api.BudgetDimension]int64 {
	out := map[api.BudgetDimension]int64{}
	for _, a := range rec.Attempts {
		for d, n := range a.Consumed {
			prev, seen := out[d]
			if seen && (prev < 0 || (n >= 0 && n <= prev)) {
				continue
			}
			out[d] = n
		}
	}
	return out
}

// admit decides, before any side effect, whether this attempt may run.
//
// It first reads the execution's record without claiming: admission state
// this contract cannot read (a legacy unversioned record, an unknown
// version) refuses before any claim exists, so an incompatible-version
// refusal never leaves one behind. Otherwise the attempt claims its
// execution_id with PutIfAbsent: an atomic put-if-absent, so of any number
// of contenders (goroutines, Engines, processes sharing a FileRecords root)
// exactly one holds the claim, and the others are refused without reserving
// budget or calling anything. Only the claim holder reads or writes the
// execution's record, so that read-modify-write needs no other lock. A claim
// left by a crashed attempt is never expired by time: its consumption is
// unknown, so every later attempt is refused until the host recovers it
// explicitly.
func (r *run) admit(ctx context.Context) (api.Termination, bool) {
	ctx = context.WithoutCancel(ctx)
	rec, err := r.e.loadAdmission(ctx, r.req.ExecutionID)
	if err != nil {
		return r.admissionNotRecorded(err), false
	}
	if reason := versionRefusal(rec, r.req.ExecutionID); reason != "" {
		return r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, reason), false
	}
	err = r.e.admissions.PutIfAbsent(ctx, claimPartition, r.req.ExecutionID, []byte(r.req.AttemptID))
	if errors.Is(err, storage.ErrExists) {
		return r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, r.claimHeld(ctx)), false
	}
	if err != nil {
		return r.admissionNotRecorded(err), false
	}
	return r.admitClaimed(ctx)
}

// admitClaimed finishes admission while holding the claim. It reads the
// record again, since it may have changed between the unclaimed read and
// the claim (another attempt settled, or a v0.1 kernel wrote a legacy
// record), and releases the claim on every refusal.
func (r *run) admitClaimed(ctx context.Context) (api.Termination, bool) {
	rec, err := r.e.loadAdmission(ctx, r.req.ExecutionID)
	if err != nil {
		return r.releaseClaim(ctx, r.admissionNotRecorded(err)), false
	}
	if reason := admissionRefusal(rec, r.req); reason != "" {
		return r.releaseClaim(ctx, r.termination(api.OutcomeBlocked, api.CauseInvalidRequest, reason)), false
	}
	r.ledger.restore(rec.consumed())
	r.admitted = true
	return api.Termination{}, true
}

// claimHeld names why a held claim refuses this attempt.
func (r *run) claimHeld(ctx context.Context) string {
	holder, err := r.e.admissions.Get(ctx, claimPartition, r.req.ExecutionID)
	switch {
	case err != nil:
		return fmt.Sprintf("execution %q has an attempt in flight or unsettled (claim unreadable: %v); consumption unknown",
			r.req.ExecutionID, err)
	case string(holder) == r.req.AttemptID:
		return fmt.Sprintf("attempt %q already admitted for execution %q", r.req.AttemptID, r.req.ExecutionID)
	}
	return fmt.Sprintf("prior attempt %q in flight or unsettled; consumption unknown", holder)
}

// releaseClaim drops this attempt's claim after a refusal; nothing ran. If
// the claim cannot be dropped it stays, which refuses later attempts.
func (r *run) releaseClaim(ctx context.Context, t api.Termination) api.Termination {
	if err := r.e.admissions.Delete(ctx, claimPartition, r.req.ExecutionID); err != nil {
		t.Detail += "; admission claim not released: " + err.Error()
	}
	return t
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
	if reason := versionRefusal(rec, req.ExecutionID); reason != "" {
		return reason
	}
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
	return ""
}

// versionRefusal names why rec cannot be read under this contract, or
// returns "". An unversioned record without attempts holds no state and is
// treated as absent.
func versionRefusal(rec admissionRecord, executionID string) string {
	switch {
	case rec.Version == admissionVersion, rec.Version == "" && len(rec.Attempts) == 0:
		return ""
	case rec.Version == "":
		return fmt.Sprintf("execution %q has legacy unversioned (v0.1) admission state; "+
			"explicit recovery or migration is required before a %s attempt", executionID, admissionVersion)
	}
	return fmt.Sprintf("execution %q has admission state of unknown version %q; "+
		"explicit recovery or migration is required", executionID, rec.Version)
}

// widenedBound names the first cumulative bound of next that is wider than
// first's, or returns "". The deadline is attempt-scoped and not compared: a
// new attempt may carry a later one. Any change to the money ceiling other than lowering it in the
// same currency is a widening: without the same ceiling, earlier money
// consumption is not comparable.
func widenedBound(first, next api.Budget) string {
	switch {
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

// settleAdmission records this attempt's consumption, then releases the
// claim, so a later attempt starts from it. It ignores the caller's
// cancellation. If either write fails the claim stays held, which refuses
// later attempts: the consumption is then unknown, and the kernel fails
// closed rather than guess.
func (r *run) settleAdmission(ctx context.Context) error {
	ctx = context.WithoutCancel(ctx)
	if err := r.recordConsumption(ctx); err != nil {
		return fmt.Errorf("consumption of attempt %q not recorded: %w", r.req.AttemptID, err)
	}
	if err := r.e.admissions.Delete(ctx, claimPartition, r.req.ExecutionID); err != nil {
		return fmt.Errorf("admission claim of attempt %q not released: %w", r.req.AttemptID, err)
	}
	return nil
}

// recordConsumption appends this attempt to its execution's record. Only the
// claim holder calls it, so the read-modify-write cannot interleave.
func (r *run) recordConsumption(ctx context.Context) error {
	rec, err := r.e.loadAdmission(ctx, r.req.ExecutionID)
	if err != nil {
		return err
	}
	if reason := versionRefusal(rec, r.req.ExecutionID); reason != "" {
		return errors.New(reason)
	}
	if len(rec.Attempts) == 0 {
		rec.Version, rec.Budget = admissionVersion, r.req.Budget
	}
	rec.Attempts = append(rec.Attempts, attemptRecord{AttemptID: r.req.AttemptID, Consumed: r.ledger.consumed()})
	return r.e.saveAdmission(ctx, r.req.ExecutionID, rec)
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
