# ADR-0003: Configuration Authority Classification and Freeze Points

- Status: Proposed
- Date: 2026-09-28
- Issue: #329 (Phase A). Inputs: #58, #131, #307, #89, #328, #83, #84, #327.

## Context

Every member of the operator configuration, and every member of the in-repo
tighten-only layer, is inside one digest. That digest is inside the controller
binding, the run identity and the plan identity. So changing any value, even a
default that only decides how much time future runs get, is a controller
identity change. #324 recovery showed the cost. Moving `budgets.wall_limit_seconds`
from 1800 to 5400 needed #307's cold `controller re-adopt`, and re-adopt
correctly refused while one run from the old configuration was still alive.

The refusal was correct. The problem is that the classification is too coarse.
This ADR inventories every field that currently contributes to the
controller-effective digest. For each field it makes two separate decisions:
where the value freezes, and which identities or digests it takes part in. It
also records the migration law and splits Phase B into separate pieces.

This ADR changes no behaviour. `runtime/config_classification_test.go` fails
when a configuration field that `encoding/json` would serialize is not named in
the §3 inventory tables.

## How the digest is built today (evidence)

- The operator layer is digested whole. `LoadOperatorConfig` resolves budgets and
  then calls `Digest(operator)` over the whole `OperatorConfig`
  (`runtime/config.go:745-746`). The repository layer is digested whole too
  (`runtime/config.go:786`). Together they form `ConfigDigest{Global, Repository}`
  (`runtime/config.go:701,722`; type at `runtime/controller.go:132-135`).
- The repository layer comes from `.zenchron.json` under the invoking process's
  working directory (`LoadConfig(flags.Config, cwd)`, e.g.
  `cmd/zenchron-engineering/autonomy.go:614`).
- `ConfigDigest` takes part in:
  1. **Controller binding.** `ControllerBinding{Controller, Build, Config}`
     (`runtime/controller_succession.go:62-67`) is digested into
     `run.ControllerSHA256` (`runtime/controller.go:418`). Ordinary succession
     requires an identical `Config` (`runtime/controller_succession.go:291-299`).
     A mismatch parks the run as `controller_changed`
     (`runtime/reconciler.go:215,799`). Re-adoption is blocked while an
     incompatible run is nonterminal (`runtime/controller_readopt.go:296-305`).
  2. **Run identity.** `derivedRunID` hashes `{repository, issue, config,
     generation, plan_stage}` (`runtime/controller.go:546-557`). Watch probes
     only the current digest's identity space (`runtime/watch.go:503-530`). That
     is the #58 defect, and it is still open.
  3. **Plan identity.** `PlanID` hashes `{repository, issue, config}`
     (`runtime/plan_intent.go:141-145`). That is #131.
  4. **Authority request identity**, through `ControllerSHA256`
     (`runtime/authority_request.go:251`).
  5. **Other `ControllerSHA256` / controller-digest consumers.** Any change to
     the digest's composition moves each of these:
     - evidence producer revision `Producer: {ControllerID, r.controller}`
       (`runtime/operations.go:1539,1678`). This is part of the evidence identity;
     - PR body controller line (`runtime/operations.go:2022`);
     - adoption guard `refuseUnlessSucceeded` and `AdoptedFrom`
       (`runtime/controller.go:711,1268`);
     - handoff recovery owner `MayRecover` (`runtime/controller_handoff.go:401`,
       `runtime/controller_choreography.go:109,192`);
     - the `runs.controller_sha256` column (`runtime/source_claim.go:52`,
       `runtime/journal.go:75`);
     - plan-cancel scheduler owner (`runtime/plan_reconciler.go:444`);
     - re-adopt binding recompute (`cmd/zenchron-engineering/controller_succession.go:390`);
     - plan-stage run identity `derivedRunID(..., ConfigDigest, ...)`
       (`runtime/controller.go:1224`).
  6. **Recorded, not identity.** `AgentHandoffRecord.Config`
     (`runtime/agent_binding.go:112,331`) and the status `ControllerIdentity.ConfigDigest`
     (`runtime/controller.go:1077`). Status reports the current process's
     digest, not the digest the run was created under.
- Only the policy, project-model and planning-directory **paths** are digested.
  Their contents are not. Policy content is identified separately through the
  contract's `policy` object revision (`domain/types.go:217`). Planning artifacts
  are identified by per-profile and per-pack digests
  (`runtime/plan_reconciler.go:835-842`).

The rest of this document uses **D** for this whole set of digest participations
as it exists today.

## Decision

### 1. Five categories

