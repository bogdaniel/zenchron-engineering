# React implementation — batches 01 and 02

The visual identity and primary composition are frozen. `/app` implements the four main
experiences and the remaining existing browser read workflows through the same Go listener.
The operational replacement still needs operator acceptance before legacy deletion.
No production work was approved, published or merged.

## Current capability map

| Destination | Source and behavior |
| --- | --- |
| `/app#/overview` | Controller, SQL-paged plan identities, 50 run summaries, canonical fleet aggregates; overlapping activity scopes, pending/actionable distinction, unknown capacity |
| `/app#/objectives` | Full-index repository/issue/plan search, 50-row pages, explicitly local request filter |
| `/app#/objectives/{id}[/{revision}]` | Exact plan revision, proposed/governing distinction, revision selection, requirements versus bindings, gates/evidence references, review subjects, change preview, budgets and journal history |
| `/app#/attempts/{id}` | Metadata history for refused attempts with no executable revision; typed refusal detail remains `autonomy plan show` |
| `/app#/actions/{id}/{revision}` | Frozen exact-subject plan rejection, fresh controller/revision review, durable historical receipt, fail-closed reconciliation and predictable focus |
| `/app#/runs` | Full-fleet disposition/held/source filters before paging; local keyword filter is explicitly scoped to the loaded page |
| `/app#/runs/{id}[/{after}]` | Lifecycle, operation/heartbeat versus journal, worker and controller provenance, candidate/contract, budgets, evidence/publication obligations, held material, diagnostics and governed stop |
| `/app#/workgraphs[/{id}]` | Actual dependencies, authoritative frontier/counts, selected unit, admitted handoff, independent review, readiness hold and unresolved human request context |
| `/app#/products[/{id}]` | Product repositories/configuration references; SQL-paged associated graph summaries with exact navigation |
| `/app#/agents` | Recorded agent/provider/model/trust bindings per loaded run; installed registry/readiness stays in the existing CLI |
| `/app#/controller` | Separate identity/reachability/role/admission/projection/consistency dimensions, findings and diagnostic guidance |
| `/app` without a valid credential | React login verifies the existing bearer token before setting the existing cookie; preserves destination; no legacy JS/CSS dependency |

Objective prose is private in the existing HTTP contract. Source issue/repository identifies a plan;
private objective/rationale/provider prose is not republished as a title. A PlanStage is not a WorkUnit.
No plan-to-WorkGraph association is fabricated. Publication, plan approval and decision resolution
remain on their existing governed CLI paths: the frozen HTTP allowlist permits only stop/rejection.
These limitations must be included in operator acceptance, rather than hidden by fixture controls.

## Shared implementation and state ownership

| Owner | Responsibility |
| --- | --- |
| `components.tsx` | Head, Panel, Badge, Link, Empty, native Disclosure/Dialog, exact Binding, ControllerFacts and named Pagination |
| `pages.tsx` | Overview, portfolio and plan workspace compositions |
| `main.tsx` | One shell/navigation/theme; route-local observation ownership, cancellation, refresh and stale/error handling |
| `history.tsx` | Shared run/plan metadata timeline, exclusive cursor, 50-record window and back navigation |
| `runs.tsx`, `workgraphs.tsx`, `system.tsx` | Execution, graph/product and system compositions; reusable factual inspector |
| `decisions.tsx`, `stop-run.tsx` | Local frozen confirmation, one submission, uncertain-outcome reconciliation and historical feedback/focus |
| `login.tsx` | Existing token authentication, focused failure recovery and destination preservation |
| `api.ts`, `operations-api.ts` | One typed integration layer: sole bearer transport, wire validation, bounded reads and existing action contracts |
| Tokens/layout/components/runtime CSS | One dark/light design system; runtime inspector rules are scoped to avoid changing the approved fixture composition |

Observed DTOs, local selection/filter/confirmation state and server action results have separate
owners. No UI transition grants authority. Native React/TypeScript and the existing esbuild pipeline
remain the only frontend dependencies; no new dependency was added in batch 02. Presentation
satisfies repository §6(c); HTTP integration is §6(b). There is no new backend interface, runtime,
scheduler, policy system, workflow database, authoritative state machine or production Node server.

The app and exact code-only bootstrap assets are public so React can render login. API observations
still require Bearer authentication. Mutation checks still require exact Host/Origin, JSON shape,
controller binding and the controller's own role/authority. Fixture modules remain gated; `/design`
forbids runtime connections. Its measurements are never loaded by React.

