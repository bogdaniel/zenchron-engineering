# ADR-0008: Bounded, Governed Remediation Epochs for an Already-Admitted WorkUnit

- Status: Proposed (design + independent review only — not implemented)
- Date: 2026-10-12 (revised twice same day: pass 1 resolved migration,
  #508 extension, epoch state separation, concurrency, execution scope,
  budget semantics and three open questions; pass 2 — this one — fixes
  three focused correctness concerns found in pass 1's own design: §8's
  admission atomicity across epoch+run+operations, effective-output vs.
  historical-handoff semantics while remediation is outstanding, and
  how a human decision can authorize an epoch beyond the default policy
  ceiling without implicit expansion)
- Issue: #581 (bridge gap identified during #475 bounded acceptance,
  PR #580). Depends on: #472 (WorkGraph), #473 (communication), #508
  (decision resolution). Related, not depended on: #474 (review
  remediation, disjoint), #480 (dynamic orchestration, conceptual only —
  see §14).
- Supersedes: nothing. Extends #472's `WorkUnitActivation` and #508's
  `DecisionRequest`/`DecisionResolution` vocabulary; does not replace
  either, and does not modify any accepted ADR.

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

## 3. Compatibility and migration strategy (resolved)

The prior draft asserted "no backfill necessary" without showing the
mechanism. Here is the mechanism, concretely:

**`work_graph_activations` is never altered, migrated, or re-keyed.** It
stays exactly as it is today — schema, primary key `(graph_id, unit_id)`,
every existing row, every existing query against it. This is epoch 1's
system of record, unconditionally, forever.

A new, wholly separate, **additive-only** table holds every epoch beyond
the first:

```sql
CREATE TABLE work_graph_remediation_epochs (
  graph_id          TEXT NOT NULL,
  unit_id           TEXT NOT NULL,
  epoch             INTEGER NOT NULL,   -- 2, 3, ... (epoch 1 is never a row here)
  batch_id          TEXT NOT NULL,      -- same role as today's WorkUnitActivation.BatchID
  run_id            TEXT NOT NULL,      -- FK into runs; created in the SAME
                                        -- transaction as this row, see §8
  inputs_digest     TEXT NOT NULL,      -- same role as today's ActivationInputsDigest:
                                        -- the dependency-set digest THIS epoch activated
                                        -- against, for invalidation comparison (§4.3)
  authorization_id  TEXT NOT NULL,      -- FK, see §5; never null here
  activated_at      TEXT NOT NULL,
  PRIMARY KEY (graph_id, unit_id, epoch)
  -- Deliberately NO status/outcome column (pass 1's mistake): every field
  -- above is a fact about HOW this epoch was activated, written once in
  -- one transaction, and never updated again. "How this epoch is going"
  -- is always read off `run_id`'s own, already-durable, already-event-
  -- journaled lifecycle (EpochOutcome, §4.1) - never a second,
  -- separately-mutated copy here.
);
```

**Why this, not an ALTER on the existing table:** a brand-new, empty table
requires zero migration of zero existing rows. Every graph that has ever
existed on main today has zero rows in this table, by definition (the
table did not exist when they were created, and nothing backfills it).
Projection correctness for every existing graph is therefore not "no
backfill needed" as an assertion — it falls out structurally: a query
against an empty table returns nothing, so the derived facts below are
identical to today's for every unit that has never remediated:

```text
CurrentEpoch(unit)        = MAX(epoch) FROM work_graph_remediation_epochs
                             WHERE graph_id=? AND unit_id=?
                             -- empty for all existing units → NULL
                           ?? 1   -- SQL COALESCE: no remediation row → epoch 1

CurrentEpochSource(unit)  = CurrentEpoch == 1
                             ? work_graph_activations row (unchanged table, unchanged query)
                             : work_graph_remediation_epochs row at CurrentEpoch
```

Every existing call site that reads `work_graph_activations` directly for
a unit's activation/run/batch continues to compile and return the exact
same answer it does today, **because it is never pointed at the new table
at all** unless a reviewer explicitly updates it to go through
`CurrentEpochSource`. Phase 1 (§13) is precisely: add the new table (DDL
only, no reads from it wired up yet) plus the `CurrentEpoch`/
`CurrentEpochSource` projection helpers, with zero call sites changed to
use them. That is independently reviewable, independently mergeable, and
**provably** a no-op for every graph on main today, because the new table
has no rows and no code path queries it yet. Phase 2 (§13) is the first
phase that wires `ProjectWorkGraph`'s frontier/output resolution through
`CurrentEpochSource` instead of `work_graph_activations` directly — at
that point the behavior for a never-remediated unit is identical by the
`?? 1` fallback shown above, and the only observably new behavior is for
units that now, for the first time, have a row in the new table.

This is the proof the prior draft was missing: compatibility is not
claimed, it is structural — an empty additive table cannot change the
answer to any query that previously ignored it.

## 4. Epoch state model

### 4.1 Epoch-level facts (per `(unit, epoch)`, all derived, never stored)

The pass-1 draft stored a `status` column on
`work_graph_remediation_epochs` and updated it as the run progressed. A
reviewer correctly flagged this as inconsistent with "historical
activation records are immutable" — a column that transitions *is* a
mutation of the historical record, whatever we call it.

**Fix: the epoch row has no mutable column at all.** Once written (§8),
every field of an epoch row — `graph_id, unit_id, epoch, run_id,
authorization_id, activated_at` — is permanent and never updated. "How did
it go" is never asked of the epoch row; it is always asked of the **bound
run's own, already-existing, already-event-journaled lifecycle** — the
exact same append-only mechanism #63 already uses for every run on main
today, epoch 1 included:

```text
EpochOutcome(unit, epoch) — derived, never stored
  = "running"            if the epoch's bound run's ItemState is non-terminal
  = "completed_admitted" if terminal AND that run produced an admitted Output
  = "completed_no_output" if terminal, ItemCompleted, no admitted Output
  = "failed"              if terminal, ItemFailed
  = "cancelled"           if the run's own cancellation fact is set
```

This makes the immutability claim literally true (there is nothing to
transition) and deletes an entire category of "what if the status update
races the run's own event append" bugs by construction — there is only
ever one append-only source of truth for an epoch's outcome, the run
itself, exactly as today.

| Concept | Definition | Computed from |
|---|---|---|
| **LatestAttemptEpoch** | The highest epoch number with *any* row | `MAX(epoch)` across `work_graph_activations` (epoch 1) ∪ `work_graph_remediation_epochs` |
| **LatestAdmittedHandoffEpoch** | The highest epoch whose `EpochOutcome` is `completed_admitted` | Same union, filtered via the derived `EpochOutcome` above |
| **`superseded(unit, epoch)`** | True iff some epoch `> epoch` has `EpochOutcome = completed_admitted` | Derived; never stored. An epoch row is immutable from the instant it is written — "superseded" is a statement about what exists *elsewhere*, never about that row |

### 4.2 Unit-level, downstream-facing facts (resolves reviewer concern "effective-output semantics")

The pass-1 draft conflated "the last handoff we ever successfully
admitted" with "what a downstream unit may currently depend on." A
reviewer correctly flagged that this lets a **failed** remediation epoch
silently fall back to presenting the *original, still-unresolved,
conflicting* handoff as current — exactly the bug the whole mechanism
exists to prevent. Fix: two more concepts, kept explicitly separate.

