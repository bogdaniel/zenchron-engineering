package runtime

// GitHub feedback is how an operator says more about the work without copying
// anything into a terminal. It is also the largest untrusted input surface the
// runtime has: anyone who can comment on a public pull request can write text
// that a coding agent running under the operator's account would otherwise read
// as engineering direction.
//
// So every model-visible feedback item passes an ADMISSION gate before it can
// reach a worker, and the gate is uniform across the three classes that exist:
// pull-request reviews, pull-request conversation comments, and comments added
// to the source issue after the run was created. A refused item stays fully
// visible as GitHub state and fully auditable here; it simply never enters an
// agent's context.
//
// Four rules shape the design.
//
//   - Admission is decided by the ACTOR's current repository permission, not by
//     what the comment says. Text is never the gate.
//   - Self-loops are prevented by IDENTITY: the runtime knows which comments it
//     authored because it recorded them when it posted them, and bot/service
//     accounts are refused unless an operator allowlisted them. Nothing here
//     matches on message text, which an attacker controls.
//   - An item is delivered ONCE. Admission and consumption are separate durable
//     facts, so a restart, a retry or a re-poll cannot replay a human's review
//     into a second remediation.
//   - Feedback applies to the CURRENT head of the ACTIVE generation. A review
//     of a superseded commit is history, and a frozen generation never wakes up
//     because someone commented on its old pull request.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"time"
)

// FeedbackClass is where an item came from. It is recorded because the operator
// question "why did the worker see this" is answered differently for a review
// of the exact head and for a comment on the source issue.
type FeedbackClass string

const (
	// FeedbackReview is a submitted pull-request review.
	FeedbackReview FeedbackClass = "pull_request_review"
	// FeedbackReviewComment is an inline review comment on a diff.
	FeedbackReviewComment FeedbackClass = "pull_request_review_comment"
	// FeedbackPullRequestComment is a top-level pull-request conversation
	// comment.
	FeedbackPullRequestComment FeedbackClass = "pull_request_comment"
	// FeedbackIssueComment is a comment added to the source issue after the run
	// was created. The pinned issue title and body remain frozen: a comment is
	// a scoped feedback event, never a licence to re-read and replace the
	// snapshot the run was compiled from.
	FeedbackIssueComment FeedbackClass = "issue_comment"
)

// GitHubPermission is an actor's current permission on the repository, in
// GitHub's own vocabulary. The zero value is "unresolved", which is refused:
// an actor whose permission could not be established is never admitted.
type GitHubPermission string

const (
	PermissionUnresolved GitHubPermission = ""
	PermissionNone       GitHubPermission = "none"
	PermissionRead       GitHubPermission = "read"
	PermissionTriage     GitHubPermission = "triage"
	PermissionWrite      GitHubPermission = "write"
	PermissionMaintain   GitHubPermission = "maintain"
	PermissionAdmin      GitHubPermission = "admin"
)

// permissionRank orders GitHub's permission ladder. An unknown spelling ranks
// below everything, so a permission this runtime does not recognize can never
// clear a threshold.
var permissionRank = map[GitHubPermission]int{
	PermissionNone: 0, PermissionRead: 1, PermissionTriage: 2,
	PermissionWrite: 3, PermissionMaintain: 4, PermissionAdmin: 5,
}

// AtLeast reports whether p meets the threshold.
func (p GitHubPermission) AtLeast(threshold GitHubPermission) bool {
	if p == PermissionUnresolved || threshold == PermissionUnresolved {
		return false
	}
	return permissionRank[p] >= permissionRank[threshold]
}

// DefaultFeedbackPermission is the admission threshold when the operator states
// none: collaborator-equivalent write access. It is deliberately not `read`.
// Read access on a public repository is granted to the entire internet, and
// this gate decides what a coding agent running under the operator's own
// account is told to do.
const DefaultFeedbackPermission = PermissionWrite

