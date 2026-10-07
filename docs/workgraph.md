# WorkGraph

A **WorkGraph** is the durable operational dependency graph (#472). Where
`autonomy orchestrate` (#470) hands the supervisor a flat set of issues,
`autonomy workgraph` states the dependencies between them, so a unit waits for
its upstream work's **admitted handoff** instead of the operator watching for it
and starting the next thing by hand.

```text
WorkGraph
  ├── WorkUnit A
  ├── WorkUnit B depends_on A
  ├── WorkUnit C depends_on A
  └── WorkUnit D depends_on B, C
```

```bash
zenchron-engineering serve                       # in one terminal, owns the work
zenchron-engineering autonomy workgraph adopt graph.json --agent claude
zenchron-engineering autonomy workgraph status GRAPH_ID --text
```

```json
{
  "name": "m2-o1",
  "revision": 1,
  "units": [
    {"id": "schema",  "purpose": "land the schema",  "role": "implementer", "issue": 481},
    {"id": "reader",  "purpose": "land the reader",  "role": "implementer", "issue": 482, "depends_on": ["schema"]},
    {"id": "writer",  "purpose": "land the writer",  "role": "implementer", "issue": 483, "depends_on": ["schema"]},
    {"id": "surface", "purpose": "land the surface", "role": "implementer", "issue": 484, "depends_on": ["reader", "writer"]}
  ]
}
```

## The five questions it answers

A WorkGraph answers exactly these, and no others:

- what work units exist;
- what depends on what;
- which dependencies are satisfied;
- which units are runnable now (the **frontier**);
- which child `EngineeringRun` belongs to a unit.

**Which eligible operation executes now remains the scheduler's question.** A
graph has no worker pool, semaphore, queue, lease, attempt counter, retry
engine or run database of its own. A unit becoming runnable writes one ordinary
one-issue #470 batch; the existing orchestration pass creates its ordinary
`EngineeringRun`, and the existing scheduler leases it under the existing
`max_concurrent_runs` ceiling. A graph can never raise concurrency, and can
never make a run execute sooner than the scheduler grants it.

It is also **not a second planner**. #64's `EngineeringPlan` answers which
stages one objective requires, resolves them to profiles and owns the approval
boundary; read `docs/planning.md` for that. A WorkGraph advances existing
issue-backed units through handoffs, and reuses #64's role catalogue rather
than inventing a second vocabulary for the same fact.

## Dependency satisfaction is the admitted handoff

**Provider exit is not satisfaction.** A unit is satisfied only when its child
run has transferred an `EngineeringHandoff` that the runtime **admitted** and
bound to the exact candidate that child committed — the #470 primitive
described in `docs/orchestration.md`, consumed here unchanged.

So between a worker finishing and its handoff being admitted, the unit reads as
`running` with handoff `reported`, and nothing downstream of it has a run. An
admitted handoff whose report says `partial` does not satisfy either: unresolved
work named by the producer is not a completed input.

Provider kind appears nowhere in graph progression. The same graph advances
identically under any agent whose adapter can write the typed result directory,
and an agent that cannot is refused at adoption — nothing it produced could ever
satisfy a dependent unit.

## Adoption, revisions and mutation

- The graph identity is a pure function of repository, agent and **name**; a
  revision's identity is its **contents**. Resubmitting the same proposal — after
  a lost reply, or from another terminal — finds the revision already adopted.
- `--agent` is required. A graph never falls back to the default agent.
- A revision is an **append**. Adopting revision N+1 leaves N and every
  activation exactly as they were, so a revision can never reset a budget a
  child run has already consumed.
- Adoption **starts nothing**. The next supervisor pass activates whatever the
  frontier then says is runnable. A unit that stays `ready` across several
  passes is one the supervisor could not activate; `serve` logs the reason,
  most often that the unit's issue already has a live run this graph did not
  create.
- A proposal may later be written by a planner or a model. Nothing trusts the
  proposer: a revision is adopted only by passing deterministic validation.

Refused deterministically, before anything executes:

- a dependency cycle, a self-dependency, a repeated or unknown dependency;
- a duplicate unit id, or two units performing one issue (they would collide on
  one child run);
- a role outside the catalogue, a missing purpose, id or issue;
- more than 32 units (the same cohort bound a #470 batch has, and the bound
  that keeps a proposal inside one control request);
- a first revision other than 1, a revision that skips, and a different
  document under a revision already adopted;
- a mutation that **removes** a unit already activated, or changes its issue,
  role, purpose or dependencies. Such a unit is frozen: work has already been
  performed against the inputs the earlier revision named. Adding units, and
  editing units nothing has activated, are allowed.

A proposal may not state the repository, the agent or anything else the runtime
owns; the strict decoder refuses such members rather than dropping them.

## Status

`autonomy workgraph status GRAPH_ID [--text]` reads the current revision from
durable state — with or without a running supervisor, and identically after a
restart. Nothing is stored: every unit state is projected from the revision
document, the activation records and the child runs on each read.

| state | meaning |
| --- | --- |
| `blocked` | a dependency is not satisfied; the reason names it and says whether anything could still satisfy it |
| `ready` | runnable: every dependency is satisfied by an admitted output, and no child run is claimed yet |
| `invalidated` | activated, but its dependencies no longer present the outputs it was activated against |
| `unknown` | activated, and its child run could not be read |
| anything else | the activated child's own #470 item state: `not_created`, `queued`, `running`, `waiting`, `handoff_pending`, `completed`, `partial`, `failed`, `stopped` |

A failed or stopped unit **blocks** its dependents, with a reason saying the
branch is dead, and leaves every other branch alone. So does a unit whose child
run has **ended** without satisfying it — a terminal `handoff_pending` or
`partial` — which the state alone does not say; while the same child is still
live, its dependents read as waiting instead. An unreadable child, or one in a
state this binary does not recognize, blocks only what depends on it.

"Dead" propagates: everything below a dead unit is dead too, so a whole branch
below one failure says so rather than only the unit directly above it.

## Exact-subject invalidation

Once completed is **not** always completed.

When a unit is activated, the exact upstream outputs it consumes — each
dependency's admitted candidate revision and tree — are digested and recorded
with the activation. If an upstream output is later replaced (its run transfers
another admitted handoff bound to a different candidate), or an upstream unit
stops being satisfied at all, the recorded digest no longer matches and the
affected downstream units become `invalidated` rather than silently staying
complete.

`invalidated` is terminal for the graph: an invalidated unit is not in the
frontier, nothing downstream of it is, and no pass re-performs it. Re-performing
invalidated work is remediation, which #472 deliberately does not do.

## A unit's child run is its issue's one-issue batch

Activating a unit writes — or finds — the #470 batch for exactly
(repository, agent, that one issue). Two consequences follow from that identity,
and neither is adoption of foreign work:

- if that batch already exists from an earlier direct `autonomy orchestrate`,
  the unit uses the child run it already owns. Work already done with an
  admitted handoff therefore satisfies the unit immediately.
- that also holds when the existing run is terminal and **failed**: the unit
  inherits it, and the branch below it is dead. A one-issue batch is bound to
  its first run generation for the lifetime of the batch, so re-performing that
  issue needs a batch generation #470 does not yet offer. Use a distinct issue,
  or stop and resubmit the underlying work, until it does.

## Restart and replay

A unit's child run identity comes from the one-issue batch, which is a pure
function of repository, agent and issue, and an activation row is write-once
keyed by graph and unit. So a crash between writing the batch and recording the
activation replays onto the same batch and the same run, a restart reproduces
the identical frontier, and no replay or recovery produces a second child run
for a unit.

## Not in this ticket

No automatic review, remediation or re-performance of invalidated work; no
integration algorithm; no cross-repository programs; no gate or human-decision
units (every #472 unit is agent-backed, and gates remain #64's); no typed
collaboration messages, findings or decision requests (#473); and no changes to
the handoff protocol itself (#492).
