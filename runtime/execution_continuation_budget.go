package runtime

// Execution-invoke resumption identity and the budget ceilings that gate it
// (#508 review: extracted from reconciler.go to keep that file at its
// frozen file-size-baseline.tsv ceiling; a pure move, no behavior change).
//
// bindExecutionInvoke (reconciler.go) asks three of these for the binding it
// wants - unresolvedFeedbackBinding, decisionResumeBinding
// (decision_resumption.go) via unresumedDecisionResumeBinding, and the plain
// checkpoint-continuation and remediation shapes inline - and
// continuationCeilingReached/providerInvocationCeilingReached ask it right
// back for the binding a NEW one would need, which is what makes neither
// ceiling able to drift from what the planner would actually dispatch.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// unresolvedFeedbackBinding re-proposes the exact feedback execution binding
// an earlier attempt for this head already started, when that operation has
// not succeeded. See the call site in bindExecutionInvoke for why this is
// necessary rather than merely convenient.
func (s *runState) unresolvedFeedbackBinding(head string) (string, bool) {
	prefix := "feedback|" + head + "|"
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke || op.State == Succeeded {
			continue
		}
		if binding := bindingOf(op); strings.HasPrefix(binding, prefix) {
			return binding, true
		}
	}
	return "", false
}

// pendingFeedbackKeys is the admitted, applicable, undelivered feedback for the
// current head, in stable order.
func (s *runState) pendingFeedbackKeys() []string {
	var keys []string
	for _, decision := range s.feedbackState().Pending(s.projection.Head()) {
		keys = append(keys, decision.Key)
	}
	sort.Strings(keys)
	return keys
}

// digestOfKeys is a stable identity for a SET of feedback items. The keys
// themselves would make an unbounded idempotency key; their digest is fixed
// width and just as exact.
func digestOfKeys(keys []string) string {
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])[:32]
}

// invocationContinuationPrefix marks an execution binding as continuing
// interrupted work on an exact checkpoint commit. It is part of the durable
// operation identity, which is what makes continuation depth replayable.
const invocationContinuationPrefix = "continuation|"

// isResumptionBinding reports whether binding spends the run's finite
// continuation-depth ceiling: a checkpoint continuation, or (#508 P4b) a
// decision resumption. Both resume a run WITHOUT a brand new EngineeringRun,
// which is exactly the resource continuationLimit() bounds; a decision
// resumption spends the SAME ceiling rather than a second, unbounded one
// (#508 review P4b §3/§5, D5 of the earlier architecture review).
func isResumptionBinding(binding string) bool {
	return strings.HasPrefix(binding, invocationContinuationPrefix) || strings.HasPrefix(binding, decisionResumptionPrefix)
}

// startedContinuationBindings is the set of DISTINCT resumption execution
// bindings durable state shows this run has already started.
//
// It reads operations, not events, because an operation IS the binding: every
// retry of continuation|A reuses one operation with one idempotency key, so
// counting operations counts bindings and counting attempts does not. Nothing
// here looks at checkpoints, commits, provider invocations or reassessments.
func (s *runState) startedContinuationBindings() map[string]bool {
	started := map[string]bool{}
	for _, op := range s.snapshot.Operations {
		if op.Kind != OpExecutionInvoke {
			continue
		}
		if binding := bindingOf(op); isResumptionBinding(binding) {
			started[binding] = true
		}
	}
	return started
}

