# Running several tasks at once

`autonomy run issue N` drives one run in your terminal. That is the wrong shape
for three tickets and one afternoon: the terminal becomes the thing that has to
stay alive, and three terminals are three chances to close the wrong one.
`serve` is the persistent local runtime that owns the work instead.

It is not a second runtime. There is no second task database, no second
scheduler, no second authority system and no queue of its own: the durable runs
in the existing store ARE the queue, and driving one is the same reconcile an
operator command has always called. What `serve` adds is ownership, concurrency,
a shared forge observation path and a lifecycle.

## Start the supervisor

```bash
zenchron-engineering serve --agent codex
```

```text
zenchron-engineering serve
  state directory   /Users/you/.zenchron/state
  control endpoint  /Users/you/.zenchron/state/serve.sock
  mechanism         unix domain socket, owner-only (0600) inside the owner-only state directory
  agent             codex (codex_cli, operator_trusted)
  repositories      owner/name
  discovery         disabled (explicit operator submissions only)
```

The control endpoint is an operator-authority boundary, not a convenience
socket: anything that can submit an accepted request to it can cause
`operator_trusted` coding agents to execute under your local account, against
your authenticated subscriptions, with your repository credentials owning
publication. So the floor is enforced before the listener exists. There is no
TCP listener and no configuration member anywhere that could introduce one. The
socket lives inside the owner-only state directory and is itself mode 0600 — two
independent statements of the same boundary. Nothing authenticating is stored
beside it: the only credential is filesystem access to an owner-only path,
because a token in a file is a token that can be read, copied and leaked and
would buy nothing the directory permission does not already provide.

A socket left behind by a crash is removed only after a connection attempt
PROVES nobody is listening. A second `serve` on the same state directory is
refused with `a supervisor is already listening on this state directory's
control endpoint`, before it starts competing for leases rather than after.

`serve` governs the repositories you enrolled in `watch.repositories`, or the
one repository this invocation targets when you enrolled none. A control request
selects among those; it can never introduce one.

One supervisor drives EVERY configured agent. Each run carries its own durable
agent binding, and the supervisor builds an adapter per repository-and-agent
pairing, so `#101` can be worked by `codex` while `#102` is worked by `claude`
in the same process. Driving both with whichever worker `serve` happened to be
started with would be a silent provider handoff on every tick, which is exactly
what a run's agent binding exists to prevent.

A submission naming an agent you did not configure is still refused: a control
request selects among your configured workers and can never introduce one.

## Submit work

```bash
zenchron-engineering autonomy run issues 101 102 103 --assign 101=codex --assign 102=codex
zenchron-engineering autonomy run issue 104            # delegates when a supervisor is running
```

`run issues` is the batch form. It requires a running supervisor to own the
work; without one it refuses with `starting several issues at once needs a
supervisor to own them` rather than opening three drivers in one process.
Submissions are ordered deterministically, each is one durable run, and one
failed submission reports itself without stopping the rest.

Assignment is explicit. There is no router choosing which model suits which
ticket: that is a decision an operator makes, and a system that made it silently
would be spending your subscriptions on its own opinion. `--assign N=AGENT`
resolves against your own registry, so a request can select among the agents you
configured and can never introduce an executable, a trust mode or a credential.
`--agent` supplies the default for issues with no explicit assignment.

`run issue N` with a supervisor running creates the run and hands it over:

```text
submitted to the running supervisor; follow it with `autonomy status --text` or `autonomy logs run-... --follow`
```

and exits 10. The `--agent` you named is frozen into that run's provenance, and
the supervisor continues it with that worker rather than with its own default.

## Read the control room

```bash
zenchron-engineering autonomy status --text
```

```text
ZENCHRON ENGINEERING

Supervisor: running   Workers: 2 / 3 active

ISSUE    AGENT      STATE              BRANCH / PR              ELAPSED    REASON
#101     codex      active:assurance.go PR #418                 4m12s
#102     codex      waiting            zenchron/run-2f9c...     3m50s      awaiting_authority
#103     codex      completed          PR #419                  22m1s      merged
```

`Workers: 2 / 3` is a fact about your configuration, not about this process:
the capacity is the operator-authorized ceiling. Active work sorts first. This
view is a pure projection over the same durable state every other read uses — no
network call, no lease, no lock — so it is safe while the supervisor drives the
same runs. `autonomy status RUN --text` still explains one run, and `autonomy
logs RUN --follow` still shows one worker talking; both work whether or not a
supervisor is running, because both are reads.

Following several workers is several readers:

```bash
zenchron-engineering autonomy logs run-101... --follow &
zenchron-engineering autonomy logs run-102... --follow &
```

## The ceiling

The default concurrency is 1. An operator who wants parallel work must raise it:

```json
{"supervisor": {"max_concurrent_runs": 3, "poll_interval_seconds": 60}}
```

`supervisor.*` and `watch.*` are combined by taking the STRICTER value — fewer
runs, longer interval — so stating one can never loosen the other, and a
repository that tightens the watch bound still tightens the effective one. A
repository may lower the ceiling in `.zenchron.json` and may not raise it.

Each tick drives the active runs oldest first, capped at the ceiling, so a
long-queued run is not starved by newer submissions. The scheduler enforces the
same ceiling durably; the supervisor bound exists so the process does not start
work it cannot lease. A per-run failure is REPORTED inside the tick report and
never propagated: one run whose forge call failed, whose provider is unavailable
or whose workspace is broken must not take its siblings down, and that isolation
is the whole reason several tasks share one process.

Each run gets its own EngineeringRun, candidate clone, branch, agent binding,
budgets, artifacts and pull-request lifecycle. Nothing is shared between runs
except the store, the ceiling, and one forge observation stream per repository:
runs in one repository share a single multiplexed observer rather than each
polling GitHub independently, so ten runs on one repository do not cost ten
times the rate-limit budget.

