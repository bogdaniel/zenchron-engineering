# ADR-0008: Bounded, Governed Remediation Epochs for an Already-Admitted WorkUnit

- Status: Proposed (design + independent review only — not implemented)
- Date: 2026-10-12
- Issue: #581 (bridge gap identified during #475 bounded acceptance,
  PR #580). Depends on: #472 (WorkGraph), #473 (communication), #508
  (decision resolution). Related, not depended on: #474 (review
  remediation, disjoint), #480 (dynamic orchestration, conceptual only —
  see "Ownership" below).
- Supersedes: nothing. Extends #472's `WorkUnitActivation` and #508's
  `DecisionResolution` vocabulary; does not replace either.

## The decision in one sentence

**A WorkUnit may legitimately run a second (third, ...) time — a new
*remediation epoch* — but only when a durable, policy-or-human-authorized
`RemediationAuthorization` record says so; a delivered Finding is never
itself that authorization, only its justifying input.**

## 1. Why this ADR exists

#475 (PR #579) proved that an integration-conflict Finding can be
delivered into the exact producer unit's own inbox, verified through that
producer's real rendered communication context. PR #580 then proved,
executably, that delivery is where it stops: twenty further real scheduler
ticks after delivery leave the blamed WorkUnit's run, state, activation row
and handoff count byte-identical. The Finding sits there, read, unconsumed,
forever.

