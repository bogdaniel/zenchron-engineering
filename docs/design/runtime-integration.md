# React integration — reconciliation batch

PR #594 integrates main at `e6d90b9454ebb125cc1252eb363e7a147fec9a75`, preserving both interfaces
until independent review and operator acceptance. The accepted identity/composition remains intact.
No production restart, authorization, publication, merge or cutover occurred.

## One application and existing authority

`/app` contains Overview, Objectives, Objective Workspace, Action Required, Runs/history, WorkGraphs,
Products, installed/recorded agents, Controller health and React login. One Go listener delivers the
embedded React bundle and the existing `/v1/` API. The temporary legacy URLs and `/design` checkpoint
remain. There is no production Node server, scheduler, authority system or UI-owned lifecycle.

Main added objective intake, assignment preview, plan approval/revision, typed decisions, publication
human evidence and run controls. React now consumes those existing governed paths. Earlier batch
statements describing a stop/rejection-only HTTP allowlist are historical and superseded.

| Browser interaction | Existing governed request / binding |
| --- | --- |
| Objective intake | `POST /v1/objectives`; governed repo/issue, installed agent/profile/template, deterministic choice |
| Plan approval | `POST /v1/plans/{id}/approve`; immutable revision/digest, current assignments digest and live controller |
| Plan revision | `POST /v1/plans/{id}/revise`; exact current revision/digest, one new unapproved proposal |
| Plan rejection | `POST /v1/plans/{id}/reject`; exact pending revision/digest and live controller |
| Typed decision | `POST /v1/decisions/{id}/resolve`; existing exact immutable request and outcome grammar, controller revalidates |
| Publication approval/rejection | `POST /v1/runs/{id}/authorize`; exact request/digest binds candidate/tree/contract/action; records human evidence only |
| Stop/pause/unpause/resume/refresh | Existing `/v1/runs/{id}/{action}` and controller role/scheduler/kernel paths |

Plan approval is not acceptance or publication. Publication authorization records human evidence;
the kernel still checks policy, independent evidence and eligibility. No frontend operation merges.
Read-only preview may omit controller binding by design; React pairs it with a fresh controller read,
then POST carries the binding which the server proves on that same action request.

## Shared components and state

- `components.tsx`: semantic shell elements, panels, badges, disclosures, bounded pagination and native dialog.
- `api.ts`: sole bearer transport, bounded cancellation/deadlines and strict shared decoding.
- `operations-api.ts`, `workspace-api.ts`: typed operational/planning/receipt DTOs using that transport.
- `governed-action.tsx`: reusable exact local confirmation, one submission, unknown-outcome closure,
  no automatic retries, receipt reconciliation and predictable result focus.
- `planning.tsx`, `human-requests.tsx`, `run-controls.tsx`: existing controller actions composed into
  workspace/intake/inbox/run views; installed inventory reuses controller workspace options.
- `history.tsx`: exclusive cursor and 50-event DOM bound. WorkGraphs decode full observed graph,
  calculate actual dependency layout and render a 50-node window with mobile selected inspection.

Observed server DTOs, local route/filter/selection and frozen confirmation results have distinct
owners. None of these components computes authoritative lifecycle or grants authority. Reusable
presentation satisfies repository interface gate §6(c); transport decoding is the existing §6(b)
external boundary. No new dependency, backend interface, SQL migration or exception was added.
Delivery-only journal event additions are documented in the focused recovery review.
The integrated main carries its existing store migrations; use its documented rollback semantics.

Visible observations refresh every 20 seconds, age to stale after 30 seconds, and pause while hidden
or confirming. Identity attestation is independent of endpoint reachability. Pending requests are
not actionable decisions. Historical receipts retain their exact context after pending rows disappear;
focus moves to status/receipt, never automatically to another request. Unknown publication outcomes
close both approve and reject until the exact durable receipt is observed.

## Additive read projections

- `/v1/plans` is bounded at 50, with search and canonical lifecycle separately from revision state.
  Action Required has an accessible pager including mobile and preserves `?offset=` in selected URLs.
- `/v1/plans/{id}/context` adds canonical lifecycle and an existing durable objective-bound WorkGraph,
  with an explicit exact-plan binding mismatch. No association is inferred from repository similarity.
- `/v1/workspace/requests?limit=&offset=&graphs_offset=` reduces at most 50 existing graph projections
  per window and pages 50 human/publication request rows independently. `graphs_has_more` prevents
  treating a request-free graph window as the complete inbox. Unit/history reads remain unbounded
  inside each existing projection; this is documented and requires an index if measured cost grows.
- `/v1/decisions/{id}/resolution` exposes only durable request ID, typed outcome and resolution time.
- `/v1/runs/{id}/authority-receipts?request_id=&digest=` exposes exact human decision metadata,
  scanning the existing journal in 500-event pages. No actor, notes or event payload is published.
- GET run and SSE snapshot use the same read helper for journal-owned operator pause state.

All use the existing bearer API. POST keeps Host/Origin/JSON validation and role/controller proof.
No useful API is deleted with a former template consumer. Creation/revision/resume/refresh now retain
original delivery references across navigation and reconcile exact correlated journal receipts; no
receipt or mismatched read remains unknown. These observations grant no retry or authority entitlement.
See [the focused recovery review](action-recovery-review.md) for schema/rollback and crash-gap limits.

See [the current review evidence](batch-02-review.md) and [complete parity/removal inventory](frontend-inventory.md).
