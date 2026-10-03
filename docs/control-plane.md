# Local Control Plane read API and console

Start the separate process against an existing runtime state directory:

```
zenchron-engineering control-plane --state-dir /absolute/path/to/state
```

The default listener is `127.0.0.1:8787`. `--listen` accepts only literal
loopback IP addresses (including `::1`). `--controller-root` overrides the
standard adopted-controller root when necessary.

Every request requires `Authorization: Bearer <token>`. The process generates
32 random bytes, hex-encoded in `control-plane.token`, inside the owner-only
state directory. The file must be a regular file with mode 0600. Tokens never
appear in responses or startup output. To rotate, stop the Control Plane,
remove this file, and restart; existing clients must read the new token.

The read routes are GET (the two governed POST routes are in "Governed actions" below):

| Route | Projection |
| --- | --- |
| `/v1/controller` | Controller consistency, serving, projection, role, admission, generation match |
| `/v1/runs?offset=0&limit=100` | Runtime fleet run summaries |
| `/v1/runs/{id}` | Runtime status, lifecycle times, operation progress and its source |
| `/v1/runs/{id}/events?after=0&limit=100` | Run-local event metadata |
| `/v1/runs/{id}/stream` | Per-run SSE: snapshot, then events (below) |
| `/v1/plans/{id}` | Plan summary, revisions and stage counts |

Limits are 1–500. Event pages return `next_after` and `has_more`; pass
`next_after` as the next exclusive `after`. SQLite fetches at most `limit+1`
rows. This cursor is the run's `sequence`, **not** the fleet journal's
`global_sequence`. There is no fleet change feed in S1. Run-list offsets are
not a stable cross-request snapshot during concurrent runtime changes.

### Per-run event stream

`GET /v1/runs/{id}/stream[?limit=1-500]` is a Server-Sent Events stream for
one run, behind the same Bearer token (so a browser `EventSource`, which cannot
send that header, is not a client). Each frame is `id: <sequence>` plus one
`data:` JSON object validated by `control-plane-run-stream.schema.json`:

- `snapshot`: on a fresh connect only, the same `run` projection as
  `GET /v1/runs/{id}`; its `id` is the run's latest `sequence`.
- `event`: the same metadata as `/events`, in `sequence` order.
- `heartbeat`: liveness only; its `id` is the last delivered `sequence`.
- `stale` (`reason: read_failed`): a read failed after the stream opened.
  The server closes the stream; reconnect to resume.

