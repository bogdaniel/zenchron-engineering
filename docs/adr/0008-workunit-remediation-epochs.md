# ADR-0008: Bounded, Governed Remediation Epochs for an Already-Admitted WorkUnit

- Status: Proposed (design + independent review only — not implemented)
- Date: 2026-10-12 (revised same day: resolved reviewer concerns on
  migration, #508 extension, epoch state separation, concurrency,
  execution scope, budget semantics, and the three prior open questions)
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
  batch_id          TEXT NOT NULL,
  run_id            TEXT NOT NULL,
  inputs_digest     TEXT NOT NULL,
  authorization_id  TEXT NOT NULL,      -- FK, see §5; never null here
  activated_at      TEXT NOT NULL,
  status            TEXT NOT NULL,      -- see §4's epoch_status enum
  PRIMARY KEY (graph_id, unit_id, epoch)
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

Four previously conflated concepts, now separated:

| Concept | Definition | Computed from |
|---|---|---|
| **LatestAttemptEpoch** | The highest epoch number with *any* row (running, completed, failed, or cancelled) | `MAX(epoch)` across `work_graph_activations` (epoch 1) ∪ `work_graph_remediation_epochs` |
| **LatestAdmittedHandoffEpoch** | The highest epoch whose run produced an admitted handoff (`Output != nil`) | Same union, filtered to `status = completed_admitted` |
| **CurrentEffectiveOutput** | The `Output` downstream dependency satisfaction actually reads | The handoff belonging to `LatestAdmittedHandoffEpoch` — **not** `LatestAttemptEpoch`. A running or failed epoch N+1 never blanks out epoch N's still-good output. |
| **DownstreamEligibility** | Whether a dependent unit is `invalidated`/eligible for its own epoch | Recomputed whenever `CurrentEffectiveOutput` changes (§8) — never on every attempt, only on a *successful* new admission |

Per-epoch `status` (stored, not derived):

| Status | Meaning | Terminal? | Can a new epoch be proposed while this is current? |
|---|---|---|---|
| `running` | Run in flight | No | No — `EligibleRemediation` (§6) requires the current epoch terminal |
| `completed_admitted` | Run finished, produced an admitted `Output` | Yes | Yes (this is what makes an epoch "superseded" the instant a later one reaches this status) |
| `completed_no_output` | Run finished (e.g. conflict still present) but produced no admitted `Output` | Yes | Yes |
| `failed` | Run's own assurance/execution failed | Yes | Yes, subject to `PolicyMaxEpochs` (§11) |
| `cancelled` | Operator/timeout cancelled the run | Yes | Yes, subject to `PolicyMaxEpochs` |

