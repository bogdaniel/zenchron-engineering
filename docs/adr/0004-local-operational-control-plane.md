# ADR-0004: Local Operational Control Plane

- Status: Accepted
- Date: 2026-10-02
- Issue: #392 (S0 — this ADR and the reconciliation it requires). Parent tracker
  for the implementation slices that follow it.

## Context

`docs/construction-principles.md` lists a hosted control plane under YAGNI, and
`README.md`'s non-goals say not to prematurely build a dashboard. Both were
correct when written: no concrete operational cost had yet been paid for their
absence, and `autonomy status`, `logs` and `events` were enough to run one or
two things by hand.

That is no longer the case. Running several concurrent `EngineeringRun`s
through `serve` (#63) and diagnosing a stuck or failed one now routinely
requires reconstructing state across several disjoint surfaces in one sitting:

- `autonomy doctor --text` and `autonomy agents --text` for preflight and
  worker readiness;
- `autonomy status --text` for the fleet, then `autonomy status RUN --text`
  for one run's `next action`, budget and progress lines;
- `autonomy events RUN` for the durable journal, to see which typed event
  actually fired;
- `autonomy logs RUN` for what the worker said, which is a pointer into the
  SQLite-backed operation rows (`runtime.db`, see ADR-0002 "Operation
  persistence") when progress is live, and into the raw transcript artifact on
  disk when it is not — `docs/supervisor.md`'s `progress_source: row|journal`
  split exists because the two do not agree during the lag between them.

`docs/troubleshooting.md` documents this as the correct per-symptom procedure,
which is exactly the problem: a correct procedure that is five separate
commands against four separate surfaces (CLI output, live operation rows,
journal events, raw transcript) **is** the dashboard-shaped gap the earlier
non-goals were written to defer, now paid for repeatedly rather than
hypothetically. The guidance was right to defer it before this evidence
existed; it is wrong to keep deferring it after.

That evidence does not license building `docs/vision.md`'s **Engineering
Control Plane** early. That later product is explicitly organization-level —
"spanning repositories, policies, providers, budgets, identities, assurance
systems, and engineering intelligence" — and `ROADMAP.md` places it at M6
(#72), gated on M2–M5 evidence this repository does not yet have. Nothing in
the diagnosis cost above is a multi-repository, multi-identity or shared-policy
problem; it is a single operator, on one machine, unable to see state their own
already-running `serve` already holds. Conflating the two would mean building
#72 eight milestones early under a different name.

## Decision

A **local Control Plane** is an operational projection and control surface
over the existing Engineering Runtime — not a new authority, not a new store,
and not #72.

```text
#392  local Control Plane          #72  organization Control Plane
one operator, one machine          an organization, several identities
projects THIS serve's state        spans repositories and shared policy
no new authority                   RBAC, shared policy, identity federation
M1/M2-adjacent tooling              M6, gated on M2-M5 evidence
```

It reuses the runtime exactly as `autonomy status`/`logs`/`events` already do,
and adds nothing to what may be authorized:

- **A separate local process.** It is not folded into `serve`; `serve` remains
  the only process owning the scheduler, leases, and active runs. The console
  is a client of that state, started and stopped independently.
- **Read-only runtime/store projections for reads.** Fleet status, one run's
  detail, journal events and transcript references are read the same way
  `autonomy status`/`events`/`logs` read them today — durable store and journal
  reads that take no lease and no ownership — rendered as a projection rather
  than re-derived as a second source of truth.
- **The existing control socket plus controller identity proof for
  mutations.** Any action that is not a pure read goes through the same
  owner-only `<state_dir>/serve.sock` endpoint `docs/supervisor.md` already
  documents, limited to that endpoint's closed command vocabulary as it exists
  in the runtime today (`submit`, `status`, `agents`, `drain`, `shutdown`,
  `stop`, `stop-all`, `ping`, `controller.snapshot`, `plan-approve`,
  `plan-reject`, `plan-revise` — `runtime/control_endpoint.go`), through the
  same `ControlSession`/`ProveControllerIdentity` mechanism the runtime uses
  for controller succession today (`runtime/control_session.go`). No new
  mutation path, no new authority boundary, and no privilege the CLI does not
  already have. This ADR names no verb the socket does not already accept:
  a later action becomes console-visible only once its owning runtime
  semantics exist on the socket (for example, an explicit journalled release —
  #344), never because the console or this ADR names it first.
- **Go stdlib `net/http` + `html/template` + `embed`, plus small plain
  JavaScript.** Consistent with the three direct dependencies this module
  already carries (`go.mod`): nothing web-framework-shaped is added to reach
  this milestone's acceptance.
- **No Node, bundler, or frontend framework initially.** A build toolchain
  this repository does not otherwise need is exactly the kind of premature
  infrastructure `docs/construction-principles.md`'s YAGNI section already
  warns against; it is deferred until a concrete UI requirement cannot be met
  without one.
- **Per-run SSE using `EventsAfter`.** Live event streaming for one run reuses
  `SQLiteOperationStore.EventsAfter` (`runtime/journal.go`) — the same cursor
  contract `autonomy events` already relies on — pushed over Server-Sent
  Events rather than polled, instead of a new subscription mechanism.
- **Loopback-only by default, with a bearer token in the state directory at
  mode `0600`, plus same-origin enforcement on every mutating request.**
  `serve.sock` authenticates by filesystem access alone —
  `docs/supervisor.md` is explicit that nothing authenticating is stored
  beside it, because an owner-only Unix-domain socket path already is the
  credential, and a token file there would only add something to leak. The
  console's HTTP listener is a different transport with a different threat
  surface: a loopback socket a browser can reach does not inherit that
  filesystem authentication, so this ADR deliberately introduces a new,
  owner-only local credential for *this* surface, rather than claiming to
  reuse an existing-socket argument that does not apply to it. That token is
  generated locally, is not configurable from repository state, lives inside
  the `0700` state directory at `0600`, and is never exposed in a URL, a log
  line, or an API payload; S1 owns its exact filename and rotation.

  Read requests require that token. Mutating requests require it too, and
  additionally enforce same-origin/CSRF protections, per #392's binding
  decision that mutating requests enforce same-origin protections and
  server-side authority: a browser's claimed origin, or the token's presence
  alone, is never treated as sufficient, and authority/eligibility for a
  mutation is revalidated server-side — through the same control-session
  identity proof above — never inferred from what the browser sent.
- **JSON Schema for public API contracts.** The console's HTTP responses are
  described by schemas validated with the `santhosh-tekuri/jsonschema`
  dependency already in `go.mod`, consistent with every other canonical
  contract in `schemas/`, rather than an ad hoc, undocumented response shape.

### Reconciliation of existing source-of-truth documents

This ADR is the resolution the acceptance criteria require, not a rewrite of
the documents it reconciles:

- `README.md`'s non-goals keep "do not prematurely build a dashboard"
  unchanged, with a note that the local operational console this ADR
  authorizes is a narrow, read-mostly projection over the existing runtime —
  not the kind of dashboard product the non-goal was written to defer, and not
  the #72 organization Control Plane.
- `docs/construction-principles.md`'s YAGNI list keeps "a hosted control
  plane" out of scope unchanged — it names *hosted*, which #392 is not; this
  ADR is the concrete authorization for the local one, with the first-slice
  bounds stated above rather than an open-ended build.
- `ROADMAP.md` gains #392 as M1/M2-adjacent operational tooling over the
  already-implemented runtime, explicitly distinguished from #72 at M6. See
  "The boundary between #392 and #72" there.

No runtime behaviour changes as a result of this ADR. It authorizes and bounds
the first implementation slice; it does not implement the API or the UI, and
it does not reopen #72 or choose organization identity/RBAC.

## Consequences

### Positive

- Diagnosis that currently costs five commands across four surfaces becomes
  one projection, without inventing a second source of truth: every number it
  shows still traces to the same store and journal reads `autonomy` already
  performs.
- The mutation boundary does not grow. Reusing the control socket and
  controller-identity proof means the console can request `drain`, `stop`, or
  `plan-approve` with exactly the authority an operator's own CLI already has,
  never more.
- No new runtime dependency is incurred to reach first-slice acceptance:
  stdlib HTTP/templating/embed and the two contract libraries already vendored
  are sufficient.

### Costs

- A second local process to start, stop, and secure, with its own credential
  (the bearer token) to provision and rotate — cost the existing `serve.sock`
  explicitly avoided by relying on filesystem permissions alone.
- A second HTTP surface is a second thing `autonomy doctor` must learn to
  report on truthfully (reachable, bound to loopback, token present and
  `0600`) the same way it already reports on the control endpoint, or the
  floor this ADR states is undocumented rather than enforced.
- The read projection must stay honestly read-only under load: a console that
  silently acquires a lease, a lock, or scheduling side effects while "just
  showing status" would be the second scheduler `docs/supervisor.md` already
  refuses to become.
- The boundary against #72 has to be re-asserted by every later slice, not
  just this ADR: an organization-identity field, a cross-repository view, or a
  shared-policy control added "while we're in there" is #72 built early under
  cover of #392, which is exactly what this ADR's non-goals refuse.

## Invariants

- The local Control Plane projects runtime state; it does not become a second
  source of truth for it.
- Every mutation the console can request, the operator's CLI could already
  request, through the same control socket, the same closed command
  vocabulary, and the same controller-identity proof. The console introduces
  no socket verb the runtime does not already define.
- Read requests against the console's HTTP listener require the local HTTP
  credential (the state-directory token). Mutating HTTP requests require that
  credential and additionally enforce same-origin/CSRF protections; authority
  and eligibility are revalidated server-side and are never inferred from the
  browser's claimed origin or from token possession alone.
- No organization identity, cross-repository scope, or shared policy is
  introduced under #392. That is #72, and reopening it is out of scope here.
- A build toolchain (Node, a bundler, a frontend framework) is added only when
  a concrete UI requirement cannot be met with `html/template` + `embed` +
  plain JavaScript, not in anticipation of one.
