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

A dependency does not only gate time — it **delivers**. When a unit is activated,
the exact admitted outputs it consumes are recorded with the batch that owns its
child run, and that child's invocation is given them: each upstream unit id, run,
commit, tree, the admitted handoff identity, the producer's own report, and the
diff that change contains. The report is another worker's words, so it reaches
the invocation framed and neutralized as untrusted data, exactly as a reviewer's
upstream diff already does.

Composing upstream *code* into a downstream candidate is #475's. What #472
guarantees is that the recorded input set is a fact about the execution rather
than bookkeeping beside it: the invocation receives the handoffs its activation
was bound to, and a run bound to anything else receives none of them.

Provider kind appears nowhere in graph progression. The same graph advances
identically under any agent whose adapter can write the typed result directory,
and an agent that cannot is refused at adoption — nothing it produced could ever
satisfy a dependent unit.

## Adoption, revisions and mutation

- The graph identity is a pure function of repository, agent and **name**; a
  revision's identity is its **contents**, canonicalized by unit id and by
  dependency reference. Two documents that describe the same graph therefore have
  the same revision identity however the proposer ordered them, and resubmitting
  the same proposal — after a lost reply, from another terminal, or re-emitted in
  another order by a planner — finds the revision already adopted.
- `--agent` is required. A graph never falls back to the default agent.
- A revision is an **append**. Adopting revision N+1 leaves N and every
  activation exactly as they were, so a revision can never reset a budget a
  child run has already consumed.
- An **unresolved decision** can hold a unit even when every dependency is
  satisfied; see below.
- Adoption **starts nothing**. The next supervisor pass activates whatever the
  frontier then says is runnable. A unit that stays `ready` across several
  passes is one the supervisor could not activate; `serve` logs the reason,
  most often that the unit's issue already has a live run this graph did not
  create.
- A proposal may later be written by a planner or a model. Nothing trusts the
  proposer: a revision is adopted only by passing deterministic validation.

Refused deterministically, before anything executes:

- a dependency cycle, a self-dependency, a repeated or unknown dependency;
- a duplicate unit id, or two units performing one issue (each would get its own
  bound child execution, and one live run per issue means one of them could never
  start);
- a role outside the catalogue, a missing purpose, id or issue;
- more than 32 units, or a document above 6 KiB. The document bound is the one
  that matters: per-field limits multiply out past any single control request, so
  the graph's own content is bounded by the number the proposal reader and the
  control transport use. A graph this build accepts is a graph an operator can
  actually submit, and that is asserted against the real request-line ceiling
  rather than reasoned about;
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
| `awaiting_decision` | every dependency is satisfied, and a readiness owner outside the graph is holding the work on an unresolved decision |
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
dependency's unit, run, admitted handoff, candidate revision and tree, with the
producer's own report — are recorded with the batch that owns its child run, and
their digest with the activation. If an upstream output is later replaced (its run transfers
another admitted handoff bound to a different candidate), or an upstream unit
stops being satisfied at all, the recorded digest no longer matches and the
affected downstream units become `invalidated` rather than silently staying
complete.

`invalidated` is terminal for the graph: an invalidated unit is not in the
frontier, nothing downstream of it is, and no pass re-performs it. Re-performing
invalidated work is remediation, which #472 deliberately does not do.

## A unit's child run is the unit's, not the issue's

Activating a unit writes — or finds — a #470 batch whose identity binds the
**graph**, the **unit** and the **exact input set** it consumes, over that one
issue. The issue number alone never names it.

That is the whole correctness property. Reuse is valid only where durable state
proves it is the same unit execution against the same exact inputs:

- replaying a crashed activation finds the batch and child run it already
  created, so no replay or recovery produces a second child;
- an earlier direct `autonomy orchestrate` of the same issue is a **different**
  batch, so however finished that run is, it never satisfies this unit — it never
  executed against these inputs;
- another graph's unit on the same issue is a different batch too;
- the same unit against different inputs is a different batch, and an already
  activated unit whose inputs have since moved is `invalidated` rather than
  re-pointed.

The existing "one live run per issue" law still applies, and it applies to a
graph the same way it applies to anything else: a unit whose issue already has a
live run this graph did not create is **refused** rather than adopting it, the
refusal is reported in the supervisor's `work_graphs` tick lines, and the unit
stays `ready` until that run reaches a terminal disposition.

## Restart and replay

A unit's child run identity comes from the one-issue batch, which is a pure
function of repository, agent and issue, and an activation row is write-once
keyed by graph and unit. So a crash between writing the batch and recording the
activation replays onto the same batch and the same run, a restart reproduces
the identical frontier, and no replay or recovery produces a second child run
for a unit.

## Unresolved decisions

A unit can be held by a readiness owner **outside** the graph: an unresolved
decision the work needs before it may run. Such a unit reads `awaiting_decision`,
is not in the frontier, and is therefore never activated; everything downstream
of it waits, but is not reported as a dead branch. The moment the owner stops
reporting the hold, the ordinary frontier computation includes the unit again and
it runs like anything else.

The graph **represents** holds and resolves none. #472 owns no decision record,
no authority and no persistence for one — that is #508 — and there is no path by
which a worker answers its own hold. A hold source that cannot answer holds
everything: the pass reports it and activates nothing for that graph rather than
proceeding past a hold that may exist. Until #508 persists them, nothing supplies
holds, so `autonomy workgraph status` shows none.

## Not in this ticket

No automatic review, remediation or re-performance of invalidated work; no
integration algorithm; no cross-repository programs; no gate or human-decision
units (every #472 unit is agent-backed, and gates remain #64's); no typed
collaboration messages, findings or decision requests (#473); and no changes to
the handoff protocol itself (#492).

#473's typed messages are scoped to a **batch**, and a graph unit's batch is that
one unit execution's, so units of one graph do not share a message scope.
Dependencies deliver through the admitted handoff described above — that is the
#472 channel. Cross-unit messaging would change #473's scoping and belongs with
whoever owns that question, not here.

A `decision_request` message (#473) is a worker **asking** for a decision. The
`awaiting_decision` hold above is the authority that owns a decision saying the
work may not proceed. They are different facts, and nothing wires the first into
the second: a worker cannot hold, or release, its own graph.
