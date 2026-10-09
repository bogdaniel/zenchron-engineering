# Automatic review-to-remediation routing (#474)

This document is the #474 half of the loop `docs/review.md` (#233) ends with:
"Nothing in this document grants #474 authority it does not independently
establish." It covers exactly one thing: the durable authorization gate
between an independently reached `REQUEST_CHANGES` and a producer's own
bounded remediation invocation, and the exact, still-pending wiring that
turns an admission into a scheduled invocation.

## Ownership map (what already existed)

Investigated against `main` at `adf1697976091212fa1da86ec5a2461d6978ce5a`
(merged #508, #233) before any #474 code was written, per #474's own
"Mandatory architecture discovery" requirement and the prior PR #543
HOLD/revert history recorded on the issue.

| Responsibility | Owner | Evidence |
| --- | --- | --- |
| Independent review operation, exact-head binding, durable `review.Decision`, GitHub publication | #233 | `runtime/review_port.go`, `runtime/review_run.go`, `review/decision.go` |
| Reviewer/producer independence | #233 | `runtime/review_independence.go` (`CheckReviewIndependence`), enforced before a reviewer is ever dispatched |
| Bounded same-`RunID` producer re-invocation, with no provider-session continuity (every invocation is a fresh `ExecutionRequest`) | #63 | `runtime/reconciler.go`'s `bindExecutionInvoke` (remediation\|..., feedback\|... bindings), `runtime/operations.go`'s `invokeExecution` |
| GitHub-feedback-to-remediation admission, including the self-loop guard that refuses the runtime's own publishing identity | #63 | `runtime/feedback.go` (`AdmitFeedback`, `FeedbackPolicy.isSelf`) |
| WorkGraph dependency satisfaction (admitted handoff only), frontier computation, exact-subject invalidation | #472 | `orchestration/workgraph_projection.go` (`ProjectWorkGraph`) |
| #508 `DecisionRequest`/`DecisionResolution`, `allow_deny` holds | #508 | `orchestration/decision_resolution.go`, `orchestration/workgraph_hold.go` |
| Typed, batch-scoped worker messages (`Finding`, `DecisionRequest`, ...) | #473 | `orchestration/communication.go` |

**Gap confirmed by direct source reading, not assumption:** nothing in the
codebase at that revision read `review.Decision`/`review.Verdict` anywhere
outside the `review` and `runtime` review-execution files. No WorkUnit schema
concept of a "review obligation" exists. `bindExecutionInvoke` had exactly
three producer-remediation branches (continuation, current-head-failure
remediation, GitHub feedback) and no fourth. This is the one missing
responsibility #474 owns: deciding whether an independently reached
`REQUEST_CHANGES` may become an authorized remediation trigger, and
plugging that fact into the existing #63 re-invocation path - not a new
review engine, not a new scheduler, not a new messaging bus.

## The admission gate

`(*runtime.EngineeringRuntime).AdmitReviewRemediation` (`runtime/review_remediation.go`)
is the whole gate. It takes only `(ctx, ReviewPort, repo, prNumber)` - never a
caller-supplied `review.Decision` - and resolves the decision itself from the
durable store through `ReviewPort.LatestDecision`/`IsStale`. That is a
deliberate API choice, not an oversight: there is no parameter a forged or
stale Decision-shaped argument could ever occupy.

It fails closed, in order, on:

1. no decision exists yet for this (repo, PR) (`no_independent_decision`);
2. the decision is not `REQUEST_CHANGES` (`decision_not_blocking`) - an
   `APPROVE` or `COMMENT_ONLY` is never routed as a block;
3. the decision names the same agent as producer and reviewer
   (`reviewer_not_independent`) - defense in depth: #233 already refuses to
   construct such a Decision, so this re-check only matters against a
   corrupted or hand-crafted store row, never against a correctly-produced
   one;
4. the decision is stale against the PR's current head (`subject_stale`);
5. the producing run cannot be loaded (`run_unknown`);
6. the run is terminal - completed, failed or cancelled (`run_terminal`);
7. the run has no compiled contract (`no_compiled_contract`);
8. the run's **current** candidate no longer matches the decision's exact
   reviewed head (`candidate_superseded`) - the run moved on its own, even if
   the PR's last-observed head still agrees with the decision.

GitHub publication state is never consulted (`docs/review.md`, "The #474
interface"): a local `REQUEST_CHANGES` with no publication, a disabled
`--publish`, or an uncertain GitHub outcome all admit identically, because
`ReviewOutcome.Decision.Verdict` - not `Published`/`PublishedVerdict` - is
the fact #474 routes on.

### Durable identity and idempotency

One `ReviewRemediationAdmission` row per `review.Decision.ID`, ever, enforced
by a `PRIMARY KEY` + `ON CONFLICT(decision_id) DO NOTHING` insert
(`review_remediation_admissions` table, `runtime/review_remediation_store.go`).
Since `DecisionID` is itself deterministic over the exact `(repository,
pr_number, head_sha, reviewer_agent_id)` tuple (`review.DecisionID`), this is
also the identity that prevents two different heads, or H1 reviewed twice,
from ever sharing a remediation budget envelope. A second admission attempt
for an already-admitted decision - from the same caller, a concurrent one, or
a caller after a restart - returns the existing row with `created=false`
rather than minting a second authorization.

On success, the fact is **also** journalled on the producing run's own event
stream (`EventReviewRemediationAdmitted`), so the run's existing in-package
event-folding can plan from it without any further store I/O - the same
shape `unresolvedFeedbackBinding`/`pendingFeedbackKeys` already use for
admitted GitHub feedback.

`AdmitReviewRemediation` touches no `RunOperation` row, consumes no budget,
and schedules nothing (`TestAdmitReviewRemediationTouchesNoOperation`).
Admission is a fact; turning a fact into a scheduled, budgeted invocation
remains entirely the existing reconciler's job.

## Deferred wiring (not yet applied - see below)

Two existing files are the integration seam, and both are presently PR
#546's (#475) active files. The diffs below are complete, reviewed and
tested against the helper methods already shipped in this PR
(`runtime/review_remediation.go`); they are being held rather than applied,
per #474's own "Critical concurrent-development constraint" and this
session's explicit coordination with the #475 agent (acknowledged: one
`ExecutionKindIntegrationCompose` early-return guard added to
`bindExecutionInvoke`'s top, no other control-flow changes, composes cleanly
with the addition below).

**1. `runtime/reconciler.go`, inside `bindExecutionInvoke`** - a new branch at
the same priority tier as the existing GitHub-feedback branches, inserted
after them and before the `#508 P4b` fallback:

```go
if binding, ok := s.unresolvedReviewRemediationBinding(s.projection.CandidateRevision); ok {
    return binding, true
}
if pending := s.pendingReviewRemediationKeys(); len(pending) > 0 {
    return reviewRemediationBindingPrefix + s.projection.CandidateRevision + "|" + digestOfKeys(pending), true
}
```

Both helper methods, and the `reviewRemediationBindingPrefix` constant, are
already implemented and unit-tested in `runtime/review_remediation.go` -
this is a two-line consumer of code that already exists, not new logic.

**2. `runtime/operations.go`, inside `invokeExecution`'s context assembly** -
the admitted blocking findings (`ReviewRemediationAdmission.FindingSignatures`,
or the full `review.Decision.Findings` read back by `DecisionID`) need to
reach the provider the same way `feedbackDeliveryFor` already delivers
admitted GitHub feedback: framed and neutralized as untrusted data
(`UNTRUSTED-REVIEW-CONTEXT`/`UNTRUSTED-INTERWORKER-MESSAGES` precedent),
never as instructions. This is intentionally **not** written against #546's
current `invokeExecution` body in this PR: that function is #475's active
file, and the exact shape of "attach findings for binding
`review-remediation|...`" needs to be read off whatever `invokeExecution`
looks like once #546 lands, not guessed at against a moving target.

Why not applied now: `runtime/reconciler.go` and `runtime/operations.go` are
both open PR #546's actively-edited files. The host environment's own
shared-resource safeguard refused this session's attempt to edit
`reconciler.go` even read-only, which is the correct outcome here - a
two-line, already-tested, already-reviewed diff is still a diff to a file
another agent is mid-flight on, and the right place for it to land is a
follow-up commit once #546 merges (or an explicit human-approved exception).

## WorkGraph review-readiness (section 8): a frozen, deferred contract

`orchestration/workgraph_projection.go`'s `ProjectWorkGraph` marks a unit
`satisfied` purely from its admitted handoff (`docs/workgraph.md`); there is
no concept of "activated, completed, but not yet independently accepted" a
downstream unit's readiness could gate on. Investigated directly (not
assumed): no `review_gate`/`review_obligation` field exists on `WorkUnit` or
its projection anywhere in the codebase.

