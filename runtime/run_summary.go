package runtime

import "time"

func summarizeRunEvents(stateDir string, run EngineeringRun, events []EngineeringEvent, now time.Time) RunSummary {
	summary := initialRunSummary(stateDir, run, now)
	snapshot, err := Reduce(run, events)
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	projection, err := Project(events)
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	// The journal is the authority for the agent binding; the row is only a
	// projection of it, so a row that somehow disagrees loses.
	state := &runState{run: run, snapshot: snapshot, events: events, projection: projection}
	agent, err := state.recordedAgent()
	if err != nil {
		summary.Error = boundedDetail(err.Error())
		return summary
	}
	if agent.AgentID != "" {
		summary.Agent, summary.ProviderKind = agent.AgentID, agent.Kind
		summary.TrustMode, summary.Model = agent.TrustMode, agent.Model
	}
	summary.Disposition, summary.Reason = snapshot.Disposition, snapshot.Reason
	summary.producerFinished = state.producerStageFinished()
	summary.CandidateRevision, summary.CandidateTree = projection.CandidateRevision, projection.CandidateTree
	summary.Attempts = projection.Attempts
	summary.Held = snapshot.HeldMaterial != nil
	summary.Paused = snapshot.Paused
	if operation, ok := state.currentOperation(); ok {
		summary.Operation, summary.Attempt = operation.Kind, operation.Attempt
	}
	if pr := projection.PullRequest; pr != nil {
		summary.PullRequest, summary.PRState = pr.Number, pr.State
	}
	if ci := projection.CI; ci != nil && !ci.Stale {
		summary.CI = ci.Conclusion
	}
	if review := projection.Review; review != nil && !review.Stale {
		summary.Review = review.State
	}
	feedback := state.feedbackState()
	for _, decision := range feedback.Admitted {
		if decision.Admitted {
			summary.FeedbackAdmitted++
		}
	}
	summary.FeedbackPending = len(feedback.Pending(projection.Head()))
	return summary
}

func initialRunSummary(stateDir string, run EngineeringRun, now time.Time) RunSummary {
	summary := RunSummary{
		RunID: run.ID, Repository: run.Repository, Agent: run.AgentID,
		Phase: run.Phase, Disposition: run.Disposition, Reason: run.Reason,
		Branch: run.Candidate.Branch, Elapsed: now.Sub(run.CreatedAt),
	}
	if issue, err := issueNumberOf(run.Goal); err == nil {
		summary.Issue = issue
	}
	if dir := candidateDir(stateDir, run.ID); dirExists(dir) {
		summary.Workspace = dir
	}
	return summary
}