This is not a #475 defect. It is the frozen architectural invariant working
exactly as designed: *"Agents may reason and execute. Agents do not
self-authorize material outcomes."* and *"A delivered Finding is
information, not automatic authorization to rerun a producer."* The gap is
that **no mechanism exists at all** — governed or otherwise — to legitimately
re-run an already-admitted WorkUnit. This ADR designs that mechanism, once,
generally, so it serves #475's conflict-remediation case and any future
WorkGraph-level corrective action (#480) without a second one being built
later.

## 2. Current-code gap analysis (exact)

| # | Current fact | File:line | Why it blocks remediation as-is |
|---|---|---|---|
| 1 | `UnitState` has 5 graph-owned values (`blocked`, `ready`, `unknown`, `awaiting_decision`, `invalidated`); "completed" is the child run's own `ItemState` passed through unchanged | `orchestration/workgraph_projection.go:22-51`, `:402-409`; `orchestration/item.go:27` | No existing state means "eligible for a new attempt after completion" |
| 2 | `work_graph_activations` primary key is `(graph_id, unit_id)`, insert-only, `ON CONFLICT DO NOTHING` | `runtime/workgraph_store.go:189-211` | A unit can activate — get a child run — at most once, ever, by construction. Doc comment states this explicitly: "no replay or recovery can produce a second child run for a unit." |
| 3 | Frontier membership is exactly `!Activated` | `orchestration/workgraph_projection.go:238-249` | An activated unit never re-enters the frontier regardless of its own or any Finding's state |
| 4 | Invalidation fires purely from `ActivationInputsDigest != current digest` (upstream output changed) | `orchestration/workgraph_projection.go:273-274` | The *only* existing re-run-adjacent trigger is upstream, not self-directed, and even then: |
| 5 | An invalidated unit never gets a second run — proven as current, intended behavior | `TestWorkGraphIntegrationUnitInvalidatedWhenUpstreamOutputReplaced` (`runtime/workgraph_integration_test.go:664-705`): "Invalidated work is not silently re-performed: no second run appears" | Invalidation today is terminal, not a remediation trigger |
| 6 | `ValidateMutation` hard-refuses changing `DependsOn`/`Role`/`ExecutionKind`/etc. for any already-activated unit | `orchestration/workgraph.go:373`, `sameUnit` at `:424-439` | "Just add a sibling remediation unit and rewire the integrator's dependency" is not available as a free-lunch workaround |
| 7 | #508's decision-resolution continuation requires the run to be **still live**, blocked mid-run on its own `decision_request` | `orchestration/decision_resolution.go:4-21`; wired via `decisionResumeBinding` into `bindExecutionInvoke`, same `RunID` | Does not apply once the producer's run is already `ItemCompleted` |
| 8 | #508's `WorkUnitHold` is explicitly, by its own doc comment, at-most-once and pre-activation only | `orchestration/workgraph_hold.go:15-19,36-39` | Explicitly rules out reuse for an already-activated unit |
| 9 | #474's review remediation requires a published PR's live `REQUEST_CHANGES` Decision bound to the PR's current `HeadSHA` | `runtime/review_remediation.go:190,222,319` | Entirely post-publication, entirely outside WorkGraph/#473 — disjoint trigger and disjoint lifecycle point |
| 10 | No WorkUnit-scoped attempt/retry counter exists; the only budgets are plan-scoped (`PlanBudgetEnvelope`) or run-scoped (`execution_continuation_budget.go`) | `runtime/config.go:291-325`; `runtime/controller.go:175-183` | Nothing bounds "how many times has this *unit* been remediated" |
| 11 | `#473` messages have no "consumed" flag on any kind; `Live()` is a pure, stateless recomputation from `Supersedes` | `orchestration/communication_admission.go:173-198` | A Finding can be checked for liveness/staleness, but nothing marks it "acted on" |
| 12 | #480 is purely conceptual: zero comments, zero linked PRs. Its own acceptance text depends on #508's decision path and never mentions Findings or #475 | `gh issue view 480` | #480 does not already own this case; it is real, additive, unclaimed scope — consistent with #581's framing |

Reusable as-is (no new mechanism needed):

- `DecisionResolutionAuthority` / `ResolveDecision` / `AuthorityKindOperator` — #508's authority/provenance check (`orchestration/decision_resolution.go:85+`).
- The deterministic-id + insert-only idempotency pattern #579 already uses for `DeliveredFindingID` (`orchestration/communication.go`).
- `AppliesTo` staleness comparison (`orchestration/communication.go:424-426`) for refusing a stale Finding/handoff.
- The entire per-run operation DAG and scheduler (`runtime/operations.go`, `runtime/scheduler.go`) — a new epoch is just a new `RunID` getting the same fresh op-DAG any new unit's first run gets. No scheduler code changes.
- #475's existing `OpIntegrationCompose`/conflict-classification/fresh-assurance-gating machinery, unchanged, for the re-integration attempt itself.
- #475's own existing no-progress pattern: park after one identical-attempt (`ReasonDeterministicFailureUnchanged`), proven in `TestWorkGraphIntegrationTextualConflictBlocksAndFindsNoHandoff`.

## 3. Lifecycle: the remediation epoch

A WorkUnit's durable activation record is widened from one-per-unit to
one-per-(unit, epoch):

```text
work_graph_activations
  (graph_id, unit_id)                               -- UNCHANGED shape today
      ↓ widen primary key, additive
work_graph_unit_epochs
  (graph_id, unit_id, epoch)            PRIMARY KEY, insert-only, same
                                         ON CONFLICT DO NOTHING idempotency
  batch_id, run_id, inputs_digest, activated_at      -- same fields as today
  trigger: "original" | "remediation"
  authorization_id                                   -- null for epoch 1
  superseded                                          -- boolean, projection-derived, never written
```

- Epoch 1 **is** today's activation, byte-for-byte. A graph that never
  remediates has exactly the rows it has today; no backfill, no migration.
- Every epoch row is permanent. A later epoch never updates or deletes an
  earlier one — remediation adds rows, it never rewrites history.
- The unit's **current epoch** is a *projected* fact, recomputed from the
  durable rows exactly the way `Live()` recomputes message currency today
  (gap #11's pattern, reused): the highest epoch whose run reached an
  admitted `Output`, or whose run is still in flight. This keeps
  `ProjectWorkGraph` a pure function of durable state — restart-safe by the
  same construction already proven for today's single-epoch case.
- `UnitState` projection changes in exactly one place: frontier membership
  (gap #3) becomes `!Activated(unit) OR EligibleForNextEpoch(unit)`, where
  `EligibleForNextEpoch` is defined in §5. No new `UnitState` constant is
  required; the unit's reported state is still its current epoch's child
  `ItemState`, exactly as today.
- Invalidation (gap #4/#5) gains a `Cause` discriminant:
  - `external_replacement` — today's only case. Stays exactly as it is:
    terminal, no re-run. An operator or tool legitimately superseding a
    unit's output outside this mechanism must never auto-cascade.
  - `remediation_epoch_advance` — new. An upstream producer's new,
    authorized epoch changed the digest. The downstream unit becomes
    eligible for *its own* next epoch (§5), not permanently invalidated.
    This is the one behavior change to existing invalidation semantics in
    this ADR, and it is gated by the same authorization chain as every
    other epoch advance — it is not automatic cascading, it is "also
    eligible to request its own governed epoch."

## 4. Authorization

A Finding never authorizes anything by itself (never-cut: "no invented
producer activation"). Two paths produce the one authorization record that
does:

```text
                    Finding (live, exact-subject-bound)
                              │
                    EligibleRemediation() — pure function, §5
                              │
                    RemediationRequest (durable, deterministic id)
                    ┌─────────┴─────────┐
      plan-preauthorized           requires human decision
      (RemediationPolicy           (default; mandatory once
       compiled from the           auto-path's own bound is
       approved EngineeringPlan,    exhausted, or a no-progress
       §64's existing plan-        park already occurred once)
       approval boundary)                    │
              │                     #508 DecisionRequest
              │                     (AudienceAuthority, Subject
              │                      bound to Finding + unit +
              │                      proposed epoch)
              │                              │
              │                     operator resolves via
              │                     DecisionResolutionAuthority /
              │                     ResolveDecision — REUSED AS-IS
              └─────────────┬────────────────┘
                             ▼
              RemediationAuthorization (append-only, append-only link
              to: Finding.ID, unit, proposed epoch, authorizing policy
              clause OR DecisionResolution.ID, actor, timestamp)
```

- `RemediationAuthorization` is the single record either path produces. It,
  not the Finding, is what `EligibleForNextEpoch` reads. This mirrors the
  project's own core invariant chain (facts → policy → obligations →
  evidence → authority) instead of inventing a parallel one.
- Plan-preauthorization is compiled once, at plan-approval time, by a
  human — exactly #64's existing EngineeringPlan approval boundary. It
  bounds *routine* remediation (e.g. "up to 2 conflict-remediation epochs
  per integration unit, scoped to the conflicting paths only") so an
  individual Finding doesn't need its own one-off click when the plan
  already covers it — directly satisfying "do not force routine authorized
  remediation through unnecessary human approval."
- Identity is never authority: the same law #508 already states
  (`orchestration/decision_resolution.go`) applies unchanged — a
  model-authored Finding or a worker's own claim is never itself
  sufficient, only a `RemediationAuthorization` traceable to an approved
  plan clause or a resolved human decision.

## 5. Trigger (`EligibleRemediation`)

A pure function, functional core, no side effects:

```text
EligibleRemediation(finding, unit, graph) -> (RemediationRequest, ok)
  require: finding is live                              (Live())
  require: finding.Route.Unit == unit.Name
  require: finding.Subject.Handoff == unit's CURRENT epoch's admitted handoff id
           AND AppliesTo(finding.Subject.Revision, unit's current HandoffSubject)
                                                          (reused #475 staleness check)
  require: unit's current epoch's run is terminal (ItemCompleted or ItemFailed),
           never in-flight
  require: no existing RemediationAuthorization already covers
           (finding.ID, unit, nextEpoch)                 (idempotent — deterministic id,
                                                            same pattern as DeliveredFindingID)
  require: nextEpoch <= PolicyMaxEpochs(unit)             (gap #10, new counter — see §7)
  → RemediationRequest{finding.ID, unit, proposedEpoch: current+1}
```

`EligibleForNextEpoch(unit)` (used by the frontier, §3) is then simply: a
`RemediationAuthorization` exists for `(unit, nextEpoch)` and no epoch
`>= nextEpoch` has activated yet. Deterministic, projection-only, no new
mutable state beyond the authorization and epoch rows already described.

## 6. Scheduling

Two existing, independent gates, unchanged in kind:

- **WorkGraph layer** (`runtime/workgraph.go` `activateWorkUnit`): decides
  *which unit* gets a run. Gains one additional frontier clause
  (`EligibleForNextEpoch`, §5) alongside the existing `!Activated` clause.
  `ActivateWorkUnit` becomes `ActivateWorkUnitEpoch(graph, unit, epoch, ...)`
  against the widened `(graph_id, unit_id, epoch)` key — same insert-only,
  same `ON CONFLICT DO NOTHING` idempotency, just a wider key. Additive,
  not a rewrite.
- **Per-run Scheduler** (`runtime/scheduler.go`, `runtime/operations.go`):
  decides *which operation* runs next inside a run's own op-DAG. Completely
  unchanged — a new epoch mints a brand-new `RunID`, which gets the
  existing fresh operation set (`OpSourceObserve → ... → OpExecutionInvoke
  → ... → OpAssuranceGo/Semantic → ...`) exactly like any new unit's first
  run. No scheduler code changes.
- **Capacity**: the existing plan-scoped `PlanBudgetEnvelope`
  (`MaxChildRuns`, `MaxConcurrency`, ...) applies unchanged, since each
  epoch is a real new child run and already counts against it. The new,
  WorkUnit-scoped `PolicyMaxEpochs` counter (gap #10) is evaluated at
  `EligibleRemediation` time (§5), before a new epoch can even be proposed
  — a new, narrow counter, not a repurposing of the run-scoped
  `execution_continuation_budget.go` (which bounds attempts *inside* one
  run, a different axis).
- **Restart**: identical to today's story. `ProjectWorkGraph` plus the
  widened epoch rows plus each epoch's child `ItemState` remains a pure
  function of durable state (gap #4's restart-safety argument extends
  unchanged to N rows instead of 1). A mid-flight epoch's run resumes
  exactly like any other run resumes today; no new recovery logic.

## 7. Execution

A new epoch's run is a genuinely fresh `EngineeringRun` against the
**same, unchanged, originally compiled contract** — remediation never
re-recompiles obligations or expands privilege (never-cut:
"no automatic privilege expansion during contract recompilation"). It
receives, as context (via the existing `communicationContext` renderer
#579 already extended — no new renderer):

- the prior epoch's admitted handoff, as base context;
- the exact triggering Finding, surfaced prominently;
- a permitted remediation scope bounded to the Finding's own named
  paths — reusing the existing `Conflict.Paths`/`VerifyRemediationScope`
  bounded-scope concept #475's own review history already established
  (PR #541/#546 review threads), not a new scoping mechanism.

## 8. Supersession

- The instant epoch N's handoff admits, epoch N-1's `Output` is no longer
  current. This is *exactly* today's existing "latest admitted output
  wins" fact, already consumed by downstream digest recomputation (gap
  #4) — no new primitive, provided §6's widened activation key removes the
  "producer run is already terminal" blocker.
- A downstream integration unit that consumed epoch N-1's stale output was
  already (correctly) flagged `invalidated` by today's unchanged mechanism.
  §3's `Cause: remediation_epoch_advance` is what lets it request its own
  next epoch instead of staying permanently invalidated — gated by the
  same `RemediationAuthorization` chain (§4), so cascading reintegration is
  never automatic, only ever newly eligible.
- Re-integration reuses #475's `OpIntegrationCompose` and conflict/success
  classification completely unchanged: current `WorkUnitInputs` now
  resolve to epoch N's producer output plus the unaffected other
  producer's own current output, Git-composes, fresh assurance gates it
  exactly as today (`TestWorkGraphIntegrationHandoffWaitsForRequiredSemanticAssurance`,
  unchanged).
- A stale Finding or `RemediationAuthorization` bound to an epoch that has
  since been superseded is refused by the exact `AppliesTo` staleness
  check #579 already proved (reused, not reinvented).

## 9. Boundedness

| Case | Handling |
|---|---|
| Retry limit | `PolicyMaxEpochs(unit)`, checked in `EligibleRemediation` (§5) |
| No-progress detection | Reuse #475's own park-on-identical-attempt pattern (`ReasonDeterministicFailureUnchanged`) directly: an epoch producing the same digest as its predecessor parks and does **not** auto-propose the next epoch — it escalates to a mandatory human `DecisionRequest` (§4) |
| Duplicate reconciliation | Deterministic id = pure function of `(Finding.ID, unit, proposedEpoch)`, insert-only — identical pattern to `DeliveredFindingID`; naturally idempotent across reconcile passes |
| Failed remediation | Epoch N's run fails its own assurance exactly like any run's existing failure classification (no new failure class); `PolicyMaxEpochs` then decides whether epoch N+1 is still eligible or this escalates |
| Cancellation | Unchanged existing run-cancellation path; a cancelled epoch becomes a terminal, non-current, permanent row — never re-admitted as current |
| Escalation | Reuses the existing `awaiting_decision` `UnitState` outright — it already exists for exactly "this unit needs a human before it can proceed" |

## 10. Relationship to #474 (review remediation)

Disjoint by construction, not by convention: #474 triggers on a *published*
PR's live `REQUEST_CHANGES` Decision bound to that PR's current `HeadSHA`
— strictly post-publication, strictly outside WorkGraph/#473. This ADR's
mechanism triggers on a pre-publication #473 `Finding` and only ever
produces a *new epoch* (new `RunID`), never a same-`RunID` continuation —
the same-`RunID`-continuation shape stays #474's and #508(a)'s alone,
completely untouched by this design.

## 11. Relationship to #508 (decision authority)

This ADR adds no new authority *kind* — `AuthorityKindOperator` and
`DecisionResolutionAuthority`/`ResolveDecision` are reused exactly as they
exist today. It adds one new `DecisionRequest` *purpose*: "authorize
remediation epoch N for unit U, citing Finding F" — structurally identical
to every other `DecisionRequest` (`AudienceAuthority`, exact-subject
bound), resolved through the identical governed path. `WorkUnitHold` is
explicitly not reused (§2, gap #8) — its own documented invariant
(at-most-once, pre-activation) would be violated by stretching it to cover
this case.

## 12. Relationship to #480 (dynamic orchestration)

#480 is conceptual only today (zero linked PRs) and must not be a hard
dependency for a real, already-proven gap. This ADR's v1 cut (Phase 2,
below) works entirely through #508-gated human decisions. #480's only
addition, if and when it ships, is a second, policy-driven path into the
*same* `RemediationAuthorization` record (§4's "plan-preauthorized"
branch) — #480 never gets its own competing authorization mechanism, and
#581/this ADR never blocks on #480's timeline.

## 13. Minimal implementation sequence

| Phase | Scope | Owner | Observable change |
|---|---|---|---|
| 0 | This design + independent review | #581 | none (this document) |
| 1 | Widen `work_graph_activations` → `work_graph_unit_epochs`; `CurrentEpoch` projection; `Cause` discriminant on invalidation | #581 (#472-adjacent) | **none** — epoch defaults to 1 everywhere; bit-identical behavior until Phase 2 |
| 2 | `EligibleRemediation`, `RemediationRequest`/`RemediationAuthorization` records, new `DecisionRequest` purpose, frontier eligibility clause, `PolicyMaxEpochs` (hardcoded conservative default, e.g. 1) | #581 + #508 | First phase a real unit can get epoch 2 — always human-gated, no auto-authorization yet |
| 3 (later, optional) | Plan-preauthorized policy path, once #480's plan→WorkGraph compiler exists | #480 + #581 | Routine remediation stops needing a one-off human click when a plan already covers it |
| 4 (polish) | No-progress/escalation tuning, Product Workspace (#479) surfacing of epoch/remediation state | #479 | UX only |

Phase 1 is deliberately a no-observable-change schema/projection widening,
reviewable and mergeable entirely on its own, before any new authority
surface exists — consistent with "boring infrastructure until the
engineering semantics are proven."

## 14. Deterministic acceptance test plan (for Phase 2, not written here)

Reusing #475's exact fixture (`fleetFixture`/`integrationGraphUnits`/
`integrationWorker`/`blamedUnitInbox`, `runtime/workgraph_integration_test.go`):

1. Real Git conflict (today's Scenario B) → Finding delivered (today's
   #579, already proven, unchanged).
2. A `DecisionResolution` authorizes remediation epoch 2 for the blamed
   unit, citing the exact Finding id — reused authority check, durable
   provenance.
3. WorkGraph admits the epoch-2 activation row, mints a new `RunID`, runs
   the existing fresh op-DAG; a controlled test provider writes a
   non-conflicting fix; new handoff admits under epoch 2, superseding
   epoch 1's output.
4. The integrator unit — `invalidated` with `Cause: remediation_epoch_advance`
   — becomes eligible for its own epoch 2, re-runs `OpIntegrationCompose`
   against the current producer outputs, composes cleanly, fresh assurance
   passes, new integrated handoff admits.
5. Negative/boundary: a stale epoch-1-bound Finding/decision arriving after
   epoch 2 already admitted is refused (reused `AppliesTo` check);
   exceeding `PolicyMaxEpochs` escalates to `awaiting_decision` rather than
   looping; duplicate reconcile passes produce no second epoch-2 row
   (deterministic id); restart mid-epoch-2 resumes correctly (reused
   restart-test pattern); a cancelled epoch-2 run leaves epoch 1 as an
   immutable historical row, never re-admitted as current.
6. Downstream independent review/assurance (#233/#474-adjacent) operates
   on the final epoch-2-sourced integrated candidate only — reusing
   existing exact-subject review-binding tests unchanged.

## 15. Trade-offs and alternatives rejected

- **Reuse/extend `WorkUnitHold`.** Rejected: its own doc comment is
  explicit that its at-most-once, pre-activation identity rules out an
  already-activated unit; stretching it would silently violate a
  documented invariant rather than extend it.
- **Resurrect/reopen the same terminal `RunID`** instead of minting a new
  epoch/`RunID`. Rejected: violates immutable run history and the
  restart/idempotency model built entirely around a run's lifecycle being
  one-way; would require rewriting run-termination semantics repo-wide for
  one narrow case.
- **Let a Finding directly authorize re-activation**, no decision/policy
  gate at all. Rejected: directly violates the frozen invariant this
  entire ADR exists to preserve ("a delivered Finding is information, not
  authorization") and "no invented producer activation."
- **Make #480 a hard prerequisite.** Rejected: #480 is purely conceptual
  (zero PRs); gating a real, proven gap on it would stall indefinitely.
  Policy-preauthorization is cleanly deferred to Phase 3 instead.
- **A bespoke "integration units retry forever" special case** instead of
  a general epoch primitive. Rejected: this is exactly the #475-specific
  workaround the originating task explicitly said not to build. The epoch
  primitive is deliberately general — any `WorkUnit`, any role — so #480
  and future WorkGraph-level corrective actions reuse it rather than
  needing their own mechanism later.

## 16. Compatibility

A graph that never triggers remediation has exactly today's rows
(`epoch = 1` everywhere), exactly today's frontier/invalidation behavior,
and exactly today's test assertions holding unchanged. This is additive to
the schema and to the frontier predicate, never a rewrite of either.

## 17. Open questions for reviewers

1. Is `PolicyMaxEpochs` better expressed inside the existing
   `EngineeringPolicy` compiler (reusing its obligation vocabulary
   directly) rather than as a standalone counter, per AGENTS.md's "one
   obligation system" rule? (Leaning yes; needs #64 compiler owner input.)
2. Should `Cause: remediation_epoch_advance` cascading be opt-in per plan
   (a dependent only becomes eligible if the plan explicitly allows
   cascading re-integration) or always-eligible-but-still-gated-by-its-own-
   authorization (as drafted above)? Both preserve the no-automatic-
   authorization invariant; this is a UX/policy-default question, not a
   safety one.
3. Does `RemediationAuthorization` belong as its own table, or as a new
   `DecisionResolution.Outcome` variant reusing #508's existing table
   wholesale? Leaning toward the latter for the human-gated path and a
   separate, clearly-labeled record only for the plan-preauthorized path,
   to avoid implying every preauthorized epoch has a resolved decision
   that never existed.
