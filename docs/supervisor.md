# The supervisor (`serve`)

```bash
zenchron-engineering serve
```

`serve` is the persistent local Engineering Runtime. It owns the scheduler,
drives every active run, observes each repository once on behalf of all of them,
and stays available to answer an operator while the work continues.

Before it existed, every run needed a terminal to stay alive in. Working three
tickets meant managing three of them, and closing one stopped the work inside
it — which is the specific reason a runtime built for this was not usable for
it.

## What it owns

- the scheduler and its leases
- active and queued `EngineeringRun`s
- worker processes and their cancellation
- the agent registry and its readiness
- global concurrency and the state-storage ceiling
- one shared forge observation stream per repository
- CI, pull-request and review observation, and feedback admission
- continuation and remediation scheduling
- provider capacity backoff
- status and log availability while work continues
- crash and restart recovery
- optional issue discovery, when an operator explicitly enabled it

## What it is not

It is not a second runtime. There is no second task database, no second
scheduler, no second journal and no second authority system. The durable
`EngineeringRun`s already in the store *are* the queue, and driving one is the
same `Reconcile` an operator command has always called.

It is also not a daemon that decides what work to do. Intake is explicit
operator submission; discovery is one optional policy an operator switches on.

## The control endpoint is an authority boundary

Anything that can submit an accepted request to `serve` can cause
`operator_trusted` coding agents to execute under the operator's local account,
against their authenticated subscriptions, with their repository credentials
owning publication.

That is not a convenience socket. The floor:

- **Local machine only.** There is no TCP listener anywhere in the
  implementation and no configuration member that could introduce one. A network
  control protocol is a separate design decision with its own authority
  analysis; it is not something this grows into by accident.
- **A Unix-domain socket** at `<state_dir>/serve.sock`, mode `0600`, inside a
  state directory that must itself be `0700`. The directory is checked *before*
  the endpoint is created, so a control path is never published into a directory
  other users can enter.
- **Both sides check.** The supervisor verifies the permissions when it opens
  the endpoint, and every client verifies them before connecting. A socket whose
  mode was widened afterwards is refused by both rather than trusted because it
  was safe when it was made.
- **Nothing authenticating is stored beside it.** There is no bearer token, no
  cookie and no shared secret in runtime or configuration state. Filesystem
  access to an owner-only path is the whole credential; a token in a file would
  only add something to leak.
- **Repository configuration cannot reach it.** The path is derived from the
  operator's state directory, and the in-repo layer has no member naming a
  socket, host, port or transport.
- **A request selects; it never introduces.** A submission names a repository
  the supervisor already governs and an agent the operator already configured.
  It carries no path, program, environment or credential.

`autonomy doctor` reports the effective mechanism and path, and FAILs — not
warns — when an existing endpoint is reachable by other users.

### Stale endpoints

A socket file left behind by a crash is removed only after a connection attempt
proves nobody is listening. Unlinking first would let a second supervisor steal
a live endpoint from the first. A path holding something that is *not* a socket
is refused outright rather than deleted: removing an operator's file on a guess
is not recovery.

### Platform support

The endpoint requires a platform where owner-only permissions can be verified.
Where they cannot, `serve` declines to offer a control path rather than
publishing one whose exposure it cannot describe — the same conservative choice
the controller-instance lock and the sandbox adapter already make. Everything else still
works there: runs are driven by the command that started them, exactly as before
`serve` existed.

### Path length

A Unix socket address is a fixed-size kernel field of roughly a hundred bytes.
A `state_dir` deep enough to exceed it is refused with that reason, rather than
failing with `bind: invalid argument`.

## Delegation

While a supervisor owns a state directory:

```bash
zenchron-engineering autonomy run issue 123 --agent codex
# submitted to the running supervisor; follow it with
#   autonomy status --text
#   autonomy logs RUN --follow
```

The command submits and returns. Your terminal is not the thing that has to stay
alive — that is the entire point.

`status`, `logs` and `events` are pure reads over durable state. They take no
lease and no ownership, so they work identically whether or not a supervisor is
running, and they are safe against runs another process is driving.

## Lifecycle: three different things

