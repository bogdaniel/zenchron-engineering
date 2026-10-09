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
decision's claimed producer; and the admission's `FindingSignatures` are
**exactly** the decision's own blocking findings, sorted - never an invented
or substituted set.

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

The read side also separately refuses a row whose indexed `run_id` column
disagrees with its own document's claimed `RunID`
(`TestReviewRemediationAdmissionsForRunRefusesAnIndexDocumentDisagreement`),
mirroring the identity cross-check `ReviewPublication` already makes for its
own `DecisionID` - a prerequisite sanity check before the full invariant
function is even worth running against a row.

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
pending, err := s.pendingReviewRemediationKeys()
if err != nil {
    // MUST surface as a visible, non-advancing wait - never silently fall
    // through to "nothing wanted" for this binding. An authorized BLOCK that
    // cannot be read back must never look like goal_state_reached or a clean
    // settle; it must look like exactly what it is, an outage, the same way
    // a capacity wait is recorded in run.waiting rather than swallowed
    // (docs/orchestration.md, "Recognized capacity waits"). The exact
    // mechanism (a new wait reason recorded alongside the existing capacity-
    // wait ones, most likely) is #546 ownership territory to settle once
    // bindExecutionInvoke's actual post-merge shape is known - this snippet
    // intentionally does not guess at it.
}
if len(pending) > 0 {
    return reviewRemediationBindingPrefix + s.projection.CandidateRevision + "|" + digestOfKeys(pending), true
}
```

Both helper methods, and the `reviewRemediationBindingPrefix` constant, are
already implemented and unit-tested in `runtime/review_remediation.go` -
this is a small consumer of code that already exists, not new logic.
`pendingReviewRemediationKeys` returns an error because, unlike
`pendingFeedbackKeys`, it reads the durable admission table rather than
`s.events`. An admission that cannot be read back is NOT the same fact as no
admission existing: the former is visible authorized work this run cannot
currently see, and reconciliation must say so, never settle quietly as if
nothing were pending.

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
  (written around the write guard, modeling corruption) (R2).

Twelve guards were each deliberately inverted, confirmed to break their exact
corresponding test, and restored: the independence re-check, the staleness
re-confirmation (B2), the producer-identity check (B3), the fresh-conditions
check (B4), the gate-level run-published-this-PR check (R1), and - inside the
shared `reviewRemediationBindingInvariants` function, each broken and tested
independently at both its write-time and read-time call sites where
applicable - the run-published-this-PR invariant, the finding-signature
equality invariant, the decision-verdict invariant, the index/document
integrity check, and the `ON CONFLICT DO NOTHING` idempotent insert (all
three idempotency/concurrency/restart tests, which failed with a raw `UNIQUE
constraint failed` once removed - proving the guard, not just an
application-level check, is load-bearing).

Deferred to the reconciler/operations wiring above, and therefore not yet
independently testable end-to-end: the full H1-committed-and-published →
BLOCK → remediation **dispatch** → H2 → fresh review → ACCEPT lifecycle; a
reviewer failing independence against a *resolved* agent registry entry
(covered today only via a hand-crafted store row, per #233's own test
suite); a producer provider failing after receiving findings; the residual
external-head TOCTOU and the visible-wait-on-read-failure requirement (R3,
R4 above), both of which are explicitly the dispatch wiring's obligation,
not this isolated gate's.
