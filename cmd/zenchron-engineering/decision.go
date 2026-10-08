package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const decisionUsage = "usage: zenchron-engineering autonomy workgraph resolve <request-id> <outcome> " +
	"[--note <reason>] [--config <path>]"

// resolveDecision is the governed external-authority action (#508): answer
// one live DecisionRequest - a #473 worker-authored message, or a #508
// WorkGraph unit hold - through the supervisor that owns the durable state.
// There is no local path: a decision is resolved against the process that is
// reconciling the work it unblocks, exactly like a plan decision. It is
// reached from `autonomy workgraph resolve`, the one command namespace a
// decision this build can create - a hold - is also placed through.
func resolveDecision(flags autonomyFlags, requestID, outcomeValue string, stdout io.Writer) (int, error) {
	config, err := loadOrchestrationConfig(flags)
	if err != nil {
		return runtime.ExitInvalid, err
	}
	operator, err := config.ResolveOperator()
	if err != nil {
		return runtime.ExitInvalid, err
	}
	if !runtime.SupervisorRunning(config.StateDir) {
		return runtime.ExitInvalid, fmt.Errorf(
			"a decision is resolved against the supervisor that owns the work it unblocks; run `zenchron-engineering serve` first. State directory: %s", config.StateDir)
	}
	delegated, payload, sent, err := delegatePayloadSent(config.StateDir, runtime.ControlRequest{
		Command: runtime.ControlResolveDecision, DecisionID: requestID,
		DecisionOutcomeKind: defaultDecisionOutcomeKind(outcomeValue), DecisionOutcomeValue: outcomeValue,
		Note: flags.Note, Operator: operator.ID,
	})
	if !delegated {
		return runtime.ExitInvalid, fmt.Errorf("the supervisor on %s stopped while this request was being sent; run the command again", config.StateDir)
	}
	if err != nil {
		// A resolution's identity is deterministic from the request alone
		// (orchestration.DecisionResolutionID), so a lost reply is safe to
		// retry: the identical request finds the resolution it already wrote
		// instead of being refused as a conflict.
		if id, idErr := orchestration.DecisionResolutionID(requestID); sent && idErr == nil && errors.Is(err, runtime.ErrControlReplyLost) {
			return runtime.ExitFailed, fmt.Errorf("%w; the resolution, if it was written, is %s - sending this request again is safe", err, id)
		}
		return runtime.ExitFailed, err
	}
	var view runtime.DecisionResolutionView
	if err := json.Unmarshal(payload, &view); err != nil {
		return runtime.ExitFailed, fmt.Errorf("the supervisor's answer is not a decision resolution: %w", err)
	}
	if err := writeJSON(stdout, view); err != nil {
		return runtime.ExitFailed, err
	}
	return runtime.ExitCompleted, nil
}

// defaultDecisionOutcomeKind infers the bounded outcome kind from the answer
// alone: "allow" and "deny" are the only two allow_deny values, so anything
// else is recorded as bounded text. This CLI does not expose a kind override;
// a selected_option answer is recorded as text, which still carries the exact
// chosen value - only the stricter per-kind validation bound is not applied.
func defaultDecisionOutcomeKind(value string) string {
	switch value {
	case orchestration.DecisionAllow, orchestration.DecisionDeny:
		return orchestration.DecisionAllowDeny
	default:
		return orchestration.DecisionText
	}
}
