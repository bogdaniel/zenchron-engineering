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

All routes are GET-only:

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

Response contracts are `schemas/control-plane-*.schema.json`, including the
shared error shape. Empty lists are arrays. Internal errors become a fixed
`read_failed` code, never raw exception text. Event payloads, artifact bodies,
provider environment, goals, free-form diagnostics and private transcripts
are excluded by explicit response types. Identifiers and status metadata
remain visible to the authenticated local operator.

The database opens with SQLite `mode=ro` and `query_only` on each connection.
Startup neither creates a database nor migrates its schema. A database whose
schema differs from this binary's schema must be upgraded by the runtime.
The HTTP process has no mutation methods or operator-action routes. Only
`controller.snapshot` is sent to the controller's existing local endpoint.
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
