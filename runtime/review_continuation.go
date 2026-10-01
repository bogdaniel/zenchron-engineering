package runtime

import (
	"sort"
	"time"
)

// ReviewContinuationGrant is a one-time, run-owned envelope. It never changes
// the creation budget, and subsequent comments or restarts cannot renew it.
// ActiveBaseline includes any overrun before the continuation was granted.
type ReviewContinuationGrant struct {
	ActiveBaseline time.Duration `json:"active_baseline"`
	Allowance      time.Duration `json:"allowance"`
	FeedbackDigest string        `json:"feedback_digest"`
}

// Outstanding admissions survive delivery and interrupted execution. Only a
// successful governed publication, or completed no-change execution against the
// published subject, discharges it. Observation, delivery and checkpoints do not.
func (s *runState) outstandingReviewKeys() []string {
	outstanding := map[string]bool{}
	consumed := map[string]bool{}
	completed := map[string]string{}
	publishedHead := ""
	for _, e := range s.events {
		switch e.Type {
		case EventFeedbackObserved:
			var p FeedbackObservedPayload
			if decodeJSON(e.Payload, &p) == nil && p.Admitted {
				outstanding[p.Key] = true
			}
		case EventGitHubPRObserved:
			var p GitHubPRObservedPayload
			if decodeJSON(e.Payload, &p) == nil {
				publishedHead = p.HeadRevision
			}
		case EventExecutionCompleted:
			var p ExecutionCompletedPayload
			if decodeJSON(e.Payload, &p) == nil {
				completed[e.OperationID] = p.SubjectCommit
			}
		case EventFeedbackConsumed:
			var p FeedbackConsumedPayload
			if decodeJSON(e.Payload, &p) == nil {
				for _, key := range p.Keys {
					consumed[key] = true
				}
			}
		case EventOperationAfter:
			var op RunOperation
			if decodeJSON(e.Payload, &op) != nil || op.State != Succeeded {
				continue
			}
			// An EXPLICIT no-change resolution, bound to this attempt's exact
			// delivered keys and exact subject, leaves the already verified and
			// published subject intact: it needs no new commit or PR update.
			//
			// This reads ResolvedFeedback and nothing else (#376). The
			// provider simply returning success with an unchanged workspace is
			// NOT read as a resolution: that shape is indistinguishable from an
			// invocation that deferred unfinished background work and exited,
			// and treating it as completion is the defect this field exists to
			// close. A legitimate no-change outcome must be stated through
			// AdmitFeedbackResolution, never inferred here.
			if op.Kind == OpExecutionInvoke && publishedHead != "" && completed[op.ID] == publishedHead {
				var result mutationResult
				if decodeJSON(op.Result, &result) == nil {
					for _, key := range result.ResolvedFeedback {
						delete(outstanding, key)
					}
				}
			}
			if op.Kind == OpPullRequestUpdate {
				for key := range consumed {
					delete(outstanding, key)
				}
			}
		}
	}
	keys := make([]string, 0, len(outstanding))
	for key := range outstanding {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func (s *runState) reviewContinuationGrant() (ReviewContinuationGrant, bool) {
	for _, e := range s.events {
		if e.Type == EventReviewContinuationGranted {
			var grant ReviewContinuationGrant
			if decodeJSON(e.Payload, &grant) == nil {
				return grant, true
			}
		}
	}
	return ReviewContinuationGrant{}, false
}

func (s *runState) reviewContinuationRemaining(now time.Time) time.Duration {
	grant, ok := s.reviewContinuationGrant()
	if !ok {
		return 0
	}
	spent := s.activeElapsed(now) - grant.ActiveBaseline
	if spent < 0 {
		spent = 0
	}
	remaining := grant.Allowance - spent
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (r *EngineeringRuntime) grantReviewContinuation(s *runState) error {
	if terminalDisposition(s.snapshot.Disposition) || s.merged() {
		return nil
	}
	if _, exists := s.reviewContinuationGrant(); exists {
		return nil
	}
	keys := s.outstandingReviewKeys()
	if len(keys) == 0 {
		return nil
	}
	limit := s.budgets().WallLimit
	elapsed := s.activeElapsed(r.deps.Clock.Now())
	if !runBudgetSpent(limit, elapsed) {
		return nil
	}
	allowance := limit
	if allowance > 30*time.Minute {
		allowance = 30 * time.Minute
	}
	return r.append(s, EventReviewContinuationGranted, "", ReviewContinuationGrant{
		ActiveBaseline: elapsed, Allowance: allowance, FeedbackDigest: digestOfKeys(keys),
	}, nil)
}

// A delivered continuation may await its reviewer without buying further work.
// Any unsatisfied productive operation reinstates the original terminal bound.
func (s *runState) reviewContinuationDelivered() bool {
	if _, granted := s.reviewContinuationGrant(); !granted {
		return false
	}
	if !s.projection.CandidateComplete || s.projection.PullRequest == nil ||
		s.projection.PullRequest.HeadRevision != s.projection.CandidateRevision {
		return false
	}
	for _, spec := range operationSpecs {
		if observationKinds[spec.kind] {
			continue
		}
		key, wanted := spec.bind(s)
		if wanted && key != "" && !s.satisfied(spec.kind, key) {
			return false
		}
	}
	return true
}