```text
CurrentEffectiveOutput(unit)
  = the admitted handoff of LatestAdmittedHandoffEpoch(unit)
  -- a pure, historical fact about the handoff table. Unaffected by
  -- whether remediation is in flight, failed, or unresolved. NEVER
  -- read directly by downstream dependency satisfaction (below) —
  -- only by things that need the historical record, e.g. audit, or
  -- as the base candidate for the NEXT remediation attempt (§9).

RemediationState(unit) — derived, never stored, one of:
  none                — no Finding has ever produced a live
                         RemediationRequest for this unit's current
                         LatestAdmittedHandoffEpoch, or every one that
                         ever did has been explicitly "dismiss"ed (§4.3)
  pending_decision     — a live DecisionRequest(Purpose: epoch_authorization)
                         is outstanding for this unit
  running              — LatestAttemptEpoch's bound run is non-terminal
  exhausted_unresolved — LatestAttemptEpoch > LatestAdmittedHandoffEpoch
                         (the most recent attempt did not succeed) AND no
                         live pending decision AND no further epoch is
                         eligible under the effective ceiling (§4.3) AND
                         the triggering Finding is still live (not dismissed)
  resolved             — LatestAttemptEpoch == LatestAdmittedHandoffEpoch
                         (the most recent attempt succeeded) with no live
                         Finding against it, OR the live Finding was
                         explicitly "dismiss"ed (§4.3)

DownstreamDependencySatisfaction(unit)
  = CurrentEffectiveOutput(unit)   if RemediationState(unit) ∈ {none, resolved}
  = NOT SATISFIED                  if RemediationState(unit) ∈
                                    {pending_decision, running, exhausted_unresolved}
                                    -- regardless of whether a historical
                                    -- admitted handoff exists
```

**This is the exact fix for "never make an old conflicting handoff newly
usable merely because a later remediation failed":**
`DownstreamDependencySatisfaction` — the only fact a dependent unit's own
digest/satisfaction computation is allowed to read (§10) — drops to "not
satisfied" the moment remediation is even *proposed* (`pending_decision`)
and **stays** not-satisfied through `running` and through
`exhausted_unresolved`. A failed or cancelled epoch never moves
`RemediationState` back toward `none`/`resolved` on its own — only two
things do: a *later* epoch reaching `completed_admitted` (genuine
success), or an explicit human `dismiss` (§4.3). There is no third,
implicit path back to "usable."

Concretely, by state:

| While remediation is... | `DownstreamDependencySatisfaction` | Why |
|---|---|---|
| `pending_decision` (decision not yet resolved) | Not satisfied | The system already knows the current handoff is disputed; presenting it as satisfied would race the very decision meant to settle that |
| `running` (an epoch is executing) | Not satisfied | Same reason, now with an attempt actively in flight |
| just failed/cancelled, more epochs still eligible | Not satisfied (`RemediationState` stays `pending_decision` or re-enters it for the next epoch) | The conflict is still unresolved; a failed attempt is not progress toward "usable" |
| `exhausted_unresolved` (ceiling reached, nothing left to try automatically) | **Still not satisfied** | This is precisely the case the reviewer named: exhaustion must not silently resurrect the old handoff. The unit surfaces as `awaiting_decision` (§11) until a human `dismiss`es the Finding or explicitly raises the ceiling (§4.3) |
| `resolved` via a new `completed_admitted` epoch | Satisfied, at the **new** epoch's handoff | Genuine success |
| `resolved` via explicit `dismiss` | Satisfied, at `CurrentEffectiveOutput` (still the original handoff, now explicitly accepted) | The *only* governed path back to the original handoff being usable despite its Finding — an auditable human act, never a default |

