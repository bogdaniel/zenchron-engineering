package runtime

import "strings"

// recordedPublicationDecision is the run's pull request decision wherever it
// was recorded. A read-only projection has no repository, so it cannot name the
// branch decidedPublication keys on and would report a run that WAS authorized
// as having no decision at all. Display only: nothing waits or acts on this.
// ponytail: newest of the recorded pull request decisions; one branch per run
// today, so there is exactly one.
func (s *runState) recordedPublicationDecision() (AuthorityEvaluatedPayload, bool) {
	var newest AuthorityEvaluation
	found := false
	for key, decision := range s.projection.AuthorityDecisions {
		if !strings.HasPrefix(key, PublicationActionType+"\x00") || (found && decision.Sequence < newest.Sequence) {
			continue
		}
		newest, found = decision, true
	}
	return newest.AuthorityEvaluatedPayload, found
}