// FeedbackPolicy is the operator's admission rule. It is operator authority: a
// repository cannot widen who may direct a worker changing it.
type FeedbackPolicy struct {
	// MinPermission is the threshold an actor must meet. Empty means
	// DefaultFeedbackPermission.
	MinPermission GitHubPermission
	// AllowedBots are automation logins an operator explicitly admitted, in
	// GitHub's own login spelling. Everything else that GitHub reports as an
	// App or a bot is refused: a review bot that could direct a coding agent is
	// an unattended actor with the operator's permissions.
	AllowedBots []string
	// SelfLogins are identities that are THIS system - the account the runtime
	// publishes under, and any coding-agent service account the operator knows
	// about. They are refused as feedback so the runtime cannot talk itself
	// into a loop.
	SelfLogins []string
}

func (p FeedbackPolicy) threshold() GitHubPermission {
	if p.MinPermission == PermissionUnresolved {
		return DefaultFeedbackPermission
	}
	return p.MinPermission
}

func (p FeedbackPolicy) allowsBot(login string) bool {
	for _, allowed := range p.AllowedBots {
		if strings.EqualFold(allowed, login) {
			return true
		}
	}
	return false
}

func (p FeedbackPolicy) isSelf(login string) bool {
	for _, self := range p.SelfLogins {
		if strings.EqualFold(self, login) {
			return true
		}
	}
	return false
}

// FeedbackItem is one observed piece of forge feedback, normalized. Body is
// UntrustedText and stays that way: it is data all the way to the delimited
// block a worker reads, and it never reaches a policy decision.
type FeedbackItem struct {
	Class FeedbackClass
	// ID is GitHub's own identifier for the comment or review. It is the
	// durable identity every dedup decision is made on, because it is the only
	// thing about an item that an author cannot change.
	ID     int64
	Actor  GitHubActor
	Body   UntrustedText
	Path   string
	Commit string
	// Bot marks an actor GitHub reports as an App or bot account.
	Bot       bool
	CreatedAt time.Time
}

// Key is the durable identity of one item: its class and its forge id. It is
// what admission, delivery and consumption are all recorded against, so no two
// of them can disagree about which item they mean.
func (i FeedbackItem) Key() string { return string(i.Class) + ":" + strconv.FormatInt(i.ID, 10) }

// FeedbackDecision is one admission outcome with its provenance. It is the
// operator's answer to "why was this comment given to the agent, or not".
type FeedbackDecision struct {
	Key   string        `json:"key"`
	Class FeedbackClass `json:"class"`
	// Actor is the login and numeric id. No display name, no avatar, no email:
	// identity is what the decision was made on.
	Actor      string           `json:"actor"`
	ActorID    int64            `json:"actor_id,omitempty"`
	Permission GitHubPermission `json:"permission,omitempty"`
	Admitted   bool             `json:"admitted"`
	// Reason names the rule that decided. It is runtime-authored text from a
	// closed set, never forge text.
	Reason string `json:"reason"`
	// HeadRevision is the candidate head the item was judged against, and
	// Applicable reports whether the item still describes it. A review of a
	// superseded commit is retained as history and is not delivered.
	HeadRevision string `json:"head_revision,omitempty"`
	Applicable   bool   `json:"applicable"`
	CreatedAt    string `json:"created_at,omitempty"`
}

// Admission reasons. They are a closed vocabulary so an operator reading a
// projection sees the same words the code decided with.
const (
	feedbackAdmittedPermitted   = "actor meets the configured repository permission threshold"
	feedbackRefusedUnresolved   = "the actor could not be resolved, and an unresolved actor is never admitted"
	feedbackRefusedPermission   = "actor is below the configured repository permission threshold"
	feedbackRefusedSelf         = "authored by this runtime, so admitting it would let the system feed itself"
	feedbackRefusedBot          = "authored by an automation account that the operator has not allowlisted"
	feedbackRefusedStale        = "describes a candidate head this run has already moved past"
	feedbackRefusedEmpty        = "carries no text, so there is nothing to deliver"
	feedbackAdmittedAllowedBot  = "authored by an operator-allowlisted automation account"
	feedbackRefusedUnauthorized = "actor has no permission on this repository"
)