Visible views reobserve every 20 seconds. Hidden tabs and open confirmations suspend this polling.
After 30 seconds a snapshot becomes historical and review is disabled. Refresh failure retains only
that view's historical observations. Browser receipt time is not controller liveness. Historical
journal browsing is separately refreshed and uses its own cursor; heartbeat is not a journal event.

## Genuine decision and stop workflows

Review re-reads the exact plan or run and the live controller. Plan confirmation binds immutable
revision/digest, protected action and consequence; stop names the immutable run, current observed
context and controller binding. Full technical bindings remain disclosed on demand. Confirmation
footers stay visible while their bodies scroll. The server independently rechecks eligibility,
proves the expected controller on the connection and executes inside the existing role/plan lock.

Each confirmation submits once. A lost/invalid response is an unknown outcome; neither action is
resent automatically. Reconciliation reads the exact durable receipt or cancellation. A failed read
stays closed. Known results remain visible even if a follow-up read fails. Resolved requests leave
the pending list; the exact route remains historical and focus moves to the receipt. Cancel returns
to the exact opener. No next request is silently selected.

`TestEngineeringOSPlanRejectEndToEnd` uses the actual composition, serve control handler, owner-only
socket, real role and SQLite/hash-chained journals. It tests normal and discarded-response variants
for rejection and stop, with one durable effect each. Controller identity is self-measured by the
compiled test binary; its declared source/build adoption is isolated test input, not production CI
attestation. The forge/provider boundary stays offline; no worker or scheduler is run.

The test closes/reopens reader/composition/socket/role, then starts a separate OS controller process,
crashes it, and starts a new process against preserved state. Fresh browsers recover rejection and
cancellation without resending. Wrong digest/controller, unattested build, released role and
unreachable pending requests refuse without a decision. This proves actual backend behavior on
isolated test data; it is not an acceptance claim for an operator's adopted production generation.

## Additive read contracts and compatibility

Batch 01 added `/v1/plans` and `/v1/plans/{id}/context`. Batch 02 adds:

- `GET /v1/fleet`: existing canonical aggregates; omits unbounded run/plan collections. Capacity
  zero means unknown to this reader, and is never rendered as a zero concurrency ceiling.
- `GET /v1/plans/{id}/events?after=&limit=`: indexed, bounded, plan-local journal metadata,
  including attempt-only identities. No event payload, private reason or operator note crosses.
- Optional `status` and `source` filters on `/v1/runs`, applied before offset/limit. Shared source
  matching now prevents issue zero from selecting runs that have no issue. A regression first
  reproduced that defect, then passed through the shared rule used by both API and legacy renderer.

Plan history uses the existing stream index/canonical event reader; strict JSON Schemas are registered.
Context still rejects torn reductions with `snapshot_changed`. No persisted format/SQL migration,
controller policy or action allowlist changed. Rollback is a code/asset rollback with preserved state.

Client DOM stays bounded to 50 records/nodes. WorkGraph topology uses actual dependencies; cycles
are reported, never drawn as fabricated order. Desktop graphs scroll within their panel, mobile uses
a native picker plus one selected unit and inspector. Event history has a bounded scrolling viewport.
Product association and plan identity pages are SQL bounded. Run filtering/fleet aggregates reuse the
existing cached fleet projection and still inspect the fleet internally; wire bounds do not prove
bounded backend replay cost. Production-scale latency and full accessibility qualification remain open.

## Evidence and acceptance boundary

The maintained browser entry is `scripts/check_design_preview.mjs`; helpers cover actual login,
React read boundaries and explicitly labeled operational HTTP fixtures. Real-controller browser
checks are invoked by the Go integration test through `check_live_decision.mjs`.

Captures cover 1440/1920 desktop and 390 mobile, both themes. Actual Go captures say isolated test
data. Mock operational captures say HTTP FIXTURE and are presentation evidence only. The browser
checks cover hundreds of index rows, a 10,000-event contract, 120 units, focus containment/return,
exact-subject refusal, escaped reports, invalid cursors/dependencies/subjects, stale/error states,
contrast, names and reflow. These are focused accessibility and structural visual regression proofs,
not a complete WCAG certification or an approved pixel-diff baseline.

Rendered inspection caused a focused revision: history scrolling, earlier run controls, mobile unit
picker/one selected node, readable full-width identity values, and a scrolling navigation drawer.
The primary approved Action Required composition was preserved.

See [validation](validation.md), [batch 02 review evidence](batch-02-review.md) and the
[legacy-removal inventory](frontend-inventory.md). Old screenshots under `batch-01` are historical;
new screenshots/results belong to their tested source head. No old template, legacy CSS/JS,
presentation route or template test has been retired. Removal requires operator acceptance after
parity review. No production plan was approved, published or merged.
