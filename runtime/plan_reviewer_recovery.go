package runtime

import (
	"fmt"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// Automatic recovery of a failed REVIEWER INVOCATION (#669).
//
// A reviewer whose invocation fails - it answered in a shape the protocol
// refuses, timed out, or hit a transient provider fault - has not reviewed
// anything. That is a fact about the invocation, not a verdict about the
// candidate, and treating it as one is how an objective whose implementer and
// assurance were fine stranded behind a formatting failure (#659).
//
// So the stage gets exactly ONE further performance, through the machinery a
// stage already uses to be performed again: its execution generation advances,
// the failed run is retired, and the next pass freezes the stage's assignment
// for the new generation and starts a new run. Nothing here is a scheduler,
// store or record of its own.
//
// What is preserved is what the existing renewal boundary preserves, and more:
// the retry is frozen from the assignment the failed performance already held
// (the resolution is not recomputed), so it is the same reviewer, profile,
// worker, independence requirement, obligations and exact upstream candidate
// and tree; refusePrivilegeChange still proves that structurally before the
// run exists. The implementer is not touched, no candidate changes, and no
// verdict is invented: the retried reviewer decides, or fails, like any other.
//
// What is bounded is the plan's own envelope. The retry is a child run and its
// invocations are charged to the plan exactly as any run's are, so a plan that
// has spent its child-run or invocation ceiling settles the stage FAILED rather
// than retrying, and nothing renews a spent budget.

// reviewerPerformancesPerSubject is how many times one reviewer stage may be
// performed against ONE exact candidate: the original and one automatic retry.
const reviewerPerformancesPerSubject = 2

// reviewerFailureRecoverable is the ONE rule for which failed reviewer runs
// earn the retry, decided from the typed failure class and never from provider
// prose.
//
// The class set is the one the runtime already routes to a retry (RouteRetry:
// a protocol failure, a transient provider or infrastructure fault, a provider
// that made no progress, connectivity). Everything else is excluded by the same
// routing: a cancelled run or a passed deadline stops, a quota or an absent
// credential waits for a person, and a verdict about the candidate - a blocking
// review, an authority refusal - is not a failed invocation at all.
//
// A run that ended on a run-level BUDGET (wall time, lifecycle, continuations,
// provider invocations, assurance) is excluded whatever its last class was:
// that boundary is the approved limit, and a retry would be a way around it. A
// run whose own attempt budget ran out ("..._attempts_exhausted") is NOT
// excluded, because that is simply how repeated retryable failures end; the
// retry is a new run charged against the plan's envelope, not a renewal of the
// failed run's budget.
func reviewerFailureRecoverable(run EngineeringRun, diagnostic *ExecutionDiagnostic) bool {
	if run.Disposition != Failed || budgetReasons[run.Reason] || diagnostic == nil {
		return false
	}
	if diagnostic.Bound == BoundRunActiveWork || diagnostic.Bound == BoundAssuranceWork {
		return false
	}
	return RouteFailure(diagnostic.FailureClass) == RouteRetry
}

// retryFailedReviewer returns the settlement reason when a FAILED reviewer
// stage should be performed once more instead of settling failed, and the empty
// string when it should settle failed. Every doubt answers empty: a stage that
// cannot prove it is eligible keeps its real failure.
func (r PlanReconciler) retryFailedReviewer(plan domain.EngineeringPlan, stage domain.PlanStage, projection PlanStageProjection, snapshot PlanSnapshot, run EngineeringRun) (string, error) {
	if stage.Kind != domain.StageAgent || !stage.Role.ProducesReviewVerdict() {
		return "", nil
	}
	projected, err := r.runProjection(run.ID)
	if err != nil {
		return "", err
	}
	if !reviewerFailureRecoverable(run, projected.ExecutionDiagnostic) {
		return "", nil
	}
	// The envelope is checked HERE as well as at the start, so a retry the plan
	// cannot afford leaves the stage failed with its real failure instead of
	// invalidated and blocked forever behind a budget message.
	if snapshot.Consumed.ChildRuns >= plan.BudgetEnvelope.MaxChildRuns ||
		invocationCeilingReached(plan, snapshot, stage.ID) != nil {
		return "", nil
	}
	assignment, found, err := r.Service.frozenAssignment(plan, projection, stage.ID)
	if err != nil || !found {
		return "", err
	}
	subject, ok, err := r.reviewSubject(assignment)
	if err != nil || !ok {
		return "", err
	}
	performed, err := r.reviewerPerformances(plan, stage.ID, projection.Generation, subject)
	if err != nil || performed >= reviewerPerformancesPerSubject {
		return "", err
	}
	return boundedDetail(fmt.Sprintf(
		"the reviewer invocation failed (%s) and reached no verdict; one automatic retry of the same reviewer assignment against the same candidate",
		projected.ExecutionDiagnostic.FailureClass)), nil
}

// reviewerPerformances counts the runs this stage has actually had against one
// exact candidate and tree, from the durable assignments and runs rather than
// from a counter that could be reset: replay and restart read the same answer.
//
// ponytail: scans the stage's generations and the run table, fine for the
// handful of generations a stage has and reached only when a reviewer run fails;
// index runs by plan stage if that path ever becomes hot.
func (r PlanReconciler) reviewerPerformances(plan domain.EngineeringPlan, stageID string, current int, subject domain.UpstreamOutput) (int, error) {
	performed := 0
	for generation := 0; generation <= current; generation++ {
		assignment, found, err := r.Store.PlanAssignment(plan.ID, plan.Revision, generation, stageID)
		if err != nil {
			return 0, err
		}
		if !found {
			continue
		}
		earlier, ok, err := r.reviewSubject(assignment)
		if err != nil {
			return 0, err
		}
		if !ok || earlier.Candidate != subject.Candidate || earlier.Tree != subject.Tree {
			continue
		}
		started, err := r.startedRun(plan, stageID, generation)
		if err != nil {
			return 0, err
		}
		if started {
			performed++
		}
	}
	return performed, nil
}