Resume with `Last-Event-ID: <n>` (preferred) or `?after=<n>`: no snapshot is
sent, and events with `sequence > n` replay in LIMIT-sized pages. A resume
position beyond the run's latest `sequence` is refused with `400
invalid_page`. Read failures before the stream opens are ordinary JSON errors.

The fresh-connect cursor is read before the snapshot. An event that commits
between the two may therefore be reflected in the snapshot and still arrive
as an `event` frame. That duplicate is permitted; dedupe by `sequence`. An
event is never skipped.

There is no "live" boundary frame marking the end of replay, and only journal
events are streamed: a change to an operation row without a new event (for
example, heartbeat or progress timestamps) is not pushed. Re-read
`GET /v1/runs/{id}` for that state. Concurrent streams are not capped, which
is acceptable on the loopback, token-gated listener.

#### Measured read cost

`BenchmarkStreamReadCost` in `controlplane/stream_bench_test.go` measures the
stream against a real-shaped journal. The journal has one run of 100,000 events
(a repeating `operation.planned`, `before` and `after` cycle with full
`RunOperation` payloads over 50 operations, plus `run.waiting`), and 100 other
runs of 40 events each.

The fleet and the run's first 400 events go through `AppendEvent`. The rest
of the run is inserted in one transaction, with the real sequence, hash
chain, `EventDigest` and canonical document, so `Status`'s full `Reduce`
verifies them like appended rows. Only `state_before` and `state_after` are
format-only sha256 values. `AppendEvent` itself took 31.7 ms per append at
run length 400, and that cost grows with run length, so 100k appends would be
quadratic.

Run it with:

```
go test ./controlplane -run '^$' -bench BenchmarkStreamReadCost -benchmem -benchtime 5x
```

The results below are from an Apple M3 (darwin/arm64) with go1.27.1, 5
iterations each:

| Operation | Time/op | Memory/op | Allocs/op |
| --- | --- | --- | --- |
| Fresh connect (cursor + `Status` snapshot + first drain) | 3.50 s | 1.97 GB | 33.9 M |
| `Status` alone | 3.75 s | 1.97 GB | 33.9 M |
| `LatestSequence` alone | 84 µs | 672 B | 19 |
| Resume from sequence 50,000 (100 pages of 500) | 397 ms | 229 MB | 2.0 M |
| Steady-state poll, nothing new | 59 µs | 1.9 KB | 48 |

Fresh connect is dominated by `Status`, the full `Reduce` of all 100k events
that the S1 `GET /v1/runs/{id}` already pays. A cached run snapshot is #96's
scope, not this route's.

Replay and polling are bounded. A page is at most 500 rows, and a request for
501 is refused. A steady-state poll allocates a fixed 48 times; the benchmark
fails above 1,000. `TestStreamReadQueriesAreIndexSeeks` pins both stream
queries to an index range seek. Before that fix, the queries left `plan_id`
unbound, so SQLite searched only `(stream_kind, run_id)` and sorted the
remaining rows in a temporary B-tree on every page. A poll cost 17.1 ms,
`LatestSequence` 14.2 ms, and the 50k resume 3.45 s.

Response contracts are `schemas/control-plane-*.schema.json`, including the
shared error shape. Empty lists are arrays. Internal errors become a fixed
`read_failed` code, never raw exception text. Event payloads, artifact bodies,
provider environment, goals, free-form diagnostics and private transcripts
are excluded by explicit response types. Identifiers and status metadata
remain visible to the authenticated local operator.

The database opens with SQLite `mode=ro` and `query_only` on each connection.
Startup neither creates a database nor migrates its schema. A database whose
schema differs from this binary's schema must be upgraded by the runtime.
The HTTP process never writes the store or journal. Reads send only
`controller.snapshot` to the controller's existing local endpoint; the two
governed actions below send `stop` and `plan-reject`, and nothing else.
A missing live observation is unknown; a generation disagreement is explicit,
and an unstable controller observation cannot claim a generation match.

Run-list and plan projections currently reuse the runtime fleet projection,
which may replay more state internally than the bounded HTTP response holds.
S1 bounds the SQLite event-page query as well as its response. It does not
introduce an independently maintained projection cache.

## Console

The same process also serves a server-rendered operator console on every
other path, behind the identical bearer token, listener and store this
document already describes - there is no second HTTP server, token lifecycle
or SQLite handle for it:

| Route | Shows |
| --- | --- |
| `/overview` | controller dimensions (durable consistency, serving, projection, role, admission), fleet counts by disposition and held material, plans |
| `/runs` | the fleet, filterable by `?status=active\|waiting\|failed\|completed\|cancelled\|held` and `?source=owner/repo#123` |
| `/runs/{id}` | one run's full detail: worker, candidate, controller identity, current operation (live vs. journal), held material, assurance, publication authority, authority request, budgets and attempts, and a paginated causal event timeline (`?after=`, via the same bounded `EventsPage`) |

A browser cannot attach `Authorization` on a plain navigation, so the console
accepts the same token via a cookie instead: open any console page, paste
the token from `control-plane.token` into the sign-in form, and the page's
own JavaScript sets the cookie and nothing else - the token is never placed
in a URL, a query parameter, or a log line. The console's own live-refresh
script polls this page's `/v1/*` routes above with that same token as an
`Authorization` header; it introduces no additional read surface.