// providerInvocationCeilingReached reports that this run has spent every
// provider invocation it was created with.
//
// It counts ATTEMPTS - one per execution invocation actually begun - because
// that is what a provider account is charged for. The count comes from the
// projection of durable events, so a restart resumes at the same total rather
// than at zero.
func (s *runState) providerInvocationCeilingReached() bool {
	limit := s.providerInvocationLimit()
	if limit <= 0 {
		return false
	}
	// A ceiling refuses the NEXT invocation; it does not retroactively fail a
	// run that spent its last one productively. Without this, a run whose final
	// permitted invocation completed the candidate read as failed the moment it
	// finished - the continuation ceiling has the same exemption, for the same
	// reason.
	//
	// "Next" is a binding the planner would still DISPATCH: wanted and not yet
	// satisfied. The binding of the invocation that just succeeded stays wanted
	// until its output is committed - an initial binding until the candidate
	// exists - and reading that as a further invocation failed the run before
	// its last permitted work was ever committed (#514).
	key, wanted := bindExecutionInvoke(s)
	if !wanted || s.satisfied(OpExecutionInvoke, key) {
		return false
	}
	return s.providerInvocationsSpent() >= limit
}

// providerInvocationsSpent is the run total MaxProviderInvocations bounds:
// every begun engineering invocation and every handoff repair that reached a
// provider (#492). It is the one definition the ceiling, a successor's
// availability and the remaining-budget view all read.
func (s *runState) providerInvocationsSpent() int {
	return providerInvocationsSpent(s.projection, s.snapshot.Operations)
}

func providerInvocationsSpent(projection RunProjection, operations map[string]RunOperation) int {
	spent := projection.Attempts[OpExecutionInvoke]
	for _, op := range operations {
		if repairReachedProvider(op) {
			spent++
		}
	}
	return spent
}

func (s *runState) providerCeiling() providerCeiling {
	return providerCeiling{limit: s.providerInvocationLimit(), spent: s.providerInvocationsSpent()}
}

// providerInvocationLimit is the run's total, taken from what the run
// persisted. Absent means unbounded, exactly as it does for every run created
// before this bound existed: a run is judged by the budgets it was created
// with, never by whatever is configured now.
func (s *runState) providerInvocationLimit() int {
	if budgets := s.run.Budgets; budgets != nil {
		return budgets.MaxProviderInvocations
	}
	return 0
}

// continuationLimit is the run's continuation bound, taken from durable state.
//
// A run created after #54 persisted an explicit positive budget and is judged
// by it forever, whatever the operator configures later. A run created BEFORE
// #54 persisted nothing, and its absence is not "use the new default": it means
// the run was bounded by the execution-attempt budget, so replaying it has to
// reproduce that. The oldest runs persisted no budgets at all, and for those
// the attempt budget is the configured one, exactly as it was when they ran.
func (s *runState) continuationLimit() int {
	if budgets := s.run.Budgets; budgets != nil {
		if limit := budgets.MaxExecutionContinuations; limit > 0 {
			return limit
		}
		if legacy := budgets.MaxExecutionAttempts; legacy > 0 {
			return legacy
		}
	}
	return s.rt.deps.Budgets.MaxExecutionAttempts
}

// continuationCeilingReached answers the only question the ceiling is about:
// may this run START one more distinct continuation binding?
//
// It asks the planner what binding it wants rather than predicting anything.
// That matters because whether an invocation mutates, completes, or does
// neither is not knowable before it runs - which is exactly why counting
// checkpoints in advance could never express this rule.
//
// Consequences, all of them deliberate:
//
//   - retries of an already-started binding are not refused here at all; they
//     are bounded by that binding's own MaxExecutionAttempts;
//   - a candidate that COMPLETES is never retroactively failed, because a
//     complete head asks for no continuation binding;
//   - the last permitted continuation may finish and go on to assurance;
//   - only a genuinely NEW binding beyond the ceiling is refused, and the
//     checkpoint that asked for it is preserved by not being touched.
func (s *runState) continuationCeilingReached() bool {
	limit := s.continuationLimit()
	if limit <= 0 {
		return false
	}
	binding, wanted := bindExecutionInvoke(s)
	if !wanted || !isResumptionBinding(binding) {
		return false
	}
	started := s.startedContinuationBindings()
	if started[binding] {
		return false
	}
	return len(started) >= limit
}