// AdmitFeedback decides every item against the policy and the current head. It
// is a PURE function of its inputs: no network call, no clock, no store. That
// is what makes the gate testable against exactly the cases that matter -
// a public commenter, a bot, the runtime itself, a stale review - without a
// forge.
//
// permissions maps a login to that actor's current repository permission. A
// login absent from the map is UNRESOLVED and refused; resolution failure is
// never an admission.
func AdmitFeedback(items []FeedbackItem, policy FeedbackPolicy, permissions map[string]GitHubPermission, head string) []FeedbackDecision {
	decisions := make([]FeedbackDecision, 0, len(items))
	for _, item := range items {
		decision := FeedbackDecision{
			Key: item.Key(), Class: item.Class,
			Actor: item.Actor.Login, ActorID: item.Actor.ID,
			HeadRevision: head,
		}
		if !item.CreatedAt.IsZero() {
			decision.CreatedAt = item.CreatedAt.UTC().Format(time.RFC3339)
		}
		// Applicability is judged first and separately from admission, so a
		// projection can distinguish "this person may direct the worker, but
		// this particular review is about an older commit" from "this person
		// may not direct the worker at all".
		decision.Applicable = feedbackApplies(item, head)
		switch {
		case strings.TrimSpace(item.Actor.Login) == "":
			decision.Reason = feedbackRefusedUnresolved
		case policy.isSelf(item.Actor.Login):
			decision.Reason = feedbackRefusedSelf
		case item.Bot && !policy.allowsBot(item.Actor.Login):
			decision.Reason = feedbackRefusedBot
		case strings.TrimSpace(string(item.Body)) == "":
			decision.Reason = feedbackRefusedEmpty
		case !decision.Applicable:
			decision.Reason = feedbackRefusedStale
		default:
			permission, resolved := permissions[strings.ToLower(item.Actor.Login)]
			decision.Permission = permission
			switch {
			case item.Bot && policy.allowsBot(item.Actor.Login):
				decision.Admitted, decision.Reason = true, feedbackAdmittedAllowedBot
			case !resolved || permission == PermissionUnresolved:
				decision.Reason = feedbackRefusedUnresolved
			case permission == PermissionNone:
				decision.Reason = feedbackRefusedUnauthorized
			case permission.AtLeast(policy.threshold()):
				decision.Admitted, decision.Reason = true, feedbackAdmittedPermitted
			default:
				decision.Reason = feedbackRefusedPermission
			}
		}
		decisions = append(decisions, decision)
	}
	sort.SliceStable(decisions, func(i, j int) bool { return decisions[i].Key < decisions[j].Key })
	return decisions
}

// feedbackApplies reports whether an item still describes the run's current
// head. An item bound to an exact commit applies only to that commit; an item
// with no commit binding - a conversation comment, an issue comment - is about
// the work rather than about a diff, so it applies to whatever head is current.
func feedbackApplies(item FeedbackItem, head string) bool {
	if item.Commit == "" {
		return true
	}
	return head != "" && item.Commit == head
}

// FeedbackObservedPayload is the journalled admission record for one item. It
// is identity and decision only: the untrusted text lives in a local-only
// artifact, exactly as the pinned issue body does, so no durable event row ever
// carries third-party prose.
type FeedbackObservedPayload struct {
	FeedbackDecision
	// TextDigest binds the decision to the exact bytes that were judged, so a
	// later edit of the comment is a different item rather than a silent
	// substitution of what the worker was told.
	TextDigest string `json:"text_digest"`
}

