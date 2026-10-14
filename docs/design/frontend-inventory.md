# Frontend consolidation inventory

Reconciliation base: `e6d90b9454ebb125cc1252eb363e7a147fec9a75` (current main integrated into PR #594).
Design accepted; independent source re-review and operator acceptance remain required.
**No legacy implementation has been deleted or redirected in this batch.**

## Authority and delivery

ADR-0004 governs the existing authenticated loopback Go Control Plane. Planning v0.1 and
WorkGraph own lifecycle, assignments and admitted handoffs. React consumes their projections;
it adds no runtime, scheduler, workflow database or authority system.

`cmd/zenchron-engineering/main.go` dispatches `control-plane`; `control_plane.go` composes one
listener, bearer API, cookie boundary and existing owner-only governed controller socket.
`/app` is the React replacement. `/design` is the frozen fixture checkpoint. The Go-template
console remains operational on its existing URLs during migration. No independent production
frontend server exists. `frontend/build.mjs` uses the already-installed esbuild/TypeScript stack
and writes embedded `react.js`; production needs only the Go binary. No dependency was added.

The earlier inventory predates main's workflow workspace. `web_workspace.go` and
`web_objective.go` now exist and must be included in retirement. The prior statement that
approval/decision/publication actions were CLI-only is superseded by main's governed browser APIs.

## Supported-operation parity matrix

“Browser implemented” means an interaction submits through the existing controller action path.
It does not mean human operator acceptance or production authorization.

| Legacy supported operation | React replacement | Qualification / remaining gap |
| --- | --- | --- |
| Token login, same listener and authenticated observations | React login / shared bearer transport | Actual login and wrong-token refusal; credential stays out of URLs/local storage |
| Overview controller, fleet, plan and run observations | Overview plus controller diagnostics | Independent activity dimensions and unknown aggregates; no invented completion/trends |
| Objectives/plans list, paging, source and latest lifecycle | Objectives Portfolio | Search/paging, canonical lifecycle separate from revision state; pending beyond 50 regression |
| Create objective from governed repository and existing issue, agent/profile/template preferences | Objective intake | Real controller deterministic planning; proposal remains unapproved |
| Objective/plan detail, objective prose, assignments, revision and change inspection | Objective Workspace / controller preview disclosure | Exact plan revision/digest, prospective assignments, budgets and typed stages |
| Revise plan | Workspace review/confirmation | Real controller deterministic revision; new revision unapproved |
| Approve plan | Workspace and Action Required | Fresh exact revision/digest/assignments plus controller; real refusal and durable approval receipt |
| Reject plan | Action Required | Actual controller, one journal effect, lost response, reconnect/process-restart and historical focus |
| Exact objective-to-WorkGraph binding | Embedded workspace WorkGraph | Existing durable association; mismatches close actions; a PlanStage is not a WorkUnit |
| WorkGraph dependencies, frontier, units, holds, handoffs and independent review | WorkGraph and unit inspector | Actual edge presentation; 50 visible nodes, full graph decoding; 1200-unit load fixture |
| Typed WorkUnit human decisions / designated-operator readiness hold | Action Required / selected WorkUnit | Existing allow_deny, selected_option and text grammar; real hold resolution and durable receipt |
| Publication human authorization/rejection | Run and objective-bound request controls | Exact live request/digest/candidate/tree/contract; one real human evidence event, lost-response reconciliation; no publication/merge |
| Runs: status/source/held filters, detail, activity cursor and diagnostics | Runs / execution workspace | Shared before-page filters, 50-event cursor window, worker/controller/evidence facts |
| Run stop | Execution workspace | Existing controller cancellation; ambiguity, one effect and process-restart proof |
| Run pause / unpause | Execution workspace | Actual journal state; GET/SSE share pause projection; read-only state is labeled as observation |
| Run resume / source refresh | Execution workspace | Controller-created positive admission/successor; unowned input refused; no second scheduler |
| Products, configuration references and associated WorkGraphs | Products / product workspace | Existing paged association APIs; no inferred objective ownership |
| Installed agents, provider/model/trust, profiles/templates | Agents / objective intake | Existing controller options; active recorded bindings remain distinct from installed inventory |
| Settings/controller consistency, reachability, role, admission and findings | Controller health | Attested identity never implies endpoint reachability |
| Errors, unknown/missing records, stale/disconnected and permission refusal | Shared unavailable states | Retained observations historical; decisions closed; no fake successful empty read |
| Old bookmarks and sole root entry | Compatibility plan below | Intentionally pending cutover; legacy remains supported |

No supported legacy browser operation is intentionally dropped. The following are **not accepted
as complete replacement qualification**: manual screen-reader speech/high zoom, operator-witnessed
acceptance, production-scale service latency, a human-approved pixel baseline and production cutover.
Positive live testing exercises allow_deny hold resolution; selected_option/text controls consume the
existing grammar but need a real operator walkthrough of each policy-specific request. Resume/refresh
have no new durable idempotency key: an unknown response stays closed for manual canonical inspection.
Long planning commands may outlive the listener write deadline; this is an existing boundary, not a
new retry entitlement. Read-only display and CLI-only operations never count as browser acceptance.

## Exact presentation removal inventory (after acceptance)

All 20 template files currently remain:

```text
controlplane/assets/templates/layout.html
controlplane/assets/templates/login.html
controlplane/assets/templates/overview.html
controlplane/assets/templates/objectives.html
controlplane/assets/templates/objective_detail.html
controlplane/assets/templates/plans.html
controlplane/assets/templates/plan_detail.html
controlplane/assets/templates/decisions.html
controlplane/assets/templates/decision_card.html
controlplane/assets/templates/agents.html
controlplane/assets/templates/settings.html
controlplane/assets/templates/runs.html
controlplane/assets/templates/run_detail.html
controlplane/assets/templates/products.html
controlplane/assets/templates/product_detail.html
controlplane/assets/templates/workgraphs.html
controlplane/assets/templates/workgraph_detail.html
controlplane/assets/templates/error.html
controlplane/assets/templates/not_found.html
controlplane/assets/templates/run_not_found.html
```

Delete these exact presentation files only after parity and acceptance:

- `controlplane/assets/static/console.css`, `console.js`.
- `controlplane/web_templates.go`: template registration/functions and old asset embedding;
  relocate `mustSubFS` still used by the replacement before removing the file.
- `controlplane/web_workspace.go`, `web_objective.go`, `web_product.go`: retire HTML handlers,
  move any projection/context logic still used by `/v1/` into its cohesive backend owner.
- `controlplane/web.go`: remove page rendering/data types and legacy route dispatch; retain
  method/path, no-store/security and shared authentication behavior in the application delivery owner.
- Fixture-only `/design`, `assets/design/app.js`, `fixtures.js`, `overview.js`, `portfolio.js`, `workspace.js`, `actions.js`, `components.js`
  and any other fixture-only module discovered by the final import audit. Retain
  tokens/layout/components/runtime CSS, React bundle and the single application shell.

Do not delete `api.go`, `actions.go`, `workspace_actions.go`, `run_operations.go`, `plan_context.go`,
`workspace_requests.go`, `authority_receipts.go`, `run_observation.go`, fleet/product/workgraph/run
projections, schemas, `runs_filter.go`, read-store/domain services, controller role/auth paths or
journal reducers. Former template consumption is not evidence that a backend API is obsolete.
`workspaceGraphActions` currently lives in `web_workspace.go` and is shared by the new read API:
extract this projection before deleting that presentation file.

## Bookmark and navigation compatibility

| Existing URL | Final single application destination |
| --- | --- |
| `/`, `/overview` | `/app#/overview` |
| `/objectives`, `/plans` | `/app#/objectives` (translate offset) |
| `/objectives/new` | `/app#/intake` |
| `/objectives/{id}`, `/plans/{id}` | `/app#/objectives/{id}` (translate revision) |
| `/decisions` | `/app#/actions` (translate plans/graph offsets) |
| `/runs` | `/app#/runs` (translate supported source/status/offset filters) |
| `/runs/{id}` | `/app#/runs/{id}` (translate event cursor) |
| `/workgraphs[/{id}]` | `/app#/workgraphs[/{id}]` |
| `/products[/{id}]` | `/app#/products[/{id}]` (translate association offset) |
| `/agents` | `/app#/agents` |
| `/settings` | `/app#/controller` |

Use explicit safe redirects or shell compatibility routing, with escaped path IDs and bounded query
translation. These redirects are not active yet. An old URL must not launch a second dashboard after
cutover. No fallback preserving old HTML/JS is permitted in the final application.

## Tests, documentation and packaging

Keep legacy `web_test.go`, `web_template_test.go`, `web_workspace_test.go`, `web_objective_test.go`,
`run_lifecycle_view_test.go`, product/graph tests and `TestBrowserWorkspace`/`scripts/browser_e2e.mjs`
until equivalent behavior is accepted. Replace HTML/template assertions and retire the old driver
at cutover; preserve backend projection, privacy, authorization, refusal and compatibility proofs.

Maintain one browser entry for React covering shared navigation/login, objective intake/revision,
approval/rejection, typed decisions, publication, run controls, receipts and responsive/keyboard states.
Current drivers are `check_design_preview.mjs` (presentation only), `check_reconciled_workspace.mjs`
(real controller) and `check_live_decision.mjs` (real rejection/stop/restart). Consolidate fixture entry
retirement with removal, rather than keeping multiple supported operator test suites indefinitely.

Update CLI help in `main.go`, listener comments/help in `control_plane.go`, README and
`docs/control-plane.md`, getting-started/running-work/troubleshooting links, frontend README,
design docs, embedded asset imports and packaging/browser commands. Verify no stale legacy route,
template registration, static reference or documentation launcher remains reachable.

Deletion acceptance requires supported-workflow operator acceptance, independent review, required
exact-head CI, security/authorization preservation and an actual deleted-path report here.
**Deleted legacy paths in this reconciliation: none.** One final Engineering OS remains the invariant.
