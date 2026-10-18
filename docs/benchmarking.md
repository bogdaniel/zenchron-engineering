# Benchmark measurement (the minimal #66 slice)

This is the smallest measurement system that makes a paired
`zenchron_planned` vs `direct_agent` run trustworthy (#481 Workstream A). It is
not the #66 platform: there is no corpus, no ratio, no optimization loop. It is
three things, in package `benchmark/` with the CLI in `zenchron-engineering
benchmark`:

1. an append-only **operator activity log** (the source of truth for human time);
2. a bounded, read-only **runtime evidence collector** over existing durable state;
3. an immutable **result emitter** in the existing `benchmarks/` shape.

It is not a second run database, scheduler or orchestration service. The
collector opens the runtime store through `runtime.ReadStore` (SQLite
`mode=ro`, `query_only`), reads, and never writes. The authorization kernel does
not import this package.

## Operator activity log

One JSONL file per paired measurement, appended to while the run happens. Each
interval is a `start` line and a `stop` line; an `audit` line attaches
independent verification to a relay.

```text
zenchron-engineering benchmark log start  --log run.jsonl --arm zenchron_planned --operator me --kind reviewing_output
zenchron-engineering benchmark log stop   --log run.jsonl            # closes the single open interval
zenchron-engineering benchmark log record --log run.jsonl --arm direct_agent --operator me \
    --kind manual_relay --relay-kind finding --start 2026-10-20T09:05:00Z --end 2026-10-20T09:07:00Z
zenchron-engineering benchmark log audit  --log run.jsonl --id act-... --auditor them --evidence-sha256 <digest>
zenchron-engineering benchmark log summary --log run.jsonl
```

Kinds (`--kind`):

| kind | meaning | counts as human attention |
|---|---|---|
| `active_supervision` | watching or steering | yes |
| `task_preparation` | intake, selection, prompt or assignment composition | yes |
| `reviewing_output` | reading outputs, review | yes |
| `authority_intervention` | an intended approval, decision or merge intent | yes |
| `manual_relay` | carrying a prompt, context, finding or status between agents; `--relay-kind prompt\|context\|finding\|status` is required | yes |
| `recovery_workaround` | working around the system or a failure | yes |
| `final_acceptance_merge` | the closing acceptance check and merge | yes |
| `unattended_wait` | provider or CI waiting while the human is not attending | **never** |

The log refuses, at append time and again when read: an unknown arm or kind; a
missing operator; a relay without a sub-kind (or a sub-kind on anything else);
a duplicate entry (an entry id is derived from its content); a stop before or at
its start (zero and negative intervals); overlapping intervals (one human has
one attention stream; unattended waiting is its own interval, not a layer on
top); and a second operator anywhere in the log. A refused entry never reaches
the file. There is no cross-process lock; one operator runs one CLI at a time.

### The same-operator rule

The first valid pair requires the **same human** to operate both arms, so the
log holds both arms and one operator id. A log with two operators is reported
invalid: its result is emitted flagged `valid: false`, not silently accepted.
No AI operator assistant may queue, monitor, interpret or review in either arm;
that is a protocol rule the log cannot enforce, only record.

### Self-reported versus audited relays

A relay count is **`self_reported`** until an auditor who did not operate or
assist either arm inspects the screen/session evidence and attaches an `audit`
line (auditor id plus the sha256 of the evidence they reviewed). The auditor
must differ from the operator. The result reports relays as `audited`,
`partially_audited` or `self_reported`, with the counts of each. A claim of
**zero** relays can only ever be `self_reported`: absence of an event cannot be
attached to an audit record. Do not invent an observer's verification.

## Runtime evidence collector

`benchmark emit --state-dir <dir> --graph <id> [--run <id> ...]` reads, from
existing durable state only: the WorkGraph, its revision digest and objective
plan reference; work units and their assignments (role, issue, dependencies,
state, reason, child run, awaiting-decision, admitted handoff and output
revision, review requirement and approval); per run, the agent, provider kind,
trust mode, model, phase, disposition and reason, candidate revision/tree, PR
number and head, attempts, executed operation intervals, handoffs reported and
refused, review observations, review continuations, delivered decisions and
human-authority records; and the plan stream's event counts and budget cost.

Concurrency is **computed from real worker-execution intervals**: each finished
`execution.invoke` attempt's `started_at` to its journalled `operation.after`,
across the scope's runs. Housekeeping operations (source or GitHub polling,
assurance, base integration) are not work and are excluded, and a run's
create-to-terminal lifespan is never used (a run parked awaiting publication
authority is not terminal but its executions are exact). It is reported only when
established: at least one finished execution and no execution that began without
a recorded end; otherwise it is **unknown with a reason**, never zero. "Execution
attempts" counts worker invocations; "operation attempts, all kinds" counts every
bounded operation and is not a measure of work.

