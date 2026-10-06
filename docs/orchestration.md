# Basic explicit orchestration

`autonomy orchestrate` hands a running `serve` supervisor a bounded set of
existing GitHub issues and one execution agent **once**, and reads the whole
cohort back from one aggregate view. It is the bootstrap primitive of #470: it
removes the operator from starting, tracking and relaying between many coding
agent sessions by hand.

```bash
zenchron-engineering serve                     # in one terminal, owns the work
zenchron-engineering autonomy orchestrate issues 481 482 483 484 485 486 487 488 489 490 --agent claude
zenchron-engineering autonomy orchestrate status BATCH_ID --text
```

```text
explicit issue list
  -> one durable OrchestrationBatch (written once, never updated)
  -> one ordinary EngineeringRun per issue
  -> the existing scheduler decides when each runs
  -> the named execution agent works each run in its own candidate workspace
  -> each finished worker writes a typed handoff
  -> the runtime admits the handoff bound to the candidate it committed
  -> one aggregate status
```

## What it is not

It is **basic explicit orchestration**. It is not automatic roadmap or backlog
execution, not the later WorkGraph (no dependencies between items), not a
planner (that is `autonomy plan`, #64), not cross-repository programs, not
automatic review or remediation routing, not an organization control plane,
and not the API agent harness (#446).

It is also not a second runtime. A batch has no scheduler, worker pool,
queue, lease, retry counter, attempt counter, provider session, candidate,
evidence or authority of its own. Each item's child is an ordinary
`EngineeringRun` with its own contract, workspace, agent binding, budgets,
waits, evidence, authority and PR lifecycle, driven by the same tick loop and
leased by the same scheduler as any other run.

## Submission

- Only the issues you name are worked. Nothing is discovered, and an open issue
  is not a reason to spend.
- `--agent` is required. A batch never falls back to the default agent.
- Issues must be distinct and positive; a batch names at most 32.
- The repository must be one this supervisor governs, and the agent must pass
  the same readiness probe a single submission does.
- The agent's adapter must be able to write the runtime-owned typed result
  directory, because the handoff is mandatory. This is asked of the provider
  generically (`TypedResultWriter`); a CLI adapter answers from the argv it
  would actually run. Today Claude Code (`--add-dir`) and Codex
  (`writable_roots`) can; Gemini, Qwen and the brokered API agent cannot, and a
  batch naming one is refused before anything is written.
- An issue that already has a live run is refused, and the refused request
  writes nothing. Orchestration does not adopt or race work it did not create.
- The batch identity is a pure function of repository, agent and issue set.
  Sending the same request again — after a lost reply, or from another
  terminal — returns the same batch and creates no second run.

The batch is written with every child's run identity already decided, then the
children are created. If the supervisor stops in between, its next pass
creates exactly the missing children under those identities.

## Concurrency

A batch never raises concurrency. With `supervisor.max_concurrent_runs: 3`,
ten items execute at most three at a time and the rest queue. With a ceiling of
ten, all ten may execute at once. The ceiling is the scheduler's, enforced
durably per capacity class; the batch has no pool that could compete with it.

## The mandatory handoff

Provider success is not orchestration completion. Every invocation of a batch
child is given a runtime-owned file to write, outside its candidate workspace,
named by that exact physical attempt and emptied before the invocation starts:

```json
{"schema_version":"0.1","outcome":"completed","summary":"...","unresolved":[],"recommended_next":["..."]}
```

- `outcome` is `completed` (nothing unresolved) or `partial` (naming what is
  unresolved). Anything else, an unknown member, a wrong version, malformed or
  trailing JSON, an oversized document (32 KiB), an oversized summary (4 KiB) or
  list (16 items of 1 KiB) is refused, never repaired.
- The worker never restates the run, issue, branch, commit, tree, changed paths,
  contract or agent. The decoder refuses those members.
- The file is read by the runtime, and only for an invocation that completed.
  The transcript and final message are never read for a handoff.

When the invocation completes, the run's journal records `handoff.reported`
(the report's digest and outcome) or `handoff.refused` (`missing` or
`invalid`, with the reason). The supervisor then admits a reported handoff once
the runtime has committed that invocation's output: it re-reads the slot,
refuses it unless it is byte-identical to what was journalled, and writes an
immutable `EngineeringHandoff` whose run, issue, batch, base, candidate, tree,
contract, changed-path count and digest, and producer all come from the
runtime's own records. The worker's report stays a claim; it authorizes
nothing, satisfies no evidence and changes no contract.

A continuation that completes a runtime-owned checkpoint without changing
anything further has no commit of its own (#489). Its handoff binds to that
checkpoint, and only when the runtime's own records prove all three: the
continuation's durable binding names checkpoint C and its checkpoint-completion
claim was admitted against C; C was journalled as a checkpoint exactly once and
nothing after it moved the candidate head, so C is still the run's candidate;
and the continuation's own completion is journalled against C's exact commit
and tree. Any other answer refuses the handoff durably. A continuation that did
change the candidate binds to its own newer commit, never to C, and an
invocation with neither a commit nor a completed checkpoint stays unbindable.
Nothing the worker writes names the commit: the report cannot carry one.

Checkpoint admission uses a conditional SQLite insert against the validated
journal sequence: if a candidate-moving event commits after the snapshot was
read but before insertion, no handoff is inserted. The next admission pass,
including after restart, replays that movement and records the refusal. Journal
appends and the conditional insertion serialize in the same durable store;
currentness holds through the admission commit.

If the slot no longer holds exactly the journalled document when admission
runs (it was changed or removed), that handoff is durably refused once and the
item retains its current lifecycle; once the producer stage finishes, it
reports `handoff_pending` with the reason. Admission needs no execution
authority: it reads only the batch, the run's journal and the state directory,
so a report is still finalized after its agent is removed from configuration.

A missing or invalid handoff does not fail the run: failing it would quarantine
the work and make a retry redo it. The refusal remains visible as a handoff
observation and separate `handoff_reason` while execution, commit,
reassessment, assurance or remediation continues. Only finished producer work reports `handoff_pending`.

## Status

`autonomy orchestrate status BATCH_ID [--text]` reads the batch from durable
state — with or without a running supervisor, and identically after a restart.
Item state is projected from the child run on every read:

| state | meaning |
| --- | --- |
| `not_created` | the batch is recorded; the child's creation has not landed yet |
| `queued` | live child awaiting its turn or known scheduler capacity; capacity names its class and ceiling |
| `running` | currently holding a scheduler slot, including commit, assurance and remediation |
| `waiting` | live child in a typed wait or an operator pause |
| `handoff_pending` | producer stage finished, no operation owned, and handoff admission is the remaining orchestration obligation |
| `completed` | producer stage finished, no operation owned, and its admitted handoff reports `completed` |
| `partial` | producer stage finished, no operation owned, and its admitted handoff reports unresolved work, named in the reason |
| `failed` / `stopped` | the child run failed or was cancelled |

A failing or waiting child never stops its siblings. `autonomy status RUN` and
`autonomy logs RUN` remain the drill-down for one child.

Lifecycle takes precedence over the latest invocation's handoff. Producer-stage
completion is the runtime's durable `goal_state_reached` marker after the latest
engineering work operation, or a completed child run. New work invalidates an
older marker until reconciliation reaches the goal again. Unchanged observation polls retain the existing deduplicated wait.
Current-head assurance, CI or review failures also prevent producer completion.
A failed or stopped child retains its terminal outcome even if an earlier handoff was admitted.

Status reads all child lifecycle, operation and handoff records in one read-only
SQLite snapshot. A crash can still leave durable journal/row disagreement;
that item is counted as unknown with `observation: "transitioning"`. An owned
operation resuming from a recorded wait reports `running`, preserves the run's
recorded disposition and reason, and explicitly marks that transition. The text
view prints these observations too. An operator pause overrides a queued
capacity wait and remains visible as `paused`; it does not interrupt work
already owned. Terminal runs retaining owned work remain explicitly
transitioning until ownership is released. No batch can report completed, partial or
handoff-pending producer work while that child owns an operation or a granted
nested verification tool. Retained tool grants remain visible in
`verification_tools` until cleanup releases them durably.

Recognized capacity waits use `work_capacity_unavailable`,
`observation_capacity_unavailable` or `verification_capacity_unavailable` and
an optional `capacity` object containing `class` and the enforced `ceiling`.
The scheduler records that object in `run.waiting` at refusal, so restarting or
changing configuration cannot rewrite the explanation. Known capacity queue
time follows the existing external-wait budget accounting; no attempt ran.
Other acquisition refusals retain `operation_unavailable` when their cause is
unknown. Capacity observations grant no scheduling or execution authority.

This adds optional JSON metadata, with no database migration or rewriting of
old events. Historical waits without capacity metadata remain readable and
keep their original explanation; a current refusal can record the new metadata.
Older binaries can replay the journal but do not project these corrected
lifecycle states or expose the new capacity fields. Use the current binary for
truthful status; downgrade does not remove the recorded facts.
