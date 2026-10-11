package runtime

import (
	"encoding/json"
	"sort"

	"github.com/bogdaniel/zenchron-engineering/domain"
)

// humanDecision reports whether A PERSON decided this gate for this run.
//
// It reads the run's recorded human authority evidence and nothing else. An
// authorized #7 decision is NOT accepted in its place: authority is authorized
// whenever nothing is missing, which ordinary machine evidence can reach on its
// own, so a producing run's auto-authorized publication could discharge the
// very gate that stood in for a blocked independence obligation. A gate that
// says a person decided has to be able to name the person's evidence.
//
// The evidence is the kernel's existing HumanAuthorityRecorded record - pinned
// to a request, a candidate, a contract and a state digest - so the plan
// invents no second approval system.
func humanDecision(events []EngineeringEvent, action *domain.Action, current RunSnapshot) (humanDecisionReference, bool) {
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Type != EventHumanAuthorityRecorded {
			continue
		}
		var payload HumanAuthorityRecordedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			// An unreadable decision is not an absent one. Walking past it
			// reaches an OLDER approval of the same head and satisfies the
			// gate - an error answering the question "did a person approve
			// this", which is the class this file swept elsewhere.
			return humanDecisionReference{}, false
		}
		if action != nil && payload.Action != *action {
			continue
		}
		// A plan gate must consume the same candidate and contract that the
		// kernel bound into the person's authority request.
		if payload.Candidate != current.Candidate || payload.Contract != current.Contract {
			continue
		}
		if err := payload.Binding(current.ID).Validate(current); err != nil {
			return humanDecisionReference{}, false
		}
		// The NEWEST matching decision governs. Skipping a rejection to keep
		// looking would let a gate be satisfied by an approval the person has
		// since reversed, which is the opposite of what a human decision gate
		// is for.
		if payload.Decision != "approve" {
			return humanDecisionReference{}, false
		}
		return humanDecisionReference{
			decision:        Ref{ID: payload.Request.ID, Revision: payload.Request.Revision},
			humanEvidenceID: payload.EvidenceID,
			claims:          payload.Requires,
		}, true
	}
	return humanDecisionReference{}, false
}

// upstreamRuns are the child runs a gate judges: the runs of the agent stages
// it transitively depends on. Transitive, because a gate placed after another
// gate still judges the work, and the work is upstream of both.
func upstreamRuns(stage domain.PlanStage, plan domain.EngineeringPlan, snapshot PlanSnapshot) []string {
	ids := make([]string, 0)
	for _, id := range upstreamStageIDs(stage, plan) {
		if projection, ok := snapshot.Stages[id]; ok && projection.RunID != "" {
			ids = append(ids, projection.RunID)
		}
	}
	sort.Strings(ids)
	return ids
}

func publicationCandidateRuns(gate domain.PlanStage, plan domain.EngineeringPlan, snapshot PlanSnapshot) []string {
	ancestors := upstreamStageIDs(gate, plan)
	candidates := make([]string, 0, len(ancestors))
	for _, id := range ancestors {
		stage, ok := plan.Stage(id)
		if ok && stage.Kind == domain.StageAgent && stage.InvocationMode == domain.InvocationModeMutating && !stage.Role.ProducesReviewVerdict() {
			candidates = append(candidates, id)
		}
	}
	final := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		consumed := false
		for _, later := range candidates {
			if candidate != later && planStageDependsOn(plan, later, candidate) {
				consumed = true
				break
			}
		}
		if !consumed {
			final = append(final, candidate)
		}
	}
	if len(final) != 1 {
		return nil
	}
	projection, ok := snapshot.Stages[final[0]]
	if !ok || projection.RunID == "" {
		return nil
	}
	return []string{projection.RunID}
}

func upstreamStageIDs(stage domain.PlanStage, plan domain.EngineeringPlan) []string {
	seen := map[string]bool{}
	var walk func(string)
	walk = func(id string) {
		if seen[id] {
			return
		}
		seen[id] = true
		current, ok := plan.Stage(id)
		if !ok {
			return
		}
		for _, dependency := range current.DependsOn {
			walk(dependency)
		}
	}
	for _, dependency := range stage.DependsOn {
		walk(dependency)
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func planStageDependsOn(plan domain.EngineeringPlan, stageID, dependencyID string) bool {
	stage, ok := plan.Stage(stageID)
	if !ok {
		return false
	}
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(id string) bool {
		if id == dependencyID {
			return true
		}
		if seen[id] {
			return false
		}
		seen[id] = true
		current, ok := plan.Stage(id)
		if !ok {
			return false
		}
		for _, dependency := range current.DependsOn {
			if walk(dependency) {
				return true
			}
		}
		return false
	}
	for _, dependency := range stage.DependsOn {
		if walk(dependency) {
			return true
		}
	}
	return false
}