Collapsing these is how a controller restart becomes a fleet of failed runs.

```text
drain       stop accepting and starting work
            let already-started operations finish
            nothing is cancelled, nothing becomes terminal

shutdown    stop scheduling
            propagate cancellation into in-flight provider and verifier
            processes through the bounded machinery they already honour
            every run stays exactly as resumable as its journal says
            nothing is cancelled

stop-all    actually CANCEL the selected runs
            journalled per run, through the same single cancellation path
            `autonomy stop RUN` uses
            a running execution.invoke is interrupted and recorded
            `run_cancelled`, never `controller_shutdown`
```

Only the third is run cancellation. Signalling the process, or killing it, is a
shutdown: no `run.cancelled` is appended, and the journal is left exactly as the
last completed step wrote it.

```bash
zenchron-engineering autonomy drain      # needs a running supervisor
zenchron-engineering autonomy shutdown   # needs a running supervisor
zenchron-engineering autonomy stop-all --reason "end of day"   # works either way
zenchron-engineering autonomy stop RUN                          # one run
```

`drain` and `shutdown` are instructions *to* a supervisor; with none running
there is nothing to instruct, and the command says so rather than succeeding
silently. `stop-all` is operator authority over runs and does not depend on a
supervisor existing.

## Budgets: work, and waiting

```text
wall_limit_seconds          bounds the time the SYSTEM is working
                            pauses while the run waits on a person, an account,
                            a quota, or a provisioning step

lifecycle_deadline_seconds  bounds TOTAL elapsed calendar time
                            optional, absent by default

provider_inactivity_seconds bounds how long ONE provider invocation may go
                            without recognized provider progress;
                            finite always

attempt_wall_limit_seconds  bounds ONE physical provider attempt; its deadline
                            is min(this, remaining run work) at attempt start;
                            absent derives to wall_limit_seconds per run
```

`wall_limit_seconds` is cumulative across every attempt; the attempt limit is
per attempt. Status names which of the three per-attempt bounds ended an
attempt (`provider_inactivity`, `attempt_wall`, `run_active_work`) and never
advertises a retry or continuation the run can no longer admit. See
[configuration.md](configuration.md#budgets).

The third bound is the stall detector, and the run wall budget is not. A
provider subprocess being alive is not evidence of progress: a host that loses
its network keeps a coding CLI alive and silent, and until that was bounded the
run-wide wall budget was what discovered a dead provider — in the live run that
prompted it, 8h55m16s of "active engineering work" with zero external wait.
Reaching the window terminates the provider's process group, preserves its
transcript, and records `provider_no_progress`, which is a bounded retry.

A restart does not refund it. Silence is measured from the last moment output
was actually observed, and that datum is durable, so a controller that dies
mid-invocation hands its successor the REMAINDER of the window rather than a
fresh one — four minutes of proven silence leaves six, not ten. That is a
different authority from the execution budget, which separately charges the
abandoned interval in full. An attempt that was settled — observed, journalled
and classified — does get a fresh window, because that is what a bounded retry
is; the attempt ceiling is what ends it.

Ordinary progress is written at most once per quarter of the window. Progress that
arrives inside that interval is coalesced, not dropped: the newest observation
waits and is written at the next due point with the instant it was observed,
and a process that ends under a running controller writes its last observation
as it closes. So the datum is exact whenever the process was seen to end.

A structured tool opening or closing is not ordinary progress: it changes
whether inactivity is suspended, which is what status reports. Such an edge is
written within a sixtieth of that interval (2.5s for the default window)
instead of waiting for the next due point, so status stops claiming a
suspension shortly after the tool closes. Every write, edges included, still
starts at least that spacing after the previous one, and edges arriving inside
it coalesce into the newest state; without tool edges the rate is the ordinary
one above. That promptness holds while the process lives: an edge the process
outruns - it ends before the edge is written - collapses into the closing
write, which carries its newest observation with no tool open.

A controller that dies abruptly can lose the one observation still waiting.
The row then records that its recorder never closed, and only then does
recovering that attempt allow for it, by a fixed crash-recovery uncertainty
allowance `L` of half the window (300s for the default 600s): the silence
charged is the recorded silence less `L`, floored at zero. The implementation
and its review established a worst-case durable-progress lag approaching two
recorder intervals; with the interval `I` at a quarter of the window, the
evidence-backed bound is `L = 2I`, half the window. That is because an
observation made just after a write is written one interval later carrying its
own, older instant, and one made just before the next write is due is the one a
crash loses.

`L` is not an inactivity window. Every live invocation is supervised at the
configured limit and is killed with `provider_no_progress` after that much
silence, not a moment later; a successor's window is never longer than the
limit either. `L` only decides how much of an abandoned attempt's recorded
silence a recovery charges. So a successor is never refused on the recorder's
own lag, and never granted more than `L` beyond what the recorded silence
leaves, nor a fresh window. The recovery bound is the last recorded progress
plus the window plus `L`, and it is subordinate to every absolute bound: the
attempt deadline, the run's active work, the attempt and continuation ceilings
and cancellation are derived exactly as without it. It does not accumulate
across repeated restarts, because only a real observed write moves the datum
and a restart never does: every successor dispatched against the same datum
gets the same recovery bound.

Which restarts get the allowance is decided by durable state alone. A graceful
`shutdown` cancels the in-flight invocation, which then returns
`controller_shutdown` and is journalled and settled like any other outcome; a
`drain` lets it finish and settle. A settled attempt's successor starts from a
fresh datum, as every settled retry does, so neither gets an allowance. Only an
attempt nothing settled - the controller was killed, crashed or lost power
before it journalled the outcome, or a graceful shutdown's own unwinding was
cut short the same way - is recovered conservatively with the allowance, and
only if its recorder had not closed.

A durable write that never returns does not hold the invocation: once the
process ends, the recorder waits for its closing write for at most one write
interval, and not at all once the invocation's context has ended (cancelled,
the deadline passed, or the inactivity kill). A write still in flight then lands
late or never. Either way it is bound to its operation and physical attempt,
so it cannot touch a successor, and a row it never reached still says its
recorder is open, which is the crash-recovery case above. Store latency beyond the
interval can make the datum trail by more than `L`; that only ever errs
towards refusing, never towards granting.

