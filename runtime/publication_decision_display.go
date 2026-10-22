package runtime

import "strings"

// recordedPublicationDecision is the run's pull request decision wherever it
// was recorded. A read-only projection has no repository, so it cannot name the
// branch decidedPublication keys on and would report a run that WAS authorized
// as having no decision at all.
//
// It is for DISPLAY ONLY - publicationAuthorityOf, which feeds StatusReport and
// nothing that waits, plans or authorizes. It answers only when the run has
// recorded decisions for exactly one target. With several it answers nothing:
// naming one of them would present a decision about a different target as the
// run's publication authority, and "not recorded" is the honest reading.
func (s *runState) recordedPublicationDecision() (AuthorityEvaluatedPayload, bool) {
	var only AuthorityEvaluation
	count := 0
	for key, decision := range s.projection.AuthorityDecisions {
		if !strings.HasPrefix(key, PublicationActionType+"\x00") {
			continue
		}
		only, count = decision, count+1
	}
	if count != 1 {
		return AuthorityEvaluatedPayload{}, false
	}
	return only.AuthorityEvaluatedPayload, true
}