Bounds: at most 64 runs, 5000 events per run, 5000 plan events. Exceeding a bound
is reported as a problem that invalidates the result, not silently sampled.

What cannot come from durable state stays **unknown**, as `{"value": null,
"unknown_reason": ...}`, never zero and never estimated:

- **Tokens**: no per-run token record exists. Always unknown here.
- **Cost**: known only if the plan stream recorded a figure for *every* budget
  consumption; a partial sum would understate and is reported unknown.
- **Manual relays and active human time**: nothing in the runtime journals them;
  they come only from the activity log.
- **Decision questions asked**: the collector counts resolved decisions delivered
  to attempts and units currently awaiting a decision. Whether a genuine #508
  question arose is for the operator to state in the outcome reason.

## Results

`benchmark emit` writes, under `<out>/<arm>/`, named `<date>-<case-id>.*`:

| file | content |
|---|---|
| `.activity.jsonl` | byte-exact copy of the log that was measured |
| `.runtime.json` | the raw runtime collection (zenchron_planned only) |
| `.json` | the derived result |
| `.md` | a concise human-readable report |

The result keeps the shape of `benchmarks/zenchron_planned/2026-09-08-issue-77.json`
(`schema_version, benchmark, corpus_version, mode, recorded_at, harness, case,
subject, measured, unmeasured, outcome, interpretation`) and adds
`instrumentation`, `validity`, `supervision`, `runtime`, `sources` and
`content_digest`. The schema version is **0.2**: existing members keep their
names, unknown values become `{value, unknown_reason}` objects, and 0.1 records
are historical and never rewritten. Additive changes bump the minor version.

- **Identity**: `content_digest` is the sha256 of the record's canonical (JCS)
  form with that member emptied. Equal content has an equal digest; any edit is
  detectable.
- **Immutability**: every file is created exclusively and read-only. Emitting
  over an existing result is refused, and a set that cannot be written completely
  leaves nothing behind.
- **Provenance**: instrumentation version, arm, base SHA, and a protocol digest.
  Pass `--protocol-digest` (the sha256 of the protocol frozen before either arm).
  Without it the digest is the explicit string `unfrozen-placeholder` with
  `protocol_frozen: false`.
- **Sources**: each raw input is referenced by path and sha256.
- **Invalid or incomplete runs are emitted, flagged**: `validity.valid: false`
  with reasons (an invalid or multi-operator log, no entries for the arm, an
  interval never stopped, no outcome, a malformed base revision, an unreadable
  run or truncated journal, a `zenchron_planned` arm with no runtime evidence, a
  `direct_agent` arm carrying Zenchron runtime evidence). The report leads with
  INVALID.
- **A failed or rejected outcome is as measurable as an accepted one**:
  `--outcome accepted|rejected|failed|abandoned` yields the same measurements.
  The outcome is stated by the operator and labelled `operator_stated`; the
  runtime's own candidate and PR revisions are in the runtime section.

```text
zenchron-engineering benchmark emit --log run.jsonl --arm zenchron_planned \
  --case-id issue-344 --repo bogdaniel/zenchron-engineering --base-sha <frozen-sha> \
  --outcome accepted --protocol-digest <sha256> --state-dir <state> --graph <graph-id> --out benchmarks
```

## Limits that must stay visible

- **n = 1.** One pair is a first observation. It does not establish the
  representative >= 2x target and must not be described as if it did.
- **Second-arm learning advantage.** With Zenchron first and `direct_agent`
  second, the human has already learned the task, which favours the second arm.
  Instrumentation cannot remove it; fresh workspaces and not carrying code,
  patches or provider sessions across arms only limit the contamination.
- **`zenchron_planned` is not `zenchron_explicit`.** Do not pool or relabel
  cohorts.
- **Sums are not a ratio.** This slice computes no leverage ratio.

## Synthetic example

`benchmarks/synthetic/` holds one invented pair, with `"synthetic": true`, a
banner in the report, and fictitious ids. It exists only to show the output
shape and is not evidence of anything. A test enforces that everything under
that directory declares itself synthetic and verifies against its own digest.