Claude Code is the exception. Its window is measured by its own structured
events, which belong to one physical process, so each new physical Claude
attempt — after a restart too — gets the full window. What stays cumulative
across restarts for it is the execution budget, which still charges every
abandoned interval, and the attempt identity, which still advances; those are
what keep repeated restarts finite. See
[configuration.md](configuration.md#budgets).

An explicit diagnostic is classified instead, but only when the CLI itself said
so. Endpoint capacity (overloaded, 502/503/504) is `provider_unavailable`: a
wait that gives the attempt back. Transport loss (temporary DNS, timeout, refused, reset,
unreachable) is `connectivity_unavailable` (#380): it spends an attempt, but the
retry runs only after a durable, restart-safe backoff (`retry_not_before`, 30 s
doubling to a 5 min cap), and the run waits as `connectivity_backoff`, which is
external wait and not active work. The last attempt does not back off; it
stops as `<operation>_attempts_exhausted`. With the defaults that is one 30 s
wait for an execution (`max_execution_attempts` 2) and 30 s + 60 s for an
observation (3 attempts); the 5 min cap needs six or more attempts. The accounting is frozen in [the runtime spec](spec/runtime-v0.1.md#retry-dispositions).
A typed provider condition is read from the **terminal diagnostic surface**: the
bounded tail of the CLI's own diagnostic stream. The session rendering, where
model text and tool output go, is never consulted. A worker quoting an error, a
captured test log, or a documentation excerpt therefore cannot park a run on an
external wait that pauses accounting — a transcript is evidence, not an
assertion about the world.

Where the provider did state a condition and then went quiet, both facts are
kept: the classification is the condition the provider named, and the
termination cause records that the inactivity policy ended the process. Silence
is the weaker statement, so it does not overwrite the stronger one.

`autonomy status` prints the pair an
operator needs — time since recognized progress, and the window it is measured
against:

```text
progress   last 2026-09-18T09:14:02Z silent 4m12s inactivity limit 10m0s
```

While the operation is running, this line and the `lease` line are read from
the live operation row of that exact attempt, because recorded progress is a
row-only observation the journal never holds. They are observation, never an
input to any decision. If that row cannot be read or has moved to another
attempt, status falls back to the journal and says so: `progress (journal)`,
`lease (journal)`; JSON carries `operation.progress_source` (`row` or
`journal`). The lease heartbeat is not renewed yet (#180), so it may read as
the acquisition time for a healthy attempt's whole life; that alone is not
evidence the attempt is dead.

The live row trails the process by less than half the window, and only while
newer progress is waiting to be written. Silence of half the window or more is
therefore never an artefact of that lag.

While a structured Claude tool call is open, the inactivity kill is suspended,
and status says so instead of printing silence against a limit the watch is not
applying. The absolute deadline still bounds the attempt and stays on its own
line:

```text
progress   last 2026-09-18T09:14:02Z inactivity suspended — structured tool open since 2026-09-18T09:14:02Z (limit 10m0s resumes when it closes)
deadline   2026-09-18T09:44:02Z (attempt_wall)
```

JSON carries `operation.inactivity_suspension` (`active` or `unverified`),
`operation.inactivity_suspended_since`, `operation.deadline` and
`operation.deadline_bound`. The suspension is written when the tool opens and
cleared when the tool result, a new main-thread turn or the final result closes
it, and whenever the attempt ends. `tool_progress` heartbeats neither open nor
close it.

A suspension is a claim about one process, so status reports it as active only
while that process still owns the attempt: the row is on the suspension's
attempt, its lease is still held by the owner that recorded it, and that owner
is not provably dead by the same lock evidence a takeover uses (the lease
heartbeat cannot say, see above). A controller that crashed with a tool open
therefore stops being reported as suspended at once, and its silence is shown
as silence. When the evidence cannot decide, status prints the silence line and
marks the record `unverified` rather than claiming the suspension:

```text
progress   last 2026-09-18T09:14:02Z silent 12m3s inactivity limit 10m0s
suspension unverified — a structured tool was recorded open since 2026-09-18T09:14:02Z, but the owning controller's liveness cannot be established
```

No takeover, reclaim, new attempt or settlement inherits it, and the watchdog
never reads it: the kill is governed by the live process's own stream.

These answer different questions and were the same number until a live run
showed what that costs: a pull request reached its goal in eight minutes, waited
for review, and was killed at thirty with `run_wall_budget_exhausted`. Any pull
request not reviewed inside the execution budget died, so the review loop could
not survive human-paced review at any budget that also bounded runaway work.

Active time is derived from the journal, not from a stopwatch. An external wait
is a **state**: it opens at the `run.waiting` that declared it and closes only at
the next *disposition* event — a different wait, or a terminal one. It does not
close on an operation event, because the runtime writes `run.waiting` only when
the disposition or reason CHANGES:

```text
t0  run.waiting(goal_state_reached)     the human's turn begins
t1  operation.planned/before/after      a poll; still goal_state_reached, so no
                                        second wait event is ever written
t2  hours later, still waiting
```

An accounting model that closed the wait at `t1` would find no new wait event and
charge `t1..t2` to the execution budget — the original defect, returning after
the first polling tick. A restart re-derives the same number from the same rows.
Observation performed while waiting - re-reading the pull request, re-reading
the issue - is real work and is counted; only the idle gap between ticks is
excluded.

The set of waits that pause the clock is **closed and fail-closed**: a reason
nobody has classified spends the budget. A new wait pauses the clock only when
somebody decides it should, which is the safe direction for a bound whose whole
job is to end things.

### What a budget-ended run is holding

A budget still ends the run; nothing extends it. But completed material work
must not be silently made useless by that boundary (#203), so every terminal
failure whose reason is a budget boundary (`*_exhausted`) records, in the same
`run.failed` event, whether the run holds valuable material:

```text
verified_unpublished   execution-complete commit, assurance passed at it, no
                       pull request carries it
committed_unverified   execution-complete commit not yet verified
checkpoint             runtime-owned incomplete checkpoint commit (#54)
committed_unobserved   a runtime-owned commit candidate.commit made whose
                       observation or reassessment then failed (#402); the
                       revision and tree are that commit, never its parent
unproven_head          the candidate head moved to a commit no candidate.commit
                       attempt recorded as its own (#402); the revision and
                       tree are that head as observed. It is NOT a runtime
                       commit: author, message and parent prove nothing, so it
                       is never adopted, observed or assured, and only operator
                       release (#344) can resolve it: its next step is
                       operator_release, and it names no producing operation.
                       A runtime commit lost to process loss before any journal
                       record lands here too. A moved head takes precedence
                       over a recorded runtime commit; that commit's identity
                       then appears only in the journal (runtime_commit)
uncommitted            a succeeded producer's change the budget stopped before
                       candidate.commit; identified by producing operation, path
                       count and a content digest taken when the producer
                       returned (no Git write was made to compute it)
```

Each record binds the exact revision (and tree when known), names the next
lifecycle step and the budget reason it is blocked by, and, when the latest
attempt selected a retry or continuation that cannot be admitted (#328), that
successor and why. Its disposition is `held`: the material stays in the run's
candidate workspace, `gc` retains that workspace, and **nothing is granted** -
no attempt, continuation, active work, token or cost, provider deadline, Git
operation or publication. Carrying it forward is new, governed work.

The record is written once and read back from the journal, so a restart or a
changed configuration reports the same identity and disposition. It is also
sticky: a later re-settle of the same run keeps the original record. `autonomy
status` prints `held material`, and prints `none (no material result)` for a
budget failure that held nothing, so the two are never confused.

**The operator path, today.** `gc` never reclaims a held workspace, so its disk
is spent until an operator acts:

1. `autonomy gc --dry-run` reports every held workspace under `retained`
   (reason `run holds held material: …`) and their total under
   `held: {workspaces, bytes}`. `autonomy doctor`'s `state.storage` check points
   here.
2. `autonomy status RUN --text` names the material: kind, revision, tree, and
   for uncommitted work the producing operation and content digest. Inspect it
   in the workspace path shown there.
3. To keep the work, carry it forward as new governed work: a new run, or your
   own branch, reviewed like any other change. The runtime publishes nothing
   for you.
4. To discard it, delete `<state>/runs/RUN/candidate` by hand. There is no
   governed release yet, so this deletion is not journalled, and status keeps
   reporting what the run held. #344 tracks an explicit, journalled release that
   makes the workspace collectable.

## Concurrency

The ceiling is operator authority:

```json
{"supervisor": {"max_concurrent_runs": 3}}
```

**The default is one.** An operator who wants parallel work must raise it. The
scheduler enforces the same ceiling durably, so the bound holds across processes
and not merely across goroutines. A repository may tighten it and can never
raise it; where both `supervisor.max_concurrent_runs` and the older
`watch.max_concurrent_runs` are stated, the stricter value wins.

### Capacity classes

The ceiling is per **capacity class** (#85), decided transactionally by the
scheduler's durable acquisition:

| class | operations | ceiling |
| --- | --- | --- |
| work | every kind except the two below, including any kind added later | `max_concurrent_runs` (default 1) |
| observation | `source.observe`, `github.observe` | `max_concurrent_observations` (default 2) |

Observation reads the forge and journals what it saw; it invokes no provider or
verifier and mutates no candidate, remote or forge state. So with
`max_concurrent_runs = 1` and one run inside a long provider call, a run parked
on review still observes, discovers the review, and becomes **runnable** - and
takes the work slot only when it is released. A run holds at most one active
operation at a time, and the same durable acquisition enforces that too: no
process can observe a run beside the work another process is doing on it, and
a pass that finds a run another live driver is operating writes nothing to it.
That holds for every `Reconcile` caller - supervisor, watch, or an operator
command: when another owner holds one of the run's operations and may not be
taken over (it is alive, or its lease has not expired), the pass returns the
run unchanged with the reason `driven_elsewhere`, which is never journalled.

The supervisor starts at most `max_concurrent_runs + max_concurrent_observations`
turns at once. That bounds goroutines only: a turn whose next operation's class
is full is refused by the store and returns.

`status` shows the fleet as five mutually exclusive counts over nonterminal
runs: **working** (holds a work operation), **observing** (holds an observation),
**runnable** (holds nothing and has a pending work operation the scheduler could
lease), **waiting** (holds nothing and is not runnable) and **unavailable** (its
journal could not be replayed). They are read from durable operation rows, so
they are the same after a restart. Runnable uses the scheduler's own
eligibility test; the one approximation is liveness, which a read never probes,
so an abandoned lease counts as working or observing until a scheduler
reclaims it.

What the work ceiling bounds is **reconciliation work**, not specifically the
expensive part of it: assurance, commits and publication are work too, so the
bound is coarser than "N concurrent provider invocations". A slot is held for as long as the run
is being driven, which is usually several polling intervals: a tick is a
scheduling PASS that starts work and returns, so a run whose provider takes half
an hour keeps its slot across every pass in that half hour, and the remaining
slots stay available to anything an operator submits meanwhile. The alternative
- a pass that waits for everything it started - made admission as slow as the
slowest run and is what #202 records. If a deployment ever needs a finer bound - N expensive
operations rather than N runs - that is one more capacity class, not a
scheduler redesign.

## Shared observation

Several runs in one repository ask the forge the same questions. Left alone that
is N pull-request reads, N permission lookups for the same reviewer, and — when
the forge starts refusing — N callers each discovering that separately and each
continuing to ask.

The supervisor wraps the forge adapter so reads inside a short window are
answered once and fanned out, writes pass straight through and invalidate what
they changed, and a rate-limit refusal becomes shared backoff for the whole
repository. It is a decorator: no new persistence, no message bus, no second
normalization of anything.

What is shared is decided by **whom a fact describes**, not by whether it is an
error:

```text
describes the REPOSITORY          describes the FORGE        describes the CALLER
an answer, a 404, a 403           "retry after N seconds"     cancelled, deadline

cached for the window             becomes shared backoff      never shared
invalidated by our writes         survives our writes         never cached,
                                                              never adopted by a
                                                              joiner
```

The last column is the one that took three passes to get right. A run being
stopped must not answer its siblings' live questions with its own cancellation —
neither through the cache, nor by a sibling that happened to join its in-flight
request. A joiner that receives a caller-scoped error while its own context is
still live issues its own request instead, once.

## Restart

Recovery is replay. The supervisor holds nothing durable of its own: on start it
lists the non-terminal runs in the store and drives them, which is the same
thing it does on every tick. A restart therefore resumes eligible work without
creating duplicate logical runs, because run identity is derived from the
repository, the issue and the configuration digest rather than from anything the
supervisor remembers.

That is a claim a design can stop honouring the moment something starts being
kept in memory, so it is pinned as behaviour rather than left as intent: a
regression drives a run with one supervisor, constructs a second over the same
store with no handover, and requires the same run to resume with its agent
binding and base intact, no duplicate logical run, and no already-succeeded
operation performed again.

## Discovery

```json
{"watch": {"repositories": ["owner/name"], "label": "zenchron:auto"}}
```

Automatic issue discovery is one optional intake policy under the supervisor. It
is active only when an operator enrolled repositories for it, and new
configuration enrols none. The startup banner states which mode is in effect.

Under `serve` it is intake and **nothing else**. Discovery observes consent,
creates or adopts the durable run, and records withdrawal and credential waits —
then stops. The supervisor drives it, like every other run.

```text
standalone `autonomy watch`        under `serve`

discover -> claim -> DRIVE         discover -> claim
                                                 |
nothing else is running, so             the supervisor drives it
the controller is also the driver       with everything else
```

Collapsing those would give one process two independently capacity-bounded
advancement paths over the same runs: a freshly claimed run reconciled once by
discovery and again by the supervisor enumerating non-terminal runs, in a single
tick, with the operator's ceiling enforced twice over rather than once.

## Related documents

- [`product-architecture.md`](product-architecture.md) — where the supervisor sits
- [`agents.md`](agents.md) — the workers it drives
- [`running-multiple-tasks.md`](running-multiple-tasks.md) — the operator workflow
- [`troubleshooting.md`](troubleshooting.md) — endpoint and lifecycle symptoms
