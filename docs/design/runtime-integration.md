# React implementation batch 01

The visual gate is accepted across desktop/mobile, dark/light and negative states. Its identity and
primary composition are frozen. This is the first operational implementation batch, **not complete
frontend parity, production approval or authorization to remove the old console**.

## Implemented scope

| Destination | Real source and behavior |
| --- | --- |
| `/app#/overview` | Independent controller, 50-plan index and 50-run reads; overlapping activity scopes, partial-count labels, human request summary and execution observations |
| `/app#/objectives` | SQL-paged plan/source-context index, repository/plan/issue search across the index, request filter explicitly scoped to the loaded page, exact workspace navigation |
| `/app#/objectives/{plan-id}` | Canonical immutable plan detail; proposed versus governing revision, selectable dependency stages, role/profile/agent/provider/trust distinctions, budget consumption, review subject and historical decision receipt |
| `/app#/actions/{plan-id}/{revision}` | Pinned revision/digest, consequence and restrictions, independent live-controller inspection, fresh review reads, existing governed rejection, historical receipt and reconciliation |
| `/app#/controller` | Durable generation, live endpoint, live identity, serving, role, generation match, admission and durable consistency as separate facts |

Objective text is private in the existing API. The portfolio therefore names repository/source issue
and plan identity; it does not expose objective/rationale/provider prose or invent readable intent.
A plan is not a WorkGraph. No declared plan-to-WorkGraph association is exposed here, so the workspace
explicitly marks that integration unavailable instead of presenting PlanStages as WorkUnits.
Throughput/trends/cycle time/capacity ceilings remain unavailable. Full plan activity, standalone
runs/history, Products, WorkGraphs, installed agent inventory, diagnostics and stop remain migration
items supported by the retained console/CLI. The four React destinations are functional slices,
not claims of complete capability consolidation.

## One delivery pipeline and reusable components

`frontend/` has one pinned React/React DOM/TypeScript/types/esbuild manifest and lockfile. This is
necessary build infrastructure for the explicitly requested React/TypeScript application; there is
no new router, state framework, test framework or production server. `npm ci --ignore-scripts` and
`npm run build` produce the checked-in generated `assets/design/react.js`. The existing Go listener
embeds it and the authoritative shared CSS. Production needs only the Go binary.

`/app` and `/design` reuse the same authenticated HTML and CSS. Only the entry module differs.
The former unaccepted read-only prototype adapters were removed; the retained `/design` module is
fixture-only and cannot connect to runtime. React never imports fixture modules. The legacy token
login/`console.js` bootstrap is a documented temporary dependency until replacement login parity.

| Owner | Responsibility |
| --- | --- |
| `components.tsx` | Head, Panel, Badge, Link, Empty, Disclosure, Binding, ControllerFacts, Pagination, focus-contained native Dialog |
| `pages.tsx` | Overview, source-context table/portfolio and plan workspace compositions |
| `main.tsx` | Local route/theme/filter/page selection, canceled reads, retained snapshot/failure/age; inert closed mobile navigation |
| `decisions.tsx` | Local frozen confirmation subject, one submission, uncertain-outcome reconciliation and historical receipt focus |
| `api.ts` | Typed/validated sanitized DTOs, bounded transport and one allowlisted rejection POST |
| Shared tokens/layout/components CSS | The accepted dark/light identity, dense desktop layout and deliberate mobile anatomy |

Presentation abstractions satisfy repository §6(c), the explicit reusable-component requirement.
The HTTP boundary satisfies §6(b). No backend interface, workflow database, scheduler, policy or
authority state machine was added. Literal search is a linear index scan and each page performs at
most 50 canonical plan reductions; the code names those ceilings. Wire paging does not bound the
length of a plan journal or prove production reduction latency.

## Three corrected distinctions

1. Durable identity records never imply endpoint health. Disconnected or stale observations display
   unreachable/unobserved live state even when a durable generation remains attested/recorded.
2. Pending requests come from the canonical `PlanSnapshot.AwaitingDecision` predicate. Currently
   actionable counts additionally require a fresh, observed serving controller with attestation,
   role and generation match. **Actionable here means rejection only**, scoped to the loaded page.
3. Resolved/rejected receipts are historical journal facts. The selected URL stays pinned to its
   revision, the request leaves the pending list and focus moves to the receipt heading. No next
   request is silently selected. Reload/reconnect re-read the same historical subject. Cancellation
   restores the exact opener; “Other human requests” is explicit navigation.

Browser receipt time is not a controller observation or liveness lease. After 30 seconds, the UI
marks an observation stale and disables review; a failed refresh retains only that view's historical
snapshot. Controller/index/detail reads are independent observations. This narrowing UI behavior
never replaces the server's authority checks.

## Genuine governed workflow

The first live action is **reject a proposed plan revision**, the existing `POST /v1/plans/{id}/reject`.
Approval is disabled because prospective assignment bindings are unavailable in the current read
surface. Publication, merge, decision-resolve and other controller verbs were not added.