// FeedbackConsumedPayload records that admitted items were actually delivered
// to a worker, and which invocation received them. Delivery is a separate fact
// from admission precisely so a crash between the two cannot lose or duplicate
// a human's review.
type FeedbackConsumedPayload struct {
	Keys []string `json:"keys"`
	// AgentID and Attempt name the exact invocation that received them.
	AgentID     string `json:"agent_id,omitempty"`
	OperationID string `json:"operation_id"`
	Attempt     int    `json:"attempt"`
}

// maxFeedbackKeysPerEvent bounds one consumption record. A single invocation is
// given a bounded number of items - see maxDeliveredFeedbackItems - so this
// ceiling is never reached in practice; it exists so the canonical payload
// bound is a property of the type rather than of the caller's discipline.
const maxFeedbackKeysPerEvent = 64

// maxDeliveredFeedbackItems bounds how many admitted items one invocation is
// given. Feedback is context, not a queue to drain: handing a worker fifty
// review comments at once produces a worse change than handing it the ten most
// recent, and the rest stay admitted and undelivered for the next invocation.
//
// ponytail: newest-first truncation. If a run ever accumulates more applicable
// feedback than this in one head, prioritizing by review state rather than by
// recency is the upgrade.
const maxDeliveredFeedbackItems = 10

// FeedbackState is the replayed feedback position of one run: which items have
// been admitted, and which of those a worker has already been given.
type FeedbackState struct {
	// Admitted is every admission decision this run has recorded, newest last.
	Admitted []FeedbackObservedPayload
	// Consumed is the set of keys already delivered to a worker.
	Consumed map[string]bool
}

// Pending is the admitted, applicable, not-yet-delivered items in stable order.
func (s FeedbackState) Pending(head string) []FeedbackObservedPayload {
	var pending []FeedbackObservedPayload
	for _, decision := range s.Admitted {
		if !decision.Admitted || s.Consumed[decision.Key] {
			continue
		}
		// Applicability is re-checked against the CURRENT head rather than
		// trusted from the recording. A run that moved on between observing a
		// review and delivering it must not hand the worker feedback about a
		// commit that no longer exists.
		if decision.HeadRevision != "" && head != "" && decision.HeadRevision != head {
			continue
		}
		pending = append(pending, decision)
	}
	if len(pending) > maxDeliveredFeedbackItems {
		pending = pending[len(pending)-maxDeliveredFeedbackItems:]
	}
	return pending
}

// Seen reports whether an item has already been judged, so re-polling records
// nothing new. Dedup is by durable forge identity, never by text.
func (s FeedbackState) Seen(key string) bool {
	for _, decision := range s.Admitted {
		if decision.Key == key {
			return true
		}
	}
	return s.Consumed[key]
}

// feedbackState replays the run's feedback position from the journal.
func (s *runState) feedbackState() FeedbackState {
	state := FeedbackState{Consumed: map[string]bool{}}
	for _, event := range s.events {
		switch event.Type {
		case EventFeedbackObserved:
			var payload FeedbackObservedPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Key != "" {
				state.Admitted = append(state.Admitted, payload)
			}
		case EventFeedbackConsumed:
			var payload FeedbackConsumedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				for _, key := range payload.Keys {
					state.Consumed[key] = true
				}
			}
		}
	}
	return state
}

// consumedFeedbackCount and consumedFeedbackDigest are the continuity facts a
// provider transition has to carry: how much admitted feedback the predecessor
// has already been given, bound to exactly which items. A successor that
// re-consumed them would replay a human's review as if it were new.
func (s *runState) consumedFeedbackCount() int { return len(s.feedbackState().Consumed) }

func (s *runState) consumedFeedbackDigest() string {
	consumed := s.feedbackState().Consumed
	if len(consumed) == 0 {
		return ""
	}
	keys := make([]string, 0, len(consumed))
	for key := range consumed {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	sum := sha256.Sum256([]byte(strings.Join(keys, "\n")))
	return hex.EncodeToString(sum[:])
}