`UnitState` projection (gap #1) changes in two places, not one: frontier
membership (gap #3) becomes `!Activated(unit) OR EligibleForNextEpoch(unit)`
(§7), **and** the existing dependency-satisfaction predicate
(`orchestration/workgraph_projection.go:283`, `satisfied[id] =
fact.Item==Completed && (!RequiresReview||ReviewApproved)`) gains one more
conjunct: `&& RemediationState(unit) ∈ {none, resolved}` — additive, same
shape, no rewrite. The unit's own reported `UnitState` is still its
**current epoch's** child `ItemState`, unchanged.

### 4.3 Invalidation cause and the `dismiss` outcome

Invalidation (gap #4/#5) gains a `Cause` discriminant:

- `external_replacement` — today's only case. Stays exactly as it is:
  terminal, no re-run. An operator or tool legitimately superseding a
  unit's output outside this mechanism must never auto-cascade.
- `remediation_epoch_advance` — new, fires only on the transition into
  `RemediationState = resolved` **via a new epoch** (never via `dismiss`,
  and never while merely `pending_decision`/`running`/
  `exhausted_unresolved` — see §4.2's table and §10). The downstream unit
  becomes eligible for *its own* next epoch, not permanently invalidated.
  Gated by the same authorization chain as every other epoch advance.

`DecisionResolution.Outcome` for an `epoch_authorization` purpose (§6)
gains a fourth value beyond `approve`/`reject`/`escalate`:

- **`dismiss`** — the human explicitly declares the justifying Finding
  does not require remediation; the unit's current
  `CurrentEffectiveOutput` is accepted as final despite it. This is the
  *only* transition that moves `RemediationState` from
  `exhausted_unresolved` (or `pending_decision`) back to `resolved`
  without a new epoch ever running. It is recorded durably (which
  `DecisionResolution` dismissed which Finding, by whom, when) — never a
  silent default, always an explicit, auditable act.

## 5. Authorization

A Finding never authorizes anything by itself (never-cut: "no invented
producer activation"). A single, dedicated, #581-owned table —
**not** a #508 `DecisionResolution` variant (resolves the prior draft's
open question 3; see §18.3) — is the one record either path produces:

```sql
CREATE TABLE remediation_authorizations (
  id                 TEXT PRIMARY KEY,   -- deterministic: f(finding_id, unit_id, proposed_epoch)
  graph_id           TEXT NOT NULL,
  unit_id            TEXT NOT NULL,
  proposed_epoch     INTEGER NOT NULL,
  finding_id         TEXT NOT NULL,      -- the exact justifying Finding
  authorized_by      TEXT NOT NULL,      -- "policy" | "decision"
  policy_clause_ref  TEXT,               -- non-null iff authorized_by = "policy"
  decision_resolution_id TEXT,           -- non-null iff authorized_by = "decision" (FK into #508's existing table)
  raises_ceiling_to  INTEGER,            -- non-null ONLY when authorized_by = "decision" AND
                                          -- proposed_epoch exceeded the unit's effective ceiling
                                          -- at authorization time (§11) — NEVER settable by the
                                          -- "policy" path; see §11's ceiling-raise mechanism
  authorized_at      TEXT NOT NULL
);
```

`raises_ceiling_to` is the explicit answer to "how does a human decision
enable epoch 2 under a default `PolicyMaxEpochs = 1`" (§11): it exists
*only* as a field on a `decision`-authorized row, is only ever non-null
when the resolving `DecisionResolution` explicitly carried a
`RaisesCeilingTo` value (§6), and it is structurally impossible for the
plan-preauthorized (`"policy"`) path to populate it — `policy_clause_ref`
and `raises_ceiling_to` are mutually exclusive by the check constraint
`CHECK ((authorized_by = 'policy' AND raises_ceiling_to IS NULL) OR
authorized_by = 'decision')`. A ceiling raise is therefore always a
human's explicit, auditable, exact-unit-scoped act, never policy-derived.

```text
                    Finding (live, exact-subject-bound)
                              │
                    EligibleRemediation() — pure function, §6
                              │
                    RemediationRequest (durable, deterministic id)
                    ┌─────────┴─────────┐
      plan-preauthorized           requires human decision
      (RemediationPolicy           (default; mandatory once
       compiled into the           auto-path's own bound is
       unit's EngineeringContract, exhausted, or a no-progress
       §64's existing plan-        park already occurred once)
       approval boundary — §11)              │
              │                     #508 DecisionRequest, new
              │                     Purpose: epoch_authorization (§7)
              │                              │
              │                     operator resolves via
              │                     DecisionResolutionAuthority /
              │                     ResolveDecision — REUSED AS-IS
              │                     → DecisionResolution row (unchanged
              │                       #508 table, unchanged shape)
              └─────────────┬────────────────┘
                             ▼
           remediation_authorizations row (THIS ADR's own table,
           citing either the policy clause or the DecisionResolution.ID
           — #508's own table is never overloaded with a #581 concept)
```

- `remediation_authorizations` is what `EligibleForNextEpoch` reads (§6),
  never the Finding directly. This mirrors the project's own core
  invariant chain (facts → policy → obligations → evidence → authority)
  instead of inventing a parallel one.
- Plan-preauthorization is compiled at contract-compile time
  (`OpContractCompile`), from the approved `EngineeringPlan` — exactly
  #64's existing plan-approval boundary, not a new approval surface (§11).
- Identity is never authority: the same law #508 already states applies
  unchanged — a model-authored Finding or a worker's own claim is never
  itself sufficient, only a `remediation_authorizations` row traceable to
  an approved plan clause or a resolved `DecisionResolution`.

## 6. The #508 extension, defined explicitly (resolves reviewer concern 2)

#508 has exactly two existing mechanisms, and this ADR touches neither's
*existing* behavior — it adds a **third, structurally distinct** one.

| | #508(a): in-run decision | #508(b): `WorkUnitHold` | **New: epoch authorization** |
|---|---|---|---|
| Triggers on | a live run's own emitted `decision_request` | an operator-placed hold, pre-activation | a live Finding + `EligibleRemediation` (§6 below) |
| Required unit/run state | run still live, mid-run | unit not yet activated | unit's current epoch **terminal** |
| Resolution continues | the **same `RunID`**, next `OpExecutionInvoke` attempt | the unit's **first-ever** activation, once | a **brand-new `RunID`**, fresh op-DAG, under a **new epoch** |
| Resolved via | `DecisionResolutionAuthority`/`ResolveDecision` | an operator clearing the hold | `DecisionResolutionAuthority`/`ResolveDecision` — **same function, reused as-is** |

The explicit extension required of #508's `DecisionRequest` type: a new,
closed `Purpose` value, `epoch_authorization`, structurally distinguishing
it from an in-run question so the two can never be conflated by the
resolution code:

```text
DecisionRequest{
  Purpose: "epoch_authorization",      // new closed value; existing in-run
                                        // requests keep their existing purpose
  Subject: {
    Handoff:  <unit's LatestAdmittedHandoffEpoch handoff id>,
    Revision: <that handoff's HandoffSubject, unchanged shape>,
  },
  EpochAsk: {                          // new, typed, non-free-text sub-struct;
    ProposedEpoch: N,                  // present iff Purpose == "epoch_authorization"
    FindingID:     "<justifying Finding's id>",
    ExceedsEffectiveCeiling: bool,     // computed by the system when it emits the
                                       // request, true iff N > the unit's current
                                       // effective ceiling (§11) — tells the human,
                                       // up front, exactly what they'd be approving
  },
}
```

**Required guard, stated explicitly so it is not left implicit for an
implementer to discover the hard way:** `bindExecutionInvoke`'s existing
same-`RunID` continuation path (#508(a)) must refuse to fire for a
`DecisionResolution` whose request has `Purpose == "epoch_authorization"`
— and the reverse: the new epoch-activation code path (§8) must refuse any
`DecisionResolution` whose request does not have that exact purpose. These
are mutually exclusive by construction (one requires a live run, the other
requires a terminal one), but the purpose discriminant is what lets that
be asserted and tested rather than merely assumed from run state alone.

`DecisionResolution.Outcome` for this purpose is one of:

- **`approve`** — binds `remediation_authorizations.decision_resolution_id`,
  exact `proposed_epoch` and `finding_id` preserved verbatim from the
  request (no silent substitution). If `EpochAsk.ExceedsEffectiveCeiling`
  was true, `approve` **must** also carry an explicit `RaisesCeilingTo`
  value equal to `proposed_epoch`; resolving `approve` on an
  over-ceiling request with no `RaisesCeilingTo` is itself refused
  (fail closed — the human must affirmatively acknowledge the raise, it
  is never inferred from merely approving).
- **`reject`** — no `remediation_authorizations` row written. The unit
  stays wherever it is (§4.2's `RemediationState` table); the Finding
  remains live; no further `EligibleRemediation` auto-retry for the
  *same* Finding without a fresh triggering event.
- **`dismiss`** — the Finding is explicitly declared non-blocking (§4.3).
  No epoch row is written; `RemediationState` moves to `resolved` at the
  *existing* `CurrentEffectiveOutput`.
- **`escalate`** — no-op for this mechanism; reserved for a human routing
  the decision elsewhere, outside this ADR's scope.

`WorkUnitHold` (#508(b)) is explicitly not reused (gap #8): its own
documented invariant (at-most-once, pre-activation) would be violated by
stretching it to cover an already-activated unit.

## 7. Trigger (`EligibleRemediation`)

A pure function, functional core, no side effects:

```text
EligibleRemediation(finding, unit, graph) -> (RemediationRequest, ok)
  require: finding is live                              (Live())
  require: finding.Route.Unit == unit.Name
  require: finding.Subject.Handoff == unit's LatestAdmittedHandoffEpoch's handoff id
           AND AppliesTo(finding.Subject.Revision, that handoff's HandoffSubject)
                                                          (reused #475 staleness check)
  require: EpochOutcome(unit, LatestAttemptEpoch(unit)) is terminal
           (never "running" — §4.1)
  require: no existing remediation_authorizations row already covers
           (finding.ID, unit, nextEpoch)                 (idempotent — deterministic id,
                                                            same pattern as DeliveredFindingID)
  → RemediationRequest{
      finding.ID, unit,
      proposedEpoch: LatestAttemptEpoch(unit)+1,
      exceedsEffectiveCeiling: (LatestAttemptEpoch(unit)+1) > EffectiveCeiling(unit),  (§11)
    }
```

Note what changed from pass 1: **`EligibleRemediation` no longer hard-refuses
when the proposed epoch exceeds `PolicyMaxEpochs`.** It always produces a
`RemediationRequest` when a live, exact-subject Finding genuinely names
this unit and nothing already covers the next epoch — the ceiling only
decides *which authorization path can grant it* (§5, §11), never whether
the question is even raised. This is what makes "a human decision can
authorize epoch 2 under a default ceiling of 1" possible at all (resolves
reviewer concern P2) without weakening the ceiling itself: the
plan-preauthorized path still cannot grant past the compiled
`PolicyMaxEpochs`, full stop (§11); only an explicit, flagged,
human `RaisesCeilingTo` can.

`EligibleForNextEpoch(unit)` (used by the frontier, §4/§8) is then simply:
a `remediation_authorizations` row exists for `(unit, nextEpoch)` and no
epoch `>= nextEpoch` has activated yet. Deterministic, projection-only, no
new mutable state beyond the authorization and epoch rows already
described.

## 8. Scheduling and concurrency (resolves reviewer concern 4)

Two existing, independent gates, unchanged in kind:

- **WorkGraph layer** (`runtime/workgraph.go` `activateWorkUnit`): decides
  *which unit* gets a run. Gains one additional frontier clause
  (`EligibleForNextEpoch`, §7) alongside the existing `!Activated` clause.
- **Per-run Scheduler** (`runtime/scheduler.go`, `runtime/operations.go`):
  decides *which operation* runs next inside a run's own op-DAG. Completely
  unchanged — a new epoch mints a brand-new `RunID`, which gets the
  existing fresh operation set exactly like any new unit's first run. No
  scheduler code changes.

**Fix (resolves reviewer concern P1a):** the pass-1 draft showed a single
atomic `INSERT...SELECT` that wrote the epoch row with `run_id` already
populated and `status = 'running'` — but creating the actual
`EngineeringRun` row and seeding its operation DAG are necessarily
*separate* statements (they populate different tables: `runs`,
`run_operations`), so a single `INSERT...SELECT` cannot have been the
whole story. Left as pass 1 wrote it, this has a real hazard: the
scheduler (`Scheduler.Next`, §8's earlier text) leases purely from
`run_operations`, so if a run's operations are ever seeded *before* the
owning epoch row is confirmed to have won the admission race, a losing
reconciliation pass's abandoned run could still have leasable operations
— a second, duplicate, ungoverned execution of the same remediation
attempt. Conversely, if the epoch row commits *before* the run/operations
exist, a crash in that window leaves an epoch row pointing at a `RunID`
that was never actually created — an orphaned epoch.

**The fix is explicit transactional scope, not a provisioning state:**
the epoch row, the new `EngineeringRun` row, and its full initial
`run_operations` set are all written inside **one SQLite transaction**:

```text
BEGIN
  -- 1. Same admission guard as pass 1, now the FIRST statement in a
  --    multi-statement transaction rather than the only one:
  INSERT INTO work_graph_remediation_epochs
    (graph_id, unit_id, epoch, run_id, authorization_id, activated_at)
  SELECT :graph_id, :unit_id, :next_epoch, :run_id, ra.id, :now
  FROM remediation_authorizations ra
  WHERE ra.graph_id = :graph_id AND ra.unit_id = :unit_id
    AND ra.proposed_epoch = :next_epoch
    AND NOT EXISTS (                      -- no epoch currently running for this unit
      SELECT 1 FROM (epoch rows ∪ work_graph_activations for this unit) e2
      JOIN runs ON runs.id = e2.run_id
      WHERE runs.item_state NOT IN (terminal states)   -- EpochOutcome = "running", §4.1
    );
  -- :next_epoch is computed the same way as pass 1 (COALESCE(MAX(epoch),1)+1),
  -- read in this same transaction, so it observes a consistent snapshot.

  -- 2. Only reached if step 1 actually inserted a row (rows-affected = 1):
  INSERT INTO runs (id, work_unit_id, item_state, ...) VALUES (:run_id, ...);
  INSERT INTO run_operations (run_id, op_kind, ...) VALUES (:run_id, 'OpSourceObserve', ...), ...;
COMMIT
```

- **Atomic across epoch+run+operations:** SQLite's own transaction
  durability is the only mechanism relied on — there is no
  application-level "provisioning" status to invent or reconcile, because
  there is no durable intermediate state at all. Either the whole
  transaction commits (epoch row, run row, and every seeded operation all
  exist together) or none of it does. A crash between steps 1 and 2 is
  indistinguishable from a crash before step 1: on restart, nothing
  committed, so `EligibleRemediation` (§7) re-evaluates from exactly the
  same durable rows it would have seen had the attempt never started, and
  legitimately retries with a freshly generated `run_id`. This is the same
  transactional discipline #472 already requires for epoch 1's own
  activation+run+operation creation today, extended unchanged to later
  epochs — not a new mechanism.
- **No orphaned runs, no orphaned epochs, by construction:** because step
  2 only ever executes inside the same transaction as step 1's successful
  insert, a run row can never exist without its governing epoch row (no
  orphaned run with leasable operations and no owning epoch — closing the
  scheduler hazard above), and an epoch row can never exist without its
  run (no orphaned epoch pointing at nothing).
- **Idempotent:** two concurrent reconciliation passes attempting the same
  transaction compute the identical `:next_epoch` from the identical
  durable rows; the `PRIMARY KEY (graph_id, unit_id, epoch)` on step 1's
  `INSERT` lets only one transaction's step 1 actually affect a row.
  **The losing pass's transaction must check `rows-affected` after step 1
  and roll back without executing step 2 at all** if it inserted nothing
  — this is the one explicit instruction pass 1 omitted ("the missing
  conflict-handling semantics"): a loser never proceeds to create a run.
  It simply observes the winner's row on its next read, identical to how
  `ActivateWorkUnit`'s existing `ON CONFLICT DO NOTHING` already behaves
  for first activation today.
- **Resistant to concurrent reconciliation:** the `NOT EXISTS (...
  running)` guard inside step 1 is evaluated fresh, inside the same
  transaction, against live rows at commit time, under SQLite's existing
  serialized-writer isolation (the same isolation this runtime's whole
  single-writer SQLite model already relies on everywhere else — this ADR
  introduces no new concurrency primitive).
- **Historical immutability preserved:** per §4.1, no field of the epoch
  row is ever updated after this transaction commits. There is nothing
  left to make "consistent with immutability" because there is no
  post-creation mutation to reconcile against it.
- **Capacity:** the existing plan-scoped `PlanBudgetEnvelope`
  (`MaxChildRuns`, `MaxConcurrency`, ...) applies unchanged, since each
  epoch is a real new child run and already counts against it.
- **Restart:** identical to today's story. `ProjectWorkGraph` plus the
  epoch rows plus each epoch's child `ItemState` remains a pure function
  of durable state. A mid-flight epoch's run resumes exactly like any
  other run resumes today; no new recovery logic. See §16 for the explicit
  restart-mid-epoch negative scenario.

## 9. Execution scope (resolves reviewer concern 5)

A new epoch's run is a genuinely fresh `EngineeringRun`, with every input
explicit and bounded:

| Input | Exact value | Why |
|---|---|---|
| **Base candidate** | `LatestAdmittedHandoffEpoch`'s handoff `CandidateRevision` (§4) — **not** necessarily the immediately-preceding epoch, if that one was `failed`/`completed_no_output` | The new attempt builds on the last thing that actually succeeded, never on a failed intermediate |
| **Contract identity** | The **same, unchanged, originally compiled** `EngineeringContract`/obligations as epoch 1 (never-cut: "no automatic privilege expansion during contract recompilation") | Remediation is a new attempt against the existing contract, never a new contract |
| **Finding subject** | Bound by `remediation_authorizations.finding_id`, re-checked `AppliesTo`-live at execution-context-construction time (not just at authorization time) — if the Finding went stale between authorization and dispatch, the run refuses to start and the epoch row is marked `failed` with cause `stale_finding_at_dispatch` | An authorization is a point-in-time fact; execution must not trust it blindly if time has passed |
| **Allowed changes** | Bounded to the Finding's own named paths, enforced — not merely hinted to the provider — by a remediation-scope assurance gate (reusing the existing `Conflict.Paths`/`VerifyRemediationScope` bounded-scope concept from #475's own PR #541/#546 review history) that runs as part of the new epoch's own assurance phase and fails the run itself with cause `scope_exceeded` (so `EpochOutcome` derives to `failed`, §4.1) if the candidate's diff touches anything outside that set | A provider's good-faith attempt is not trusted as the enforcement boundary; the existing scope-check primitive already proven in #475's history is reused as the actual gate |
| **Context given to the provider** | The base candidate, the exact triggering Finding (surfaced prominently), and the permitted scope — via the existing `communicationContext` renderer #579 already extended, no new renderer | Reuse, not a parallel context mechanism |

## 10. Supersession

**Critical correction from pass 1 (resolves reviewer concern P1b):**
downstream `WorkUnitInputs.Digest()` (gap #4) is computed over each
upstream unit's **`DownstreamDependencySatisfaction`** (§4.2), **never**
over raw `CurrentEffectiveOutput`. Pass 1 said "digest recomputation"
without specifying which fact feeds it; left as the historical
`CurrentEffectiveOutput`, a downstream unit would see the digest change
(or fail to change) based purely on the last *successful* handoff,
oblivious to whether remediation is currently in flight, failed, or
unresolved — exactly the bug this correction closes.

- While an upstream unit's `RemediationState` is `pending_decision`,
  `running`, or `exhausted_unresolved`, its `DownstreamDependencySatisfaction`
  reads "not satisfied" (§4.2) regardless of any historical admitted
  handoff. A downstream unit depending on it is therefore **`blocked`**
  (today's existing, unchanged `UnitState`) during that whole window —
  never `invalidated`, since nothing about its own already-admitted
  output changed; it simply no longer has a satisfied dependency, exactly
  as if the upstream had never produced output at all. This is a
  **narrower, calmer** behavior than pass 1 implied: a downstream unit
  does not churn through invalidation on every failed upstream attempt,
  it simply waits.
- The instant upstream `RemediationState` transitions to `resolved` via a
  **new** `completed_admitted` epoch, `DownstreamDependencySatisfaction`
  changes value for the first time since the producer's original epoch —
  *this* is what the digest recomputation actually observes, and *this*
  is what fires `Cause: remediation_epoch_advance` invalidation (§4.3) on
  a downstream unit that had already consumed the producer's original
  output before remediation began. Gated by the same
  `remediation_authorizations` chain as every other epoch advance — never
  automatic cascading past this one hop without its own authorization
  (§18.2).
- The instant upstream `RemediationState` transitions to `resolved` via
  **`dismiss`** instead, `DownstreamDependencySatisfaction` returns to
  `CurrentEffectiveOutput` — the **original, unchanged** handoff. No new
  epoch ever ran; nothing about the downstream unit's own already-admitted
  output or digest changed, so no invalidation fires. The downstream unit
  simply unblocks (if it was waiting) back onto the handoff it already
  had.
- Re-integration reuses #475's `OpIntegrationCompose` and conflict/success
  classification completely unchanged: current `WorkUnitInputs` now
  resolve to the producer's new `DownstreamDependencySatisfaction` value
  plus the unaffected other producer's own current output, Git-composes,
  fresh assurance gates it exactly as today
  (`TestWorkGraphIntegrationHandoffWaitsForRequiredSemanticAssurance`,
  unchanged).
- A stale Finding or `remediation_authorizations` row bound to an epoch
  that has since been superseded is refused by the exact `AppliesTo`
  staleness check #579 already proved (reused, not reinvented) — see the
  explicit `stale_finding_at_dispatch`/`stale_authorization` negative
  scenarios in §16.

## 11. Boundedness and budget (resolves reviewer concerns 6 and P2)

**Resolved: `PolicyMaxEpochs` counts total epochs, including the
original.** `PolicyMaxEpochs(unit) = 1` means "no remediation ever" — the
default, and today's actual behavior for every existing unit, since the
new table is empty. `PolicyMaxEpochs(unit) = 2` means exactly one
remediation attempt is auto/plan-authorizable. This reads naturally as
"max attempts," consistent with the existing run-scoped `AttemptLimit`
convention (`execution_continuation_budget.go`), and avoids an off-by-one
ambiguity between "the limit" and "the limit on top of the original."

### 11.1 `EffectiveCeiling` and the ceiling-raise mechanism (resolves P2)

Pass 1 left a real contradiction: with `PolicyMaxEpochs = 1` as the
default for every existing approved plan (correct — no implicit
expansion), `EligibleRemediation`'s old hard refusal (`nextEpoch <=
PolicyMaxEpochs`) meant a human could *never even be asked* to authorize
epoch 2, because the system would refuse to produce the
`RemediationRequest` in the first place. That defeats the goal's own
requirement: "human decisions only when genuinely required" presumes a
human decision *can* genuinely authorize something policy alone would
not.

**Fix:** two numbers, not one, and `EligibleRemediation` no longer
refuses based on either (§7) — only the *grant* is gated:

```text
EffectiveCeiling(unit)
  = max(
      PolicyMaxEpochs(unit),                 -- compiled into the
                                              -- EngineeringContract at
                                              -- OpContractCompile time,
                                              -- from the approved plan;
                                              -- default 1 for every plan
                                              -- compiled before/without
                                              -- this feature
      max(raises_ceiling_to) across every remediation_authorizations row
        for this exact unit where authorized_by = "decision"
                                              -- additive, per-UNIT,
                                              -- per-DECISION override;
                                              -- never touches the
                                              -- compiled contract
    )
```

- **`PolicyMaxEpochs`** is the only number the plan-preauthorized
  (`"policy"`) authorization path (§5) may ever grant up to. It is
  compiled once, at plan-approval time, through the existing
  `EngineeringPolicy` compiler — the *same* human plan-approval act that
  already governs every other obligation, not a new approval surface.
  Changing it for an existing plan is an ordinary plan amendment, going
  through #472's existing contract-reassessment path. The "policy" path
  can never see or set `raises_ceiling_to` (§5's check constraint) — it
  is structurally blind to ceiling raises, by schema, not by convention.
- **A ceiling raise** (`raises_ceiling_to`) is never a contract change,
  never a policy change, and never plan-wide. It is one field on one
  `remediation_authorizations` row, written only when a human's
  `DecisionResolution` explicitly carried `RaisesCeilingTo` (§6), scoped
  to exactly the one unit that row names. It raises `EffectiveCeiling`
  for *that unit only* — no other unit in the graph, no other graph, no
  future plan inherits it. The compiled `PolicyMaxEpochs` obligation
  itself is never rewritten; `EffectiveCeiling` is a runtime `max()` over
  it and these explicit, auditable, per-unit overrides, nothing more.
- **This is precisely how "an authorized human decision enables epoch 2
  for an existing approved plan" works, without implicit expansion:** the
  plan's compiled `PolicyMaxEpochs` stays exactly `1`, forever, for that
  plan. A human resolving an `epoch_authorization` `DecisionRequest`
  whose `EpochAsk.ExceedsEffectiveCeiling` was `true` must explicitly set
  `RaisesCeilingTo: 2` on their `approve` (§6) — an affirmative,
  visible, recorded act — which writes one `remediation_authorizations`
  row with `raises_ceiling_to = 2`, scoped to that one unit. No contract
  is recompiled. No other unit, graph, or plan is affected. The raise is
  as auditable as the `DecisionResolution` it came from, because it
  *is* that resolution, by foreign key.

**Integration with existing policy obligations:** `PolicyMaxEpochs` is not
a standalone runtime config value. It is compiled, at `OpContractCompile`
time, into the unit's `EngineeringContract` as an ordinary obligation,
through the *same* `EngineeringPolicy` compiler that already produces
every other obligation for that unit/role/`ExecutionKind` — reusing
AGENTS.md's own "`EngineeringPolicy` is the only obligation system in this
product" rule rather than adding a second one. A plan-approval-time change
to this obligation goes through the exact same contract-reassessment path
#472 already gates privilege-relevant changes through ("contract
reassessment when observed scope materially differs from predicted
scope") — no new privilege-escalation surface is introduced by letting an
approved plan set this number.

| Case | Handling |
|---|---|
| Retry limit | `EffectiveCeiling(unit)` (§11.1), checked at *grant* time, not at request time (§7) |
| No-progress detection | Reuse #475's own park-on-identical-attempt pattern (`ReasonDeterministicFailureUnchanged`) directly: an epoch producing the same digest as its predecessor settles `EpochOutcome = failed` with cause `no_progress` (derived from the run's own event, §4.1) and does **not** auto-propose the next epoch via the plan-preauthorized path even if `PolicyMaxEpochs` would otherwise allow it — it escalates to a mandatory human `DecisionRequest` (§6) regardless of authorization path |
| Duplicate reconciliation | Deterministic id = pure function of `(Finding.ID, unit, proposedEpoch)`, insert-only — identical pattern to `DeliveredFindingID`; naturally idempotent across reconcile passes (§8) |
| Failed remediation | Epoch N's run fails its own assurance exactly like any run's existing failure classification (no new failure class); `EffectiveCeiling` then decides whether epoch N+1 is still grantable or this reaches `exhausted_unresolved` (§4.2) |
| Cancellation | Unchanged existing run-cancellation path; a cancelled epoch becomes a terminal, non-current, permanent row — never re-admitted as current |
| Exhaustion with the original conflict still unresolved | `RemediationState = exhausted_unresolved` (§4.2); `DownstreamDependencySatisfaction` stays "not satisfied" — the original handoff is **never** silently resurrected (the exact P1b fix, §4.2/§10); only a human `dismiss` or a fresh `RaisesCeilingTo` grant clears it |
| Escalation | Reuses the existing `awaiting_decision` `UnitState` outright — it already exists for exactly "this unit needs a human before it can proceed" |

## 12. Relationship to #474 (review remediation)

Disjoint by construction, not by convention: #474 triggers on a *published*
PR's live `REQUEST_CHANGES` Decision bound to that PR's current `HeadSHA`
— strictly post-publication, strictly outside WorkGraph/#473. This ADR's
mechanism triggers on a pre-publication #473 `Finding` and only ever
produces a *new epoch* (new `RunID`), never a same-`RunID` continuation —
the same-`RunID`-continuation shape stays #474's and #508(a)'s alone,
completely untouched by this design.

## 13. Relationship to #508 (decision authority)

Fully specified in §6. In summary: no new authority *kind*
(`AuthorityKindOperator`/`DecisionResolutionAuthority`/`ResolveDecision`
reused exactly as-is); one new `DecisionRequest.Purpose` value
(`epoch_authorization`) with an explicit, typed, non-free-text `EpochAsk`
sub-struct and an explicit mutual-exclusion guard against #508(a)'s
existing same-`RunID` continuation path.

## 14. Relationship to #480 (dynamic orchestration)

#480 is conceptual only today (zero linked PRs) and must not be a hard
dependency for a real, already-proven gap. This ADR's v1 cut (Phase 2,
§15) works entirely through #508-gated human decisions. #480's only
addition, if and when it ships, is a second, policy-driven path into the
*same* `remediation_authorizations` record (§5's "plan-preauthorized"
branch) — #480 never gets its own competing authorization mechanism, and
#581/this ADR never blocks on #480's timeline.

## 15. Minimal implementation sequence

| Phase | Scope | Owner | Observable change |
|---|---|---|---|
| 0 | This design + independent review | #581 | none (this document) |
| 1 | New, empty `work_graph_remediation_epochs` + `remediation_authorizations` tables (DDL only); `CurrentEpoch`/`CurrentEpochSource` projection helpers written but **not yet wired into any read path** | #581 (#472-adjacent) | **Provably none** — no existing call site changed, new tables have no rows (§3) |
| 2 | Wire `ProjectWorkGraph`'s frontier/output resolution through `CurrentEpochSource` and `DownstreamDependencySatisfaction` (§4.2); `EligibleRemediation` (§7, never refuses on ceiling alone), `remediation_authorizations` admission transaction (§8), new `DecisionRequest.Purpose = epoch_authorization` with `RaisesCeilingTo`/`dismiss` (§6), `Cause` discriminant on invalidation (§4.3), `PolicyMaxEpochs` compiled with a conservative default of `1` (no remediation) so existing plans are unaffected until explicitly raised | #581 + #508 | First phase a real unit can get epoch 2 — always human-gated: either policy already allows it (requires a plan amendment, rare in this phase) or a human's `epoch_authorization` decision explicitly sets `RaisesCeilingTo` for that one unit (§11.1) — no silent auto-authorization either way |
| 3 (later, optional) | Plan-preauthorized policy path beyond the default, once #480's plan→WorkGraph compiler exists to carry `PolicyMaxEpochs > 1` as an ordinary approved-plan obligation | #480 + #581 | Routine remediation stops needing a one-off human click (via `RaisesCeilingTo`) when a plan already covers it |
| 4 (polish) | No-progress/escalation tuning, Product Workspace (#479) surfacing of epoch/remediation state | #479 | UX only |

Phase 1 is a provably no-observable-change schema addition, reviewable and
mergeable entirely on its own, before any new authority surface exists —
consistent with "boring infrastructure until the engineering semantics are
proven."

## 16. Deterministic acceptance test plan, including negative scenarios

Reusing #475's exact fixture (`fleetFixture`/`integrationGraphUnits`/
`integrationWorker`/`blamedUnitInbox`, `runtime/workgraph_integration_test.go`).

**Positive path:**

1. Real Git conflict (today's Scenario B) → Finding delivered (today's
   #579, already proven, unchanged).
2. A `DecisionResolution` (`Purpose: epoch_authorization`) authorizes
   remediation epoch 2 for the blamed unit, citing the exact Finding id,
   with `RaisesCeilingTo: 2` (since the fixture's compiled
   `PolicyMaxEpochs` is the default `1`) — reused authority check, durable
   provenance, written to `remediation_authorizations` with
   `authorized_by = "decision"`.
3. §8's atomic admission mints the epoch-2 row and a new `RunID`; the
   existing fresh op-DAG runs; a controlled test provider writes a
   non-conflicting fix within the authorized scope; new handoff admits
   under epoch 2 as `completed_admitted`, `CurrentEffectiveOutput` moves,
   epoch 1 becomes `superseded` (derived).
4. The integrator unit — `invalidated` with `Cause: remediation_epoch_advance`
   — becomes eligible for its own epoch 2, re-runs `OpIntegrationCompose`
   against the current producer outputs, composes cleanly, fresh assurance
   passes, new integrated handoff admits.
5. Downstream independent review/assurance (#233/#474-adjacent) operates
   on the final epoch-2-sourced integrated candidate only — reusing
   existing exact-subject review-binding tests unchanged.

**Negative/boundary scenarios, each a distinct deterministic test:**

| Scenario | Setup | Required assertion |
|---|---|---|
| **Migration/restart on an untouched graph** | A graph with zero remediation activity, genuinely closed/reopened SQLite store | Every existing projection, frontier and output fact is byte-identical pre/post-restart; `work_graph_remediation_epochs`/`remediation_authorizations` remain empty; existing `TestWorkGraphIntegrationRestartReproducesTheSameOutcome`-style assertions pass unmodified |
| **Concurrent admission, no orphans (P1a)** | Two reconcile passes both observe the same eligibility fact for the same unit in the same tick (simulated via two overlapping calls to §8's full transaction — epoch row + run + operations — against the same live store) | Exactly one epoch-2 row, one run, and one operation set exist afterward; the losing transaction's step-1 insert affects zero rows and it never reaches step 2 — assert directly that **zero** orphaned runs (a run row with no owning epoch row) and **zero** orphaned epochs (an epoch row whose run does not exist) exist in the store after both passes |
| **Crash between epoch-row and run creation (P1a)** | Kill the process (or inject a failure) after step 1 of §8's transaction would have committed but before step 2, then restart | Because both steps are one transaction, nothing committed: zero epoch-2 rows, zero new runs. The restarted supervisor's next `EligibleRemediation` pass computes the identical next-epoch number from the identical durable state and legitimately retries — assert no duplicate-epoch, no orphan, no stuck "provisioning" state of any kind |
| **Restart mid-epoch-2** | Genuinely close/reopen the store while epoch 2's run is non-terminal (`EpochOutcome = running`) | The resumed run continues under the *same* epoch-2 `RunID`/row (no duplicate epoch-3 row is created merely because the process restarted); eventual admission or failure of epoch 2 proceeds exactly as if no restart occurred |
| **Downstream never sees the old handoff as satisfied while remediation is outstanding (P1b)** | Producer unit's epoch 1 is live, a `DecisionRequest(epoch_authorization)` is pending for epoch 2 (not yet resolved) | The downstream integrator's own dependency-satisfaction fact for the producer is **not satisfied** (`RemediationState = pending_decision`), even though a historical admitted handoff (epoch 1) exists; the integrator is `blocked`, never `invalidated`, and never proceeds as if the producer's output were current |
| **Exhaustion never resurrects the old handoff (P1b)** | Epoch 2 fails; `EffectiveCeiling(unit) == 2` (no further grant possible); no `dismiss` | `RemediationState = exhausted_unresolved`; `DownstreamDependencySatisfaction` stays **not satisfied** indefinitely — assert explicitly that the downstream integrator does *not* fall back to treating epoch 1's (still-conflicting) handoff as current merely because epoch 2 failed and nothing more is eligible |
| **`dismiss` is the only path back (P1b)** | Same exhausted state as above, then a human resolves a fresh `epoch_authorization` decision with `Outcome: dismiss` | `RemediationState` moves to `resolved`; `DownstreamDependencySatisfaction` returns to `CurrentEffectiveOutput` (epoch 1, unchanged); the downstream integrator unblocks onto that original handoff — and only this path, never a timeout or a failed attempt by itself, produces this transition |
| **Failed remediation, retry allowed** | Epoch 2's run fails assurance; `EffectiveCeiling(unit) >= 3` | Epoch 2's `EpochOutcome` derives to `failed` (no column written, §4.1); a *new* `remediation_authorizations`/epoch-3 cycle can be legitimately proposed and authorized; epoch 2 remains a permanent, immutable, non-current row |
| **Failed remediation, limit exhausted** | Epoch 2's run fails assurance; `EffectiveCeiling(unit) == 2` | `EligibleRemediation` still *produces* an epoch-3 `RemediationRequest` if a live Finding names the unit (§7 — it no longer hard-refuses), but no authorization path can *grant* it: the `"policy"` path is capped at `PolicyMaxEpochs`, and no `remediation_authorizations` row with a sufficient `raises_ceiling_to` exists yet. Unit settles `awaiting_decision`, never silently stuck with no signal |
| **Human decision raises the ceiling for exactly one unit (P2)** | `PolicyMaxEpochs(unit) == 1` (default, no plan change); a live Finding triggers `EligibleRemediation`; the resulting `DecisionRequest` carries `EpochAsk.ExceedsEffectiveCeiling = true` | A human `approve`s with `RaisesCeilingTo: 2`; `remediation_authorizations` gets a `"decision"`-authorized row with `raises_ceiling_to = 2`; `EffectiveCeiling(unit)` becomes `2` for **this unit only** — assert a *second*, unrelated unit in the same plan/graph still has `EffectiveCeiling == 1` and the plan's compiled `PolicyMaxEpochs` obligation itself is byte-identical before and after (no contract recompilation occurred) |
| **Policy path cannot raise the ceiling (P2)** | Attempt to construct a `remediation_authorizations` row with `authorized_by = "policy"` and a non-null `raises_ceiling_to` | Refused at the schema/constructor level (the `CHECK` constraint in §5) — assert this is impossible to construct, not merely discouraged by convention |
| **`approve` without `RaisesCeilingTo` on an over-ceiling request (P2)** | `EpochAsk.ExceedsEffectiveCeiling = true`; human resolves `approve` with no `RaisesCeilingTo` field set | Resolution is refused (fail closed); no `remediation_authorizations` row is written; the human must resubmit with an explicit `RaisesCeilingTo` to grant it — never inferred from a bare `approve` |
| **Stale Finding at authorization time** | A `DecisionResolution` arrives citing a Finding that is no longer live (superseded by a newer Finding on the same subject) | `EligibleRemediation`/admission refuses the authorization; no epoch row is created; the stale decision is recorded as refused, not silently dropped |
| **Stale Finding/authorization at dispatch time** | A `remediation_authorizations` row is valid at creation, but the Finding it cites goes stale (producer's own output changes again) before the epoch's run actually starts | §9's dispatch-time re-check (`stale_finding_at_dispatch`) refuses to start the run; the run itself fails with that cause (`EpochOutcome` derives to `failed`), never silently proceeds on stale grounds |
| **Lost/never-arriving decision response** | An `epoch_authorization` `DecisionRequest` is emitted; no `DecisionResolution` ever arrives | Unit stays `awaiting_decision` indefinitely, exactly as #508's existing unresolved-decision behavior already does today — no new timeout/retry machinery invented, no silent auto-proceed |
| **Scope exceeded** | A remediation run's candidate diff touches a path outside the Finding's named scope | §9's remediation-scope assurance gate fails the run itself with cause `scope_exceeded` (`EpochOutcome` derives to `failed`); the out-of-scope change is never admitted as a handoff |
| **Cancellation** | Epoch 2's run is cancelled mid-flight via the existing run-cancellation path | Epoch 2 becomes a terminal, non-current, permanent `cancelled` row (`EpochOutcome`, derived); `CurrentEffectiveOutput` remains epoch 1's, and — per the P1b fix above — `DownstreamDependencySatisfaction` stays not-satisfied, not epoch 1 |
| **Downstream reintegration after producer remediation** | Full positive-path run (above), then a *second*, independent conflicting Finding against the *same* re-integrated candidate | The second cycle runs through the identical mechanism from its own epoch 1 (the integrator unit's own epoch 2 in the first cycle), proving the primitive is not a #475-specific one-shot but genuinely reusable |

## 17. Trade-offs and alternatives rejected

- **Widen `work_graph_activations`'s own primary key** instead of adding a
  separate, empty, additive table. Rejected after reviewer feedback: an
  `ALTER` on the existing production table's key is a real migration with
  real risk on every existing graph; a new, empty, additive table makes
  compatibility a structural fact instead of a migration to get right
  (§3).
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
- **Overload #508's `DecisionResolution.Outcome` as the authorization
  record itself**, instead of a dedicated `remediation_authorizations`
  table. Rejected after reviewer feedback (§5, §18.3 below): it would
  couple #508's existing, generic table to a #581-specific concept, and
  would have no natural representation for the plan-preauthorized path
  (which has no `DecisionResolution` at all).
- **Make #480 a hard prerequisite.** Rejected: #480 is purely conceptual
  (zero PRs); gating a real, proven gap on it would stall indefinitely.
  Policy-preauthorization is cleanly deferred to Phase 3 instead.
- **A bespoke "integration units retry forever" special case** instead of
  a general epoch primitive. Rejected: this is exactly the #475-specific
  workaround the originating task explicitly said not to build. The epoch
  primitive is deliberately general — any `WorkUnit`, any role — so #480
  and future WorkGraph-level corrective actions reuse it rather than
  needing their own mechanism later.
- **A stored, mutable `status`/outcome column on the epoch row.** Rejected
  after reviewer feedback (§4.1): directly contradicts "historical
  activation records are immutable" the instant it is updated in place.
  Deriving `EpochOutcome` from the bound run's own existing event journal
  reuses a mechanism already proven correct for epoch 1 and leaves nothing
  to transition.
- **A separate "provisioning" status plus a crash-reconciliation sweep**
  for epoch+run+operation creation. Rejected (§8): SQLite's own
  transaction durability already makes partial creation impossible; an
  extra status value and a sweep to reconcile it would be new machinery
  solving a problem one transaction boundary already prevents.
- **Let `DownstreamDependencySatisfaction` fall back to
  `CurrentEffectiveOutput` whenever remediation is not currently
  `running`** (i.e. treat only "actively executing" as the suspend
  condition, not `pending_decision`/`exhausted_unresolved` too).
  Rejected (§4.2, §10): this is exactly the bug reviewer concern P1b
  named — a failed or exhausted remediation attempt would silently make
  the original, still-disputed handoff look current again. Suspension
  must persist through every `RemediationState` except `none`/`resolved`.
- **A plan-wide (or contract-wide) ceiling raise** instead of a per-unit
  `remediation_authorizations.raises_ceiling_to` override. Rejected
  (§11.1): a plan-wide raise is a contract change with every associated
  review/reassessment cost, for a decision that is almost always about
  one specific unit's one specific Finding. A per-unit override gives the
  same genuinely-required flexibility with a minimal, exact-subject blast
  radius — consistent with "customization may specialize or narrow; it
  may not escalate" applied to the *scope* of an escalation itself.
- **Let the human-gated path simply ignore `PolicyMaxEpochs` entirely**
  (no ceiling concept for decisions at all). Rejected: would make the
  ceiling meaningless for the one path most likely to be used under
  genuine time pressure, and would leave no auditable record of *how much*
  a human's decision expanded eligibility beyond the compiled default —
  `raises_ceiling_to` exists precisely so that record is explicit and
  queryable, not merely implied by the existence of an epoch-2 row.

## 18. Resolved design decisions, confirmed for reviewer sign-off

The prior draft left three questions fully open. Each now has a
recommended, reasoned resolution in the body of this ADR; restated here
together for a reviewer's explicit accept/override:

1. **Where does `PolicyMaxEpochs` live?** Resolved (§11): compiled into
   the unit's `EngineeringContract` via the existing `EngineeringPolicy`
   compiler, not a standalone counter — reuses AGENTS.md's "one obligation
   system" rule and routes any limit change through the existing
   contract-reassessment path. *Reviewer sign-off needed:* confirm this is
   the right compiler seam (`OpContractCompile`) rather than a later
   binding point.
2. **Does downstream cascading (`Cause: remediation_epoch_advance`)
   require its own authorization at every hop, or can one upstream
   authorization imply eligibility for every transitively-affected
   downstream unit?** Resolved, conservative default: **every hop requires
   its own `remediation_authorizations` row** — a downstream integrator
   becoming `invalidated` with `Cause: remediation_epoch_advance` makes it
   *eligible* to request its own epoch (§4), it does not itself authorize
   one. The plan-preauthorized path (§5) may cover multiple hops under one
   compiled policy clause once #480 exists, but the v1/Phase-2 human-gated
   path never auto-cascades past one hop without a fresh decision.
   *Reviewer sign-off needed:* confirm this conservative default is
   correct for v1, with cascading-under-one-policy-clause explicitly
   deferred to #480/Phase 3 rather than built now.
3. **Does `RemediationAuthorization` get its own table, or reuse
   #508's `DecisionResolution`?** Resolved (§5, §17): its own dedicated
   table, citing either a `DecisionResolution.ID` or a policy-clause
   reference, so #508's existing table is never overloaded with a concept
   it did not define and the plan-preauthorized path (which has no
   `DecisionResolution` at all) has a natural representation.
   *Reviewer sign-off needed:* confirm #508's owner is comfortable with a
   new table citing their table by foreign key, rather than any schema
   change on their side (there is none).

## 19. Second-pass correctness fixes, confirmed for reviewer sign-off (P1/P2)

Three focused correctness concerns from the first review pass, each fixed
in the body of this ADR; restated here together for explicit sign-off:

1. **(P1a) §8's admission atomicity.** Fixed: the epoch row, the new
   `EngineeringRun` row, and its initial `run_operations` are written in
   one SQLite transaction (§8), not a single statement as pass 1 implied.
   No stored "provisioning" status exists or is needed — transaction
   atomicity alone makes partial creation impossible, and a losing
   concurrent pass must check `rows-affected` after step 1 and never
   proceed to step 2 (the explicit conflict-handling rule pass 1 omitted).
   The epoch row itself lost its `status` column entirely (§4.1) —
   `EpochOutcome` is now always derived from the bound run's own
   already-immutable event journal, so "stored status transitions" no
   longer exist to be inconsistent with anything.
   *Reviewer sign-off needed:* confirm deriving `EpochOutcome` from the
   run's existing lifecycle (rather than a parallel stored fact) is
   acceptable to whoever owns the run/event-journal schema, since it
   means #581 code reads that schema rather than owning a copy of it.
2. **(P1b) Effective-output semantics.** Fixed: `CurrentEffectiveOutput`
   (historical: last successful handoff) is now explicitly distinct from
   `DownstreamDependencySatisfaction` (what a dependent may actually read),
   gated by a new, fully derived `RemediationState` (§4.2). A failed or
   exhausted remediation attempt never makes the original handoff
   "newly usable" — suspension persists through `pending_decision`,
   `running`, and `exhausted_unresolved`, and clears only via a genuine
   new `completed_admitted` epoch or an explicit human `dismiss` (§4.3).
   Downstream digest computation (§10) now reads
   `DownstreamDependencySatisfaction`, never `CurrentEffectiveOutput`
   directly.
   *Reviewer sign-off needed:* confirm the conservative choice to treat
   a downstream dependent as merely `blocked` (not `invalidated`) while
   an upstream unit's remediation is outstanding is the right default,
   versus some more eager alternative that was not proposed here because
   it risked exactly the staleness bug this fix closes.
3. **(P2) Phase 2 eligibility under the default ceiling.** Fixed:
   split into `PolicyMaxEpochs` (compiled, plan-controlled, default `1`,
   never touched by a human decision) and a per-unit, per-decision
   `raises_ceiling_to` override (§11.1) that only an explicit, human
   `approve + RaisesCeilingTo` can write — structurally unreachable from
   the plan-preauthorized path (§5's `CHECK` constraint).
   `EligibleRemediation` (§7) no longer refuses to even ask when the
   proposed epoch exceeds the current ceiling; only the *grant* remains
   gated. This is the precise, non-implicit mechanism by which a human
   decision can authorize epoch 2 for a plan compiled at the conservative
   default, without ever recompiling that plan's contract or affecting
   any other unit.
   *Reviewer sign-off needed:* confirm a per-unit override table, read via
   `EffectiveCeiling = max(compiled, overrides)`, is preferred over
   requiring every such case to go through a full plan amendment instead
   — the latter is safer in the sense of leaving zero new schema, but
   reintroduces the "can a human ever be asked" contradiction this fix
   exists to resolve.
