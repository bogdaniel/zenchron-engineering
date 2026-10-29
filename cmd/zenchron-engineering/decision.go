package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/bogdaniel/zenchron-engineering/orchestration"
	"github.com/bogdaniel/zenchron-engineering/runtime"
)

const decisionUsage = "usage: zenchron-engineering autonomy workgraph resolve <request-id> <outcome> " +
	"[--kind allow_deny|selected_option|text] [--note <reason>] [--config <path>]"

// extractKindFlag pulls an optional leading "--kind VALUE" out of resolve's
// own argument list before the shared autonomyFlags parser ever sees it
// (#508 review B5): the outcome's shape is a property of the control action,
// not an operator flag every other `autonomy` command also carries.
func extractKindFlag(args []string) (kind string, rest []string, err error) {
	if len(args) >= 2 && args[0] == "--kind" {
		if strings.TrimSpace(args[1]) == "" {
			return "", nil, errors.New(decisionUsage)
		}
		return args[1], args[2:], nil
	}
	return "", args, nil
}

// resolveDecision is the governed external-authority action (#508): answer
// one live DecisionRequest - a #473 worker-authored message, or a #508
// WorkGraph unit hold - through the supervisor that owns the durable state.
// There is no local path: a decision is resolved against the process that is
// reconciling the work it unblocks, exactly like a plan decision. It is
// reached from `autonomy workgraph resolve`, the one command namespace a
// decision this build can create - a hold - is also placed through.
//
// kind is the explicit --kind override; empty lets defaultDecisionOutcomeKind
// infer one from the answer text. The request's own contract - not whoever is
// answering it - is supposed to prescribe this; #473's decision_request
// carries no such member today (docs/orchestration.md names the exact
// boundary), so an explicit flag is the most this CLI can offer until it
// does. orchestration.ResolveDecision still enforces the one case #508 DOES
// own: a WorkUnitHold always requires allow_deny, whatever is passed here.
func resolveDecision(flags autonomyFlags, requestID, outcomeValue, kind string, stdout io.Writer) (int, error) {
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
	if strings.TrimSpace(kind) == "" {
		kind = defaultDecisionOutcomeKind(outcomeValue)
	}
	delegated, payload, sent, err := delegatePayloadSent(config.StateDir, runtime.ControlRequest{
		Command: runtime.ControlResolveDecision, DecisionID: requestID,
		DecisionOutcomeKind: kind, DecisionOutcomeValue: outcomeValue,
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

// defaultDecisionOutcomeKind infers the bounded outcome kind an operator did
// not state explicitly via --kind: "allow" and "deny" are the only two
// allow_deny values, so anything else defaults to bounded text. A
// selected_option answer needs --kind selected_option stated explicitly;
// without it, the value is still recorded exactly, as text.
func defaultDecisionOutcomeKind(value string) string {
	switch value {
	case orchestration.DecisionAllow, orchestration.DecisionDeny:
		return orchestration.DecisionAllowDeny
	default:
		return orchestration.DecisionText
	}
}
