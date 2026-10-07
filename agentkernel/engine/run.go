package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/internal/handoff"
)

// eventSource names this component in every event it records.
const eventSource = "agentkernel/engine"

// run is the state of one execution. The loop runs on one goroutine; the
// mutex guards what tools may touch concurrently through the metered store
// (events, artifacts, the account).
type run struct {
	e      *Engine
	req    api.ExecutionRequest
	parent context.Context
	ledger *ledger

	mu           sync.Mutex
	seq          int64
	observations []api.Observation
	artifacts    []api.ArtifactRef
	recordErr    error
	// exhaustedBy is set when a tool's artifact write was refused for budget.
	exhaustedBy api.BudgetDimension
	acct        account

	// settleBy bounds terminal recording; set once when settlement begins.
	settleBy time.Time
	// admitted is set once this attempt holds its execution's admission
	// claim (admission.go).
	admitted bool
	// providerCalls numbers provider hand-offs, for their call IDs.
	providerCalls int

	routing    *api.RoutingDecision
	manifest   *api.ContextManifest
	provenance api.Provenance
	finalText  string
}

func newRun(e *Engine, parent context.Context, req api.ExecutionRequest) *run {
	return &run{
		e: e, req: req, parent: parent, ledger: newLedger(req.Budget),
		acct: account{costKnown: true},
		provenance: api.Provenance{
			KernelVersion:     api.KernelVersion,
			WorkspaceID:       req.Workspace.ID,
			WorkspaceDigest:   req.Workspace.ManifestDigest,
			InstructionDigest: req.Constraints.InstructionDigest,
			Isolation:         api.IsolationUnproven,
		},
	}
}

// emit records one event. After the first recording failure nothing more is
// recorded and every later emit returns that failure, so the caller stops
// further side effects. The event is handed to the host's sink worker
// (handoff.Exchange); recording ignores cancellation of the execution (the
// sink must still see how a cancelled execution ended) but never outlasts
// its bound (run.recordingBound): an event not acknowledged in time is a
// recording failure.
func (r *run) emit(ctx context.Context, ev api.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr != nil {
		return r.recordErr
	}
	r.seq++
	ev.Version, ev.ExecutionID, ev.AttemptID = api.ExecutionVersion, r.req.ExecutionID, r.req.AttemptID
	ev.Seq, ev.ObservedAt, ev.Source = r.seq, r.e.clock.Now(), eventSource
	b := r.recordingBound(ev.Kind == api.EventRefused || ev.Kind == api.EventSettled)
	callCtx, cancel := recordingContext(ctx, b)
	defer cancel()
	id := fmt.Sprintf("%s/%s/event-%d", ev.ExecutionID, ev.AttemptID, ev.Seq)
	refused, err := handoff.Exchange("event sink", r.e.events, api.EventDelivery{ID: id, Context: callCtx, Request: ev}, b)
	if err == nil {
		err = refused
	}
	if err != nil {
		r.recordErr = fmt.Errorf("event %d (%s) not recorded: %w", ev.Seq, ev.Kind, err)
		return r.recordErr
	}
	r.observations = append(r.observations, api.Observation{
		Seq: ev.Seq, Kind: ev.Kind, Detail: ev.Detail, ToolCall: ev.ToolCall, Grant: ev.Grant, Ref: ev.Ref,
	})
	return nil
}

// beginSettlement fixes the settlement deadline the first time it is called.
func (r *run) beginSettlement() {
	if r.settleBy.IsZero() {
		r.settleBy = time.Now().Add(r.e.settleTimeout)
	}
}

// failRecording marks a record the host relies on as not written, unless an
// earlier failure is already the one reported.
func (r *run) failRecording(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.recordErr == nil {
		r.recordErr = err
	}
}

func (r *run) recordingFailure() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.recordErr
}

func (r *run) termination(outcome api.Outcome, cause api.Cause, detail string) api.Termination {
	return api.Termination{Outcome: outcome, Cause: cause, Detail: detail, ObservedAt: r.e.clock.Now()}
}

func (r *run) exhausted(dim api.BudgetDimension, detail string) api.Termination {
	t := r.termination(api.OutcomeExhausted, api.CauseBudgetExhausted, detail)
	t.Dimension = dim
	return t
}

func (r *run) recordingFailed(observed string) api.Termination {
	detail := r.recordingFailure().Error()
	if observed != "" {
		detail += "; observed before recording stopped: " + observed
	}
	return r.termination(api.OutcomeIncomplete, api.CauseRecordingFailed, detail)
}

// finish is the single settlement point: Execute calls it exactly once with
// the termination the loop returned, and nothing observed afterwards (a late
// cancellation included) can change that termination except a failure to
// record it, which the result then states alongside the observed outcome.
func (r *run) finish(ctx context.Context, t api.Termination) api.ExecutionResult {
	r.beginSettlement()
	if r.admitted {
		if err := r.settleAdmission(ctx); err != nil {
			r.failRecording(err)
		}
	}
	if r.recordingFailure() != nil && t.Cause != api.CauseRecordingFailed {
		t = r.recordingFailed(describe(t))
	}
	if t.Cause != api.CauseRecordingFailed {
		ev := api.Event{Kind: api.EventSettled, Detail: describe(t)}
		if err := r.emit(ctx, ev); err != nil {
			t = r.recordingFailed(describe(t))
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	result := api.ExecutionResult{
		Version:      api.ExecutionVersion,
		ExecutionID:  r.req.ExecutionID,
		AttemptID:    r.req.AttemptID,
		Termination:  t,
		Observations: append([]api.Observation{}, r.observations...),
		Artifacts:    append([]api.ArtifactRef{}, r.artifacts...),
		Usage:        r.acct.usage(),
		Context:      r.manifest,
		Routing:      r.routing,
		Provenance:   r.provenance,
		EventCount:   int64(len(r.observations)),
	}
	if t.Outcome == api.OutcomeCompleted {
		result.FinalText = r.finalText
	}
	return result
}

func describe(t api.Termination) string {
	s := string(t.Outcome) + "/" + string(t.Cause)
	if t.Dimension != "" {
		s += "/" + string(t.Dimension)
	}
	if t.Detail != "" {
		s += ": " + t.Detail
	}
	return s
}