## Worked example: two tasks, one moving base

Issues 101 and 102 both branch from `main`, and 101 touches a file 102 also
edits.

```text
t0   serve --agent codex, supervisor.max_concurrent_runs = 2
t1   autonomy run issues 101 102
       run-A  branch zenchron/run-A  candidate <state_dir>/runs/run-A/candidate
       run-B  branch zenchron/run-B  candidate <state_dir>/runs/run-B/candidate
t2   both reconcile: contract, execution, commit, offline assurance
t3   both settle at awaiting_authority
t4   autonomy authorize run-A req-... --approve
     autonomy authorize run-B req-... --approve
t5   base.integrate (pre-publication) -> rebase; push; PR #418 (A), PR #419 (B)
t6   PR #418 is merged. run-A observes it and completes with reason `merged`.
t7   run-B's next pass: base.integrate fetches origin and sees origin/main has
     moved past its pinned base, and its candidate is not built on it.
     run-B is published, so the strategy is MERGE FROM BASE, never a rebase and
     never a force-push.
```

Two outcomes from t7:

Clean merge. The integration commits, the journal records
`candidate.base_integrated` with the strategy, the new base revision, the commit
and the tree, and the run continues: the candidate moved, so prior evidence is
stale by construction and assurance is re-derived against the new exact tree
before authority is evaluated again. `autonomy status run-B --text` shows the
new base and candidate revisions.

Conflict. The integration aborts, the workspace is restored, and the failure is
the typed class `base_integration_conflict`. It is routed to bounded provider
remediation — the same execution worker, invoked with the conflict as a typed
finding — under the run's remediation budget. No force-push happens after
publication under any outcome: a runtime force-push is not a strategy, because
the published head is what a reviewer already read. If remediation resolves the
conflict, the run continues exactly as in the clean case. If the remediation
budget is exhausted first, the run fails with an `..._attempts_exhausted`
reason, and the operator's move is a new generation:

```bash
zenchron-engineering autonomy run issue 102 --agent codex --new-generation
```

One consequence to know: a failed integration records no new metadata baseline
even though the fetch that preceded it did move remote-tracking refs, so a
conflicting integration can leave the run stopping on
`workspace_integrity_violation` on a later pass rather than resuming in place.
That is fail-closed by design — there is no silent resume over a metadata
mismatch — and the resolution is again a new generation.

## Provider capacity is shared

Several workers on one subscription share one allowance. When the provider
refuses, the run does not burn budget discovering that again:

- `execution_provider_quota` — the allowance is spent and returns on the
  provider's own schedule. It is a capacity WAIT, not an engineering attempt: no
  reasoning happened, no candidate moved, no evidence or authority changed, and
  the run's remediation budget is not consumed by it.
- `execution_provider_rate_limited` — the provider asked to be called less
  often. Kept separate from quota because the action differs: repeated rate
  limiting means the configured concurrency is above what that account
  tolerates. Lower `supervisor.max_concurrent_runs`.
- `execution_provider_account_unavailable` — the provider refused at its own
  account boundary before any reasoning happened. Repair the account, then
  `resume`.

An unrecognized provider diagnostic stays `unknown` and stops the run for a
human rather than being guessed into a retry or an infinite wait.

## Lifecycle: three different things

```bash
zenchron-engineering autonomy drain                       # stop taking and starting work
zenchron-engineering autonomy shutdown                    # stop scheduling, unwind in flight
zenchron-engineering autonomy stop-all --reason "..."     # actually cancel every live run
```

| Command | Effect | Cancels runs |
| --- | --- | --- |
| `drain` | accepts no submissions and starts nothing new; work already inside a reconcile finishes | no |
| `shutdown` | stops scheduling and unwinds in-flight work through the cancellation providers and the Docker sandbox already honour; every run stays exactly as resumable as its journal says | no |
| `stop-all` | cancels every non-terminal run, journalled per run through the same single cancellation path `stop RUN` uses | yes |

`drain` and `shutdown` are instructions TO a supervisor and are refused with `no
supervisor is running on <state_dir>; start one with
zenchron-engineering serve` when there is none — saying so is better than
silently succeeding. `stop-all` works either way: with a supervisor it is
delegated so the process holding the leases performs it, without one it runs
through the same cancellation path locally. A drain is not reversible except by
restarting the supervisor, which is deliberate: un-draining silently would make
the instruction meaningless.

Sending SIGINT or SIGTERM to `serve` is a shutdown, not a fleet cancellation. No
`run.cancelled` is journalled and the durable journal is left exactly as the
last completed step wrote it.

## Automatic intake, if you want it

Discovery is one optional intake policy, not the reason the runtime exists. It
activates only when `watch.repositories` is non-empty, and then only issues
carrying `watch.label` (default `zenchron:auto`) are picked up. Removing the
label from a live run's issue withdraws consent and the run waits at
`opt_in_removed`. `autonomy watch` runs the discovery loop on its own, without
the supervisor, for an operator who wants intake and nothing else.

## See also

- [running-work.md](running-work.md) — one run in detail
- [github-feedback.md](github-feedback.md) — reviews reaching a live worker
- [configuration.md](configuration.md) — `supervisor`, `watch`, `storage`
- [troubleshooting.md](troubleshooting.md) — endpoint and supervisor failures
- [supervisor.md](supervisor.md), [agents.md](agents.md), [product-architecture.md](product-architecture.md)
- [architecture.md](architecture.md), [spec/runtime-v0.1.md](spec/runtime-v0.1.md), [../README.md](../README.md), [../ROADMAP.md](../ROADMAP.md)
