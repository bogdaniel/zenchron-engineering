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
the ownership lock and the sandbox adapter already make. Everything else still
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

What the ceiling bounds is **whole-run reconciliation**, not specifically the
expensive part of it. A run being reconciled holds a slot whether it is invoking
a coding agent or performing a cheap forge observation, so the bound is coarser
than "N concurrent provider invocations". In practice a tick's cost is dominated
by execution and assurance, and the rotation above means a slot is never held
across ticks. If a deployment ever needs the finer bound - N expensive
operations rather than N runs - that is a per-operation-kind concurrency class
in the scheduler, and it is deliberately not built on speculation about which
kinds would need one.

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

## Restart

Recovery is replay. The supervisor holds nothing durable of its own: on start it
lists the non-terminal runs in the store and drives them, which is the same
thing it does on every tick. A restart therefore resumes eligible work without
creating duplicate logical runs, because run identity is derived from the
repository, the issue and the configuration digest rather than from anything the
supervisor remembers.

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
