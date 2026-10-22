# Local Control Plane read API and console

## Persistent objective workspace

`/objectives` lists durable plans alongside issue-backed intake. Selecting an
objective opens `/objectives/{plan-id}`, a bookmarkable read-side composition
with Summary, Plan, Execution, Decisions, Evidence, and Changes and PR sections.
Objective, plan and inbox plan pages are database-bounded before lifecycle replay.
The inbox pages through all plans, showing only pending approvals and unobservable
records on each page; an empty page is not proof that no later approval exists.
Planning identities with revision zero remain visible after reload, explicitly
without a stored proposal or approval controls. A positive revision whose document
is missing fails closed. Repository and issue identities label the objective list;
private objective prose is not copied into the durable list projection.
Summary uses the runtime's existing governing-plan lifecycle and WorkGraph counts,
preserving concurrent dimensions. It does not derive an objective authority state.
WorkGraph detail JSON adds optional `objective_plan_id`, `objective_plan_revision`
and `objective_plan_digest` together, from the same graph revision that produced
its unit projection. Native WorkGraph status also includes the existing
`objective_plan` reference. Strict schema consumers need the updated schema;
persisted formats and controller commands are unchanged. A conflicting displayed
revision or an unverifiable graph binding offers no objective decision/publication
action; navigation opens the graph's exact plan context instead.
It reconstructs context after reload from the existing plan and the WorkGraph's
explicit `objective_plan` binding; it creates no lifecycle record or execution
authority. Product-associated graph summaries expose optional `objective_plan_id`
links to this workspace. Graphs without that binding remain diagnostic graphs.
The composed page reads these owner projections separately, so concurrent writes
can make sections reflect adjacent durable observations rather than one atomic
cross-domain snapshot. It remains informational: plan approval binds the exact
revision and digest, decision resolution names the durable request, and
publication authorization binds the exact request ID and digest; the controller
revalidates each action. After a refusal, reload before acting again.

A missing graph is shown as unrecorded execution. Multiple matching graphs fail
closed; a graph bound to a different plan revision is explicitly marked. Run,
handoff, review and publication facts retain their independent identities.
Publication is not objective completion. The objective Plan section consumes the
existing controller preview and offers exact-bound approval, rejection and
revision through the same governed routes as the plan diagnostic page. A preview
whose revision or digest differs from the rendered page is refused locally
before actions are offered; the controller still validates every submitted
binding. Intake links return to the continuing objective workspace. Decision and
run diagnostics remain available. Typed WorkUnit decisions appear in the
objective Decisions section using the same cards and guarded resolution routes
as Action Required. Inbox cards link back to their explicitly bound objective.
Resolved holds disappear from durable reads after reconnect; resolution never
starts a bypass run. Recorded review findings and current review eligibility
are shown separately, alongside assurance and classified runtime failures.
The React objective page's Outcome panel (#629) is read-only and sits between the
plan inspection and the WorkGraph. It reads, from each fact's own record, the
independent review verdict and reviewer, the assurance gate state, and for every
producer run its candidate revision, assurance result, publication-authority
status and recorded pull request (linked to GitHub only for an exact `owner/name`
repository). A plan that is `completed` with no recorded pull request says "No
pull request yet" and names the recorded reason - a missing human authority, a
non-authorized publication authority, or that none is recorded - rather than
implying publication. Anything unrecorded stays "Not recorded"; it is never shown
as passed, failed or absent. The panel offers no action and grants no authority.
The Changes and PR section consumes outstanding publication requests through the
existing authority preview and authorization path. It names candidate commit,
tree and publication target before confirmation, preserves exact request binding,
and links recorded PR numbers to GitHub. A missing request offers no publication
action. A changed candidate or controller binding reloads the read preview;
responses for superseded preview identities are ignored. Authority reads refresh
on the bounded page cadence, replacing controls when request identity, digest,
contract, action or candidate changes. No mutation is retried.
This workflow composition does not
establish completion of the browser objective-to-PR experience or #448 acceptance.

Start the separate process against an existing runtime state directory:

```
zenchron-engineering control-plane --state-dir /absolute/path/to/state
```

