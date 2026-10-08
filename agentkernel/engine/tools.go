package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/agentkernel/api"
	"github.com/bogdaniel/zenchron-engineering/agentkernel/tools"
)

// toolRound dispatches each proposal through the broker. Refusals and tool
// errors go back to the model as tool messages; an unknown side-effect
// outcome, a recording failure or an exhausted bound stops the execution
// before any further side effect.
func (r *run) toolRound(ctx context.Context, calls []api.ToolCall) ([]api.Message, api.Termination, bool) {
	var results []api.Message
	for _, call := range calls {
		if t, stop := r.interrupted(ctx); stop {
			return nil, t, false
		}
		if _, err := r.ledger.reserve(amount{api.DimensionToolCalls, 1}); err != nil {
			return nil, r.exhausted(api.DimensionToolCalls, "no tool calls remain"), false
		}
		r.count(func(a *account) { a.toolCalls++ })
		if err := r.emit(ctx, api.Event{Kind: api.EventToolProposed, ToolCall: call.ID, Detail: call.Name}); err != nil {
			return nil, r.recordingFailed(""), false
		}
		res, err := r.dispatch(ctx, call)
		if t, stop := r.afterDispatch(call, err); stop {
			r.observeUncertainMutation(ctx, call, res, err)
			return nil, t, false
		}
		kind := api.EventToolExecuted
		if res.Status == api.ToolRefused {
			kind = api.EventToolRefused
		}
		ev := api.Event{Kind: kind, ToolCall: call.ID, Grant: res.Grant, Ref: res.FullOutput, Detail: string(res.Status) + " " + res.Error}
		if err := r.emit(ctx, ev); err != nil {
			return nil, r.recordingFailed("tool " + call.Name + " " + string(res.Status)), false
		}
		results = append(results, renderToolResult(res))
	}
	return results, api.Termination{}, true
}

// dispatch runs one proposal through the broker. Its hand-off ID and
// artifact producer are a kernel sequence ("<execution>/<attempt>/tool-<n>"),
// never the model's tool-call ID, which is model output: it could repeat or
// mimic another port's ID. A proposal repeating an earlier tool-call ID of
// this attempt is refused back to the model and never dispatched.
func (r *run) dispatch(ctx context.Context, call api.ToolCall) (api.ToolResult, error) {
	if r.e.broker == nil {
		return api.ToolResult{CallID: call.ID, Status: api.ToolRefused, Error: "no tools are available in this execution"}, nil
	}
	if r.toolCallIDs[call.ID] {
		return api.ToolResult{CallID: call.ID, Status: api.ToolRefused,
			Error: fmt.Sprintf("tool call id %q repeats an earlier call of this execution; not run", call.ID)}, nil
	}
	r.toolCallIDs[call.ID] = true
	r.dispatched++
	return r.e.broker.Dispatch(ctx, call, tools.Env{
		Grants:      r.req.Grants,
		Mode:        r.req.Mode,
		Artifacts:   &meteredStore{inner: r.e.artifacts, r: r},
		Producer:    fmt.Sprintf("%s/%s/tool-%d", r.req.ExecutionID, r.req.AttemptID, r.dispatched),
		OutputLimit: r.e.outputLimit,
		StepBudget:  r.chargeStep,
	})
}

// chargeStep reserves one tool call for each composite-tool step beyond the
// first, so a macro is metered like the individual calls it performs.
func (r *run) chargeStep(context.Context) error {
	if _, err := r.ledger.reserve(amount{api.DimensionToolCalls, 1}); err != nil {
		return fmt.Errorf("no tool calls remain")
	}
	r.count(func(a *account) { a.toolCalls++ })
	return nil
}

// afterDispatch orders the stop conditions a dispatch can raise: a failed
// recording first (evidence is incomplete), then an artifact budget refusal,
// then an unknown side-effect outcome.
func (r *run) afterDispatch(call api.ToolCall, err error) (api.Termination, bool) {
	if r.recordingFailure() != nil {
		return r.recordingFailed("tool " + call.Name + " dispatched"), true
	}
	r.mu.Lock()
	dim := r.exhaustedBy
	r.mu.Unlock()
	if dim != "" {
		return r.exhausted(dim, "tool "+call.Name+" could not record its output"), true
	}
	if err != nil {
		return r.termination(api.OutcomeFailed, api.CauseToolFailed, fmt.Sprintf("tool %s: %v", call.Name, err)), true
	}
	return api.Termination{}, false
}

var errArtifactBudget = errors.New("artifact byte budget exhausted")

// meteredStore reserves artifact bytes before each write and records every
// stored artifact, so tools cannot spend past the envelope or store evidence
// the result does not reference.
type meteredStore struct {
	inner api.ArtifactStore
	r     *run
}

func (s *meteredStore) Put(ctx context.Context, in api.ArtifactInput) (api.ArtifactRef, error) {
	n := int64(len(in.Data))
	if dim, err := s.r.ledger.reserve(amount{api.DimensionArtifactBytes, n}); err != nil {
		s.r.mu.Lock()
		s.r.exhaustedBy = dim
		s.r.mu.Unlock()
		return api.ArtifactRef{}, errArtifactBudget
	}
	ref, err := s.inner.Put(ctx, in)
	if err != nil {
		s.r.ledger.settle(api.DimensionArtifactBytes, n, 0)
		return api.ArtifactRef{}, err
	}
	s.r.mu.Lock()
	s.r.artifacts = append(s.r.artifacts, ref)
	s.r.acct.artifactBytes += ref.Size
	s.r.mu.Unlock()
	if err := s.r.emit(ctx, api.Event{Kind: api.EventArtifactRecorded, Ref: &ref, Detail: in.Producer}); err != nil {
		return ref, err
	}
	return ref, nil
}

func (s *meteredStore) Get(ctx context.Context, ref api.ArtifactRef) ([]byte, error) {
	return s.inner.Get(ctx, ref)
}

// observeUncertainMutation records that a possibly mutating tool ran before the
// execution stopped, so the host never reads "exhausted" or "failed" as "no
// side effect happened". A recording failure leaves nothing more to record.
func (r *run) observeUncertainMutation(ctx context.Context, call api.ToolCall, res api.ToolResult, err error) {
	if !res.Mutated || r.recordingFailure() != nil {
		return
	}
	detail := "side effect outcome uncertain: " + string(res.Status)
	if err != nil {
		detail += " " + err.Error()
	}
	// The settlement that follows reports a recording failure if this fails.
	_ = r.emit(ctx, api.Event{Kind: api.EventToolExecuted, ToolCall: call.ID, Grant: res.Grant, Detail: detail})
}
