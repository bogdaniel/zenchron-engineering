# Frontend consolidation inventory

Status: **design gate accepted; React batch 01 implemented; legacy deletion pending complete parity and operator acceptance**.
Audit of this checkout on 2026-10-13. The working tree was clean before this task.
The four interactive fixture experiences are at `/design`. Existing routes remain operational.

## Governing decisions and contradictions

[ADR-0004](../adr/0004-local-operational-control-plane.md) governs the local listener,
authentication, sanitized projections, and governed socket actions. Architectural principles
P2, P5, P7, P9, P10, and P12 govern how the interface represents authority and evidence.
[Planning v0.1](../spec/planning-v0.1.md) owns plan approval and stage semantics;
[WorkGraph](../workgraph.md) owns issue-backed units and admitted dependency handoffs.
The preview does not add a runtime, scheduler, policy, authority, or store.

The requested `controlplane/web_workspace.go` and `controlplane/web_objective.go`
**do not exist in this checkout**. The newer Product Workspace is implemented by
`web_product.go` and templates inside the same Go console. It is not a separate application.
The initial inventory, before this work, found no other active frontend, Node manifest, Vite configuration,
React application, browser automation suite, or frontend server. The nested `agentkernel`
is an execution engine, not an operator frontend. There was no duplicate production
frontend before the replacement migration. Both current Go presentation families must be retired at cutover.

Documentation mismatches found, retained explicitly rather than silently changing scope:

- README's Status/non-goals still describe the planner as future work; ROADMAP and
  the normative planning specification describe the implemented M2 surface.
- `docs/control-plane.md` describes recursive Product Workspace detail; the current
  `product_view.go` implementation intentionally returns paged lightweight graph summaries.
- The CLI has human decision and publication evidence capabilities the HTTP console
  does not expose. Redesign cannot manufacture new HTTP authority.

## Entry points, presentation, and delivery

### Current UI shortcomings

The existing console is functionally useful and truthfully bounds freshness,
uncertainty and authority. Its shortcomings are presentation and workflow structure:

- The overview leads with controller/fleet infrastructure rather than informative
  objectives, current engineering flow and a prioritized human action queue.
- Run/plan identifiers dominate headings. Operators repeatedly cross Runs, Products
  and WorkGraphs to reconstruct one engineering objective and its next blocker.
- There is no unified approval/decision inbox, no objective title search, and no
  graphical WorkGraph or throughput view. Existing decision displays are read-only.
- All-monospace typography gives prose, headings and exact values similar emphasis;
  flat key/value sections provide weak hierarchy for a long run or graph.
- Dark-only styling has no reusable light theme. Mobile primarily wraps identifiers
  and horizontally scrolls tables instead of prioritizing navigation and decisions.
- Whole-page polling preserves the established projection boundary, but the future
  shell must preserve focus, stale warnings and outcome uncertainty while replacing
  its DOM-swapping interaction model. A polished UI must not mislabel it as a live feed.

The redesign addresses those costs while keeping the useful security, privacy,
bounded-read and freshness semantics in the migration acceptance table below.

| Current file or boundary | Current responsibility | Replacement / eventual disposition |
| --- | --- | --- |
| `cmd/zenchron-engineering/main.go` | Dispatches `control-plane`; CLI help | Keep command; update help after acceptance |
| `cmd/zenchron-engineering/control_plane.go` | One loopback listener; `/v1/` → API, `/` → Web; one token, read store, observation seam | Keep listener and composition; replace Web presentation mount |
| `controlplane/web.go` | Cookie authentication, GET/method/path guards; overview, runs, plan detail; page/error rendering and filters | Retain shared boundary protections; remove obsolete rendering/data structs/filter helpers after parity |
| `controlplane/web_templates.go` | Template registration, template FS, static FS, formatting, link generation | Remove Go HTML/template machinery; retain or relocate necessary embed/FS utility for new asset delivery |
| `controlplane/web_product.go` | Product/list/graph HTML handlers, page offsets and compatibility links | Remove presentation handlers; retain shared validation where still consumed |
| `controlplane/assets/templates/` | Thirteen embedded HTML templates; see complete inventory below | Delete after operational parity, browser proofs, and human visual approval |
| `controlplane/assets/static/console.css` | Legacy dark-only styles, all-monospace typography, table overflow, modal styling | Delete after new shell also owns login/errors/actions |
| `controlplane/assets/static/console.js` | Token login, whole-page refresh, freshness, stop/reject confirmations | Replace behavior in the single application's integration layer; delete asset |
| `controlplane/web_design.go` | Shared authenticated `/app` and `/design` shell/asset delivery; mode-specific connection CSP | Keep one app delivery boundary; retire fixture mode at accepted cutover if no longer required |
| `controlplane/assets/design/` | Shared tokens/CSS, frozen fixture checkpoint and generated React runtime entry | Consolidate into the sole supported application; never retain an independent dashboard |