**`superseded`** is never a stored column (the prior draft's mistake): it
is a *derived* fact, true for epoch N iff any epoch `> N` has reached
`completed_admitted`. An epoch row is immutable the instant it is written;
"superseded" is a statement about what exists *elsewhere*, not about that
row.

`UnitState` projection (gap #1) changes in exactly one place: frontier
membership (gap #3) becomes `!Activated(unit) OR EligibleForNextEpoch(unit)`
(§6). The unit's reported state is still its **current epoch's** child
`ItemState` — i.e. `CurrentEpochSource`'s own run, not necessarily
`LatestAttemptEpoch`'s run if a later attempt failed and an even-later one
is what's authorized next (there is no such case today, since
`EligibleRemediation` always proposes `LatestAttemptEpoch + 1`, but the
distinction matters for the restart/replay scenarios in §16).

Invalidation (gap #4/#5) gains a `Cause` discriminant:

- `external_replacement` — today's only case. Stays exactly as it is:
  terminal, no re-run. An operator or tool legitimately superseding a
  unit's output outside this mechanism must never auto-cascade.
- `remediation_epoch_advance` — new, fires only when the digest changed
  because an upstream unit's `CurrentEffectiveOutput` advanced to a new
  epoch's handoff. The downstream unit becomes eligible for *its own* next
  epoch (§6), not permanently invalidated. This is the one behavior change
  to existing invalidation semantics in this ADR, gated by the same
  `RemediationAuthorization` chain (§5) as every other epoch advance — not
  automatic cascading, "also eligible to request its own governed epoch."

## 5. Authorization

A Finding never authorizes anything by itself (never-cut: "no invented
producer activation"). A single, dedicated, #581-owned table —
**not** a #508 `DecisionResolution` variant (resolves the prior draft's
open question 3; see §17.3) — is the one record either path produces:

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
  authorized_at      TEXT NOT NULL
);
```

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

`DecisionResolution.Outcome` for this purpose is one of: `approve`
(binds `remediation_authorizations.decision_resolution_id`, exact
`proposed_epoch` and `finding_id` preserved verbatim from the request —
no silent substitution), `reject` (no row written; the unit stays at its
current epoch, no further `EligibleRemediation` is auto-retried for the
*same* Finding), or `escalate` (no-op for this mechanism; reserved for a
human routing the decision elsewhere, outside this ADR's scope).

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
  require: unit's CurrentEpoch's status is terminal       (completed_admitted,
           (never running)                                completed_no_output, failed,
                                                            or cancelled — §4)
  require: no existing remediation_authorizations row already covers
           (finding.ID, unit, nextEpoch)                 (idempotent — deterministic id,
                                                            same pattern as DeliveredFindingID)
  require: nextEpoch <= PolicyMaxEpochs(unit)             (§11, resolved)
  → RemediationRequest{finding.ID, unit, proposedEpoch: LatestAttemptEpoch(unit)+1}
```

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

**Atomic, idempotent, crash-safe admission**, in one statement, mirroring
the exact same-statement atomic-predicate technique #475/#579's own N1
review history already established for `AdmitIntegratedHandoff`:

```sql
INSERT INTO work_graph_remediation_epochs
  (graph_id, unit_id, epoch, batch_id, run_id, inputs_digest, authorization_id, activated_at, status)
SELECT
  :graph_id, :unit_id,
  (SELECT COALESCE(MAX(epoch), 1) + 1 FROM work_graph_remediation_epochs
   WHERE graph_id = :graph_id AND unit_id = :unit_id),
  :batch_id, :run_id, :inputs_digest, ra.id, :now, 'running'
FROM remediation_authorizations ra
WHERE ra.graph_id = :graph_id AND ra.unit_id = :unit_id
  AND ra.proposed_epoch = (SELECT COALESCE(MAX(epoch), 1) + 1 FROM work_graph_remediation_epochs
                            WHERE graph_id = :graph_id AND unit_id = :unit_id)
  AND NOT EXISTS (
    SELECT 1 FROM work_graph_remediation_epochs e2
    WHERE e2.graph_id = :graph_id AND e2.unit_id = :unit_id AND e2.status = 'running'
  );
-- PRIMARY KEY (graph_id, unit_id, epoch) makes a second concurrent writer's
-- identical INSERT a no-op (ON CONFLICT DO NOTHING semantics), never a second row.
```

- **Atomic:** the next-epoch number, the authorization existence check,
  and the "no epoch currently running" guard are all evaluated inside the
  *same* `SELECT`, in the *same* transaction as the `INSERT` — no
  read-then-decide-then-write gap for a second reconciliation pass to land
  in.
- **Idempotent:** two concurrent reconcile passes compute the identical
  next-epoch number from the identical durable rows; the `PRIMARY KEY`
  lets only one `INSERT` actually land. A pass that loses the race simply
  observes the row the other pass wrote on its next read — no error
  path, no retry logic beyond "read again next tick," identical to how
  `ActivateWorkUnit`'s existing `ON CONFLICT DO NOTHING` already behaves
  today for first activation.
- **Crash-safe:** either the transaction commits (the epoch row exists,
  durable) or it does not (nothing exists, the next attempt recomputes the
  identical next-epoch number from the identical durable state) — no
  partial row, no orphaned `RunID` with no epoch row, by the same
  single-statement-commit discipline #475's own handoff admission already
  uses.
- **Resistant to concurrent reconciliation:** the `NOT EXISTS (... status
  = 'running')` clause means a second reconcile pass that observes the
  same eligibility fact cannot mint a *second* running epoch for the same
  unit even if it computes the same next-epoch number from a slightly
  stale read — the guard is evaluated fresh, inside the same statement,
  against the live table at commit time, under SQLite's own serialized-
  writer transaction isolation (the same isolation this runtime's existing
  single-writer SQLite model already relies on everywhere else; this ADR
  introduces no new concurrency primitive).
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
| **Allowed changes** | Bounded to the Finding's own named paths, enforced — not merely hinted to the provider — by a remediation-scope assurance gate (reusing the existing `Conflict.Paths`/`VerifyRemediationScope` bounded-scope concept from #475's own PR #541/#546 review history) that runs as part of the new epoch's own assurance phase and fails the epoch (`status = failed`, cause `scope_exceeded`) if the candidate's diff touches anything outside that set | A provider's good-faith attempt is not trusted as the enforcement boundary; the existing scope-check primitive already proven in #475's history is reused as the actual gate |
| **Context given to the provider** | The base candidate, the exact triggering Finding (surfaced prominently), and the permitted scope — via the existing `communicationContext` renderer #579 already extended, no new renderer | Reuse, not a parallel context mechanism |

## 10. Supersession

- The instant epoch N's handoff reaches `completed_admitted` (§4), epoch
  N-1 becomes `superseded` (a derived fact, never stored) and
  `CurrentEffectiveOutput` moves to epoch N's handoff. This is *exactly*
  today's existing "latest admitted output wins" fact, already consumed by
  downstream digest recomputation — no new primitive, now that §8's
  atomic admission removes the "producer run is already terminal"
  blocker.
- A downstream integration unit that consumed epoch N-1's stale output was
  already (correctly) flagged `invalidated` by today's unchanged mechanism.
  §4's `Cause: remediation_epoch_advance` is what lets it request its own
  next epoch instead of staying permanently invalidated — gated by the
  same `remediation_authorizations` chain (§5), so cascading reintegration
  is never automatic, only ever newly eligible (see §17.2 for the
  resolved default on how eligible).
- Re-integration reuses #475's `OpIntegrationCompose` and conflict/success
  classification completely unchanged: current `WorkUnitInputs` now
  resolve to epoch N's producer `CurrentEffectiveOutput` plus the
  unaffected other producer's own current output, Git-composes, fresh
  assurance gates it exactly as today
  (`TestWorkGraphIntegrationHandoffWaitsForRequiredSemanticAssurance`,
  unchanged).
- A stale Finding or `remediation_authorizations` row bound to an epoch
  that has since been superseded is refused by the exact `AppliesTo`
  staleness check #579 already proved (reused, not reinvented) — see the
  explicit `stale_finding_at_dispatch`/`stale_authorization` negative
  scenarios in §16.

## 11. Boundedness and budget (resolves reviewer concern 6)

**Resolved: `PolicyMaxEpochs` counts total epochs, including the
original.** `PolicyMaxEpochs(unit) = 1` means "no remediation ever" — the
default, and today's actual behavior for every existing unit, since the
new table is empty and `EligibleRemediation` requires `nextEpoch <=
PolicyMaxEpochs`. `PolicyMaxEpochs(unit) = 2` means exactly one
remediation attempt is ever authorized. This reads naturally as "max
attempts," consistent with the existing run-scoped `AttemptLimit`
convention (`execution_continuation_budget.go`), and avoids an off-by-one
ambiguity between "the limit" and "the limit on top of the original."

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
| Retry limit | `PolicyMaxEpochs(unit)`, checked in `EligibleRemediation` (§7), compiled as a contract obligation (above) |
| No-progress detection | Reuse #475's own park-on-identical-attempt pattern (`ReasonDeterministicFailureUnchanged`) directly: an epoch producing the same digest as its predecessor parks (`status = failed`, cause `no_progress`) and does **not** auto-propose the next epoch via the plan-preauthorized path even if the policy would otherwise allow it — it escalates to a mandatory human `DecisionRequest` (§6) regardless of authorization path |
| Duplicate reconciliation | Deterministic id = pure function of `(Finding.ID, unit, proposedEpoch)`, insert-only — identical pattern to `DeliveredFindingID`; naturally idempotent across reconcile passes (§8) |
| Failed remediation | Epoch N's run fails its own assurance exactly like any run's existing failure classification (no new failure class); `PolicyMaxEpochs` then decides whether epoch N+1 is still eligible or this escalates |
| Cancellation | Unchanged existing run-cancellation path; a cancelled epoch becomes a terminal, non-current, permanent row — never re-admitted as current |
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
| 2 | Wire `ProjectWorkGraph`'s frontier/output resolution through `CurrentEpochSource`; `EligibleRemediation`, `remediation_authorizations` admission (§8's atomic SQL), new `DecisionRequest.Purpose = epoch_authorization` (§6), `Cause` discriminant on invalidation (§4), `PolicyMaxEpochs` compiled with a conservative default of `1` (no remediation) so existing plans are unaffected until explicitly raised | #581 + #508 | First phase a real unit can get epoch 2 — always human-gated (`PolicyMaxEpochs` default is 1; raising it is a plan-approval-time, human act), no silent auto-authorization |
| 3 (later, optional) | Plan-preauthorized policy path beyond the default, once #480's plan→WorkGraph compiler exists to carry `PolicyMaxEpochs > 1` as an ordinary approved-plan obligation | #480 + #581 | Routine remediation stops needing a one-off human click when a plan already covers it |
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
   remediation epoch 2 for the blamed unit, citing the exact Finding id —
   reused authority check, durable provenance, written to
   `remediation_authorizations`.
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
| **Concurrent admission** | Two reconcile passes both observe the same eligibility fact for the same unit in the same tick (simulated via two overlapping calls to §8's admission SQL against the same live store) | Exactly one epoch-2 row exists afterward; the losing pass's statement affects zero rows (no error, no second `RunID`, no duplicate `remediation_authorizations` consumption) |
| **Restart mid-epoch-2** | Genuinely close/reopen the store while epoch 2's run is `running`, not yet terminal | The resumed run continues under the *same* epoch-2 `RunID`/row (no duplicate epoch-3 row is created merely because the process restarted); eventual admission or failure of epoch 2 proceeds exactly as if no restart occurred |
| **Failed remediation, retry allowed** | Epoch 2's run fails assurance; `PolicyMaxEpochs(unit) >= 3` | Epoch 2 row becomes `failed`; a *new* `remediation_authorizations`/epoch-3 cycle can be legitimately proposed and authorized; epoch 2 remains a permanent, immutable, non-current row |
| **Failed remediation, limit exhausted** | Epoch 2's run fails assurance; `PolicyMaxEpochs(unit) == 2` | `EligibleRemediation` refuses to produce an epoch-3 `RemediationRequest`; unit settles `awaiting_decision` (escalation), never silently stuck with no signal |
| **Stale Finding at authorization time** | A `DecisionResolution` arrives citing a Finding that is no longer live (superseded by a newer Finding on the same subject) | `EligibleRemediation`/admission refuses the authorization; no epoch row is created; the stale decision is recorded as refused, not silently dropped |
| **Stale Finding/authorization at dispatch time** | A `remediation_authorizations` row is valid at creation, but the Finding it cites goes stale (producer's own output changes again) before the epoch's run actually starts | §9's dispatch-time re-check (`stale_finding_at_dispatch`) refuses to start the run; epoch row is written as `failed` with that cause, never silently proceeds on stale grounds |
| **Lost/never-arriving decision response** | An `epoch_authorization` `DecisionRequest` is emitted; no `DecisionResolution` ever arrives | Unit stays `awaiting_decision` indefinitely, exactly as #508's existing unresolved-decision behavior already does today — no new timeout/retry machinery invented, no silent auto-proceed |
| **Scope exceeded** | A remediation run's candidate diff touches a path outside the Finding's named scope | §9's remediation-scope assurance gate fails the epoch (`status = failed`, cause `scope_exceeded`); the out-of-scope change is never admitted as a handoff |
| **Cancellation** | Epoch 2's run is cancelled mid-flight via the existing run-cancellation path | Epoch 2 becomes a terminal, non-current, permanent `cancelled` row; `CurrentEffectiveOutput` remains epoch 1's (cancellation never promotes a non-admitting epoch to current) |
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
  table. Rejected after reviewer feedback (§5, §17.3 below): it would
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
