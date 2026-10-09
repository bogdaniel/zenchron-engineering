# Automatic review-to-remediation routing (#474)

This document is the #474 half of the loop `docs/review.md` (#233) ends with:
"Nothing in this document grants #474 authority it does not independently
establish." It covers the durable authorization gate between an
independently reached `REQUEST_CHANGES` and a producer's own bounded
remediation invocation, the wiring that turns an admission into a scheduled
invocation, and the automatic trigger that removes the operator as the
relay between an independent reviewer and the producer it blocks.

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
4. the decision's own `Subject` disagrees with the requested repository/PR
   (`subject_mismatch`) - re-checked against the document itself, never
   trusted merely because the query that found it was scoped by the same
   values;
5. the decision is stale against the PR's current head (`subject_stale`), or
   a newer decision has superseded it in the narrow window between the
   staleness read and this gate acting on it (also `subject_stale` - see
   "Closing the staleness TOCTOU" below);
6. the producing run cannot be loaded (`run_unknown`);
7. the run's own `Repository` disagrees with the requested repository
   (`subject_mismatch`);
8. the run's own runtime-recorded publication
   (`state.projection.PullRequest.Number`, folded from its own journal, never
   the decision's claim) is not the requested PR (`subject_mismatch`) - two
   different pull requests at the same repository can share an exact head
   SHA (a shared base, a cherry-pick, a coincidence), so matching repository
   and commit alone is not proof this run published THIS PR;
9. the run's `AgentID` disagrees with the decision's claimed
   `ProducerAgentID` - including an empty claimed producer
   (`producer_mismatch`) - re-checked against the run's own record, never
   trusted from the decision document;
10. the run is terminal - checked twice, see "Two directions of staleness"
    below (`run_terminal`);
11. the run has no compiled contract (`no_compiled_contract`);
12. the run's **current** candidate no longer matches the decision's exact
    reviewed head (`candidate_superseded`) - the run moved on its own, even if
    the PR's last-observed head still agrees with the decision.

Separately, the durable table itself (`CreateReviewRemediationAdmission`)
refuses to write a row whose referenced `review.Decision` is not
`REQUEST_CHANGES`, or whose own `Subject`/`RunID` disagree with what the
admission claims - see "The table defends itself" below. This is enforced at
the write, not only read back at the gate, because the table is the sole
authorization source the deferred dispatch wiring will trust, and nothing in
Go stops another caller in this package from invoking that exported method
directly with a hand-built document.

### Closing the internal staleness TOCTOU - and the one that remains

`ReviewPort.IsStale` re-fetches `LatestDecision` internally. If a newer
decision D2 is admitted in the window between this gate's own first
`LatestDecision` call (which returned D1) and its `IsStale` call, `IsStale`
reports on D2 - not D1 - and a naive "not stale" would let D1 through even
though D2 has superseded it. This gate closes **that specific** window by
re-fetching `LatestDecision` a second time immediately after `IsStale` and
refusing unless its ID still matches D1's: nothing changed between the three
reads if and only if the latest decision is still the one being authorized.
`reviewRemediationRaceTestHook` exists solely to force this exact
interleaving on demand in `TestAdmitReviewRemediationRefusesADecisionSupersededBetweenReads`.

What this does **not** and cannot close: the PR's head can still move on
GitHub, with no new decision ever reached, in the narrow window between this
gate's last freshness read and the SQLite `INSERT` that commits the
admission - an external system cannot be linearized against a local write by
adding more reads, however many. The admission this gate writes is therefore
a **historical authorization fact** - "an independent reviewer blocked
exactly this head, as of the checks above" - never a currently-executable
grant good at any later instant. Closing that residual gap is explicitly the
deferred dispatch wiring's job, not this gate's: the bindExecutionInvoke
integration described below MUST re-validate the real current PR head, the
run's current candidate, and run eligibility immediately before creating or
leasing an `execution.invoke` operation, refusing a stale admission without
spending capacity on it. This gate's admission alone is not sufficient
authority to execute; it is sufficient authority to be WORTH re-checking.

### Two directions of run staleness

"Terminal" is checked both ways, because each direction is blind to the
other: the **persisted** `state.snapshot.Disposition` is how an
already-concluded run (merged, explicitly failed, operator-cancelled) is
read back - folded from the events that actually recorded it - but it is
exactly as fresh as the last reconciliation tick. A fresh `state.conditions()`
call re-derives what the disposition would be **right now**, which catches a
run whose wall budget (or other ceiling) exhausted since that last tick but
whose next pass has not run yet to journal `Failed`. Neither alone is
sufficient: `conditions()` does not re-derive an already-concluded
Completed/Failed from the snapshot (producing that disposition is its job,
not its input), and the snapshot alone cannot see an exhaustion nobody has
journalled yet.

This is a best-effort reduction of a stale-authorization window, not a
transactional guarantee and not meant to be one: the existing scheduler's
`AcquireOperation` is what re-validates terminal/paused run state,
atomically, at the actual moment an invocation would start (see
`runtime/scheduler.go`). An admission that slightly outraces a termination is
harmless - it is a durable fact that the real dispatch gate will still
correctly refuse to act on - which is why this gate's own check only needs to
be a reasonable reduction of the window, never a second transactional
authority duplicating the scheduler's.

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

That single row **is** the complete, self-contained authorization fact.
`pendingReviewRemediationKeys` (the deferred wiring's read side, see below)
queries `review_remediation_admissions` directly by `run_id`, rather than
folding a second, separately-written journal event the way admitted GitHub
feedback's `EventFeedbackObserved` does. An earlier version of this code did
write such a second event (`EventReviewRemediationAdmitted`) right after the
admission row committed - and had a real crash window because of it: if the
process stopped (or that second write failed) between the two writes, the
row was durable but no event existed to fold, and because a repeat
`AdmitReviewRemediation` call finds the existing row (`created=false`) and
never re-attempts the journal write, the gap was **permanent**, not merely
delayed (`TestAdmitReviewRemediationSurvivesRestart` only exercised a fully
successful first write, never this window). Reading the one authoritative
table directly removes the second write - and the gap - entirely:
`TestReviewRemediationAdmissionIsCompleteOnItsOwnSingleWrite` seeds exactly
the row a crash immediately after the real call's one commit would leave
behind and proves it is immediately, fully visible with no further action.

`AdmitReviewRemediation` touches no `RunOperation` row, consumes no budget,
and schedules nothing (`TestAdmitReviewRemediationTouchesNoOperation`).
Admission is a fact; turning a fact into a scheduled, budgeted invocation
remains entirely the existing reconciler's job.

### The table defends itself - at both ends, with one shared check

`AdmitReviewRemediation` is the one INTENDED writer, but it is not the only
POSSIBLE one: nothing in Go stops other code in this package from calling
the exported `CreateReviewRemediationAdmission` directly with a hand-built
document. An earlier version of this defense only checked that the admission
was *self-consistent with the decision it names* - verdict is
`REQUEST_CHANGES`, and the admission's own claimed subject/run agree with
that decision's. That is not enough: a direct caller can construct a
**different**, otherwise perfectly valid `REQUEST_CHANGES` decision - for a
**different** PR number, at the same repository and head, naming the
**same** run and producer - and an admission built from it passes that
check trivially, because the decision and the admission agree with each
other. What they cannot agree with is the run's own actual history.

`reviewRemediationBindingInvariants` (`runtime/review_remediation_store.go`)
is the one function that closes this: it re-reads the run's own durable
state (never trusts the admission's claim) and checks, together, that the
decision is `REQUEST_CHANGES`; the admission agrees with that decision's
subject and run; the run's **own runtime-recorded publication**
(`RunProjection.PullRequest.Number`, folded from its own journal) is the
exact PR number the admission claims; the run's **own** `AgentID` is the
decision's claimed producer; the decision's `ReviewerAgentID` is neither
empty nor equal to that same `AgentID` - independence, re-checked against
the run's own agent, because a direct store call never passes through
#233's `CheckReviewIndependence`, and `review.Decision.Validate` itself does
not reject `ProducerAgentID == ReviewerAgentID`; and the admission's
`FindingSignatures` are **exactly** the decision's own blocking findings,
sorted - never an invented or substituted set.

This one function is called at BOTH ends, so a row written through either
path is held to the same complete standard:

- **at the write** (`CreateReviewRemediationAdmission`), reading the
  referenced decision and the named run's events fresh before ever
  inserting (`TestCreateReviewRemediationAdmissionRefusesAPRTheRunNeverPublished`,
  `TestCreateReviewRemediationAdmissionRefusesForgedFindingSignatures`,
  `TestCreateReviewRemediationAdmissionRefusesANonBlockingDecision`);
- **at the read** (`pendingReviewRemediationKeys`), using the already-loaded
  `runState`'s own `run`/`projection` - no extra I/O beyond fetching the
  referenced decision - so a row that somehow exists anyway (a raw SQL
  insert bypassing the writer entirely, modeling corruption or a write path
  this table's own guard has not yet been taught about) still can never
  surface as an executable binding
  (`TestPendingReviewRemediationKeysRefusesARowWrittenAroundTheWriteGuard`).

The read side also separately refuses a row whose indexed `run_id` **and**
`decision_id` columns disagree with its own document's claimed `RunID`/
`DecisionID` (`TestReviewRemediationAdmissionsForRunRefusesAnIndexDocumentDisagreement`,
`TestPendingReviewRemediationKeysRefusesARowIndexedUnderADifferentDecision`),
mirroring the identity cross-check `ReviewPublication` already makes for its
own `DecisionID` - a prerequisite sanity check before the full invariant
function is even worth running against a row. Both indexed columns matter
here specifically because `ReviewRemediationAdmissionsForRun` is the LIST
method the actual planner path (`pendingReviewRemediationKeys`) calls: a row
indexed under decision B but whose document claims decision A must never
read back as authorization for A merely because the single-row
`ReviewRemediationAdmission(decisionID)` lookup - which the planner never
calls - happens to carry its own, separate `decision_id` check
(`TestReviewRemediationAdmissionRefusesAKeyDocumentDisagreement`). #233's own
`ReviewDecision` (`runtime/review_store.go`) carries the matching check on
its `id` - the one lookup `reviewRemediationBindingInvariants` trusts as
authoritative for the decision it is given
(`TestReviewDecisionRefusesAKeyDocumentDisagreement`). `ReviewDecision` is
already-merged #233 code, outside this PR's own files, but the fix is a
single additive identity check in the same pattern `ReviewPublication`
already established, directly motivated by #474 now depending on that
method's result as ground truth - not unrelated cleanup.
`LatestReviewDecision`/`ReviewDecisionsForPullRequest` are left as they are:
`AdmitReviewRemediation`'s own Subject re-check (item 4 in "The admission
gate" above) already catches a document/row disagreement reached through
either of them, so they are not in this function's direct trust chain the
way the single-decision `ReviewDecision` lookup is.

## The wiring (now live - #546 merged at `122f438`)

#546 merged into `main`, releasing `runtime/reconciler.go` and
`runtime/operations.go`. This PR now contains the real wiring, not a
documented diff waiting for one.

**1. `runtime/reconciler.go`, inside `bindExecutionInvoke`** - a new branch at
the same priority tier as the existing GitHub-feedback branches, inserted
after them and before the `#508 P4b` fallback:

```go
if binding, ok := s.unresolvedReviewRemediationBinding(s.projection.CandidateRevision); ok {
    return binding, true
}
if pending, err := s.pendingReviewRemediationKeys(); err == nil && len(pending) > 0 {
    return reviewRemediationBindingPrefix + s.projection.CandidateRevision + "|" + digestOfKeys(pending), true
}
```

A read failure here falls through to the next fallback, not to a fabricated
binding - and that is safe, not a swallowed error, because **`conditions()`
is where the failure is actually surfaced**, checked once per pass before
`plan()`/`bindExecutionInvoke` ever runs:

```go
// in conditions(), alongside controller_changed/candidate_external_changed:
if _, err := s.pendingReviewRemediationKeys(); err != nil {
    return Waiting, "review_remediation_unavailable"
}
```

`Reconcile`'s own existing control flow (`runtime/reconciler.go`, the
`!wanted` branch) settles a pass with nothing else to plan under
`conditions()`'s disposition/reason rather than always
`goal_state_reached` - so an unreadable admission table reads as the visible
wait `review_remediation_unavailable`, never as an empty queue, exactly
when there would otherwise be nothing else keeping the run busy.
`pendingReviewRemediationKeys` itself degrades to "nothing pending" (not an
error) when `s.rt`/`s.rt.deps.Store` is nil - the shape several existing
tests use to exercise `conditions()`'s budget arithmetic in isolation,
which never had a real database to begin with and genuinely has nothing
admitted.

**2. `runtime/operations.go`, inside `invokeExecution`'s context assembly** -
right after the existing GitHub-feedback findings block:

```go
reviewFindings, err := r.reviewRemediationFindings(state)
if err != nil {
    return failed(err)
}
if len(reviewFindings) > 0 && purpose != InvocationContinuation {
    purpose = InvocationRemediation
    findings = append(findings, reviewFindings...)
}
```

`reviewRemediationFindings` (`runtime/review_remediation_delivery.go`)
re-validates every admission bound to the current head through the same
`reviewRemediationBindingInvariants` the store already enforces, then
renders each blocking `review.Finding` as a typed `Finding`: `Signature`
carries the reviewer's own bounded dedup key (printed directly, the same
treatment stage-review findings already get); `Detail` reaches the provider
only inside the existing `verifierEvidenceEnvelope`'s `UNTRUSTED-SOURCE`
markers - reused as-is, no new framing mechanism. A store read failure here
fails the operation outright (`failed(err)`), consuming the attempt rather
than silently proceeding with no findings - the same "never an empty queue"
discipline as `conditions()`'s own check, at the point that actually matters
most: the moment a producer would otherwise be invoked uninformed.

**3. `runtime/review_remediation_trigger.go` (new)** - `ReconcileReviewRemediation`
is the one piece neither `reconciler.go` nor `operations.go` could be:
the automatic entry point that actually calls `ReviewPort.RequestReview`/
`AdmitReviewRemediation` for a published PR, removing the operator as relay.
Nothing in `Reconcile`'s own per-run tick calls `RequestReview` - #233's
review operation runs a provider and must stay outside a producer run's
tick, capacity-respecting the same way `review pr` already is a bounded,
separately-scheduled operation (`docs/review.md`, "Known scope limits").
`ReconcileReviewRemediation` is idempotent at every step (`NeedsReview`,
`RequestReview` and `AdmitReviewRemediation` all are), so calling it
repeatedly - once per supervisor tick, forever - is the whole mechanism.
**Now wired into `serve` (B3)**: `runtime.SupervisorDependencies.ReviewTrigger`
is the same nil-disabled convention `Discovery`/`AgentProber` already use;
`Supervisor.driveOne` calls it once per run per pass, same cadence as
`ObserveFeedback`, through a new `ReconcileReviewRemediationForRun` that
resolves the run's own repository and published PR from durable state.
`serve --reviewer-agent <id>` is the operator opt-in (empty disables the
trigger entirely, never a silently-chosen default reviewer); the composition
root builds the `ReviewPort` per call from the fixed CLI-era fields
(`Store`, `GitHub`, agent registry, provider factory) plus the run's own
`GovernedRemote` identity. That last part closes a real pre-existing #233 gap:
`RunIndependentReviewInput.Source` assumed a single operator checkout,
which a long-running multi-repository supervisor has no equivalent of;
`RunIndependentReviewInput.Remote`/`Credentials` and `reviewWorkspace` now
clone from the governed remote instead, through the same credential-bound
Git boundary a candidate clone already uses
(`CreatePlanningWorkspaceFromRemote`), when `Source` is empty.

**Test-fixture fix along the way**: `FakeGitHubAdapter.PullRequest` returned
a `HeadSHA` frozen at `CreatePullRequest` time and never refreshed - real
GitHub reports whatever the branch currently points at with no separate
"update the PR" call for the head. No existing test before the one below
ever pushed a SECOND commit to an already-published PR through the real
reconciler loop, so nothing had surfaced this. Fixed to re-read the live ref
on every `PullRequest` call.

## Independent-review re-review hardening (B1, B2)

Two gaps an independent exact-head re-review found in the wiring above,
both closed with no new mechanism:

- **B1 - a BLOCK silently discharged by a no-op return.** `invokeExecution`'s
  completion gate (#376) only ever consulted `len(feedback)` for "was this
  invocation's obligation discharged". A remediation invocation carrying
  delivered review-remediation findings that returned success without
  mutating the candidate and without an admitted no-change resolution left
  `EventExecutionCompleted`/`Succeeded` for the exact
  `review-remediation|H1|...` binding, which `bindExecutionInvoke` then
  treated as satisfied forever. `reviewRemediationUnresolved` is
  `feedbackUnresolved`'s sibling, under a new `FailureReviewRemediationUnresolved`
  class (same `RouteRetry`, bounded-attempt shape), and review-remediation
  finding signatures now feed the SAME `deliveredKeys`/`admitResolution`
  exact-match check feedback keys already use: a no-change resolution must
  name every delivered obligation, not a subset.
- **B2 - no live GitHub re-check at dispatch time.** `reviewRemediationFindings`
  and `bindExecutionInvoke`'s binding both read only journalled state,
  refreshed by whenever `observeGitHub` last ran - never the PR's actual
  current head. `reviewRemediationLiveHeadCheck` (`review_remediation_delivery.go`)
  is a live `GitHub.PullRequest` read, called from `invokeExecution`
  immediately before `Provider.Execute` - the same point `runStopObserved`
  already guards - compared against the head the findings were assembled
  against. A live disagreement or an unreachable GitHub both refuse THIS
  attempt under a new `FailureReviewRemediationStale` class, rather than
  deliver findings whose subject may already have moved. The external read
  and the provider actually starting can still never be made perfectly
  atomic from inside this process; this closes the gap that previously had
  no live check at all, not the theoretical remainder.

Both added guards were deliberately disabled and confirmed to fail their
exact regression test before being restored:
`TestUnresolvedReviewRemediationReturnFailsTheOperationAndStaysActionable`
(B1, the `TestUnresolvedFeedbackReturnFailsTheOperationAndStaysActionable`
sibling, driven through the real coordinator) and the four focused tests in
`review_remediation_delivery_test.go` (B2), including the adversarial case
explicitly asked for: the PR's forge-side head moves with no observation
tick in between, so the run's own journalled projection is untouched and
only the live read can see the move.

A second exact-head re-review of B1/B2 found two further exact-source gaps,
both closed with no new mechanism:

- **R1 - the B1 failure gate didn't gate the completion EVENT.**
  `reviewRemediationUnresolved` correctly failed the OPERATION, but the
  event-creation guard in `invokeExecution` (`!feedbackUnresolved &&
  !reviewUnresolved && !continuationUnresolved`) still omitted it: a failed
  no-op remediation attempt could still journal `EventExecutionCompleted`
  (and, for a WorkGraph unit, an admissible handoff), contradicting the
  `OperationFailed` record right beside it - `Project`
  (`runtime/projection.go`) reads `EventExecutionCompleted` as "this
  candidate is execution-complete". Added `reviewRemediationUnresolved` to
  that same guard.
- **R2 - a same-head review supersession was unchecked at dispatch.** B2
  proves the live PR head still agrees with the head an admission's findings
  were assembled against; it says nothing about whether that admission's
  referenced decision is still the LATEST independent decision for the PR.
  A fresh decision for the SAME exact head - a second reviewer reaching
  APPROVE with no head move at all - supersedes an earlier REQUEST_CHANGES
  without ever failing B2's check. `reviewRemediationSuperseded`
  (`review_remediation_store.go`) is a `LatestReviewDecision` read compared
  against the admission's own referenced decision's ID, called from both
  `pendingReviewRemediationKeys` (binding) and `reviewRemediationFindings`
  (delivery) - the same two read-time call sites
  `reviewRemediationBindingInvariants` already runs at. A store read failure
  fails closed (treated as superseded, never as current).

`TestUnresolvedReviewRemediationReturnFailsTheOperationAndStaysActionable`
now also asserts the real journal contains exactly one
`EventExecutionCompleted` (the original successful commit) and no handoff
observation for the failed remediation attempt (R1).
`TestSameHeadReviewSupersessionBlocksProviderDispatch` admits D1
(REQUEST_CHANGES) for H1, then durably records D2 (APPROVE, a different
reviewer) for the identical subject with no intervening projection change,
and asserts both that D1 no longer binds or delivers findings and that the
producer is never invoked for it (R2); a second focused test
(`TestReviewRemediationSupersessionFailsClosedOnAnUnreadableLatestDecision`)
proves the fail-closed error path. All four new guards (R1's event gate,
R2's two call sites, R2's error path) were deliberately disabled and
confirmed to fail their exact test before being restored.

## WorkGraph review-readiness (section 8): a frozen, deferred contract

`orchestration/workgraph_projection.go`'s `ProjectWorkGraph` marks a unit
`satisfied` purely from its admitted handoff (`docs/workgraph.md`); there is
no concept of "activated, completed, but not yet independently accepted" a
downstream unit's readiness could gate on. Investigated directly (not
assumed): no `review_gate`/`review_obligation` field exists on `WorkUnit` or
its projection anywhere in the codebase.

This remains deferred even now that #546 has merged and released the files
it would touch (`orchestration/workgraph.go`, `runtime/workgraph_status.go`):
it is a cross-package schema change with its own test surface, layered
**above** the now-complete producer review/remediation loop this PR proves
end-to-end, and #474's own governing comment thread already HELD exactly
this shape of change once before (PR #543) for widening WorkGraph scope
without an agreed contract first. The frozen contract, for whoever picks
this up next:

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

Split across two files by concern (`runtime/review_remediation_test.go`:
fixture, positive path, idempotency/concurrency/restart, and the B1-B4
regressions below; `runtime/review_remediation_refusal_test.go`: ordinary
refusal reasons plus the R1/R2 bypass-closing regressions), both run with
`-race`:

- no decision; `APPROVE`/`COMMENT_ONLY` not routed as block; publication
  disabled/absent still admits; stale subject (PR moved); forged
  independence collapse (producer==reviewer in a hand-crafted store row);
  terminal run (completed/failed/cancelled); unknown run; no compiled
  contract; candidate superseded independent of PR-observed head; H1's
  decision never satisfies H2 once H2 is current, while H2's own fresh
  decision admits cleanly; `pendingReviewRemediationKeys` ignores an
  admission bound to a superseded head;
- idempotent re-admission (same process, and across a closed-and-reopened
  store handle modeling a restart); two independent SQLite handles racing
  the same admission admit it exactly once; admission touches no
  `RunOperation` row;
- the admission row is immediately, completely visible on its own single
  write, with no second write to crash between (B1);
- a decision superseded by a newer one in the exact window between the
  staleness read and the gate acting on it is refused, forced
  deterministically via `reviewRemediationRaceTestHook` (B2);
- a decision whose own `Subject` disagrees with the row that found it, a
  decision whose claimed producer is not the run's actual agent, and an
  empty claimed producer are all refused (B3);
- a run whose wall budget is exhausted but whose persisted disposition has
  not caught up yet (no reconciliation tick has run since) is refused (B4);
- a decision for a PR this run never published, even at the same commit and
  with the same producer agent, is refused at the gate (R1); the shared
  `reviewRemediationBindingInvariants` check closes the same gap at the
  store layer too, independent of which caller writes or reads: a durable
  admission row referencing an `APPROVE` decision, one whose finding
  signatures are invented rather than the decision's actual blocking ones,
  one for a PR the run never published (submitted directly to the store
  API, bypassing the gate entirely), and one whose indexed `run_id` disagrees
  with its own document, are all refused - at the write where possible, and
  independently re-verified at the read for a row that exists anyway
  (written around the write guard, modeling corruption) (R2);
- a directly stored self-review decision - reviewer equal to the run's own
  actual agent, something `review.Decision.Validate` itself never rejects
  and a direct store call never routes through #233's
  `CheckReviewIndependence` - is refused by the shared invariant function at
  both the write and, independently, at the read for a row injected around
  the write guard entirely.

Sixteen guards were each deliberately inverted, confirmed to break their
exact corresponding test, and restored: the gate-level independence
re-check, the staleness re-confirmation (B2), the producer-identity check
(B3), the fresh-conditions check (B4), the gate-level run-published-this-PR
check (R1); inside the shared `reviewRemediationBindingInvariants` function,
each broken and tested independently at both its write-time and read-time
call sites - the run-published-this-PR invariant, the finding-signature
equality invariant, the decision-verdict invariant, and the independence
invariant; the three indexed-key/document integrity checks on
`ReviewRemediationAdmissionsForRun` (both `run_id` and `decision_id`,
checked separately), `ReviewRemediationAdmission`, and #233's
`ReviewDecision`; and the `ON CONFLICT DO NOTHING` idempotent insert (all
three idempotency/concurrency/restart tests, which failed with a raw
`UNIQUE constraint failed` once removed - proving the guard, not just an
application-level check, is load-bearing).

**End-to-end, through the real wiring (no manually injected rows), now that
#546 has merged:**

- `TestReviewRemediationEndToEndH1BlockH2Approve` drives the full lifecycle
  through the real `Reconcile` loop, the real `ReviewPort`
  (`RunIndependentReview`), and `ReconcileReviewRemediation`: producer A
  commits H1, publishes; an independent review reaches `REQUEST_CHANGES`
  with a blocking finding; `ReconcileReviewRemediation` durably admits
  remediation with no operator relay; the SAME `RunID`'s next
  `execution.invoke` is a genuinely fresh `Execute` call (no inherited
  transcript - `isolatedProvider`'s stateful mutate proves the SECOND
  invocation is a distinct call, not a resumed one) and produces a
  different exact H2; H2 is freshly committed, assured and republished; H1's
  decision is confirmed stale (`IsStale`); a fresh independent review of the
  exact new head reaches a DIFFERENT `DecisionID` and `APPROVE`; and
  attempting to admit against the (now superseded) latest decision again is
  refused - H1's review can never approve H2, and an `APPROVE` is never
  routed through remediation admission.
- `TestConditionsSurfacesAnUnreadableReviewRemediationTable` and
  `TestBindExecutionInvokeToleratesAnUnreadableReviewRemediationTable` prove
  the store-read-failure-must-be-visible requirement at both of
  `bindExecutionInvoke`'s and `conditions()`'s real call sites, deliberately
  broken and restored like every other guard above.
- `TestUnresolvedReviewRemediationReturnFailsTheOperationAndStaysActionable`
  (B1, now also asserting the event-journal absence R1 requires) and
  `review_remediation_delivery_test.go`'s four tests (B2) - see the
  hardening section above.
- `TestSameHeadReviewSupersessionBlocksProviderDispatch` and
  `TestReviewRemediationSupersessionFailsClosedOnAnUnreadableLatestDecision`
  (R2) - see the hardening section above.
- `TestReviewTriggerRunsOncePerTickPerRun`, `TestReviewTriggerErrorIsReported-
  SeparatelyFromDriveFailure` and `TestReviewTriggerSurvivesSupervisorRestart`
  (B3) prove the supervisor wiring itself - repeated tick, isolated error
  reporting, and no reliance on in-process state across a simulated restart;
  `TestRunIndependentReviewClonesFromTheGovernedRemoteWithNoSourceCheckout`
  and `TestRunIndependentReviewRefusesWithNeitherSourceNorRemote` prove the
  remote-clone fallback.

**Still deferred, and why:** the WorkGraph review-readiness gate (section 8
below - a cross-package schema change layered above this now-complete
producer loop); the residual external-head TOCTOU between this gate's last
freshness check and its SQLite commit (explicitly documented above as the
dispatch path's own obligation, not something more internal reads can
close - B2 closes the dispatch-time instance of this, not the theoretical
remainder); and a reviewer failing independence against a *resolved* agent
registry entry with a distinct, real vendor family mismatch (covered today
only via a hand-crafted store row with an identical agent ID, per #233's own
test suite - the E2E test's own two agents already prove the
registry-resolved, cross-vendor-family independence path on the SUCCESS
side).