The React batch has one esbuild bundle step and one Go embed/delivery pipeline.
Its pinned manifest/lockfile lives in `frontend/`; production needs only the Go binary.
The checkpoint and runtime entry share CSS, authentication and listener. No production Vite server exists.

Complete existing template inventory:

| Template | Capability to preserve |
| --- | --- |
| `layout.html` | Shell, primary navigation, freshness, action confirmation dialog |
| `login.html` | Token entry, wrong-token error, same token session |
| `overview.html` | Controller dimensions, fleet counts, held material, plans |
| `runs.html` | Run list, disposition/held/source filters |
| `run_detail.html` | Source/candidate/controller, trust, operation progress and source, budgets/attempts, held work, assurance, publication authority/request, bounded event timeline, stop |
| `plan_detail.html` | Exact revisions/digests, governing vs proposed revision, stages/dependencies, assignments, gates, reviews, budget, reject |
| `products.html` | Adopted product list and offset paging |
| `product_detail.html` | Product configuration, repository scope, associated graph summary paging |
| `workgraphs.html` | Graph inventory and offset paging |
| `workgraph_detail.html` | Frontier, units/dependencies, run details, holds, admitted output, independent review/findings, open decisions |
| `error.html` | Bounded read failure without raw exception text |
| `not_found.html` | Unknown route recovery |
| `run_not_found.html` | Missing run recovery |

## Authentication and mutation behavior to retain

- Listener is literal-loopback-only; the HTTP process opens the existing SQLite store
  read-only and never creates/migrates runtime state.
- Token is generated into the owner-only state directory as a regular 0600 file.
  No token may appear in a URL, response, startup output, or logs.
- Browser navigation accepts the same token through `zenchron_control_plane_token`.
  Current login JavaScript validates it with `GET /v1/controller`. Invalid tokens stay
  on login. The session is not a second credential lifecycle.
- API requests require a Bearer header. Mutating requests never authenticate by
  cookie alone; preserve exact Host/Origin/Sec-Fetch-Site, JSON shape/size checks,
  controller-binding proof, server-side eligibility, and same-request authority.
- No CORS. Preserve no-store and nosniff. New navigation must not bypass path or
  method guards or expose a second static application entry point accidentally.
- Preserve bounded reads/cursors, progress-row vs journal distinctions, attestation,
  generation uncertainty, and sanitized DTOs. Do not render raw transcripts or errors.
- Lost replies remain unknown until reconciled, never automatically retried.

Preview decisions only modify an in-memory browser Map, reset on reload. The preview
CSP forbids connections and form submissions; authenticated assets contain no runtime
measurements. Real runtime state is never loaded alongside fixture measurements.

## Retained backend inventory and API integration map

**Retain**, even after their old template consumers are removed:
`api.go`, `actions.go`, `server.go`, `stream.go`, `plan_detail.go`, `product_view.go`,
`workgraph_view.go`, sanitized DTOs/projections, read-store services, controller
identity/authorization, and all `schemas/control-plane-*.schema.json` contracts.