The default listener is `127.0.0.1:8787`. `--listen` accepts only literal
loopback IP addresses (including `::1`). `--controller-root` overrides the
standard adopted-controller root when necessary.

For an isolated supervisor and Control Plane, set `ZENCHRON_CONTROLLER_ROOT`
to one absolute, owner-controlled directory for both processes. The variable
overrides the usual `~/.zenchron-adopted-controller` root for controller
commands as well; an unset value preserves the standard path. This separates
development adoption state from the operator's normal controller installation.

Every request requires `Authorization: Bearer <token>`. The process generates
32 random bytes, hex-encoded in `control-plane.token`, inside the owner-only
state directory. The file must be a regular file with mode 0600. Tokens never
appear in responses or startup output. To rotate, stop the Control Plane,
remove this file, and restart; existing clients must read the new token.

The read routes are GET. Browser mutations use the governed controller actions
described below; they never write SQLite directly.

| Route | Projection |
| --- | --- |
| `/v1/controller` | Controller consistency, serving, projection, role, admission, generation match |
| `/v1/fleet` | Existing fleet aggregates without run/plan collections; zero capacity means unknown |
| `/v1/runs?offset=0&limit=100` | Runtime run summaries; optional `status`/`source` filters apply before paging |
| `/v1/runs/{id}` | Runtime status, lifecycle times, operation progress and its source |
| `/v1/runs/{id}/events?after=0&limit=100` | Run-local event metadata |
| `/v1/runs/{id}/stream` | Per-run SSE: snapshot, then events (below) |
| `/v1/plans/{id}` | Plan summary, revisions and stage counts |
| `/v1/plans/{id}/detail` | Plan stages, dependencies, bindings, review verdicts and budget envelope |
| `/v1/plans/{id}/events?after=0&limit=100` | Plan-local journal metadata, including refused attempts; no payload/prose |
| `/v1/products` | Product (#476) summaries: identity, revision, repositories |
| `/v1/products/{id}` | One product's configuration and a SQL-paged association list (`graphs_offset`/`graphs_limit`); inspect each graph separately |
| `/v1/workgraphs` | WorkGraph (#472) summaries |
| `/v1/workgraphs/{id}` | One WorkGraph's units, frontier, independent-review state, each unit's full run detail and its own open #473 decisions |

### Product Workspace (#479)

`/v1/products/{id}` and `/v1/workgraphs/{id}` (and their console pages below)
are read-only aggregations over four existing, separately-owned projections -
Product (#476), WorkGraph (#472), EngineeringRun (#470/#392) and the typed
communication protocol (#473) - never a second truth store. A WorkGraph with
no product association yet simply has none; nothing is fabricated.

Open WorkUnit decisions show their durable request, frozen answer grammar and
exact subject. Browser resolution delegates to `Supervisor.ResolveDecision`
(#508), preserving actor admission, liveness, subject binding and immutable
resolution history. Plan human-decision gates remain distinct: they reference
the evidence and authority that satisfy them; resolving a WorkUnit question
does not manufacture plan-gate evidence.

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

### Visual design checkpoint

An authenticated, fixture-only replacement preview is available at `/design` on
this same listener. It includes Engineering Overview, Objectives Portfolio,
Objective Workspace and Action Required, with dark/light themes and mobile layouts.
Its simulated decisions never call the runtime. The current console and all
`/v1/` contracts remain supported during review; no legacy interface has been removed.
The accepted React/TypeScript application is being implemented at `/app` on this same listener.
It includes Overview, Objectives, exact plan workspaces, Action Required, Runs/history, WorkGraphs,
Products, agent observations, controller diagnostics and React token login. Existing governed stop
and plan rejection have real-controller integration proofs, including lost responses and process
restart. The reconciliation implements browser objective intake/revision/approval, typed decisions,
publication human evidence and run controls using the existing governed APIs below. Legacy retirement
requires independent review and operator acceptance; old pages remain operational during migration.
Build the embedded asset with `npm ci --ignore-scripts` and `npm run build` inside `frontend/` before
building Go. Production uses the Go binary alone, with no independent frontend server.
The additive read endpoints `/v1/plans?limit=50&offset=0&q=...` and
`/v1/plans/{id}/context?revision=...` expose sanitized source identities, canonical pending state
and exact historical decision receipts. A torn context read returns 409 `snapshot_changed`;
it grants no authority and is never retried implicitly.
See [the implementation batch](design/runtime-integration.md), [the design checkpoint](design/README.md) and
[the frontend consolidation inventory](design/frontend-inventory.md).

The same process also serves a server-rendered operator console on every
other path, behind the identical bearer token, listener and store this
document already describes - there is no second HTTP server, token lifecycle
or SQLite handle for it:

| Route | Shows |
| --- | --- |
| `/overview` | controller dimensions (durable consistency, serving, projection, role, admission), fleet counts by disposition and held material, plans |
| `/runs` | the fleet, filterable by `?status=active\|waiting\|failed\|completed\|cancelled\|held` and `?source=owner/repo#123` |
| `/runs/{id}` | one run's full detail: worker, candidate, controller identity, current operation (live vs. journal), held material, assurance, publication authority, authority request, budgets and attempts, and a paginated causal event timeline (`?after=`, via the same bounded `EventsPage`) |
| `/plans/{id}` | one plan revision's stages, dependencies, bindings, review verdicts and budget envelope |
| `/products` | every adopted Product |
| `/products/{id}` | the Product Workspace: one product's repositories, current configuration, and every associated WorkGraph's units/runs/review-state/open-decisions on one page |
| `/workgraphs` | every adopted WorkGraph |
| `/workgraphs/{id}` | one WorkGraph's engineering-oriented drill-down: units, frontier, independent-review state, per-unit run detail and open decisions |

`control-plane` prints one line on startup, `control plane: http://ADDR/app (sign in
with the token in STATE/control-plane.token)`. It names the token file and never
prints the token.

Plan approval needs no copied digests (#639). A plan proposed from the CLI
(`autonomy plan issue N`) or from the browser is the same objective plan. It is
listed under Objectives and Action required; Review & approve binds the exact
revision, plan digest and live assignment digest. Only a plan created through
the browser intake with a product chosen is associated with a product, so only
that plan's WorkGraph appears under `/products/{id}`; the Products page links to
Action required for the rest. `TestOperatorPlanApprovalJourney` (opt-in, real
Chrome, `ZENCHRON_OPERATOR_REHEARSAL=1`) proves it for a CLI-shaped proposal.

Two blocked states are shown, never bypassed. With no controller role held,
Review & approve is disabled and the page says "Controller role not held". A plan
proposed under an earlier effective configuration is refused by the controller;
the refusal's explanation (`configuration_changed: ... propose the issue again
under the current configuration`) is returned in the action result's `detail`
(up to 400 characters, plan refusals only) and shown beside the refusal.
`TestOperatorHeldPlanVisibility` and `TestOperatorUnobservedControllerVisibility`
cover both in a real browser.

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

## Browser workspace actions (#479, #398, #508; parent #471)

The workspace adds Objectives, Plans, Decisions, Agents and Settings beside
Overview, Products, WorkGraphs and Runs. Light/dark presentation, responsive
cards, keyboard focus and technical drill-down use the existing embedded Go
templates and plain JavaScript. No frontend package manager or backend service
has been added.

An objective starts from an existing issue in a controller-enrolled repository.
The browser selects a configured worker, optional default profile preference,
and template; explicit template or reasoning-stage profile preferences take
precedence, and the controller still resolves eligibility and assignments.
The operator may choose deterministic compilation when no planning model is
wanted. The controller owns the budget envelope. Generation grants no execution
authority. Review the proposed revision, stage dependencies, assignments,
blockers and budgets before approving it.
Approval names both the plan digest and the live resolved assignment digest.
A changed revision, worker binding or controller generation is refused.

| Route | Existing controller/domain path |
| --- | --- |
| `GET /v1/workspace/options` | Controller-owned repositories, agents, profiles, templates and budget envelope |
| `GET /v1/plans/{id}/preview` | Live `PlanService.View`, including exact assignment digest |
| `POST /v1/objectives` | Bounded AI planning intake; an existing plan requires an explicit revision |
| `POST /v1/issues` | Opens one GitHub issue on a governed repository from the operator's typed title (≤200 bytes, one line) and description (≤3000 bytes) with the runtime's existing forge credential; the body ends with a footer naming the dashboard and operator. It is the only way an objective starts from words instead of an existing issue. A request reference makes a lost reply recoverable through `GET /v1/operator-action-receipts/{id}`, whose `issue-create` receipt names the repository and issue number, so a resend never opens a second issue. It grants no planning, execution or publication authority |
| `POST /v1/plans/{id}/approve` | Exact revision/digest/assignments approval |
| `POST /v1/plans/{id}/revise` | Existing proposal path, exact predecessor revision/digest |
| `POST /v1/decisions/{id}/resolve` | `Supervisor.ResolveDecision`, configured operator provenance |
| `GET /v1/runs/{id}/authority` | Current `PendingAuthorityRequest`, or no request |
| `POST /v1/runs/{id}/authorize` | Existing `Authorize`, exact request id/digest and approve/reject evidence |
| `POST /v1/runs/{id}/pause` and `/unpause` | Existing operator pause journal and scheduler admission |
| `POST /v1/runs/{id}/resume` | Existing scheduler reconciliation request; policy and capacity remain authoritative |
| `POST /v1/runs/{id}/refresh` | New source generation for an eligible issue run; plan/WorkGraph-bound work requires a governed revision |

Stop, reject and lifecycle actions use fixed routes and existing controller
commands. Run detail explains when an action is unavailable; it does not offer
a manual retry that bypasses scheduler budgets. Every mutation requires
Bearer authentication, exact Host, same Origin, strict bounded JSON and a
same-request measured controller binding under the held controller role. No
browser-supplied actor is accepted. Unsupported verbs remain unreachable.
Publication evidence is recorded through the run's own engine; partial
objective candidates with deferred publication have no authorizable request.
Human evidence still cannot replace independent review, assurance or policy.

An action is sent once. A lost reply is an unknown outcome until durable state
settles it, never permission to retry a budget-spending planning invocation.
Re-read Plans after uncertain intake; an existing plan is not silently replaced.
The Action Required view pages WorkGraphs and links to all plans and runs; a
page with no decisions does not claim that the entire fleet has none.

Refreshes run at a bounded cadence without overlapping requests. Unchanged
elements retain identity; focused edits and open confirmation dialogs defer
updates rather than changing the target underneath an operator. Hidden tabs
pause refresh. HTTP errors, controller disconnection and paused updates show
stale/unavailable state. A successfully refreshed durable snapshot is never
proof that a disconnected controller is executing work.

Automated browser checks run against real isolated SQLite, controller sockets
and HTTP, using external provider/forge boundary fixtures:

```sh
ZENCHRON_BROWSER_E2E=1 go test ./cmd/zenchron-engineering -run '^TestBrowserWorkspace$' -v
```

The dependency-free Chrome runner requires installed Chrome and Node.
`ZENCHRON_E2E_CHROME` selects the browser binary and
`ZENCHRON_E2E_ARTIFACTS` selects the screenshot/result directory. It uses a
temporary browser profile and does not connect to production state.
Automated fixture approvals do not count as real operator approval. Final
acceptance requires an existing live issue, genuine human plan/publication
approval and operator confirmation of the full browser workflow.

## Original governed actions (S4a, historical boundary)

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

### Exact operator delivery recovery

Creation/revision/resume/refresh may carry `X-Zenchron-Operation-ID` (a canonical UUID). After normal
auth/CSRF checks the API hashes the exact escaped path and bounded JSON wire body and passes the
correlation through the existing governed socket command. The controller appends successful delivery
observations to the existing plan/run journal before replying.
`GET /v1/operator-action-receipts/{id}?digest=SHA256` returns only a hash/subject-checked historical
receipt under bearer authentication. Receipt-write failure is `unknown`, never proof of refusal;
missing receipts cannot establish non-execution. Legacy callers need no header.
[Complete semantics, privacy and paired-build rollback limits](design/action-recovery-review.md).
