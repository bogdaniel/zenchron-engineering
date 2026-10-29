package runtime

import "testing"

func TestRecordedPublicationDecisionNeverPicksBetweenTargets(t *testing.T) {
	decision := func(target string) AuthorityEvaluation {
		var evaluated AuthorityEvaluation
		evaluated.Action.Type, evaluated.Action.Target = PublicationActionType, target
		return evaluated
	}
	for name, test := range map[string]struct {
		decisions map[string]AuthorityEvaluation
		want      bool
	}{
		"none":                   {map[string]AuthorityEvaluation{}, false},
		"one target":             {map[string]AuthorityEvaluation{PublicationActionType + "\x00main": decision("main")}, true},
		"two targets":            {map[string]AuthorityEvaluation{PublicationActionType + "\x00main": decision("main"), PublicationActionType + "\x00release": decision("release")}, false},
		"another action's entry": {map[string]AuthorityEvaluation{"git.push\x00main": decision("main")}, false},
	} {
		state := &runState{projection: RunProjection{AuthorityDecisions: test.decisions}}
		if _, got := state.recordedPublicationDecision(); got != test.want {
			t.Errorf("%s: answered=%v, want %v", name, got, test.want)
		}
	}
}
