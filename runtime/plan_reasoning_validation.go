package runtime

import (
	"errors"
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// validatePlanReasoning refuses a reasoning record that does not carry its own
// proof. A planner invocation whose workspace was not verified afterwards, or
// which ran in a mutating mode, is exactly what the non-mutating boundary
// exists to prevent, so it cannot be recorded as if it had happened safely.
func validatePlanReasoning(reasoning *PlanReasoningPayload) error {
	if reasoning == nil {
		return nil
	}
	if reasoning.InvocationMode != string(domain.InvocationModeNonMutatingPlanning) {
		return fmt.Errorf("planner reasoning ran in mode %q: planning is %q", reasoning.InvocationMode, domain.InvocationModeNonMutatingPlanning)
	}
	if !reasoning.WorkspaceUnchanged {
		return errors.New("planner reasoning recorded a changed planning workspace: a plan may not be built on an invocation that wrote")
	}
	if reasoning.WorkspaceDigestBefore != reasoning.WorkspaceDigestAfter {
		return errors.New("planner reasoning claims an unchanged workspace while its own digests differ")
	}
	return errors.Join(
		required("agent_id", reasoning.AgentID),
		required("provider_kind", reasoning.ProviderKind),
		required("trust_mode", reasoning.TrustMode),
		required("workspace_digest_before", reasoning.WorkspaceDigestBefore),
		required("workspace_digest_after", reasoning.WorkspaceDigestAfter),
		bounded("vendor_family", reasoning.VendorFamily),
		bounded("model", reasoning.Model),
		bounded("profile_id", reasoning.ProfileID),
		bounded("provider_mode", reasoning.ProviderMode),
		validateReasoningInvocation(reasoning.Invocation))
}

func validateReasoningInvocation(o *domain.InvocationObservation) error {
	if o == nil {
		return nil
	}
	return validateInvocationObservation(*o)
}