Event payloads and free-form run/plan diagnostic text (`reason`, replay
`error`) are not rendered, for the same reason API's own response types
exclude them: they are unbounded prose, not a stable projection.

## Governed actions (S4a, #398)

This section is the frozen S4a specification: the design reviewed on #398
("Design for review: S4 governed socket-backed operator actions") with the
maintainer's decision "S4a approved with one change" applied - `drain` is
dropped, because it changes live admission and journals nothing, so an
ambiguous reply could not be settled from durable history. Changing anything
here is a new review, not an implementation detail.

### Allowlist

Exactly two actions, each a fixed route mapped to a fixed socket command.
There is no `/{verb}` passthrough and no input field names a command.

| Route | Body | Socket request (the CLI's, plus `expected_controller`) | Binding |
| --- | --- | --- | --- |
| `POST /v1/runs/{id}/stop` | `{controller_binding}` | `stop`, `run_id` | the run id |
| `POST /v1/plans/{id}/reject` | `{controller_binding, revision, digest, note?}` | `plan-reject`, `plan_id`, `revision`, `digest`, `note` | plan id + revision + digest |

Not in S4a, each for a stated reason: `plan-approve` (the control plane cannot
show or bind the assignment set an approval authorizes; S4b needs a read-only
`plan.view` socket observation first), `plan-revise` (a long, budget-spending
provider invocation with no idempotency key), `drain` (journals nothing - see
above), `shutdown` and `stop-all` (end the only process that executes actions;
a bulk cancel with no per-target binding), `resume`/`refresh` (not socket
verbs), `submit` (intake, not a control). Operator attribution of UI actions in
the journal is the deferred #392 gap: `serve` records its own resolved operator.

### Controller identity

`/v1/controller` carries `controller_binding`: `runtime.ControllerBuildBinding`
of the live snapshot's build - the digest of kind, version, source revision,
source tree and measured binary sha256. It is empty for an unattested or
unobserved controller. Every action POST echoes the binding its page
rendered, and the control plane forwards THAT value, never a freshly observed
one, as the socket request's `expected_controller`.

`serve` checks it on the same connection that carries the action
(`governedControl`): it refuses `controller_unattested` when its own measured
identity is unattested, `controller_mismatch` when its binding differs, and
`unsupported_command` for any verb other than `stop`/`plan-reject`; otherwise it
executes the verb inside `ControllerRoleLease.WithAuthority`, so a released role
refuses with `role_not_held`. Nothing is journalled by a refusal. A request
without `expected_controller` - every CLI request - is handled exactly as
before. `ControlResponse.code` carries the typed refusal so no client matches
prose; it also types `run_terminal` (#443) and `plan_refused` (any
`PlanService` refusal).

### HTTP boundary

Before any route runs, a POST must pass, in order, refusing with no socket
dial and no write:

1. `Authorization: Bearer <token>` (constant-time). The console cookie is never
   accepted for a POST. → 401 `unauthorized`
2. `Host` equals the listen address (DNS rebinding). → 403 `forbidden_host`
3. `Origin` equals `http://<listen address>`, and `Sec-Fetch-Site`, when sent,
   is `same-origin`. → 403 `forbidden_origin`
4. `Content-Type: application/json`. → 415 `unsupported_media_type`

Open the console at the exact listen address (`http://127.0.0.1:8787`, not
`localhost`), or actions are refused by 2-3. Bodies are capped at 8 KiB, decoded
with unknown fields refused and exactly one JSON value (→ 400
`invalid_request`). Unknown routes are 404. No CORS headers are ever sent.

### Exactly once, and where each guarantee lives

One process-wide mutex serializes actions in ONE control-plane process. Under
it, each action:

1. pre-checks its durable effect - stop: the run is `cancelled`; reject: a
   `plan.rejected` event for that exact revision and digest exists
   (`runtime.DecisionEventFor`, the CLI's lost-reply reconciliation). If
   present the result is `already_applied` and nothing is sent. A failure of
   THIS read is the only bare `500 read_failed`: nothing was sent.
2. narrows: unknown run → 404 `run_not_found`; unknown plan revision → 404
   `plan_not_found`; a revision that is not the latest one awaiting a decision
   → 409 `not_awaiting_decision`; a digest that is not that revision's → 409
   `digest_mismatch`; an empty binding → 409 `controller_unattested`. A stale
   value is refused, never silently refreshed.
3. re-observes the controller exactly as the page did and refuses with 409
   `controller_not_actionable` (detail: the reason) unless it is reachable,
   serving, attested, holding the role and matching the durable generation.
4. sends ONE request.
5. maps the answer. A reply lost after the request was written
   (`runtime.ErrControlReplyLost`) is never resent: durable state is re-read,
   and the result is `applied` if the effect is there, otherwise `unknown`.
   Once a request may have been sent, a failed re-read never becomes a bare
   error: the result keeps its outcome, omits `state`, and `detail` says the
   durable state could not be re-read. The console shows any 5xx error body as
   "outcome not confirmed", never as refused.

Which guarantees hold where:

| guarantee | enforced by | scope |
| --- | --- | --- |
| controller binding equals serve's own measured, attested identity | `serve` (`governedControl`) | every client |
| executed under the controller role (`WithAuthority`) | `serve` | every client |
| only `stop` / `plan-reject` reachable with a binding | `serve` and the HTTP allowlist | every client |
| a governed reject decides only a revision still awaiting a decision (`PlanService.RefuseUnlessAwaitingDecision`, under the plan lock): a second reject, or a reject after a concurrent approval, appends nothing → `not_awaiting_decision` | `serve` | every client |
| digest, validation verdict, governing order (`PlanService.decide`); a completed/failed run is never cancelled (#443, `run_terminal`) | `serve` / runtime | every client, CLI included |
| serving, generation match and work admission at the moment of the action | control-plane re-observation (step 3) | per control-plane process |
| duplicate stop answered `already_applied` without a send | control-plane mutex + pre-check | per control-plane process; a second process's stop is idempotent in `CancelRun` anyway |
| Bearer / Host / Origin / content type | control-plane HTTP boundary | per control-plane process |

The CLI's own decisions are unchanged: an ungoverned request (no
`expected_controller`) may still approve a revision it rejected, and
`PlanService.decide` itself still does not deduplicate.

| outcome | HTTP | meaning |
| --- | --- | --- |
| `applied` | 200 | `serve` applied it (or the lost reply was settled by re-read) |
| `already_applied` | 200 | the durable effect already existed; nothing sent |
| `refused` | 409 / 404 | `code` says why; nothing applied |
| `unreachable` | 503 | the socket could not be reached; nothing sent |
| `unknown` | 504 | sent, reply lost, effect not found: re-read before retrying |

Every result is `{action, target, outcome, code?, detail?, state?}` where
`state` is the target's projection re-read from the durable store after the
attempt (`control-plane-run` / `control-plane-plan-detail`), never the request
echoed back. Contracts: `schemas/control-plane-action-request.schema.json`,
`schemas/control-plane-action-result.schema.json`.

### Console

An action button is rendered only when the observed controller is reachable,
serving, attested (non-empty binding), holds the role and matches the durable
generation, and the target is eligible (run not terminal; revision shown as
`proposed`/`unapproved`). Otherwise the page states why. The control plane
re-checks all of it before sending (step 3 above); `serve` itself enforces the
binding, the role and, for a reject, that the revision still awaits a decision. A button opens a native `<dialog>` naming the exact target (run id,
or plan id + revision + full digest), the controller generation and binding,
and the consequence in one sentence; only its confirm sends, once, with the
Bearer header. The dialog then shows pending, and the outcome with the
resulting durable state; it never retries, and closing it is blocked while a
request is in flight.
