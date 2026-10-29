package runtime

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const ReasonOperatorSourceRefresh = "operator_source_refresh"

// RequestRunReconciliation wakes the supervisor's existing refill path. Its
// scheduler still decides eligibility, capacity, leases and retry timing.
func (s *Supervisor) RequestRunReconciliation(runID string) error {
	return s.admission.admit(func() error {
		run, found, err := s.deps.Store.Run(runID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("run %s was not found", runID)
		}
		if terminalDisposition(run.Disposition) {
			return &RunTerminalError{RunID: run.ID, Disposition: run.Disposition, Reason: run.Reason, Verb: "resume"}
		}
		engine, err := s.engine(run.Repository, run.AgentID)
		if err != nil {
			return err
		}
		if err := engine.refuseUnlessSucceeded(runID, run); err != nil {
			return err
		}
		s.notifyReconciliation()
		return nil
	})
}

func (s *Supervisor) notifyReconciliation() {
	select {
	case s.freed <- struct{}{}:
	default:
	}
}

// RefreshIssueRun preserves CLI refresh's generation boundary but never
// detaches a plan assignment or a WorkGraph unit from its approved lineage.
func (s *Supervisor) RefreshIssueRun(ctx context.Context, runID string) (StartOutcome, error) {
	var result StartOutcome
	err := s.admission.admit(func() error {
		run, found, err := s.deps.Store.Run(runID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("run %s was not found", runID)
		}
		if run.Plan != nil || run.Orchestration != nil {
			return fmt.Errorf("bound_work_requires_plan_revision: request an approved plan or WorkGraph revision to refresh this work")
		}
		engine, err := s.engine(run.Repository, run.AgentID)
		if err != nil {
			return err
		}
		result, err = engine.refreshIssueRun(ctx, run)
		if err == nil {
			s.notifyReconciliation()
		}
		return err
	})
	return result, err
}

func (r *EngineeringRuntime) refreshIssueRun(ctx context.Context, run EngineeringRun) (StartOutcome, error) {
	if err := ctx.Err(); err != nil {
		return StartOutcome{}, err
	}
	if err := r.refuseUnlessSucceeded(run.ID, run); err != nil {
		return StartOutcome{}, err
	}
	if terminalDisposition(run.Disposition) && !(run.Disposition == Cancelled && run.Reason == ReasonOperatorSourceRefresh) {
		return StartOutcome{}, &RunTerminalError{RunID: run.ID, Disposition: run.Disposition, Reason: run.Reason, Verb: "refresh"}
	}
	prefix, number, ok := strings.Cut(run.Goal, "#")
	issue, err := strconv.Atoi(number)
	if !ok || err != nil || issue < 1 || prefix != "github-issue:"+run.Repository {
		return StartOutcome{}, fmt.Errorf("run %s does not name an exact source issue", run.ID)
	}
	// The bounded identity space is the same one StartIssueRun owns. Looking
	// up the immediate successor makes retries stable even after it completed.
	for generation := 0; generation < maxRunGenerations-1; generation++ {
		id, err := issueRunID(run.Repository, issue, r.deps.ConfigDigest, generation)
		if err != nil {
			return StartOutcome{}, err
		}
		if id != run.ID {
			continue
		}
		successorID, err := issueRunID(run.Repository, issue, r.deps.ConfigDigest, generation+1)
		if err != nil {
			return StartOutcome{}, err
		}
		successor, found, err := r.deps.Store.Run(successorID)
		if err != nil {
			return StartOutcome{}, err
		}
		if found {
			if run.Disposition != Cancelled || run.Reason != ReasonOperatorSourceRefresh {
				return StartOutcome{}, fmt.Errorf("source already has another generation; refresh refused")
			}
			if successor.AgentID != run.AgentID || successor.Repository != run.Repository || successor.Goal != run.Goal {
				return StartOutcome{}, fmt.Errorf("refresh successor binding mismatch")
			}
			return StartOutcome{RunID: successorID, Adopted: true}, nil
		}
		if _, err := CancelRun(r.deps.Store, r.scheduler, r.deps.Clock.Now(), run.ID, ReasonOperatorSourceRefresh); err != nil {
			return StartOutcome{}, err
		}
		result, err := r.StartIssueRun(ctx, issue, AdoptCompatibleGeneration)
		if err != nil {
			return StartOutcome{}, err
		}
		if result.RunID != successorID {
			return StartOutcome{}, fmt.Errorf("refresh created an unexpected generation; inspect current runs before retrying")
		}
		return result, nil
	}
	return StartOutcome{}, fmt.Errorf("run %s has no refreshable next generation under the current configuration", run.ID)
}