Review reads the exact revision and live controller again. A changed digest, resolved request or
changed controller refuses confirmation. Confirmation shows the exact plan ID/revision/digest,
protected action and consequence, with full controller binding available. It submits once; it never
refreshes a binding silently or retries an uncertain write. The existing server independently
re-observes eligibility, proves the expected controller on that connection, executes under the
actual controller role and plan lock, and verifies the pending revision/digest before journal append.
Unknown outcomes stay unavailable when a re-read still has no receipt; a pending GET cannot prove
that an earlier command will not complete. Known applied/refused responses are retained when the
follow-up read fails. Lost-reply/idempotency behavior remains the existing durable-effect reconciliation, not a client
state transition. Transport or response-validation failure after POST is an unknown outcome;
reconciliation reads the pinned revision before offering another review.

Two additive GET contracts reuse the existing read store/journal:

- `/v1/plans?limit=50&offset=0&q=...`: bounded identities, including refused attempts with no revision.
- `/v1/plans/{id}/context?revision=...`: existing sanitized PlanDetail, canonical pending predicate
  and historical receipt metadata. Private objective, note, operator and event payload stay excluded.

A context read checks the journal tip against the reduced view and returns 409 `snapshot_changed`
for a torn read. It never merges a stale pending projection with a newer receipt. No persisted format,
SQL migration, role policy or action allowlist changed; rollback is a frontend/API code rollback.

## Evidence and visual review

`TestEngineeringOSPlanRejectEndToEnd` seeds only an isolated test workspace, invokes the actual
`serve` control handler over the owner-only Unix socket, holds the real controller role and writes
its real hash-chained plan journal. The browser driver never mocks `API.Send` or supplies an
acceptance verdict. It verifies exact-subject rejection, one POST, durable receipt, no resubmission
on reload, and recovery after closing/reopening the read store, controller composition, role and
socket. Negative HTTP cases cover changed digest/controller, unattested generation, released role
and disconnected pending requests. A separate transport fault discards the reply after an actual journal write; a failed reconciliation
read stays closed, and a later read recovers the receipt without resubmitting. Existing governed
tests retain racing-decision and stop proofs.
This is actual backend behavior on deterministic **test data**, not production fleet acceptance.

The retained preview suite separately covers synthetic layouts/negative states and HTTP-boundary
presentation mocks. Those mocks are never cited as governed-action evidence or captured as runtime
measurements. React checks cover 50-row rendering across a 500-row contract, full-index search,
plain-text rendering, gate/run separation, changed-subject review refusal, stale/permission/malformed/
mismatched/disconnected reads, historical receipts, keyboard navigation and dark/light text contrast.

Thirty primary React captures plus seven lost-reply/reconciliation captures use visibly labeled isolated test data and the actual controller path:

| Experience | 1440 | 1920 | 390 | Light example |
| --- | --- | --- | --- | --- |
| Overview | [Dark](screenshots/batch-01/overview-1440-dark.png) | [Dark](screenshots/batch-01/overview-1920-dark.png) | [Dark](screenshots/batch-01/overview-390-dark.png) | [Mobile](screenshots/batch-01/overview-390-light.png) |
| Portfolio | [Dark](screenshots/batch-01/portfolio-1440-dark.png) | [Dark](screenshots/batch-01/portfolio-1920-dark.png) | [Dark](screenshots/batch-01/portfolio-390-dark.png) | [Mobile](screenshots/batch-01/portfolio-390-light.png) |
| Workspace | [Dark](screenshots/batch-01/workspace-1440-dark.png) | [Dark](screenshots/batch-01/workspace-1920-dark.png) | [Dark](screenshots/batch-01/workspace-390-dark.png) | [Mobile](screenshots/batch-01/workspace-390-light.png) |
| Action Required | [Dark](screenshots/batch-01/actions-1440-dark.png) | [Dark](screenshots/batch-01/actions-1920-dark.png) | [Dark](screenshots/batch-01/actions-390-dark.png) | [Mobile](screenshots/batch-01/actions-390-light.png) |

[Expanded confirmation](screenshots/batch-01/confirmation-390-expanded.png),
[historical rejection](screenshots/batch-01/receipt-390-dark.png),
[disconnected receipt](screenshots/batch-01/receipt-disconnected-390.png),
[disconnected controller](screenshots/batch-01/controller-disconnected-390.png),
[reconnected receipt](screenshots/batch-01/receipt-reconnected-390.png),
[unknown outcome after actual journal write](screenshots/batch-01/lost-reply/unknown-outcome-390.png).

Actual captures were inspected and refined: mobile action visibility, exact opener/receipt focus,
light disabled-control contrast and closed-drawer shadow/focus. Automated visual regression evidence
is layout/reflow/visibility contracts plus rendered contrast checks; there is no approved pixel-diff
baseline yet. Screen-reader/high-zoom qualification, production load performance, complete parity
and operator acceptance of the operational replacement remain open. See
[validation](validation.md) and the [final legacy-removal inventory](frontend-inventory.md).

No old template, CSS, JavaScript, route or duplicate template test has been retired. Removal is gated
on complete parity and operator acceptance. No production plan was approved, published or merged.