| Capability | Existing contract / path | Unified application destination |
| --- | --- | --- |
| Controller consistency, serving, role, admission, binding, generation | `GET /v1/controller` | Overview health strip + Controller health |
| Runs and fleet | `GET /v1/runs` | Objective portfolio / Runs view with source and held filters |
| Run detail | `GET /v1/runs/{id}` | Objective workspace run inspector; also standalone run route |
| Causal history / progress | `GET /v1/runs/{id}/events`, `/stream` | Workspace activity; retain run-local cursor and row rereads |
| Plans / revision selection | `GET /v1/plans/{id}`, `/detail?revision=N` | Portfolio plans view + workspace Plan; preserve proposed/governing distinction |
| Products / configuration | `GET /v1/products`, `/v1/products/{id}` | Product scope workspace; retain association paging |
| WorkGraphs / issue units | `GET /v1/workgraphs`, `/v1/workgraphs/{id}` | Operational graph view; unmatched graphs remain navigable |
| Independent review, holds, admitted output, open requests | WorkGraph detail DTOs, plan stage review DTOs | Workspace review/remediation and Action required |
| Governed run stop | `POST /v1/runs/{id}/stop` | Run inspector → bound confirmation → reconciled result |
| Governed plan rejection | `POST /v1/plans/{id}/reject` | Plan decision → exact revision/digest/controller confirmation |
| Agents / readiness / provenance | CLI `autonomy agents`; worker metadata in run DTO | Agents view; dedicated read API remains a migration gap |
| Detailed diagnostics | CLI `autonomy doctor`, existing controller findings | Diagnostics; only bounded safe projections may be added |
| Plan approval | CLI/socket exists; **not** in governed HTTP allowlist | Preview only until assignment-view and bound governed route are reviewed |
| Human decision resolution / readiness holds | CLI `autonomy workgraph resolve` / `hold`; **not** in HTTP allowlist | Preview only until a reviewed exact-request governed action exists |
| Human publication evidence | CLI `autonomy authorize`; request visible in run DTO; **not** in HTTP allowlist | Preview only until reviewed authority-request evidence recording exists |

No objective aggregate/title collection endpoint exists. Informative fixture titles are
not proof of a production API: migration must define a bounded sanitized projection
from existing pinned intent without introducing a second objective truth store.
Likewise historical throughput, supervision time, and capacity slot measurements are
illustrations; absent measurements must be omitted or explicitly unknown in production.

The replacement will have **one typed API integration module** consuming the existing
JSON Schemas, centralizing Bearer headers, bounded reads, cancellation, typed failures,
and mutation outcome reconciliation. No new `/api/` authority surface, projection cache,
or frontend-side policy evaluator is permitted. No speculative runtime adapter is built
during this fixture design gate.

## Test, documentation, dependency, and removal inventory

| Item | Preserve / replace at cutover |
| --- | --- |
| `controlplane/web_test.go` | Replace presentation assertions with authenticated shell, route compatibility and browser behavior tests; keep method/path/token invariants |
| `controlplane/web_template_test.go` | Retire template execution assertions only after equivalent empty/optional/unknown/held/freshness and UI coverage |
| `controlplane/product_view_test.go` | Keep projection tests; replace template-only test `TestProductDetailTemplateRendersAssociatedGraph` |
| `controlplane/product_api_test.go` | Keep paging/association/integrity/restart/decision projection proofs; replace HTML link checks only where shell compatibility changes |
| `controlplane/plan_detail_test.go`, `workgraph_view_test.go`, `projection_test.go` | Keep gates-runless, binding, optional-field, error propagation, privacy proofs |
| `controlplane/api_test.go`, `actions_test.go`, `stream_test.go`, `stream_bench_test.go` | Keep public contracts, auth, no-secret boundary, closed actions, ambiguity, bounded cursor and read-cost proofs |
| `cmd/zenchron-engineering/control_plane_test.go` | Keep invocation/listener/composition behavior; expand accepted shell packaging coverage |
| `scripts/check_design_preview.mjs`, `web_design_test.go` | This checkpoint's browser interactions/captures and auth/isolation checks; become the single maintained browser set at integration |
| Existing browser suite | None found; Go HTTP/template tests are not browser tests |
| README, `docs/control-plane.md`, `docs/workgraph.md` | Update operator launch/navigation/action limits, routes, screenshots and new delivery; reconcile noted stale prose |
| `docs/getting-started.md`, `running-work.md`, `running-multiple-tasks.md`, `planning.md`, `agents.md`, `configuration.md`, `troubleshooting.md`, `supervisor.md` | Review operator instructions and CLI fallbacks against completed parity; update affected instructions, not domain semantics |
| ADR-0004, ROADMAP, product/construction architecture | Preserve boundaries; explicitly record any approved change in frontend delivery infrastructure |
| `go.mod`, `go.sum`, CLI build, `.github/workflows/ci.yml`, `ci/static.sh`, `ci/evidence/registry.json` | No dependency/build change now; at migration verify embed packaging and evidence selection for new assets/browser tests |

The React build has one pinned manifest/lockfile in `frontend/`. Do not delete Go dependencies or backend
schemas simply because the template layer no longer uses them.