| Category | Meaning | Freeze point | Identity it owns | Change semantics |
| --- | --- | --- | --- | --- |
| **C: Controller identity / authority** | Changes what the controller may trust, own, observe as authoritative, credential, publish as, or mutate, or who is recorded as authorizing. | Controller start (process composition) | Controller-effective digest → `ControllerBinding.Config`; also run ID and PlanID (kept, per #58) | Unchanged: identical config for succession, otherwise #307 cold re-adopt. |
| **S: Supervisor operating policy** | Governs the long-running supervisor (admission rate, concurrency, local resource ceilings, retention, unattended eligibility). It never governs the content or authority of a run. | Supervisor start | New **SupervisorPolicyDigest**, recorded durably once per supervisor start | Takes effect at the next start. No hot reload. Never parks or rewrites a run. |
| **P: EngineeringPlan policy defaults** | The aggregate ceiling for a plan. | Plan revision proposal | Existing plan revision content digest (`domain/plan.go:1025`), approved by the operator | A new revision re-resolves against the current ceiling and needs a fresh approval. Consumed budget never resets. |
| **R: EngineeringRun policy defaults** | Budgets and ceilings for one run. | Run creation | New **RunPolicyDigest** over the frozen `RunPolicy`, recorded in the run's genesis event | Only new runs receive the new value. A live run keeps its frozen value exactly. Restart re-reads the frozen value and never the current config. |
| **A: Physical-attempt / provider invocation policy** | The exact form of one provider invocation (posture, environment, per-attempt bounds). | Two stages. The ceiling or posture freezes at run creation or agent assignment (R or #84). The effective value freezes at attempt start. | The run-level ceiling goes in RunPolicyDigest or the #84 posture fingerprint. The effective per-attempt value goes in durable attempt provenance (#327). | A retry or restart cannot pick up a wider value. A material posture change is drift (#89), not a silent continuation. |

**Freeze point and identity are separate decisions.** Freezing a value into a
run contract does not put it into the run ID, the PlanID, the controller binding
or source identity. Under this ADR, only category C takes part in run ID and
PlanID. R, A, S and P values each take part only in their own policy digest.
This is the #329 law applied to #58 and #131. A mutable default is never both
something a live run is supposed to survive and a source of a fresh identity
that renews budgets or bypasses lineage.

### 2. Mandatory migration law

A field may leave the controller-effective digest only when **all** of these
hold:

1. its destination contract (RunPolicy, attempt provenance, SupervisorPolicy, or
   the plan revision) exists;
2. the exact effective value is frozen there;
3. that contract carries a stable digest that is enough to say later which
   policy governed the run, attempt or supervisor generation;
4. journal, status and provenance reference that digest, and do not
   reconstruct it from today's file;
5. the legacy interpretation for records that have no such digest is defined
   (see §5);
6. every reader of the value reads the frozen value. No reader may take
   `min(live, frozen)` and no reader may read the live value.

Condition 6 is about correctness, not only provenance. Several run budgets are
already persisted but are still read live or as `min(live, persisted)`. See §6,
contradictions 1 and 2. Today that is harmless only because every budget change
is also a controller change. If a field moved out of the digest first, a config
edit would mutate live runs. `lifecycle_deadline_seconds`,
`max_remediation_attempts` and `max_assurance_attempts` could even be **widened**.

Also:

- Mutable configuration never silently mutates an existing run, and a lowered
  default does not tighten one either. A frozen value is exact. An operator who
  wants a live run stopped cancels it.
- A restart never renews a budget, re-resolves a frozen value, or widens a run.
- Repository tightening is resolved at config load, before the run freezes its
  policy. It is recorded as part of the frozen RunPolicy, and together with the
  repository digest it feeds the RunPolicyDigest. Re-resolving on a later load
  cannot replace a frozen value, in either direction.
- #307 stays the fail-closed path for every field still classified C.
- A field that is **not yet** migrated stays controller-effective. Its target
  category below is a plan, not current behaviour.

### 3. Field inventory

In the table, "Today" is **D** for every row: controller binding, run ID, PlanID
and authority request ID, as listed above. "Target" is the identity set once the
row's Phase B migration has landed. "Default change" answers two questions. (a)
Does an operator changing the value affect existing work? (b) Does a binary that
changes the shipped default affect existing work? Line references are to the
field declaration.

#### Operator layer (`OperatorConfig`, `runtime/config.go:528-601`)

| Field(s) | Governs | Target category | Freeze (target) | Target identity | Default change affects existing work? | Historical migration | Rationale |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `state_dir` (config.go:529) | Which durable store, journal, locks and authority records this controller owns | C | controller start | controller-effective digest, run ID, PlanID | Yes. A different store is a different controller world. No default exists (required). | None. Stays C. | Defines what the controller owns and mutates. #307 is the correct boundary. |
| `project_model_path`, `policy_path` (config.go:530-531) | Which governance inputs compile into contracts and obligations | C | controller start | controller-effective digest, run ID, PlanID. Content is identified separately by the contract `policy` revision. | Yes, for new contracts. Existing contracts are frozen by revision. No default. | None. Stays C. | Governance inputs are authority (core invariant: facts → policy → obligations). Open question 3 covers path versus content. |
| `planning_dir` (config.go:544) | Where operator-defined profiles, instruction packs, context policies and templates live | C | controller start | as above. Artifact content has its own digests. | New plans only. Plan revisions pin profile and pack digests. | None. Stays C. | Names the source of operator customization authority. The config.go:535-543 comment already applies this reasoning to instruction text. |
| `assurance.image`, `assurance.docker_host`, `assurance.dependency_cache_dir` (config.go:107-109) | The program and daemon that decide whether a candidate passed, plus the verifier's dependency environment | C | controller start | controller-effective digest, run ID, PlanID | Yes. Changing the verifier changes what counts as evidence. | None. Stays C. | Acceptance authority. `dependency_cache_dir` stays C until verifier provenance records it, which is open question 4. |
| `toolchain.path`, `toolchain.required_tools` (config.go:129-130) | The brokered PATH and the capability ceiling handed to a worker | A (ceiling frozen at run creation, effective environment recorded per attempt) | run creation (ceiling) and attempt start (effective) | RunPolicyDigest (ceiling) and attempt provenance (#327) | Today (a) is yes, through `controller_changed`. After migration, the new value reaches new runs only. A widened toolchain for a live run is drift. | Legacy runs: the toolchain is the one identified by their controller binding's config digest, which is not reconstructable from the file. Continuing them requires the old digest or settlement. | A declared tool is a capability (config.go:558-563). Widening capability for a live run is a privilege increase, so it cannot be picked up silently. **Stays C until #327 and B5.** |
| `provider.kind`, `provider.auth_mode`, `provider.credential_path`, `provider.endpoint` (config.go:157-161) | The legacy single worker's identity, credential and remote endpoint | C | controller start, and agent identity at run assignment | controller-effective digest, run ID, PlanID | Yes | None. Stays C. | Credential and endpoint authority (config.go:152-155). |
| `provider.model` (config.go:158) | The legacy worker's model | A | agent assignment (#84 posture) and attempt start | #84 posture fingerprint and attempt provenance | After migration, new runs only. A changed model for a live run is #84 drift. | As `toolchain`. | Execution posture, not authority. **Stays C until B5.** |
| `agents.<id>.kind`, `agents.<id>.trust_mode` (agents.go:121-122) | Which workers exist and how much execution trust each holds | C | controller start, and frozen per run in `run.agent_assigned` | controller-effective digest, run ID, PlanID | Yes | None. Stays C. | Trust mode is execution-trust authority. Agent id, provider kind and trust mode are three facts (AGENTS.md). |
| `agents.<id>.credential_path`, `agents.<id>.endpoint` (agents.go:145,147) | A Zenchron-held credential and its remote endpoint | C | controller start | as above | Yes | None. Stays C. | Credential authority. |
| `agents.<id>.allow_permission_bypass` (agents.go:154) | Standing permission for unsafe bypass invocation | C | controller start | as above | Yes | None. Stays C. | Privilege. Moving it would allow silent escalation on a live run, which ADR-0001 prohibits. |
| `agents.<id>.command`, `agents.<id>.home`, `agents.<id>.model` (agents.go:127,131,139) | The executable, provider profile and model of a worker | A | agent assignment (#84 posture) and attempt start | #84 posture fingerprint and attempt provenance (#327) | After migration, new runs only. A change for a live run → `waiting_configuration_drift` (#89). | As `toolchain`. | Execution posture (#84). **Stays C until #84, #327 and B5.** |
| `agents.<id>.unattended` (agents.go:160) | Whether supervisor discovery may start work with this agent | S | supervisor start | SupervisorPolicyDigest | No. It gates creation only, and never an existing run. | Pre-migration supervisor starts carry no policy digest. Their policy is identified by the controller binding. | Scheduling eligibility, not trust. Explicit work is unaffected. Provisional: B4 must re-justify it, because it gates autonomous starts under the agent's account. |
| `default_agent` (config.go:580) | Which agent a new run is assigned when none is named | R | run creation (frozen as `run.agent_assigned`) | Existing `run.agent_assigned` event. Also RunPolicyDigest. | No. The assignment is already durable per run. | Runs without `agent_assigned` keep the documented legacy meaning (runtime.go:298-302). | Already self-identifying at its freeze point. This is the first field eligible to move, in B3. |
| `github.credential_mode`, `github.token_path`, `github.app_id`, `github.installation_id`, `github.private_key_path`, `github.endpoint`, `github.governance_credential_mode` (config.go:167-237) | Publication identity, forge endpoint, trust-root observation identity | C | controller start | controller-effective digest, run ID, PlanID | Yes | None. Stays C. | Who the runtime publishes as and what it treats as authoritative forge state. |
| `feedback.min_permission`, `feedback.allowed_bots`, `feedback.self_logins` (config.go:360-367) | Who may direct a worker through model-visible feedback | C | controller start | as above | Yes. Widening admission on a live run widens who can steer it. | None. Stays C. | Authority over model input (config.go:345-349). |
| `storage.max_state_bytes` (config.go:342) | Allocation ceiling on local state | S | supervisor start | SupervisorPolicyDigest | Only as an operational wait before a new allocation. It never widens authority. | As `agents.<id>.unattended`. | A local resource ceiling. It says nothing about what a run may do. |
| `supervisor.max_concurrent_runs`, `supervisor.poll_interval_seconds` (config.go:260-263) | Supervisor concurrency and poll cadence | S | supervisor start | SupervisorPolicyDigest. The plan clamp is frozen into the plan revision (P). | No. Scheduling only. The scheduler still enforces the ceiling durably. | As above. | This is the runtime's own operating bound (config.go:247-258). |
| `plan.max_child_runs`, `plan.max_concurrency`, `plan.max_provider_invocations`, `plan.max_wall_seconds`, `plan.max_cost_micros` (config.go:275-288) | Aggregate plan ceiling | P | plan revision proposal (`plan.BudgetEnvelope`, `runtime/plan_service.go:245`) | Plan revision content digest (already exists) | No for approved revisions. A new revision re-resolves and needs re-approval. Consumed budget never resets. | Existing revisions already carry `budget_envelope`. | Already frozen and self-identifying. Moving it out of PlanID removes one #131 fork source. Moves in B3. |
| `budgets.wall_limit_seconds` (config.go:390) | **Cumulative run active-work budget** (#83). Today it is also, implicitly, the physical-attempt deadline. | R | run creation | RunPolicyDigest | After migration, no. See §4. | See §4 and §5. | See §4. |
| `budgets.lifecycle_deadline_seconds` (config.go:399) | Total calendar bound on a run | R | run creation | RunPolicyDigest | Today it is persisted but **read live** (reconciler.go:796). Target: no. | Runs with `run.Budgets` keep their persisted value. Runs with nil `run.Budgets` take the value their controller binding identifies. Under an identical digest, which is the only binding that reconciles them, that is the configured value. It is **not** "no deadline": absent would widen a run that was always judged by the configured deadline. See §5. | A per-run bound. Reading it live would let a config edit widen or shorten a live run. |
| `budgets.max_execution_attempts` (config.go:400) | Same-binding retry budget (#54) | R | run creation. `operation.MaxAttempts` is frozen when the operation is planned (reconciler.go:1575). | RunPolicyDigest | Today `min(live, persisted)` (reconciler.go:923). Target: frozen exact. | Persisted value, else the legacy value identified by the controller binding. | A per-run retry authority. |
| `budgets.max_execution_continuations` (config.go:418) | Continuation-binding budget (#54) | R | run creation | RunPolicyDigest | No. It is already frozen exactly (reconciler.go:1108-1119). | Already defined: nil → the persisted `max_execution_attempts`, else the configured value (reconciler.go:1100-1107). | Already meets condition 6. |
| `budgets.max_remediation_attempts`, `budgets.max_assurance_attempts` (config.go:419-420) | Remediation and assurance retry budgets | R | run creation | RunPolicyDigest | Today persisted but **read live** (reconciler.go:944-953). Target: no. | Persisted value, else the legacy value. | Per-run retry authority. |
| `budgets.provider_inactivity_seconds` (config.go:438) | The no-progress window of one invocation | A (value frozen at run creation, recorded per attempt) | run creation (ceiling) and attempt start (effective) | RunPolicyDigest and attempt provenance `inactivity_limit` (cli_agent.go:900, persisted by #327) | Today `min(live, persisted)` (reconciler.go:931). Planning invocations read it live (supervisor.go:913). Target: frozen exact. | Runs persisted before it existed (pre-#238) take the window their controller binding identifies. Under an identical digest that is the configured window, which is the one the reconciler has always given them. It is **not** "no window": absent would widen them to the unbounded stall this budget exists to remove. See §5. | Per-invocation bound, but its ceiling must not widen on restart (#322). |
| `budgets.attempt_wall_limit_seconds` | Physical provider-attempt wall bound (#328), distinct from the run active-work budget | A | run creation (effective limit, frozen into `run.Budgets`); attempt start (effective deadline, the durable operation `deadline` and `deadline_bound`) | A stated value: controller-effective until B3, then RunPolicyDigest. Absent: in no digest | No. Absent is derived per run at creation; a stated value reaches new runs only. | Runs without a frozen limit keep the operation-remainder rule (§4). | See §4. Implemented by #328. |
| `watch.repositories`, `watch.label` (config.go:492-493) | Which repositories automation may touch, and what counts as consent | C | controller start | controller-effective digest, run ID, PlanID | Yes | None. Stays C. | Enrolment and consent are authority (config.go:488-490). |
| `watch.poll_interval_seconds`, `watch.max_concurrent_runs` (config.go:494-498) | Watch cadence and concurrency | S | supervisor start | SupervisorPolicyDigest | No | As `storage`. | Same bounds as `supervisor.*`, combined stricter-wins (config.go:926-944). |
| `gc.retention_hours` (config.go:515) | Reclamation window for `autonomy gc` | S | at each gc invocation, recorded with the gc report | SupervisorPolicyDigest | Only terminal, unleased material outside the window. A shorter window reclaims sooner and never touches a live run. | None needed. gc journals its own reclamation. | Operator retention policy, not run authority. |
| `operator.id`, `operator.require_configured_id` (operator.go:55,59) | Who a decision is recorded as authorized by | C | controller start. Recorded on every decision. | controller-effective digest, run ID, PlanID, and each decision record | Yes, for future decisions. Past decisions keep their recorded identity. | None. Stays C. | Approver identity is authority provenance (config.go:597-600). |

#### Repository layer (`RepositoryConfig`, `runtime/config.go:604-633`)

The repository layer can only tighten (`runtime/config.go:808-857`). Its members
belong to the same category as the operator bounds they tighten. Once all of them
have moved, the `Repository` member leaves `ControllerBinding.Config` and becomes
an input to the RunPolicyDigest and the SupervisorPolicyDigest.

| Field(s) | Target category | Freeze | Rationale |
| --- | --- | --- | --- |
| `.zenchron.json:budgets.wall_limit_seconds`, `.zenchron.json:budgets.max_execution_attempts`, `.zenchron.json:budgets.max_execution_continuations`, `.zenchron.json:budgets.max_remediation_attempts`, `.zenchron.json:budgets.max_assurance_attempts` (config.go:607-611) | R | run creation, as the already-tightened value | A repository may narrow its own runs. Re-resolution after freeze has no effect. |
| `.zenchron.json:budgets.provider_inactivity_seconds` (config.go:616) | A | as the operator field | As the operator field. |
| `.zenchron.json:budgets.attempt_wall_limit_seconds` | A | run creation, as the already-tightened value | Tighten-only against the operator limit, or the operator `wall_limit_seconds` when none is stated. |
| `.zenchron.json:watch.poll_interval_seconds`, `.zenchron.json:watch.max_concurrent_runs` (config.go:625-626) | S | supervisor start | Supervisor cadence and concurrency, loosen-for-self only. |

### 4. Budget fields needed by #328 (decided before #328 adds them)

| | Run active-work budget | Physical-attempt wall deadline |
| --- | --- | --- |
| Configuration | `budgets.wall_limit_seconds`. The name is kept, and its meaning is narrowed to exactly this. | New operator member (name owned by #328, e.g. `budgets.attempt_wall_limit_seconds`) and the same repository tighten-only member |
| Category | R | A |
| Freezes | At run creation, into `run.Budgets` (already persisted today as `run.Budgets.WallLimit`, controller.go:778-795), and into `RunPolicy` once B1 exists. | The **effective limit** freezes at run creation into `run.Budgets`, and into `RunPolicy` once B1 exists. It is then read back exactly: never live, never `min(live, persisted)`. The **effective deadline** freezes at each physical attempt start as `start + min(frozen attempt limit, remaining run active-work)`. It is persisted as the operation `deadline` (scheduler.go:536-543, already durable) and in attempt provenance (#327). |
| Digest / identity | **Controller-effective until B3**, like every budget today. After B3 it is in the RunPolicyDigest only, and **not** in the controller-effective digest, the run ID or the PlanID. | A **stated** value is **controller-effective until B3**, like every budget today. An **absent** value is never in any digest (see Default). After B3 the limit is in the RunPolicyDigest only. The effective deadline and which bound was binding are in attempt provenance. It is never in the run ID, the PlanID or any operation idempotency key. |
| Charged against | Cumulative active elapsed time across all attempts and operations (reconciler.go:785, #83). Waiting is never charged. | One physical attempt. Ending an attempt at this deadline charges only the active time actually spent to the run budget. It neither consumes nor restores the remaining run budget. |
| Default | Required, as today (config.go:1121). No shipped default. Embedders get 1h (controller.go:437-438), and this is unchanged. | An absent value **stays absent in the digested form**: `omitempty`, an unresolved zero, and **not** resolved in `BudgetConfig.resolved()`. Resolving it before the digest, the `provider_inactivity_seconds` pattern at config.go:464-479 and 745, would add the new key to every existing config's Global digest. That would park every nonterminal run as `controller_changed`, move the run-ID space (#58) and fork every PlanID (#131). The absent value is instead **derived at run creation**, as `wall_limit_seconds` unless #328 justifies and pins a shipped constant (capped by the run budget), and is frozen into `run.Budgets`. A stated value is digested like any other member. This is §6.5 applied to a new field: its shipped default freezes at its own freeze point, not into the controller digest. "30m attempt / 90m run" is an example, not a decision. |
| Invariant | — | A stated attempt limit greater than `wall_limit_seconds` is refused at load, like the lifecycle-below-wall refusal at config.go:1159-1162. If the remaining run budget is below one attempt limit, the attempt is truncated to the remaining budget, and a stop at that bound is recorded as **run active-work exhaustion**, not as an attempt-wall stop. A successor route (a same-binding retry, or `continuation|<revision>` per #54) is advertised only if both its binding-specific authority and the remaining run active-work allow it. |
| Ceilings | Operator ceiling. The repository may tighten it. | Operator ceiling. The repository may tighten it. It can never exceed the frozen run budget. |
| Migration from `wall_limit_seconds` | A config with only `wall_limit_seconds` keeps its meaning as the run active-work budget. | The attempt limit is absent, stays out of the digest, and is derived at run creation by the default rule. A historical run with no frozen attempt limit keeps **today's rule**, which is not "remaining run active-work". Today the attempt bound is the operation's `WallBudget` minus **that operation's own** `ConsumedExecution` (operations.go:1254-1262, scheduler.go:536-541). `WallBudget` is set when the operation is planned from the run's effective wall limit, `min(live, persisted)` (reconciler.go:1583, 915-921), and the bound is further capped by the review-continuation remaining time (operations.go:1259-1263). Cumulative run active time across operations is checked only at reconcile (reconciler.go:785). Nothing is backfilled. **#328 must decide** what `op.WallBudget` becomes: the attempt limit, the remaining run active-work, or the minimum of the two. It must also decide how the review-continuation cap composes with the attempt limit. **Decided by #328:** `op.WallBudget` is kept as the run envelope the operation was planned under, and the attempt deadline composes with it by `min` at `StartWithin` (as does the review-continuation remaining, which is the enclosing envelope once granted). |
| Restart | Reads the frozen value. Consumed active time comes from the existing durable cumulative counter. | An attempt in flight when its controller died is **abandoned**. Its orphaned interval is charged in full to `ConsumedExecution` (scheduler.go:503-508). The next activation mints a **new** `Deadline` (scheduler.go:536-543). Under #328 that successor is a **new physical attempt**. It counts against same-binding retry or continuation authority (#54), and its deadline is `min(frozen attempt limit, remaining run active-work)`. Neither budget is renewed. |

**Ordering (coordinator decision).** #328 may land **before** B1, provided that it:

- (a) keeps an absent attempt limit out of the digested form;
- (b) freezes the effective attempt limit into `run.Budgets` at creation and
  reads it exactly, never live and never `min(live, persisted)`;
- (c) computes remaining run active-work from the existing cumulative counter.

A stated attempt limit is controller-effective until B3, like every budget. B1
remains a prerequisite for B3.

### 5. Legacy interpretation (no invented backfill)

- **Run created before RunPolicy exists, with `run.Budgets` present.** Its
  RunPolicy is derived from `run.Budgets` and marked
  `source: legacy_run_budgets`. It is never claimed as a record made at creation.
  Some dimensions have a documented meaning when absent from `run.Budgets`, and
  keep it: continuations (the #54 rule, `runState.continuationLimit`), provider
  invocations (absent means unbounded), lifecycle deadline (absent means none)
  and attempt wall limit (absent means the pre-#328 operation rule). Any other
  member the run never recorded (wall, execution, remediation and assurance
  attempts, and the inactivity window, which is absent for pre-#238 runs) takes
  **the value the run's controller binding identifies**. That is the
  configured value, and it is correct only because it cannot be anything else:
  such a run is reconciled solely by a controller with the identical binding,
  and so the identical config digest. Any other controller parks it
  `controller_changed` before planning, and #307 re-adopt refuses while it is
  live. The fallback therefore reproduces exactly the value the run already
  had, and never a wider one. Present members, including `lifecycle_deadline`,
  `max_remediation_attempts` and `max_assurance_attempts` (persisted but never
  read back before B1), are read from `run.Budgets` exactly.
- **Run with nil `run.Budgets`.** The configuration that governed it is
  identified only by the config digest inside its controller binding, and the
  file cannot be recovered from that. Every member takes the binding value, by
  the same rule and for the same reason as above. Status reports the source as
  `legacy_controller_binding` and claims no RunPolicyDigest. Under a changed
  controller, the binding value is unverifiable, and status says so instead of
  presenting today's configuration as the run's. Such a run can be continued only by a
  controller with the identical legacy binding. Otherwise it is settled, as
  #307 requires today. #89 may classify it `compatible_with_migration` only when
  every dimension can be derived from durable run facts. Otherwise the result is
  `waiting_configuration_drift` or `blocked_governance_change`.
- **Attempts before #327.** They carry no persisted invocation provenance. The
  operation's durable `deadline` and `no_progress_budget` are what can be said
  about them. Nothing is reconstructed.
- **Supervisor starts before SupervisorPolicy exists.** Their policy is
  identified by the controller binding's config digest, and nothing more.

### 6. Contradictions found (recorded, not silently resolved)

Seven items: six contradictions or gaps, and one confirmed agreement (7).

1. **"Existing runs keep their policy" (#329, requirement 4) against the current
   reads.** `run.Budgets` is persisted at creation (controller.go:778-795), and
   the comment at controller.go:790-794 admits that "the wall limit and the
   attempt ceilings are still read live". Wall limit, execution attempts and
   inactivity window are read as `min(live, persisted)` (reconciler.go:915-935).
   Lifecycle deadline is read live (reconciler.go:796). Remediation and
   assurance attempts are read live (reconciler.go:944-953). Planning invocations
   take the inactivity window live (supervisor.go:913) and the wall limit live
   (controller.go:472-483). This is safe only while every budget is
   controller-effective. It is condition 6 of §2. B1 fixes every run reader.
   The planning reads move to **B3** (#345): no run exists for a planning
   invocation, so there is no RunPolicy to freeze into, and both values are C
   until B3. B3 must give that fallback a freeze point, for example the plan
   revision, before `wall_limit_seconds` or `provider_inactivity_seconds` leaves
   the controller-effective digest.
2. **Status against requirement 8.** The run report's `Budgets` is the current
   process's configuration (controller.go:1096), not the run's. Its
   `ControllerIdentity.ConfigDigest` is the current digest (controller.go:1077),
   not the digest the run was created under. The run row stores only
   `ControllerSHA256`. B1 fixes the budgets. The digest the run was created
   under can already be identified through the binding.
3. **#58 ("do not remove ConfigDigest from run identity") against #329 (a mutable
   default must not fork identity).** This is resolved by scope, not by choosing
   one. The controller-effective digest stays in the run ID and PlanID, so #58's
   constraint holds. Only non-C fields leave, and their provenance moves to their
   own digest. However, the digest composition change is **itself** a
   run-ID-space change. With `watch.issueRun` still probing only the current
   space (watch.go:503-530), the migration controller would re-enrol every
   terminal opted-in source. B3 is therefore **gated on #58's source-level
   history lookup**, or must ship with it.
4. **#131.** `PlanID` includes the whole digest (plan_intent.go:141-145), so any
   config edit forks the plan and a fresh envelope comes with it. B3 narrows this
   to C fields. #131's remaining question, whether a C change should fork PlanID
   at all, stays open and is not decided here.
5. **Shipped defaults sit on both sides of the digest.** Budgets resolve before
   the digest (config.go:745), so a binary that changes
   `DefaultProviderInactivitySeconds` (provider_inactivity.go:79) or
   `DefaultMaxExecutionContinuations` changes the config digest of every config
   that omits them. That refuses ordinary succession. The plan-envelope defaults
   (config.go:294-298), the watch defaults, `DefaultGCRetention` (gc.go:60) and
   `DefaultFeedbackPermission` (feedback.go:98) are applied **after** the digest,
   so changing them in a binary alters behaviour across an ordinary succession
   with an identical config digest. For `feedback.min_permission` that is an
   authority default. It is currently authorized only by the adopted-build
   review of the code change. Rule adopted here: **the shipped default of a
   category C field is authority and must be resolved before the
   controller-effective digest. Shipped defaults of R, A, S and P fields are
   frozen into their own policy digest at the freeze point.** This is B6.
6. **The repository layer comes from the process working directory**
   (autonomy.go:614), not from each target repository. Two invocations with the
   same operator file from different directories compute different controller
   bindings. This is not in #329's scope. It is recorded because it is a
   non-obvious input to the controller-effective digest today.
7. **The #329 body and the current code agree on the #324 mechanism.**
   `evaluateConfiguration` refuses any digest change and defers compatibility
   to #89 (controller_succession.go:287-299), and #307 re-adopt refuses while an
   incompatible run is live (controller_readopt.go:296-305). There is no
   contradiction here.

### 7. What #89's `waiting_configuration_drift` consumes

#89 does not classify fields. For each nonterminal run it compares the incoming
generation against this table:

- **A change to a C field** is not drift. It is a controller change: ordinary
  succession if the digest is identical, otherwise #307. It is never `resume`.
- **R and P fields** can never drift. The run or plan revision carries its
  frozen policy and digest, and new defaults apply only to new runs or new
  revisions. The only question is whether that frozen policy exists: yes →
  `compatible`; derivable per §5 → `compatible_with_migration`; not derivable →
  `waiting_configuration_drift`.
- **A fields are what `waiting_configuration_drift` is for.** When a run's next
  attempt would execute under a resolved posture that differs from the posture
  frozen at assignment (#84 fingerprint: command, home, model, and the effective
  toolchain), the result is `waiting_configuration_drift`. A **widening**, such as
  an added toolchain capability or a bypass, is `blocked_governance_change` and
  cannot be acknowledged automatically.
- **S fields** can never drift. They apply at the next supervisor start.

## Phase B plan (separable issues)

Each item is its own issue and PR. None of them removes a field from the
controller-effective digest before its destination satisfies all of §2.

- **B1: frozen RunPolicy with a RunPolicyDigest.** Persist the complete resolved
  run policy at creation, together with the repository tightening that produced
  it. Record its digest in the genesis event. Make every reader use the frozen
  value (fix reconciler.go:796, 915-953). Report it in status
  (controller.go:1096). Implement the §5 legacy derivation. The planning
  invocation reads (supervisor.go:913, controller.go:472-483) are **not** B1:
  no run exists, and they are C until B3 (see §6.1 and B3).
  Nothing leaves the digest yet. B1 is a prerequisite for B3. It is **not** a
  prerequisite for #328.
- **B2: #328.** Add the attempt wall limit and the successor admissibility rule
  of §4. It may land before B1 under the three conditions in §4 (Ordering). It
  keeps an absent attempt limit out of the digest.
- **B3: split the controller-effective digest.** `ControllerBinding.Config`,
  the run ID and the PlanID hash only the C projection. R and P fields
  (`budgets.*` except `provider_inactivity_seconds`, `default_agent`, `plan.*`)
  leave. It is a one-time boundary: the binding digest changes, so the first
  controller needs #307 re-adopt with no incompatible live runs, unless #89 has
  landed a `compatible_with_migration` for it. **Gated on #58.** Partially
  addresses #131. Includes the required dogfood: Run R alive under default A,
  change to B, R continues under A, new Run S gets B, no re-adopt. A separate
  C-field change still requires #307. B3 must list every consumer in "How the
  digest is built today" item 5 and handle it explicitly. In particular, the
  **evidence producer identity** `Producer: {ControllerID, ControllerSHA256}`
  (operations.go:1539,1678) changes for every new evidence record. Existing
  evidence keeps its recorded producer revision. B3 also covers the adoption
  guard, the handoff recovery owner, the `runs.controller_sha256` column, the
  plan-cancel owner, the re-adopt recompute, and plan-stage `derivedRunID`.
  B3 also owns the **planning invocation fallback** (supervisor.go:913,
  controller.go:472-483), which is moved here from B1. No run exists for a
  planning invocation, and its fallback wall and inactivity window are C until
  B3. They must be given a freeze point before those fields leave the digest.
  Tracked in #345.
- **B4: SupervisorPolicy generation.** A durable supervisor-start record with a
  SupervisorPolicyDigest over `storage.*`, `supervisor.*`,
  `watch.poll_interval_seconds`, `watch.max_concurrent_runs`, `gc.*` and
  `agents.<id>.unattended`. Then those fields leave C. No hot reload.
  `agents.<id>.unattended` must be **re-justified** in B4 before it leaves C.
  It gates autonomous starts under that agent's account, so a false → true
  change widens what may run unattended as that account. If B4 cannot show
  that this is scheduling policy rather than authority, it stays C.
- **B5: attempt and provider posture.** After #327 (persisted invocation
  provenance) and #84 (resolved-agent fingerprint bound at assignment), move
  `agents.<id>.command`, `home` and `model`, `provider.model`, `toolchain.*`
  and `budgets.provider_inactivity_seconds` to A.
- **B6: shipped-default law.** Resolve authority-bearing C defaults (feedback
  permission, watch label) before the digest, and freeze the defaults of the
  other categories into their policy digest (§6.5).
- **B7: re-scope #89** against §7 and the §3 table.

These stay C with no follow-up: `state_dir`, `project_model_path`,
`policy_path`, `planning_dir`, `assurance.*`, `provider.kind`,
`provider.auth_mode`, `provider.credential_path`, `provider.endpoint`,
`agents.<id>.kind`, `agents.<id>.trust_mode`, `agents.<id>.credential_path`,
`agents.<id>.endpoint`, `agents.<id>.allow_permission_bypass`, `github.*`,
`feedback.*`, `watch.repositories`, `watch.label` and `operator.*`.

## Open questions

1. Should a C change fork PlanID at all (#131), or should plan lineage survive a
   controller change through #307, the way run lineage will through #58?
2. Should a RunPolicy for a plan stage record the stage and profile narrowing
   as a separate provenance member (`tightenedBy`, controller.go:493-506), or
   only the resulting values?
3. The policy and project-model **content** is not in the controller-effective
   digest, only the path is. The contract pins the policy revision. Should the C
   digest bind a content digest instead of a path?
4. `assurance.dependency_cache_dir` stays C. Once verifier provenance records
   the cache identity, can it become A?

## Consequences

- An operator can change a future-run default without settling unrelated live
  work, but only after B1 and B3. Until then, #307 remains the path, correctly.
- Each run, attempt and supervisor generation becomes self-identifying, so
  status can stop reconstructing policy from today's file.
- The cost is three new durable digests (RunPolicy, SupervisorPolicy, and
  attempt provenance through #327), plus one deliberate identity boundary (B3)
  that has to be crossed with #307 and must not ship before #58.