Adding one is a WorkGraph-schema change to files `orchestration/workgraph.go`
and `runtime/workgraph_status.go` presently own (#475's active files), and
#474's own governing comment thread on the issue already HELD exactly this
kind of change once before (PR #543) for widening scope without an agreed
contract. This PR does not implement it. The frozen contract, for whoever
picks this up once #546 lands:

- a `WorkUnit` MAY declare `requires_review: true` (opt-in; every existing
  WorkGraph with no such unit behaves identically to today - "preserve
  existing behavior for ordinary WorkGraphs" is non-negotiable per #474);
- such a unit's `satisfied` fact becomes: admitted handoff **AND**
  `!ReviewPort.NeedsReview(...)` **AND** the latest `review.Decision` for its
  bound PR is `APPROVE` (never routed through `AdmitReviewRemediation`, which
  is REQUEST_CHANGES-only machinery - acceptance is a #233 fact read
  directly, not an admission);
- a fresh candidate head (a new admitted handoff on the same unit's run, same
  mechanism exact-subject invalidation already uses) makes the unit
  unsatisfied again until a fresh `APPROVE` exists for the new exact head -
  this is what makes "H1 approval never authorizes H2" true for a
  review-gated unit without any new invalidation mechanism;
- a `REQUEST_CHANGES` decision does **not** itself invalidate anything
  downstream (nothing was ever satisfied on it); remediation and a fresh
  review are what eventually produce the `APPROVE` the gate is waiting for;
  downstream simply stays `blocked` ("review required, not yet accepted")
  the whole time, visibly, which is the existing `blocked` state's reason
  string doing exactly its documented job;
- this uses #472's existing invalidation and frontier machinery as-is; it
  adds one additional satisfaction predicate, never a second satisfaction
  mechanism, a second activation path, or a second graph.

## Negative and recovery coverage

Implemented and passing (`runtime/review_remediation_test.go`, run with
`-race`): no decision; `APPROVE`/`COMMENT_ONLY` not routed as block;
publication disabled/absent still admits; stale subject (PR moved); forged
independence collapse (producer==reviewer in a hand-crafted store row);
terminal run (completed/failed/cancelled); unknown run; no compiled
contract; candidate superseded independent of PR-observed head; H1's
decision never satisfies H2 once H2 is current, while H2's own fresh
decision admits cleanly; idempotent re-admission (same process, and across a
closed-and-reopened store handle modeling a restart); two independent SQLite
handles racing the same admission admit it exactly once; admission touches
no `RunOperation` row; `pendingReviewRemediationKeys` ignores an admission
bound to a superseded head.

Three guards were each deliberately inverted, confirmed to break their
exact corresponding test, and restored: the independence re-check
(`TestAdmitReviewRemediationRefusesAForgedIndependenceCollapse`), the
staleness check (`TestAdmitReviewRemediationRefusesAStaleSubject`), and the
`ON CONFLICT DO NOTHING` idempotent insert (all three idempotency/
concurrency/restart tests, which failed with a raw `UNIQUE constraint
failed` once removed - proving the guard, not just an app-level check, is
load-bearing).

Deferred to the reconciler/operations wiring above, and therefore not yet
independently testable end-to-end: the full H1-committed-and-published →
BLOCK → remediation **dispatch** → H2 → fresh review → ACCEPT lifecycle: a
reviewer failing independence against a *resolved* agent registry entry
(covered today only via a hand-crafted store row, per #233's own test
suite); a producer provider failing after receiving findings; a crash
precisely inside the two-write admission-then-journal sequence (the
cross-run table write is the sole authorization record and is already
durable the instant it commits - a crash after it but before the journal
append is recovered by re-calling `AdmitReviewRemediation`, which finds the
existing row and journals it, exactly as `TestAdmitReviewRemediationSurvivesRestart`
already proves for the simpler case of no prior journal entry at all).