## Bookmark compatibility after acceptance

These are **planned mappings**, not redirects implemented at this checkpoint.
They preserve exact identity, source filters, revision selection and causal cursor;
compatibility handlers are redirects or routing glue, never old HTML renderers.

| Old URL | Final shell destination |
| --- | --- |
| `/`, `/overview` | Engineering overview |
| `/runs?status=...&source=...` | Objectives → Runs view; retain filter meaning |
| `/runs/{id}?after=...` | Standalone run inspector / linked objective activity; retain exclusive cursor |
| `/plans/{id}?revision=...` | Exact plan workspace; governing/proposed revision preserved |
| `/products`, `/products/{id}?graphs_offset=...` | Product inventory / workspace with association page |
| `/workgraphs`, `/workgraphs/{id}` | WorkGraph inventory / operational view; graphs without plans/products still supported |
| `/design#/...` | Retire preview at cutover or explicitly redirect supported prototype views to real destinations |
| `/static/console.js`, `/static/console.css` | Removed; new shell delivers its assets through one embed pipeline |

## Sequenced migration and deletion gate

1. **Now:** review four actual rendered experiences and approve or request visual changes.
   Keep legacy UI, listener, `/v1/`, actions and data unchanged.
2. Define required bounded read projections and separately review any governed action
   additions. Implement one schema-derived typed integration layer. All production
   measurements have provenance; fixture import is excluded from production bootstrap.
3. Wire the accepted shell through the existing Control Plane. Complete each capability
   in the parity table, including standalone/unassociated runs and graphs, failures,
   preserved material, unsupported-action CLI paths, diagnostics, and authentication.
4. Prove behavioral parity using the same browser set plus retained backend tests.
   Compare live views with the CLI/store, including unknown, stale, changed binding,
   refused, unreachable, and ambiguous outcomes. Obtain human visual approval on the
   operational replacement, not merely these screenshots or a green test run.
5. In one reviewable cutover, replace `/` with the accepted shell, add compatibility
   mappings, remove the thirteen templates, both legacy static assets, obsolete Web
   presentation code and template tests, and retire the isolated fixture preview mount.
   Retain the common auth, listener, APIs and governed action paths.
6. Update operator/build docs; record exact deleted paths and parity proofs. Search for
   old route/template/static registrations; verify none serves obsolete HTML and no
   independent production frontend server exists. Run focused browser/backend checks,
   build, vet and size checks. Full-suite evidence belongs to current-head CI.

No persisted-data migration is needed for this checkpoint. Later shell replacement must
not rewrite runtime journals, plans, evidence, decisions or identifiers. Rollback before
deletion is a presentation rollback. After cutover, rollback is a code/release rollback,
with preserved runtime contracts; no hidden switch should keep a second dashboard alive.

**Deletion performed in this task: none.** Behavioral parity, real API integration,
production accessibility qualification, migration and removal remain gated on acceptance.
Final invariant: one Engineering OS, one frontend, one design system, one governed backend.

## Migration update: React implementation batch 01

`/app` loads the generated React entry; `/design` retains the frozen fixture entry. Both reuse
one authenticated embedded HTML and the authoritative CSS tokens/layout/components on one Go listener.
`frontend/src/api.ts` is the sole runtime frontend API layer. The superseded, unaccepted read-only
prototype adapters `assets/design/api.js` and `runtime.js` and their bootstrap branches were removed
before commit. No legacy Go frontend component was deleted.

The maintained browser entry is `scripts/check_design_preview.mjs`, with React boundary checks in
`check_engineering_app.mjs`; the genuine controller/socket/journal driver is
`TestEngineeringOSPlanRejectEndToEnd` plus `check_live_decision.mjs`. The legacy login template and
`console.js` cookie bootstrap remain explicit temporary dependencies. Full objective/public title
projection, plan approval assignment bindings, standalone runs/history/WorkGraphs/Products, agent
inventory, diagnostics, governed stop, a qualified replacement login and compatibility redirects
remain parity prerequisites. No old template, CSS/JS asset or presentation handler was removed.

See [the precise batch scope and acceptance proof](runtime-integration.md). Deletion inventory:
**none of the thirteen legacy templates, two static assets, old route registrations or template tests
has been deleted**. The deletion candidates and route mappings above remain the final cutover list;
record actual deleted paths and evidence in this document when operator acceptance permits cutover.
